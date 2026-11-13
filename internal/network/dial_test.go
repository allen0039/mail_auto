package network

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func profileFor(t *testing.T, endpoint, protocol string) *Profile {
	t.Helper()
	host, rawPort, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(rawPort)
	return &Profile{Host: host, Port: port, Protocol: protocol}
}

func TestHTTPConnectTunnelRemoteDNSAndAuthentication(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodConnect || request.Host != "unresolvable.invalid:993" || request.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			w.WriteHeader(407)
			return
		}
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffered.Flush()
		io.Copy(conn, conn)
	}))
	defer server.Close()
	profile := profileFor(t, strings.TrimPrefix(server.URL, "http://"), "http")
	profile.Username = "user"
	route := Route{Profile: profile, Password: "pass"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := route.DialContext(ctx, "tcp", "unresolvable.invalid:993")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(time.Second))
	conn.Write([]byte("through tunnel"))
	got := make([]byte, len("through tunnel"))
	_, err = io.ReadFull(conn, got)
	conn.Close()
	if err != nil || string(got) != "through tunnel" {
		t.Fatal(string(got), err)
	}
	route.Password = "bad"
	_, err = route.DialContext(ctx, "tcp", "unresolvable.invalid:993")
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "proxy_auth_failed" || calls.Load() != 2 {
		t.Fatal("auth failure lost", err, calls.Load())
	}
}

func TestSOCKS5RemoteDNSAndAuthentication(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(conn)
		header := make([]byte, 2)
		if _, err = io.ReadFull(reader, header); err != nil {
			done <- err
			return
		}
		methods := make([]byte, int(header[1]))
		io.ReadFull(reader, methods)
		conn.Write([]byte{5, 2})
		io.ReadFull(reader, header)
		username := make([]byte, int(header[1]))
		io.ReadFull(reader, username)
		size, _ := reader.ReadByte()
		password := make([]byte, int(size))
		io.ReadFull(reader, password)
		if string(username) != "user" || string(password) != "pass" {
			done <- errors.New("wrong credentials")
			return
		}
		conn.Write([]byte{1, 0})
		request := make([]byte, 5)
		io.ReadFull(reader, request)
		if request[3] != 3 {
			done <- errors.New("target DNS was resolved locally")
			return
		}
		host := make([]byte, int(request[4]))
		io.ReadFull(reader, host)
		port := make([]byte, 2)
		io.ReadFull(reader, port)
		if string(host) != "unresolvable.invalid" || binary.BigEndian.Uint16(port) != 993 {
			done <- errors.New("wrong tunnel target")
			return
		}
		conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
		payload := make([]byte, 4)
		_, err = io.ReadFull(reader, payload)
		if err == nil {
			_, err = conn.Write(payload)
		}
		done <- err
	}()
	route := Route{Profile: profileFor(t, listener.Addr().String(), "socks5"), Password: "pass"}
	route.Profile.Username = "user"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := route.DialContext(ctx, "tcp", "unresolvable.invalid:993")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("test"))
	payload := make([]byte, 4)
	if _, err = io.ReadFull(conn, payload); err != nil || string(payload) != "test" {
		t.Fatal(string(payload), err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFailedProxyNeverFallsBackAndDirectIgnoresEnvironment(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		w.WriteHeader(204)
	}))
	defer target.Close()
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	badProxy := listener.Addr().String()
	listener.Close()
	for _, protocol := range []string{"http", "https", "socks5"} {
		route := Route{Profile: profileFor(t, badProxy, protocol)}
		client := HTTPClient(route.Transport(), time.Second)
		if response, err := client.Get(target.URL); err == nil {
			response.Body.Close()
			t.Fatal("failed proxy bypassed", protocol)
		}
	}
	if targetCalls.Load() != 0 {
		t.Fatal("unexpected direct fallback")
	}
	t.Setenv("HTTP_PROXY", "http://"+badProxy)
	t.Setenv("HTTPS_PROXY", "http://"+badProxy)
	t.Setenv("ALL_PROXY", "socks5://"+badProxy)
	client := HTTPClient((Route{}).Transport(), time.Second)
	response, err := client.Get(target.URL)
	if err != nil {
		t.Fatal("direct inherited environment", err)
	}
	response.Body.Close()
	if targetCalls.Load() != 1 {
		t.Fatal("direct not used")
	}
}

func TestHTTPSProxyCertificateAndCancellation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer server.Close()
	route := Route{Profile: profileFor(t, strings.TrimPrefix(server.URL, "https://"), "https")}
	_, err := route.DialContext(context.Background(), "tcp", "api.telegram.org:443")
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "proxy_tls_failed" {
		t.Fatal("untrusted proxy certificate accepted", err)
	}
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { <-request.Context().Done() }))
	defer proxyServer.Close()
	route.Profile = profileFor(t, strings.TrimPrefix(proxyServer.URL, "http://"), "http")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = route.DialContext(ctx, "tcp", "api.telegram.org:443")
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("CONNECT ignored cancellation", err)
	}
}

func TestHTTPSProxyTrustedTunnel(t *testing.T) {
	if endpoint := os.Getenv("MAIL_AUTO_TEST_HTTPS_PROXY"); endpoint != "" {
		route := Route{Profile: profileFor(t, endpoint, "https")}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, err := route.DialContext(ctx, "tcp", "imap.unresolvable.invalid:993")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(time.Second))
		conn.Write([]byte("test"))
		payload := make([]byte, 4)
		if _, err = io.ReadFull(conn, payload); err != nil || string(payload) != "test" {
			t.Fatal("HTTPS proxy tunnel failed", err)
		}
		return
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != "CONNECT" || request.Host != "imap.unresolvable.invalid:993" {
			w.WriteHeader(400)
			return
		}
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffered.Flush()
		io.Copy(conn, conn)
	}))
	defer server.Close()
	certificate := filepath.Join(t.TempDir(), "proxy-ca.pem")
	if err := os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestHTTPSProxyTrustedTunnel$", "-test.timeout=10s")
	command.Env = append(os.Environ(), "MAIL_AUTO_TEST_HTTPS_PROXY="+strings.TrimPrefix(server.URL, "https://"), "SSL_CERT_FILE="+certificate)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
}
