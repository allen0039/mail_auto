package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mail-auto/internal/config"
	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
	"mail-auto/internal/mailoauth"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMailOAuthDiagnosticDistinguishesGmailCapabilities(t *testing.T) {
	for _, test := range []struct {
		provider string
		input    error
		code     string
	}{
		{"google", fmt.Errorf("wrapped: %w", mail.ErrCapability), "oauth_gmail_capability_missing"},
		{"google", mailoauth.Failure("oauth_imap_denied"), "oauth_imap_denied"},
		{"google", errors.New("temporary network failure"), "oauth_network_failed"},
	} {
		if actual := mailoauth.Code(mailOAuthDiagnostic(test.provider, test.input)); actual != test.code {
			t.Fatalf("diagnostic code = %q, want %q", actual, test.code)
		}
	}
	if mailOAuthDiagnostic("google", nil) != nil {
		t.Fatal("successful Gmail diagnostic became a failure")
	}
}

func TestMailOAuthAPIBoundariesAndMCPIndependence(t *testing.T) {
	auth, _, _ := newAuth(t)
	service := &mail.Service{Store: auth.Store, Vault: auth.Vault}
	server := New(auth, service, config.Config{})
	initial, _ := auth.Show()
	restricted, err := auth.Login(initial.Username, initial.Password)
	if err != nil {
		t.Fatal(err)
	}
	client := &browser{t: t, h: server.Handler(), cookies: map[string]*http.Cookie{sessionCookie: {Name: sessionCookie, Value: restricted.Token}}, csrf: restricted.CSRF}
	for _, path := range []string{"/api/settings/mail-oauth", "/api/accounts/not-found/oauth-start"} {
		if result := client.request("POST", path, "{}", true); result.Code != 403 {
			t.Fatal("restricted OAuth API accepted", path, result.Code)
		}
	}
	if err = auth.Setup(restricted, "owner", "fixture-password-long"); err != nil {
		t.Fatal(err)
	}
	session, err := auth.Login("owner", "fixture-password-long")
	if err != nil {
		t.Fatal(err)
	}
	client.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: session.Token}
	client.csrf = session.CSRF
	input := `{"client_id":"fixture-client","client_secret":"fixture-secret","origin":"https://mail.example.test"}`
	if result := client.request("PUT", "/api/settings/mail-oauth/google", input, false); result.Code != 403 {
		t.Fatal("app CSRF bypass")
	}
	result := client.request("PUT", "/api/settings/mail-oauth/google", input, true)
	if result.Code != 200 || bytes.Contains(result.Body.Bytes(), []byte("fixture-secret")) {
		t.Fatal("app configuration failed or secret echoed", result.Code)
	}
	var app mailoauth.App
	if err = json.Unmarshal(result.Body.Bytes(), &app); err != nil || app.Callback != "https://mail.example.test/api/mail-oauth/google/callback" {
		t.Fatal("callback used request Host", err)
	}
	account, err := service.Create(mail.AccountInput{Name: "fixture", Provider: "google", Username: "fixture@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if result = client.request("POST", "/api/accounts/"+account.ID+"/oauth-start", "{}", false); result.Code != 403 {
		t.Fatal("start CSRF bypass")
	}
	result = client.request("POST", "/api/accounts/"+account.ID+"/oauth-start", "{}", true)
	if result.Code != 200 {
		t.Fatal("MCP off blocked mail OAuth", result.Code)
	}
	var started map[string]string
	json.Unmarshal(result.Body.Bytes(), &started)
	location, _ := url.Parse(started["url"])
	if location.Host != "accounts.google.com" {
		t.Fatal("nonofficial authorization host")
	}
	cookie := client.cookies[mailOAuthCookie]
	if cookie == nil || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/api/mail-oauth/" {
		t.Fatal("cross-site callback cookie not properly constrained")
	}
	result = client.request("GET", "/api/mail-oauth/google/callback?state="+url.QueryEscape(location.Query().Get("state"))+"&error=access_denied&error_description=private-provider-error", "", false)
	if result.Code != 303 || strings.Contains(result.Header().Get("Location"), "private-provider-error") || strings.Contains(result.Header().Get("Location"), "state=") || !strings.Contains(result.Header().Get("Location"), "oauth_denied") {
		t.Fatal("callback was blocked or leaked provider response")
	}
	if _, err = service.OAuthManager().Consume(t.Context(), "google", location.Query().Get("state"), credentials.Digest(session.Token)); err == nil {
		t.Fatal("denied callback state replayed")
	}
	result = client.request("GET", "/api/settings/mail-oauth", "", false)
	if result.Code != 200 || bytes.Contains(result.Body.Bytes(), []byte("fixture-secret")) {
		t.Fatal("settings leaked secret")
	}
	for _, header := range []string{"Cache-Control", "Referrer-Policy"} {
		if result.Header().Get(header) == "" {
			t.Fatal("missing response hygiene", header)
		}
	}
}

func TestMailOAuthCallbackWithoutSessionAlwaysSanitizes(t *testing.T) {
	auth, _, _ := newAuth(t)
	server := New(auth, &mail.Service{Store: auth.Store, Vault: auth.Vault}, config.Config{})
	for _, provider := range []string{"google", "microsoft", "evil"} {
		request := httptest.NewRequest("GET", "https://mail.test/api/mail-oauth/"+provider+"/callback?code=private-code&state=private-state", nil)
		writer := httptest.NewRecorder()
		server.Handler().ServeHTTP(writer, request)
		if writer.Code != 303 || strings.Contains(writer.Header().Get("Location"), "private-") {
			t.Fatal("anonymous callback leaked code/state", writer.Code)
		}
	}
}
