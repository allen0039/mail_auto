package network

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

var ErrConfig = errors.New("network_config_invalid")
var ErrConflict = errors.New("network_config_changed")
var ErrReferenced = errors.New("proxy_in_use")

type Policy struct {
	Mode    string `json:"mode"`
	ProxyID string `json:"proxy_id"`
}

type Profile struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Protocol         string `json:"protocol"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	Username         string `json:"username"`
	PasswordSet      bool   `json:"password_set"`
	TrustedRemoteDNS bool   `json:"trusted_remote_dns"`
}

type ProfileInput struct {
	Profile
	Password      string `json:"password"`
	ClearPassword bool   `json:"clear_password"`
}

type Settings struct {
	Version  int       `json:"version"`
	Global   Policy    `json:"global"`
	Telegram Policy    `json:"telegram"`
	Agent    Policy    `json:"agent"`
	Profiles []Profile `json:"profiles"`
}

type Input struct {
	Version  int            `json:"version"`
	Global   Policy         `json:"global"`
	Telegram Policy         `json:"telegram"`
	Agent    Policy         `json:"agent"`
	Profiles []ProfileInput `json:"profiles"`
}

type Manager struct {
	Store *store.Store
	Vault *credentials.Vault
	mu    sync.Mutex
	http  map[string]*http.Transport
}

func New(st *store.Store, vault *credentials.Vault) *Manager {
	return &Manager{Store: st, Vault: vault, http: map[string]*http.Transport{}}
}

type querier interface {
	QueryRow(string, ...any) *sql.Row
}

func defaults() Settings {
	return Settings{Global: Policy{Mode: "direct"}, Telegram: Policy{Mode: "inherit"}, Agent: Policy{Mode: "inherit"}, Profiles: []Profile{}}
}

func load(q querier) (Settings, error) {
	out := defaults()
	var raw string
	err := q.QueryRow("SELECT version,payload FROM network_settings WHERE id=1").Scan(&out.Version, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	version := out.Version
	if json.Unmarshal([]byte(raw), &out) != nil {
		return out, ErrConfig
	}
	out.Version = version
	return out, nil
}

func (m *Manager) Settings() (Settings, error) { return load(m.Store.DB) }

func NormalizePolicy(p Policy, inherit bool) (Policy, error) {
	if p.Mode == "" && inherit {
		p.Mode = "inherit"
	}
	if p.Mode == "proxy" && p.ProxyID != "" && len(p.ProxyID) <= 80 {
		return p, nil
	}
	if (p.Mode == "direct" || inherit && p.Mode == "inherit") && p.ProxyID == "" {
		return p, nil
	}
	return p, ErrConfig
}

func validatePolicy(p Policy, profiles map[string]bool, inherit bool) error {
	normalized, err := NormalizePolicy(p, inherit)
	if err != nil || normalized != p || p.Mode == "proxy" && !profiles[p.ProxyID] {
		return ErrConfig
	}
	return nil
}

func validProfile(p *ProfileInput) bool {
	p.Name = strings.TrimSpace(p.Name)
	p.Host = strings.TrimSpace(p.Host)
	if p.Protocol == "socks5h" {
		p.Protocol = "socks5"
	}
	if p.Protocol != "http" && p.Protocol != "https" && p.Protocol != "socks5" {
		return false
	}
	if len(p.Name) == 0 || len(p.Name) > 80 || len(p.Host) == 0 || len(p.Host) > 253 || p.Port < 1 || p.Port > 65535 {
		return false
	}
	if strings.ContainsAny(p.Name+p.Host+p.Username+p.Password, "\r\n\x00") || strings.ContainsAny(p.Host, "/\\@?# \t") {
		return false
	}
	if net.ParseIP(p.Host) == nil {
		for _, label := range strings.Split(p.Host, ".") {
			if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return false
			}
			for _, char := range label {
				if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-') {
					return false
				}
			}
		}
	}
	if len(p.Username) > 255 || strings.Contains(p.Username, ":") || len(p.Password) > 4096 || p.Password != "" && p.ClearPassword {
		return false
	}
	return p.Protocol != "socks5" || len(p.Password) <= 255
}

func (m *Manager) Save(in Input) (Settings, error) {
	tx, err := m.Store.DB.Begin()
	if err != nil {
		return Settings{}, err
	}
	defer tx.Rollback()
	old, err := load(tx)
	if err != nil {
		return old, err
	}
	if old.Version != in.Version {
		return old, ErrConflict
	}
	oldAgent, err := m.resolve(tx, "agent", "")
	if err != nil {
		return old, err
	}
	if len(in.Profiles) > 20 {
		return old, ErrConfig
	}
	out := Settings{Version: old.Version + 1, Global: in.Global, Telegram: in.Telegram, Agent: in.Agent, Profiles: []Profile{}}
	ids := map[string]bool{}
	secrets := map[string][]byte{}
	for _, profile := range in.Profiles {
		if !validProfile(&profile) {
			return old, ErrConfig
		}
		if profile.ID == "" {
			profile.ID = credentials.Random(18)
		} else if len(profile.ID) > 80 || ids[profile.ID] {
			return old, ErrConfig
		}
		var secret []byte
		err = tx.QueryRow("SELECT secret FROM proxy_profiles WHERE id=?", profile.ID).Scan(&secret)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return old, err
		}
		if profile.ClearPassword {
			secret = nil
		}
		if profile.Password != "" {
			secret, err = m.Vault.Encrypt(profile.Password, "proxy:"+profile.ID)
			if err != nil {
				return old, ErrConfig
			}
		}
		if secret == nil {
			secret = []byte{}
		}
		if profile.Username == "" && len(secret) > 0 || profile.Username != "" && len(secret) == 0 {
			return old, ErrConfig
		}
		profile.PasswordSet = len(secret) > 0
		ids[profile.ID] = true
		secrets[profile.ID] = secret
		out.Profiles = append(out.Profiles, profile.Profile)
	}
	for _, policy := range []struct {
		value   Policy
		inherit bool
	}{{out.Global, false}, {out.Telegram, true}, {out.Agent, true}} {
		if err = validatePolicy(policy.value, ids, policy.inherit); err != nil {
			return old, err
		}
	}
	rows, err := tx.Query("SELECT proxy_id FROM account_network WHERE mode='proxy'")
	if err != nil {
		return old, err
	}
	referenced := false
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		if !ids[id] {
			referenced = true
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil || rowErr != nil {
		return old, ErrConfig
	}
	if referenced {
		return old, ErrReferenced
	}
	if _, err = tx.Exec("DELETE FROM proxy_profiles"); err != nil {
		return old, err
	}
	for _, profile := range out.Profiles {
		raw, _ := json.Marshal(profile)
		if _, err = tx.Exec("INSERT INTO proxy_profiles(id,payload,secret) VALUES(?,?,?)", profile.ID, string(raw), secrets[profile.ID]); err != nil {
			return old, err
		}
	}
	raw, _ := json.Marshal(out)
	if _, err = tx.Exec("INSERT INTO network_settings(id,version,payload) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET version=excluded.version,payload=excluded.payload", out.Version, string(raw)); err != nil {
		return old, err
	}
	nextAgent, err := m.resolve(tx, "agent", "")
	if err != nil {
		return old, err
	}
	if oldAgent.Key != nextAgent.Key {
		now := time.Now().Unix()
		for _, statement := range []string{
			"UPDATE agent_provider_settings SET version=version+1,enabled=0,capabilities='{}',updated_at=? WHERE id=1",
			"UPDATE agent_turns SET state='cancelled',error_code='network_changed',updated_at=? WHERE state IN ('queued','running')",
			"UPDATE agent_proposals SET state='invalidated',error_code='network_changed',updated_at=? WHERE state IN ('pending','accepted','applying')",
		} {
			if _, err = tx.Exec(statement, now); err != nil {
				return old, err
			}
		}
	}
	if err = store.Audit(tx, "network.configure", "outbound", "ok"); err != nil {
		return old, err
	}
	if err = tx.Commit(); err != nil {
		return old, err
	}
	m.CloseIdle()
	return out, nil
}

func SetAccount(tx *sql.Tx, id string, policy Policy) error {
	policy, err := NormalizePolicy(policy, true)
	if err != nil {
		return err
	}
	if policy.Mode == "proxy" {
		var exists int
		if tx.QueryRow("SELECT 1 FROM proxy_profiles WHERE id=?", policy.ProxyID).Scan(&exists) != nil {
			return ErrConfig
		}
	}
	_, err = tx.Exec("INSERT INTO account_network(account_id,mode,proxy_id) VALUES(?,?,?) ON CONFLICT(account_id) DO UPDATE SET mode=excluded.mode,proxy_id=excluded.proxy_id", id, policy.Mode, policy.ProxyID)
	return err
}

func (m *Manager) AccountPolicy(id string) (Policy, error) {
	policy := Policy{Mode: "inherit"}
	err := m.Store.DB.QueryRow("SELECT mode,proxy_id FROM account_network WHERE account_id=?", id).Scan(&policy.Mode, &policy.ProxyID)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, nil
	}
	return policy, err
}

type Route struct {
	Profile  *Profile `json:"profile,omitempty"`
	Key      string   `json:"revision"`
	Password string   `json:"-"`
}

func (m *Manager) Resolve(scope, accountID string) (Route, error) {
	tx, err := m.Store.DB.Begin()
	if err != nil {
		return Route{}, err
	}
	defer tx.Rollback()
	return m.resolve(tx, scope, accountID)
}

func (m *Manager) ResolveIn(tx *sql.Tx, scope, accountID string) (Route, error) {
	return m.resolve(tx, scope, accountID)
}

func (m *Manager) resolve(q querier, scope, accountID string) (Route, error) {
	settings, err := load(q)
	if err != nil {
		return Route{}, err
	}
	policy := settings.Global
	switch scope {
	case "global":
	case "telegram":
		policy = settings.Telegram
	case "agent":
		policy = settings.Agent
	case "account":
		policy = Policy{Mode: "inherit"}
		err = q.QueryRow("SELECT mode,proxy_id FROM account_network WHERE account_id=?", accountID).Scan(&policy.Mode, &policy.ProxyID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Route{}, err
		}
	default:
		return Route{}, ErrConfig
	}
	if policy.Mode == "inherit" {
		policy = settings.Global
	}
	return m.route(q, policy)
}

func (m *Manager) ProfileRoute(id string) (Route, error) {
	return m.route(m.Store.DB, Policy{Mode: "proxy", ProxyID: id})
}

func (m *Manager) route(q querier, policy Policy) (Route, error) {
	if policy.Mode == "direct" {
		return Route{Key: "direct"}, nil
	}
	if policy.Mode != "proxy" {
		return Route{}, ErrConfig
	}
	var raw string
	var secret []byte
	if err := q.QueryRow("SELECT payload,secret FROM proxy_profiles WHERE id=?", policy.ProxyID).Scan(&raw, &secret); err != nil {
		return Route{}, ErrConfig
	}
	var profile Profile
	if json.Unmarshal([]byte(raw), &profile) != nil {
		return Route{}, ErrConfig
	}
	password := ""
	if len(secret) > 0 {
		var err error
		password, err = m.Vault.Decrypt(secret, "proxy:"+profile.ID)
		if err != nil {
			return Route{}, ErrConfig
		}
	}
	identity, _ := json.Marshal([]any{profile.ID, profile.Protocol, profile.Host, profile.Port, profile.Username, profile.TrustedRemoteDNS})
	digest := sha256.Sum256(append(identity, secret...))
	return Route{Profile: &profile, Key: hex.EncodeToString(digest[:]), Password: password}, nil
}

func (r Route) Label() string {
	if r.Profile == nil {
		return "直连"
	}
	return r.Profile.Name + " · " + r.Profile.Protocol + "://" + net.JoinHostPort(r.Profile.Host, strconv.Itoa(r.Profile.Port))
}

func (m *Manager) CloseIdle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, transport := range m.http {
		transport.CloseIdleConnections()
		delete(m.http, key)
	}
}

func (m *Manager) Transport(route Route) *http.Transport {
	m.mu.Lock()
	defer m.mu.Unlock()
	if transport := m.http[route.Key]; transport != nil {
		return transport
	}
	if len(m.http) >= 32 {
		for key, transport := range m.http {
			transport.CloseIdleConnections()
			delete(m.http, key)
		}
	}
	transport := route.Transport()
	m.http[route.Key] = transport
	return transport
}
