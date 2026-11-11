package mail

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	_ "time/tzdata"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

// Task narrows an administrator policy. It never grants provider access.
type Task struct {
	LastState       string         `json:"last_state,omitempty"`
	ID              string         `json:"id"`
	AccountID       string         `json:"account_id"`
	Name            string         `json:"name"`
	Version         int            `json:"version"`
	Enabled         bool           `json:"enabled"`
	Trigger         string         `json:"trigger_mode"`
	Engine          string         `json:"engine"`
	Mode            string         `json:"mode"`
	SourceFolders   []string       `json:"source_folders"`
	RuleIDs         []string       `json:"rule_ids,omitempty"`
	RuleVersions    map[string]int `json:"rule_versions,omitempty"`
	MaxBatch        int            `json:"max_batch"`
	Since           int64          `json:"since"`
	ProtectUnread   bool           `json:"protect_unread"`
	ProtectFlagged  bool           `json:"protect_flagged"`
	Schedule        TaskSchedule   `json:"schedule"`
	Notify          string         `json:"notify"`
	ExpectedSeconds int            `json:"expected_seconds"`
	NextRunAt       int64          `json:"next_run_at"`
	NextTimes       []int64        `json:"next_times,omitempty"`
	LastTriggered   int64          `json:"last_triggered"`
	Cursor          string         `json:"cursor,omitempty"`
	UpdatedAt       int64          `json:"updated_at"`
	AdminVersion    int            `json:"-"`
	ServiceOwner    string         `json:"-"`
}
type taskRecord struct {
	Task         Task   `json:"task"`
	AdminVersion int    `json:"admin_version"`
	ServiceOwner string `json:"service_owner"`
}
type TaskSchedule struct {
	Frequency string `json:"frequency"`
	Hour      int    `json:"hour"`
	Minute    int    `json:"minute"`
	Weekday   int    `json:"weekday"`
	Timezone  string `json:"timezone"`
}
type TaskRun struct {
	Finalized     bool   `json:"finalized"`
	ID            string `json:"id"`
	TaskID        string `json:"task_id"`
	AccountID     string `json:"account_id"`
	TaskName      string `json:"task_name"`
	TaskVersion   int    `json:"task_version"`
	PolicyVersion int    `json:"policy_version"`
	RulesDigest   string `json:"rules_digest"`
	Owner         string `json:"-"`
	AdminVersion  int    `json:"-"`
	Source        string `json:"source"`
	RunKey        string `json:"run_key"`
	Mode          string `json:"mode"`
	State         string `json:"state"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
	Expires       int64  `json:"expires"`
	ScheduledAt   int64  `json:"scheduled_at"`
	BatchID       string `json:"batch_id,omitempty"`
	Scanned       int    `json:"scanned"`
	Claimed       int    `json:"claimed"`
	Kept          int    `json:"kept"`
	NeedsAI       int    `json:"needs_ai"`
	Completed     int    `json:"completed"`
	Failed        int    `json:"failed"`
	Unknown       int    `json:"unknown"`
	Skipped       int    `json:"skipped"`
	Protected     int    `json:"protected"`
	HasMore       bool   `json:"has_more"`
	NextCursor    string `json:"next_cursor,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	NotifyState   string `json:"notification_state"`
	NotifyError   string `json:"notification_error,omitempty"`
}
type TaskRunInput struct {
	Manual  bool   `json:"manual,omitempty"`
	TaskID  string `json:"task_id"`
	Version int    `json:"version"`
	RunKey  string `json:"run_key"`
}

