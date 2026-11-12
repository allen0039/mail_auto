package mail

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"time"
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
				s.reconcileNextOperation(ctx, time.Now().Add(-operationReconcileDelay), operationReconcileWindow)
			}
		}
	}()
	return nil
}

// Reconcile at most one interrupted write after a short quiet period. The
// recovery path verifies both remote copies before cleaning the source and is
// idempotent, so an ambiguous provider response can settle without replaying
// MOVE. Age-bounding avoids probing abandoned operations forever.
func (s *Service) reconcileNextOperation(ctx context.Context, staleBefore time.Time, maxAge time.Duration) bool {
	var id, owner, raw string
	err := s.Store.DB.QueryRow(`
		SELECT o.id,o.owner,p.payload
		FROM operations o
		JOIN operation_plans p ON p.id=o.plan_id
		WHERE o.state='needs_verification'
		  AND o.updated_at<=?
		  AND o.created_at>=?
		ORDER BY o.updated_at,o.created_at,o.id
		LIMIT 1`,
		staleBefore.Unix(), time.Now().Add(-maxAge).Unix(),
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
	slog.Info("自动核验邮件整理结果", "operation_id", id)
	_, _ = s.Reconcile(ctx, Access{Owner: owner, Version: plan.Version}, id)
	return true
}

func (s *Service) Reconcile(ctx context.Context, a Access, id string) (Operation, error) {
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
		release, e := s.acquire(item.Message.AccountID)
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
						_ = s.publishMove(id, x.Position, item.Message, x.Result)
					} else {
						err := s.finishRecoveredMove(ctx, access, o, p, item, account, pw, x.Result)
						if err == nil {
							_ = s.publishMove(id, x.Position, item.Message, x.Result)
						} else {
							slog.Warn("邮件移动结果仍待核验", "operation_id", id, "position", x.Position, "reason", syncErrorMessage(err))
						}
					}
				} else if inspector, ok := s.Remote.(MoveReconciler); ok {
					evidence, inspectErr := inspector.InspectMove(ctx, account, pw, item.Message, item.Destination)
					if inspectErr != nil {
						slog.Warn("邮件移动结果仍待核验", "operation_id", id, "position", x.Position, "reason", syncErrorMessage(inspectErr))
					} else if evidence.SourcePresent && !evidence.DestinationPresent {
						_, _ = s.Store.DB.Exec(
							"UPDATE operation_items SET state='failed',error_code='move_not_applied' WHERE operation_id=? AND position=? AND state='needs_verification'",
							id, x.Position,
						)
					}
				} else {
					slog.Warn("邮件移动结果仍待核验", "operation_id", id, "position", x.Position, "reason", syncErrorMessage(ErrCapability))
				}
			}
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
	if operation.Cancelled || result.Folder != item.Destination {
		return ErrDenied
	}
	settings, err := s.FolderSettings(account.ID)
	if err != nil {
		return err
	}
	if !settings.WritesEnabled || settings.Version != item.SettingsVersion {
		return ErrDenied
	}
	if plan.Automation != nil {
		freshRemote, ok := s.Remote.(RuleStateRemote)
		if !ok {
			return ErrCapability
		}
		fresh, err := freshRemote.RuleState(ctx, account, password, item.Message)
		if err != nil {
			return err
		}
		fresh.Ref = item.Message.Ref
		fresh.ChangedAt = item.Message.ChangedAt
		if _, _, err = s.validateAutomationPlan(access, plan, fresh); err != nil {
			return err
		}
	}
	var cancelled bool
	if err = s.Store.DB.QueryRow("SELECT cancelled FROM operations WHERE id=?", operation.ID).Scan(&cancelled); err != nil {
		return err
	}
	if cancelled {
		return ErrDenied
	}
	finisher, ok := s.Remote.(MoveFinisher)
	if !ok {
		return ErrCapability
	}
	return finisher.FinishMove(ctx, account, password, item.Message, result)
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
