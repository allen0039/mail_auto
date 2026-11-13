package mailtools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"mail-auto/internal/agenttest"
	"mail-auto/internal/mail"
)

func invoke(t *testing.T, r *Registry, c Call, name string, args any) (Result, map[string]any, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	canonical, err := r.Canonical(name, string(raw))
	if err != nil {
		return Result{}, nil, err
	}
	res, encoded, err := r.Invoke(context.Background(), c, name, canonical)
	var out map[string]any
	_ = json.Unmarshal(encoded, &out)
	return res, out, err
}

func TestRegistryWhitelistAndStrictArguments(t *testing.T) {
	r := Agent()
	allowed := "list_accounts list_folders get_policy list_rules get_rule list_tasks get_task list_task_runs get_task_run preview_task save_rule_draft save_task_draft propose_workflow propose_rule_change propose_task_change run_task pause_task cancel_task_run"
	if len(r.Tools()) != len(strings.Fields(allowed)) {
		t.Fatal("unexpected tool count", len(r.Tools()))
	}
	for _, tool := range r.Tools() {
		if !strings.Contains(" "+allowed+" ", " "+tool.Name+" ") {
			t.Fatal("tool outside first-version whitelist", tool.Name)
		}
		var schema map[string]any
		if json.Unmarshal(tool.Schema, &schema) != nil || schema["additionalProperties"] != false {
			t.Fatal("schema must reject additional properties", tool.Name)
		}
	}
	f := agenttest.New(t)
	a, _ := f.Grant(t, "read")
	c := Call{Service: f.Service, Access: a, ActionID: "a1"}
	for name, raw := range map[string]string{
		"unknown tool":  `{"x":1}`,
		"extra field":   `{"account_id":"x","owner":"admin"}`,
		"trailing JSON": `{"account_id":"x"} {"account_id":"y"}`,
		"array":         `[1,2]`,
		"deep nesting":  `{"a":[[[[[[[[[[1]]]]]]]]]]}`,
		"truncated":     `{"account_id":"x"`,
	} {
		tool := "list_rules"
		if name == "unknown tool" {
			tool = "mail_execute"
		}
		canonical, err := r.Canonical(tool, raw)
		if err == nil {
			_, _, err = r.Invoke(context.Background(), c, tool, canonical)
		}
		if err == nil {
			t.Fatal(name, "accepted")
		}
	}
	// Enum and type errors are rejected by the handler's strict decoding.
	if _, _, err := invoke(t, r, c, "list_rules", map[string]any{"account_id": 5}); err == nil {
		t.Fatal("wrong type accepted")
	}
	_, out, err := invoke(t, r, c, "list_accounts", map[string]any{})
	if err != nil || len(out["accounts"].([]any)) != 1 {
		t.Fatal(out, err)
	}
}

