package mailtools

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"mail-auto/internal/mail"
)

// Proposal is a server-built change plan that needs explicit confirmation.
// The model can request one but can never mark it confirmed.
type Proposal struct {
	Kind        string `json:"kind"`
	AccountID   string `json:"account_id"`
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	Steps       []Step `json:"steps"`
	StateDigest string `json:"state_digest"`
}

// Step is one business write of a proposal. Later steps take IDs and
// versions from earlier step results (FromStep), never from the model.
type Step struct {
	Kind          string     `json:"kind"`
	Rule          *mail.Rule `json:"rule,omitempty"`
	Task          *mail.Task `json:"task,omitempty"`
	TaskID        string     `json:"task_id,omitempty"`
	Version       int        `json:"version,omitempty"`
	Mode          string     `json:"mode,omitempty"`
	FromStep      int        `json:"from_step"`
	DraftRuleStep int        `json:"draft_rule_step"`
}

// StepResult is what a committed step produced.
type StepResult struct {
	TargetID string     `json:"target_id"`
	Version  int        `json:"version"`
	Rule     *mail.Rule `json:"rule,omitempty"`
	Task     *mail.Task `json:"task,omitempty"`
	Run      *mail.TaskRun
}

// ApplyStep is ApplyStepGuarded without a lifecycle guard (tests and
// non-agent callers). The agent runtime always uses ApplyStepGuarded.
func ApplyStep(ctx context.Context, svc *mail.Service, a mail.Access, proposalID string, steps []Step, i int, prior []StepResult) (StepResult, error) {
	return ApplyStepGuarded(ctx, svc, a, proposalID, steps, i, prior, nil)
}

// MutationID is the server idempotency key of step i of a proposal.
func MutationID(proposalID string, i int) string { return proposalID + ":" + strconv.Itoa(i) }

// ApplyStep executes step i. Re-running a committed step returns its stored
// result without writing again, so recovery may replay steps from 0.
//
// guard runs inside each step's business transaction; it must reject the
// write when the proposal's frozen execution conditions no longer hold.
func ApplyStepGuarded(ctx context.Context, svc *mail.Service, a mail.Access, proposalID string, steps []Step, i int, prior []StepResult, guard func(*sql.Tx) error) (StepResult, error) {
	st := steps[i]
	m := mail.Mutation{ID: MutationID(proposalID, i), Guard: guard}
	switch st.Kind {
	case "rule_save":
		m.Kind = "rule.save"
		r, err := svc.SaveRuleWithMutation(a, *st.Rule, m)
		if err != nil {
			return StepResult{}, err
		}
		return StepResult{TargetID: r.ID, Version: r.Version, Rule: &r}, nil
	case "task_create":
		m.Kind = "task.save"
		t := *st.Task
		t.RuleVersions = mapsClone(t.RuleVersions)
		if st.DraftRuleStep >= 0 {
			if st.DraftRuleStep >= len(prior) || prior[st.DraftRuleStep].Rule == nil {
				return StepResult{}, mail.ErrConflict
			}
			t.RuleVersions[prior[st.DraftRuleStep].TargetID] = prior[st.DraftRuleStep].Version
		}
		saved, err := svc.SaveTaskWithMutation(a, t, m)
		if err != nil {
			return StepResult{}, err
		}
		return StepResult{TargetID: saved.ID, Version: saved.Version, Task: &saved}, nil
	case "task_save":
		m.Kind = "task.save"
		var t mail.Task
		if st.FromStep >= 0 {
			if st.FromStep >= len(prior) || prior[st.FromStep].Task == nil {
				return StepResult{}, mail.ErrConflict
			}
			t = *prior[st.FromStep].Task
			t.Mode = st.Mode
		} else {
			t = *st.Task
		}
		t.NextTimes, t.LastState = nil, ""
		saved, err := svc.SaveTaskWithMutation(a, t, m)
		if err != nil {
			return StepResult{}, err
		}
		return StepResult{TargetID: saved.ID, Version: saved.Version, Task: &saved}, nil
	case "task_enable":
		m.Kind = "task.enabled"
		id, version := st.TaskID, st.Version
		if st.FromStep >= 0 {
			if st.FromStep >= len(prior) {
				return StepResult{}, mail.ErrConflict
			}
			id, version = prior[st.FromStep].TargetID, prior[st.FromStep].Version
		}
		saved, err := svc.SetTaskEnabledWithMutation(a, id, version, true, m)
		if err != nil {
			return StepResult{}, err
		}
		return StepResult{TargetID: saved.ID, Version: saved.Version, Task: &saved}, nil
	case "task_run":
		id, version := st.TaskID, st.Version
		if st.FromStep >= 0 {
			if st.FromStep >= len(prior) {
				return StepResult{}, mail.ErrConflict
			}
			id, version = prior[st.FromStep].TargetID, prior[st.FromStep].Version
		}
		run, err := svc.RunTaskGuarded(a, mail.TaskRunInput{TaskID: id, Version: version, RunKey: "agent:proposal:" + proposalID}, "manual", guard)
		if err != nil {
			return StepResult{}, err
		}
		return StepResult{TargetID: run.ID, Run: &run}, nil
	}
	return StepResult{}, mail.ErrInput
}

