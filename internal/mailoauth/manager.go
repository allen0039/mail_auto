package mailoauth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/network"
	"mail-auto/internal/store"
)

type cached struct {
	token      Token
	generation int64
	config     int64
	route      string
	retryAt    time.Time
	err        error
}
type Manager struct {
	Store     *store.Store
	Vault     *credentials.Vault
	Network   *network.Manager
	Changed   func(string)
	mu        sync.Mutex
	locks     map[string]*sync.Mutex
	cache     map[string]cached
	transport func(network.Route) http.RoundTripper
}

func (manager *Manager) lock(id string) func() {
	manager.mu.Lock()
	if manager.locks == nil {
		manager.locks = map[string]*sync.Mutex{}
	}
	lock := manager.locks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		manager.locks[id] = lock
	}
	manager.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}
func (manager *Manager) Invalidate(id string) {
	manager.mu.Lock()
	if id == "" {
		manager.cache = nil
	} else {
		delete(manager.cache, id)
	}
	manager.mu.Unlock()
	if manager.Changed != nil {
		manager.Changed(id)
	}
}
func (manager *Manager) App(provider string) (App, error) {
	app := App{Provider: provider}
	if !IsProvider(provider) {
		return app, Failure("oauth_config_invalid")
	}
	err := manager.Store.DB.QueryRow(`SELECT client_id,secret,authority,origin,version,secret_expires FROM mail_oauth_apps WHERE provider=?`, provider).Scan(&app.ClientID, &app.Secret, &app.Authority, &app.Origin, &app.Version, &app.SecretExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return app, nil
	}
	app.Configured = err == nil && app.ClientID != "" && len(app.Secret) > 0
	app.Callback = app.Origin + callback(provider)
	return app, err
}
func (manager *Manager) SaveApp(provider string, input AppInput) (App, error) {
	input.ClientID = strings.TrimSpace(input.ClientID)
	input.Origin = strings.TrimRight(strings.TrimSpace(input.Origin), "/")
	if input.Authority == "" {
		input.Authority = "consumers"
	}
	if !IsProvider(provider) || !validOrigin(input.Origin) || input.ClientID == "" || len(input.ClientID) > 512 || strings.ContainsAny(input.ClientID, "\r\n\x00") || len(input.ClientSecret) > 8192 || (provider == "microsoft" && input.Authority != "consumers" && input.Authority != "common") {
		return App{}, Failure("oauth_config_invalid")
	}
	tx, err := manager.Store.DB.Begin()
	if err != nil {
		return App{}, err
	}
	defer tx.Rollback()
	var old App
	err = tx.QueryRow(`SELECT client_id,secret,authority,origin,version FROM mail_oauth_apps WHERE provider=?`, provider).Scan(&old.ClientID, &old.Secret, &old.Authority, &old.Origin, &old.Version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return App{}, err
	}
	secret := old.Secret
	if input.ClientSecret != "" {
		secret, err = manager.Vault.Encrypt(input.ClientSecret, "mail-oauth-app:"+provider)
		if err != nil {
			return App{}, err
		}
	} else if old.ClientID != input.ClientID || len(secret) == 0 {
		return App{}, Failure("oauth_config_invalid")
	}
	version := old.Version + 1
	_, err = tx.Exec(`INSERT INTO mail_oauth_apps(provider,client_id,secret,authority,origin,version,secret_expires,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(provider) DO UPDATE SET client_id=excluded.client_id,secret=excluded.secret,authority=excluded.authority,origin=excluded.origin,version=excluded.version,secret_expires=excluded.secret_expires,updated_at=excluded.updated_at`, provider, input.ClientID, secret, input.Authority, input.Origin, version, input.SecretExpires, time.Now().Unix())
	if err != nil {
		return App{}, err
	}
	if _, err = tx.Exec("DELETE FROM mail_oauth_states WHERE provider=?", provider); err != nil {
		return App{}, err
	}
	if old.ClientID == input.ClientID && old.Authority == input.Authority && old.Origin == input.Origin {
		_, err = tx.Exec("UPDATE mail_oauth_accounts SET config_version=?,generation=generation+1 WHERE provider=?", version, provider)
	} else {
		_, err = tx.Exec("UPDATE mail_oauth_accounts SET config_version=?,generation=generation+1,refresh=NULL,status='reauthorize',error_code='oauth_config_changed' WHERE provider=?", version, provider)
	}
	if err != nil {
		return App{}, err
	}
	if err = store.Audit(tx, "mail.oauth_app.update", provider, "ok"); err != nil {
		return App{}, err
	}
	if err = tx.Commit(); err != nil {
		return App{}, err
	}
	manager.Invalidate("")
	return manager.App(provider)
}

