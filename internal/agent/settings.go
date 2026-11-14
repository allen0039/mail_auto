// Package agent runs the built-in management agent: conversation turns, the
// model loop over the shared mail tools, proposals with explicit
// confirmation, stop/recovery and run-result reporting.
package agent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
	"mail-auto/internal/modelprovider"
	"mail-auto/internal/network"
	"mail-auto/internal/store"
)

// ProviderSettings is the redacted provider configuration. The API key is
// never returned; KeyConfigured reports whether one is stored.
type ProviderSettings struct {
	Version        int          `json:"version"`
	Enabled        bool         `json:"enabled"`
	APIType        string       `json:"api_type"`
	BaseURL        string       `json:"base_url"`
	Endpoint       string       `json:"endpoint"`
	Model          string       `json:"model"`
	TimeoutSeconds int          `json:"timeout_seconds"`
	MaxTokensParam string       `json:"max_tokens_param"`
	KeyConfigured  bool         `json:"key_configured"`
	ConnectionID   string       `json:"connection_id"`
	Capabilities   Capabilities `json:"capabilities"`
	UpdatedAt      int64        `json:"updated_at"`
}

// Capabilities are bound to the configuration digest they were measured on;
// any change of address, model, key or parameters makes them stale.
type Capabilities struct {
	Digest   string `json:"-"`
	Current  bool   `json:"current"`
	Chat     string `json:"chat"`
	ChatAt   int64  `json:"chat_at"`
	ToolLoop string `json:"tool_loop"`
	ToolAt   int64  `json:"tool_loop_at"`
}

type ProviderInput struct {
	Version        int    `json:"version"`
	Enabled        bool   `json:"enabled"`
	BaseURL        string `json:"base_url"`
	Model          string `json:"model"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	MaxTokensParam string `json:"max_tokens_param"`
	APIKey         string `json:"api_key"`
	ClearKey       bool   `json:"clear_key"`
}

type providerConfig struct {
	APIType        string `json:"api_type"`
	BaseURL        string `json:"base_url"`
	Model          string `json:"model"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	MaxTokensParam string `json:"max_tokens_param"`
}

type storedProvider struct {
	version      int
	enabled      bool
	instanceID   string
	connectionID string
	cfg          providerConfig
	secret       []byte
	keyGen       int
	caps         Capabilities
	updated      int64
}

var errNotAdmin = mail.ErrDenied

func (r *Runtime) requireAdmin(a mail.Access) error {
	a, err := r.Mail.CheckAccess(a)
	if err != nil || a.Owner != "admin" {
		return errNotAdmin
	}
	return nil
}

type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func (r *Runtime) loadProvider() (storedProvider, error) { return loadProviderQ(r.Store.DB) }

func loadProviderQ(q queryer) (storedProvider, error) {
	var p storedProvider
	var cfg, caps string
	err := q.QueryRow("SELECT version,enabled,instance_id,connection_id,config,secret,key_generation,capabilities,updated_at FROM agent_provider_settings WHERE id=1").Scan(&p.version, &p.enabled, &p.instanceID, &p.connectionID, &cfg, &p.secret, &p.keyGen, &caps, &p.updated)
	if errors.Is(err, sql.ErrNoRows) {
		return storedProvider{cfg: providerConfig{APIType: "chat_completions", TimeoutSeconds: 60}}, nil
	}
	if err != nil {
		return p, err
	}
	if json.Unmarshal([]byte(cfg), &p.cfg) != nil || json.Unmarshal([]byte(caps), &p.caps) != nil {
		return p, mail.ErrConflict
	}
	var raw struct {
		Digest string `json:"digest"`
	}
	_ = json.Unmarshal([]byte(caps), &raw)
	p.caps.Digest = raw.Digest
	return p, nil
}