func nextSchedule(c TaskSchedule, after time.Time) (int64, error) {
	if !slices.Contains([]string{"hourly", "daily", "weekly"}, c.Frequency) || c.Hour < 0 || c.Hour > 23 || c.Minute < 0 || c.Minute > 59 || c.Weekday < 0 || c.Weekday > 6 {
		return 0, ErrInput
	}
	loc, e := time.LoadLocation(c.Timezone)
	if e != nil {
		return 0, ErrInput
	}
	// Iterate real minutes, but skip the second occurrence of an ambiguous local time.
	// This also skips nonexistent local times at spring DST changes.
	start := after.UTC().Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 9*24*60; i++ {
		v := start.Add(time.Duration(i) * time.Minute)
		l := v.In(loc)
		if l.Minute() != c.Minute || (c.Frequency != "hourly" && l.Hour() != c.Hour) || (c.Frequency == "weekly" && int(l.Weekday()) != c.Weekday) {
			continue
		}
		{ // An ambiguous wall-clock occurrence runs only once, including hourly schedules.
			duplicate := false
			for j := 1; j <= 180; j++ {
				old := v.Add(-time.Duration(j) * time.Minute).In(loc)
				if old.Year() == l.Year() && old.YearDay() == l.YearDay() && old.Hour() == l.Hour() && old.Minute() == l.Minute() {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
		}
		return v.Unix(), nil
	}
	return 0, ErrInput
}
func scheduleTimes(c TaskSchedule) ([]int64, error) {
	out := []int64{}
	at := time.Now()
	for i := 0; i < 5; i++ {
		n, e := nextSchedule(c, at)
		if e != nil {
			return nil, e
		}
		out = append(out, n)
		at = time.Unix(n, 0)
	}
	return out, nil
}
func taskJSON(t Task) string {
	b, _ := json.Marshal(taskRecord{t, t.AdminVersion, t.ServiceOwner})
	return string(b)
}
func (s *Service) loadTask(id string) (Task, error) {
	var raw string
	var next, last int64
	e := s.Store.DB.QueryRow("SELECT payload,next_run_at,updated_at FROM automation_tasks WHERE id=?", id).Scan(&raw, &next, &last)
	if e != nil {
		return Task{}, e
	}
	var rec taskRecord
	if json.Unmarshal([]byte(raw), &rec) != nil {
		return Task{}, ErrConflict
	}
	t := rec.Task
	t.AdminVersion = rec.AdminVersion
	t.ServiceOwner = rec.ServiceOwner
	t.NextRunAt = next
	t.UpdatedAt = last
	return t, nil
}
func taskVisible(a Access, t Task) bool {
	if !a.Allows(t.AccountID, "", "read") {
		return false
	}
	for _, f := range t.SourceFolders {
		if !a.Allows(t.AccountID, f, "read") {
			return false
		}
	}
	return true
}

func taskRuleOverlap(left, right []string) bool {
	for _, a := range left {
		if slices.Contains(right, a) {
			return true
		}
	}
	return false
}

// resolveTaskRules validates an explicit task rule binding and returns the
// current rule snapshot. An empty RuleIDs list intentionally keeps the legacy
// meaning: all account rules are considered and enabled rules are evaluated.
// Explicit bindings include every enabled rule up to the selected priority in
// an overlapping source folder. This prevents skipping a higher-priority rule
// to reach a lower-priority action or protection rule.
func (s *Service) resolveTaskRules(a Access, t Task, saving bool) ([]Rule, []string, map[string]int, error) {
	all, e := s.rulesForAccount(t.AccountID)
	if e != nil {
		return nil, nil, nil, e
	}
	if len(t.RuleIDs) == 0 {
		if len(t.RuleVersions) > 0 {
			if saving {
				return nil, nil, nil, ErrInput
			}
			return nil, nil, nil, ErrConflict
		}
		return all, nil, nil, nil
	}
	if len(t.RuleIDs) > 200 || !a.Allows(t.AccountID, "", "read") {
		return nil, nil, nil, ErrDenied
	}
	byID := make(map[string]Rule, len(all))
	for _, r := range all {
		byID[r.ID] = r
	}
	selected := make([]Rule, 0, len(t.RuleIDs))
	seen := make(map[string]bool, len(t.RuleIDs))
	maxPriority := -1
	for _, id := range t.RuleIDs {
		if id == "" || seen[id] {
			return nil, nil, nil, ErrInput
		}
		seen[id] = true
		r, ok := byID[id]
		if !ok || r.AccountID != t.AccountID {
			return nil, nil, nil, ErrDenied
		}
		if !ruleVisible(a, r) {
			return nil, nil, nil, ErrDenied
		}
		if !taskRuleOverlap(t.SourceFolders, r.SourceFolders) || !r.Enabled {
			if saving {
				return nil, nil, nil, ErrInput
			}
			return nil, nil, nil, ErrConflict
		}
		if version, ok := t.RuleVersions[id]; !ok || version == 0 || version != r.Version {
			if saving && ok && version != 0 {
				return nil, nil, nil, ErrConflict
			}
			if !saving {
				return nil, nil, nil, ErrConflict
			}
		}
		selected = append(selected, r)
		if r.Priority > maxPriority {
			maxPriority = r.Priority
		}
	}
	for id := range t.RuleVersions {
		if !seen[id] {
			return nil, nil, nil, ErrInput
		}
	}
	for _, r := range all {
		if !r.Enabled || r.Priority > maxPriority || !taskRuleOverlap(t.SourceFolders, r.SourceFolders) {
			continue
		}
		if !seen[r.ID] {
			if saving {
				return nil, nil, nil, ErrInput
			}
			return nil, nil, nil, ErrConflict
		}
	}
	slices.SortFunc(selected, func(x, y Rule) int {
		if x.Priority != y.Priority {
			return x.Priority - y.Priority
		}
		return strings.Compare(x.ID, y.ID)
	})
	ids := make([]string, 0, len(selected))
	versions := make(map[string]int, len(selected))
	for _, r := range selected {
		ids = append(ids, r.ID)
		versions[r.ID] = r.Version
	}
	return selected, ids, versions, nil
}

func (s *Service) taskRules(a Access, t Task) ([]Rule, error) {
	rules, _, _, e := s.resolveTaskRules(a, t, false)
	return rules, e
}

func (s *Service) Task(a Access, id string) (Task, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return Task{}, e
	}
	t, e := s.loadTask(id)
	if e != nil {
		return t, e
	}
	if !taskVisible(a, t) {
		return Task{}, ErrDenied
	}
	if t.Trigger == "internal" {
		t.NextTimes, _ = scheduleTimes(t.Schedule)
	}
	t.LastState = ""
	_ = s.Store.DB.QueryRow("SELECT state FROM task_runs WHERE task_id=? AND (owner=? OR ?='admin') ORDER BY created_at DESC,id DESC LIMIT 1", t.ID, a.Owner, a.Owner).Scan(&t.LastState)
	return t, nil
}
func (s *Service) Tasks(a Access, account string) ([]Task, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return nil, e
	}
	if !a.Allows(account, "", "read") {
		return nil, ErrDenied
	}
	rows, e := s.Store.DB.Query("SELECT id FROM automation_tasks WHERE account_id=? ORDER BY updated_at DESC,id LIMIT 100", account)
	if e != nil {
		return nil, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			break
		}
		ids = append(ids, id)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return nil, e
	}
	out := []Task{}
	for _, id := range ids {
		t, e := s.loadTask(id)
		if e != nil {
			return nil, e
		}
		if taskVisible(a, t) {
			t.LastState = ""
			_ = s.Store.DB.QueryRow("SELECT state FROM task_runs WHERE task_id=? AND (owner=? OR ?='admin') ORDER BY created_at DESC,id DESC LIMIT 1", t.ID, a.Owner, a.Owner).Scan(&t.LastState)
			out = append(out, t)
		}
	}
	return out, nil
}
func (s *Service) SaveTask(a Access, t Task) (Task, error) {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	a, e := s.CheckAccess(a)
	if e != nil {
		return t, e
	}
	if !a.Allows(t.AccountID, "", "task_manage") || !taskVisible(a, t) {
		return t, ErrDenied
	}
	p, e := s.loadAutomationPolicy(t.AccountID)
	if e != nil {
		return t, e
	}
	if t.Trigger == "" {
		t.Trigger = "manual"
	}
	if t.Engine == "" {
		t.Engine = "rules"
	}
	if t.Mode == "" {
		t.Mode = "observe"
	}
	if t.ID == "" {
		t.Enabled = false
		t.Mode = "observe"
	}
	if t.Notify == "" {
		t.Notify = "inherit"
	}
	if t.MaxBatch == 0 {
		t.MaxBatch = min(20, p.MaxBatch)
	}
	if t.Schedule.Timezone == "" {
		t.Schedule.Timezone = "Asia/Shanghai"
	}
	t.Name = strings.TrimSpace(t.Name)
	if len(t.Name) < 1 || len(t.Name) > 120 || strings.ContainsAny(t.Name, "\x00\r\n") || !slices.Contains([]string{"manual", "internal", "external"}, t.Trigger) || !slices.Contains([]string{"rules", "ai"}, t.Engine) || !slices.Contains([]string{"observe", "execute"}, t.Mode) || !validNotificationPolicy(t.Notify, true) || t.MaxBatch < 1 || t.MaxBatch > p.MaxBatch || t.Since < 0 || t.Since > time.Now().Unix() || len(t.SourceFolders) == 0 || len(t.SourceFolders) > 20 || t.ExpectedSeconds < 0 || t.ExpectedSeconds > 604800 || (t.ExpectedSeconds > 0 && t.ExpectedSeconds < 3600) {
		return t, ErrInput
	}
	for _, f := range t.SourceFolders {
		if !slices.Contains(p.SourceFolders, f) {
			return t, ErrDenied
		}
	}
	if t.Engine == "ai" && t.Trigger == "internal" {
		return t, ErrInput
	}
	if t.Trigger == "internal" && !p.AllowInternal {
		return t, ErrDenied
	}
	if t.Mode == "execute" && p.Mode != "execute" {
		return t, ErrDenied
	}
	if t.Enabled && (!p.Enabled || p.Expires <= time.Now().Unix() || p.AdminVersion != a.Version) {
		return t, ErrDenied
	}
	old := Task{}
	if t.ID != "" {
		old, e = s.loadTask(t.ID)
		if e != nil {
			return t, e
		}
		if old.AccountID != t.AccountID || old.Version != t.Version {
			return t, ErrConflict
		}
	} else if t.Version != 0 {
		return t, ErrInput
	}
	_, normalizedIDs, normalizedVersions, e := s.resolveTaskRules(a, t, true)
	if e != nil {
		return t, e
	}
	t.RuleIDs = normalizedIDs
	t.RuleVersions = normalizedVersions
	if t.ID == "" {
		t.ID = credentials.Random(18)
		t.Enabled = false
		t.Mode = "observe"
	}
	t.Version++
	t.AdminVersion = a.Version
	t.ServiceOwner = "svc:" + credentials.Random(24)
	t.UpdatedAt = time.Now().Unix()
	t.NextRunAt = 0
	t.NextTimes = nil
	t.LastState = ""
	t.Cursor = ""
	t.LastTriggered = old.LastTriggered
	if t.Trigger == "internal" {
		t.NextTimes, e = scheduleTimes(t.Schedule)
		if e != nil {
			return t, e
		}
		if t.Enabled {
			t.NextRunAt = t.NextTimes[0]
		}
	}
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return t, e
	}
	defer tx.Rollback()
	if old.Version == 0 {
		var n int
		if e = tx.QueryRow("SELECT count(*) FROM automation_tasks WHERE account_id=?", t.AccountID).Scan(&n); e != nil {
			return t, e
		}
		if n >= 50 {
			return t, ErrInput
		}
		_, e = tx.Exec("INSERT INTO automation_tasks VALUES(?,?,?,?,?,?,?,?,?)", t.ID, t.AccountID, t.Version, t.ServiceOwner, taskJSON(t), t.Enabled, t.Trigger, t.NextRunAt, t.UpdatedAt)
	} else {
		var r sql.Result
		r, e = tx.Exec("UPDATE automation_tasks SET version=?,service_owner=?,payload=?,enabled=?,trigger_mode=?,next_run_at=?,updated_at=? WHERE id=? AND version=?", t.Version, t.ServiceOwner, taskJSON(t), t.Enabled, t.Trigger, t.NextRunAt, t.UpdatedAt, t.ID, old.Version)
		if e == nil {
			n, _ := r.RowsAffected()
			if n != 1 {
				return t, ErrConflict
			}
		}
	}
	if e != nil {
		return t, e
	}
	if e = store.Audit(tx, "mail.task.save", t.ID, "ok"); e != nil {
		return t, e
	}
	if e = tx.Commit(); e != nil {
		return t, e
	}
	// Version changes fence every not-yet-started operation. The monitor records the interruption.
	return t, nil
}
func (s *Service) taskServiceAccess(a Access) (Access, error) {
	var id string
	if s.Store.DB.QueryRow("SELECT id FROM automation_tasks WHERE service_owner=?", a.Owner).Scan(&id) != nil {
		return Access{}, ErrDenied
	}
	t, e := s.loadTask(id)
	if e != nil || t.AdminVersion != a.Version {
		return Access{}, ErrDenied
	}
	p, e := s.loadAutomationPolicy(t.AccountID)
	if e != nil || !p.Enabled || p.AdminVersion != a.Version || p.Expires <= time.Now().Unix() {
		return Access{}, ErrDenied
	}
	// Full policy visibility preserves high-priority account rules; task narrowing is applied separately.
	folders := append(slices.Clone(p.SourceFolders), p.Destinations...)
	fs, e := s.FolderSettings(t.AccountID)
	if e != nil {
		return Access{}, e
	}
	if fs.Archive != "" {
		folders = append(folders, fs.Archive)
	}
	if fs.Trash != "" {
		folders = append(folders, fs.Trash)
	}
	return Access{Owner: a.Owner, Version: a.Version, Accounts: []string{t.AccountID}, Folders: map[string][]string{t.AccountID: folders}, Actions: []string{"read", "automation"}, automation: a.automation}, nil
}