const accountColumns = "account_id,provider,subject,tenant,login,scope,refresh,generation,config_version,status,error_code"

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var account Account
	err := row.Scan(&account.ID, &account.Provider, &account.Subject, &account.Tenant, &account.Login, &account.Scope, &account.Refresh, &account.Generation, &account.ConfigVersion, &account.Status, &account.ErrorCode)
	return account, err
}
func (manager *Manager) Account(id string) (Account, error) {
	return scanAccount(manager.Store.DB.QueryRow("SELECT "+accountColumns+" FROM mail_oauth_accounts WHERE account_id=?", id))
}
func (manager *Manager) route(id string) (network.Route, error) {
	if manager.Network != nil {
		return manager.Network.Resolve("account", id)
	}
	return network.Route{Key: "direct"}, nil
}
func (manager *Manager) client(route network.Route) *http.Client {
	var transport http.RoundTripper = route.Transport()
	if manager.Network != nil {
		transport = manager.Network.Transport(route)
	}
	if manager.transport != nil {
		transport = manager.transport(route)
	}
	return network.HTTPClient(transport, 25*time.Second)
}
func (manager *Manager) Start(ctx context.Context, id, provider, sessionHash string) (string, error) {
	account, err := manager.Account(id)
	if err != nil || account.Provider != provider {
		return "", Failure("oauth_state_invalid")
	}
	app, err := manager.App(provider)
	if err != nil {
		return "", err
	}
	if !app.Configured {
		return "", Failure("oauth_config_missing")
	}
	if app.SecretExpires > 0 && app.SecretExpires <= time.Now().Unix() {
		return "", Failure("oauth_config_invalid")
	}
	route, err := manager.route(id)
	if err != nil {
		return "", err
	}
	state, verifier, nonce := credentials.Random(32), credentials.Random(32), credentials.Random(32)
	encrypted, err := manager.Vault.Encrypt(verifier, "mail-oauth-state:"+credentials.Digest(state))
	if err != nil {
		return "", err
	}
	tx, err := manager.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var valid int
	err = tx.QueryRow(`SELECT count(*) FROM sessions WHERE token_hash=? AND restricted=0 AND expires>? AND version=(SELECT version FROM admin WHERE id=1)`, sessionHash, time.Now().Unix()).Scan(&valid)
	if err != nil {
		return "", err
	}
	if valid != 1 {
		return "", Failure("oauth_state_invalid")
	}
	if _, err = tx.Exec("DELETE FROM mail_oauth_states WHERE account_id=? OR expires<?", id, time.Now().Unix()); err != nil {
		return "", err
	}
	result, err := tx.Exec(`INSERT INTO mail_oauth_states(state_hash,session_hash,provider,account_id,generation,config_version,network_revision,verifier,nonce,expires) SELECT ?,?,?,?,?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM mail_oauth_accounts a JOIN mail_oauth_apps p ON p.provider=a.provider WHERE a.account_id=? AND a.generation=? AND p.version=?)`, credentials.Digest(state), sessionHash, provider, id, account.Generation, app.Version, route.Key, encrypted, nonce, time.Now().Add(10*time.Minute).Unix(), id, account.Generation, app.Version)
	if err != nil {
		return "", err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return "", Failure("oauth_config_changed")
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	authorize, _, _ := endpoints(app)
	query := url.Values{"client_id": {app.ClientID}, "redirect_uri": {app.Callback}, "response_type": {"code"}, "scope": {scopes(provider)}, "state": {state}, "nonce": {nonce}, "code_challenge_method": {"S256"}}
	challenge := sha256.Sum256([]byte(verifier))
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	if provider == "google" {
		query.Set("access_type", "offline")
		query.Set("prompt", "consent select_account")
	} else {
		query.Set("prompt", "select_account")
		query.Set("response_mode", "query")
	}
	return authorize + "?" + query.Encode(), nil
}

func (manager *Manager) Consume(ctx context.Context, provider, state, sessionHash string) (Request, error) {
	var request Request
	if !IsProvider(provider) || len(state) != 43 {
		return request, Failure("oauth_state_invalid")
	}
	var encrypted []byte
	var id string
	var generation, configVersion int64
	err := manager.Store.DB.QueryRowContext(ctx, `DELETE FROM mail_oauth_states WHERE state_hash=? AND provider=? AND session_hash=? AND expires>? AND EXISTS (SELECT 1 FROM sessions WHERE token_hash=? AND expires>? AND restricted=0 AND version=(SELECT version FROM admin WHERE id=1)) RETURNING account_id,generation,config_version,network_revision,verifier,nonce`, credentials.Digest(state), provider, sessionHash, time.Now().Unix(), sessionHash, time.Now().Unix()).Scan(&id, &generation, &configVersion, &request.NetworkRevision, &encrypted, &request.Nonce)
	if err != nil {
		return request, Failure("oauth_state_invalid")
	}
	request.Account, err = manager.Account(id)
	if err != nil {
		return request, Failure("oauth_state_invalid")
	}
	request.App, err = manager.App(provider)
	if err != nil {
		return request, err
	}
	route, err := manager.route(id)
	if err != nil {
		return request, err
	}
	if request.Account.Provider != provider || request.Account.Generation != generation || request.App.Version != configVersion || route.Key != request.NetworkRevision {
		return request, Failure("oauth_config_changed")
	}
	request.Verifier, err = manager.Vault.Decrypt(encrypted, "mail-oauth-state:"+credentials.Digest(state))
	request.SessionHash = sessionHash
	return request, err
}

func (manager *Manager) Exchange(ctx context.Context, request Request, code string) (Token, Identity, error) {
	route, err := manager.route(request.Account.ID)
	if err != nil {
		return Token{}, Identity{}, err
	}
	if route.Key != request.NetworkRevision {
		return Token{}, Identity{}, Failure("oauth_config_changed")
	}
	client := manager.client(route)
	defer client.CloseIdleConnections()
	token, err := manager.tokenRequest(ctx, client, request.App, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {request.Verifier}, "redirect_uri": {request.App.Callback}})
	if err != nil {
		return token, Identity{}, err
	}
	identity, err := verifyIdentity(ctx, client, request.App, token.IDToken, request.Nonce)
	if err != nil {
		return Token{}, Identity{}, err
	}
	if request.Account.Subject != "" && (request.Account.Subject != identity.Subject || request.Account.Tenant != identity.Tenant) {
		return Token{}, Identity{}, Failure("oauth_identity_mismatch")
	}
	if request.App.Provider == "google" && !strings.EqualFold(identity.Email, request.Account.Login) {
		return Token{}, Identity{}, Failure("oauth_identity_mismatch")
	}
	if request.App.Provider == "microsoft" && request.Account.Subject == "" {
		if (identity.Email != "" && !strings.EqualFold(identity.Email, request.Account.Login)) || (identity.Tenant != consumerTenant && identity.Email == "") {
			return Token{}, Identity{}, Failure("oauth_identity_mismatch")
		}
	}
	return token, identity, nil
}

