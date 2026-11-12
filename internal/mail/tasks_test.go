package mail

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func taskFixture(t *testing.T, mode, trigger, engine string) (*Service, *automationRemote, Access, Access, Task, Message) {
	t.Helper()
	s, remote, admin, client, _, p, m := automationFixture(t, mode)
	p.AllowInternal = true
	var e error
	if _, e = s.SaveAutomationPolicy(admin, p); e != nil {
		t.Fatal(e)
	}
	task, e := s.SaveTask(admin, Task{AccountID: m.AccountID, Name: "归档任务", Trigger: trigger, Engine: engine, Mode: "execute", Enabled: true, SourceFolders: []string{"INBOX"}, MaxBatch: 20, Notify: "inherit", Schedule: TaskSchedule{Frequency: "daily", Hour: 2, Timezone: "Asia/Shanghai"}})
	if e != nil {
		t.Fatal(e)
	}
	if task.Enabled || task.Mode != "observe" {
		t.Fatal("new task must be paused observation", task)
	}
	if mode == "execute" {
		task.Mode = "execute"
	}
	task.Enabled = true
	task, e = s.SaveTask(admin, task)
	if e != nil {
		t.Fatal(e)
	}
	s.workerID = "task-test-worker"
	return s, remote, admin, client, task, m
}

func TestFixedTaskBatchLeaseCoversRunDeadline(t *testing.T) {
	s, _, admin, _, task, message := taskFixture(t, "execute", "manual", "rules")
	run, err := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "full-deadline"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.DB.Exec("UPDATE task_runs SET state='running',lease_owner=?,lease_until=? WHERE id=?", s.workerID, time.Now().Unix()+45, run.ID); err != nil {
		t.Fatal(err)
	}
	batch, err := s.ClaimAI(Access{Owner: run.Owner, Version: run.AdminVersion}, AIClaimInput{RunID: run.ID, AccountID: message.AccountID})
	if err != nil || batch.Expires != run.Expires {
		t.Fatal("batch expires before its task", batch.Expires, run.Expires, err)
	}
	var expires int64
	if err = s.Store.DB.QueryRow("SELECT expires FROM automation_claims WHERE batch_id=?", batch.ID).Scan(&expires); err != nil || expires != run.Expires {
		t.Fatal("claim expires before its task", expires, run.Expires, err)
	}
}