func (p storedProvider) digest() string {
	raw, _ := json.Marshal([]any{p.instanceID, p.cfg, p.keyGen})
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func (p storedProvider) view() ProviderSettings {
	endpoint := ""
	if _, e, err := modelprovider.NormalizeBaseURL(p.cfg.BaseURL); err == nil {
		endpoint = e
	}
	caps := p.caps
	caps.Current = caps.Digest != "" && caps.Digest == p.digest()
	if !caps.Current {
		caps = Capabilities{}
	}
	return ProviderSettings{Version: p.version, Enabled: p.enabled, APIType: p.cfg.APIType, BaseURL: p.cfg.BaseURL, Endpoint: endpoint, Model: p.cfg.Model, TimeoutSeconds: p.cfg.TimeoutSeconds, MaxTokensParam: p.cfg.MaxTokensParam, KeyConfigured: len(p.secret) > 0, ConnectionID: p.publicConnectionID(), Capabilities: caps, UpdatedAt: p.updated}
}

func (p storedProvider) publicConnectionID() string {
	if p.connectionID == store.LegacyConnection {
		return ""
	}
	return p.connectionID
}

// normalizeConfig validates the shared provider fields. An empty base URL is
// allowed here; callers that need one check it.
func normalizeConfig(baseURL, model string, timeout int, maxParam string) (providerConfig, error) {
	if timeout == 0 {
		timeout = 60
	}
	model = strings.TrimSpace(model)
	if timeout < 10 || timeout > 120 || len(model) > 200 || strings.ContainsAny(model, "\r\n\x00") {
		return providerConfig{}, mail.ErrInput
	}
	if maxParam != "" && maxParam != "max_tokens" && maxParam != "max_completion_tokens" {
		return providerConfig{}, mail.ErrInput
	}
	base := strings.TrimSpace(baseURL)
	if base != "" {
		normalized, _, err := modelprovider.NormalizeBaseURL(base)
		if err != nil {
			return providerConfig{}, mail.ErrInput
		}
		base = normalized
	}
	return providerConfig{APIType: "chat_completions", BaseURL: base, Model: model, TimeoutSeconds: timeout, MaxTokensParam: maxParam}, nil
}

func validKey(key string) bool {
	return len(key) <= 4096 && !strings.ContainsAny(key, " \r\n\t\x00")
}

// ready reports whether the provider may serve the write-capable agent:
// enabled, a key stored, and the tool loop verified on this exact config.
func (p storedProvider) ready() bool {
	v := p.view()
	return p.enabled && v.KeyConfigured && v.Capabilities.Current && v.Capabilities.ToolLoop == "ok"
}

func (r *Runtime) Provider(a mail.Access) (ProviderSettings, error) {
	if err := r.requireAdmin(a); err != nil {
		return ProviderSettings{}, err
	}
	p, err := r.loadProvider()
	return p.view(), err
}

func (r *Runtime) SaveProvider(a mail.Access, in ProviderInput) (ProviderSettings, error) {
	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	if err := r.requireAdmin(a); err != nil {
		return ProviderSettings{}, err
	}
	old, err := r.loadProvider()
	if err != nil {
		return ProviderSettings{}, err
	}
	if in.Version != old.version {
		return old.view(), mail.ErrConflict
	}
	in.APIKey = strings.TrimSpace(in.APIKey)
	cfg, err := normalizeConfig(in.BaseURL, in.Model, in.TimeoutSeconds, in.MaxTokensParam)
	if err != nil || (in.APIKey != "" && in.ClearKey) || !validKey(in.APIKey) {
		return old.view(), mail.ErrInput
	}
	base := cfg.BaseURL
	in.Model = cfg.Model
	next := old
	if next.instanceID == "" {
		next.instanceID = credentials.Random(12)
	}
	next.version = old.version + 1
	next.enabled = in.Enabled
	next.cfg = cfg
	if next.cfg != old.cfg || in.ClearKey || in.APIKey != "" {
		// Edited outside a saved connection: the current configuration no
		// longer matches the connection it was applied from.
		next.connectionID = ""
	}
	if in.ClearKey {
		next.secret = []byte{}
		next.keyGen++
		next.enabled = false
	}
	if in.APIKey != "" {
		next.secret, err = r.Vault.Encrypt(in.APIKey, "agent:provider:"+next.instanceID)
		if err != nil {
			return old.view(), mail.ErrConflict
		}
		next.keyGen++
	}
	if next.secret == nil {
		next.secret = []byte{}
	}
	if next.enabled && (base == "" || in.Model == "" || len(next.secret) == 0) {
		return old.view(), mail.ErrInput
	}
	if next.enabled && !next.ready() {
		// Enabling a write-capable agent requires a verified tool loop on
		// exactly this configuration.
		return old.view(), ErrToolLoopUnverified
	}
	next.updated = time.Now().Unix()
	tx, err := r.Store.DB.Begin()
	if err != nil {
		return old.view(), err
	}
	defer tx.Rollback()
	if err = persistProvider(tx, old, next, "agent.provider.save"); err != nil {
		return old.view(), err
	}
	if err = tx.Commit(); err != nil {
		return old.view(), err
	}
	if !next.enabled {
		r.cancelCurrent("provider_disabled")
	}
	return next.view(), nil
}

// persistProvider writes next over old (optimistically on old.version) and
// applies the generation rules of a configuration change: proposals frozen
// under another version can no longer be confirmed or applied, and disabling
// also cancels queued and running turns. Fixed tasks enabled on their own
// keep running under their own authorization.
func persistProvider(tx *sql.Tx, old, next storedProvider, audit string) error {
	cfg, _ := json.Marshal(next.cfg)
	caps, _ := json.Marshal(map[string]any{"digest": next.caps.Digest, "chat": next.caps.Chat, "chat_at": next.caps.ChatAt, "tool_loop": next.caps.ToolLoop, "tool_loop_at": next.caps.ToolAt})
	var err error
	if old.version == 0 {
		_, err = tx.Exec("INSERT INTO agent_provider_settings(id,version,enabled,instance_id,connection_id,config,secret,key_generation,capabilities,updated_at) VALUES(1,?,?,?,?,?,?,?,?,?)", next.version, next.enabled, next.instanceID, next.connectionID, string(cfg), next.secret, next.keyGen, string(caps), next.updated)
		if err != nil {
			return mail.ErrConflict
		}
	} else {
		var res sql.Result
		res, err = tx.Exec("UPDATE agent_provider_settings SET version=?,enabled=?,connection_id=?,config=?,secret=?,key_generation=?,capabilities=?,updated_at=? WHERE id=1 AND version=?", next.version, next.enabled, next.connectionID, string(cfg), next.secret, next.keyGen, string(caps), next.updated, old.version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return mail.ErrConflict
		}
	}
	now := time.Now().Unix()
	if _, err = tx.Exec("UPDATE agent_proposals SET state='invalidated',error_code=?,updated_at=? WHERE state IN ('pending','accepted','applying') AND config_version<>?", map[bool]string{true: "config_changed", false: "agent_disabled"}[next.enabled], now, next.version); err != nil {
		return err
	}
	if !next.enabled {
		if _, err = tx.Exec("UPDATE agent_turns SET state='cancelled',error_code='agent_disabled',updated_at=? WHERE state IN ('queued','running')", now); err != nil {
			return err
		}
	}
	return store.Audit(tx, audit, "chat_completions", map[bool]string{true: "enabled", false: "disabled"}[next.enabled])
}

// ErrToolLoopUnverified rejects enabling without a passing tool-loop check.
var ErrToolLoopUnverified = errors.New("tool_call_unsupported")

func (r *Runtime) providerClient(p storedProvider) (modelprovider.Provider, error) {
	route := network.Route{Key: "direct"}
	if r.Mail.Network != nil {
		var err error
		route, err = r.Mail.Network.Resolve("agent", "")
		if err != nil {
			return nil, &modelprovider.Error{Code: modelprovider.CodeConfig}
		}
	}
	return r.providerClientRoute(p, route)
}

func (r *Runtime) providerClientRoute(p storedProvider, route network.Route) (modelprovider.Provider, error) {
	if len(p.secret) == 0 {
		return nil, &modelprovider.Error{Code: modelprovider.CodeConfig}
	}
	key, err := r.Vault.Decrypt(p.secret, "agent:provider:"+p.instanceID)
	if err != nil {
		return nil, &modelprovider.Error{Code: modelprovider.CodeConfig}
	}
	return r.clientFor(p.cfg.settings(key), route)
}

func (c providerConfig) settings(key string) modelprovider.Settings {
	return modelprovider.Settings{BaseURL: c.BaseURL, Model: c.Model, APIKey: key, Timeout: time.Duration(c.TimeoutSeconds) * time.Second, MaxTokensParam: c.MaxTokensParam}
}

func (r *Runtime) clientFor(s modelprovider.Settings, route network.Route) (modelprovider.Provider, error) {
	if r.NewProvider != nil {
		return r.NewProvider(s)
	}
	return modelprovider.NewChatCompletions(s, modelprovider.ProxyTransport(route))
}

// CheckProvider runs the chat or tool_loop test. Only the local echo tool is
// used. The result is stored only if the configuration did not change while
// the test ran, so an old in-flight result never marks a new config ready.
func (r *Runtime) CheckProvider(ctx context.Context, a mail.Access, kind string) (ProviderSettings, string, error) {
	if err := r.requireAdmin(a); err != nil {
		return ProviderSettings{}, "", err
	}
	if kind != "chat" && kind != "tool_loop" {
		return ProviderSettings{}, "", mail.ErrInput
	}
	p, err := r.loadProvider()
	if err != nil {
		return ProviderSettings{}, "", err
	}
	client, err := r.providerClient(p)
	code := "ok"
	if err != nil {
		code = modelprovider.CodeOf(err)
	} else {
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		if base, _, e := modelprovider.NormalizeBaseURL(p.cfg.BaseURL); e != nil {
			err = e
		} else if r.NewProvider == nil && r.Mail.Network == nil {
			err = modelprovider.CheckResolution(ctx, base)
		}
		if err == nil {
			if kind == "chat" {
				err = modelprovider.ProbeChat(ctx, client)
			} else {
				err = modelprovider.ProbeToolLoop(ctx, client)
			}
		}
		if err != nil {
			code = modelprovider.CodeOf(err)
		}
	}
	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	current, err := r.loadProvider()
	if err != nil {
		return ProviderSettings{}, "", err
	}
	if current.version != p.version || current.digest() != p.digest() {
		return current.view(), "config_changed", mail.ErrConflict
	}
	caps := current.caps
	if caps.Digest != current.digest() {
		caps = Capabilities{Digest: current.digest()}
	}
	now := time.Now().Unix()
	if kind == "chat" {
		caps.Chat, caps.ChatAt = code, now
	} else {
		caps.ToolLoop, caps.ToolAt = code, now
	}
	raw, _ := json.Marshal(map[string]any{"digest": caps.Digest, "chat": caps.Chat, "chat_at": caps.ChatAt, "tool_loop": caps.ToolLoop, "tool_loop_at": caps.ToolAt})
	// The version is unchanged: capability records do not alter configuration.
	if _, err = r.Store.DB.Exec("UPDATE agent_provider_settings SET capabilities=? WHERE id=1 AND version=?", string(raw), current.version); err != nil {
		return ProviderSettings{}, "", err
	}
	_ = store.Audit(r.Store.DB, "agent.provider.check", kind, code)
	current.caps = caps
	return current.view(), code, nil
}
