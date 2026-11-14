package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"mail-auto/internal/mail"
	"mail-auto/internal/modelprovider"
)

type authFails struct{}

func (authFails) Complete(context.Context, modelprovider.Request) (modelprovider.Response, error) {
	return modelprovider.Response{}, &modelprovider.Error{Code: modelprovider.CodeAuth}
}

func TestModelConnectionAPI(t *testing.T) {
	b, server := loggedIn(t)
	anon := &browser{t: t, h: server.Handler(), cookies: map[string]*http.Cookie{}}
	if w := anon.request("GET", "/api/agent/connections", "", false); w.Code != 401 {
		t.Fatal("unauthenticated", w.Code)
	}
	save := `{"name":"A","base_url":"https://a.example/v1","model":"m","api_key":"sk-secret-connection"}`
	if w := b.request("POST", "/api/agent/connections", save, false); w.Code != 403 {
		t.Fatal("CSRF bypass", w.Code)
	}
	w := b.request("POST", "/api/agent/connections", save, true)
	if w.Code != 200 || strings.Contains(w.Body.String(), "sk-secret") {
		t.Fatal(w.Code, w.Body)
	}
	var c struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
		Active  bool   `json:"active"`
	}
	json.Unmarshal(w.Body.Bytes(), &c)
	if c.ID == "" || c.Active {
		t.Fatal(w.Body)
	}
	if w = b.request("GET", "/api/agent/connections", "", false); w.Code != 200 || strings.Contains(w.Body.String(), "sk-secret") || !strings.Contains(w.Body.String(), `"key_configured":true`) {
		t.Fatal(w.Code, w.Body)
	}
	// A failed check is a normal answer with a Chinese explanation and the
	// current configuration unchanged.
	server.Agent.NewProvider = func(modelprovider.Settings) (modelprovider.Provider, error) { return authFails{}, nil }
	w = b.request("POST", "/api/agent/connections/activate", `{"id":"`+c.ID+`","version":1,"base_url":"https://a.example/v1","model":"m","provider_version":0}`, true)
	var out struct {
		OK      bool   `json:"ok"`
		Result  string `json:"result"`
		Message string `json:"message"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out.OK || out.Result != modelprovider.CodeAuth || !strings.Contains(out.Message, "API Key") {
		t.Fatal(w.Code, w.Body)
	}
	if p, _ := server.Agent.Provider(mustAdmin(t, server)); p.Enabled || p.Version != 0 {
		t.Fatal("failed check changed provider", p)
	}
	// Host change without key, stale page version, and model list without key.
	if w = b.request("POST", "/api/agent/connections", `{"id":"`+c.ID+`","version":1,"base_url":"https://other.example/v1","model":"m"}`, true); w.Code != 422 || !strings.Contains(w.Body.String(), "key_required") {
		t.Fatal(w.Code, w.Body)
	}
	if w = b.request("POST", "/api/agent/connections/activate", `{"id":"`+c.ID+`","version":1,"base_url":"https://a.example/v1","model":"m","provider_version":9}`, true); w.Code != 409 {
		t.Fatal("stale page accepted", w.Code)
	}
	if w = b.request("POST", "/api/agent/connections/models", `{"base_url":"https://other.example/v1"}`, true); w.Code != 422 {
		t.Fatal(w.Code, w.Body)
	}
	if w = b.request("POST", "/api/agent/connections/delete", `{"id":"`+c.ID+`","version":1}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
}

func mustAdmin(t *testing.T, s *Server) mail.Access {
	t.Helper()
	a, err := s.Mail.AdminAccess()
	if err != nil {
		t.Fatal(err)
	}
	return a
}
