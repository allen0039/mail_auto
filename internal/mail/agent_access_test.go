package mail

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// bindAgent creates a grant and an active binding row the way the Telegram
// binding flow does, returning the resolved agent access.
func bindAgent(t *testing.T, s *Service, admin Access, in AgentGrantInput) (Access, AgentGrant) {
	t.Helper()
	g, err := s.SaveAgentGrant(admin, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.DB.Exec("INSERT INTO telegram_bindings(id,bot_id,user_id,chat_id,grant_id,admin_version,epoch,state,created_at) VALUES('binding',1,42,42,?,?,1,'active',?)", g.ID, admin.Version, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.AgentIdentity(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a, g
}

func TestAgentIdentityNeedsPersistedGrantAndBinding(t *testing.T) {
	s, _, admin, _, task, m := taskFixture(t, "execute", "manual", "rules")
	forged := Access{Owner: AgentOwnerPrefix + "made-up", Version: admin.Version, Accounts: []string{m.AccountID}, Folders: map[string][]string{m.AccountID: {}}, Actions: AgentActions}
	if _, err := s.CheckAccess(forged); !errors.Is(err, ErrDenied) {
		t.Fatal("forged agent identity accepted", err)
	}
	g, err := s.SaveAgentGrant(admin, AgentGrantInput{Enabled: true, Accounts: []string{m.AccountID}, Actions: []string{"read", "task_manage", "automation"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CheckAccess(Access{Owner: AgentOwnerPrefix + g.ID, Version: admin.Version}); !errors.Is(err, ErrDenied) {
		t.Fatal("grant without binding accepted", err)
	}
	if _, err = s.Store.DB.Exec("INSERT INTO telegram_bindings(id,bot_id,user_id,chat_id,grant_id,admin_version,epoch,state,created_at) VALUES('b1',1,42,42,?,?,1,'active',?)", g.ID, admin.Version, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.AgentIdentity(g.ID)
	if err != nil || a.Allows(m.AccountID, "INBOX", "rule_manage") || !a.Allows(m.AccountID, "INBOX", "read") {
		t.Fatal("grant scope not applied", a, err)
	}
	// Supplied scope fields are ignored: access is recomputed from storage.
	widened := a
	widened.Actions = AgentActions
	checked, err := s.CheckAccess(widened)
	if err != nil || slices.Contains(checked.Actions, "rule_manage") {
		t.Fatal("caller-supplied actions widened access", checked, err)
	}
	if _, err = s.Store.DB.Exec("INSERT INTO agent_proposals(id,conversation_id,binding_id,token_hash,kind,state,state_digest,grant_id,grant_version,admin_version,intent_epoch,context_epoch,stop_epoch,expires,created_at,updated_at) VALUES('p','c','b1','h','workflow','pending','d',?,1,?,1,1,0,?,1,1)", g.ID, admin.Version, time.Now().Unix()+600); err != nil {
		t.Fatal(err)
	}
	// Narrowing the grant invalidates pending proposals and narrows access.
	g, err = s.SaveAgentGrant(admin, AgentGrantInput{Version: g.Version, Enabled: true, Accounts: []string{m.AccountID}, Folders: map[string][]string{m.AccountID: {"Archive"}}, Actions: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	var state string
	if err = s.Store.DB.QueryRow("SELECT state FROM agent_proposals WHERE id='p'").Scan(&state); err != nil || state != "invalidated" {
		t.Fatal("proposal survived grant change", state, err)
	}
	if _, err = s.Task(a, task.ID); !errors.Is(err, ErrDenied) {
		t.Fatal("narrowed grant still sees INBOX task", err)
	}
	// Administrator credential change (version bump) blocks the old identity.
	if _, err = s.Store.DB.Exec("UPDATE admin SET version=version+1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CheckAccess(a); !errors.Is(err, ErrDenied) {
		t.Fatal("stale admin version accepted", err)
	}
	if _, err = s.Store.DB.Exec("UPDATE admin SET version=version-1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeAgentGrant(admin); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CheckAccess(a); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked grant accepted", err)
	}
	var bindingState string
	if err = s.Store.DB.QueryRow("SELECT state FROM telegram_bindings WHERE id='b1'").Scan(&bindingState); err != nil || bindingState != "revoked" {
		t.Fatal("binding survived revocation", bindingState, err)
	}
}

func TestAgentGrantExpiryAndInvalidInput(t *testing.T) {
	s, _, admin, _, _, m := taskFixture(t, "execute", "manual", "rules")
	for _, in := range []AgentGrantInput{
		{Accounts: []string{m.AccountID}, Actions: []string{"read", "mail_execute"}},
		{Accounts: []string{m.AccountID}, Actions: []string{"task_manage"}},
		{Accounts: []string{m.AccountID}, Actions: []string{"read"}, Days: 91},
		{Accounts: []string{"missing"}, Actions: []string{"read"}},
		{Accounts: []string{m.AccountID}, Actions: []string{"read"}, Folders: map[string][]string{m.AccountID: {"NoSuchFolder"}}},
	} {
		if _, err := s.SaveAgentGrant(admin, in); !errors.Is(err, ErrInput) {
			t.Fatal("invalid grant accepted", in, err)
		}
	}
	a, g := bindAgent(t, s, admin, AgentGrantInput{Enabled: true, Accounts: []string{m.AccountID}, Actions: []string{"read"}})
	if _, err := s.SaveAgentGrant(admin, AgentGrantInput{Version: g.Version - 1, Enabled: true, Accounts: []string{m.AccountID}, Actions: []string{"read"}}); !errors.Is(err, ErrConflict) {
		t.Fatal("stale grant version accepted", err)
	}
	if _, err := s.Store.DB.Exec("UPDATE agent_grants SET expires=? WHERE id=?", time.Now().Unix()-1, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckAccess(a); !errors.Is(err, ErrDenied) {
		t.Fatal("expired grant accepted", err)
	}
}

func TestAgentCrossOwnerRunsAreScopedAndMCPIsolationUnchanged(t *testing.T) {
	s, _, admin, client, task, m := taskFixture(t, "observe", "internal", "rules")
	run, err := s.createTaskRun(Access{Owner: task.ServiceOwner, Version: task.AdminVersion}, task, "schedule-test", "internal", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	// A regular MCP token with task_manage still cannot see the svc: run.
	if _, err = s.TaskRun(client, run.ID); !errors.Is(err, ErrDenied) {
		t.Fatal("MCP token crossed owner", err)
	}
	limited, g := bindAgent(t, s, admin, AgentGrantInput{Enabled: true, Accounts: []string{m.AccountID}, Actions: []string{"read", "task_manage"}})
	if _, err = s.AgentTaskRun(limited, run.ID); !errors.Is(err, ErrDenied) {
		t.Fatal("agent without task_run_manage crossed owner", err)
	}
	if runs, err := s.AgentTaskRuns(limited, task.ID); err != nil || len(runs) != 0 {
		t.Fatal("owner isolation lost without task_run_manage", runs, err)
	}
	if _, err = s.AgentCancelTaskRun(limited, run.ID); !errors.Is(err, ErrDenied) {
		t.Fatal("agent cancelled foreign run without grant", err)
	}
	g, err = s.SaveAgentGrant(admin, AgentGrantInput{Version: g.Version, Enabled: true, Accounts: []string{m.AccountID}, Actions: []string{"read", "task_manage", "task_run_manage"}})
	if err != nil {
		t.Fatal(err)
	}
	full, _, err := s.AgentIdentity(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentTaskRun(full, run.ID)
	if err != nil || got.ID != run.ID {
		t.Fatal(got, err)
	}
	if runs, err := s.AgentTaskRuns(full, task.ID); err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	cancelled, err := s.AgentCancelTaskRun(full, run.ID)
	if err != nil || cancelled.State != "cancelled" {
		t.Fatal(cancelled, err)
	}
	stored, err := s.loadTaskRun(run.ID)
	if err != nil || stored.Owner != task.ServiceOwner || stored.State != "cancelled" {
		t.Fatal("cancel changed owner or failed", stored, err)
	}
	var actor string
	if err = s.Store.DB.QueryRow("SELECT outcome FROM audits WHERE action='agent.task_run.cancel' AND target=?", run.ID).Scan(&actor); err != nil || actor != full.Owner {
		t.Fatal("audit does not record real operator", actor, err)
	}
	// Folder narrowing hides the task and its runs.
	g, err = s.SaveAgentGrant(admin, AgentGrantInput{Version: g.Version, Enabled: true, Accounts: []string{m.AccountID}, Folders: map[string][]string{m.AccountID: {"Archive"}}, Actions: []string{"read", "task_run_manage"}})
	if err != nil {
		t.Fatal(err)
	}
	narrow, _, _ := s.AgentIdentity(g.ID)
	if _, err = s.AgentTaskRun(narrow, run.ID); !errors.Is(err, ErrDenied) {
		t.Fatal("run visible outside folder scope", err)
	}
}

func TestAgentCannotStartAIRuns(t *testing.T) {
	s, _, admin, _, task, m := taskFixture(t, "observe", "external", "ai")
	a, _ := bindAgent(t, s, admin, AgentGrantInput{Enabled: true, Accounts: []string{m.AccountID}, Actions: []string{"read", "task_manage", "automation"}})
	if _, err := s.RunTask(a, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "agent:x"}, "manual"); !errors.Is(err, ErrCapability) {
		t.Fatal("agent started awaiting_ai run", err)
	}
	task.Name = "renamed"
	if _, err := s.SaveTask(a, task); !errors.Is(err, ErrCapability) {
		t.Fatal("agent modified AI task", err)
	}
}

func TestMutationIdempotencyForRulesAndTasks(t *testing.T) {
	s, _, admin, _, _, m := taskFixture(t, "execute", "manual", "rules")
	a, _ := bindAgent(t, s, admin, AgentGrantInput{Enabled: true, Accounts: []string{m.AccountID}, Actions: []string{"read", "rule_manage", "task_manage", "automation"}})
	draft := Rule{AccountID: m.AccountID, Name: "测试发件人", Priority: 50, SourceFolders: []string{"INBOX"}, Match: RuleGroup{Mode: "all", Conditions: []RuleCondition{{Field: "sender", Operator: "contains", Value: "test@example.com"}}}, Action: "move", Destination: "Archive"}
	mu := Mutation{ID: "proposal-1:0", Kind: "rule.save"}
	var wg sync.WaitGroup
	results := make([]Rule, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.SaveRuleWithMutation(a, draft, mu)
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].ID != results[0].ID {
			t.Fatal("duplicate mutation created another rule", results, errs)
		}
	}
	var n int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM mail_rules WHERE json_extract(payload,'$.name')='测试发件人'").Scan(&n); err != nil || n != 1 {
		t.Fatal("rule count", n, err)
	}
	changed := draft
	changed.Name = "另一个"
	if _, err := s.SaveRuleWithMutation(a, changed, mu); !errors.Is(err, ErrConflict) {
		t.Fatal("same mutation with different input accepted", err)
	}
	task := Task{AccountID: m.AccountID, Name: "Agent 草稿", SourceFolders: []string{"INBOX"}}
	tm := Mutation{ID: "proposal-1:1", Kind: "task.save"}
	first, err := s.SaveTaskWithMutation(a, task, tm)
	if err != nil || first.Enabled || first.Mode != "observe" {
		t.Fatal(first, err)
	}
	again, err := s.SaveTaskWithMutation(a, task, tm)
	if err != nil || again.ID != first.ID || again.Version != first.Version {
		t.Fatal("task mutation not idempotent", again, err)
	}
	pm := Mutation{ID: "proposal-1:2", Kind: "task.enabled"}
	paused, err := s.SetTaskEnabledWithMutation(a, first.ID, first.Version, false, pm)
	if err != nil {
		t.Fatal(err)
	}
	pausedAgain, err := s.SetTaskEnabledWithMutation(a, first.ID, first.Version, false, pm)
	if err != nil || pausedAgain.Version != paused.Version {
		t.Fatal("pause mutation not idempotent", pausedAgain, err)
	}
	if target, version, ok, err := s.MutationTarget(pm.ID); err != nil || !ok || target != first.ID || version != paused.Version {
		t.Fatal(target, version, ok, err)
	}
}

func TestDraftWorkflowPreviewDoesNotEnableAndIncludesClosure(t *testing.T) {
	s, remote, admin, _, task, m := taskFixture(t, "execute", "manual", "rules")
	a, _ := bindAgent(t, s, admin, AgentGrantInput{Enabled: true, Accounts: []string{m.AccountID}, Actions: []string{"read", "rule_manage", "task_manage", "automation"}})
	draft, err := s.SaveRule(a, Rule{AccountID: m.AccountID, Name: "草稿", Priority: 100, SourceFolders: []string{"INBOX"}, Match: RuleGroup{Mode: "all"}, Action: "trash"})
	if err != nil || draft.Enabled {
		t.Fatal(draft, err)
	}
	impacts, hidden, err := s.RuleImpact(a, Rule{ID: draft.ID, AccountID: m.AccountID, Priority: 100, Enabled: true, SourceFolders: []string{"INBOX"}})
	if err != nil || hidden != 0 || len(impacts) != 1 || impacts[0].TaskID != task.ID {
		t.Fatal("follow-all task not reported as impacted", impacts, hidden, err)
	}
	before, _ := s.AccountStateDigest(m.AccountID)
	preview, normalized, err := s.PreviewDraftWorkflow(a, DraftWorkflow{RuleID: draft.ID, RuleVersion: draft.Version, Task: Task{AccountID: m.AccountID, Name: "新任务", SourceFolders: []string{"INBOX"}}})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Scanned != 1 || preview.MaxBatch != 20 {
		t.Fatal(preview)
	}
	// The higher-priority enabled fixture rule (priority 10) is in the closure
	// and wins the evaluation; the draft cannot skip it.
	if len(normalized.RuleIDs) != 2 || preview.Items[0].RuleID == draft.ID {
		t.Fatal("closure missing higher-priority rule", normalized.RuleIDs, preview.Items)
	}
	after, _ := s.AccountStateDigest(m.AccountID)
	stored, _ := s.Rule(admin, draft.ID)
	if before != after || stored.Enabled || remote.moves != 0 {
		t.Fatal("draft preview changed state", before != after, stored.Enabled, remote.moves)
	}
	var tasks int
	if err = s.Store.DB.QueryRow("SELECT count(*) FROM automation_tasks").Scan(&tasks); err != nil || tasks != 1 {
		t.Fatal("draft preview created a task", tasks, err)
	}
	_ = context.Background()
}
