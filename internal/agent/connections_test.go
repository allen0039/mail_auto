package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"mail-auto/internal/mail"
	"mail-auto/internal/modelprovider"
	"mail-auto/internal/store"
)

// keyedModels records which key each probe used and lets a key fail.
type keyedModels struct {
	mu     sync.Mutex
	used   []string
	fail   map[string]error
	block  chan struct{}
	inner  *fakeModel
	probes int
}

func (k *keyedModels) factory(s modelprovider.Settings) (modelprovider.Provider, error) {
	return keyedProvider{k: k, key: s.APIKey + "|" + s.Model}, nil
}

type keyedProvider struct {
	k   *keyedModels
	key string
}

func (p keyedProvider) Complete(ctx context.Context, req modelprovider.Request) (modelprovider.Response, error) {
	p.k.mu.Lock()
	p.k.used = append(p.k.used, p.key)
	err := p.k.fail[p.key]
	block := p.k.block
	if len(req.Tools) == 1 && req.Tools[0].Name == "echo" {
		p.k.probes++
	}
	p.k.mu.Unlock()
	if block != nil {
		<-block
	}
	if err != nil {
		return modelprovider.Response{}, err
	}
	return p.k.inner.Complete(ctx, req)
}

func (k *keyedModels) last() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.used) == 0 {
		return ""
	}
	return k.used[len(k.used)-1]
}

func connEnv(t *testing.T) (*env, *keyedModels) {
	e := setup(t)
	k := &keyedModels{fail: map[string]error{}, inner: e.model}
	e.rt.NewProvider = k.factory
	return e, k
}

func activate(t *testing.T, e *env, in ConnectionInput) (ActivateResult, error) {
	t.Helper()
	p, _ := e.rt.Provider(e.f.Admin)
	return e.rt.Activate(context.Background(), e.f.Admin, ActivateInput{ConnectionInput: in, ProviderVersion: p.Version})
}

