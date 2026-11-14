package agent

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
	"mail-auto/internal/modelprovider"
	"mail-auto/internal/network"
	"mail-auto/internal/store"
)

// Saved model connections are candidates only. The runtime keeps reading the
// single current provider row; a connection reaches it only through Activate,
// which verifies the tool loop on exactly the candidate and then copies it
// into the provider row in one transaction. Each record encrypts its key for
// its own AAD, so ciphertext is never copied between records.

// Connection is the redacted view of a saved connection.
type Connection struct {
	ID             string `json:"id"`
	Version        int    `json:"version"`
	Name           string `json:"name"`
	BaseURL        string `json:"base_url"`
	Endpoint       string `json:"endpoint"`
	Model          string `json:"model"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	MaxTokensParam string `json:"max_tokens_param"`
	KeyConfigured  bool   `json:"key_configured"`
	// Active: the current provider was applied from this connection.
	// Pending: active, but the saved connection was edited since and the
	// edit has not been verified and applied.
	Active    bool  `json:"active"`
	Pending   bool  `json:"pending"`
	UpdatedAt int64 `json:"updated_at"`
}

// ConnectionInput creates (ID empty) or edits a connection. An empty APIKey
// keeps the stored key, but only while the API origin is unchanged.
type ConnectionInput struct {
	ID             string `json:"id"`
	Version        int    `json:"version"`
	Name           string `json:"name"`
	BaseURL        string `json:"base_url"`
	Model          string `json:"model"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	MaxTokensParam string `json:"max_tokens_param"`
	APIKey         string `json:"api_key"`
}

// ActivateInput is the candidate to verify and apply. ProviderVersion is the
// current provider version the page was showing.
type ActivateInput struct {
	ConnectionInput
	ProviderVersion int `json:"provider_version"`
}

type ActivateResult struct {
	OK         bool             `json:"ok"`
	Result     string           `json:"result"`
	Unchanged  bool             `json:"unchanged"`
	Provider   ProviderSettings `json:"provider"`
	Connection *Connection      `json:"connection,omitempty"`
}

type ConnectionList struct {
	Connections []Connection     `json:"connections"`
	Provider    ProviderSettings `json:"provider"`
}

var (
	// ErrKeyRequired: no key given and none can be reused for this origin.
	ErrKeyRequired = errors.New("key_required")
	// ErrConnectionInUse: the connection serves the enabled Agent.
	ErrConnectionInUse = errors.New("connection_in_use")
)

type storedConnection struct {
	id      string
	version int
	name    string
	cfg     providerConfig
	secret  []byte
	created int64
	updated int64
}

func connectionAAD(id string) string { return "agent:connection:" + id }
func providerAAD(instanceID string) string {
	return "agent:provider:" + instanceID
}

func loadConnection(q queryer, id string) (storedConnection, error) {
	c := storedConnection{id: id}
	var cfg string
	err := q.QueryRow("SELECT version,name,config,secret,created_at,updated_at FROM agent_provider_connections WHERE id=?", id).Scan(&c.version, &c.name, &cfg, &c.secret, &c.created, &c.updated)
	if err != nil {
		return c, err
	}
	if json.Unmarshal([]byte(cfg), &c.cfg) != nil {
		return c, mail.ErrConflict
	}
	return c, nil
}

func (c storedConnection) view(p storedProvider) Connection {
	endpoint := ""
	if _, e, err := modelprovider.NormalizeBaseURL(c.cfg.BaseURL); err == nil {
		endpoint = e
	}
	active := p.connectionID == c.id
	return Connection{ID: c.id, Version: c.version, Name: c.name, BaseURL: c.cfg.BaseURL, Endpoint: endpoint, Model: c.cfg.Model, TimeoutSeconds: c.cfg.TimeoutSeconds, MaxTokensParam: c.cfg.MaxTokensParam, KeyConfigured: len(c.secret) > 0, Active: active, Pending: active && c.cfg != p.cfg, UpdatedAt: c.updated}
}

