package agent

import (
	"context"
	"sync"
	"testing"

	"mail-auto/internal/modelprovider"
)

// The admin page no longer shows the output-length parameter; it submits the
// stored value back unchanged. These checks pin that a stored value (empty or
// either parameter name) survives every page action and keeps reaching the
// model client as before.
func TestMaxTokensParamSurvivesPageActions(t *testing.T) {
	for _, param := range []string{"", "max_tokens", "max_completion_tokens"} {
		t.Run("param="+param, func(t *testing.T) {
			e, k := connEnv(t)
			var mu sync.Mutex
			var seen []string
			inner := k.factory
			e.rt.NewProvider = func(s modelprovider.Settings) (modelprovider.Provider, error) {
				mu.Lock()
				seen = append(seen, s.MaxTokensParam)
				mu.Unlock()
				return inner(s)
			}
			lastSeen := func() string {
				mu.Lock()
				defer mu.Unlock()
				if len(seen) == 0 {
					return "<none>"
				}
				return seen[len(seen)-1]
			}
			stored := func(id string) (Connection, ProviderSettings) {
				t.Helper()
				list, err := e.rt.Connections(e.f.Admin)
				if err != nil {
					t.Fatal(err)
				}
				for _, c := range list.Connections {
					if c.ID == id {
						return c, list.Provider
					}
				}
				t.Fatal("connection missing", id)
				return Connection{}, list.Provider
			}

			// An existing connection carrying the value (set through the API,
			// as an older page or migration left it).
			a, err := activate(t, e, ConnectionInput{BaseURL: "https://a.example/v1", Model: "m-a", APIKey: "key-a", MaxTokensParam: param})
			if err != nil || !a.OK || lastSeen() != param {
				t.Fatal("activate", a, err, lastSeen())
			}
			// A new connection saved as backup sends nothing (page default).
			b, err := e.rt.SaveConnection(e.f.Admin, ConnectionInput{BaseURL: "https://b.example/v1", Model: "m-b", APIKey: "key-b"})
			if err != nil || b.MaxTokensParam != "" {
				t.Fatal("backup", b, err)
			}
			// Editing A from the page resubmits the stored value.
			ca, _ := stored(a.Connection.ID)
			edited, err := e.rt.SaveConnection(e.f.Admin, ConnectionInput{ID: ca.ID, Version: ca.Version, Name: "改名", BaseURL: ca.BaseURL, Model: ca.Model, TimeoutSeconds: ca.TimeoutSeconds, MaxTokensParam: ca.MaxTokensParam})
			if err != nil || edited.MaxTokensParam != param || edited.Pending {
				t.Fatal("edit", edited, err)
			}
			// The applied config resubmitted as-is stays unchanged.
			p1, _ := e.rt.Provider(e.f.Admin)
			probes := k.probes
			same, err := e.rt.Activate(context.Background(), e.f.Admin, ActivateInput{ConnectionInput: ConnectionInput{ID: edited.ID, Version: edited.Version, Name: edited.Name, BaseURL: edited.BaseURL, Model: edited.Model, TimeoutSeconds: edited.TimeoutSeconds, MaxTokensParam: edited.MaxTokensParam}, ProviderVersion: p1.Version})
			p2, _ := e.rt.Provider(e.f.Admin)
			if err != nil || !same.Unchanged || p2.Version != p1.Version || k.probes != probes {
				t.Fatal("unchanged", same, err, p1.Version, p2.Version)
			}
			// Switch to B and back to A.
			cb, _ := stored(b.ID)
			sw, err := activate(t, e, ConnectionInput{ID: cb.ID, Version: cb.Version, BaseURL: cb.BaseURL, Model: cb.Model, TimeoutSeconds: cb.TimeoutSeconds, MaxTokensParam: cb.MaxTokensParam})
			if err != nil || !sw.OK || lastSeen() != "" || sw.Provider.MaxTokensParam != "" {
				t.Fatal("switch to b", sw, err, lastSeen())
			}
			ca, _ = stored(a.Connection.ID)
			back, err := activate(t, e, ConnectionInput{ID: ca.ID, Version: ca.Version, BaseURL: ca.BaseURL, Model: ca.Model, TimeoutSeconds: ca.TimeoutSeconds, MaxTokensParam: ca.MaxTokensParam})
			if err != nil || !back.OK || lastSeen() != param || back.Provider.MaxTokensParam != param || back.Connection.MaxTokensParam != param {
				t.Fatal("switch back", back, err, lastSeen())
			}
			// Disabling (the page resubmits the provider's own value).
			p, _ := e.rt.Provider(e.f.Admin)
			off, err := e.rt.SaveProvider(e.f.Admin, ProviderInput{Version: p.Version, Enabled: false, BaseURL: p.BaseURL, Model: p.Model, TimeoutSeconds: p.TimeoutSeconds, MaxTokensParam: p.MaxTokensParam})
			if err != nil || off.Enabled || off.MaxTokensParam != param || off.ConnectionID != a.Connection.ID {
				t.Fatal("disable", off, err)
			}
			ca, _ = stored(a.Connection.ID)
			if ca.MaxTokensParam != param || ca.Pending {
				t.Fatal("connection after disable", ca)
			}
			// The runtime client still gets the stored name.
			sp, _ := e.rt.loadProvider()
			if _, err := e.rt.providerClient(sp); err != nil || lastSeen() != param {
				t.Fatal("runtime client", err, lastSeen())
			}
		})
	}
}
