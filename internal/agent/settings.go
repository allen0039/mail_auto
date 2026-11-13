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
	version    int
	enabled    bool
	instanceID string
	cfg        providerConfig
	secret     []byte
	keyGen     int
	caps       Capabilities
	updated    int64
}

var errNotAdmin = mail.ErrDenied

func (r *Runtime) requireAdmin(a mail.Access) error {
	a, err := r.Mail.CheckAccess(a)
	if err != nil || a.Owner != "admin" {
		return errNotAdmin
	}
	return nil
}

func (r *Runtime) loadProvider() (storedProvider, error) {
	var p storedProvider
	var cfg, caps string
	err := r.Store.DB.QueryRow("SELECT version,enabled,instance_id,config,secret,key_generation,capabilities,updated_at FROM agent_provider_settings WHERE id=1").Scan(&p.version, &p.enabled, &p.instanceID, &cfg, &p.secret, &p.keyGen, &caps, &p.updated)
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
	return ProviderSettings{Version: p.version, Enabled: p.enabled, APIType: p.cfg.APIType, BaseURL: p.cfg.BaseURL, Endpoint: endpoint, Model: p.cfg.Model, TimeoutSeconds: p.cfg.TimeoutSeconds, MaxTokensParam: p.cfg.MaxTokensParam, KeyConfigured: len(p.secret) > 0, Capabilities: caps, UpdatedAt: p.updated}
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
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = 60
	}
	if in.TimeoutSeconds < 10 || in.TimeoutSeconds > 120 || (in.APIKey != "" && in.ClearKey) || len(in.APIKey) > 4096 || strings.ContainsAny(in.APIKey, " \r\n\t\x00") {
		return old.view(), mail.ErrInput
	}
	if in.MaxTokensParam != "" && in.MaxTokensParam != "max_tokens" && in.MaxTokensParam != "max_completion_tokens" {
		return old.view(), mail.ErrInput
	}
	in.Model = strings.TrimSpace(in.Model)
	if len(in.Model) > 200 || strings.ContainsAny(in.Model, "\r\n\x00") {
		return old.view(), mail.ErrInput
	}
	base := strings.TrimSpace(in.BaseURL)
	if base != "" {
		normalized, _, err := modelprovider.NormalizeBaseURL(base)
		if err != nil {
			return old.view(), mail.ErrInput
		}
		base = normalized
	}
	next := old
	if next.instanceID == "" {
		next.instanceID = credentials.Random(12)
	}
	next.version = old.version + 1
	next.enabled = in.Enabled
	next.cfg = providerConfig{APIType: "chat_completions", BaseURL: base, Model: in.Model, TimeoutSeconds: in.TimeoutSeconds, MaxTokensParam: in.MaxTokensParam}
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
	cfg, _ := json.Marshal(next.cfg)
	caps, _ := json.Marshal(map[string]any{"digest": next.caps.Digest, "chat": next.caps.Chat, "chat_at": next.caps.ChatAt, "tool_loop": next.caps.ToolLoop, "tool_loop_at": next.caps.ToolAt})
	tx, err := r.Store.DB.Begin()
	if err != nil {
		return old.view(), err
	}
	defer tx.Rollback()
	if old.version == 0 {
		_, err = tx.Exec("INSERT INTO agent_provider_settings(id,version,enabled,instance_id,config,secret,key_generation,capabilities,updated_at) VALUES(1,?,?,?,?,?,?,?,?)", next.version, next.enabled, next.instanceID, string(cfg), next.secret, next.keyGen, string(caps), next.updated)
	} else {
		var res sql.Result
		res, err = tx.Exec("UPDATE agent_provider_settings SET version=?,enabled=?,config=?,secret=?,key_generation=?,capabilities=?,updated_at=? WHERE id=1 AND version=?", next.version, next.enabled, string(cfg), next.secret, next.keyGen, string(caps), next.updated, old.version)
		if err == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				return old.view(), mail.ErrConflict
			}
		}
	}
	if err != nil {
		return old.view(), err
	}
	if err = store.Audit(tx, "agent.provider.save", "chat_completions", map[bool]string{true: "enabled", false: "disabled"}[next.enabled]); err != nil {
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

// ErrToolLoopUnverified rejects enabling without a passing tool-loop check.
var ErrToolLoopUnverified = errors.New("tool_call_unsupported")

func (r *Runtime) providerClient(p storedProvider) (modelprovider.Provider, error) {
	if len(p.secret) == 0 {
		return nil, &modelprovider.Error{Code: modelprovider.CodeConfig}
	}
	key, err := r.Vault.Decrypt(p.secret, "agent:provider:"+p.instanceID)
	if err != nil {
		return nil, &modelprovider.Error{Code: modelprovider.CodeConfig}
	}
	s := modelprovider.Settings{BaseURL: p.cfg.BaseURL, Model: p.cfg.Model, APIKey: key, Timeout: time.Duration(p.cfg.TimeoutSeconds) * time.Second, MaxTokensParam: p.cfg.MaxTokensParam}
	if r.NewProvider != nil {
		return r.NewProvider(s)
	}
	return modelprovider.NewChatCompletions(s, nil)
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
		} else if r.NewProvider == nil {
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
