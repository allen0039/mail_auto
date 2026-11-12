package mail

import (
	"errors"
	"slices"
	"time"
)

// recoveryBlock explains, with a fixed code and a sanitized message, why an
// uncertain write cannot be finished automatically. It never carries provider
// text or mail content.
type recoveryBlock struct {
	code, message string
	cause         error
}

func (e *recoveryBlock) Error() string              { return e.code }
func (e *recoveryBlock) OperationErrorCode() string { return e.code }
func (e *recoveryBlock) PublicMessage() string      { return e.message }
func (e *recoveryBlock) Unwrap() error              { return e.cause }

func blocked(code, message string, cause error) error {
	return &recoveryBlock{code: code, message: message, cause: cause}
}

// validateAutomationRecovery authorizes completing the source cleanup of an
// automated move whose COPY/MOVE was already issued. Compared with
// validateAutomationPlan it does not require the task run to be active, the
// worker lease or batch deadline to be valid, the same worker process, or the
// same Shanghai day: those gate *starting* writes. Everything that decides
// whether the change is still wanted and allowed is checked again: current
// grant and task identity, policy/rule versions, batch not stopped by the user,
// action and destination grants, unchanged message fingerprint, rule match and
// unread/flagged/contact protection.
func (s *Service) validateAutomationRecovery(a Access, p Plan, fresh Message) error {
	g := p.Automation
	if g == nil || len(p.Items) != 1 {
		return blocked("recovery_plan_invalid", "计划不是单封自动化计划，不能自动恢复", ErrConflict)
	}
	b, e := s.AIBatch(a, g.BatchID)
	if e != nil {
		return blocked("recovery_access_revoked", "任务授权已撤销或任务已修改，不能自动恢复", e)
	}
	if b.State == "cancelled" {
		return blocked("recovery_batch_cancelled", "该批次已被手动停止，不再自动清理源邮件", ErrDenied)
	}
	a, policy, e := s.authorizedAutomation(a, b.AccountID)
	if e != nil {
		return blocked("recovery_policy_disabled", "整理授权已关闭、过期或不再覆盖该目录", e)
	}
	var rules []Rule
	if b.RunID != "" {
		r, err := s.loadTaskRun(b.RunID)
		if err != nil {
			return blocked("recovery_task_missing", "找不到所属任务运行记录", err)
		}
		t, err := s.loadTask(r.TaskID)
		if err != nil {
			return blocked("recovery_task_missing", "找不到所属任务", err)
		}
		if policy, err = s.taskRunPolicy(a, policy, r, t); err != nil {
			return blocked("recovery_task_changed", "任务、整理授权或管理员凭据已变化，不能沿用原计划", err)
		}
		if rules, err = s.taskRules(a, t); err != nil {
			return blocked("recovery_rules_changed", "任务绑定的规则已变化", err)
		}
	} else if rules, e = s.rulesForAccount(b.AccountID); e != nil {
		return e
	}
	if b.Owner != a.Owner || b.Version != a.Version || b.PolicyVersion != policy.Version || g.PolicyVersion != policy.Version {
		return blocked("recovery_policy_changed", "整理授权版本已变化", ErrConflict)
	}
	if rulesDigest(rules) != b.RulesDigest || g.RulesDigest != b.RulesDigest {
		return blocked("recovery_rules_changed", "规则已修改，原计划不再有效", ErrConflict)
	}
	if policy.Mode != "execute" || b.State != "submitted" || !slices.Contains(b.PlanIDs, p.ID) {
		return blocked("recovery_policy_changed", "任务或授权已不是执行模式", ErrConflict)
	}
	if !slices.Contains(policy.Actions, p.Action) || (p.Action == "move" && !slices.Contains(policy.Destinations, p.Items[0].Destination)) {
		return blocked("recovery_action_denied", "整理授权不再允许该动作或目标目录", ErrDenied)
	}
	for _, extra := range p.Also {
		if !slices.Contains(policy.Actions, extra) {
			return blocked("recovery_action_denied", "整理授权不再允许附带的标记动作", ErrDenied)
		}
	}
	found := false
	for _, item := range b.Items {
		if item.Message.Ref == fresh.Ref && item.Signature == g.Signature && item.RuleID == g.RuleID && item.RuleVersion == g.RuleVersion {
			found = true
			break
		}
	}
	if !found {
		return blocked("recovery_plan_invalid", "批次中找不到对应邮件", ErrConflict)
	}
	if messageSignature(fresh) != g.Signature {
		return blocked("recovery_fingerprint_mismatch", "源邮件的元数据或已读/星标状态已变化，不能确认是同一封未改动的邮件", ErrConflict)
	}
	evaluated := evaluateRules(fresh, rules, policy, time.Now().Unix())
	if evaluated.Status == "protected" {
		return blocked("recovery_protected", "邮件现在受保护："+firstReason(evaluated.Reasons), ErrDenied)
	}
	if evaluated.Status != "matched" || evaluated.RuleID != g.RuleID || evaluated.RuleVersion != g.RuleVersion {
		return blocked("recovery_rule_mismatch", "邮件不再命中原规则", ErrConflict)
	}
	return nil
}

func firstReason(reasons []string) string {
	if len(reasons) == 0 {
		return "保护条件"
	}
	return reasons[0]
}

// recoveryDiagnostic returns a code and sanitized message for persisting.
func recoveryDiagnostic(err error) (string, string) {
	var b *recoveryBlock
	if errors.As(err, &b) {
		return b.code, b.message
	}
	return "recovery_" + errorCode(err), syncErrorMessage(err)
}

// recordRecovery persists the latest verification attempt for one item.
func (s *Service) recordRecovery(operationID string, position int, err error) {
	code, message := "", ""
	if err != nil {
		code, message = recoveryDiagnostic(err)
	}
	_, _ = s.Store.DB.Exec("UPDATE operation_items SET recovery_code=?,recovery_detail=?,checked_at=? WHERE operation_id=? AND position=?", code, message, time.Now().Unix(), operationID, position)
}

// recordRecoveryForOperation marks every unresolved item when the whole
// operation cannot be inspected, e.g. because its task identity was revoked.
func (s *Service) recordRecoveryForOperation(operationID string, err error) {
	code, message := recoveryDiagnostic(err)
	if code == "recovery_permission_denied" {
		code, message = "recovery_access_revoked", "任务已修改或暂停、整理授权已关闭或管理员凭据已变化，不能自动恢复"
	}
	_, _ = s.Store.DB.Exec("UPDATE operation_items SET recovery_code=?,recovery_detail=?,checked_at=? WHERE operation_id=? AND state='needs_verification'", code, message, time.Now().Unix(), operationID)
}
