package mail

import (
	"database/sql"
	"encoding/json"
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
	// hourly, daily, weekly, interval, cron or on_new_mail.
	Frequency string `json:"frequency"`
	Hour      int    `json:"hour"`
	Minute    int    `json:"minute"`
	Weekday   int    `json:"weekday"`
	Timezone  string `json:"timezone"`
	// Every is the interval in minutes for "interval" (5-1440).
	Every int `json:"every_minutes,omitempty"`
	// Cron is a five-field expression (minute hour day month weekday).
	Cron string `json:"cron,omitempty"`
}
type TaskRunInput struct {
	Manual  bool   `json:"manual,omitempty"`
	TaskID  string `json:"task_id"`
	Version int    `json:"version"`
	RunKey  string `json:"run_key"`
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
		if t.Enabled && len(t.NextTimes) > 0 {
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
