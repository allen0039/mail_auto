package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mail-auto/internal/config"
	"mail-auto/internal/mail"
	"net/http"
	"testing"
)

type adminGmailRemoteStub struct {
	err error
}

func (r *adminGmailRemoteStub) Folders(_ context.Context, a mail.Account, pw string) ([]string, error) {
	if r.err != nil {
		return nil, r.err
	}
	if pw != "validapppassword" {
		return nil, errors.New("rejected")
	}
	return []string{"INBOX"}, nil
}
func (r *adminGmailRemoteStub) Scan(context.Context, mail.Account, string, string, func(mail.Snapshot) error) error {
	return nil
}
func (r *adminGmailRemoteStub) Read(context.Context, mail.Account, string, mail.Message) (mail.Content, error) {
	return mail.Content{}, nil
}

type codedErrorStub struct {
	code string
	msg  string
}

func (e codedErrorStub) Error() string              { return e.msg }
func (e codedErrorStub) PublicMessage() string      { return e.msg }
func (e codedErrorStub) OperationErrorCode() string { return e.code }

func TestRetiredMailboxOAuthRoutesAndOutlookRejected(t *testing.T) {
	auth, session := formalAuth(t)
	svc := &mail.Service{Store: auth.Store, Vault: auth.Vault}
	client := &browser{t: t, h: New(auth, svc, config.Config{}).Handler(), csrf: session.CSRF, cookies: map[string]*http.Cookie{sessionCookie: {Name: sessionCookie, Value: session.Token}}}
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/settings/mail-oauth"},
		{"PUT", "/api/settings/mail-oauth/google"},
		{"GET", "/api/mail-oauth/microsoft/callback"},
		{"POST", "/api/accounts/mailbox/oauth-start"},
		{"POST", "/api/accounts/mailbox/oauth-device/start"},
		{"POST", "/api/accounts/mailbox/migrate-password"},
	} {
		if response := client.request(route.method, route.path, "{}", true); response.Code != http.StatusNotFound {
			t.Fatal("retired route remains reachable", route, response.Code)
		}
	}
	response := client.request("POST", "/api/accounts", `{"name":"Retired","provider":"microsoft","username":"fixture@outlook.test","password":"fixture"}`, true)
	if response.Code != http.StatusBadRequest {
		t.Fatal("unsupported mailbox provider accepted", response.Code)
	}
}

func TestGmailHTTPVerificationAndSecretHygiene(t *testing.T) {
	auth, session := formalAuth(t)
	remote := &adminGmailRemoteStub{}
	svc := &mail.Service{Store: auth.Store, Vault: auth.Vault, Remote: remote}
	b := &browser{t: t, h: New(auth, svc, config.Config{}).Handler(), csrf: session.CSRF, cookies: map[string]*http.Cookie{sessionCookie: {Name: sessionCookie, Value: session.Token}}}

	// Wrong credentials must be rejected with structured error_code and must NOT create account or echo password.
	remote.err = codedErrorStub{code: "credentials_rejected", msg: "应用专用密码被拒绝"}
	w := b.request("POST", "/api/accounts", `{"name":"gm","provider":"google","username":"u@gmail.com","password":"secret-wrong-pass","port":993}`, true)
	if w.Code != 422 || !bytes.Contains(w.Body.Bytes(), []byte("credentials_rejected")) || bytes.Contains(w.Body.Bytes(), []byte("secret-wrong-pass")) {
		t.Fatalf("bad Gmail create response: %d %s", w.Code, w.Body.String())
	}
	if accounts, _ := svc.Accounts(); len(accounts) != 0 {
		t.Fatal("failed Gmail verification created account")
	}

	// TLS failure is distinguishable from credential rejection.
	remote.err = codedErrorStub{code: "target_tls_failed", msg: "TLS 证书校验失败"}
	w = b.request("POST", "/api/accounts", `{"name":"gm","provider":"google","username":"u@gmail.com","password":"valid app password","port":993}`, true)
	if w.Code != 422 || !bytes.Contains(w.Body.Bytes(), []byte("target_tls_failed")) {
		t.Fatalf("expected target_tls_failed: %d %s", w.Code, w.Body.String())
	}

	// Valid grouped password succeeds and never echoes password in API or audits.
	remote.err = nil
	w = b.request("POST", "/api/accounts", `{"name":"gm","provider":"google","username":"u@gmail.com","password":"vali dapp pass word","port":993}`, true)
	if w.Code != 201 || bytes.Contains(w.Body.Bytes(), []byte("validapppassword")) {
		t.Fatalf("valid Gmail create failed or leaked password: %d %s", w.Code, w.Body.String())
	}
	var created mail.Account
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	// Failed rotation via PUT preserves working credential.
	remote.err = codedErrorStub{code: "credentials_rejected", msg: "新密码被拒绝"}
	w = b.request("PUT", "/api/accounts/"+created.ID, `{"name":"gm","provider":"google","username":"u@gmail.com","password":"bad-new-password","port":993}`, true)
	if w.Code != 422 || !bytes.Contains(w.Body.Bytes(), []byte("credentials_rejected")) {
		t.Fatalf("failed rotation response wrong: %d %s", w.Code, w.Body.String())
	}
	remote.err = nil
	if _, err := svc.Diagnose(context.Background(), created.ID); err != nil {
		t.Fatalf("failed rotation broke existing working Gmail credential: %v", err)
	}
	// Check audits and /api/accounts never leak passwords.
	w = b.request("GET", "/api/accounts", "", false)
	if bytes.Contains(w.Body.Bytes(), []byte("validapppassword")) || bytes.Contains(w.Body.Bytes(), []byte("bad-new-password")) {
		t.Fatal("accounts API leaked password")
	}
	w = b.request("GET", "/api/audits", "", false)
	if bytes.Contains(w.Body.Bytes(), []byte("validapppassword")) || bytes.Contains(w.Body.Bytes(), []byte("bad-new-password")) {
		t.Fatal("audits API leaked password")
	}
}
