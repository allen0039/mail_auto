package mailoauth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"mail-auto/internal/network"
)

func TestGoogleIdentityOnlyConsentDoesNotBindMailbox(t *testing.T) {
	for _, granted := range []string{
		"openid email",
		"openid https://www.googleapis.com/auth/userinfo.email",
		"openid email https://www.googleapis.com/auth/gmail.readonly",
		"",
	} {
		t.Run(granted, func(t *testing.T) {
			manager, session := fixture(t, "google")
			request, _ := authorize(t, manager, session, "google")
			calls := 0
			manager.transport = func(network.Route) http.RoundTripper {
				return roundTrip(func(incoming *http.Request) (*http.Response, error) {
					calls++
					if incoming.URL.Host != "oauth2.googleapis.com" || incoming.URL.Path != "/token" {
						t.Fatal("identity-only consent continued to identity or IMAP validation")
					}
					return response(200, map[string]any{
						"access_token": "scope-test-access", "refresh_token": "scope-test-refresh",
						"token_type": "Bearer", "expires_in": 3600, "scope": granted,
						"id_token": "scope-test-identity",
					}), nil
				})
			}
			token, identity, err := manager.Exchange(context.Background(), request, "scope-test-code")
			if Code(err) != "oauth_scope_missing" || token.Access != "" || token.Refresh != "" || identity.Subject != "" {
				t.Fatal("partial consent accepted or credentials escaped failed exchange")
			}
			if calls != 1 || strings.Contains(err.Error(), "scope-test") {
				t.Fatal("partial consent repeated requests or exposed sensitive response")
			}
			account, err := manager.Account("account")
			if err != nil {
				t.Fatal(err)
			}
			if account.Status != "pending" || account.Subject != "" || len(account.Refresh) != 0 || account.Generation != 1 {
				t.Fatal("partial consent changed mailbox binding")
			}
		})
	}
}

func TestGoogleGrantedIMAPScopeIsIndependentOfIdentityScopeAliases(t *testing.T) {
	manager, _ := fixture(t, "google")
	app, err := manager.App("google")
	if err != nil {
		t.Fatal(err)
	}
	for _, granted := range []string{
		GoogleScope,
		"openid email " + GoogleScope,
		"openid https://www.googleapis.com/auth/userinfo.email " + GoogleScope,
		" \t" + GoogleScope + "\n openid  https://www.googleapis.com/auth/userinfo.email ",
	} {
		t.Run(granted, func(t *testing.T) {
			client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
				return response(200, map[string]any{
					"access_token": "scope-test-access", "token_type": "Bearer",
					"expires_in": 3600, "scope": granted,
				}), nil
			})}
			token, err := manager.tokenRequest(context.Background(), client, app, url.Values{"grant_type": {"authorization_code"}})
			if err != nil || token.Access == "" || token.Scope != granted {
				t.Fatal("valid IMAP grant rejected because identity scopes were normalized")
			}
		})
	}
}
