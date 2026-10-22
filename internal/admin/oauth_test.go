package admin

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"mail-auto/internal/config"
	"mail-auto/internal/mail"
)

func TestOAuthPKCEConsentReplayRefreshAndRevocation(t *testing.T) {
	auth, session := formalAuth(t)
	svc := &mail.Service{Store: auth.Store, Vault: auth.Vault}
	account, e := svc.Create(mail.AccountInput{Name: "fixture", Provider: "qq", Username: "fake@qq.com", Password: "fake"})
	if e != nil {
		t.Fatal(e)
	}
	srv := New(auth, svc, config.Config{SecureCookie: true})
	if _, e = srv.saveMCP(MCPSettings{Enabled: true, PublicURL: "https://mail.test"}); e != nil {
		t.Fatal(e)
	}
	request := func(method, path, body, content string, login, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://mail.test"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", content)
		if login {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
		}
		if csrf {
			r.Header.Set("Origin", "https://mail.test")
			r.Header.Set("X-CSRF-Token", session.CSRF)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}
	w := request("GET", "/.well-known/oauth-authorization-server", "", "", false, false)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request("POST", "/oauth/register", `{"client_name":"fixture","redirect_uris":["https://client.test/callback"],"token_endpoint_auth_method":"none"}`, "application/json", false, false)
	var registered struct {
		ID string `json:"client_id"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &registered) != nil || registered.ID == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	verifier := strings.Repeat("x", 43)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	q := url.Values{"client_id": {registered.ID}, "response_type": {"code"}, "redirect_uri": {"https://client.test/callback"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "scope": {"mail.read mail.write offline_access"}, "resource": {"https://mail.test/mcp"}, "state": {"original"}}
	w = request("GET", "/oauth/authorize?"+q.Encode(), "", "", false, false)
	if w.Code != 302 {
		t.Fatal(w.Code, w.Body.String())
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	id := loc.Query().Get("oauth_request")
	consent := `{"accounts":["` + account.ID + `"],"actions":["read","folder_manage"]}`
	path := "/api/oauth/requests/" + id
	if w = request("POST", path, consent, "application/json", false, false); w.Code != 401 {
		t.Fatal("anonymous consent", w.Code)
	}
	if w = request("POST", path, consent, "application/json", true, false); w.Code != 403 {
		t.Fatal("CSRF", w.Code)
	}
	w = request("POST", path, consent, "application/json", true, true)
	var approved struct {
		Redirect string `json:"redirect"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &approved) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	callback, _ := url.Parse(approved.Redirect)
	code := callback.Query().Get("code")
	if code == "" || callback.Query().Get("state") != "original" {
		t.Fatal("callback binding")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {registered.ID}, "redirect_uri": {"https://client.test/callback"}, "code_verifier": {strings.Repeat("y", 43)}, "resource": {"https://mail.test/mcp"}}
	if w = request("POST", "/oauth/token", form.Encode(), "application/x-www-form-urlencoded", false, false); w.Code != 400 {
		t.Fatal("wrong PKCE", w.Code)
	}
	form.Set("code_verifier", verifier)
	form.Set("resource", "https://other.test/mcp")
	if w = request("POST", "/oauth/token", form.Encode(), "application/x-www-form-urlencoded", false, false); w.Code != 400 {
		t.Fatal("wrong audience", w.Code)
	}
	form.Set("resource", "https://mail.test/mcp")
	w = request("POST", "/oauth/token", form.Encode(), "application/x-www-form-urlencoded", false, false)
	var tokens struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &tokens) != nil || tokens.Refresh == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	access, e := svc.AuthenticateToken(tokens.Access)
	if e != nil || !access.Allows(account.ID, "INBOX", "folder_manage") || access.Allows(account.ID, "INBOX", "trash") {
		t.Fatal(access, e)
	}
	if w = request("POST", "/oauth/token", form.Encode(), "application/x-www-form-urlencoded", false, false); w.Code != 400 {
		t.Fatal("code replay", w.Code)
	}
	refresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {registered.ID}, "refresh_token": {tokens.Refresh}}
	w = request("POST", "/oauth/token", refresh.Encode(), "application/x-www-form-urlencoded", false, false)
	var rotated struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rotated) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, e = svc.AuthenticateToken(tokens.Access); e == nil {
		t.Fatal("old token survived rotation")
	}
	if w = request("POST", "/oauth/token", refresh.Encode(), "application/x-www-form-urlencoded", false, false); w.Code != 400 {
		t.Fatal("refresh replay", w.Code)
	}
	current, e := svc.AuthenticateToken(rotated.Access)
	if e != nil {
		t.Fatal(e)
	}
	svc.RevokeToken(current.Owner)
	refresh.Set("refresh_token", rotated.Refresh)
	if w = request("POST", "/oauth/token", refresh.Encode(), "application/x-www-form-urlencoded", false, false); w.Code != 400 {
		t.Fatal("revoked grant refreshed", w.Code)
	}
}

func TestMCPDisabledWithoutDomain(t *testing.T) {
	auth, _ := formalAuth(t)
	srv := New(auth, &mail.Service{Store: auth.Store, Vault: auth.Vault}, config.Config{})
	for _, path := range []string{"/mcp", "/.well-known/oauth-authorization-server"} {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://mail.test"+path, nil))
		if w.Code != 503 && w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
}
