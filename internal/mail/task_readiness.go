package mail

import (
	"slices"
	"strings"
	"time"
)

// TaskIssue is one reason a task cannot be saved enabled, enabled or run as
// configured. Code is stable; Area names the setting that fixes it (policy,
// folders, rules, task); Target is a folder or rule ID when relevant.
type TaskIssue struct {
	Code     string `json:"code"`
	Area     string `json:"area"`
	Target   string `json:"target,omitempty"`
	Blocking bool   `json:"blocking"`
	err      error
}

// TaskReadiness is a read-only answer to "will this task run as configured".
// It never creates runs, changes grants or calls a model; execution still
// re-validates everything.
type TaskReadiness struct {
	TaskID string      `json:"task_id"`
	Ready  bool        `json:"ready"`
	Issues []TaskIssue `json:"issues"`
	// LastRun is the latest run record as stored (counts are the server's
	// own; nothing is inferred). Nil when the task never ran.
	LastRun *TaskRun `json:"last_run,omitempty"`
}

// taskPolicyIssues is the policy check shared by saving a task and by the
// readiness report. The order is the order saveTask reports errors in.
func taskPolicyIssues(a Access, t Task, p AutomationPolicy, now int64) []TaskIssue {
	out := []TaskIssue{}
	add := func(code, area, target string, err error) {
		out = append(out, TaskIssue{Code: code, Area: area, Target: target, Blocking: true, err: err})
	}
	for _, f := range t.SourceFolders {
		if !slices.Contains(p.SourceFolders, f) {
			add("source_not_authorized", "policy", f, ErrDenied)
		}
	}
	if t.Engine == "ai" && t.Trigger == "internal" {
		add("ai_engine_internal", "task", "", ErrInput)
	}
	if t.Engine == "ai" && strings.HasPrefix(a.Owner, AgentOwnerPrefix) {
		add("ai_engine_agent", "task", "", ErrCapability)
	}
	if t.Trigger == "internal" && !p.AllowInternal {
		add("internal_not_allowed", "policy", "", ErrDenied)
	}
	if t.Mode == "execute" && p.Mode != "execute" {
		add("execute_not_allowed", "policy", "", ErrDenied)
	}
	if t.Enabled {
		if !p.Enabled {
			add("policy_disabled", "policy", "", ErrDenied)
		}
		if p.Expires <= now {
			add("policy_expired", "policy", "", ErrDenied)
		}
		if p.AdminVersion != a.Version {
			add("policy_admin_changed", "policy", "", ErrDenied)
		}
	}
	return out
}

// sourceNotSynced reports a source folder outside the account's limited
// folder list; authorizedAutomation refuses such runs.
func sourceNotSynced(account Account, f string) bool {
	return len(account.Folders) > 0 && !slices.Contains(account.Folders, f)
}

// grantSourceIssues is the per-folder admission authorizedAutomation applies
// before any run: every source folder of the grant, not only the task's own,
// must be readable by the caller and inside the account's sync range. The
// readiness report uses the same function, so it names the folder a run would
// be refused for.
func grantSourceIssues(a Access, account Account, p AutomationPolicy) []TaskIssue {
	out := []TaskIssue{}
	for _, f := range p.SourceFolders {
		switch {
		case !a.Allows(account.ID, f, "read"):
			out = append(out, TaskIssue{Code: "source_not_readable", Area: "policy", Target: f, Blocking: true, err: ErrDenied})
		case sourceNotSynced(account, f):
			out = append(out, TaskIssue{Code: "source_not_synced", Area: "account", Target: f, Blocking: true, err: ErrDenied})
		}
	}
	return out
}

// actionsAllowed and destinationAllowed are the execute-mode grant checks
// for one decision; SubmitAI and the readiness report share them.
func actionsAllowed(p AutomationPolicy, action string, also []string) bool {
	if !slices.Contains(p.Actions, action) {
		return false
	}
	for _, extra := range also {
		if !slices.Contains(p.Actions, extra) {
			return false
		}
	}
	return true
}
func destinationAllowed(p AutomationPolicy, destination string) bool {
	return slices.Contains(p.Destinations, destination)
}