// Pausing is always possible after grant revocation; enabling revalidates all settings.
func (s *Service) SetTaskEnabled(a Access, id string, version int, enabled bool) (Task, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return Task{}, e
	}
	t, e := s.Task(a, id)
	if e != nil {
		return t, e
	}
	if !a.Allows(t.AccountID, "", "task_manage") {
		return t, ErrDenied
	}
	if t.Version != version {
		return t, ErrConflict
	}
	if enabled {
		t.Enabled = true
		return s.SaveTask(a, t)
	}
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	t, e = s.loadTask(id)
	if e != nil {
		return t, e
	}
	if t.Version != version {
		return t, ErrConflict
	}
	t.Enabled = false
	t.Version++
	t.NextRunAt = 0
	t.NextTimes = nil
	t.Cursor = ""
	t.AdminVersion = a.Version
	t.ServiceOwner = "svc:" + credentials.Random(24)
	t.UpdatedAt = time.Now().Unix()
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return t, e
	}
	defer tx.Rollback()
	res, e := tx.Exec("UPDATE automation_tasks SET version=?,service_owner=?,payload=?,enabled=0,next_run_at=0,updated_at=? WHERE id=? AND version=?", t.Version, t.ServiceOwner, taskJSON(t), t.UpdatedAt, t.ID, version)
	if e != nil {
		return t, e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return t, ErrConflict
	}
	if e = store.Audit(tx, "mail.task.pause", t.ID, "ok"); e != nil {
		return t, e
	}
	return t, tx.Commit()
}
func (s *Service) loadTaskRun(id string) (TaskRun, error) {
	var raw, owner, state string
	var updated int64
	e := s.Store.DB.QueryRow("SELECT payload,owner,state,updated_at FROM task_runs WHERE id=?", id).Scan(&raw, &owner, &state, &updated)
	if e != nil {
		return TaskRun{}, e
	}
	var r TaskRun
	if json.Unmarshal([]byte(raw), &r) != nil {
		return r, ErrConflict
	}
	r.Owner = owner
	r.State = state
	r.UpdatedAt = updated
	var private struct {
		AdminVersion int `json:"admin_version"`
	}
	if json.Unmarshal([]byte(raw), &private) != nil {
		return r, ErrConflict
	}
	r.AdminVersion = private.AdminVersion
	_ = s.Store.DB.QueryRow("SELECT batch_id FROM task_run_batches WHERE run_id=?", id).Scan(&r.BatchID)
	r.NotifyState = "not_required"
	_ = s.Store.DB.QueryRow("SELECT state,error_code FROM notification_outbox WHERE run_id=? ORDER BY created_at DESC,id DESC LIMIT 1", id).Scan(&r.NotifyState, &r.NotifyError)
	return r, nil
}
func runJSON(r TaskRun) string {
	// owner/admin version are private API fields, but must survive service restarts.
	b, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["admin_version"] = r.AdminVersion
	b, _ = json.Marshal(m)
	return string(b)
}
func (s *Service) TaskRun(a Access, id string) (TaskRun, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return TaskRun{}, e
	}
	r, e := s.loadTaskRun(id)
	if e != nil {
		return r, e
	}
	t, e := s.Task(a, r.TaskID)
	if e != nil {
		return TaskRun{}, e
	}
	_ = t
	if a.Owner != "admin" && a.Owner != r.Owner {
		return TaskRun{}, ErrDenied
	}
	return r, nil
}
func (s *Service) TaskRuns(a Access, taskID string) ([]TaskRun, error) {
	if _, e := s.Task(a, taskID); e != nil {
		return nil, e
	}
	rows, e := s.Store.DB.Query("SELECT id FROM task_runs WHERE task_id=? AND (owner=? OR ?='admin') ORDER BY created_at DESC,id DESC LIMIT 30", taskID, a.Owner, a.Owner)
	if e != nil {
		return nil, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			break
		}
		ids = append(ids, id)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return nil, e
	}
	out := []TaskRun{}
	for _, id := range ids {
		r, e := s.loadTaskRun(id)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, nil
}
func (s *Service) RunTask(a Access, in TaskRunInput, source string) (TaskRun, error) {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	a, e := s.CheckAccess(a)
	if e != nil {
		return TaskRun{}, e
	}
	t, e := s.Task(a, in.TaskID)
	if e != nil {
		return TaskRun{}, e
	}
	if !a.Allows(t.AccountID, "", "automation") || !slices.Contains([]string{"manual", "external"}, source) {
		return TaskRun{}, ErrDenied
	}
	if in.RunKey == "" || len(in.RunKey) > 128 || strings.ContainsAny(in.RunKey, "\x00\r\n") || t.Version != in.Version {
		return TaskRun{}, ErrInput
	}
	var previous string
	if s.Store.DB.QueryRow("SELECT id FROM task_runs WHERE task_id=? AND run_key=?", t.ID, in.RunKey).Scan(&previous) == nil {
		return s.TaskRun(a, previous)
	}
	if source == "external" && (t.Trigger != "external" || !t.Enabled) {
		return TaskRun{}, ErrDenied
	}
	if t.Engine == "ai" && a.Owner == "admin" {
		return TaskRun{}, ErrCapability
	}
	if _, _, e = s.authorizedAutomation(a, t.AccountID); e != nil {
		return TaskRun{}, e
	}
	return s.createTaskRun(a, t, in.RunKey, source, 0)
}
func (s *Service) createTaskRun(a Access, t Task, key, source string, scheduled int64) (TaskRun, error) {
	now := time.Now().Unix()
	owner := a.Owner
	if owner == "admin" || source == "internal" {
		owner = t.ServiceOwner
	}
	p, e := s.loadAutomationPolicy(t.AccountID)
	if e != nil {
		return TaskRun{}, e
	}
	ruleAccess := a
	if strings.HasPrefix(a.Owner, "svc:") {
		ruleAccess, e = s.CheckAccess(a)
		if e != nil {
			return TaskRun{}, e
		}
	}
	rules, e := s.taskRules(ruleAccess, t)
	if e != nil {
		return TaskRun{}, e
	}
	r := TaskRun{ID: credentials.Random(24), TaskID: t.ID, AccountID: t.AccountID, TaskName: t.Name, TaskVersion: t.Version, PolicyVersion: p.Version, RulesDigest: rulesDigest(rules), Owner: owner, AdminVersion: t.AdminVersion, Source: source, RunKey: key, Mode: t.Mode, State: "queued", CreatedAt: now, UpdatedAt: now, Expires: now + taskRunTTL(t), ScheduledAt: scheduled, NextCursor: t.Cursor}
	if t.Engine == "ai" {
		r.State = "awaiting_ai"
	}
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return r, e
	}
	defer tx.Rollback()
	var version int
	if e = tx.QueryRow("SELECT version FROM automation_tasks WHERE id=?", t.ID).Scan(&version); e != nil {
		return r, e
	}
	if version != t.Version {
		return r, ErrConflict
	}
	_, e = tx.Exec("INSERT INTO task_runs(id,task_id,account_id,owner,run_key,task_version,source,state,payload,created_at,updated_at,expires) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", r.ID, t.ID, t.AccountID, owner, key, t.Version, source, r.State, runJSON(r), now, now, r.Expires)
	if e != nil {
		tx.Rollback()
		var existing string
		if s.Store.DB.QueryRow("SELECT id FROM task_runs WHERE task_id=? AND run_key=?", t.ID, key).Scan(&existing) == nil {
			return s.TaskRun(a, existing)
		}
		return r, ErrBusy
	}
	t.LastTriggered = now
	if source == "internal" {
		t.NextRunAt, e = nextSchedule(t.Schedule, time.Now())
		if e != nil {
			return r, e
		}
	}
	if _, e = tx.Exec("UPDATE automation_tasks SET payload=?,next_run_at=? WHERE id=? AND version=?", taskJSON(t), t.NextRunAt, t.ID, t.Version); e != nil {
		return r, e
	}
	if e = store.Audit(tx, "mail.task.run", r.ID, source); e != nil {
		return r, e
	}
	return r, tx.Commit()
}

