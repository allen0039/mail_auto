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

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

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
	// Queued and Running are writes not yet attempted or still in flight;
	// ProgressAt is the latest state change of any of the run's operations.
	// FlagWarnings counts completed moves whose follow-up flag change was
	// not applied (error_code flags_not_applied); they are not failures.
	FlagWarnings int    `json:"flag_warnings"`
	Queued       int    `json:"queued"`
	Running      int    `json:"running"`
	ProgressAt   int64  `json:"progress_at,omitempty"`
	Skipped      int    `json:"skipped"`
	Protected    int    `json:"protected"`
	HasMore      bool   `json:"has_more"`
	NextCursor   string `json:"next_cursor,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	NotifyState  string `json:"notification_state"`
	NotifyError  string `json:"notification_error,omitempty"`
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
	return s.RunTaskGuarded(a, in, source, nil)
}

// RunTaskGuarded is RunTask with guard checked inside the run-creation
// transaction. A retry with an existing run key returns that run.
func (s *Service) RunTaskGuarded(a Access, in TaskRunInput, source string, guard func(*sql.Tx) error) (TaskRun, error) {
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
	if t.Engine == "ai" && (a.Owner == "admin" || strings.HasPrefix(a.Owner, AgentOwnerPrefix)) {
		// The built-in agent never starts a run that waits for external AI.
		return TaskRun{}, ErrCapability
	}
	if _, _, e = s.authorizedAutomation(a, t.AccountID); e != nil {
		return TaskRun{}, e
	}
	return s.createTaskRunGuarded(a, t, in.RunKey, source, 0, guard)
}
func (s *Service) createTaskRun(a Access, t Task, key, source string, scheduled int64) (TaskRun, error) {
	return s.createTaskRunGuarded(a, t, key, source, scheduled, nil)
}
func (s *Service) createTaskRunGuarded(a Access, t Task, key, source string, scheduled int64, guard func(*sql.Tx) error) (TaskRun, error) {
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
	if source == "internal" && t.Schedule.Frequency == "on_new_mail" {
		// New-mail runs always start from the newest messages.
		r.NextCursor = ""
	}
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
	if e = runGuard(tx, guard); e != nil {
		return r, e
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
	if r.Expires <= time.Now().Unix() || !slices.Contains([]string{"queued", "running", "awaiting_ai"}, r.State) {
		return p, ErrConflict
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
	return s.taskRunPolicy(a, p, r, t)
}

// taskRunPolicy checks that a run still belongs to the current task, grant and
// administrator, and narrows the account policy to the task. It deliberately
// ignores run state, deadline and worker lease: those gate starting new writes,
// while recovery of an already issued write must still work after the run ends.
func (s *Service) taskRunPolicy(a Access, p AutomationPolicy, r TaskRun, t Task) (AutomationPolicy, error) {
	if r.Owner != a.Owner || r.AdminVersion != a.Version || t.Version != r.TaskVersion || t.AdminVersion != a.Version || p.Version != r.PolicyVersion {
		return p, ErrConflict
	}
	if r.Source == "internal" && (!p.AllowInternal || !t.Enabled || t.Trigger != "internal") {
		return p, ErrDenied
	}
	if r.Source == "external" && (!t.Enabled || t.Trigger != "external") {
		return p, ErrDenied
	}
	for _, f := range t.SourceFolders {
		if !slices.Contains(p.SourceFolders, f) {
			return p, ErrDenied
		}
	}
	if t.Mode == "execute" && p.Mode != "execute" {
		return p, ErrDenied
	}
	if _, e := s.taskRules(a, t); e != nil {
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

// Preview limits: one page shows 25 messages; "only matched" scans at most 500
// source messages per call so a sparse rule can be located without unbounded
// work. Previews read only the local index: no body reads, flag changes or moves.
const (
	previewPageSize = 25
	previewScanCap  = 500
)

func (s *Service) PreviewTask(a Access, id, cursor string, onlyMatched bool) (RulePreview, error) {
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
	return s.previewTaskRules(a, t, rules, p, cursor, onlyMatched)
}

// previewTaskRules evaluates a task against a rule snapshot. It reads only the
// local index and never writes; PreviewTask and draft workflow previews share it.
func (s *Service) previewTaskRules(a Access, t Task, rules []Rule, p AutomationPolicy, cursor string, onlyMatched bool) (RulePreview, error) {
	for _, r := range rules {
		if r.Enabled && !ruleVisible(a, r) {
			return RulePreview{}, ErrDenied
		}
	}
	p.SourceFolders = t.SourceFolders
	p.Since = max(p.Since, t.Since)
	p.ProtectUnread = p.ProtectUnread || t.ProtectUnread
	p.ProtectFlagged = p.ProtectFlagged || t.ProtectFlagged
	out := RulePreview{Items: []RuleEvaluation{}, Counts: map[string]int{}, MaxBatch: min(p.MaxBatch, t.MaxBatch), OnlyMatched: onlyMatched}
	now := time.Now().Unix()
	if !onlyMatched {
		ms, total, next, e := s.ruleCandidates(a, t.AccountID, t.SourceFolders, cursor, previewPageSize)
		if e != nil {
			return RulePreview{}, e
		}
		out.Total, out.NextCursor, out.Scanned = total, next, len(ms)
		for _, m := range ms {
			item := evaluateRules(m, rules, p, now)
			out.Counts[item.Status]++
			out.Items = append(out.Items, item)
		}
		return out, nil
	}
	for out.Scanned < previewScanCap && len(out.Items) < previewPageSize {
		ms, total, next, e := s.ruleCandidates(a, t.AccountID, t.SourceFolders, cursor, min(100, previewScanCap-out.Scanned))
		if e != nil {
			return RulePreview{}, e
		}
		out.Total = total
		stopped := false
		for _, m := range ms {
			out.Scanned++
			item := evaluateRules(m, rules, p, now)
			out.Counts[item.Status]++
			if item.Status == "matched" {
				out.Items = append(out.Items, item)
			}
			cursor = ruleCursorAfter(t.AccountID, t.SourceFolders, m)
			if len(out.Items) == previewPageSize {
				stopped = true
				break
			}
		}
		if next == "" && !stopped {
			cursor = ""
			break
		}
	}
	out.NextCursor = cursor
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
