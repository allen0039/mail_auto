package mail

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

func readinessFor(t *testing.T, s *Service, a Access, account, task string) TaskReadiness {
	t.Helper()
	all, err := s.TaskReadiness(a, account)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.TaskID == task {
			return r
		}
	}
	t.Fatal("task missing from readiness", task)
	return TaskReadiness{}
}

func issueCodes(r TaskReadiness) []string {
	out := []string{}
	for _, i := range r.Issues {
		out = append(out, i.Code)
	}
	return out
}

// tableCounts snapshots everything a read-only check must not touch.
func tableCounts(t *testing.T, s *Service) string {
	t.Helper()
	var runs, audits, ops int
	var tasks, policies string
	for q, dst := range map[string]any{"SELECT count(*) FROM task_runs": &runs, "SELECT count(*) FROM audits": &audits, "SELECT count(*) FROM operations": &ops} {
		if err := s.Store.DB.QueryRow(q).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Store.DB.QueryRow("SELECT group_concat(id||':'||version||':'||enabled||':'||next_run_at) FROM automation_tasks").Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.DB.QueryRow("SELECT group_concat(account_id||':'||version) FROM automation_policies").Scan(&policies); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]any{runs, audits, ops, tasks, policies})
	return string(raw)
}

// setPolicyField rewrites one stored policy field, bypassing validation, to
// reach states (expiry, revoked destinations) that only time or another
// administrator action would produce.
func setPolicyField(t *testing.T, s *Service, account string, edit func(*AutomationPolicy)) {
	t.Helper()
	var raw string
	if err := s.Store.DB.QueryRow("SELECT payload FROM automation_policies WHERE account_id=?", account).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var rec policyRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatal(err)
	}
	edit(&rec.Policy)
	next, _ := json.Marshal(rec)
	if _, err := s.Store.DB.Exec("UPDATE automation_policies SET payload=? WHERE account_id=?", string(next), account); err != nil {
		t.Fatal(err)
	}
}

func TestReadinessReadyTaskIsReadOnlyAndShowsLastRun(t *testing.T) {
	s, remote, admin, _, task, m := taskFixture(t, "execute", "internal", "rules")
	before := tableCounts(t, s)
	r := readinessFor(t, s, admin, m.AccountID, task.ID)
	if !r.Ready || len(r.Issues) != 0 || r.LastRun != nil {
		t.Fatal("ready task reported issues", r)
	}
	if after := tableCounts(t, s); after != before || remote.moves != 0 {
		t.Fatal("readiness changed state", before, after)
	}
	// A fixed-rule task needs no model, Telegram or MCP: the service has none
	// configured here and the run still completes with a verified move.
	run, err := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "ready-run"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StartOperations(); err != nil {
		t.Fatal(err)
	}
	s.runFixedTask(context.Background(), run.ID)
	s.Stop()
	r = readinessFor(t, s, admin, m.AccountID, task.ID)
	if r.LastRun == nil || r.LastRun.ID != run.ID || r.LastRun.Completed != 1 || remote.moves != 1 {
		t.Fatal("last run not reported from the stored record", r.LastRun, remote.moves)
	}
}

