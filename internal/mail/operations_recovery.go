package mail

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"time"

	"mail-auto/internal/store"
)

func (s *Service) StartOperations() error {
	s.mu.Lock()
	if s.stopping || s.operationsStarted {
		s.mu.Unlock()
		return ErrBusy
	}
	s.operationsStarted = true
	s.mu.Unlock()
	_, e := s.Store.DB.Exec(`UPDATE operation_items SET state='needs_verification',error_code='interrupted' WHERE state='running'; UPDATE operations SET state=CASE WHEN EXISTS(SELECT 1 FROM operation_items i WHERE i.operation_id=operations.id AND i.state='needs_verification') THEN 'needs_verification' ELSE 'queued' END WHERE state='running'`)
	if e != nil {
		s.mu.Lock()
		s.operationsStarted = false
		s.mu.Unlock()
		return e
	}
	s.mu.Lock()
	if s.Context == nil {
		s.Context = context.Background()
	}
	if s.cancel == nil {
		s.Context, s.cancel = context.WithCancel(s.Context)
	}
	ctx := s.Context
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var id string
				if s.Store.DB.QueryRow("SELECT id FROM operations WHERE state='queued' ORDER BY created_at,id LIMIT 1").Scan(&id) == nil {
					s.runOperation(ctx, id)
					continue
				}
				now := time.Now()
				if !s.reconcileNextOperation(ctx, now.Add(-operationReconcileDelay), now.Add(-operationReconcileWindow), now, false) {
					s.reconcileNextOperation(ctx, now.Add(-operationSlowReconcileDelay), now.Add(-operationSlowReconcileWindow), now.Add(-operationReconcileWindow), true)
				}
			}
		}
	}()
	return nil
}

// Reconcile at most one interrupted write created in [createdAfter,
// createdBefore) whose last attempt is older than staleBefore. The recovery
// path verifies both remote copies before cleaning the source and is
// idempotent, so an ambiguous provider response can settle without replaying
// MOVE. Two lanes call this: recent operations every 30 seconds, and older
// ones (up to 30 days) at most every six hours. Nothing older is probed
// automatically, but it stays visible and can be verified manually.
func (s *Service) reconcileNextOperation(ctx context.Context, staleBefore, createdAfter, createdBefore time.Time, slow bool) bool {
	var id, owner, raw string
	err := s.Store.DB.QueryRow(`
		SELECT o.id,o.owner,p.payload
		FROM operations o
		JOIN operation_plans p ON p.id=o.plan_id
		WHERE o.state='needs_verification'
		  AND o.updated_at<=?
		  AND o.created_at>=?
		  AND o.created_at<?
		ORDER BY o.updated_at,o.created_at,o.id
		LIMIT 1`,
		staleBefore.Unix(), createdAfter.Unix(), createdBefore.Unix(),
	).Scan(&id, &owner, &raw)
	if err != nil {
		return false
	}
	var plan Plan
	if err = json.Unmarshal([]byte(raw), &plan); err != nil {
		return false
	}
	claimed, err := s.Store.DB.Exec(
		"UPDATE operations SET updated_at=? WHERE id=? AND state='needs_verification' AND updated_at<=?",
		time.Now().Unix(), id, staleBefore.Unix(),
	)
	if err != nil {
		return false
	}
	if n, err := claimed.RowsAffected(); err != nil || n != 1 {
		return false
	}
	slog.Info("自动核验邮件整理结果", "operation_id", id, "slow_lane", slow)
	o, err := s.reconcile(ctx, Access{Owner: owner, Version: plan.Version}, id)
	if err != nil {
		s.recordRecoveryForOperation(id, err)
		slog.Warn("邮件整理结果无法自动核验", "operation_id", id, "reason", syncErrorMessage(err))
	}
	if slow {
		outcome := o.State
		if err != nil {
			outcome = "blocked"
		}
		_ = store.Audit(s.Store.DB, "mail.operation.reconcile.slow", id, outcome)
	}
	return true
}

// Reconcile verifies an operation on request (web or MCP), regardless of its
// age. It runs exactly the same checks as the background lanes and is audited.
func (s *Service) Reconcile(ctx context.Context, a Access, id string) (Operation, error) {
	o, err := s.reconcile(ctx, a, id)
	outcome := o.State
	if err != nil {
		outcome = errorCode(err)
	}
	_ = store.Audit(s.Store.DB, "mail.operation.reconcile.manual", id, outcome)
	return o, err
}