func mapsClone(in map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ruleFields is a controlled patch: absent fields keep their stored value.
type ruleFields struct {
	Name          *string         `json:"name,omitempty"`
	Priority      *int            `json:"priority,omitempty"`
	SourceFolders *[]string       `json:"source_folders,omitempty"`
	Match         *mail.RuleGroup `json:"match,omitempty"`
	Action        *string         `json:"action,omitempty"`
	Destination   *string         `json:"destination,omitempty"`
	Also          *[]string       `json:"also,omitempty"`
}

func (f ruleFields) apply(r *mail.Rule) error {
	if f.Name != nil {
		r.Name = trimName(*f.Name)
	}
	if f.Priority != nil {
		r.Priority = *f.Priority
	}
	if f.SourceFolders != nil {
		r.SourceFolders = slices.Clone(*f.SourceFolders)
	}
	if f.Match != nil {
		r.Match = *f.Match
	}
	if f.Action != nil {
		if *f.Action == "ai" {
			return fail("capability_unavailable", "首版内置 Agent 不支持持续 AI 分类规则；请改用固定条件规则")
		}
		r.Action = *f.Action
	}
	if f.Destination != nil {
		r.Destination = *f.Destination
	}
	if f.Also != nil {
		r.Also = slices.Clone(*f.Also)
	}
	if r.Action != "move" {
		r.Destination = ""
	}
	return nil
}

// taskFields is a controlled task patch. Mode and Enabled are honored only by
// proposals, never by drafts.
type taskFields struct {
	Name           *string            `json:"name,omitempty"`
	SourceFolders  *[]string          `json:"source_folders,omitempty"`
	RuleIDs        *[]string          `json:"rule_ids,omitempty"`
	FollowAllRules *bool              `json:"follow_all_rules,omitempty"`
	TriggerMode    *string            `json:"trigger_mode,omitempty"`
	Schedule       *mail.TaskSchedule `json:"schedule,omitempty"`
	MaxBatch       *int               `json:"max_batch,omitempty"`
	ProtectUnread  *bool              `json:"protect_unread,omitempty"`
	ProtectFlagged *bool              `json:"protect_flagged,omitempty"`
	Notify         *string            `json:"notify,omitempty"`
	Mode           *string            `json:"mode,omitempty"`
	Enabled        *bool              `json:"enabled,omitempty"`
}

// apply merges the patch; rule binding is resolved separately.
func (f taskFields) apply(t *mail.Task, allowState bool) error {
	if !allowState && (f.Mode != nil || f.Enabled != nil) {
		return fail("confirmation_required", "启用或切换执行模式需要使用 propose_task_change 生成确认方案")
	}
	if f.Name != nil {
		t.Name = trimName(*f.Name)
	}
	if f.SourceFolders != nil {
		t.SourceFolders = slices.Clone(*f.SourceFolders)
	}
	if f.TriggerMode != nil {
		if *f.TriggerMode != "manual" && *f.TriggerMode != "internal" {
			return fail("invalid_input", "trigger_mode 仅支持 manual 或 internal")
		}
		t.Trigger = *f.TriggerMode
	}
	if f.Schedule != nil {
		t.Schedule = *f.Schedule
		if t.Schedule.Timezone == "" {
			t.Schedule.Timezone = "Asia/Shanghai"
		}
	}
	if f.MaxBatch != nil {
		t.MaxBatch = *f.MaxBatch
	}
	if f.ProtectUnread != nil {
		t.ProtectUnread = *f.ProtectUnread
	}
	if f.ProtectFlagged != nil {
		t.ProtectFlagged = *f.ProtectFlagged
	}
	if f.Notify != nil {
		t.Notify = *f.Notify
	}
	if f.Mode != nil {
		if *f.Mode != "observe" && *f.Mode != "execute" {
			return fail("invalid_input", "mode 仅支持 observe 或 execute")
		}
		t.Mode = *f.Mode
	}
	if f.Enabled != nil {
		t.Enabled = *f.Enabled
	}
	return nil
}

// bindRules resolves the binding on the current enabled rules. Without an
// explicit choice the existing binding is kept. Explicit rules get the
// higher-priority closure; follow_all must be chosen explicitly.
func bindRules(c Call, t *mail.Task, f taskFields, creating bool) error {
	if f.FollowAllRules != nil && *f.FollowAllRules {
		if f.RuleIDs != nil && len(*f.RuleIDs) > 0 {
			return fail("invalid_input", "rule_ids 与 follow_all_rules 不能同时使用")
		}
		t.RuleIDs, t.RuleVersions = nil, nil
		return nil
	}
	if f.RuleIDs == nil {
		if creating {
			return fail("invalid_input", "请指定 rule_ids，或明确设置 follow_all_rules=true（未来启用的规则也会影响该任务）")
		}
		return nil
	}
	if len(*f.RuleIDs) == 0 {
		return fail("invalid_input", "rule_ids 不能为空；跟随全部规则需明确设置 follow_all_rules=true")
	}
	rules, err := c.Service.Rules(c.Access, t.AccountID)
	if err != nil {
		return err
	}
	for _, id := range *f.RuleIDs {
		found := false
		for _, r := range rules {
			if r.ID == id {
				found = true
				if !r.Enabled {
					return fail("rule_disabled", "绑定的规则尚未启用；新规则请用 propose_workflow 生成启用方案")
				}
			}
		}
		if !found {
			return mail.ErrDenied
		}
	}
	ids, versions, err := mail.RuleClosure(rules, *f.RuleIDs, t.SourceFolders)
	if err != nil {
		return err
	}
	t.RuleIDs, t.RuleVersions = ids, versions
	return nil
}

func impactText(impacts []mail.TaskImpact) string {
	if len(impacts) == 0 {
		return "受影响的现有任务：无"
	}
	lines := []string{fmt.Sprintf("受影响的现有任务 %d 个：", len(impacts))}
	for _, x := range impacts {
		state := "暂停"
		if x.Enabled {
			state = "启用"
		}
		lines = append(lines, fmt.Sprintf("· %s（%s）：%s", x.Name, state, x.Reason))
	}
	return strings.Join(lines, "\n")
}

func previewText(p mail.RulePreview) string {
	if p.Scanned == 0 {
		return "试运行：来源文件夹当前没有可评估的邮件（0 候选，不代表远端写入已验证）"
	}
	more := ""
	if p.NextCursor != "" {
		more = "，尚未扫描完，以上仅为本次扫描"
	}
	return fmt.Sprintf("试运行（仅本地元数据，不写原邮箱）：本次扫描 %d 封（来源共 %d 封%s）；命中 %d，保护跳过 %d，条件未知 %d，未命中 %d；每批上限 %d 封", p.Scanned, p.Total, more, p.Counts["matched"], p.Counts["protected"], p.Counts["unknown"], p.Counts["unmatched"], p.MaxBatch)
}

func denyHidden(hidden int) error {
	if hidden > 0 {
		return fail("impact_outside_grant", "该规则会影响 Agent 无权管理的任务，请在 Web 管理界面处理")
	}
	return nil
}