func TestReadinessMatchesEnableAndRunChecks(t *testing.T) {
	s, _, admin, _, task, m := taskFixture(t, "execute", "internal", "rules")
	acc := m.AccountID

	// Internal schedule not allowed: readiness says so, and enabling refuses.
	p, _ := s.AutomationPolicy(admin, acc)
	p.AllowInternal = false
	if _, err := s.SaveAutomationPolicy(admin, p); err != nil {
		t.Fatal(err)
	}
	task, _ = s.SetTaskEnabled(admin, task.ID, task.Version, false)
	r := readinessFor(t, s, admin, acc, task.ID)
	if r.Ready || !slices.Contains(issueCodes(r), "internal_not_allowed") || !slices.Contains(issueCodes(r), "task_paused") {
		t.Fatal(r)
	}
	if _, err := s.SetTaskEnabled(admin, task.ID, task.Version, true); !errors.Is(err, ErrDenied) {
		t.Fatal("enable allowed despite readiness block", err)
	}
	p, _ = s.AutomationPolicy(admin, acc)
	p.AllowInternal = true
	if _, err := s.SaveAutomationPolicy(admin, p); err != nil {
		t.Fatal(err)
	}
	if task, _ = s.SetTaskEnabled(admin, task.ID, task.Version, true); !task.Enabled {
		t.Fatal("re-enable failed")
	}
	if r = readinessFor(t, s, admin, acc, task.ID); !r.Ready {
		t.Fatal("not ready after fix", r)
	}

	// Expired grant: readiness and a manual run agree.
	setPolicyField(t, s, acc, func(p *AutomationPolicy) { p.Expires = time.Now().Unix() - 1 })
	if r = readinessFor(t, s, admin, acc, task.ID); !slices.Contains(issueCodes(r), "policy_expired") {
		t.Fatal(r)
	}
	if _, err := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "expired"}, "manual"); !errors.Is(err, ErrDenied) {
		t.Fatal("run allowed with expired grant", err)
	}
	setPolicyField(t, s, acc, func(p *AutomationPolicy) { p.Expires = time.Now().Unix() + 86400 })

	// Destination no longer allowed: readiness names the rule; the run's
	// write step is refused by the same check.
	setPolicyField(t, s, acc, func(p *AutomationPolicy) { p.Destinations = []string{} })
	r = readinessFor(t, s, admin, acc, task.ID)
	if r.Ready || !slices.Contains(issueCodes(r), "destination_not_allowed") {
		t.Fatal(r)
	}
	run, err := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "no-destination"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	s.runFixedTask(context.Background(), run.ID)
	if run, _ = s.TaskRun(admin, run.ID); run.State != "failed" || run.Completed != 0 {
		t.Fatal("run wrote outside the grant", run)
	}
	setPolicyField(t, s, acc, func(p *AutomationPolicy) { p.Destinations = []string{"Archive"} })

	// Writes switched off for the account.
	if _, err = s.Store.DB.Exec("UPDATE folder_settings SET writes_enabled=0 WHERE account_id=?", acc); err != nil {
		t.Fatal(err)
	}
	if r = readinessFor(t, s, admin, acc, task.ID); !slices.Contains(issueCodes(r), "writes_disabled") {
		t.Fatal(r)
	}
	if _, err = s.Store.DB.Exec("UPDATE folder_settings SET writes_enabled=1 WHERE account_id=?", acc); err != nil {
		t.Fatal(err)
	}

	// Source folder no longer synced for the account.
	if _, err = s.Store.DB.Exec(`UPDATE accounts SET folders='["Archive"]' WHERE id=?`, acc); err != nil {
		t.Fatal(err)
	}
	if r = readinessFor(t, s, admin, acc, task.ID); !slices.Contains(issueCodes(r), "source_not_synced") {
		t.Fatal(r)
	}
	if _, err = s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "not-synced"}, "manual"); !errors.Is(err, ErrDenied) {
		t.Fatal("run allowed for unsynced source", err)
	}
	if _, err = s.Store.DB.Exec(`UPDATE accounts SET folders='[]' WHERE id=?`, acc); err != nil {
		t.Fatal(err)
	}

	// A bound rule changed after binding.
	rules, _ := s.Rules(admin, acc)
	task.RuleIDs = []string{rules[0].ID}
	task.RuleVersions = map[string]int{rules[0].ID: rules[0].Version}
	if task, err = s.SaveTask(admin, task); err != nil {
		t.Fatal(err)
	}
	rule := rules[0]
	rule.Name = "changed"
	if _, err = s.SaveRule(admin, rule); err != nil {
		t.Fatal(err)
	}
	if r = readinessFor(t, s, admin, acc, task.ID); !slices.Contains(issueCodes(r), "rules_changed") {
		t.Fatal(r)
	}
	if _, err = s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "stale-rule"}, "manual"); !errors.Is(err, ErrConflict) {
		t.Fatal("run allowed with stale rule binding", err)
	}
}

// The extracted policy check must report errors in saveTask's former order
// with the former error kinds.
func TestTaskPolicyIssuesKeepSaveErrors(t *testing.T) {
	p := AutomationPolicy{Enabled: true, Mode: "observe", SourceFolders: []string{"INBOX"}, Expires: time.Now().Unix() + 60, AdminVersion: 1}
	a := Access{Owner: "admin", Version: 1}
	cases := []struct {
		t    Task
		code string
		err  error
	}{
		{Task{SourceFolders: []string{"Other"}, Trigger: "internal", Engine: "rules", Mode: "observe"}, "source_not_authorized", ErrDenied},
		{Task{SourceFolders: []string{"INBOX"}, Trigger: "internal", Engine: "ai", Mode: "observe"}, "ai_engine_internal", ErrInput},
		{Task{SourceFolders: []string{"INBOX"}, Trigger: "internal", Engine: "rules", Mode: "observe"}, "internal_not_allowed", ErrDenied},
		{Task{SourceFolders: []string{"INBOX"}, Trigger: "manual", Engine: "rules", Mode: "execute"}, "execute_not_allowed", ErrDenied},
		{Task{SourceFolders: []string{"INBOX"}, Trigger: "manual", Engine: "rules", Mode: "observe", Enabled: true}, "", nil},
	}
	for _, c := range cases {
		got := taskPolicyIssues(a, c.t, p, time.Now().Unix())
		if c.code == "" {
			if len(got) != 0 {
				t.Fatal(got)
			}
			continue
		}
		if len(got) == 0 || got[0].Code != c.code || !errors.Is(got[0].err, c.err) {
			t.Fatal(c.code, got)
		}
	}
	expired := p
	expired.Expires = time.Now().Unix() - 1
	got := taskPolicyIssues(a, Task{SourceFolders: []string{"INBOX"}, Trigger: "manual", Engine: "rules", Mode: "observe", Enabled: true}, expired, time.Now().Unix())
	if len(got) != 1 || got[0].Code != "policy_expired" || !errors.Is(got[0].err, ErrDenied) {
		t.Fatal(got)
	}
}
