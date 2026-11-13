package modelprovider

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"mail-auto/internal/network"
)

func ProxyTransport(route network.Route) *http.Transport {
	if route.Profile == nil {
		return GuardedTransport()
	}
	transport := route.Transport()
	transport.DialContext = func(ctx context.Context, protocol, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || host == "" || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
			return nil, &Error{Code: CodeDestination}
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			if !PublicAddress(ip) {
				return nil, &Error{Code: CodeDestination}
			}
			return route.DialContext(ctx, protocol, address)
		}
		if route.Profile.TrustedRemoteDNS {
			return route.DialContext(ctx, protocol, address)
		}
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(addresses) == 0 {
			return nil, &Error{Code: CodeNetwork}
		}
		for _, ip := range addresses {
			if !PublicAddress(ip) {
				return nil, &Error{Code: CodeDestination}
			}
		}
		return route.DialContext(ctx, protocol, net.JoinHostPort(addresses[0].String(), port))
	}
	return transport
}