func (manager *Manager) Bind(ctx context.Context, request Request, token Token, identity Identity, diagnostic error) error {
	if token.Refresh == "" {
		return Failure("oauth_reauthorization_required")
	}
	encrypted, err := manager.Vault.Encrypt(token.Refresh, "mail-oauth-refresh:"+request.Account.ID)
	if err != nil {
		return err
	}
	route, err := manager.route(request.Account.ID)
	if err != nil {
		return err
	}
	if route.Key != request.NetworkRevision {
		return Failure("oauth_config_changed")
	}
	status, code := "connected", ""
	if diagnostic != nil {
		status, code = "network_error", "oauth_network_failed"
	}
	tx, err := manager.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if request.Account.Subject == "" {
		var active int
		if err = tx.QueryRow("SELECT count(*) FROM accounts a LEFT JOIN mail_oauth_accounts o ON o.account_id=a.id WHERE o.account_id IS NULL OR o.subject<>''").Scan(&active); err != nil {
			return err
		}
		if active >= 10 {
			return Failure("oauth_account_limit")
		}
	}
	result, err := tx.Exec(`UPDATE mail_oauth_accounts SET subject=?,tenant=?,scope=?,refresh=?,generation=generation+1,config_version=?,status=?,error_code=? WHERE account_id=? AND generation=? AND provider=? AND EXISTS (SELECT 1 FROM mail_oauth_apps WHERE provider=? AND version=?) AND EXISTS (SELECT 1 FROM sessions WHERE token_hash=? AND expires>? AND restricted=0 AND version=(SELECT version FROM admin WHERE id=1))`, identity.Subject, identity.Tenant, token.Scope, encrypted, request.App.Version, status, code, request.Account.ID, request.Account.Generation, request.App.Provider, request.App.Provider, request.App.Version, request.SessionHash, time.Now().Unix())
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return Failure("oauth_config_changed")
	}
	if err = store.Audit(tx, "mail.oauth.authorized", request.Account.ID, status); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	manager.Invalidate(request.Account.ID)
	return nil
}

