package modelprovider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

var errBlockedDestination = errors.New("destination blocked")

// blockedPrefixes are non-public ranges not covered by netip helpers.
var blockedPrefixes = func() []netip.Prefix {
	out := []netip.Prefix{}
	for _, p := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "255.255.255.255/32", "64:ff9b::/96", "64:ff9b:1::/48", "2001:db8::/32", "2002::/16", "100::/64", "fec0::/10"} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// PublicAddress reports whether ip is a routable public unicast address.
// Loopback, private, link-local (including cloud metadata 169.254.169.254),
// CGNAT, multicast, documentation, NAT64 and 6to4 ranges are rejected; an
// IPv4-mapped IPv6 address is checked as IPv4.
func PublicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// guardControl runs on every actual connection attempt with the resolved
// address, so DNS rebinding between validation and connect is still caught.
func guardControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errBlockedDestination
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !PublicAddress(ip) {
		return errBlockedDestination
	}
	return nil
}

// GuardedTransport is the production transport: no environment proxy, no
// connection to non-public addresses, bounded handshake and header waits.
func GuardedTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: guardControl}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       60 * time.Second,
	}
}

// NewHTTPClient returns a client that never follows redirects, so an
// Authorization header can never be forwarded to another destination.
func NewHTTPClient(rt http.RoundTripper) *http.Client {
	if rt == nil {
		rt = GuardedTransport()
	}
	return &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// NormalizeBaseURL validates an administrator-entered API root and returns it
// without a trailing slash together with the final chat completions URL.
// Only https is allowed; userinfo, query and fragment are rejected, and a
// path already ending in /chat/completions is rejected to avoid duplication.
func NormalizeBaseURL(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 512 || strings.ContainsAny(raw, " \t\r\n\x00") {
		return "", "", &Error{Code: CodeConfig}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", "", &Error{Code: CodeConfig}
	}
	host := u.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return "", "", &Error{Code: CodeDestination}
	}
	if ip, err := netip.ParseAddr(host); err == nil && !PublicAddress(ip) {
		return "", "", &Error{Code: CodeDestination}
	}
	path := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(path, "/chat/completions") {
		return "", "", &Error{Code: CodeConfig}
	}
	base := u.Scheme + "://" + u.Host + path
	return base, base + "/chat/completions", nil
}

// CheckResolution resolves host and rejects any non-public answer. It gives
// a clear configuration error early; the dial-time check remains authoritative.
func CheckResolution(ctx context.Context, base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return &Error{Code: CodeConfig}
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		if !PublicAddress(ip) {
			return &Error{Code: CodeDestination}
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return &Error{Code: CodeNetwork}
	}
	for _, a := range addrs {
		if !PublicAddress(a) {
			return &Error{Code: CodeDestination}
		}
	}
	return nil
}
