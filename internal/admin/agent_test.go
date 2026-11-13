package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mail-auto/internal/agent"
	"mail-auto/internal/config"
	"mail-auto/internal/mail"
	"mail-auto/internal/telegram"
)

// loggedIn returns a browser with a completed setup and normal session.
func loggedIn(t *testing.T) (*browser, *Server) {
	t.Helper()
	auth, _, _ := newAuth(t)
	svc := &mail.Service{Store: auth.Store, Vault: auth.Vault}
	server := New(auth, svc, config.Config{SessionHours: 12})
	server.Agent = agent.New(auth.Store, auth.Vault, svc)
	server.Telegram = telegram.New(auth.Store, auth.Vault, svc, server.Agent)
	b := &browser{t: t, h: server.Handler(), cookies: map[string]*http.Cookie{}}
	b.request("GET", "/api/session", "", false)
	b.csrf = b.cookies[csrfCookie].Value
	initial, _ := auth.Show()
	body, _ := json.Marshal(map[string]string{"username": initial.Username, "password": initial.Password})
	w := b.request("POST", "/api/login", string(body), true)
	var s Session
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	b.csrf = s.CSRF
	if w = b.request("POST", "/api/setup", `{"username":"owner","password":"Abcdef123456"}`, true); w.Code != 200 { // gitleaks:allow -- synthetic test password
		t.Fatal(w.Code, w.Body)
	}
	b.request("GET", "/api/session", "", false)
	b.csrf = b.cookies[csrfCookie].Value
	w = b.request("POST", "/api/login", `{"username":"owner","password":"Abcdef123456"}`, true) // gitleaks:allow -- synthetic test password
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	b.csrf = s.CSRF
	return b, server
}

func TestAgentAPIBoundaryAndRedaction(t *testing.T) {
	anon := &browser{t: t, cookies: map[string]*http.Cookie{}}
	b, server := loggedIn(t)
	anon.h = server.Handler()
	for _, path := range []string{"/api/agent/provider", "/api/agent/grant", "/api/telegram/interaction", "/api/agent/conversations"} {
		if w := anon.request("GET", path, "", false); w.Code != 401 {
			t.Fatal("unauthenticated access", path, w.Code)
		}
		if w := b.request("GET", path, "", false); w.Code != 200 {
			t.Fatal(path, w.Code, w.Body)
		}
	}
	// Writes need CSRF and same origin.
	if w := b.request("PUT", "/api/agent/provider", `{"version":0,"base_url":"https://api.example.com/v1","model":"m","api_key":"sk-secret-value"}`, false); w.Code != 403 {
		t.Fatal("CSRF bypass", w.Code)
	}
	// Unknown fields and trailing JSON are rejected.
	if w := b.request("PUT", "/api/agent/provider", `{"version":0,"owner":"admin"}`, true); w.Code != 400 {
		t.Fatal("unknown field accepted", w.Code)
	}
	if w := b.request("PUT", "/api/agent/provider", `{"version":0} {}`, true); w.Code != 400 {
		t.Fatal("trailing JSON accepted", w.Code)
	}
	for _, bad := range []string{"http://api.example.com/v1", "https://127.0.0.1/v1", "https://169.254.169.254/latest"} {
		body, _ := json.Marshal(map[string]any{"version": 0, "base_url": bad, "model": "m", "api_key": "sk-x"})
		if w := b.request("PUT", "/api/agent/provider", string(body), true); w.Code != 400 {
			t.Fatal("unsafe base URL accepted", bad, w.Code)
		}
	}
	w := b.request("PUT", "/api/agent/provider", `{"version":0,"base_url":"https://api.example.com/v1/","model":"m","api_key":"sk-secret-value"}`, true)
	if w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte("sk-secret")) || !bytes.Contains(w.Body.Bytes(), []byte(`"endpoint":"https://api.example.com/v1/chat/completions"`)) {
		t.Fatal("provider save", w.Code, w.Body)
	}
	// Enabling without a verified tool loop is refused with a fixed code.
	if w = b.request("PUT", "/api/agent/provider", `{"version":1,"enabled":true,"base_url":"https://api.example.com/v1","model":"m"}`, true); w.Code != 422 || !bytes.Contains(w.Body.Bytes(), []byte("tool_call_unsupported")) {
		t.Fatal("enable without tool loop", w.Code, w.Body)
	}
	// Stale version conflicts instead of overwriting.
	if w = b.request("PUT", "/api/agent/provider", `{"version":0,"base_url":"https://api.example.com/v1","model":"x"}`, true); w.Code != 409 {
		t.Fatal("stale version", w.Code)
	}
	// A bind code needs interaction and a grant; GET never creates one.
	if w = b.request("GET", "/api/telegram/bind-code", "", false); w.Code != 405 {
		t.Fatal("GET bind code", w.Code)
	}
	if w = b.request("POST", "/api/telegram/bind-code", `{}`, true); w.Code == 200 {
		t.Fatal("bind code without interaction")
	}
	// Agent state never affects readiness.
	ready := httptest.NewRecorder()
	server.Handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "http://mail.test/readyz", nil))
	if ready.Code != 200 {
		t.Fatal("readyz", ready.Code)
	}
	w = b.request("GET", "/api/status", "", false)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"agent":{`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"continuous_ai_classification":false`)) {
		t.Fatal("status lacks agent state", w.Body)
	}
	if w = b.request("GET", "/agent.js", "", false); w.Code != 200 {
		t.Fatal("agent.js not served", w.Code)
	}
}

// AT-AUD-08: folder scope must be explicit; an empty selection is refused
// and never stored as "all folders".
func TestAgentGrantFolderScopeIsExplicit(t *testing.T) {
	b, server := loggedIn(t)
	db := server.Auth.Store.DB
	if _, err := db.Exec("INSERT INTO accounts VALUES('acc','fixture','qq','f@qq.com','imap.qq.com',993,'[]',X'01','',1); INSERT INTO mailboxes VALUES('acc','INBOX',7,1),('acc','Archive',7,1)"); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"legacy body without all_folders": `{"version":0,"enabled":true,"accounts":["acc"],"folders":{"acc":[]},"actions":["read"]}`,
		"empty selection":                 `{"version":0,"enabled":true,"accounts":["acc"],"all_folders":[],"folders":{"acc":[]},"actions":["read"]}`,
		"all plus folders":                `{"version":0,"enabled":true,"accounts":["acc"],"all_folders":["acc"],"folders":{"acc":["INBOX"]},"actions":["read"]}`,
	} {
		if w := b.request("PUT", "/api/agent/grant", body, true); w.Code != 400 {
			t.Fatal(name, "accepted", w.Code, w.Body)
		}
	}
	var grants int
	_ = db.QueryRow("SELECT count(*) FROM agent_grants").Scan(&grants)
	if grants != 0 {
		t.Fatal("refused request stored a grant")
	}
	w := b.request("PUT", "/api/agent/grant", `{"version":0,"enabled":true,"accounts":["acc"],"all_folders":[],"folders":{"acc":["Archive"]},"actions":["read"]}`, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = b.request("GET", "/api/agent/grant", "", false)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"folders":{"acc":["Archive"]}`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"all_folders":[]`)) {
		t.Fatal("partial scope echo", w.Body)
	}
	w = b.request("PUT", "/api/agent/grant", `{"version":1,"enabled":true,"accounts":["acc"],"all_folders":["acc"],"folders":{"acc":[]},"actions":["read"]}`, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = b.request("GET", "/api/agent/grant", "", false)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"all_folders":["acc"]`)) {
		t.Fatal("all-folder scope echo", w.Body)
	}
}