func TestTaskObserveIdempotenceAndIdentityRevocation(t *testing.T) {
	s, remote, admin, _, task, _ := taskFixture(t, "observe", "manual", "rules")
	in := TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "once"}
	run, e := s.RunTask(admin, in, "manual")
	if e != nil {
		t.Fatal(e)
	}
	if run.Owner == "admin" {
		t.Fatal("unrestricted task identity")
	}
	retry, e := s.RunTask(admin, in, "manual")
	if e != nil || retry.ID != run.ID {
		t.Fatal(retry, e)
	}
	s.runFixedTask(context.Background(), run.ID)
	run, e = s.TaskRun(admin, run.ID)
	if e != nil || run.State != "observed" || run.Claimed != 1 || !run.Finalized || remote.moves != 0 {
		t.Fatal(run, e)
	}
	access, e := s.CheckAccess(Access{Owner: run.Owner, Version: admin.Version})
	if e != nil || access.Allows(task.AccountID, "INBOX", "restore") {
		t.Fatal(access, e)
	}
	task.Name = "changed"
	if _, e = s.SaveTask(admin, task); e != nil {
		t.Fatal(e)
	}
	if _, e = s.CheckAccess(access); !errors.Is(e, ErrDenied) {
		t.Fatal("old task identity survived", e)
	}
}
func TestTaskExecuteVerifiedAndSharedManualConflict(t *testing.T) {
	s, remote, admin, _, task, m := taskFixture(t, "execute", "manual", "rules")
	run, e := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "run"}, "manual")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.StartOperations(); e != nil {
		t.Fatal(e)
	}
	s.runFixedTask(context.Background(), run.ID)
	s.Stop()
	run, e = s.TaskRun(admin, run.ID)
	if e != nil || run.State != "completed" || run.Completed != 1 || remote.moves != 1 {
		t.Fatal(run, e, remote.moves)
	}
	mail, e := s.Message(m.Ref)
	if e != nil || mail.Folder != "Archive" {
		t.Fatal(mail, e)
	}
}
func TestTaskAIRuleNeverFallsThrough(t *testing.T) {
	s, remote, admin, _, task, m := taskFixture(t, "execute", "manual", "rules")
	_, e := s.SaveRule(admin, Rule{AccountID: m.AccountID, Name: "优先由 AI 判断", Priority: 0, Enabled: true, SourceFolders: []string{"INBOX"}, Match: RuleGroup{Mode: "all"}, Action: "ai"})
	if e != nil {
		t.Fatal(e)
	}
	run, e := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "ai-stop"}, "manual")
	if e != nil {
		t.Fatal(e)
	}
	s.runFixedTask(context.Background(), run.ID)
	run, e = s.TaskRun(admin, run.ID)
	if e != nil || run.NeedsAI != 1 || run.Kept != 1 || run.State != "requires_ai" || remote.moves != 0 {
		t.Fatal(run, e)
	}
}
func TestExternalTaskBindsBatchAndDeniesOtherClient(t *testing.T) {
	s, _, admin, client, task, m := taskFixture(t, "observe", "external", "ai")
	run, e := s.RunTask(client, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "external-1"}, "external")
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.ClaimAI(client, AIClaimInput{AccountID: m.AccountID, RunID: run.ID, RunKey: "page-1"})
	if e != nil || b.RunID != run.ID || len(b.Items) != 1 {
		t.Fatal(b, e)
	}
	retry, e := s.ClaimAI(client, AIClaimInput{AccountID: m.AccountID, RunID: run.ID, RunKey: "different-key"})
	if e != nil || retry.ID != b.ID {
		t.Fatal("second batch in one run", retry, e)
	}
	_, secret, e := s.CreateToken(TokenInput{Name: "other", Accounts: []string{m.AccountID}, Actions: []string{"read", "automation"}})
	if e != nil {
		t.Fatal(e)
	}
	other, e := s.AuthenticateToken(secret)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.TaskRun(other, run.ID); !errors.Is(e, ErrDenied) {
		t.Fatal("cross owner run", e)
	}
	if _, e = s.ClaimAI(other, AIClaimInput{AccountID: m.AccountID, RunID: run.ID, RunKey: "other"}); e == nil {
		t.Fatal("cross owner claim")
	}
	if _, e = s.SubmitAI(context.Background(), client, AISubmitInput{BatchID: b.ID, IdempotencyKey: "submit", Decisions: decisionFor(b)}); e != nil {
		t.Fatal(e)
	}
	s.reconcileTaskRun(run.ID)
	run, e = s.TaskRun(admin, run.ID)
	if e != nil || run.State != "observed" {
		t.Fatal(run, e)
	}
}
func TestTaskLeaseExpiryAndModeChangeRejectOldWork(t *testing.T) {
	s, remote, admin, client, task, m := taskFixture(t, "execute", "external", "ai")
	run, e := s.RunTask(client, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "old-mode"}, "external")
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.ClaimAI(client, AIClaimInput{AccountID: m.AccountID, RunID: run.ID, RunKey: "one"})
	if e != nil {
		t.Fatal(e)
	}
	task.Trigger = "manual"
	if _, e = s.SaveTask(admin, task); e != nil {
		t.Fatal(e)
	}
	if _, e = s.SubmitAI(context.Background(), client, AISubmitInput{BatchID: b.ID, IdempotencyKey: "late", Decisions: decisionFor(b)}); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	s.reconcileTaskRun(run.ID)
	run, e = s.TaskRun(admin, run.ID)
	if e != nil || run.State != "cancelled" || remote.moves != 0 {
		t.Fatal(run, e)
	}
}
func TestTaskPermissionsAndNativeGrant(t *testing.T) {
	s, _, admin, client, task, m := taskFixture(t, "observe", "manual", "rules")
	if _, e := s.SaveTask(client, task); !errors.Is(e, ErrDenied) {
		t.Fatal("automation grants configuration", e)
	}
	p, e := s.loadAutomationPolicy(m.AccountID)
	if e != nil {
		t.Fatal(e)
	}
	p.AllowInternal = false
	if _, e = s.SaveAutomationPolicy(admin, p); e != nil {
		t.Fatal(e)
	}
	task.Trigger = "internal"
	if _, e = s.SaveTask(admin, task); !errors.Is(e, ErrDenied) {
		t.Fatal("native grant bypass", e)
	}
	task.Engine = "ai"
	if _, e = s.SaveTask(admin, task); !errors.Is(e, ErrInput) {
		t.Fatal("internal AI allowed", e)
	}
}

