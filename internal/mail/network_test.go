package mail

import (
	"context"
	"testing"

	"mail-auto/internal/network"
)

func TestAccountProxyPreservesIndexAndGlobalRevision(t *testing.T) {
	service, account, input := updateService(t)
	service.Network = network.New(service.Store, service.Vault)
	settings, _ := service.Network.Settings()
	in := network.Input{Version: settings.Version, Global: settings.Global, Telegram: settings.Telegram, Agent: settings.Agent,
		Profiles: []network.ProfileInput{{Profile: network.Profile{ID: "nas", Name: "NAS", Host: "192.168.1.20", Port: 7890, Protocol: "http"}}}}
	settings, err := service.Network.Save(in)
	if err != nil {
		t.Fatal(err)
	}
	var before int
	service.Store.DB.QueryRow("SELECT count(*) FROM messages WHERE account_id=?", account.ID).Scan(&before)
	current, _ := service.Account(account.ID)
	if current.NetworkRevision != "direct" || current.Proxy.Mode != "inherit" {
		t.Fatal("wrong defaults", current.Proxy, current.NetworkRevision)
	}
	input.Proxy = &network.Policy{Mode: "proxy", ProxyID: "nas"}
	updated, err := service.Update(account.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	var after int
	service.Store.DB.QueryRow("SELECT count(*) FROM messages WHERE account_id=?", account.ID).Scan(&after)
	if before != after || updated.NetworkRevision == current.NetworkRevision {
		t.Fatal("proxy changed identity/index or failed revision", before, after)
	}
	input.Proxy = &network.Policy{Mode: "inherit"}
	updated, err = service.Update(account.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	in.Version = settings.Version
	in.Global = network.Policy{Mode: "proxy", ProxyID: "nas"}
	if _, err = service.Network.Save(in); err != nil {
		t.Fatal(err)
	}
	inherited, err := service.automaticAccounts(context.Background())
	if err != nil || len(inherited) == 0 || inherited[0].NetworkRevision == updated.NetworkRevision {
		t.Fatal("global update failed to update automatic worker generation", err)
	}
}
