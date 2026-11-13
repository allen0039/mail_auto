package network

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vault, err := credentials.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(st, vault)
	t.Cleanup(func() { manager.CloseIdle(); st.Close() })
	return manager
}

func inputFrom(settings Settings) Input {
	in := Input{Version: settings.Version, Global: settings.Global, Telegram: settings.Telegram, Agent: settings.Agent}
	for _, profile := range settings.Profiles {
		in.Profiles = append(in.Profiles, ProfileInput{Profile: profile})
	}
	return in
}

func TestPoliciesEncryptionReferencesAndNoEnvironment(t *testing.T) {
	manager := testManager(t)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	for _, scope := range []string{"global", "telegram", "agent", "account"} {
		route, err := manager.Resolve(scope, "mail")
		if err != nil || route.Profile != nil || route.Transport().Proxy != nil {
			t.Fatalf("default %s: %#v %v", scope, route, err)
		}
	}
	settings, _ := manager.Settings()
	in := inputFrom(settings)
	in.Profiles = []ProfileInput{{Profile: Profile{ID: "nas", Name: "NAS", Protocol: "socks5h", Host: "proxy", Port: 1080, Username: "user"}, Password: "synthetic-proxy-password"}}
	in.Global = Policy{Mode: "proxy", ProxyID: "nas"}
	settings, err := manager.Save(in)
	if err != nil {
		t.Fatal(err)
	}
	serialized, _ := json.Marshal(settings)
	if strings.Contains(string(serialized), "synthetic-proxy-password") || !settings.Profiles[0].PasswordSet || settings.Profiles[0].Protocol != "socks5" {
		t.Fatal("credential leaked or invalid normalized view", string(serialized))
	}
	var stored []byte
	if err = manager.Store.DB.QueryRow("SELECT secret FROM proxy_profiles WHERE id='nas'").Scan(&stored); err != nil || strings.Contains(string(stored), "synthetic-proxy-password") {
		t.Fatal("plaintext password stored", err)
	}
	in = inputFrom(settings)
	in.Telegram = Policy{Mode: "direct"}
	settings, err = manager.Save(in)
	if err != nil {
		t.Fatal(err)
	}
	route, err := manager.Resolve("account", "mail")
	if err != nil || route.Profile == nil || route.Password != "synthetic-proxy-password" {
		t.Fatal("inherit/retained credential", route.Label(), err)
	}
	originalKey := route.Key
	tg, _ := manager.Resolve("telegram", "")
	if tg.Profile != nil {
		t.Fatal("forced direct lost")
	}
	if _, err = manager.Save(in); !errors.Is(err, ErrConflict) {
		t.Fatal("stale update accepted", err)
	}
	if _, err = manager.Store.DB.Exec("INSERT INTO accounts VALUES('mail','mail','qq','test@qq.com','imap.qq.com',993,'[]',NULL,'',1)"); err != nil {
		t.Fatal(err)
	}
	tx, _ := manager.Store.DB.Begin()
	if err = SetAccount(tx, "mail", Policy{Mode: "proxy", ProxyID: "nas"}); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	in = inputFrom(settings)
	in.Global = Policy{Mode: "direct"}
	in.Profiles = nil
	if _, err = manager.Save(in); !errors.Is(err, ErrReferenced) {
		t.Fatal("deleted referenced proxy", err)
	}
	in = inputFrom(settings)
	in.Profiles[0].Password = "replacement-test-password"
	settings, err = manager.Save(in)
	if err != nil {
		t.Fatal(err)
	}
	route, _ = manager.Resolve("account", "mail")
	if route.Key == originalKey || route.Password != "replacement-test-password" {
		t.Fatal("credential change did not change effective generation")
	}
	in = inputFrom(settings)
	in.Profiles[0].ClearPassword = true
	in.Profiles[0].Username = ""
	if _, err = manager.Save(in); err != nil {
		t.Fatal(err)
	}
	route, _ = manager.Resolve("account", "mail")
	if route.Password != "" {
		t.Fatal("clear did not clear credential")
	}
}

func TestProxyValidationAndTransactionalAgentFence(t *testing.T) {
	manager := testManager(t)
	settings, _ := manager.Settings()
	for _, host := range []string{"http://proxy", "proxy:1080", "user@proxy", "proxy\ninjected", "proxy/path", "proxy?x", "[::1]", "-bad.host"} {
		in := inputFrom(settings)
		in.Profiles = []ProfileInput{{Profile: Profile{Name: "test", Host: host, Protocol: "http", Port: 8080}}}
		if _, err := manager.Save(in); !errors.Is(err, ErrConfig) {
			t.Fatalf("accepted host %q: %v", host, err)
		}
	}
	if _, err := manager.Store.DB.Exec(`INSERT INTO agent_provider_settings(id,version,enabled,instance_id,config,secret,key_generation,capabilities,updated_at)
VALUES(1,7,1,'instance','{}',x'',1,'{"tool_loop":"ok"}',1)`); err != nil {
		t.Fatal(err)
	}
	in := inputFrom(settings)
	in.Profiles = []ProfileInput{{Profile: Profile{ID: "p", Name: "private proxy", Host: "192.168.1.2", Protocol: "http", Port: 7890}}}
	in.Agent = Policy{Mode: "proxy", ProxyID: "p"}
	settings, err := manager.Save(in)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	var enabled bool
	var capabilities string
	manager.Store.DB.QueryRow("SELECT version,enabled,capabilities FROM agent_provider_settings").Scan(&version, &enabled, &capabilities)
	if version != 8 || enabled || capabilities != "{}" {
		t.Fatal("agent was not fenced", version, enabled, capabilities)
	}
	in = inputFrom(settings)
	in.Telegram = Policy{Mode: "proxy", ProxyID: "p"}
	if _, err = manager.Save(in); err != nil {
		t.Fatal(err)
	}
	manager.Store.DB.QueryRow("SELECT version FROM agent_provider_settings").Scan(&version)
	if version != 8 {
		t.Fatal("unrelated TG change invalidated agent")
	}
}