func taskRunTTL(t Task) int64 {
	if t.Engine == "rules" {
		// A 20-message batch performs multiple provider round trips per item.
		// A short task deadline turns ordinary IMAP jitter into task_timeout even
		// though every individual write remains safely bounded and verified.
		return 1800
	}
	return 600
}
func (s *Service) taskBatchPolicy(a Access, p AutomationPolicy, runID string) (AutomationPolicy, error) {
	r, e := s.loadTaskRun(runID)
	if e != nil {
		return p, e
	}
	t, e := s.loadTask(r.TaskID)
	if e != nil {
		return p, e
	}
	if r.Owner != a.Owner || r.AdminVersion != a.Version || t.Version != r.TaskVersion || t.AdminVersion != a.Version || r.Expires <= time.Now().Unix() || !slices.Contains([]string{"queued", "running", "awaiting_ai"}, r.State) || p.Version != r.PolicyVersion {
		return p, ErrConflict
	}
	if r.Source == "internal" && (!p.AllowInternal || !t.Enabled || t.Trigger != "internal") {
		return p, ErrDenied
	}
	if r.Source == "external" && (!t.Enabled || t.Trigger != "external") {
		return p, ErrDenied
	}
	if t.Engine == "rules" {
		var until int64
		if e = s.Store.DB.QueryRow("SELECT lease_until FROM task_runs WHERE id=?", r.ID).Scan(&until); e != nil {
			return p, e
		}
		if until <= time.Now().Unix() {
			return p, ErrConflict
		}
	}
	for _, f := range t.SourceFolders {
		if !slices.Contains(p.SourceFolders, f) {
			return p, ErrDenied
		}
	}
	if t.Mode == "execute" && p.Mode != "execute" {
		return p, ErrDenied
	}
	if _, e = s.taskRules(a, t); e != nil {
		return p, e
	}
	p.Mode = t.Mode
	p.SourceFolders = slices.Clone(t.SourceFolders)
	p.MaxBatch = min(p.MaxBatch, t.MaxBatch)
	p.Since = max(p.Since, t.Since)
	p.ProtectUnread = p.ProtectUnread || t.ProtectUnread
	p.ProtectFlagged = p.ProtectFlagged || t.ProtectFlagged
	p.taskDigest = digest([]any{t.ID, t.Version, t.RuleIDs, t.RuleVersions})
	return p, nil
}
func (s *Service) PreviewTask(a Access, id, cursor string) (RulePreview, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return RulePreview{}, e
	}
	t, e := s.Task(a, id)
	if e != nil {
		return RulePreview{}, e
	}
	p, e := s.loadAutomationPolicy(t.AccountID)
	if e != nil {
		return RulePreview{}, e
	}
	if a.Owner != "admin" && !a.Allows(t.AccountID, "", "automation") {
		return RulePreview{}, ErrDenied
	}
	rules, e := s.taskRules(a, t)
	if e != nil {
		return RulePreview{}, e
	}
	for _, r := range rules {
		if r.Enabled && !ruleVisible(a, r) {
			return RulePreview{}, ErrDenied
		}
	}
	p.SourceFolders = t.SourceFolders
	p.Since = max(p.Since, t.Since)
	p.ProtectUnread = p.ProtectUnread || t.ProtectUnread
	p.ProtectFlagged = p.ProtectFlagged || t.ProtectFlagged
	ms, total, next, e := s.ruleCandidates(a, t.AccountID, t.SourceFolders, cursor, 25)
	if e != nil {
		return RulePreview{}, e
	}
	out := RulePreview{Items: []RuleEvaluation{}, Total: total, NextCursor: next}
	for _, m := range ms {
		out.Items = append(out.Items, evaluateRules(m, rules, p, time.Now().Unix()))
	}
	return out, nil
}
func (s *Service) CancelTaskRun(a Access, id string) error {
	r, e := s.TaskRun(a, id)
	if e != nil {
		return e
	}
	if r.BatchID != "" {
		if e = s.CancelAIBatch(a, r.BatchID); e != nil {
			return e
		}
	}
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	r, e = s.loadTaskRun(id)
	if e != nil {
		return e
	}
	if !activeTaskState(r.State) {
		return nil
	}
	r.State = "cancelled"
	r.ErrorCode = ""
	r.Finalized = false
	return s.saveTaskRun(r)
}
func activeTaskState(state string) bool {
	return slices.Contains([]string{"queued", "running", "awaiting_ai"}, state)
}

// Keep errors machine-readable: provider exceptions must never reach notifications.
func taskError(e error) string {
	if errors.Is(e, context.DeadlineExceeded) {
		return "task_timeout"
	}
	return errorCode(e)
}
func taskRunKey(t Task, at int64) string { return fmt.Sprintf("schedule:%d:%d", t.Version, at) }
