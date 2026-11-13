package mailtools

import (
	"context"
	"fmt"
	"strings"

	"mail-auto/internal/mail"
)

const groupSchema = `{"type":"object","properties":{"mode":{"type":"string","enum":["all","any","not"]},"conditions":{"type":"array","items":{"type":"object","properties":{"field":{"type":"string","enum":["subject","folder","sender","domain","to","cc","recipient","list_id","read_state","flagged","has_attachment","is_reply","received_days","size"]},"operator":{"type":"string"},"value":{"type":"string"}},"required":["field","operator","value"],"additionalProperties":false}},"groups":{"type":"array","items":{"type":"object"}}},"required":["mode","conditions"],"additionalProperties":false}`

const ruleFieldsSchema = `{"type":"object","properties":{"name":{"type":"string"},"priority":{"type":"integer","minimum":0,"maximum":10000},"source_folders":{"type":"array","items":{"type":"string"}},"match":` + groupSchema + `,"action":{"type":"string","enum":["move","archive","trash","keep","mark_read","mark_unread","flag","unflag"]},"destination":{"type":"string"},"also":{"type":"array","items":{"type":"string"}}},"additionalProperties":false}`

const scheduleSchema = `{"type":"object","properties":{"frequency":{"type":"string","enum":["hourly","daily","weekly","interval","cron","on_new_mail"]},"hour":{"type":"integer"},"minute":{"type":"integer"},"weekday":{"type":"integer","description":"0=Sunday..6=Saturday"},"timezone":{"type":"string"},"every_minutes":{"type":"integer"},"cron":{"type":"string"}},"required":["frequency"],"additionalProperties":false}`

const taskFieldsBase = `"name":{"type":"string"},"source_folders":{"type":"array","items":{"type":"string"}},"rule_ids":{"type":"array","items":{"type":"string"}},"follow_all_rules":{"type":"boolean"},"trigger_mode":{"type":"string","enum":["manual","internal"]},"schedule":` + scheduleSchema + `,"max_batch":{"type":"integer","minimum":1,"maximum":50},"protect_unread":{"type":"boolean"},"protect_flagged":{"type":"boolean"},"notify":{"type":"string","enum":["inherit","off","failures","changes","all"]}`

type ruleDraftIn struct {
	AccountID string     `json:"account_id"`
	RuleID    string     `json:"rule_id,omitempty"`
	Version   int        `json:"version,omitempty"`
	Rule      ruleFields `json:"rule"`
}
type taskDraftIn struct {
	AccountID string     `json:"account_id"`
	TaskID    string     `json:"task_id,omitempty"`
	Version   int        `json:"version,omitempty"`
	Task      taskFields `json:"task"`
}
type workflowIn struct {
	RuleID      string     `json:"rule_id"`
	RuleVersion int        `json:"rule_version"`
	Task        taskFields `json:"task"`
	Mode        string     `json:"mode"`
	EnableTask  bool       `json:"enable_task"`
}
type ruleChangeIn struct {
	RuleID  string     `json:"rule_id"`
	Version int        `json:"version"`
	Rule    ruleFields `json:"rule"`
	Enabled *bool      `json:"enabled,omitempty"`
}
type taskChangeIn struct {
	TaskID  string     `json:"task_id"`
	Version int        `json:"version"`
	Task    taskFields `json:"task"`
}