func TestDraftsNeverEnableAndReadOnlyGrantCannotWrite(t *testing.T) {
	f := agenttest.New(t)
	r := Agent()
	readOnly, g := f.Grant(t, "read")
	c := Call{Service: f.Service, Access: readOnly, ActionID: "draft-1"}
	rule := map[string]any{"account_id": f.Account.ID, "rule": map[string]any{"name": "测试发件人到回收站", "source_folders": []string{"INBOX"}, "match": map[string]any{"mode": "all", "conditions": []any{map[string]any{"field": "sender", "operator": "contains", "value": "test@example.com"}, map[string]any{"field": "received_days", "operator": "gte", "value": "30"}}}, "action": "trash"}}
	if _, _, err := invoke(t, r, c, "save_rule_draft", rule); !errors.Is(err, mail.ErrDenied) {
		t.Fatal("read-only grant saved a rule", err)
	}
	var err error
	g, err = f.Service.SaveAgentGrant(f.Admin, mail.AgentGrantInput{Version: g.Version, Enabled: true, Accounts: []string{f.Account.ID}, Actions: []string{"read", "rule_manage", "task_manage", "automation"}})
	if err != nil {
		t.Fatal(err)
	}
	full, _, err := f.Service.AgentIdentity(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.Access = full
	_, out, err := invoke(t, r, c, "save_rule_draft", rule)
	if err != nil {
		t.Fatal(err)
	}
	saved := out["rule"].(map[string]any)
	if saved["enabled"] != false || out["state"] != "draft_disabled" {
		t.Fatal(out)
	}
	// Retrying the same call (same action ID) returns the same rule.
	_, again, err := invoke(t, r, c, "save_rule_draft", rule)
	if err != nil || again["rule"].(map[string]any)["id"] != saved["id"] {
		t.Fatal("draft retry created another rule", again, err)
	}
	// A new user instruction with the same name hits the existing draft.
	c.ActionID = "draft-2"
	if _, _, err := invoke(t, r, c, "save_rule_draft", rule); err == nil || err.(*Error).Code != "draft_exists" {
		t.Fatal("duplicate draft created", err)
	}
	// AI rules are not offered by the first version.
	aiRule := map[string]any{"account_id": f.Account.ID, "rule": map[string]any{"name": "AI", "source_folders": []string{"INBOX"}, "match": map[string]any{"mode": "all", "conditions": []any{}}, "action": "ai"}}
	if _, _, err := invoke(t, r, c, "save_rule_draft", aiRule); err == nil {
		t.Fatal("AI rule accepted")
	}
	// A task draft cannot bind the disabled rule directly.
	c.ActionID = "task-1"
	task := map[string]any{"account_id": f.Account.ID, "task": map[string]any{"name": "回收测试", "source_folders": []string{"INBOX"}, "rule_ids": []string{saved["id"].(string)}}}
	if _, _, err := invoke(t, r, c, "save_task_draft", task); err == nil || err.(*Error).Code != "rule_disabled" {
		t.Fatal("task bound a disabled rule", err)
	}
	// Drafts cannot request execute or enabled.
	task["task"].(map[string]any)["follow_all_rules"] = true
	delete(task["task"].(map[string]any), "rule_ids")
	task["task"].(map[string]any)["mode"] = "execute"
	if _, _, err := invoke(t, r, c, "save_task_draft", task); err == nil {
		t.Fatal("draft accepted mode")
	}
}

func TestWorkflowProposalAppliesInOrderAndIsIdempotent(t *testing.T) {
	f := agenttest.New(t)
	r := Agent()
	a, _ := f.Grant(t, "read", "rule_manage", "task_manage", "automation")
	c := Call{Service: f.Service, Access: a, ActionID: "wf-1"}
	_, out, err := invoke(t, r, c, "save_rule_draft", map[string]any{"account_id": f.Account.ID, "rule": map[string]any{"name": "测试发件人", "priority": 50, "source_folders": []string{"INBOX"}, "match": map[string]any{"mode": "all", "conditions": []any{map[string]any{"field": "sender", "operator": "contains", "value": "test@example.com"}}}, "action": "trash"}})
	if err != nil {
		t.Fatal(err)
	}
	rule := out["rule"].(map[string]any)
	digestBefore, _ := f.Service.AccountStateDigest(f.Account.ID)
	c.ActionID = "wf-2"
	res, out, err := invoke(t, r, c, "propose_workflow", map[string]any{"rule_id": rule["id"], "rule_version": rule["version"], "mode": "execute", "enable_task": true, "task": map[string]any{"name": "每周五清理", "trigger_mode": "internal", "schedule": map[string]any{"frequency": "weekly", "weekday": 5, "hour": 20, "minute": 0}}})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Proposal
	if p == nil || out["status"] != "confirmation_required" || len(p.Steps) != 4 {
		t.Fatal("expected 4-step proposal", out, p)
	}
	preview := out["preview"].(map[string]any)
	if preview["counts_this_call"].(map[string]any)["matched"] != float64(2) {
		t.Fatal("draft preview counts", preview)
	}
	if !strings.Contains(p.Summary, "未来 5 次") || !strings.Contains(p.Summary, "每批最多 20 封") || !strings.Contains(p.Summary, "无每日处理额度") {
		t.Fatal("summary misses effective values", p.Summary)
	}
	// Proposing wrote nothing effective.
	digestAfter, _ := f.Service.AccountStateDigest(f.Account.ID)
	if digestBefore != digestAfter {
		t.Fatal("proposal changed configuration")
	}
	// Apply all steps; replaying every step afterwards writes nothing new.
	results := []StepResult{}
	for i := range p.Steps {
		sr, err := ApplyStep(context.Background(), f.Service, a, "prop-1", p.Steps, i, results)
		if err != nil {
			t.Fatal(i, err)
		}
		results = append(results, sr)
	}
	task, err := f.Service.Task(f.Admin, results[1].TargetID)
	if err != nil || !task.Enabled || task.Mode != "execute" || task.Trigger != "internal" || len(task.RuleIDs) != 1 || task.MaxBatch != 20 {
		t.Fatal("final task state", task, err)
	}
	enabledRule, _ := f.Service.Rule(f.Admin, rule["id"].(string))
	if !enabledRule.Enabled {
		t.Fatal("rule not enabled")
	}
	replay := []StepResult{}
	for i := range p.Steps {
		sr, err := ApplyStep(context.Background(), f.Service, a, "prop-1", p.Steps, i, replay)
		if err != nil || sr.TargetID != results[i].TargetID || sr.Version != results[i].Version {
			t.Fatal("replay diverged", i, sr, err)
		}
		replay = append(replay, sr)
	}
	var tasks, rules int
	_ = f.Store.DB.QueryRow("SELECT count(*) FROM automation_tasks").Scan(&tasks)
	_ = f.Store.DB.QueryRow("SELECT count(*) FROM mail_rules").Scan(&rules)
	if tasks != 1 || rules != 1 {
		t.Fatal("replay created duplicates", tasks, rules)
	}
}

func TestPartialWorkflowStopsOnConflictWithoutRollback(t *testing.T) {
	f := agenttest.New(t)
	r := Agent()
	a, _ := f.Grant(t, "read", "rule_manage", "task_manage", "automation")
	c := Call{Service: f.Service, Access: a, ActionID: "p-1"}
	_, out, err := invoke(t, r, c, "save_rule_draft", map[string]any{"account_id": f.Account.ID, "rule": map[string]any{"name": "草稿", "source_folders": []string{"INBOX"}, "match": map[string]any{"mode": "all", "conditions": []any{}}, "action": "trash"}})
	if err != nil {
		t.Fatal(err)
	}
	rule := out["rule"].(map[string]any)
	c.ActionID = "p-2"
	res, _, err := invoke(t, r, c, "propose_workflow", map[string]any{"rule_id": rule["id"], "rule_version": rule["version"], "mode": "observe", "enable_task": false, "task": map[string]any{"name": "观察"}})
	if err != nil {
		t.Fatal(err)
	}
	steps := res.Proposal.Steps
	first, err := ApplyStep(context.Background(), f.Service, a, "prop-2", steps, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Someone edits the rule between steps: the task step must conflict
	// rather than bind a version the user did not confirm.
	edited := *first.Rule
	edited.Name = "被他人修改"
	if _, err = f.Service.SaveRule(f.Admin, edited); err != nil {
		t.Fatal(err)
	}
	if _, err = ApplyStep(context.Background(), f.Service, a, "prop-2", steps, 1, []StepResult{first}); !errors.Is(err, mail.ErrConflict) {
		t.Fatal("stale binding accepted", err)
	}
	stored, _ := f.Service.Rule(f.Admin, first.TargetID)
	if !stored.Enabled {
		t.Fatal("completed step was rolled back")
	}
}

func TestTaskPatchKeepsUnspecifiedFieldsAndNoDailyQuota(t *testing.T) {
	f := agenttest.New(t)
	r := Agent()
	a, _ := f.Grant(t, "read", "task_manage", "automation")
	original, err := f.Service.SaveTask(f.Admin, mail.Task{AccountID: f.Account.ID, Name: "既有任务", SourceFolders: []string{"INBOX"}, MaxBatch: 7, ProtectUnread: true, ProtectFlagged: true})
	if err != nil {
		t.Fatal(err)
	}
	c := Call{Service: f.Service, Access: a, ActionID: "patch"}
	res, out, err := invoke(t, r, c, "propose_task_change", map[string]any{"task_id": original.ID, "version": original.Version, "task": map[string]any{"name": "改名"}})
	if err != nil {
		t.Fatal(err)
	}
	next := res.Proposal.Steps[0].Task
	if next.MaxBatch != 7 || !next.ProtectUnread || !next.ProtectFlagged || next.Since != original.Since || next.Mode != "observe" || next.Enabled {
		t.Fatal("patch dropped existing fields", next)
	}
	if !strings.Contains(out["summary"].(string), "每批最多 7 封") || strings.Contains(out["summary"].(string), "每日上限") {
		t.Fatal(out["summary"])
	}
	// A stale version is a conflict, not a silent overwrite.
	if _, _, err := invoke(t, r, c, "propose_task_change", map[string]any{"task_id": original.ID, "version": original.Version - 1, "task": map[string]any{"name": "x"}}); !errors.Is(err, mail.ErrConflict) {
		t.Fatal(err)
	}
	// Running a paused task needs confirmation; it is not queued directly.
	res, out, err = invoke(t, r, c, "run_task", map[string]any{"task_id": original.ID})
	if err != nil || res.Proposal == nil || out["status"] != "confirmation_required" {
		t.Fatal("paused task ran without confirmation", out, err)
	}
	var runs int
	_ = f.Store.DB.QueryRow("SELECT count(*) FROM task_runs").Scan(&runs)
	if runs != 0 {
		t.Fatal("run created before confirmation")
	}
}
