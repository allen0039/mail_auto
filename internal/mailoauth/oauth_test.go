package mailoauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/network"
	"mail-auto/internal/store"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (transport roundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
func response(status int, payload any) *http.Response {
	encoded, _ := json.Marshal(payload)
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(encoded)))}
}
func fixture(t *testing.T, provider string) (*Manager, string) {
	t.Helper()
	database, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	vault, err := credentials.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Store: database, Vault: vault}
	_, err = manager.SaveApp(provider, AppInput{ClientID: "fixture-client", ClientSecret: "fixture-client-secret", Origin: "https://mail.example.test", Authority: "consumers"})
	if err != nil {
		t.Fatal(err)
	}
	session := credentials.Digest("fixture-admin-session")
	_, err = database.DB.Exec(`INSERT INTO admin(id,username,password_hash,temp_expires,must_change) VALUES(1,'fixture','fixture',0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.DB.Exec(`INSERT INTO sessions(token_hash,csrf,version,restricted,expires) VALUES(?,'fixture',1,0,?)`, session, time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.DB.Exec(`INSERT INTO accounts(id,name,provider,username,host,port,folders,secret_file,created_at) VALUES('account','fixture',?,'fixture@example.test','imap.fixture',993,'[]','',1)`, provider)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.DB.Exec(`INSERT INTO mail_oauth_accounts(account_id,provider,login,config_version) VALUES('account',?,'fixture@example.test',1)`, provider)
	if err != nil {
		t.Fatal(err)
	}
	return manager, session
}
func authorize(t *testing.T, manager *Manager, session, provider string) (Request, url.Values) {
	t.Helper()
	location, err := manager.Start(context.Background(), "account", provider, session)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(location)
	request, err := manager.Consume(context.Background(), provider, parsed.Query().Get("state"), session)
	if err != nil {
		t.Fatal(err)
	}
	return request, parsed.Query()
}
func bindFixture(t *testing.T, manager *Manager, session, provider string) {
	t.Helper()
	request, _ := authorize(t, manager, session, provider)
	err := manager.Bind(context.Background(), request, Token{Refresh: "fixture-refresh", Scope: Scope(provider)}, Identity{Subject: "fixture-subject", Email: "fixture@example.test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestStatePKCEAndBinding(t *testing.T) {
	for _, provider := range []string{"google", "microsoft"} {
		t.Run(provider, func(t *testing.T) {
			manager, session := fixture(t, provider)
			request, query := authorize(t, manager, session, provider)
			challenge := sha256.Sum256([]byte(request.Verifier))
			if query.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) || query.Get("code_challenge_method") != "S256" || query.Get("nonce") != request.Nonce || query.Get("redirect_uri") != request.App.Callback || !strings.Contains(query.Get("scope"), Scope(provider)) {
				t.Fatal("PKCE or authorization binding missing")
			}
			for _, forbidden := range []string{"fixture-client-secret", "SMTP", "Mail.Read", "calendar", "contacts"} {
				if strings.Contains(query.Encode(), forbidden) {
					t.Fatal("authorization URL leaked secret or requested unrelated permission")
				}
			}
			if _, err := manager.Consume(context.Background(), provider, query.Get("state"), session); Code(err) != "oauth_state_invalid" {
				t.Fatal("state replay accepted")
			}
			if len(request.Verifier) != 43 || len(query.Get("state")) != 43 {
				t.Fatal("insufficient entropy")
			}
		})
	}
}

func TestStateRejectsInvalidContext(t *testing.T) {
	for _, kind := range []string{"missing", "forged", "expired", "wrong_provider", "wrong_session", "restricted", "session_deleted", "admin_rotated", "config_changed", "account_deleted", "newer_request"} {
		t.Run(kind, func(t *testing.T) {
			manager, session := fixture(t, "google")
			location, err := manager.Start(context.Background(), "account", "google", session)
			if err != nil {
				t.Fatal(err)
			}
			parsed, _ := url.Parse(location)
			state, provider, callbackSession := parsed.Query().Get("state"), "google", session
			switch kind {
			case "missing":
				state = ""
			case "forged":
				state = credentials.Random(32)
			case "expired":
				_, err = manager.Store.DB.Exec("UPDATE mail_oauth_states SET expires=1")
			case "wrong_provider":
				provider = "microsoft"
			case "wrong_session":
				callbackSession = "other"
			case "restricted":
				_, err = manager.Store.DB.Exec("UPDATE sessions SET restricted=1")
			case "session_deleted":
				_, err = manager.Store.DB.Exec("DELETE FROM sessions")
			case "admin_rotated":
				_, err = manager.Store.DB.Exec("UPDATE admin SET version=version+1")
			case "config_changed":
				_, err = manager.SaveApp("google", AppInput{ClientID: "new-client", ClientSecret: "new-secret", Origin: "https://mail.example.test"})
			case "account_deleted":
				_, err = manager.Store.DB.Exec("DELETE FROM accounts")
			case "newer_request":
				_, err = manager.Start(context.Background(), "account", "google", session)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = manager.Consume(context.Background(), provider, state, callbackSession); err == nil {
				t.Fatal("invalid context accepted")
			}
		})
	}
}

func TestConfigurationEncryptionAndValidation(t *testing.T) {
	manager, session := fixture(t, "google")
	bindFixture(t, manager, session, "google")
	app, _ := manager.App("google")
	encoded, _ := json.Marshal(app)
	if strings.Contains(string(encoded), "fixture-client-secret") || strings.Contains(string(app.Secret), "fixture-client-secret") {
		t.Fatal("plaintext secret")
	}
	_, err := manager.SaveApp("google", AppInput{ClientID: app.ClientID, Origin: app.Origin})
	if err != nil {
		t.Fatal(err)
	}
	account, _ := manager.Account("account")
	if account.Status != "connected" || account.ConfigVersion != 2 {
		t.Fatal("secret-only config invalidated identity")
	}
	_, err = manager.SaveApp("google", AppInput{ClientID: app.ClientID, Origin: "https://other.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	account, _ = manager.Account("account")
	if account.Status != "reauthorize" || len(account.Refresh) != 0 {
		t.Fatal("origin change retained authorization")
	}
	for _, origin := range []string{"http://mail.test", "https://a.test/path", "https://a.test?query=1", "https://a.test#fragment", "https://user:pass@a.test", "https://a.test\\evil"} {
		if _, err := manager.SaveApp("google", AppInput{ClientID: app.ClientID, Origin: origin}); err == nil {
			t.Errorf("unsafe origin accepted %s", origin)
		}
	}
	if _, err := manager.SaveApp("microsoft", AppInput{ClientID: "id", ClientSecret: "secret", Origin: "https://mail.test", Authority: "https://evil.test"}); err == nil {
		t.Fatal("arbitrary authority accepted")
	}
}

func TestConcurrentRefreshAndRotation(t *testing.T) {
	for _, provider := range []string{"google", "microsoft"} {
		t.Run(provider, func(t *testing.T) {
			manager, session := fixture(t, provider)
			bindFixture(t, manager, session, provider)
			var calls atomic.Int32
			manager.transport = func(route network.Route) http.RoundTripper {
				return roundTrip(func(request *http.Request) (*http.Response, error) {
					calls.Add(1)
					if request.URL.Scheme != "https" || (request.URL.Host != "oauth2.googleapis.com" && request.URL.Host != "login.microsoftonline.com") || route.Key != "direct" {
						t.Error("non-official endpoint or route")
					}
					if err := request.ParseForm(); err != nil {
						t.Error(err)
					}
					if request.Form.Get("refresh_token") != "fixture-refresh" || request.Form.Get("grant_type") != "refresh_token" {
						t.Error("wrong refresh request")
					}
					payload := map[string]any{"access_token": "fixture-access", "token_type": "Bearer", "expires_in": 3600, "scope": Scope(provider)}
					if provider == "microsoft" {
						payload["refresh_token"] = "rotated-refresh"
					}
					return response(200, payload), nil
				})
			}
			var workers sync.WaitGroup
			for index := 0; index < 20; index++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					credential, err := manager.Credential(context.Background(), "account", "direct")
					if err != nil || credential.Mechanism != "XOAUTH2" || credential.Token != "fixture-access" {
						t.Error("refresh credential unavailable", err)
					}
				}()
			}
			workers.Wait()
			if calls.Load() != 1 {
				t.Fatal("concurrent refresh was not merged", calls.Load())
			}
			account, _ := manager.Account("account")
			plain, err := manager.Vault.Decrypt(account.Refresh, "mail-oauth-refresh:account")
			want := "fixture-refresh"
			if provider == "microsoft" {
				want = "rotated-refresh"
			}
			if err != nil || plain != want || strings.Contains(string(account.Refresh), want) {
				t.Fatal("refresh rotation or encryption failed")
			}
		})
	}
}

func TestAccessExpiryAndRestartRefresh(t *testing.T) {
	for _, provider := range []string{"google", "microsoft"} {
		t.Run(provider, func(t *testing.T) {
			manager, session := fixture(t, provider)
			bindFixture(t, manager, session, provider)
			calls := 0
			manager.transport = func(network.Route) http.RoundTripper {
				return roundTrip(func(request *http.Request) (*http.Response, error) {
					if err := request.ParseForm(); err != nil {
						t.Fatal(err)
					}
					if request.Form.Get("refresh_token") != "fixture-refresh" {
						t.Fatal("restart or expiry lost persisted refresh credential")
					}
					calls++
					return response(200, map[string]any{"access_token": strings.Repeat("fresh-", calls), "token_type": "Bearer", "expires_in": 3600}), nil
				})
			}
			first, err := manager.Credential(context.Background(), "account", "direct")
			if err != nil {
				t.Fatal(err)
			}
			manager.mu.Lock()
			entry := manager.cache["account"]
			entry.token.Expires = time.Now().Add(30 * time.Second)
			manager.cache["account"] = entry
			manager.mu.Unlock()
			second, err := manager.Credential(context.Background(), "account", "direct")
			if err != nil || second.Token == first.Token || calls != 2 {
				t.Fatal("near-expired access token reused", err, calls)
			}
			restarted := &Manager{Store: manager.Store, Vault: manager.Vault, transport: manager.transport}
			third, err := restarted.Credential(context.Background(), "account", "direct")
			if err != nil || third.Token == second.Token || calls != 3 {
				t.Fatal("restart did not refresh from encrypted storage", err, calls)
			}
		})
	}
}

func TestRefreshRaces(t *testing.T) {
	for _, mutation := range []string{"reauthorize", "delete", "disconnect", "config", "write_failure"} {
		t.Run(mutation, func(t *testing.T) {
			manager, session := fixture(t, "google")
			bindFixture(t, manager, session, "google")
			started, finish := make(chan struct{}), make(chan struct{})
			manager.transport = func(network.Route) http.RoundTripper {
				return roundTrip(func(*http.Request) (*http.Response, error) {
					close(started)
					<-finish
					return response(200, map[string]any{"access_token": "old-access", "refresh_token": "old-rotation", "token_type": "Bearer", "expires_in": 3600, "scope": GoogleScope}), nil
				})
			}
			done := make(chan error, 1)
			go func() { _, err := manager.Credential(context.Background(), "account", "direct"); done <- err }()
			<-started
			var err error
			switch mutation {
			case "reauthorize":
				request, _ := authorize(t, manager, session, "google")
				err = manager.Bind(context.Background(), request, Token{Refresh: "new-authorization", Scope: GoogleScope}, Identity{Subject: "fixture-subject"}, nil)
			case "delete":
				_, err = manager.Store.DB.Exec("DELETE FROM accounts WHERE id='account'")
			case "disconnect":
				err = manager.Disconnect("account")
			case "config":
				_, err = manager.SaveApp("google", AppInput{ClientID: "different", ClientSecret: "different", Origin: "https://mail.example.test"})
			case "write_failure":
				_, err = manager.Store.DB.Exec("CREATE TRIGGER fail_refresh BEFORE UPDATE ON mail_oauth_accounts BEGIN SELECT RAISE(ABORT,'fixture database write failure'); END")
			}
			if err != nil {
				t.Fatal(err)
			}
			close(finish)
			if err = <-done; err == nil {
				t.Fatal("stale refresh escaped CAS")
			}
			manager.mu.Lock()
			entry := manager.cache["account"]
			manager.mu.Unlock()
			if entry.token.Access != "" {
				t.Fatal("failed refresh was cached")
			}
			if mutation == "reauthorize" {
				account, _ := manager.Account("account")
				plain, _ := manager.Vault.Decrypt(account.Refresh, "mail-oauth-refresh:account")
				if plain != "new-authorization" {
					t.Fatal("refresh overwrote reauthorization")
				}
			}
		})
	}
}

func TestTokenFailuresAreSanitizedAndBounded(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		status  int
		payload any
		code    string
	}{
		{"invalid_grant", 400, map[string]any{"error": "invalid_grant", "error_description": "sensitive-fixture"}, "oauth_reauthorization_required"},
		{"scope", 200, map[string]any{"access_token": "access", "token_type": "Bearer", "expires_in": 3600, "scope": "https://graph.microsoft.com/Mail.Read"}, "oauth_scope_missing"},
		{"rate_limit", 429, map[string]any{"error": "slow_down"}, "oauth_rate_limited"},
		{"server", 503, map[string]any{"error": "sensitive-fixture"}, "oauth_provider_unavailable"},
		{"redirect", 307, map[string]any{}, "oauth_provider_unavailable"},
		{"bad_type", 200, map[string]any{"access_token": "access", "token_type": "OAUTHBEARER", "expires_in": 3600, "scope": GoogleScope}, "oauth_provider_unavailable"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			manager, session := fixture(t, "google")
			bindFixture(t, manager, session, "google")
			var calls atomic.Int32
			manager.transport = func(network.Route) http.RoundTripper {
				return roundTrip(func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					result := response(scenario.status, scenario.payload)
					result.Header.Set("Location", "https://evil.test/")
					return result, nil
				})
			}
			for index := 0; index < 3; index++ {
				_, err := manager.Credential(context.Background(), "account", "direct")
				if Code(err) != scenario.code || strings.Contains(err.Error(), "sensitive-fixture") {
					t.Fatal("unsafe or incorrect error", Code(err))
				}
			}
			if calls.Load() != 1 {
				t.Fatal("failure caused high-frequency retries or redirect", calls.Load())
			}
		})
	}
	t.Run("oversized", func(t *testing.T) {
		manager, session := fixture(t, "google")
		bindFixture(t, manager, session, "google")
		manager.transport = func(network.Route) http.RoundTripper {
			return roundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", (1<<20)+1)))}, nil
			})
		}
		if _, err := manager.Credential(context.Background(), "account", "direct"); err == nil {
			t.Fatal("unbounded token response")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		manager, session := fixture(t, "google")
		bindFixture(t, manager, session, "google")
		manager.transport = func(network.Route) http.RoundTripper {
			return roundTrip(func(request *http.Request) (*http.Response, error) {
				<-request.Context().Done()
				return nil, request.Context().Err()
			})
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := manager.Credential(ctx, "account", "direct"); err == nil {
			t.Fatal("cancel ignored")
		}
	})
}

func TestOIDCVerificationAndStableIdentity(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"google", "microsoft"} {
		for _, mutation := range []string{"valid", "signature", "issuer", "audience", "expiry", "nonce", "algorithm", "subject", "tenant", "email_missing"} {
			t.Run(provider+"/"+mutation, func(t *testing.T) {
				app := App{Provider: provider, ClientID: "client", Authority: "consumers"}
				issuer := "https://accounts.google.com"
				if provider == "microsoft" {
					issuer = "https://login.microsoftonline.com/" + consumerTenant + "/v2.0"
				}
				claims := map[string]any{"iss": issuer, "aud": "client", "sub": "stable-subject", "tid": consumerTenant, "nonce": "nonce", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "email": "fixture@example.test", "email_verified": true}
				header := map[string]any{"alg": "RS256", "kid": "fixture"}
				switch mutation {
				case "issuer":
					claims["iss"] = "https://evil.test"
				case "audience":
					claims["aud"] = "other"
				case "expiry":
					claims["exp"] = 1
				case "nonce":
					claims["nonce"] = "other"
				case "algorithm":
					header["alg"] = "none"
				case "subject":
					claims["sub"] = ""
				case "tenant":
					claims["tid"] = "other"
				case "email_missing":
					delete(claims, "email")
				}
				encode := func(value any) string {
					data, _ := json.Marshal(value)
					return base64.RawURLEncoding.EncodeToString(data)
				}
				input := encode(header) + "." + encode(claims)
				digest := sha256.Sum256([]byte(input))
				signature, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
				if mutation == "signature" {
					signature[0] ^= 1
				}
				client := network.HTTPClient(roundTrip(func(request *http.Request) (*http.Response, error) {
					if request.URL.Host != "www.googleapis.com" && request.URL.Host != "login.microsoftonline.com" {
						t.Error("JWKS endpoint escaped allowlist")
					}
					return response(200, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "fixture", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}}), nil
				}), time.Second)
				identity, err := verifyIdentity(context.Background(), client, app, input+"."+base64.RawURLEncoding.EncodeToString(signature), "nonce")
				valid := mutation == "valid" || (mutation == "email_missing" && provider == "microsoft") || (mutation == "tenant" && provider == "google")
				if (err == nil) != valid {
					t.Fatal("identity verification outcome", err)
				}
				if valid && identity.Subject != "stable-subject" {
					t.Fatal("not bound to stable subject")
				}
			})
		}
	}
}

func TestExchangePKCEIdentityAndScope(t *testing.T) {
	manager, session := fixture(t, "google")
	request, query := authorize(t, manager, session, "google")
	manager.transport = func(network.Route) http.RoundTripper {
		return roundTrip(func(outgoing *http.Request) (*http.Response, error) {
			outgoing.ParseForm()
			challenge := sha256.Sum256([]byte(outgoing.Form.Get("code_verifier")))
			if outgoing.Form.Get("code") != "bad-code" || base64.RawURLEncoding.EncodeToString(challenge[:]) != query.Get("code_challenge") {
				t.Error("PKCE not transmitted")
			}
			return response(400, map[string]any{"error": "invalid_grant", "error_description": "private-code"}), nil
		})
	}
	if _, _, err := manager.Exchange(context.Background(), request, "bad-code"); Code(err) != "oauth_reauthorization_required" || strings.Contains(err.Error(), "private-code") {
		t.Fatal("code failure leaked or accepted")
	}
	if err := manager.Disconnect("account"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Credential(context.Background(), "account", "direct"); !errors.As(err, new(*Error)) {
		t.Fatal("missing safe error")
	}
}

func TestWrongPKCEVerifierCannotAuthorize(t *testing.T) {
	manager, session := fixture(t, "google")
	request, query := authorize(t, manager, session, "google")
	request.Verifier = credentials.Random(32)
	var calls int
	manager.transport = func(network.Route) http.RoundTripper {
		return roundTrip(func(outgoing *http.Request) (*http.Response, error) {
			calls++
			if err := outgoing.ParseForm(); err != nil {
				t.Fatal(err)
			}
			challenge := sha256.Sum256([]byte(outgoing.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(challenge[:]) == query.Get("code_challenge") {
				t.Fatal("wrong-verifier fixture did not change challenge")
			}
			return response(400, map[string]any{"error": "invalid_grant", "error_description": "fixture-invalid-verifier"}), nil
		})
	}
	if _, _, err := manager.Exchange(context.Background(), request, "fixture-code"); Code(err) != "oauth_reauthorization_required" || calls != 1 {
		t.Fatal("wrong PKCE was accepted or retried", err, calls)
	}
	account, err := manager.Account("account")
	if err != nil || account.Subject != "" || len(account.Refresh) != 0 || account.Status != "pending" {
		t.Fatal("failed PKCE mutated authorization", err)
	}
}

func TestOAuthEndToEndSimulated(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"google", "microsoft"} {
		t.Run(provider, func(t *testing.T) {
			manager, session := fixture(t, provider)
			request, _ := authorize(t, manager, session, provider)
			issuer := "https://accounts.google.com"
			if provider == "microsoft" {
				issuer = "https://login.microsoftonline.com/" + consumerTenant + "/v2.0"
			}
			subject := "original-subject"
			var tokenCalls, jwksCalls atomic.Int32
			manager.transport = func(network.Route) http.RoundTripper {
				return roundTrip(func(outgoing *http.Request) (*http.Response, error) {
					if outgoing.Method == http.MethodGet {
						jwksCalls.Add(1)
						return response(200, map[string]any{"keys": []any{map[string]any{"kid": "key", "kty": "RSA", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}}), nil
					}
					tokenCalls.Add(1)
					outgoing.ParseForm()
					if outgoing.Form.Get("grant_type") == "authorization_code" {
						if outgoing.Form.Get("code_verifier") != request.Verifier || outgoing.Form.Get("redirect_uri") != request.App.Callback {
							t.Error("authorization exchange lost PKCE or redirect binding")
						}
						encode := func(value any) string {
							data, _ := json.Marshal(value)
							return base64.RawURLEncoding.EncodeToString(data)
						}
						claims := map[string]any{"iss": issuer, "aud": request.App.ClientID, "sub": subject, "tid": consumerTenant, "nonce": request.Nonce, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "email": "fixture@example.test", "email_verified": true}
						input := encode(map[string]any{"alg": "RS256", "kid": "key"}) + "." + encode(claims)
						digest := sha256.Sum256([]byte(input))
						signature, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
						return response(200, map[string]any{"access_token": "exchange-access", "refresh_token": "exchange-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": Scope(provider), "id_token": input + "." + base64.RawURLEncoding.EncodeToString(signature)}), nil
					}
					if outgoing.Form.Get("refresh_token") != "exchange-refresh" {
						t.Error("wrong refresh credential")
					}
					return response(200, map[string]any{"access_token": "refreshed-access", "token_type": "Bearer", "expires_in": 3600, "scope": Scope(provider)}), nil
				})
			}
			token, identity, err := manager.Exchange(context.Background(), request, "fixture-code")
			if err != nil {
				t.Fatal(err)
			}
			if err = manager.Bind(context.Background(), request, token, identity, nil); err != nil {
				t.Fatal(err)
			}
			credential, err := manager.Credential(context.Background(), "account", "direct")
			if err != nil || credential.Token != "refreshed-access" || tokenCalls.Load() != 2 || jwksCalls.Load() != 1 {
				t.Fatal("simulated code to refresh flow", err)
			}
			request, _ = authorize(t, manager, session, provider)
			subject = "different-person"
			if _, _, err = manager.Exchange(context.Background(), request, "fixture-code"); Code(err) != "oauth_identity_mismatch" {
				t.Fatal("reauthorization silently changed person", err)
			}
			request, _ = authorize(t, manager, session, provider)
			if _, err = manager.Store.DB.Exec("DELETE FROM sessions"); err != nil {
				t.Fatal(err)
			}
			if err = manager.Bind(context.Background(), request, token, identity, nil); err == nil {
				t.Fatal("session revoked during network request still bound")
			}
		})
	}
}

func TestTokenNetworkPolicyAndExplicitDiagnosticRoute(t *testing.T) {
	manager, session := fixture(t, "google")
	bindFixture(t, manager, session, "google")
	manager.Network = network.New(manager.Store, manager.Vault)
	defer manager.Network.CloseIdle()
	settings, _ := manager.Network.Settings()
	_, err := manager.Network.Save(network.Input{Global: network.Policy{Mode: "proxy", ProxyID: "proxy"}, Telegram: settings.Telegram, Agent: settings.Agent, Profiles: []network.ProfileInput{{Profile: network.Profile{ID: "proxy", Name: "fixture proxy", Protocol: "http", Host: "127.0.0.1", Port: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	var chosen []bool
	manager.transport = func(route network.Route) http.RoundTripper {
		chosen = append(chosen, route.Profile != nil)
		return roundTrip(func(*http.Request) (*http.Response, error) {
			return response(200, map[string]any{"access_token": "access", "token_type": "Bearer", "expires_in": 3600, "scope": GoogleScope}), nil
		})
	}
	route, err := manager.Network.Resolve("account", "account")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Credential(context.Background(), "account", route.Key); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.CredentialForRoute(context.Background(), "account", network.Route{Key: "direct"}); err != nil {
		t.Fatal(err)
	}
	if len(chosen) != 2 || !chosen[0] || chosen[1] {
		t.Fatal("token requests ignored chosen or diagnostic route", chosen)
	}
	manager.Invalidate("account")
	manager.transport = nil
	if _, err = manager.Credential(context.Background(), "account", route.Key); Code(err) != "oauth_network_failed" {
		t.Fatal("failed proxy did not fail closed", err)
	}
}