func TestTaskExplicitRuleBindingSnapshotsVersions(t *testing.T) {
	s, _, admin, _, task, m := taskFixture(t, "observe", "manual", "rules")
	rules, e := s.Rules(admin, m.AccountID)
	if e != nil || len(rules) != 1 {
		t.Fatal(rules, e)
	}
	task.RuleIDs = []string{rules[0].ID}
	task.RuleVersions = map[string]int{rules[0].ID: rules[0].Version}
	task, e = s.SaveTask(admin, task)
	if e != nil || len(task.RuleIDs) != 1 || task.RuleVersions[rules[0].ID] != rules[0].Version {
		t.Fatal(task, e)
	}
	selected, e := s.taskRules(admin, task)
	if e != nil || len(selected) != 1 || selected[0].ID != rules[0].ID {
		t.Fatal(selected, e)
	}
	preview, e := s.PreviewTask(admin, task.ID, "")
	if e != nil || len(preview.Items) != 1 || preview.Items[0].RuleID != rules[0].ID {
		t.Fatal(preview, e)
	}
	run, e := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "bound"}, "manual")
	if e != nil || run.RulesDigest != rulesDigest(selected) {
		t.Fatal(run, e)
	}
	if e = s.CancelTaskRun(admin, run.ID); e != nil {
		t.Fatal(e)
	}

	rules[0].Name = "已修改规则"
	if _, e = s.SaveRule(admin, rules[0]); e != nil {
		t.Fatal(e)
	}
	if _, e = s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "stale-bound"}, "manual"); !errors.Is(e, ErrConflict) {
		t.Fatal("stale bound rule was accepted", e)
	}
}

func TestTaskBindingCannotSkipHigherPriorityRule(t *testing.T) {
	s, _, admin, _, task, m := taskFixture(t, "observe", "manual", "rules")
	rules, e := s.Rules(admin, m.AccountID)
	if e != nil || len(rules) != 1 {
		t.Fatal(rules, e)
	}
	high, e := s.SaveRule(admin, Rule{AccountID: m.AccountID, Name: "先保护", Priority: rules[0].Priority - 1, Enabled: true, SourceFolders: []string{"INBOX"}, Match: RuleGroup{Mode: "all"}, Action: "keep"})
	if e != nil {
		t.Fatal(e)
	}
	task.RuleIDs = []string{rules[0].ID}
	task.RuleVersions = map[string]int{rules[0].ID: rules[0].Version}
	if _, e = s.SaveTask(admin, task); !errors.Is(e, ErrInput) {
		t.Fatal("task skipped higher-priority protection rule", e)
	}
	task.RuleIDs = []string{high.ID, rules[0].ID}
	task.RuleVersions = map[string]int{high.ID: high.Version, rules[0].ID: rules[0].Version}
	if _, e = s.SaveTask(admin, task); e != nil {
		t.Fatal("complete binding rejected", e)
	}
}