func (s *Service) reconcile(ctx context.Context, a Access, id string) (Operation, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	o, e := s.Operation(a, id)
	if e != nil {
		return o, e
	}
	if o.State != "needs_verification" {
		return o, nil
	}
	p, e := s.GetPlan(a, o.PlanID)
	if e != nil {
		return o, e
	}
	remote, ok := s.Remote.(WritableRemote)
	if !ok {
		return o, ErrCapability
	}
	for _, x := range o.Items {
		if x.State != "needs_verification" {
			continue
		}
		if x.Position < 0 || x.Position >= len(p.Items) {
			return o, ErrConflict
		}
		item := p.Items[x.Position]
		access, e := s.CheckAccess(a)
		if e != nil {
			return o, e
		}
		access.automation = p.Automation != nil
		if !access.Allows(item.Message.AccountID, item.Message.Folder, p.Action) || !access.Allows(item.Message.AccountID, x.Result.Folder, p.Action) {
			return o, ErrDenied
		}
		release, e := s.acquireRecovery(item.Message.AccountID)
		if e != nil {
			continue
		}
		account, e := s.Account(item.Message.AccountID)
		current, lookup := s.Message(item.Message.Ref)
		unchanged := lookup == sql.ErrNoRows || (lookup == nil && ((current.Folder == item.Message.Folder && current.UID == item.Message.UID && current.UIDValidity == item.Message.UIDValidity) || (current.Folder == x.Result.Folder && current.UID == x.Result.UID && current.UIDValidity == x.Result.UIDValidity)))
		if e == nil && unchanged && accountIdentity(account) == item.Config {
			pw, err := s.password(account)
			if err == nil {
				if x.Result.UID != 0 && x.Result.UIDValidity != 0 && x.Result.Folder != "" {
					if verifyErr := remote.VerifyMove(ctx, account, pw, item.Message, x.Result); verifyErr == nil {
						s.invalidateAutomatic(item.Message.AccountID)
						if err = s.publishRecoveredMove(id, x.Position, item, x.Result); err != nil {
							s.recordRecovery(id, x.Position, err)
						}
					} else {
						err := s.finishRecoveredMove(ctx, access, o, p, item, account, pw, x.Result)
						if err == nil {
							err = s.publishRecoveredMove(id, x.Position, item, x.Result)
						}
						if err != nil {
							s.recordRecovery(id, x.Position, err)
							slog.Warn("邮件移动结果仍待核验", "operation_id", id, "position", x.Position, "reason", syncErrorMessage(err))
						}
					}
				} else if inspector, ok := s.Remote.(MoveReconciler); ok {
					evidence, inspectErr := inspector.InspectMove(ctx, account, pw, item.Message, item.Destination)
					if inspectErr != nil {
						s.recordRecovery(id, x.Position, blocked("recovery_no_trusted_mapping", "没有保存服务器返回的目标 UID，且现有证据不足以确认结果；需人工核对", inspectErr))
						slog.Warn("邮件移动结果仍待核验", "operation_id", id, "position", x.Position, "reason", syncErrorMessage(inspectErr))
					} else if evidence.SourcePresent && !evidence.DestinationPresent {
						_, _ = s.Store.DB.Exec(
							"UPDATE operation_items SET state='failed',error_code='move_not_applied',recovery_code='',recovery_detail='',checked_at=? WHERE operation_id=? AND position=? AND state='needs_verification'",
							time.Now().Unix(), id, x.Position,
						)
					} else {
						s.recordRecovery(id, x.Position, blocked("recovery_no_trusted_mapping", "没有保存服务器返回的目标 UID；目标目录中有疑似副本，不能凭元数据确认，需人工核对", ErrUncertain))
					}
				} else {
					s.recordRecovery(id, x.Position, ErrCapability)
					slog.Warn("邮件移动结果仍待核验", "operation_id", id, "position", x.Position, "reason", syncErrorMessage(ErrCapability))
				}
			} else {
				s.recordRecovery(id, x.Position, err)
			}
		} else if e != nil || accountIdentity(account) != item.Config {
			s.recordRecovery(id, x.Position, blocked("recovery_account_changed", "邮箱连接配置已变化，不能自动恢复", ErrConflict))
		} else {
			s.recordRecovery(id, x.Position, blocked("recovery_index_changed", "本地索引中的邮件位置已变化，需重新同步后核验", ErrConflict))
		}
		release()
	}
	var unknown, completed, failed, queued, cancelled int
	if e = s.Store.DB.QueryRow(`SELECT COALESCE(sum(state IN ('needs_verification','running')),0),COALESCE(sum(state IN ('completed','skipped')),0),COALESCE(sum(state='failed'),0),COALESCE(sum(state='queued'),0),COALESCE(sum(state='cancelled'),0) FROM operation_items WHERE operation_id=?`, id).Scan(&unknown, &completed, &failed, &queued, &cancelled); e == nil && unknown == 0 {
		state := "completed"
		if failed > 0 || cancelled > 0 {
			state = "failed"
			if cancelled > 0 {
				state = "cancelled"
			}
			if completed > 0 {
				state = "partial"
			}
		}
		if queued > 0 {
			state = "queued"
		}
		_, _ = s.Store.DB.Exec("UPDATE operations SET state=?,updated_at=? WHERE id=?", state, time.Now().Unix(), id)
	}
	return s.Operation(a, id)
}

