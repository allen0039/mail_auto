package mail

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

type RuleCondition struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}
type RuleGroup struct {
	Mode       string          `json:"mode"`
	Conditions []RuleCondition `json:"conditions"`
	Groups     []RuleGroup     `json:"groups,omitempty"`
}
type Rule struct {
	ID            string    `json:"id"`
	AccountID     string    `json:"account_id"`
	Name          string    `json:"name"`
	Version       int       `json:"version"`
	Priority      int       `json:"priority"`
	Enabled       bool      `json:"enabled"`
	SourceFolders []string  `json:"source_folders"`
	Match         RuleGroup `json:"match"`
	Action        string    `json:"action"`
	Also          []string  `json:"also,omitempty"`
	Destination   string    `json:"destination"`
	Instructions  string    `json:"instructions"`
	UpdatedAt     int64     `json:"updated_at"`
}
type RuleEvaluation struct {
	Message      Message  `json:"message"`
	Signature    string   `json:"signature"`
	RuleID       string   `json:"rule_id"`
	RuleVersion  int      `json:"rule_version"`
	Action       string   `json:"action"`
	Also         []string `json:"also,omitempty"`
	Destination  string   `json:"destination"`
	Instructions string   `json:"instructions,omitempty"`
	Status       string   `json:"status"`
	Reasons      []string `json:"reasons"`
}
type RulePreviewInput struct {
	AccountID string `json:"account_id"`
	RuleID    string `json:"rule_id,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}
type RulePreview struct {
	Items      []RuleEvaluation `json:"items"`
	Total      int              `json:"total"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

func digest(v any) string {
	raw, _ := json.Marshal(v)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func messageSignature(m Message) string {
	return digest([]any{m.Ref, m.AccountID, m.Folder, m.UIDValidity, m.UID, m.Subject, m.Sender, m.SentAt, m.ReceivedAt, m.Unread, m.Flagged, m.FlagsKnown, m.Size, m.ChangedAt})
}
func (s *Service) rulesForAccount(id string) ([]Rule, error) {
	rows, e := s.Store.DB.Query("SELECT payload FROM mail_rules WHERE account_id=? ORDER BY id LIMIT 201", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		var raw string
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		var r Rule
		if json.Unmarshal([]byte(raw), &r) != nil {
			return nil, ErrConflict
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out, rows.Err()
}
func ruleVisible(a Access, r Rule) bool {
	for _, f := range r.SourceFolders {
		if !a.Allows(r.AccountID, f, "read") {
			return false
		}
	}
	return r.Destination == "" || a.Allows(r.AccountID, r.Destination, "read")
}
func (s *Service) Rules(a Access, id string) ([]Rule, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return nil, e
	}
	if !a.Allows(id, "", "read") {
		return nil, ErrDenied
	}
	rules, e := s.rulesForAccount(id)
	if e != nil {
		return nil, e
	}
	out := []Rule{}
	for _, r := range rules {
		if ruleVisible(a, r) {
			out = append(out, r)
		}
	}
	return out, nil
}
func (s *Service) Rule(a Access, id string) (Rule, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return Rule{}, e
	}
	var raw string
	if e = s.Store.DB.QueryRow("SELECT payload FROM mail_rules WHERE id=?", id).Scan(&raw); e != nil {
		return Rule{}, e
	}
	var r Rule
	if json.Unmarshal([]byte(raw), &r) != nil {
		return r, ErrConflict
	}
	if !ruleVisible(a, r) || !a.Allows(r.AccountID, "", "read") {
		return Rule{}, ErrDenied
	}
	return r, nil
}
func (s *Service) RuleHistory(a Access, id string) ([]Rule, error) {
	if _, e := s.Rule(a, id); e != nil {
		return nil, e
	}
	rows, e := s.Store.DB.Query("SELECT payload FROM mail_rule_versions WHERE rule_id=? ORDER BY version DESC LIMIT 20", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		var raw string
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		var r Rule
		if json.Unmarshal([]byte(raw), &r) != nil {
			return nil, ErrConflict
		}
		if ruleVisible(a, r) {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}
func validateGroup(g RuleGroup, depth int, total *int) bool {
	if depth > 3 || !slices.Contains([]string{"all", "any", "not"}, g.Mode) || len(g.Conditions)+len(g.Groups) > 20 {
		return false
	}
	if g.Mode == "not" && len(g.Conditions)+len(g.Groups) != 1 {
		return false
	}
	for _, c := range g.Conditions {
		*total++
		if *total > 50 || !validCondition(c) {
			return false
		}
	}
	for _, child := range g.Groups {
		if !validateGroup(child, depth+1, total) {
			return false
		}
	}
	return true
}
func (s *Service) knownFolder(account Account, name string) bool {
	if name == "" || len(name) > 256 || strings.ContainsAny(name, "\x00\r\n") || (len(account.Folders) > 0 && !slices.Contains(account.Folders, name)) {
		return false
	}
	var n int
	return s.Store.DB.QueryRow("SELECT count(*) FROM mailboxes WHERE account_id=? AND name=?", account.ID, name).Scan(&n) == nil && n == 1
}
func (s *Service) SaveRule(a Access, r Rule) (Rule, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	a, e := s.CheckAccess(a)
	if e != nil {
		return Rule{}, e
	}
	if !a.Allows(r.AccountID, "", "rule_manage") {
		return Rule{}, ErrDenied
	}
	account, e := s.Account(r.AccountID)
	if e != nil {
		return Rule{}, e
	}
	r.Name = strings.TrimSpace(r.Name)
	total := 0
	if r.Name == "" || len(r.Name) > 120 || r.Priority < 0 || r.Priority > 10000 || len(r.Instructions) > 2000 || len(r.SourceFolders) < 1 || len(r.SourceFolders) > 20 || !validateGroup(r.Match, 0, &total) || !(slices.Contains([]string{"move", "archive", "trash", "keep", "ai"}, r.Action) || isFlagAction(r.Action)) || !validAlso(r.Action, r.Also) {
		return Rule{}, ErrInput
	}
	for _, f := range r.SourceFolders {
		if !s.knownFolder(account, f) || !a.Allows(account.ID, f, "rule_manage") || !a.Allows(account.ID, f, "read") {
			return Rule{}, ErrDenied
		}
	}
	if r.Action == "move" {
		if !s.knownFolder(account, r.Destination) || !a.Allows(account.ID, r.Destination, "read") || slices.Contains(r.SourceFolders, r.Destination) {
			return Rule{}, ErrInput
		}
	} else if r.Destination != "" {
		return Rule{}, ErrInput
	}
	oldVersion := r.Version
	if r.ID != "" {
		old, err := s.Rule(a, r.ID)
		if err != nil {
			return Rule{}, err
		}
		if old.AccountID != r.AccountID || old.Version != oldVersion {
			return Rule{}, ErrConflict
		}
	} else {
		if oldVersion != 0 {
			return Rule{}, ErrConflict
		}
		r.ID = credentials.Random(18)
	}
	all, e := s.rulesForAccount(r.AccountID)
	if e != nil {
		return Rule{}, e
	}
	if len(all) >= 200 && oldVersion == 0 {
		return Rule{}, ErrInput
	}
	graph := map[string][]string{}
	for _, x := range append(all, r) {
		if x.ID == r.ID && x.Version != r.Version {
			continue
		}
		if x.ID == r.ID {
			x = r
		}
		if x.Enabled && x.Action == "move" {
			for _, f := range x.SourceFolders {
				graph[f] = append(graph[f], x.Destination)
			}
		}
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var cycle func(string) bool
	cycle = func(f string) bool {
		if visiting[f] {
			return true
		}
		if visited[f] {
			return false
		}
		visiting[f] = true
		for _, next := range graph[f] {
			if cycle(next) {
				return true
			}
		}
		visiting[f] = false
		visited[f] = true
		return false
	}
	for f := range graph {
		if cycle(f) {
			return Rule{}, ErrConflict
		}
	}
	r.Version++
	r.UpdatedAt = time.Now().Unix()
	raw, _ := json.Marshal(r)
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return Rule{}, e
	}
	defer tx.Rollback()
	if oldVersion == 0 {
		_, e = tx.Exec("INSERT INTO mail_rules VALUES(?,?,?,?,?)", r.ID, r.AccountID, r.Version, string(raw), r.UpdatedAt)
	} else {
		var result sql.Result
		result, e = tx.Exec("UPDATE mail_rules SET version=?,payload=?,updated_at=? WHERE id=? AND version=?", r.Version, string(raw), r.UpdatedAt, r.ID, oldVersion)
		if e == nil {
			n, _ := result.RowsAffected()
			if n != 1 {
				return Rule{}, ErrConflict
			}
		}
	}
	if e != nil {
		return Rule{}, ErrConflict
	}
	if _, e = tx.Exec("INSERT INTO mail_rule_versions VALUES(?,?,?,?)", r.ID, r.Version, string(raw), r.UpdatedAt); e != nil {
		return Rule{}, e
	}
	if e = store.Audit(tx, "mail.rule.save", r.ID, "ok"); e != nil {
		return Rule{}, e
	}
	return r, tx.Commit()
}
func groupMatch(g RuleGroup, m Message, now int64) (int, []string) {
	values := []int{}
	reasons := []string{}
	for _, c := range g.Conditions {
		v, r := conditionMatch(c, m, now)
		values = append(values, v)
		reasons = append(reasons, r)
	}
	for _, child := range g.Groups {
		v, r := groupMatch(child, m, now)
		values = append(values, v)
		reasons = append(reasons, r...)
	}
	if g.Mode == "not" {
		if values[0] < 0 {
			return -1, reasons
		}
		return 1 - values[0], reasons
	}
	unknown := false
	for _, v := range values {
		if v < 0 {
			unknown = true
		}
		if g.Mode == "all" && v == 0 {
			return 0, reasons
		}
		if g.Mode == "any" && v == 1 {
			return 1, reasons
		}
	}
	if unknown {
		return -1, reasons
	}
	if g.Mode == "any" {
		return 0, reasons
	}
	return 1, reasons
}
func evaluateRules(m Message, rules []Rule, p AutomationPolicy, now int64) RuleEvaluation {
	out := RuleEvaluation{Message: m, Signature: messageSignature(m), Status: "unmatched", Action: "keep", Reasons: []string{"没有启用规则匹配"}}
	if why := policyProtection(p, m, now); why != "" {
		out.Status = "protected"
		out.Reasons = []string{why}
		return out
	}
	for _, r := range rules {
		if !r.Enabled || !slices.Contains(r.SourceFolders, m.Folder) {
			continue
		}
		match, reasons := groupMatch(r.Match, m, now)
		if match == 0 {
			continue
		}
		out.RuleID = r.ID
		out.RuleVersion = r.Version
		out.Reasons = reasons
		if match < 0 {
			out.Status = "unknown"
			return out
		}
		out.Action = r.Action
		out.Also = r.Also
		out.Destination = r.Destination
		out.Instructions = r.Instructions
		out.Status = "matched"
		if r.Action == "keep" {
			out.Status = "protected"
		}
		if isFlagAction(r.Action) {
			// A flag rule on a message already in the target state needs no write.
			change, _ := flagChangeFor(append([]string{r.Action}, r.Also...)...)
			if !m.FlagsKnown {
				out.Status = "unknown"
				out.Reasons = append(slices.Clone(reasons), "已读/星标状态尚未验证")
				return out
			}
			if change.Satisfied(m) {
				out.Status = "satisfied"
				out.Reasons = append(slices.Clone(reasons), "已是目标标记状态")
				return out
			}
		}
		if len(reasons) == 0 {
			out.Reasons = []string{"符合规则限定目录"}
		}
		return out
	}
	return out
}

type ruleCursor struct {
	Time  int64  `json:"time"`
	Ref   string `json:"ref"`
	Scope string `json:"scope"`
}

func (s *Service) ruleCandidates(a Access, accountID string, folders []string, cursor string, limit int) ([]Message, int, string, error) {
	if limit < 1 || limit > 100 || len(folders) == 0 || len(folders) > 100 {
		return nil, 0, "", ErrInput
	}
	scope := digest([]any{accountID, folders})
	where := "account_id=? AND folder IN (" + strings.TrimSuffix(strings.Repeat("?,", len(folders)), ",") + ")"
	args := []any{accountID}
	for _, f := range folders {
		if !a.Allows(accountID, f, "read") {
			return nil, 0, "", ErrDenied
		}
		args = append(args, f)
	}
	var total int
	if e := s.Store.DB.QueryRow("SELECT count(*) FROM messages WHERE "+where, args...).Scan(&total); e != nil {
		return nil, 0, "", e
	}
	if cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		var c ruleCursor
		if len(cursor) > 1024 || e != nil || json.Unmarshal(raw, &c) != nil || c.Scope != scope {
			return nil, 0, "", ErrInput
		}
		where += " AND (sent_at<? OR (sent_at=? AND ref>?))"
		args = append(args, c.Time, c.Time, c.Ref)
	}
	args = append(args, limit)
	rows, e := s.Store.DB.Query("SELECT "+messageColumns+" FROM messages WHERE "+where+" ORDER BY sent_at DESC,ref LIMIT ?", args...)
	if e != nil {
		return nil, 0, "", e
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, e := scanMessage(rows)
		if e != nil {
			return nil, 0, "", e
		}
		out = append(out, m)
	}
	if e = rows.Err(); e != nil {
		return nil, 0, "", e
	}
	next := ""
	if len(out) == limit {
		m := out[len(out)-1]
		raw, _ := json.Marshal(ruleCursor{Time: m.SentAt, Ref: m.Ref, Scope: scope})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, total, next, nil
}
func (s *Service) PreviewRules(a Access, in RulePreviewInput) (RulePreview, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return RulePreview{}, e
	}
	rules, e := s.Rules(a, in.AccountID)
	if e != nil {
		return RulePreview{}, e
	}
	if in.RuleID != "" {
		r, e := s.Rule(a, in.RuleID)
		if e != nil {
			return RulePreview{}, e
		}
		if r.AccountID != in.AccountID {
			return RulePreview{}, ErrDenied
		}
		r.Enabled = true
		rules = []Rule{r}
	}
	folders := []string{}
	for _, r := range rules {
		for _, f := range r.SourceFolders {
			if !slices.Contains(folders, f) {
				folders = append(folders, f)
			}
		}
	}
	if len(folders) == 0 {
		return RulePreview{Items: []RuleEvaluation{}}, nil
	}
	sort.Strings(folders)
	p, e := s.AutomationPolicy(a, in.AccountID)
	if e != nil {
		return RulePreview{}, e
	}
	p.Since = 0
	p.StableSeconds = 0
	limit := in.Limit
	if limit == 0 {
		limit = 25
	}
	messages, total, next, e := s.ruleCandidates(a, in.AccountID, folders, in.Cursor, limit)
	if e != nil {
		return RulePreview{}, e
	}
	out := RulePreview{Items: []RuleEvaluation{}, Total: total, NextCursor: next}
	for _, m := range messages {
		out.Items = append(out.Items, evaluateRules(m, rules, p, time.Now().Unix()))
	}
	return out, nil
}