func (manager *Manager) Disconnect(id string) error {
	tx, err := manager.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM mail_oauth_states WHERE account_id=?", id); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE mail_oauth_accounts SET refresh=NULL,status='reauthorize',error_code='oauth_reauthorization_required',generation=generation+1 WHERE account_id=?", id); err != nil {
		return err
	}
	if err = store.Audit(tx, "mail.oauth.disconnect_local", id, "ok"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	manager.Invalidate(id)
	return nil
}

func (manager *Manager) Credential(ctx context.Context, id, expectedRoute string) (Credential, error) {
	return manager.credential(ctx, id, expectedRoute, nil)
}

func (manager *Manager) CredentialForRoute(ctx context.Context, id string, route network.Route) (Credential, error) {
	return manager.credential(ctx, id, route.Key, &route)
}

func (manager *Manager) credential(ctx context.Context, id, expectedRoute string, override *network.Route) (Credential, error) {
	unlock := manager.lock(id)
	defer unlock()
	account, err := manager.Account(id)
	if err != nil {
		return Credential{}, Failure("oauth_config_changed")
	}
	if account.Status == "pending" {
		return Credential{}, Failure("oauth_pending")
	}
	if account.Status == "reauthorize" || len(account.Refresh) == 0 {
		if account.ErrorCode == "oauth_scope_missing" {
			return Credential{}, Failure("oauth_scope_missing")
		}
		return Credential{}, Failure("oauth_reauthorization_required")
	}
	app, err := manager.App(account.Provider)
	if err != nil {
		return Credential{}, err
	}
	if !app.Configured {
		return Credential{}, Failure("oauth_config_missing")
	}
	if app.Version != account.ConfigVersion {
		return Credential{}, Failure("oauth_config_changed")
	}
	route, err := manager.route(id)
	if err != nil {
		return Credential{}, err
	}
	configuredRoute := route
	if override != nil {
		route = *override
	}
	if expectedRoute != "" && route.Key != expectedRoute {
		return Credential{}, Failure("oauth_config_changed")
	}
	manager.mu.Lock()
	entry := manager.cache[id]
	manager.mu.Unlock()
	routeKey := route.Key + "|configured:" + configuredRoute.Key
	valid := entry.generation == account.Generation && entry.config == app.Version && entry.route == routeKey
	if valid && time.Now().Before(entry.retryAt) {
		return Credential{}, entry.err
	}
	if valid && time.Now().Add(time.Minute).Before(entry.token.Expires) {
		return Credential{Mechanism: "XOAUTH2", Username: account.Login, Token: entry.token.Access}, nil
	}
	refresh, err := manager.Vault.Decrypt(account.Refresh, "mail-oauth-refresh:"+id)
	if err != nil {
		return Credential{}, err
	}
	client := manager.client(route)
	defer client.CloseIdleConnections()
	token, err := manager.tokenRequest(ctx, client, app, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "scope": {scopes(account.Provider)}})
	if err != nil {
		code := Code(err)
		if code == "oauth_reauthorization_required" || code == "oauth_scope_missing" {
			_, writeErr := manager.Store.DB.Exec("UPDATE mail_oauth_accounts SET status='reauthorize',error_code=? WHERE account_id=? AND generation=? AND config_version=? AND refresh=?", code, id, account.Generation, app.Version, account.Refresh)
			if writeErr != nil {
				return Credential{}, writeErr
			}
		}
		manager.mu.Lock()
		if manager.cache == nil {
			manager.cache = map[string]cached{}
		}
		manager.cache[id] = cached{generation: account.Generation, config: app.Version, route: routeKey, retryAt: time.Now().Add(time.Minute), err: err}
		manager.mu.Unlock()
		return Credential{}, err
	}
	if token.Refresh == "" {
		token.Refresh = refresh
	}
	encrypted, err := manager.Vault.Encrypt(token.Refresh, "mail-oauth-refresh:"+id)
	if err != nil {
		return Credential{}, err
	}
	currentRoute, err := manager.route(id)
	if err != nil {
		return Credential{}, err
	}
	if currentRoute.Key != configuredRoute.Key {
		return Credential{}, Failure("oauth_config_changed")
	}
	result, err := manager.Store.DB.Exec(`UPDATE mail_oauth_accounts SET refresh=?,error_code='',status='connected' WHERE account_id=? AND generation=? AND config_version=? AND refresh=? AND status<>'reauthorize' AND EXISTS (SELECT 1 FROM mail_oauth_apps WHERE provider=? AND version=?)`, encrypted, id, account.Generation, app.Version, account.Refresh, account.Provider, app.Version)
	if err != nil {
		return Credential{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return Credential{}, Failure("oauth_config_changed")
	}
	manager.mu.Lock()
	if manager.cache == nil {
		manager.cache = map[string]cached{}
	}
	manager.cache[id] = cached{token: token, generation: account.Generation, config: app.Version, route: routeKey}
	manager.mu.Unlock()
	return Credential{Mechanism: "XOAUTH2", Username: account.Login, Token: token.Access}, nil
}

func (manager *Manager) AuthenticationFailed(id, token string) {
	manager.mu.Lock()
	entry, exists := manager.cache[id]
	manager.mu.Unlock()
	if !exists || entry.token.Access != token {
		return
	}
	_, err := manager.Store.DB.Exec("UPDATE mail_oauth_accounts SET status='reauthorize',error_code='oauth_imap_denied' WHERE account_id=? AND generation=? AND config_version=?", id, entry.generation, entry.config)
	if err == nil {
		manager.mu.Lock()
		delete(manager.cache, id)
		manager.mu.Unlock()
	}
}

func (manager *Manager) ValidateGeneration(id string, generation, config int64) error {
	account, err := manager.Account(id)
	if err != nil || account.Generation != generation || account.ConfigVersion != config || account.Status == "pending" || account.Status == "reauthorize" {
		return Failure("oauth_config_changed")
	}
	return nil
}