// ruleWriteIssue is the per-rule write permission that SubmitAI enforces for
// execute mode, plus the account folder a rule's action needs.
func ruleWriteIssue(r Rule, p AutomationPolicy, fs FolderSettings) string {
	if r.Action == "keep" || r.Action == "ai" {
		return ""
	}
	if !actionsAllowed(p, r.Action, r.Also) {
		return "action_not_allowed"
	}
	switch r.Action {
	case "move":
		if !destinationAllowed(p, r.Destination) {
			return "destination_not_allowed"
		}
	case "archive":
		if fs.Archive == "" {
			return "archive_folder_missing"
		}
	case "trash":
		if fs.Trash == "" {
			return "trash_folder_missing"
		}
	}
	return ""
}

// TaskReadiness reports, for each visible task of an account, what would
// stop it from being enabled or run. It only reads.
func (s *Service) TaskReadiness(a Access, account string) ([]TaskReadiness, error) {
	tasks, e := s.Tasks(a, account)
	if e != nil {
		return nil, e
	}
	a, e = s.CheckAccess(a)
	if e != nil {
		return nil, e
	}
	acc, e := s.Account(account)
	if e != nil {
		return nil, e
	}
	p, e := s.loadAutomationPolicy(account)
	if e != nil {
		return nil, e
	}
	fs, e := s.FolderSettings(account)
	if e != nil {
		return nil, e
	}
	all, e := s.rulesForAccount(account)
	if e != nil {
		return nil, e
	}
	now := time.Now().Unix()
	out := make([]TaskReadiness, 0, len(tasks))
	for _, t := range tasks {
		r := TaskReadiness{TaskID: t.ID, Issues: []TaskIssue{}}
		add := func(code, area, target string, blocking bool) {
			r.Issues = append(r.Issues, TaskIssue{Code: code, Area: area, Target: target, Blocking: blocking})
		}
		// Check what enabling (or a run of an enabled task) would require.
		check := t
		if check.Trigger != "manual" {
			check.Enabled = true
		}
		r.Issues = append(r.Issues, taskPolicyIssues(a, check, p, now)...)
		if check.Trigger == "manual" {
			// Manual runs still need an open, current grant (authorizedAutomation).
			if !p.Enabled {
				add("policy_disabled", "policy", "", true)
			}
			if p.Expires <= now {
				add("policy_expired", "policy", "", true)
			}
			if p.AdminVersion != a.Version {
				add("policy_admin_changed", "policy", "", true)
			}
		}
		// The folder admission a run applies to the whole grant, then any task
		// folder outside the grant that is not synced either.
		r.Issues = append(r.Issues, grantSourceIssues(a, acc, p)...)
		for _, f := range t.SourceFolders {
			if !slices.Contains(p.SourceFolders, f) && sourceNotSynced(acc, f) {
				add("source_not_synced", "account", f, true)
			}
		}
		rules, _, _, err := resolveTaskRulesFrom(a, t, false, all)
		if err != nil {
			add("rules_changed", "rules", "", true)
		}
		if t.Mode == "execute" && t.Engine == "rules" {
			if !fs.WritesEnabled {
				add("writes_disabled", "folders", "", true)
			}
			for _, rule := range rules {
				if !rule.Enabled || !taskRuleOverlap(t.SourceFolders, rule.SourceFolders) {
					continue
				}
				if code := ruleWriteIssue(rule, p, fs); code != "" {
					area := "policy"
					if strings.HasSuffix(code, "_folder_missing") {
						area = "folders"
					}
					add(code, area, rule.ID, true)
				}
			}
		}
		if t.Trigger != "manual" && !t.Enabled {
			add("task_paused", "task", "", false)
		}
		var runID string
		if s.Store.DB.QueryRow("SELECT id FROM task_runs WHERE task_id=? AND (owner=? OR ?='admin') ORDER BY created_at DESC,id DESC LIMIT 1", t.ID, a.Owner, a.Owner).Scan(&runID) == nil {
			if run, err := s.loadTaskRun(runID); err == nil {
				r.LastRun = &run
			}
		}
		r.Ready = true
		for _, i := range r.Issues {
			if i.Blocking {
				r.Ready = false
			}
		}
		out = append(out, r)
	}
	return out, nil
}
