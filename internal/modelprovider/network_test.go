package modelprovider

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"mail-auto/internal/network"
)

func TestProxyDestinationGuardAndTrustedRemoteDNS(t *testing.T) {
	var calls atomic.Int32
	destinations := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		destinations <- request.Host
		w.WriteHeader(502)
	}))
	defer proxy.Close()
	host, portString, _ := net.SplitHostPort(strings.TrimPrefix(proxy.URL, "http://"))
	port, _ := strconv.Atoi(portString)
	route := network.Route{Profile: &network.Profile{Protocol: "http", Host: host, Port: port, TrustedRemoteDNS: true}}
	transport := ProxyTransport(route)
	for _, target := range []string{"127.0.0.1:443", "[::1]:443", "169.254.169.254:443", "localhost:443", "10.0.0.1:443"} {
		conn, err := transport.DialContext(context.Background(), "tcp", target)
		if err == nil {
			conn.Close()
			t.Fatal("private model target allowed", target)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("guard forwarded private model target")
	}
	_, err := transport.DialContext(context.Background(), "tcp", "model.unresolvable.invalid:443")
	if err == nil || calls.Load() != 1 {
		t.Fatal("trusted proxy did not resolve model hostname", err, calls.Load())
	}
	if destination := <-destinations; destination != "model.unresolvable.invalid:443" {
		t.Fatal("wrong proxy target", destination)
	}
	route.Profile.TrustedRemoteDNS = false
	_, err = ProxyTransport(route).DialContext(context.Background(), "tcp", "model.unresolvable.invalid:443")
	if err == nil || calls.Load() != 1 {
		t.Fatal("local DNS validation unexpectedly bypassed", err)
	}
}