func draftTools(r *Registry) {
	def(r, "save_rule_draft", RiskDraft, true, "Create a DISABLED rule draft, or update an existing disabled draft (rule_id+version) with a partial patch. Never enables a rule. Ask the user for missing sender address or folder instead of guessing.", `{"type":"object","properties":{"account_id":{"type":"string"},"rule_id":{"type":"string"},"version":{"type":"integer"},"rule":`+ruleFieldsSchema+`},"required":["account_id","rule"],"additionalProperties":false}`, func(ctx context.Context, c Call, in ruleDraftIn) (Result, error) {
		var rule mail.Rule
		if in.RuleID != "" {
			old, err := c.Service.Rule(c.Access, in.RuleID)
			if err != nil {
				return Result{}, err
			}
			if old.Enabled {
				return Result{}, fail("rule_enabled", "该规则已启用，修改需使用 propose_rule_change 生成确认方案")
			}
			if old.AccountID != in.AccountID || old.Version != in.Version {
				return Result{}, mail.ErrConflict
			}
			rule = old
		} else {
			if in.Version != 0 || in.Rule.Name == nil || in.Rule.SourceFolders == nil || in.Rule.Match == nil || in.Rule.Action == nil {
				return Result{}, fail("invalid_input", "新建草稿需要 name、source_folders、match、action")
			}
			existing, err := c.Service.Rules(c.Access, in.AccountID)
			if err != nil {
				return Result{}, err
			}
			// A retry of this same call returns its recorded result below; the
			// duplicate-name guard applies only to a new instruction.
			_, _, done, err := c.Service.MutationTarget("action:" + c.ActionID)
			if err != nil {
				return Result{}, err
			}
			for _, x := range existing {
				if !done && !x.Enabled && x.Name == trimName(*in.Rule.Name) {
					return Result{}, fail("draft_exists", fmt.Sprintf("已有同名禁用草稿 %s（版本 %d），请用 rule_id 更新它", x.ID, x.Version))
				}
			}
			rule = mail.Rule{AccountID: in.AccountID, Priority: 100}
		}
		if err := in.Rule.apply(&rule); err != nil {
			return Result{}, err
		}
		rule.Enabled = false
		saved, err := c.Service.SaveRuleWithMutation(c.Access, rule, mail.Mutation{ID: "action:" + c.ActionID, Kind: "rule.save"})
		if err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"rule": ruleView(saved), "state": "draft_disabled", "note": "规则草稿已保存为禁用状态，未影响任何任务"}}, nil
	})
	def(r, "save_task_draft", RiskDraft, true, "Create a PAUSED OBSERVE task bound to already-enabled rules (rule_ids, closure of higher-priority rules is added automatically) or, only if the user explicitly asks, follow_all_rules=true. Or update an existing paused observe task (task_id+version) with a partial patch. Mode/enabled changes need propose_task_change. For a new disabled rule use propose_workflow instead.", `{"type":"object","properties":{"account_id":{"type":"string"},"task_id":{"type":"string"},"version":{"type":"integer"},"task":{"type":"object","properties":{`+taskFieldsBase+`},"additionalProperties":false}},"required":["account_id","task"],"additionalProperties":false}`, func(ctx context.Context, c Call, in taskDraftIn) (Result, error) {
		var t mail.Task
		creating := in.TaskID == ""
		if !creating {
			old, err := c.Service.Task(c.Access, in.TaskID)
			if err != nil {
				return Result{}, err
			}
			if old.Enabled || old.Mode != "observe" {
				return Result{}, fail("task_active", "该任务已启用或处于执行模式，修改需使用 propose_task_change")
			}
			if old.AccountID != in.AccountID || old.Version != in.Version {
				return Result{}, mail.ErrConflict
			}
			if old.Engine != "rules" {
				return Result{}, fail("capability_unavailable", "首版 Agent 不管理 AI 引擎任务")
			}
			t = old
		} else {
			if in.Version != 0 || in.Task.Name == nil || in.Task.SourceFolders == nil {
				return Result{}, fail("invalid_input", "新建任务草稿需要 name、source_folders")
			}
			t = mail.Task{AccountID: in.AccountID, Engine: "rules", Trigger: "manual", Notify: "inherit"}
		}
		if err := in.Task.apply(&t, false); err != nil {
			return Result{}, err
		}
		if err := bindRules(c, &t, in.Task, creating); err != nil {
			return Result{}, err
		}
		t.NextTimes, t.LastState = nil, ""
		saved, err := c.Service.SaveTaskWithMutation(c.Access, t, mail.Mutation{ID: "action:" + c.ActionID, Kind: "task.save"})
		if err != nil {
			return Result{}, err
		}
		p, err := c.Service.EffectivePolicy(c.Access, saved.AccountID)
		if err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"task": taskView(saved, p), "state": "paused_observe", "note": "任务已保存为暂停观察草稿，不会运行；启用或执行需确认方案"}}, nil
	})
}

