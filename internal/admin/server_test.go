package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mail-auto/internal/config"
	"mail-auto/internal/mail"
)

type browser struct {
	t       *testing.T
	h       http.Handler
	cookies map[string]*http.Cookie
	csrf    string
}

func (b *browser) request(method, path, body string, validCSRF bool) *httptest.ResponseRecorder {
	b.t.Helper()
	r := httptest.NewRequest(method, "http://mail.test"+path, bytes.NewBufferString(body))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "127.0.0.1:1234"
	if validCSRF {
		r.Header.Set("Origin", "http://mail.test")
		r.Header.Set("X-CSRF-Token", b.csrf)
	}
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return w
}
func TestHTTPAccessBoundary(t *testing.T) {
	auth, _, _ := newAuth(t)
	cfg := config.Config{SessionHours: 12}
	svc := &mail.Service{Store: auth.Store, Vault: auth.Vault}
	server := New(auth, svc, cfg)
	b := &browser{t: t, h: server.Handler(), cookies: map[string]*http.Cookie{}}
	w := b.request("GET", "/api/accounts", "", false)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	w = b.request("GET", "/api/session", "", false)
	var anon struct {
		CSRF string `json:"csrf"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &anon); e != nil {
		t.Fatal(e)
	}
	b.csrf = anon.CSRF
	initial, _ := auth.Show()
	body, _ := json.Marshal(map[string]string{"username": initial.Username, "password": initial.Password})
	if w = b.request("POST", "/api/login", string(body), false); w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if w = b.request("POST", "/api/login", string(body), true); w.Code != 200 {
		t.Fatalf("login %d %s", w.Code, w.Body)
	}
	var setup Session
	json.Unmarshal(w.Body.Bytes(), &setup)
	b.csrf = setup.CSRF
	for _, route := range []struct{ method, path string }{{"GET", "/api/status"}, {"GET", "/api/accounts"}, {"POST", "/api/accounts"}, {"GET", "/api/messages"}, {"GET", "/api/audits"}, {"POST", "/api/settings/credentials"}, {"POST", "/api/future-task"}, {"GET", "/api/mcp/settings"}, {"PUT", "/api/mcp/settings"}, {"POST", "/api/remote-folders"}} {
		if w = b.request(route.method, route.path, "{}", true); w.Code != 403 {
			t.Fatalf("restricted session bypassed %s: %d", route.path, w.Code)
		}
	}
	if w = b.request("POST", "/api/setup", `{"username":"owner","password":"Abcdef123456"}`, false); w.Code != 403 { // gitleaks:allow -- synthetic test password
		t.Fatal("setup CSRF bypass")
	}
	if w = b.request("POST", "/api/setup", `{"username":"owner","password":"Abcdef12345"}`, true); w.Code != 400 {
		t.Fatalf("11-character password accepted: %d", w.Code)
	}
	if w = b.request("POST", "/api/setup", `{"username":"owner","password":"Abcdef123456"}`, true); w.Code != 200 { // gitleaks:allow -- synthetic test password
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	if w = b.request("GET", "/api/status", "", false); w.Code != 401 {
		t.Fatal("old session retained")
	}
	b.request("GET", "/api/session", "", false)
	b.csrf = b.cookies[csrfCookie].Value
	if w = b.request("POST", "/api/login", `{"username":"owner","password":"Abcdef123456"}`, true); w.Code != 200 { // gitleaks:allow -- synthetic test password
		t.Fatal("formal login", w.Code, w.Body)
	}
	var normal Session
	json.Unmarshal(w.Body.Bytes(), &normal)
	b.csrf = normal.CSRF
	if c := b.cookies[sessionCookie]; c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatal("cookie flags")
	}
	for _, path := range []string{"/api/status", "/api/accounts", "/api/audits", "/api/messages"} {
		if w = b.request("GET", path, "", false); w.Code != 200 {
			t.Fatalf("%s %d %s", path, w.Code, w.Body)
		}
	}
	if w = b.request("POST", "/api/accounts", `{"name":"test","provider":"qq","username":"a@qq.com","password":"secret-test"}`, true); w.Code != 201 {
		t.Fatal("account create", w.Code, w.Body)
	}
	w = b.request("GET", "/api/accounts", "", false)
	if bytes.Contains(w.Body.Bytes(), []byte("secret-test")) || bytes.Contains(w.Body.Bytes(), []byte(`"secret"`)) {
		t.Fatal("credential exposed")
	}
	if w = b.request("GET", "/mcp", "", false); w.Code != 503 {
		t.Fatal("MCP must be closed")
	}
	if w = b.request("POST", "/api/logout", "{}", true); w.Code != 200 {
		t.Fatal("logout")
	}
	if _, e := auth.Session(normal.Token); e == nil {
		t.Fatal("logout session still valid")
	}
}
func TestLoginLimitAndSessionExpiry(t *testing.T) {
	auth, _, _ := newAuth(t)
	server := New(auth, &mail.Service{}, config.Config{})
	b := &browser{t: t, h: server.Handler(), cookies: map[string]*http.Cookie{}}
	b.request("GET", "/api/session", "", false)
	b.csrf = b.cookies[csrfCookie].Value
	for i := 0; i < 5; i++ {
		if w := b.request("POST", "/api/login", `{"username":"bad","password":"bad"}`, true); w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	if w := b.request("POST", "/api/login", `{"username":"bad","password":"bad"}`, true); w.Code != 429 {
		t.Fatal("rate limit missing")
	}
	initial, _ := auth.Show()
	session, e := auth.Login(initial.Username, initial.Password)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = auth.Store.DB.Exec("UPDATE sessions SET expires=?", time.Now().Add(-time.Minute).Unix()); e != nil {
		t.Fatal(e)
	}
	if _, e = auth.Session(session.Token); e == nil {
		t.Fatal("expired session accepted")
	}
}

func TestHTTPCredentialSettings(t *testing.T) {
	a, session := formalAuth(t)
	server := New(a, &mail.Service{Store: a.Store, Vault: a.Vault}, config.Config{})
	b := &browser{t: t, h: server.Handler(), csrf: session.CSRF, cookies: map[string]*http.Cookie{
		sessionCookie: {Name: sessionCookie, Value: session.Token},
	}}
	const path = "/api/settings/credentials"
	const body = `{"username":"new_owner","current_password":"OriginalPass123","password":"UpdatedPass123"}`
	anonymous := &browser{t: t, h: server.Handler(), cookies: map[string]*http.Cookie{}}
	if w := anonymous.request("POST", path, body, true); w.Code != 401 {
		t.Fatal("anonymous change accepted", w.Code)
	}
	if w := b.request("POST", path, body, false); w.Code != 403 {
		t.Fatal("CSRF bypass", w.Code)
	}
	if w := b.request("POST", path, `{"username":"new_owner","current_password":"wrong"}`, true); w.Code != 403 {
		t.Fatal("wrong password accepted", w.Code)
	}
	if w := b.request("POST", path, body, true); w.Code != 200 {
		t.Fatal("credentials update failed", w.Code, w.Body)
	}
	if len(b.cookies) != 0 {
		t.Fatal("cookies not cleared after credentials update")
	}
	if w := b.request("GET", "/api/accounts", "", false); w.Code != 401 {
		t.Fatal("old browser still authenticated", w.Code)
	}
	b.request("GET", "/api/session", "", false)
	b.csrf = b.cookies[csrfCookie].Value
	if w := b.request("POST", "/api/login", `{"username":"new_owner","password":"UpdatedPass123"}`, true); w.Code != 200 {
		t.Fatal("updated login failed", w.Code, w.Body)
	}
	var secretCount int
	if e := a.Store.DB.QueryRow("SELECT count(*) FROM audits WHERE target LIKE '%Pass123%' OR action LIKE '%Pass123%' OR outcome LIKE '%Pass123%'").Scan(&secretCount); e != nil || secretCount != 0 {
		t.Fatal("password in audit", e)
	}
}

func TestCredentialSettingsRateLimit(t *testing.T) {
	a, session := formalAuth(t)
	server := New(a, &mail.Service{}, config.Config{})
	b := &browser{t: t, h: server.Handler(), csrf: session.CSRF, cookies: map[string]*http.Cookie{
		sessionCookie: {Name: sessionCookie, Value: session.Token},
	}}
	for range 5 {
		if w := b.request("POST", "/api/settings/credentials", `{"username":"new_owner","current_password":"wrong"}`, true); w.Code != 403 {
			t.Fatal("unexpected password rejection", w.Code)
		}
	}
	if w := b.request("POST", "/api/settings/credentials", `{"username":"new_owner","current_password":"OriginalPass123"}`, true); w.Code != 429 || w.Header().Get("Retry-After") != "300" {
		t.Fatal("credential changes not rate limited", w.Code)
	}
}
