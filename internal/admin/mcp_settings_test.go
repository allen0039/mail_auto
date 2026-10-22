package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mail-auto/internal/config"
	"mail-auto/internal/mail"
)

func TestVisualMCPSettingsPersistenceHotSwitchAndDomainRevocation(t *testing.T) {
	auth, session := formalAuth(t)
	svc := &mail.Service{Store: auth.Store, Vault: auth.Vault}
	srv := New(auth, svc, config.Config{PublicURL: "http://mail.test"})
	request := func(method, path, body string, login, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://mail.test"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if login {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
		}
		if csrf {
			r.Header.Set("Origin", "http://mail.test")
			r.Header.Set("X-CSRF-Token", session.CSRF)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/api/mcp/settings", "", false, false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request("PUT", "/api/mcp/settings", `{"public_url":"https://mail.test","enabled":true,"version":0}`, true, false); w.Code != 403 {
		t.Fatal("CSRF", w.Code)
	}
	if w := request("PUT", "/api/mcp/settings", `{"public_url":"","enabled":false,"version":0}`, true, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("PUT", "/api/mcp/settings", `{"public_url":"http://mail.test","enabled":true,"version":1}`, true, true); w.Code != 400 {
		t.Fatal("HTTP enabled", w.Code)
	}
	w := request("PUT", "/api/mcp/settings", `{"public_url":"https://mail.test/","enabled":true,"version":1}`, true, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "codex mcp add mail-auto") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request("GET", "/mcp", "", false, false); w.Code != 401 {
		t.Fatal("hot enable", w.Code, w.Body.String())
	}
	if w = request("GET", "/.well-known/oauth-authorization-server", "", false, false); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = request("PUT", "/api/mcp/settings", `{"enabled":false,"version":1}`, true, true); w.Code != 409 {
		t.Fatal("lost update accepted", w.Code)
	}
	// New server instance must prefer the database over stale CLI defaults.
	srv = New(auth, svc, config.Config{PublicURL: "http://mail.test", MCPEnabled: false})
	if !srv.currentMCP().Enabled || srv.currentMCP().PublicURL != "https://mail.test" {
		t.Fatal(srv.currentMCP())
	}
	account, e := svc.Create(mail.AccountInput{Name: "fixture", Provider: "qq", Username: "fake@qq.com", Password: "fake"})
	if e != nil {
		t.Fatal(e)
	}
	_, secret, e := svc.CreateToken(mail.TokenInput{Name: "domain bound", Accounts: []string{account.ID}, Actions: []string{"read"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = svc.AuthenticateToken(secret); e != nil {
		t.Fatal(e)
	}
	w = request("PUT", "/api/mcp/settings", `{"public_url":"https://new-mail.test","enabled":true,"version":2}`, true, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, e = svc.AuthenticateToken(secret); e == nil {
		t.Fatal("old domain token survived")
	}
	if w = request("GET", "/mcp", "", false, false); w.Code != 421 {
		t.Fatal("old Host accepted", w.Code)
	}
	// Admin HTTP origin remains usable after a future HTTPS domain is saved.
	w = request("PUT", "/api/mcp/settings", `{"public_url":"","enabled":false,"version":3}`, true, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request("GET", "/mcp", "", false, false); w.Code != 503 {
		t.Fatal("hot disable", w.Code)
	}
	if w = request("POST", "/api/mcp/settings/check", "{}", true, true); w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, e = auth.Session(session.Token); e != nil {
		t.Fatal("admin session invalidated", e)
	}
}
func TestMCPURLValidation(t *testing.T) {
	for _, value := range []string{"http://mail.test", "https://u:p@mail.test", "https://mail.test/mcp", "https://mail.test?x=1", "https://mail.test#x", "https://mail.test:0", "https://mail.test:65536", "https://bad'host", "https://mail.test/$(id)"} {
		if _, e := normalizeMCPSettings(MCPSettings{Enabled: true, PublicURL: value}); e == nil {
			t.Fatal("invalid URL", value)
		}
	}
	if _, e := normalizeMCPSettings(MCPSettings{Enabled: true}); e == nil {
		t.Fatal("empty enabled")
	}
	for _, value := range []string{"", "https://mail.test", "https://mail.test/", "https://[::1]:443"} {
		if _, e := normalizeMCPSettings(MCPSettings{PublicURL: value}); e != nil {
			t.Fatal(value, e)
		}
	}
}
func TestMCPHTTPSChecksAndSecureProxyCookie(t *testing.T) {
	auth, _ := formalAuth(t)
	svc := &mail.Service{Store: auth.Store, Vault: auth.Vault}
	srv := New(auth, svc, config.Config{})
	tlsServer := httptest.NewTLSServer(srv.Handler())
	defer tlsServer.Close()
	if _, e := srv.saveMCP(MCPSettings{Enabled: true, PublicURL: tlsServer.URL}); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	checks := mcpChecks(ctx, srv.currentMCP(), tlsServer.Client())
	for _, c := range checks {
		if !c.OK {
			t.Fatal(c)
		}
	}
	if len(checks) != 3 {
		t.Fatal(checks)
	}
	r := httptest.NewRequest("GET", strings.Replace(tlsServer.URL, "https:", "http:", 1), nil)
	w := httptest.NewRecorder()
	srv.cookie(w, r, sessionCookie, "fixture", time.Now().Add(time.Hour).Unix())
	if cookies := w.Result().Cookies(); len(cookies) != 1 || !cookies[0].Secure {
		t.Fatal("proxy cookie not secure", cookies)
	}
	// A misrouted reverse proxy must fail every metadata/authentication check.
	wrong := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": "https://wrong.test"})
	}))
	defer wrong.Close()
	state := &mcpRuntime{MCPSettings: MCPSettings{PublicURL: wrong.URL, Enabled: true}}
	for _, c := range mcpChecks(ctx, state, wrong.Client()) {
		if c.OK {
			t.Fatal("wrong proxy passed", c)
		}
	}
}
func TestMCPRuntimeConcurrentReadsAndSaves(t *testing.T) {
	auth, _ := formalAuth(t)
	srv := New(auth, &mail.Service{Store: auth.Store, Vault: auth.Vault}, config.Config{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 30; n++ {
				r := httptest.NewRequest("GET", "https://mail.test/mcp", nil)
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, r)
				if w.Code != 401 && w.Code != 503 {
					t.Errorf("inconsistent runtime %d", w.Code)
				}
			}
		}()
	}
	for n := 0; n < 12; n++ {
		old := srv.currentMCP()
		if _, e := srv.saveMCP(MCPSettings{Enabled: n%2 == 0, PublicURL: "https://mail.test", Version: old.Version}); e != nil {
			t.Fatal(e)
		}
	}
	wg.Wait()
}