func proposalTools(r *Registry) {
	def(r, "propose_workflow", RiskProposal, false, "For a NEW disabled rule draft: preview it read-only together with a not-yet-created task, list affected existing tasks, and prepare a confirmation card that will enable the rule, create the task (paused observe), set the requested mode and optionally enable it. Nothing is enabled until the user confirms the card.", `{"type":"object","properties":{"rule_id":{"type":"string"},"rule_version":{"type":"integer"},"task":{"type":"object","properties":{`+taskFieldsBase+`},"additionalProperties":false},"mode":{"type":"string","enum":["observe","execute"]},"enable_task":{"type":"boolean"}},"required":["rule_id","rule_version","task","mode","enable_task"],"additionalProperties":false}`, func(ctx context.Context, c Call, in workflowIn) (Result, error) {
		return proposeWorkflow(c, in)
	})
	def(r, "propose_rule_change", RiskProposal, false, "Prepare a confirmation card to modify and/or enable or disable an existing rule with a partial patch at its current version. Shows the diff and affected tasks. Nothing changes until confirmed.", `{"type":"object","properties":{"rule_id":{"type":"string"},"version":{"type":"integer"},"rule":`+ruleFieldsSchema+`,"enabled":{"type":"boolean"}},"required":["rule_id","version","rule"],"additionalProperties":false}`, func(ctx context.Context, c Call, in ruleChangeIn) (Result, error) {
		return proposeRuleChange(c, in)
	})
	def(r, "propose_task_change", RiskProposal, false, "Prepare a confirmation card to modify an existing task with a partial patch (including mode observe/execute and enabled). Shows old/new effective values and next five times. Nothing changes until confirmed. To pause, use pause_task.", `{"type":"object","properties":{"task_id":{"type":"string"},"version":{"type":"integer"},"task":{"type":"object","properties":{`+taskFieldsBase+`,"mode":{"type":"string","enum":["observe","execute"]},"enabled":{"type":"boolean"}},"additionalProperties":false}},"required":["task_id","version","task"],"additionalProperties":false}`, func(ctx context.Context, c Call, in taskChangeIn) (Result, error) {
		return proposeTaskChange(c, in)
	})
}

func runTools(r *Registry) {
	def(r, "run_task", RiskRun, true, "Run a fixed-rule task once because the user explicitly asked to execute it now. An enabled task at its current version is queued directly; a paused task returns a confirmation card. Returns the real run ID; poll get_task_run for results.", `{"type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false}`, func(ctx context.Context, c Call, in taskIDIn) (Result, error) {
		t, err := c.Service.Task(c.Access, in.TaskID)
		if err != nil {
			return Result{}, err
		}
		if t.Engine != "rules" {
			return Result{}, fail("capability_unavailable", "首版内置 Agent 不触发 AI 引擎任务；请改用固定条件规则任务")
		}
		if !t.Enabled {
			p, err := c.Service.EffectivePolicy(c.Access, t.AccountID)
			if err != nil {
				return Result{}, err
			}
			digest, err := c.Service.AccountStateDigest(t.AccountID)
			if err != nil {
				return Result{}, err
			}
			prop := &Proposal{Kind: "run_task", AccountID: t.AccountID, Title: "执行暂停中的任务一次", Summary: "任务当前为暂停状态。确认后只手动运行一次，不改变暂停状态。\n" + DescribeTask(t, p), Steps: []Step{{Kind: "task_run", TaskID: t.ID, Version: t.Version, FromStep: -1, DraftRuleStep: -1}}, StateDigest: digest}
			return Result{Data: map[string]any{"status": "confirmation_required", "task_id": t.ID}, Proposal: prop}, nil
		}
		run, err := c.Service.RunTask(c.Access, mail.TaskRunInput{TaskID: t.ID, Version: t.Version, RunKey: "agent:" + c.ActionID}, "manual")
		if err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"status": "queued", "run": RunView(run), "note": "已入队；结果以运行记录为准，未知结果不算成功"}}, nil
	})
	def(r, "pause_task", RiskRun, true, "Pause a task immediately at its current version. Already started writes are not rolled back.", `{"type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false}`, func(ctx context.Context, c Call, in taskIDIn) (Result, error) {
		t, err := c.Service.Task(c.Access, in.TaskID)
		if err != nil {
			return Result{}, err
		}
		if !t.Enabled {
			return Result{Data: map[string]any{"status": "already_paused", "task_id": t.ID, "version": t.Version}}, nil
		}
		saved, err := c.Service.SetTaskEnabledWithMutation(c.Access, t.ID, t.Version, false, mail.Mutation{ID: "action:" + c.ActionID, Kind: "task.enabled"})
		if err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"status": "paused", "task_id": saved.ID, "version": saved.Version, "note": "已暂停；已完成的邮件动作保留"}}, nil
	})
	def(r, "cancel_task_run", RiskRun, true, "Stop the not-yet-started items of an active fixed-rule run. Completed moves remain; this is not a rollback.", `{"type":"object","properties":{"run_id":{"type":"string"}},"required":["run_id"],"additionalProperties":false}`, func(ctx context.Context, c Call, in runIDIn) (Result, error) {
		run, err := c.Service.AgentCancelTaskRun(c.Access, in.RunID)
		if err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"status": "stop_requested", "run": RunView(run), "note": "已请求停止剩余项；已完成动作保留，不是回滚"}}, nil
	})
}

