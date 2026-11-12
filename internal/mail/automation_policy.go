package mail

import (
	"database/sql"
	"encoding/json"
	"net/mail"
	"slices"
	"strings"
	"time"

	"mail-auto/internal/store"
)

type AutomationPolicy struct {
	AccountID        string   `json:"account_id"`
	Version          int      `json:"version"`
	AdminVersion     int      `json:"-"`
	Enabled          bool     `json:"enabled"`
	Mode             string   `json:"mode"`
	SourceFolders    []string `json:"source_folders"`
	Destinations     []string `json:"destinations"`
	Actions          []string `json:"actions"`
	AllowBody        bool     `json:"allow_body"`
	AllowInternal    bool     `json:"allow_internal"`
	ProtectUnread    bool     `json:"protect_unread"`
	ProtectFlagged   bool     `json:"protect_flagged"`
	ProtectedSenders []string `json:"protected_senders"`
	MaxBatch         int      `json:"max_batch"`
	DailyLimit       int      `json:"daily_limit,omitempty"`
	StableSeconds    int      `json:"stable_seconds"`
	IncludeHistory   bool     `json:"include_history"`
	Since            int64    `json:"since"`
	Expires          int64    `json:"expires"`
	taskDigest       string
}

// AdminVersion is stored in an envelope; ordinary responses never authorize a run.
type policyRecord struct {
	Policy       AutomationPolicy `json:"policy"`
	AdminVersion int              `json:"admin_version"`
}

