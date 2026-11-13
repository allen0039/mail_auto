package network

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

type Error struct {
	Stage string `json:"stage"`
	Code  string `json:"code"`
	cause error
}

func (e *Error) Error() string              { return e.Code }
func (e *Error) OperationErrorCode() string { return e.Code }
func (e *Error) Unwrap() error              { return e.cause }

func (e *Error) Retryable() bool {
	switch e.Code {
	case "proxy_connection_failed", "proxy_socks5_failed", "proxy_connect_failed",
		"proxy_connect_rejected_502", "proxy_connect_rejected_503", "proxy_connect_rejected_504":
		return true
	case "target_tls_failed":
		var timeout net.Error
		return errors.Is(e.cause, io.EOF) || errors.Is(e.cause, io.ErrUnexpectedEOF) ||
			errors.Is(e.cause, net.ErrClosed) || errors.As(e.cause, &timeout) && timeout.Timeout()
	}
	return false
}

func Failure(err error) (string, string) {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Stage, failure.Code
	}
	var coded interface{ OperationErrorCode() string }
	if errors.As(err, &coded) {
		return "target_dns", coded.OperationErrorCode()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", "network_timeout"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout", "network_timeout"
	}
	return "connection", "network_connection_failed"
}

func (r Route) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if r.Profile == nil {
		return dialer.DialContext(ctx, network, address)
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, &Error{Stage: "proxy", Code: "proxy_protocol_invalid"}
	}
	profile := r.Profile
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	endpoint := net.JoinHostPort(profile.Host, strconv.Itoa(profile.Port))
	if profile.Protocol == "socks5" {
		var auth *proxy.Auth
		if profile.Username != "" {
			auth = &proxy.Auth{User: profile.Username, Password: r.Password}
		}
		proxyDialer, err := proxy.SOCKS5("tcp", endpoint, auth, dialer)
		if err != nil {
			return nil, &Error{Stage: "proxy", Code: "proxy_config_invalid"}
		}
		conn, err := proxyDialer.(proxy.ContextDialer).DialContext(ctx, network, address)
		if err != nil {
			if strings.Contains(err.Error(), "authentication") {
				return nil, &Error{Stage: "proxy_auth", Code: "proxy_auth_failed"}
			}
			return nil, &Error{Stage: "proxy_tunnel", Code: "proxy_socks5_failed", cause: err}
		}
		return conn, nil
	}
	if profile.Protocol != "http" && profile.Protocol != "https" {
		return nil, &Error{Stage: "proxy", Code: "proxy_protocol_invalid"}
	}
	conn, err := dialer.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return nil, &Error{Stage: "proxy_connect", Code: "proxy_connection_failed", cause: err}
	}
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()
	socket := conn
	stop := context.AfterFunc(ctx, func() { socket.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if profile.Protocol == "https" {
		secured := tls.Client(conn, &tls.Config{ServerName: profile.Host, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
		if err = secured.HandshakeContext(ctx); err != nil {
			return nil, &Error{Stage: "proxy_tls", Code: "proxy_tls_failed"}
		}
		conn = secured
	}
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	if profile.Username != "" {
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(profile.Username+":"+r.Password)))
	}
	if err = request.Write(conn); err != nil {
		return nil, &Error{Stage: "proxy_tunnel", Code: "proxy_connect_failed"}
	}
	reader := bufio.NewReader(io.LimitReader(conn, 32<<10))
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, &Error{Stage: "proxy_tunnel", Code: "proxy_response_invalid"}
	}
	if response.StatusCode == http.StatusProxyAuthRequired {
		return nil, &Error{Stage: "proxy_auth", Code: "proxy_auth_failed"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &Error{Stage: "proxy_tunnel", Code: fmt.Sprintf("proxy_connect_rejected_%d", response.StatusCode)}
	}
	buffered := reader.Buffered()
	if buffered > 0 {
		return nil, &Error{Stage: "proxy_tunnel", Code: "proxy_response_invalid"}
	}
	_ = conn.SetDeadline(time.Time{})
	success = true
	return conn, nil
}

func (r Route) DialTLS(ctx context.Context, address, serverName string) (net.Conn, error) {
	conn, err := r.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	secured := tls.Client(conn, &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12})
	if err = secured.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, &Error{Stage: "target_tls", Code: "target_tls_failed", cause: err}
	}
	return secured, nil
}

func (r Route) Transport() *http.Transport {
	return &http.Transport{
		Proxy: nil, DialContext: r.DialContext,
		ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second, IdleConnTimeout: 60 * time.Second,
		MaxIdleConns: 8, MaxIdleConnsPerHost: 4,
	}
}

func HTTPClient(transport http.RoundTripper, timeout time.Duration) *http.Client {
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