func proposeWorkflow(c Call, in workflowIn) (Result, error) {
	draft, err := c.Service.Rule(c.Access, in.RuleID)
	if err != nil {
		return Result{}, err
	}
	if draft.Enabled {
		return Result{}, fail("rule_enabled", "规则已启用：请先用 save_task_draft 保存任务，再用 propose_task_change 启用")
	}
	if draft.Version != in.RuleVersion {
		return Result{}, mail.ErrConflict
	}
	if !hasAction(c.Access, draft.AccountID, "rule_manage") || !hasAction(c.Access, draft.AccountID, "task_manage") {
		return Result{}, mail.ErrDenied
	}
	if in.Mode != "observe" && in.Mode != "execute" {
		return Result{}, fail("invalid_input", "mode 仅支持 observe 或 execute")
	}
	if in.Task.RuleIDs != nil || in.Task.FollowAllRules != nil || in.Task.Mode != nil || in.Task.Enabled != nil {
		return Result{}, fail("invalid_input", "工作流任务自动绑定该草稿规则及更高优先级规则；不要提供 rule_ids、follow_all_rules、mode、enabled")
	}
	t := mail.Task{AccountID: draft.AccountID, Engine: "rules", Trigger: "manual", Notify: "inherit", SourceFolders: draft.SourceFolders, Name: draft.Name}
	if err = in.Task.apply(&t, false); err != nil {
		return Result{}, err
	}
	p, err := c.Service.EffectivePolicy(c.Access, t.AccountID)
	if err != nil {
		return Result{}, err
	}
	if in.Mode == "execute" && p.Mode != "execute" {
		return Result{}, fail("policy_observe_only", "管理员策略当前只允许观察模式；执行模式需先在 Web 调整策略")
	}
	if t.Trigger == "internal" && !p.AllowInternal {
		return Result{}, fail("internal_schedule_not_allowed", "管理员策略未允许内置定时；请在 Web 授权后重试")
	}
	if in.Mode == "execute" && (draft.Action == "move" || draft.Action == "archive" || draft.Action == "trash") && !strings.Contains(","+strings.Join(p.Actions, ",")+",", ","+draft.Action+",") {
		return Result{}, fail("policy_action_not_allowed", "管理员策略未授权该动作（"+draft.Action+"）；请在 Web 授权后重新预览")
	}
	preview, normalized, err := c.Service.PreviewDraftWorkflow(c.Access, mail.DraftWorkflow{RuleID: draft.ID, RuleVersion: draft.Version, Task: t, OnlyMatched: true})
	if err != nil {
		return Result{}, err
	}
	enabled := draft
	enabled.Enabled = true
	impacts, hidden, err := c.Service.RuleImpact(c.Access, enabled)
	if err != nil {
		return Result{}, err
	}
	if err = denyHidden(hidden); err != nil {
		return Result{}, err
	}
	digest, err := c.Service.AccountStateDigest(t.AccountID)
	if err != nil {
		return Result{}, err
	}
	normalized.MaxBatch = preview.MaxBatch
	steps := []Step{{Kind: "rule_save", Rule: &enabled, FromStep: -1, DraftRuleStep: -1}}
	create := normalized
	create.Enabled, create.Mode, create.ID, create.Version = false, "observe", "", 0
	steps = append(steps, Step{Kind: "task_create", Task: &create, FromStep: -1, DraftRuleStep: 0})
	last := 1
	if in.Mode == "execute" {
		steps = append(steps, Step{Kind: "task_save", Mode: "execute", FromStep: last, DraftRuleStep: -1})
		last++
	}
	if in.EnableTask {
		steps = append(steps, Step{Kind: "task_enable", FromStep: last, DraftRuleStep: -1})
	}
	final := normalized
	final.Mode, final.Enabled = in.Mode, in.EnableTask
	lines := []string{"启用规则：" + DescribeRule(enabled), "创建任务：" + DescribeTask(final, p)}
	if nt := NextTimesText(final); nt != "" {
		lines = append(lines, nt)
	}
	lines = append(lines, previewText(preview), impactText(impacts), "执行顺序：启用规则 → 创建暂停观察任务"+map[bool]string{true: " → 切换执行模式", false: ""}[in.Mode == "execute"]+map[bool]string{true: " → 启用任务", false: ""}[in.EnableTask])
	prop := &Proposal{Kind: "workflow", AccountID: t.AccountID, Title: "启用新规则并创建任务", Summary: strings.Join(lines, "\n"), Steps: steps, StateDigest: digest}
	return Result{Data: map[string]any{"status": "confirmation_required", "preview": PreviewView(preview), "affected_tasks": impacts, "summary": prop.Summary}, Proposal: prop}, nil
}