// origin is scheme://host[:port]; a stored key is reused only for the origin
// it was entered for.
func origin(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + strings.ToLower(u.Host)
}

func defaultName(base string) string {
	if u, err := url.Parse(base); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "模型连接"
}

func normalizeName(name, base string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = defaultName(base)
	}
	if len([]rune(name)) > 60 || strings.ContainsAny(name, "\r\n\x00") {
		return "", mail.ErrInput
	}
	return name, nil
}

func (r *Runtime) Connections(a mail.Access) (ConnectionList, error) {
	if err := r.requireAdmin(a); err != nil {
		return ConnectionList{}, err
	}
	p, err := r.loadProvider()
	if err != nil {
		return ConnectionList{}, err
	}
	rows, err := r.Store.DB.Query("SELECT id FROM agent_provider_connections ORDER BY created_at,rowid")
	if err != nil {
		return ConnectionList{}, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	out := ConnectionList{Connections: []Connection{}, Provider: p.view()}
	for _, id := range ids {
		c, err := loadConnection(r.Store.DB, id)
		if err != nil {
			return ConnectionList{}, err
		}
		out.Connections = append(out.Connections, c.view(p))
	}
	return out, nil
}

// prepare validates a candidate against the stored connection (nil when
// new) and returns the next record plus the plaintext key to use, if any.
func (r *Runtime) prepare(in ConnectionInput, old *storedConnection) (storedConnection, string, error) {
	cfg, err := normalizeConfig(in.BaseURL, in.Model, in.TimeoutSeconds, in.MaxTokensParam)
	key := strings.TrimSpace(in.APIKey)
	if err != nil || cfg.BaseURL == "" || !validKey(key) {
		return storedConnection{}, "", mail.ErrInput
	}
	name, err := normalizeName(in.Name, cfg.BaseURL)
	if err != nil {
		return storedConnection{}, "", err
	}
	next := storedConnection{name: name, cfg: cfg, secret: []byte{}}
	if old != nil {
		next.id, next.version, next.secret, next.created = old.id, old.version, old.secret, old.created
		if key == "" && len(old.secret) > 0 {
			if origin(old.cfg.BaseURL) != origin(cfg.BaseURL) {
				return storedConnection{}, "", ErrKeyRequired
			}
			key, err = r.Vault.Decrypt(old.secret, connectionAAD(old.id))
			if err != nil {
				return storedConnection{}, "", ErrKeyRequired
			}
		}
	}
	return next, key, nil
}

func (c storedConnection) same(o storedConnection) bool {
	return c.name == o.name && c.cfg == o.cfg && string(c.secret) == string(o.secret)
}

// writeConnection inserts or optimistically updates c; it returns the stored
// record (version bumped only on real change).
func (r *Runtime) writeConnection(tx *sql.Tx, old *storedConnection, next storedConnection, key string, now int64) (storedConnection, error) {
	if old == nil {
		next.id = credentials.Random(12)
		next.version = 1
		next.created = now
	}
	if key != "" {
		reuse := false
		if old != nil && len(old.secret) > 0 {
			if prev, err := r.Vault.Decrypt(old.secret, connectionAAD(old.id)); err == nil && subtle.ConstantTimeCompare([]byte(prev), []byte(key)) == 1 {
				reuse = true
			}
		}
		if !reuse {
			sealed, err := r.Vault.Encrypt(key, connectionAAD(next.id))
			if err != nil {
				return next, mail.ErrConflict
			}
			next.secret = sealed
		}
	}
	cfg, _ := json.Marshal(next.cfg)
	if old == nil {
		next.updated = now
		_, err := tx.Exec("INSERT INTO agent_provider_connections(id,version,name,config,secret,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", next.id, next.version, next.name, string(cfg), next.secret, next.created, next.updated)
		return next, err
	}
	if next.same(*old) {
		return *old, nil
	}
	next.version = old.version + 1
	next.updated = now
	res, err := tx.Exec("UPDATE agent_provider_connections SET version=?,name=?,config=?,secret=?,updated_at=? WHERE id=? AND version=?", next.version, next.name, string(cfg), next.secret, next.updated, next.id, old.version)
	if err != nil {
		return next, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return next, mail.ErrConflict
	}
	return next, nil
}

// SaveConnection stores a candidate without touching the current provider,
// even when it is the active connection (the edit then shows as pending).
func (r *Runtime) SaveConnection(a mail.Access, in ConnectionInput) (Connection, error) {
	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	if err := r.requireAdmin(a); err != nil {
		return Connection{}, err
	}
	var old *storedConnection
	if in.ID != "" {
		c, err := loadConnection(r.Store.DB, in.ID)
		if err != nil {
			return Connection{}, err
		}
		if c.version != in.Version {
			return Connection{}, mail.ErrConflict
		}
		old = &c
	}
	next, key, err := r.prepare(in, old)
	if err != nil {
		return Connection{}, err
	}
	tx, err := r.Store.DB.Begin()
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback()
	saved, err := r.writeConnection(tx, old, next, key, time.Now().Unix())
	if err != nil {
		return Connection{}, err
	}
	if err = store.Audit(tx, "agent.connection.save", saved.id, "ok"); err != nil {
		return Connection{}, err
	}
	p, err := loadProviderQ(tx)
	if err != nil {
		return Connection{}, err
	}
	if err = tx.Commit(); err != nil {
		return Connection{}, err
	}
	return saved.view(p), nil
}

// DeleteConnection refuses the connection serving the enabled Agent: switch
// to another one or disable the Agent first. The current provider row is
// never changed here.
func (r *Runtime) DeleteConnection(a mail.Access, id string, version int) error {
	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	if err := r.requireAdmin(a); err != nil {
		return err
	}
	tx, err := r.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := loadProviderQ(tx)
	if err != nil {
		return err
	}
	if p.connectionID == id && p.enabled {
		return ErrConnectionInUse
	}
	res, err := tx.Exec("DELETE FROM agent_provider_connections WHERE id=? AND version=?", id, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		if _, e := loadConnection(tx, id); errors.Is(e, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		return mail.ErrConflict
	}
	if p.connectionID == id {
		// Not a configuration change: the disabled provider keeps its values.
		if _, err = tx.Exec("UPDATE agent_provider_settings SET connection_id='' WHERE id=1 AND connection_id=?", id); err != nil {
			return err
		}
	}
	if err = store.Audit(tx, "agent.connection.delete", id, "ok"); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Runtime) route(q *sql.Tx) (network.Route, error) {
	if r.Mail.Network == nil {
		return network.Route{Key: "direct"}, nil
	}
	if q != nil {
		return r.Mail.Network.ResolveIn(q, "agent", "")
	}
	return r.Mail.Network.Resolve("agent", "")
}

// ModelsInput asks for the model list of a candidate. Without APIKey the
// stored key of connection ID is used, only for the same API origin.
type ModelsInput struct {
	ID      string `json:"id"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// ListModels fetches the model list on demand through the Agent's network
// route. Nothing is stored.
func (r *Runtime) ListModels(ctx context.Context, a mail.Access, in ModelsInput) ([]string, error) {
	if err := r.requireAdmin(a); err != nil {
		return nil, err
	}
	cfg, err := normalizeConfig(in.BaseURL, "", 0, "")
	key := strings.TrimSpace(in.APIKey)
	if err != nil || cfg.BaseURL == "" || !validKey(key) {
		return nil, mail.ErrInput
	}
	if key == "" {
		if in.ID == "" {
			return nil, ErrKeyRequired
		}
		c, err := loadConnection(r.Store.DB, in.ID)
		if err != nil {
			return nil, err
		}
		if len(c.secret) == 0 || origin(c.cfg.BaseURL) != origin(cfg.BaseURL) {
			return nil, ErrKeyRequired
		}
		if key, err = r.Vault.Decrypt(c.secret, connectionAAD(c.id)); err != nil {
			return nil, ErrKeyRequired
		}
	}
	route, err := r.route(nil)
	if err != nil {
		return nil, &modelprovider.Error{Code: modelprovider.CodeConfig}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if r.ModelLister != nil {
		return r.ModelLister(ctx, cfg.BaseURL, key)
	}
	if r.Mail.Network == nil {
		if err := modelprovider.CheckResolution(ctx, cfg.BaseURL); err != nil {
			return nil, err
		}
	}
	return modelprovider.ListModels(ctx, modelprovider.ProxyTransport(route), cfg.BaseURL, key)
}

// Activate verifies the candidate with the tool-loop check (local echo tool
// only, no business tool) and, if it passes, saves the connection and makes
// it the current provider in one transaction. The commit re-checks the
// candidate and provider versions and the network route; any change rejects
// the stale result. A failed check changes nothing. Re-submitting the
// configuration that is already in effect returns without a new generation.
func (r *Runtime) Activate(ctx context.Context, a mail.Access, in ActivateInput) (ActivateResult, error) {
	if err := r.requireAdmin(a); err != nil {
		return ActivateResult{}, err
	}
	r.settingsMu.Lock()
	p, err := r.loadProvider()
	var old *storedConnection
	if err == nil && in.ID != "" {
		var c storedConnection
		if c, err = loadConnection(r.Store.DB, in.ID); err == nil {
			old = &c
			if c.version != in.Version {
				err = mail.ErrConflict
			}
		}
	}
	if err == nil && p.version != in.ProviderVersion {
		err = mail.ErrConflict
	}
	var next storedConnection
	var key string
	if err == nil {
		next, key, err = r.prepare(in.ConnectionInput, old)
	}
	if err == nil && next.cfg.Model == "" {
		err = mail.ErrInput
	}
	if err == nil && key == "" {
		err = ErrKeyRequired
	}
	var route network.Route
	if err == nil {
		route, err = r.route(nil)
		if err != nil {
			err = &modelprovider.Error{Code: modelprovider.CodeConfig}
		}
	}
	unchanged := err == nil && old != nil && next.name == old.name && next.cfg == old.cfg && p.connectionID == old.id && p.ready() && p.cfg == next.cfg && r.providerKeyIs(p, key)
	r.settingsMu.Unlock()
	if err != nil {
		return ActivateResult{Provider: p.view()}, err
	}
	if unchanged {
		v := old.view(p)
		return ActivateResult{OK: true, Result: "ok", Unchanged: true, Provider: p.view(), Connection: &v}, nil
	}

	code := r.probeCandidate(ctx, next.cfg.settings(key), route)
	_ = store.Audit(r.Store.DB, "agent.connection.check", "tool_loop", code)
	if code != "ok" {
		return ActivateResult{Result: code, Provider: p.view()}, nil
	}

	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	tx, err := r.Store.DB.Begin()
	if err != nil {
		return ActivateResult{}, err
	}
	defer tx.Rollback()
	current, err := loadProviderQ(tx)
	if err != nil {
		return ActivateResult{}, err
	}
	nowRoute, err := r.route(tx)
	if err != nil || current.version != p.version || nowRoute.Key != route.Key {
		return ActivateResult{Result: "config_changed", Provider: current.view()}, mail.ErrConflict
	}
	if old != nil {
		if c, e := loadConnection(tx, old.id); e != nil || c.version != old.version {
			return ActivateResult{Result: "config_changed", Provider: current.view()}, mail.ErrConflict
		}
	}
	now := time.Now().Unix()
	saved, err := r.writeConnection(tx, old, next, key, now)
	if err != nil {
		return ActivateResult{Result: "config_changed", Provider: current.view()}, err
	}
	np := current
	if np.instanceID == "" {
		np.instanceID = credentials.Random(12)
	}
	np.version = current.version + 1
	np.enabled = true
	np.connectionID = saved.id
	np.cfg = next.cfg
	if np.secret, err = r.Vault.Encrypt(key, providerAAD(np.instanceID)); err != nil {
		return ActivateResult{}, mail.ErrConflict
	}
	np.keyGen++
	np.updated = now
	np.caps = Capabilities{ToolLoop: "ok", ToolAt: now}
	np.caps.Digest = np.digest()
	if err = persistProvider(tx, current, np, "agent.connection.activate"); err != nil {
		return ActivateResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return ActivateResult{}, err
	}
	v := saved.view(np)
	return ActivateResult{OK: true, Result: "ok", Provider: np.view(), Connection: &v}, nil
}

func (r *Runtime) providerKeyIs(p storedProvider, key string) bool {
	if len(p.secret) == 0 {
		return false
	}
	current, err := r.Vault.Decrypt(p.secret, providerAAD(p.instanceID))
	return err == nil && subtle.ConstantTimeCompare([]byte(current), []byte(key)) == 1
}

func (r *Runtime) probeCandidate(ctx context.Context, s modelprovider.Settings, route network.Route) string {
	client, err := r.clientFor(s, route)
	if err != nil {
		return modelprovider.CodeOf(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if r.NewProvider == nil && r.Mail.Network == nil {
		if err = modelprovider.CheckResolution(ctx, s.BaseURL); err != nil {
			return modelprovider.CodeOf(err)
		}
	}
	if err = modelprovider.ProbeToolLoop(ctx, client); err != nil {
		return modelprovider.CodeOf(err)
	}
	return "ok"
}

// MigrateConnections completes the schema 13 upgrade: the current provider
// configuration becomes the first saved connection, with its key decrypted
// and re-encrypted for the connection record and verified before the server
// starts. The provider row keeps its version, enabled state and
// capabilities. It is idempotent and a no-op once done.
func (r *Runtime) MigrateConnections() error {
	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	tx, err := r.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := loadProviderQ(tx)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && p.connectionID != store.LegacyConnection) {
		return nil
	}
	if err != nil {
		return err
	}
	link := ""
	if p.cfg.BaseURL != "" {
		c := storedConnection{id: "legacy-" + p.instanceID, version: 1, name: defaultName(p.cfg.BaseURL), cfg: p.cfg, secret: []byte{}, created: p.updated, updated: p.updated}
		if len(p.secret) > 0 {
			key, err := r.Vault.Decrypt(p.secret, providerAAD(p.instanceID))
			if err != nil {
				// The current key was already unusable; keep the provider row
				// as is and migrate the rest without a key.
				slog.Warn("model connection migration: current key cannot be decrypted; saved without key")
			} else {
				if c.secret, err = r.Vault.Encrypt(key, connectionAAD(c.id)); err != nil {
					return err
				}
				if back, err := r.Vault.Decrypt(c.secret, connectionAAD(c.id)); err != nil || back != key {
					return errors.New("model connection migration: key verification failed")
				}
			}
		}
		cfg, _ := json.Marshal(c.cfg)
		if _, err = tx.Exec("INSERT OR IGNORE INTO agent_provider_connections(id,version,name,config,secret,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", c.id, c.version, c.name, string(cfg), c.secret, c.created, c.updated); err != nil {
			return err
		}
		link = c.id
	}
	if _, err = tx.Exec("UPDATE agent_provider_settings SET connection_id=? WHERE id=1 AND connection_id=?", link, store.LegacyConnection); err != nil {
		return err
	}
	if err = store.Audit(tx, "agent.connection.migrate", link, "ok"); err != nil {
		return err
	}
	return tx.Commit()
}