func defaultPolicy(id string) AutomationPolicy {
	now := time.Now().Unix()
	return AutomationPolicy{AccountID: id, Mode: "observe", SourceFolders: []string{}, Destinations: []string{}, Actions: []string{}, ProtectedSenders: []string{}, ProtectUnread: true, ProtectFlagged: true, MaxBatch: 20, StableSeconds: 180, Since: now, Expires: now + 30*86400}
}
func (s *Service) loadAutomationPolicy(id string) (AutomationPolicy, error) {
	var raw string
	e := s.Store.DB.QueryRow("SELECT payload FROM automation_policies WHERE account_id=?", id).Scan(&raw)
	if e == sql.ErrNoRows {
		return defaultPolicy(id), nil
	}
	if e != nil {
		return AutomationPolicy{}, e
	}
	var record policyRecord
	if json.Unmarshal([]byte(raw), &record) != nil {
		return AutomationPolicy{}, ErrConflict
	}
	record.Policy.DailyLimit = 0
	record.Policy.AdminVersion = record.AdminVersion
	return record.Policy, nil
}
func (s *Service) AutomationPolicy(a Access, id string) (AutomationPolicy, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return AutomationPolicy{}, e
	}
	if !a.Allows(id, "", "read") {
		return AutomationPolicy{}, ErrDenied
	}
	if _, e = s.Account(id); e != nil {
		return AutomationPolicy{}, e
	}
	p, e := s.loadAutomationPolicy(id)
	if e != nil {
		return p, e
	}
	if a.Owner != "admin" {
		src, dest := []string{}, []string{}
		for _, f := range p.SourceFolders {
			if a.Allows(id, f, "read") {
				src = append(src, f)
			}
		}
		for _, f := range p.Destinations {
			if a.Allows(id, f, "read") {
				dest = append(dest, f)
			}
		}
		p.SourceFolders = src
		p.Destinations = dest
	}
	return p, nil
}
func (s *Service) SaveAutomationPolicy(a Access, p AutomationPolicy) (AutomationPolicy, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	a, e := s.CheckAccess(a)
	if e != nil {
		return AutomationPolicy{}, e
	}
	if a.Owner != "admin" {
		return AutomationPolicy{}, ErrDenied
	}
	account, e := s.Account(p.AccountID)
	if e != nil {
		return p, e
	}
	old, e := s.loadAutomationPolicy(p.AccountID)
	if e != nil {
		return p, e
	}
	if old.Version != p.Version {
		return p, ErrConflict
	}
	p.DailyLimit = 0
	now := time.Now().Unix()
	if !slices.Contains([]string{"observe", "execute"}, p.Mode) || len(p.SourceFolders) > 20 || len(p.Destinations) > 50 || len(p.Actions) > 3 || p.MaxBatch < 1 || p.MaxBatch > 50 || p.StableSeconds < 0 || p.StableSeconds > 3600 || len(p.ProtectedSenders) > 100 || p.Since < 0 || p.Since > now {
		return p, ErrInput
	}
	if p.Enabled && (len(p.SourceFolders) == 0 || p.Expires <= now || p.Expires > now+90*86400) {
		return p, ErrInput
	}
	for _, f := range append(slices.Clone(p.SourceFolders), p.Destinations...) {
		if !s.knownFolder(account, f) {
			return p, ErrInput
		}
	}
	settings, e := s.FolderSettings(account.ID)
	if e != nil {
		return p, e
	}
	for _, f := range p.SourceFolders {
		if f == settings.Archive || f == settings.Trash {
			return p, ErrInput
		}
	}
	for _, action := range p.Actions {
		if !slices.Contains([]string{"move", "archive", "trash"}, action) {
			return p, ErrInput
		}
		if action == "archive" && settings.Archive == "" {
			return p, ErrInput
		}
		if action == "trash" && settings.Trash == "" {
			return p, ErrInput
		}
	}
	if p.Enabled && p.Mode == "execute" && (!settings.WritesEnabled || len(p.Actions) == 0) {
		return p, ErrCapability
	}
	for i, v := range p.ProtectedSenders {
		v = strings.TrimSpace(v)
		addr, err := mail.ParseAddress(v)
		if err != nil || addr.Address != v {
			return p, ErrInput
		}
		p.ProtectedSenders[i] = strings.ToLower(v)
	}
	if !p.IncludeHistory {
		if old.Version == 0 {
			p.Since = now
		} else {
			p.Since = old.Since
			if old.IncludeHistory {
				p.Since = now
			}
		}
	}
	p.Version++
	p.AdminVersion = a.Version
	record := policyRecord{Policy: p, AdminVersion: a.Version}
	raw, _ := json.Marshal(record)
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return p, e
	}
	defer tx.Rollback()
	if old.Version == 0 {
		_, e = tx.Exec("INSERT INTO automation_policies VALUES(?,?,?)", p.AccountID, p.Version, string(raw))
	} else {
		var result sql.Result
		result, e = tx.Exec("UPDATE automation_policies SET version=?,payload=? WHERE account_id=? AND version=?", p.Version, string(raw), p.AccountID, old.Version)
		if e == nil {
			n, _ := result.RowsAffected()
			if n != 1 {
				return p, ErrConflict
			}
		}
	}
	if e != nil {
		return p, e
	}
	if e = store.Audit(tx, "mail.automation.policy", p.AccountID, "ok"); e != nil {
		return p, e
	}
	return p, tx.Commit()
}
func policyProtection(p AutomationPolicy, m Message, now int64) string {
	if p.Since > 0 {
		if m.ReceivedAt <= 0 {
			return "收信时间尚未同步"
		}
		if m.ReceivedAt < p.Since {
			return "早于允许的历史范围"
		}
	}
	if p.StableSeconds > 0 && (m.ChangedAt == 0 || now-m.ChangedAt < int64(p.StableSeconds)) {
		return "邮件状态仍在稳定窗口内"
	}
	if (p.ProtectUnread || p.ProtectFlagged) && !m.FlagsKnown {
		return "保护条件所需标记尚未同步"
	}
	if p.ProtectUnread && m.Unread {
		return "保留未读邮件"
	}
	if p.ProtectFlagged && m.Flagged {
		return "保留星标邮件"
	}
	if len(p.ProtectedSenders) > 0 {
		addresses, e := mail.ParseAddressList(m.Sender)
		if e != nil {
			return "无法验证保护联系人"
		}
		for _, addr := range addresses {
			if slices.Contains(p.ProtectedSenders, strings.ToLower(addr.Address)) {
				return "保留保护联系人的邮件"
			}
		}
	}
	return ""
}
func (s *Service) authorizedAutomation(a Access, id string) (Access, AutomationPolicy, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return a, AutomationPolicy{}, e
	}
	if !a.Allows(id, "", "automation") || !a.Allows(id, "", "read") {
		return a, AutomationPolicy{}, ErrDenied
	}
	p, e := s.loadAutomationPolicy(id)
	if e != nil {
		return a, p, e
	}
	if !p.Enabled || p.AdminVersion != a.Version || p.Expires <= time.Now().Unix() {
		return a, p, ErrDenied
	}
	account, e := s.Account(id)
	if e != nil {
		return a, p, e
	}
	for _, f := range p.SourceFolders {
		if !a.Allows(id, f, "read") || (len(account.Folders) > 0 && !slices.Contains(account.Folders, f)) {
			return a, p, ErrDenied
		}
	}
	a.automation = true
	return a, p, nil
}
func (s *Service) RuleCapabilities(a Access, id string) (map[string]any, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return nil, e
	}
	if !a.Allows(id, "", "read") {
		return nil, ErrDenied
	}
	account, e := s.Account(id)
	if e != nil {
		return nil, e
	}
	boxes, e := s.Mailboxes(id)
	if e != nil {
		return nil, e
	}
	visible := []Mailbox{}
	for _, b := range boxes {
		if a.Allows(id, b.Name, "read") {
			visible = append(visible, b)
		}
	}
	return map[string]any{"account_id": id, "provider": account.Provider, "folders": visible, "classification": "provider_folders", "native_labels": map[string]any{"read": false, "write": false, "status": "not_verified"}, "rule_fields": []string{"sender", "domain", "subject", "folder", "read_state", "received_days", "size", "flagged"}, "metadata_notice": "flags_known=false or received_at=0 means unknown; rules requiring missing data skip", "ai_trigger": "external_scheduled_task", "server_ai": false, "server_organizer_schedule": true, "internal_schedule_requires_admin_grant": true, "trigger_modes": []string{"manual", "internal", "external"}, "max_batch": 50}, nil
}