func TestTaskClaimsBlockOrdinaryManualExecute(t *testing.T) {
	s, _, admin, client, _, _, m := automationFixture(t, "execute")
	_, e := s.ClaimAI(client, AIClaimInput{AccountID: m.AccountID, RunKey: "claimed"})
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.NewPlan(context.Background(), admin, PlanInput{Refs: []string{m.Ref}, Action: "trash"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ExecutePlan(admin, p.ID, "manual-overlap"); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
}
func TestTaskAccountConcurrentRunsHaveOneWinner(t *testing.T) {
	s, _, admin, _, task, _ := taskFixture(t, "observe", "manual", "rules")
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, e := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: time.Unix(int64(i), 0).String()}, "manual")
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	wins := 0
	for e := range results {
		if e == nil {
			wins++
		} else if !errors.Is(e, ErrBusy) {
			t.Fatal(e)
		}
	}
	if wins != 1 {
		t.Fatal(wins)
	}
}
func TestTaskScheduleTimezoneDSTAndInvalid(t *testing.T) {
	c := TaskSchedule{Frequency: "daily", Hour: 2, Timezone: "Asia/Shanghai"}
	start := time.Date(2026, 10, 23, 17, 0, 0, 0, time.UTC)
	n, e := nextSchedule(c, start)
	if e != nil || time.Unix(n, 0).UTC() != time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC) {
		t.Fatal(time.Unix(n, 0), e)
	}
	c.Timezone = "America/New_York"
	c.Hour = 2
	c.Minute = 30
	n, e = nextSchedule(c, time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC))
	if e != nil || time.Unix(n, 0).UTC() != time.Date(2026, 3, 9, 6, 30, 0, 0, time.UTC) {
		t.Fatal("spring DST", time.Unix(n, 0), e)
	}
	c.Hour = 1
	c.Minute = 30
	n, e = nextSchedule(c, time.Date(2026, 11, 1, 5, 31, 0, 0, time.UTC))
	if e != nil || time.Unix(n, 0).UTC() != time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC) {
		t.Fatal("fall DST duplicate", time.Unix(n, 0), e)
	}
	c.Timezone = "invalid"
	if _, e = nextSchedule(c, start); !errors.Is(e, ErrInput) {
		t.Fatal(e)
	}
}
func TestTaskExpiredWorkerAndFreshStarBlockWrites(t *testing.T) {
	s, remote, admin, _, task, _ := taskFixture(t, "execute", "manual", "rules")
	remote.flagged = true
	run, e := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "fresh-star"}, "manual")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.StartOperations(); e != nil {
		t.Fatal(e)
	}
	s.runFixedTask(context.Background(), run.ID)
	s.Stop()
	run, e = s.TaskRun(admin, run.ID)
	if e != nil || run.Completed != 0 || run.Failed != 1 || remote.moves != 0 {
		t.Fatal(run, e)
	}
}

func TestTaskExpiredNativeLeaseAndPauseAfterRevocation(t *testing.T) {
	s, remote, admin, _, task, m := taskFixture(t, "execute", "internal", "rules")
	r, e := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "expired-worker"}, "manual")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Store.DB.Exec("UPDATE task_runs SET state='running',lease_owner='old-process',lease_until=? WHERE id=?", time.Now().Unix()-1, r.ID); e != nil {
		t.Fatal(e)
	}
	s.reconcileTaskRun(r.ID)
	r, e = s.TaskRun(admin, r.ID)
	if e != nil || r.State != "interrupted" || r.ErrorCode != "worker_lease_expired" || remote.moves != 0 {
		t.Fatal(r, e)
	}
	p, e := s.loadAutomationPolicy(m.AccountID)
	if e != nil {
		t.Fatal(e)
	}
	p.AllowInternal = false
	p.Enabled = false
	if _, e = s.SaveAutomationPolicy(admin, p); e != nil {
		t.Fatal(e)
	}
	paused, e := s.SetTaskEnabled(admin, task.ID, task.Version, false)
	if e != nil || paused.Enabled {
		t.Fatal("cannot pause revoked schedule", paused, e)
	}
}
func TestTaskDurableOccurrenceAndMisfireCoalescing(t *testing.T) {
	s, _, admin, _, task, _ := taskFixture(t, "observe", "internal", "rules")
	at := time.Now().Add(-3 * 24 * time.Hour).Unix()
	r, e := s.createTaskRun(admin, task, taskRunKey(task, at), "internal", at)
	if e != nil {
		t.Fatal(e)
	}
	retry, e := s.createTaskRun(admin, task, taskRunKey(task, at), "internal", at)
	if e != nil || retry.ID != r.ID {
		t.Fatal(retry, e)
	}
	updated, e := s.loadTask(task.ID)
	if e != nil || updated.NextRunAt <= time.Now().Unix() {
		t.Fatal(updated, e)
	}
	var count int
	if e = s.Store.DB.QueryRow("SELECT count(*) FROM task_runs").Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
}

func TestTaskTickRecordsStaleRuleAndAdvancesSchedule(t *testing.T) {
	s, _, admin, _, task, m := taskFixture(t, "observe", "internal", "rules")
	rules, e := s.Rules(admin, m.AccountID)
	if e != nil || len(rules) != 1 {
		t.Fatal(rules, e)
	}
	task.RuleIDs = []string{rules[0].ID}
	task.RuleVersions = map[string]int{rules[0].ID: rules[0].Version}
	task, e = s.SaveTask(admin, task)
	if e != nil {
		t.Fatal(e)
	}
	rules[0].Name = "变更后的规则"
	if _, e = s.SaveRule(admin, rules[0]); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Store.DB.Exec("UPDATE automation_tasks SET next_run_at=? WHERE id=?", time.Now().Unix()-1, task.ID); e != nil {
		t.Fatal(e)
	}
	s.taskTick(context.Background())
	updated, e := s.loadTask(task.ID)
	if e != nil || updated.NextRunAt <= time.Now().Unix() {
		t.Fatal(updated, e)
	}
	runs, e := s.TaskRuns(admin, task.ID)
	if e != nil || len(runs) != 1 || runs[0].State != "failed" || runs[0].ErrorCode != "state_conflict" {
		t.Fatal(runs, e)
	}
}

