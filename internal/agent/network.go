package agent

import (
	"context"
	"net"
	"net/url"

	"mail-auto/internal/mail"
	"mail-auto/internal/modelprovider"
	"mail-auto/internal/network"
)

func (r *Runtime) NetworkTarget(a mail.Access) (string, error) {
	if err := r.requireAdmin(a); err != nil {
		return "", err
	}
	provider, err := r.loadProvider()
	if err != nil {
		return "", err
	}
	base, _, err := modelprovider.NormalizeBaseURL(provider.cfg.BaseURL)
	if err != nil {
		return "", err
	}
	target, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	port := target.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(target.Hostname(), port), nil
}

func (r *Runtime) DiagnoseNetwork(ctx context.Context, a mail.Access, route network.Route) error {
	if err := r.requireAdmin(a); err != nil {
		return err
	}
	provider, err := r.loadProvider()
	if err != nil {
		return err
	}
	client, err := r.providerClientRoute(provider, route)
	if err != nil {
		return err
	}
	return modelprovider.ProbeChat(ctx, client)
}