func (s *Service) finishRecoveredMove(ctx context.Context, access Access, operation Operation, plan Plan, item PlanItem, account Account, password string, result MoveResult) error {
	if result.Folder != item.Destination {
		return blocked("recovery_destination_mismatch", "保存的目标目录与计划不一致", ErrDenied)
	}
	if plan.Automation != nil {
		// Automated plans are always re-authorized as the task that created them,
		// also when an administrator triggers the verification manually.
		owner, err := s.CheckAccess(Access{Owner: plan.Owner, Version: plan.Version})
		if err != nil {
			return blocked("recovery_access_revoked", "任务已修改或暂停、整理授权已关闭或管理员凭据已变化，不能自动恢复", err)
		}
		owner.automation = true
		access = owner
	}
	// check runs before opening the cleanup session (with the planned snapshot)
	// and, inside FinishMove, twice with fresh remote state on that session.
	check := func(m Message) error {
		if blockedByCancel, err := s.recoveryCancelled(operation.ID, plan); err != nil {
			return err
		} else if blockedByCancel {
			return blocked("recovery_operation_cancelled", "操作已被手动停止，不再自动清理源邮件", ErrDenied)
		}
		settings, err := s.FolderSettings(account.ID)
		if err != nil {
			return err
		}
		if !settings.WritesEnabled {
			return blocked("recovery_writes_disabled", "该邮箱的原邮箱写入已关闭", ErrDenied)
		}
		if settings.Version != item.SettingsVersion {
			return blocked("recovery_settings_changed", "归档/回收站映射或写入设置已变化", ErrDenied)
		}
		current, err := s.Account(account.ID)
		if err != nil || accountIdentity(current) != item.Config {
			return blocked("recovery_account_changed", "邮箱连接配置已变化，不能自动恢复", ErrConflict)
		}
		if plan.Automation != nil {
			return s.validateAutomationRecovery(access, plan, m)
		}
		return nil
	}
	if err := check(item.Message); err != nil {
		return err
	}
	finisher, ok := s.Remote.(MoveFinisher)
	if !ok {
		return ErrCapability
	}
	s.invalidateAutomatic(account.ID)
	return finisher.FinishMove(ctx, account, password, item.Message, result, func(fresh Message) error {
		fresh.Ref = item.Message.Ref
		fresh.ChangedAt = item.Message.ChangedAt
		return check(fresh)
	})
}

func (s *Service) UndoPlan(ctx context.Context, a Access, id string) ([]Plan, error) {
	o, e := s.Operation(a, id)
	if e != nil {
		return nil, e
	}
	p, e := s.GetPlan(a, o.PlanID)
	if e != nil {
		return nil, e
	}
	out := []Plan{}
	for _, x := range o.Items {
		if x.State != "completed" {
			continue
		}
		old := p.Items[x.Position]
		m, e := s.Message(x.Ref)
		if e != nil {
			continue
		}
		in := PlanInput{Refs: []string{x.Ref}}
		if strings.HasPrefix(p.Action, "label_") {
			expected := map[string]bool{}
			for _, name := range x.BeforeLabels {
				expected[name] = true
			}
			for _, name := range p.Labels {
				if p.Action == "label_add" {
					expected[name] = true
				} else {
					delete(expected, name)
				}
			}
			if len(expected) != len(m.Labels) {
				return out, ErrConflict
			}
			for _, l := range m.Labels {
				if !expected[l.Name] {
					return out, ErrConflict
				}
			}
			if p.Action == "label_add" {
				in.Action = "label_remove"
				for _, name := range p.Labels {
					if !slices.Contains(x.BeforeLabels, name) {
						in.Labels = append(in.Labels, name)
					}
				}
			} else {
				in.Action = "label_add"
				for _, name := range p.Labels {
					if slices.Contains(x.BeforeLabels, name) {
						in.Labels = append(in.Labels, name)
					}
				}
			}
			if len(in.Labels) == 0 {
				continue
			}
		} else if isFlagAction(p.Action) {
			if m.Folder != old.Message.Folder || m.UID != old.Message.UID || m.UIDValidity != old.Message.UIDValidity {
				continue
			}
			in.Action = oppositeFlagAction(p.Action)
		} else {
			if m.Folder != x.Result.Folder || m.UID != x.Result.UID || m.UIDValidity != x.Result.UIDValidity {
				continue
			}
			in.Action = "move"
			in.Destination = old.Message.Folder
		}
		undo, e := s.NewPlan(ctx, a, in)
		if e != nil {
			return out, e
		}
		out = append(out, undo)
	}
	return out, nil
}

func sameLabelState(a, b []Label) bool {
	if len(a) != len(b) {
		return false
	}
	ids := map[string]bool{}
	for _, l := range a {
		ids[l.ID] = true
	}
	for _, l := range b {
		if !ids[l.ID] {
			return false
		}
	}
	return true
}

// recoveryCancelled reports whether an explicit stop forbids finishing the
// source cleanup. A task deadline or expired worker lease also marks pending
// operations cancelled to stop new writes; for automated plans that system
// cancellation does not block completing a write that was already issued.
func (s *Service) recoveryCancelled(operationID string, plan Plan) (bool, error) {
	var cancelled bool
	var source string
	if err := s.Store.DB.QueryRow("SELECT cancelled,cancel_source FROM operations WHERE id=?", operationID).Scan(&cancelled, &source); err != nil {
		return false, err
	}
	if !cancelled {
		return false, nil
	}
	if plan.Automation != nil && (source == "system:task_timeout" || source == "system:worker_lease_expired") {
		return false, nil
	}
	return true, nil
}