func TestExternalCadenceWatchdogNeverTriggersMail(t *testing.T) {
	s, remote, admin, _, task, _ := taskFixture(t, "observe", "external", "ai")
	configureNotification(t, s, admin, "failures")
	task.ExpectedSeconds = 3600
	task.UpdatedAt = time.Now().Unix() - 7200
	task.LastTriggered = task.UpdatedAt
	if _, e := s.Store.DB.Exec("UPDATE automation_tasks SET payload=?,updated_at=? WHERE id=?", taskJSON(task), task.UpdatedAt, task.ID); e != nil {
		t.Fatal(e)
	}
	s.checkExternalCadence(time.Now().Unix())
	s.checkExternalCadence(time.Now().Unix())
	runs, e := s.TaskRuns(admin, task.ID)
	if e != nil || len(runs) != 1 || runs[0].ErrorCode != "external_trigger_overdue" || runs[0].BatchID != "" || remote.moves != 0 {
		t.Fatal(runs, e)
	}
	var n int
	if e = s.Store.DB.QueryRow("SELECT count(*) FROM notification_outbox").Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}
func TestManualMCPTaskNeedsExplicitManualEntry(t *testing.T) {
	s, _, admin, client, task, _ := taskFixture(t, "observe", "manual", "ai")
	task, e := s.SetTaskEnabled(admin, task.ID, task.Version, false)
	if e != nil {
		t.Fatal(e)
	}
	in := TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "manual-mcp", Manual: true}
	if _, e = s.RunTask(client, in, "external"); !errors.Is(e, ErrDenied) {
		t.Fatal(e)
	}
	r, e := s.RunTask(client, in, "manual")
	if e != nil || r.State != "awaiting_ai" || r.Source != "manual" {
		t.Fatal(r, e)
	}
}

func TestCancelledTaskRetainsUnknownMailOutcome(t *testing.T) {
	s, remote, admin, client, task, m := taskFixture(t, "execute", "external", "ai")
	configureNotification(t, s, admin, "failures")
	r, e := s.RunTask(client, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "cancel-unknown"}, "external")
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.ClaimAI(client, AIClaimInput{AccountID: m.AccountID, RunID: r.ID, RunKey: "claim"})
	if e != nil {
		t.Fatal(e)
	}
	b, e = s.SubmitAI(context.Background(), client, AISubmitInput{BatchID: b.ID, IdempotencyKey: "submit", Decisions: decisionFor(b)})
	if e != nil {
		t.Fatal(e)
	}
	op, e := s.ExecuteAI(client, b.ID, b.PlanIDs[0])
	if e != nil {
		t.Fatal(e)
	}
	// A remote write has settled without a definitive response when cancellation arrives.
	if _, e = s.Store.DB.Exec("UPDATE operation_items SET state='needs_verification' WHERE operation_id=?", op.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Store.DB.Exec("UPDATE operations SET state='needs_verification' WHERE id=?", op.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.CancelTaskRun(client, r.ID); e != nil {
		t.Fatal(e)
	}
	s.reconcileTaskRun(r.ID)
	r, e = s.TaskRun(admin, r.ID)
	if e != nil || r.State != "needs_verification" || r.Unknown != 1 || !r.Finalized || remote.moves != 0 {
		t.Fatal(r, e)
	}
	deliveries, e := s.NotificationDeliveries(admin)
	if e != nil || len(deliveries) != 1 {
		t.Fatal(deliveries, e)
	}
	if _, e = s.Store.DB.Exec("UPDATE operation_items SET state='completed' WHERE operation_id=?; UPDATE operations SET state='completed' WHERE id=?", op.ID, op.ID); e != nil {
		t.Fatal(e)
	}
	s.reconcileTaskRun(r.ID)
	r, e = s.TaskRun(admin, r.ID)
	if e != nil || r.State != "cancelled" || r.Unknown != 0 || r.ErrorCode != "" {
		t.Fatal("verified task retained an obsolete error", r, e)
	}
}