func TestTwoConnectionsSwitchWithoutMixingKeys(t *testing.T) {
	e, k := connEnv(t)
	a, err := activate(t, e, ConnectionInput{BaseURL: "https://a.example/v1", Model: "m-a", APIKey: "key-a"})
	if err != nil || !a.OK || a.Connection == nil || !a.Connection.Active || a.Connection.Name != "a.example" {
		t.Fatal(a, err)
	}
	if k.last() != "key-a|m-a" {
		t.Fatal("probe used wrong key", k.last())
	}
	b, err := activate(t, e, ConnectionInput{Name: "备用", BaseURL: "https://b.example/v1", Model: "m-b", APIKey: "key-b"})
	if err != nil || !b.OK || k.last() != "key-b|m-b" {
		t.Fatal(b, err, k.last())
	}
	list, _ := e.rt.Connections(e.f.Admin)
	if len(list.Connections) != 2 || list.Connections[0].Active || !list.Connections[1].Active || list.Provider.ConnectionID != b.Connection.ID || !list.Provider.Enabled || list.Provider.Model != "m-b" {
		t.Fatal(list)
	}
	// Switch back to A without re-entering its key; it keeps its last model.
	ca := list.Connections[0]
	back, err := activate(t, e, ConnectionInput{ID: ca.ID, Version: ca.Version, BaseURL: ca.BaseURL, Model: ca.Model})
	if err != nil || !back.OK || k.last() != "key-a|m-a" {
		t.Fatal(back, err, k.last())
	}
	// The runtime itself now talks to A with A's key.
	p, _ := e.rt.loadProvider()
	client, err := e.rt.providerClient(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Complete(context.Background(), modelprovider.Request{Messages: []modelprovider.Message{{Role: "user", Content: "x"}}}); err != nil || k.last() != "key-a|m-a" {
		t.Fatal("runtime key", k.last(), err)
	}
	// Ciphertext is bound to its own record: a connection secret does not
	// decrypt as the provider secret.
	c, _ := loadConnection(e.f.Store.DB, ca.ID)
	if _, err = e.rt.Vault.Decrypt(c.secret, providerAAD(p.instanceID)); err == nil {
		t.Fatal("connection ciphertext usable as provider secret")
	}
	view, _ := e.rt.Connections(e.f.Admin)
	for _, s := range []string{"key-a", "key-b"} {
		if strings.Contains(strings.Join([]string{view.Connections[0].BaseURL, view.Connections[1].Name}, ""), s) {
			t.Fatal("key leaked")
		}
	}
}

func TestFailedCheckKeepsCurrentConfiguration(t *testing.T) {
	e, k := connEnv(t)
	good, _ := activate(t, e, ConnectionInput{BaseURL: "https://a.example/v1", Model: "m-a", APIKey: "key-a"})
	before, _ := e.rt.Provider(e.f.Admin)
	for _, tc := range []struct {
		key  string
		err  error
		code string
	}{
		{"bad-key", &modelprovider.Error{Code: modelprovider.CodeAuth}, modelprovider.CodeAuth},
		{"slow-key", &modelprovider.Error{Code: modelprovider.CodeTimeout}, modelprovider.CodeTimeout},
	} {
		k.fail[tc.key+"|m-x"] = tc.err
		out, err := activate(t, e, ConnectionInput{BaseURL: "https://x.example/v1", Model: "m-x", APIKey: tc.key})
		if err != nil || out.OK || out.Result != tc.code {
			t.Fatal(out, err)
		}
	}
	// A chat-only model cannot be activated.
	e.rt.NewProvider = func(modelprovider.Settings) (modelprovider.Provider, error) { return chatOnly{}, nil }
	out, err := activate(t, e, ConnectionInput{ID: good.Connection.ID, Version: good.Connection.Version, BaseURL: "https://a.example/v1", Model: "chat-only"})
	if err != nil || out.Result != modelprovider.CodeToolUnsupported {
		t.Fatal(out, err)
	}
	after, _ := e.rt.Provider(e.f.Admin)
	list, _ := e.rt.Connections(e.f.Admin)
	if after.Version != before.Version || !after.Enabled || after.Model != "m-a" || len(list.Connections) != 1 || list.Connections[0].Model != "m-a" || list.Connections[0].Version != good.Connection.Version {
		t.Fatal("failed check changed state", before, after, list)
	}
}

func TestUnchangedResubmitKeepsGeneration(t *testing.T) {
	e, k := connEnv(t)
	a, _ := activate(t, e, ConnectionInput{BaseURL: "https://a.example/v1", Model: "m-a", APIKey: "key-a"})
	p1, _ := e.rt.Provider(e.f.Admin)
	probes := k.probes
	again, err := activate(t, e, ConnectionInput{ID: a.Connection.ID, Version: a.Connection.Version, BaseURL: "https://a.example/v1", Model: "m-a"})
	p2, _ := e.rt.Provider(e.f.Admin)
	if err != nil || !again.OK || !again.Unchanged || p2.Version != p1.Version || k.probes != probes {
		t.Fatal(again, err, p1.Version, p2.Version)
	}
	// Same model with the same key re-typed is still unchanged.
	again, _ = activate(t, e, ConnectionInput{ID: a.Connection.ID, Version: a.Connection.Version, BaseURL: "https://a.example/v1", Model: "m-a", APIKey: "key-a"})
	if !again.Unchanged {
		t.Fatal(again)
	}
	// A real model change is checked again and starts a new generation.
	changed, _ := activate(t, e, ConnectionInput{ID: a.Connection.ID, Version: a.Connection.Version, BaseURL: "https://a.example/v1", Model: "m-a2"})
	p3, _ := e.rt.Provider(e.f.Admin)
	if !changed.OK || changed.Unchanged || p3.Version != p1.Version+1 || k.probes == probes {
		t.Fatal(changed, p3)
	}
}

func TestConcurrentChangeRejectsStaleActivation(t *testing.T) {
	e, k := connEnv(t)
	a, _ := activate(t, e, ConnectionInput{BaseURL: "https://a.example/v1", Model: "m-a", APIKey: "key-a"})
	before, _ := e.rt.Provider(e.f.Admin)

	// 1. Another activation wins while a slow check is running.
	slow := make(chan struct{})
	k.block = slow
	done := make(chan error, 1)
	go func() {
		_, err := e.rt.Activate(context.Background(), e.f.Admin, ActivateInput{ConnectionInput: ConnectionInput{BaseURL: "https://slow.example/v1", Model: "m-s", APIKey: "key-s"}, ProviderVersion: before.Version})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	k.mu.Lock()
	k.block = nil
	k.mu.Unlock()
	winner, err := activate(t, e, ConnectionInput{BaseURL: "https://b.example/v1", Model: "m-b", APIKey: "key-b"})
	if err != nil || !winner.OK {
		t.Fatal(winner, err)
	}
	// Release the slow probe: it must not activate its now-stale candidate.
	close(slow)
	if err := <-done; !errors.Is(err, mail.ErrConflict) {
		t.Fatal("stale activation applied", err)
	}
	p, _ := e.rt.Provider(e.f.Admin)
	if p.Model != "m-b" || p.ConnectionID != winner.Connection.ID {
		t.Fatal(p)
	}
	list, _ := e.rt.Connections(e.f.Admin)
	for _, c := range list.Connections {
		if c.Model == "m-s" {
			t.Fatal("stale candidate saved", c)
		}
	}

	// 2. An old page version is rejected before any request.
	if _, err := e.rt.Activate(context.Background(), e.f.Admin, ActivateInput{ConnectionInput: ConnectionInput{ID: a.Connection.ID, Version: a.Connection.Version, BaseURL: "https://a.example/v1", Model: "m-a"}, ProviderVersion: before.Version}); !errors.Is(err, mail.ErrConflict) {
		t.Fatal("old provider version accepted", err)
	}
	// 3. The connection edited elsewhere: the old connection version loses.
	cur, _ := e.rt.Provider(e.f.Admin)
	edited, err := e.rt.SaveConnection(e.f.Admin, ConnectionInput{ID: a.Connection.ID, Version: a.Connection.Version, Name: "改名", BaseURL: "https://a.example/v1", Model: "m-a3"})
	if err != nil || edited.Version != a.Connection.Version+1 {
		t.Fatal(edited, err)
	}
	if _, err = e.rt.Activate(context.Background(), e.f.Admin, ActivateInput{ConnectionInput: ConnectionInput{ID: a.Connection.ID, Version: a.Connection.Version, BaseURL: "https://a.example/v1", Model: "m-a"}, ProviderVersion: cur.Version}); !errors.Is(err, mail.ErrConflict) {
		t.Fatal("stale connection version accepted", err)
	}

	// 4. A network egress change during a check rejects the result.
	k.block = make(chan struct{})
	go func() {
		_, err := e.rt.Activate(context.Background(), e.f.Admin, ActivateInput{ConnectionInput: ConnectionInput{ID: edited.ID, Version: edited.Version, BaseURL: "https://a.example/v1", Model: "m-a3"}, ProviderVersion: cur.Version})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err = e.f.Store.DB.Exec("UPDATE agent_provider_settings SET version=version+1,enabled=0,capabilities='{}' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	releaseAll(k)
	if err := <-done; !errors.Is(err, mail.ErrConflict) {
		t.Fatal("result applied after egress change", err)
	}
}

func releaseAll(k *keyedModels) {
	k.mu.Lock()
	b := k.block
	k.block = nil
	k.mu.Unlock()
	if b != nil {
		close(b)
	}
}

func TestSwitchInvalidatesFrozenProposals(t *testing.T) {
	e, _ := connEnv(t)
	e.draftAndPropose(t)
	var pending int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM agent_proposals WHERE state='pending'").Scan(&pending)
	if pending == 0 {
		t.Fatal("fixture created no proposal")
	}
	if out, err := activate(t, e, ConnectionInput{BaseURL: "https://b.example/v1", Model: "m-b", APIKey: "key-b"}); err != nil || !out.OK {
		t.Fatal(out, err)
	}
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM agent_proposals WHERE state='pending'").Scan(&pending)
	if pending != 0 {
		t.Fatal("proposal from old configuration still pending")
	}
}

func TestKeyStaysWithItsOriginAndDeleteRules(t *testing.T) {
	e, _ := connEnv(t)
	a, _ := activate(t, e, ConnectionInput{BaseURL: "https://a.example/v1", Model: "m-a", APIKey: "key-a"})
	// Path change keeps the key; host change requires a new one.
	if _, err := e.rt.SaveConnection(e.f.Admin, ConnectionInput{ID: a.Connection.ID, Version: a.Connection.Version, BaseURL: "https://evil.example/v1", Model: "m-a"}); !errors.Is(err, ErrKeyRequired) {
		t.Fatal("stored key moved to another host", err)
	}
	var sent []string
	e.rt.ModelLister = func(_ context.Context, base, key string) ([]string, error) {
		sent = append(sent, base+"#"+key)
		return []string{"m-1", "m-2"}, nil
	}
	if _, err := e.rt.ListModels(context.Background(), e.f.Admin, ModelsInput{ID: a.Connection.ID, BaseURL: "https://evil.example/v1"}); !errors.Is(err, ErrKeyRequired) || len(sent) != 0 {
		t.Fatal("list sent stored key to another host", err, sent)
	}
	models, err := e.rt.ListModels(context.Background(), e.f.Admin, ModelsInput{ID: a.Connection.ID, BaseURL: "https://a.example/v2"})
	if err != nil || len(models) != 2 || sent[0] != "https://a.example/v2#key-a" {
		t.Fatal(models, err, sent)
	}
	if _, err = e.rt.ListModels(context.Background(), e.f.Admin, ModelsInput{BaseURL: "https://n.example/v1"}); !errors.Is(err, ErrKeyRequired) {
		t.Fatal(err)
	}
	// Saving a backup connection never changes the current provider.
	before, _ := e.rt.Provider(e.f.Admin)
	backup, err := e.rt.SaveConnection(e.f.Admin, ConnectionInput{BaseURL: "https://b.example/v1", Model: "m-b", APIKey: "key-b"})
	if err != nil || backup.Active {
		t.Fatal(backup, err)
	}
	edit, err := e.rt.SaveConnection(e.f.Admin, ConnectionInput{ID: a.Connection.ID, Version: a.Connection.Version, BaseURL: "https://a.example/v1", Model: "m-new"})
	if err != nil || !edit.Pending {
		t.Fatal("edit of active connection not shown as pending", edit, err)
	}
	after, _ := e.rt.Provider(e.f.Admin)
	if after.Version != before.Version || after.Model != "m-a" {
		t.Fatal("saving changed current provider", before, after)
	}
	// The connection in use cannot be deleted; a backup can.
	if err = e.rt.DeleteConnection(e.f.Admin, a.Connection.ID, edit.Version); !errors.Is(err, ErrConnectionInUse) {
		t.Fatal(err)
	}
	if err = e.rt.DeleteConnection(e.f.Admin, backup.ID, backup.Version+5); !errors.Is(err, mail.ErrConflict) {
		t.Fatal(err)
	}
	if err = e.rt.DeleteConnection(e.f.Admin, backup.ID, backup.Version); err != nil {
		t.Fatal(err)
	}
	// After disabling, the formerly active one can be deleted; the provider
	// keeps its values and only loses the link.
	p := after
	if _, err = e.rt.SaveProvider(e.f.Admin, ProviderInput{Version: p.Version, Enabled: false, BaseURL: p.BaseURL, Model: p.Model}); err != nil {
		t.Fatal(err)
	}
	if err = e.rt.DeleteConnection(e.f.Admin, a.Connection.ID, edit.Version); err != nil {
		t.Fatal(err)
	}
	final, _ := e.rt.Provider(e.f.Admin)
	if final.ConnectionID != "" || final.Model != "m-a" || !final.KeyConfigured {
		t.Fatal(final)
	}
}

func TestLegacyProviderMigratesToConnection(t *testing.T) {
	e, k := connEnv(t)
	before, _ := e.rt.Provider(e.f.Admin)
	if _, err := e.f.Store.DB.Exec("UPDATE agent_provider_settings SET connection_id=?", store.LegacyConnection); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := e.rt.MigrateConnections(); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := e.rt.Connections(e.f.Admin)
	after := list.Provider
	if len(list.Connections) != 1 || !list.Connections[0].Active || list.Connections[0].Pending || !list.Connections[0].KeyConfigured || list.Connections[0].Model != "fake" {
		t.Fatal(list)
	}
	if after.Version != before.Version || !after.Enabled || !after.Capabilities.Current || after.Capabilities.ToolLoop != "ok" {
		t.Fatal("migration changed running state", before, after)
	}
	c, _ := loadConnection(e.f.Store.DB, list.Connections[0].ID)
	if key, err := e.rt.Vault.Decrypt(c.secret, connectionAAD(c.id)); err != nil || key != "sk-test" {
		t.Fatal("migrated key", key, err)
	}
	// The migrated connection can be used without re-entering the key, and
	// re-submitting it unchanged does not start a new generation.
	again, err := activate(t, e, ConnectionInput{ID: c.id, Version: c.version, BaseURL: c.cfg.BaseURL, Model: c.cfg.Model})
	if err != nil || !again.Unchanged {
		t.Fatal(again, err)
	}
	if _, err = activate(t, e, ConnectionInput{ID: c.id, Version: c.version, BaseURL: c.cfg.BaseURL, Model: "fake-2"}); err != nil || k.last() != "sk-test|fake-2" {
		t.Fatal(err, k.last())
	}
}

func TestEmptyLegacyProviderMigratesWithoutConnection(t *testing.T) {
	e := setup(t)
	if _, err := e.f.Store.DB.Exec("UPDATE agent_provider_settings SET connection_id=?,config='{\"api_type\":\"chat_completions\",\"timeout_seconds\":60}'", store.LegacyConnection); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.MigrateConnections(); err != nil {
		t.Fatal(err)
	}
	list, _ := e.rt.Connections(e.f.Admin)
	if len(list.Connections) != 0 || list.Provider.ConnectionID != "" {
		t.Fatal(list)
	}
}

// MC-01: the administrator is re-checked in the commit transaction. A
// credential change (version bump) or a forced change (restricted) while the
// probe runs rejects the result; nothing is saved or applied.
func TestAdminChangeDuringCheckRejectsActivation(t *testing.T) {
	for _, change := range []string{"UPDATE admin SET version=version+1 WHERE id=1", "UPDATE admin SET must_change=1 WHERE id=1"} {
		e, k := connEnv(t)
		before, _ := e.rt.Provider(e.f.Admin)
		var conns int
		e.f.Store.DB.QueryRow("SELECT count(*) FROM agent_provider_connections").Scan(&conns)
		k.block = make(chan struct{})
		done := make(chan error, 1)
		go func() {
			out, err := e.rt.Activate(context.Background(), e.f.Admin, ActivateInput{ConnectionInput: ConnectionInput{BaseURL: "https://n.example/v1", Model: "m-n", APIKey: "key-n"}, ProviderVersion: before.Version})
			if err == nil && out.OK {
				err = errors.New("accepted")
			}
			done <- err
		}()
		time.Sleep(50 * time.Millisecond)
		if _, err := e.f.Store.DB.Exec(change); err != nil {
			t.Fatal(err)
		}
		releaseAll(k)
		if err := <-done; !errors.Is(err, mail.ErrDenied) {
			t.Fatal(change, "stale administrator activation", err)
		}
		var after, afterConns int
		var model string
		e.f.Store.DB.QueryRow("SELECT version,json_extract(config,'$.model') FROM agent_provider_settings").Scan(&after, &model)
		e.f.Store.DB.QueryRow("SELECT count(*) FROM agent_provider_connections").Scan(&afterConns)
		if after != before.Version || model != "fake" || afterConns != conns {
			t.Fatal(change, "state changed", after, model, afterConns)
		}
		// Saving and deleting with the old access are refused as well.
		if _, err := e.rt.SaveConnection(e.f.Admin, ConnectionInput{BaseURL: "https://n.example/v1", Model: "m", APIKey: "k"}); !errors.Is(err, mail.ErrDenied) {
			t.Fatal(change, "stale save", err)
		}
	}
}

// MC-04: a saved key change on the connection in use is pending until it
// is checked and applied; the runtime keeps the old key meanwhile.
func TestKeyOnlyChangeIsPendingUntilApplied(t *testing.T) {
	e, k := connEnv(t)
	a, _ := activate(t, e, ConnectionInput{BaseURL: "https://a.example/v1", Model: "m-a", APIKey: "key-old"})
	if a.Connection.Pending {
		t.Fatal("pending right after activation")
	}
	saved, err := e.rt.SaveConnection(e.f.Admin, ConnectionInput{ID: a.Connection.ID, Version: a.Connection.Version, BaseURL: "https://a.example/v1", Model: "m-a", APIKey: "key-new"})
	if err != nil || !saved.Pending || !saved.Active {
		t.Fatal("key-only change not pending", saved, err)
	}
	list, _ := e.rt.Connections(e.f.Admin)
	if !list.Connections[0].Pending {
		t.Fatal("list does not show pending key change")
	}
	p, _ := e.rt.loadProvider()
	client, _ := e.rt.providerClient(p)
	client.Complete(context.Background(), modelprovider.Request{Messages: []modelprovider.Message{{Role: "user", Content: "x"}}})
	if k.last() != "key-old|m-a" {
		t.Fatal("runtime switched key before apply", k.last())
	}
	// Saving the same key again is not a change.
	same, _ := e.rt.SaveConnection(e.f.Admin, ConnectionInput{ID: saved.ID, Version: saved.Version, BaseURL: "https://a.example/v1", Model: "m-a", APIKey: "key-new"})
	if same.Version != saved.Version || !same.Pending {
		t.Fatal(same)
	}
	// The resubmit is not "unchanged": it is checked with the new key.
	out, err := activate(t, e, ConnectionInput{ID: saved.ID, Version: saved.Version, BaseURL: "https://a.example/v1", Model: "m-a"})
	if err != nil || !out.OK || out.Unchanged || out.Connection.Pending || k.last() != "key-new|m-a" {
		t.Fatal(out, err, k.last())
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "key-new") || strings.Contains(string(raw), "key-old") {
		t.Fatal("key leaked")
	}
}