func proposeRuleChange(c Call, in ruleChangeIn) (Result, error) {
	old, err := c.Service.Rule(c.Access, in.RuleID)
	if err != nil {
		return Result{}, err
	}
	if old.Version != in.Version {
		return Result{}, mail.ErrConflict
	}
	if !hasAction(c.Access, old.AccountID, "rule_manage") {
		return Result{}, mail.ErrDenied
	}
	next := old
	if err = in.Rule.apply(&next); err != nil {
		return Result{}, err
	}
	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	if !old.Enabled && !next.Enabled {
		return Result{}, fail("use_draft", "禁用规则的修改请直接用 save_rule_draft")
	}
	impacts, hidden, err := c.Service.RuleImpact(c.Access, next)
	if err != nil {
		return Result{}, err
	}
	if old.Enabled {
		// Changing or disabling an effective rule also affects tasks that
		// evaluate the current form.
		more, moreHidden, err := c.Service.RuleImpact(c.Access, old)
		if err != nil {
			return Result{}, err
		}
		hidden += moreHidden
		for _, x := range more {
			dup := false
			for _, y := range impacts {
				dup = dup || y.TaskID == x.TaskID
			}
			if !dup {
				impacts = append(impacts, x)
			}
		}
	}
	if err = denyHidden(hidden); err != nil {
		return Result{}, err
	}
	digest, err := c.Service.AccountStateDigest(old.AccountID)
	if err != nil {
		return Result{}, err
	}
	summary := strings.Join([]string{"原规则：" + DescribeRule(old), "新规则：" + DescribeRule(next), impactText(impacts)}, "\n")
	prop := &Proposal{Kind: "rule_change", AccountID: old.AccountID, Title: "修改规则", Summary: summary, Steps: []Step{{Kind: "rule_save", Rule: &next, FromStep: -1, DraftRuleStep: -1}}, StateDigest: digest}
	return Result{Data: map[string]any{"status": "confirmation_required", "summary": summary, "affected_tasks": impacts}, Proposal: prop}, nil
}

func proposeTaskChange(c Call, in taskChangeIn) (Result, error) {
	old, err := c.Service.Task(c.Access, in.TaskID)
	if err != nil {
		return Result{}, err
	}
	if old.Version != in.Version {
		return Result{}, mail.ErrConflict
	}
	if old.Engine != "rules" {
		return Result{}, fail("capability_unavailable", "首版 Agent 不管理 AI 引擎任务")
	}
	if !hasAction(c.Access, old.AccountID, "task_manage") {
		return Result{}, mail.ErrDenied
	}
	if in.Task.Enabled != nil && !*in.Task.Enabled && in.Task.Mode == nil && in.Task.Name == nil {
		return Result{}, fail("use_pause", "暂停任务请直接使用 pause_task")
	}
	next := old
	if err = in.Task.apply(&next, true); err != nil {
		return Result{}, err
	}
	if err = bindRules(c, &next, in.Task, false); err != nil {
		return Result{}, err
	}
	p, err := c.Service.EffectivePolicy(c.Access, old.AccountID)
	if err != nil {
		return Result{}, err
	}
	if next.Mode == "execute" && p.Mode != "execute" {
		return Result{}, fail("policy_observe_only", "管理员策略当前只允许观察模式")
	}
	if next.Trigger == "internal" && !p.AllowInternal {
		return Result{}, fail("internal_schedule_not_allowed", "管理员策略未允许内置定时")
	}
	digest, err := c.Service.AccountStateDigest(old.AccountID)
	if err != nil {
		return Result{}, err
	}
	lines := []string{"原任务：" + DescribeTask(old, p), "新任务：" + DescribeTask(next, p)}
	if nt := NextTimesText(next); nt != "" {
		lines = append(lines, nt)
	}
	if len(next.RuleIDs) == 0 {
		lines = append(lines, "注意：该任务跟随账号全部启用规则，未来启用的规则也会影响它")
	}
	if old.Mode != "execute" && next.Mode == "execute" {
		lines = append(lines, "首次开启执行模式：将按规则实际写入原邮箱")
	}
	next.NextTimes, next.LastState = nil, ""
	summary := strings.Join(lines, "\n")
	prop := &Proposal{Kind: "task_change", AccountID: old.AccountID, Title: "修改任务", Summary: summary, Steps: []Step{{Kind: "task_save", Task: &next, FromStep: -1, DraftRuleStep: -1}}, StateDigest: digest}
	return Result{Data: map[string]any{"status": "confirmation_required", "summary": summary}, Proposal: prop}, nil
}
