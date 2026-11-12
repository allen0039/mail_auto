package mail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

// StartTaskAutomation starts durable task supervision and independent notification
// delivery. No schedules or mail permissions are enabled by starting it.
func (s *Service) StartTaskAutomation() error {
	s.mu.Lock()
	if s.stopping || s.tasksStarted {
		s.mu.Unlock()
		return ErrBusy
	}
	s.tasksStarted = true
	s.workerID = credentials.Random(18)
	s.taskWorkers = map[string]bool{}
	if s.Context == nil {
		s.Context = context.Background()
	}
	if s.cancel == nil {
		s.Context, s.cancel = context.WithCancel(s.Context)
	}
	ctx := s.Context
	s.wg.Add(2)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			s.taskTick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.deliverNotification(ctx)
			}
		}
	}()
	return nil
}
func (s *Service) taskTick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := time.Now().Unix()
	s.checkExternalCadence(now)
	rows, e := s.Store.DB.QueryContext(ctx, "SELECT id FROM automation_tasks WHERE enabled=1 AND trigger_mode='internal' AND next_run_at>0 AND next_run_at<=? ORDER BY next_run_at LIMIT 20", now)
	if e == nil {
		ids := []string{}
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			s.taskMu.Lock()
			t, e := s.loadTask(id)
			if e == nil && t.Enabled && t.Trigger == "internal" && t.NextRunAt > 0 && t.NextRunAt <= now {
				// Creating the occurrence and advancing next_run_at commit together.
				scheduled := t.NextRunAt
				_, e = s.createTaskRun(Access{Owner: t.ServiceOwner, Version: t.AdminVersion}, t, taskRunKey(t, scheduled), "internal", scheduled)
				if errors.Is(e, ErrBusy) {
					next, err := nextSchedule(t.Schedule, time.Now())
					if err == nil && next == 0 {
						// Event-driven task: retry shortly instead of dropping the arrival.
						next = time.Now().Unix() + 60
					}
					if err == nil {
						_, _ = s.Store.DB.Exec("UPDATE automation_tasks SET next_run_at=? WHERE id=? AND version=?", next, t.ID, t.Version)
						_ = store.Audit(s.Store.DB, "mail.task.missed", t.ID, "active_run_coalesced")
					}
				} else if errors.Is(e, ErrConflict) || errors.Is(e, ErrDenied) || errors.Is(e, ErrInput) {
					// A changed rule binding, revoked grant, or invalid schedule must
					// not leave the occurrence permanently due and spin every tick.
					next, nextErr := nextSchedule(t.Schedule, time.Now())
					if nextErr == nil {
						_, _ = s.Store.DB.Exec("UPDATE automation_tasks SET next_run_at=? WHERE id=? AND version=?", next, t.ID, t.Version)
					}
					s.recordTaskScheduleFailure(t, scheduled, e)
				}
			}
			s.taskMu.Unlock()
		}
	}
	rows, e = s.Store.DB.QueryContext(ctx, "SELECT id FROM task_runs WHERE state IN ('queued','running','awaiting_ai','needs_verification') OR COALESCE(json_extract(payload,'$.finalized'),0)=0 ORDER BY CASE WHEN state IN ('queued','running','awaiting_ai') THEN 0 ELSE 1 END,created_at LIMIT 200")
	if e != nil {
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		r, e := s.loadTaskRun(id)
		if e != nil {
			continue
		}
		if r.State == "queued" {
			s.mu.Lock()
			busy := s.stopping || s.taskWorkers[id] || len(s.taskWorkers) >= 2
			if !busy {
				s.taskWorkers[id] = true
				s.wg.Add(1)
			}
			s.mu.Unlock()
			if !busy {
				go func(id string) {
					defer s.wg.Done()
					defer func() { s.mu.Lock(); delete(s.taskWorkers, id); s.mu.Unlock() }()
					s.runFixedTask(ctx, id)
				}(id)
			}
		} else {
			s.reconcileTaskRun(id)
		}
	}
}

func (s *Service) recordTaskScheduleFailure(t Task, scheduled int64, cause error) {
	now := time.Now().Unix()
	r := TaskRun{ID: credentials.Random(24), TaskID: t.ID, AccountID: t.AccountID, TaskName: t.Name, TaskVersion: t.Version, Owner: t.ServiceOwner, AdminVersion: t.AdminVersion, Source: "internal", RunKey: taskRunKey(t, scheduled), Mode: t.Mode, State: "failed", CreatedAt: now, UpdatedAt: now, Expires: now, ScheduledAt: scheduled, Finalized: true, ErrorCode: taskError(cause)}
	result, e := s.Store.DB.Exec("INSERT OR IGNORE INTO task_runs(id,task_id,account_id,owner,run_key,task_version,source,state,payload,created_at,updated_at,expires) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", r.ID, t.ID, t.AccountID, r.Owner, r.RunKey, t.Version, r.Source, r.State, runJSON(r), now, now, now)
	if e == nil {
		if n, _ := result.RowsAffected(); n == 1 {
			_ = s.finishTaskRun(r)
		}
	}
}
func (s *Service) ensureTaskIndex(ctx context.Context, p AutomationPolicy) error {
	automatic := s.AutomaticStatus().Enabled
	for _, f := range p.SourceFolders {
		var at int64
		e := s.Store.DB.QueryRow("SELECT synced_at FROM mailboxes WHERE account_id=? AND name=?", p.AccountID, f).Scan(&at)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		// Automatic sync owns index freshness. A scheduled task must not launch a
		// full multi-folder sync: under provider latency that can consume the whole
		// task deadline before the first candidate is scanned. Every automation
		// write is checked against fresh remote state before execution.
		staleManualIndex := !automatic && time.Now().Unix()-at > 120
		if e == sql.ErrNoRows || at == 0 || staleManualIndex {
			_, e = s.Sync(ctx, p.AccountID)
			if e != nil {
				return e
			}
			break
		}
	}
	return nil
}

// This watchdog observes incoming external calls. It never reads mail or wakes AI.
func (s *Service) checkExternalCadence(now int64) {
	rows, e := s.Store.DB.Query("SELECT id FROM automation_tasks WHERE enabled=1 AND trigger_mode='external' AND json_extract(payload,'$.task.expected_seconds')>0 LIMIT 500")
	if e != nil {
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.taskMu.Lock()
		t, e := s.loadTask(id)
		if e != nil {
			s.taskMu.Unlock()
			continue
		}
		base := max(t.LastTriggered, t.UpdatedAt)
		grace := max(300, t.ExpectedSeconds/10)
		if now <= base+int64(t.ExpectedSeconds+grace) {
			s.taskMu.Unlock()
			continue
		}
		r := TaskRun{ID: credentials.Random(24), TaskID: t.ID, AccountID: t.AccountID, TaskName: t.Name, TaskVersion: t.Version, Owner: t.ServiceOwner, AdminVersion: t.AdminVersion, Source: "monitor", RunKey: fmt.Sprintf("overdue:%d:%d", t.Version, base), Mode: "observe", State: "failed", CreatedAt: now, UpdatedAt: now, Expires: now, ErrorCode: "external_trigger_overdue"}
		res, err := s.Store.DB.Exec("INSERT OR IGNORE INTO task_runs(id,task_id,account_id,owner,run_key,task_version,source,state,payload,created_at,updated_at,expires) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", r.ID, t.ID, t.AccountID, r.Owner, r.RunKey, t.Version, r.Source, r.State, runJSON(r), now, now, now)
		if err == nil {
			n, _ := res.RowsAffected()
			if n == 1 {
				_ = s.finishTaskRun(r)
			}
		}
		s.taskMu.Unlock()
	}
}
func (s *Service) runFixedTask(parent context.Context, id string) {
	now := time.Now().Unix()
	res, e := s.Store.DB.Exec("UPDATE task_runs SET state='running',lease_owner=?,lease_until=? WHERE id=? AND state='queued'", s.workerID, now+45, id)
	if e != nil {
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return
	}
	r, e := s.loadTaskRun(id)
	if e != nil {
		return
	}
	ctx, cancel := context.WithDeadline(parent, time.Unix(r.Expires, 0))
	defer cancel()
	done := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				result, err := s.Store.DB.Exec("UPDATE task_runs SET lease_until=? WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?", time.Now().Unix()+45, id, s.workerID, time.Now().Unix())
				if err != nil {
					cancel()
					return
				}
				n, _ := result.RowsAffected()
				if n != 1 {
					cancel()
					return
				}
			}
		}
	}()
	e = s.executeFixedTask(ctx, r)
	close(done)
	<-heartbeatDone
	if e != nil {
		s.taskMu.Lock()
		current, err := s.loadTaskRun(id)
		if err == nil && activeTaskState(current.State) {
			current.State = "failed"
			current.ErrorCode = taskError(e)
			if errors.Is(e, ErrDailyLimit) {
				current.State = "limited"
				current.HasMore = true
			}
			if ctx.Err() != nil {
				current.State = "interrupted"
				current.ErrorCode = "task_timeout"
			}
			if current.BatchID != "" {
				_ = s.cancelRunBatch(current)
			}
			_ = s.saveTaskRun(current)
		}
		s.taskMu.Unlock()
	}
	s.reconcileTaskRun(id)
}
func (s *Service) executeFixedTask(ctx context.Context, r TaskRun) error {
	a := Access{Owner: r.Owner, Version: r.AdminVersion}
	a, p, e := s.authorizedAutomation(a, r.AccountID)
	if e != nil {
		return e
	}
	p, e = s.taskBatchPolicy(a, p, r.ID)
	if e != nil {
		return e
	}
	releaseAutomatic := s.holdAutomatic(p.AccountID)
	defer releaseAutomatic()
	if writableSessions, ok := s.Remote.(WriteSessionRemote); ok {
		releaseWriteSession := writableSessions.BeginWriteSession(p.AccountID)
		defer releaseWriteSession()
	}
	if e = s.ensureTaskIndex(ctx, p); e != nil {
		return e
	}
	b, e := s.ClaimAI(a, AIClaimInput{RunID: r.ID, AccountID: r.AccountID, RunKey: "taskrun:" + r.ID})
	if e != nil {
		return e
	}
	if b.State == "empty" {
		return nil
	}
	decisions := []AIDecision{}
	for _, item := range b.Items {
		d := AIDecision{Ref: item.Message.Ref, Signature: item.Signature, Action: item.Action, Destination: item.Destination, Reason: "按已配置规则执行"}
		if item.Action == "ai" {
			d.Action = "keep"
			d.Destination = ""
			d.Reason = "需要外部 AI 判断，保留原位"
		}
		decisions = append(decisions, d)
	}
	b, e = s.SubmitAI(ctx, a, AISubmitInput{BatchID: b.ID, IdempotencyKey: "task-submit:" + r.ID, Decisions: decisions})
	if e != nil {
		return e
	}
	for _, planID := range b.PlanIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Deterministic tasks still load and inspect the frozen plan before queueing.
		if _, e = s.GetPlan(a, planID); e != nil {
			return e
		}
		if _, e = s.ExecuteAI(a, b.ID, planID); e != nil {
			return e
		}
	}
	for {
		var count int
		e = s.Store.DB.QueryRow("SELECT count(*) FROM operations WHERE plan_id IN (SELECT plan_id FROM automation_claims WHERE batch_id=?) AND state IN ('queued','running')", b.ID).Scan(&count)
		if e != nil {
			return e
		}
		if count == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
func (s *Service) cancelRunBatch(r TaskRun) error {
	// Cancellation must work even if task/credential revocation made its identity invalid.
	_, e := s.Store.DB.Exec("UPDATE operations SET cancelled=1 WHERE plan_id IN (SELECT plan_id FROM automation_claims WHERE batch_id=?)", r.BatchID)
	if e != nil {
		return e
	}
	_, e = s.Store.DB.Exec("UPDATE automation_claims SET state='done',expires=0 WHERE batch_id=? AND plan_id=''", r.BatchID)
	return e
}
func (s *Service) reconcileTaskRun(id string) {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	r, e := s.loadTaskRun(id)
	if e != nil {
		return
	}
	t, e := s.loadTask(r.TaskID)
	if e != nil {
		return
	}
	now := time.Now().Unix()
	if activeTaskState(r.State) {
		if t.Version != r.TaskVersion {
			r.State = "cancelled"
			r.ErrorCode = "task_configuration_changed"
			_ = s.cancelRunBatch(r)
		}
		if activeTaskState(r.State) {
			p, err := s.loadAutomationPolicy(r.AccountID)
			_, accessErr := s.CheckAccess(Access{Owner: r.Owner, Version: r.AdminVersion})
			if err != nil || accessErr != nil || !p.Enabled || p.Expires <= now || p.AdminVersion != r.AdminVersion {
				r.State = "failed"
				r.ErrorCode = "permission_denied"
				_ = s.cancelRunBatch(r)
			} else if p.Version != r.PolicyVersion {
				r.State = "cancelled"
				r.ErrorCode = "policy_changed"
				_ = s.cancelRunBatch(r)
			}
		}
		if r.Expires <= now {
			r.State = "interrupted"
			r.ErrorCode = "task_timeout"
			_ = s.cancelRunBatch(r)
		}
		if r.State == "running" {
			var until int64
			_ = s.Store.DB.QueryRow("SELECT lease_until FROM task_runs WHERE id=?", r.ID).Scan(&until)
			if until <= now {
				r.State = "interrupted"
				r.ErrorCode = "worker_lease_expired"
				_ = s.cancelRunBatch(r)
			}
		}
	}
	if r.BatchID == "" {
		if !activeTaskState(r.State) {
			_ = s.finishTaskRun(r)
		}
		return
	}
	b, e := s.loadAIBatch(r.BatchID)
	if e != nil {
		return
	}
	r.Scanned = b.Scanned
	r.Claimed = len(b.Items)
	r.Protected = b.Protected
	r.HasMore = b.NextCursor != ""
	r.NextCursor = b.NextCursor
	r.Kept = 0
	r.NeedsAI = 0
	for _, d := range b.Decisions {
		if d.Action == "keep" {
			r.Kept++
		}
	}
	if t.Engine == "rules" {
		for _, item := range b.Items {
			if item.Action == "ai" {
				r.NeedsAI++
			}
		}
	}
	var pending, planned int
	e = s.Store.DB.QueryRow(`SELECT COALESCE(sum(i.state='completed'),0),COALESCE(sum(i.state='failed'),0),COALESCE(sum(i.state IN ('needs_verification','running')),0),COALESCE(sum(i.state IN ('skipped','cancelled')),0),COALESCE(sum(i.state IN ('queued','running')),0) FROM operation_items i JOIN operations o ON o.id=i.operation_id WHERE o.plan_id IN (SELECT plan_id FROM automation_claims WHERE batch_id=?)`, b.ID).Scan(&r.Completed, &r.Failed, &r.Unknown, &r.Skipped, &pending)
	if e != nil {
		return
	}
	if e = s.Store.DB.QueryRow("SELECT count(*) FROM operations WHERE plan_id IN (SELECT plan_id FROM automation_claims WHERE batch_id=?)", b.ID).Scan(&planned); e != nil {
		return
	}
	if r.Unknown == 0 && r.ErrorCode == "remote_state_unknown" {
		r.ErrorCode = ""
	}
	if activeTaskState(r.State) || r.State == "needs_verification" {
		if b.State == "empty" {
			r.State = "noop"
		} else if b.State == "observed" {
			r.State = "observed"
		} else if b.State == "cancelled" {
			r.State = "cancelled"
		} else if b.State == "submitted" && planned == len(b.PlanIDs) && pending == 0 {
			r.State = "completed"
			r.ErrorCode = ""
			if r.Completed == 0 {
				r.State = "noop"
			}
			if r.NeedsAI > 0 {
				r.State = "requires_ai"
			}
			if r.Failed > 0 {
				r.State = "failed"
				if r.Completed > 0 {
					r.State = "partial"
				}
				r.ErrorCode = "mail_operation_failed"
			}
			if r.Unknown > 0 {
				r.State = "needs_verification"
				r.ErrorCode = "remote_state_unknown"
			}
			if r.Skipped > 0 && r.State == "completed" {
				r.State = "partial"
				r.ErrorCode = "mail_operation_skipped"
			}
		}
	}
	// A running network write may finish after cancellation. Publish a final summary only after it settles.
	if pending > 0 {
		r.Finalized = false
		_ = s.saveTaskRun(r)
		return
	}
	if r.Unknown > 0 {
		r.State = "needs_verification"
		r.ErrorCode = "remote_state_unknown"
	}
	if !activeTaskState(r.State) {
		_ = s.finishTaskRun(r)
	} else {
		_ = s.saveTaskRun(r)
	}
}
func (s *Service) saveTaskRun(r TaskRun) error {
	r.UpdatedAt = time.Now().Unix()
	_, e := s.Store.DB.Exec("UPDATE task_runs SET state=?,payload=?,updated_at=? WHERE id=?", r.State, runJSON(r), r.UpdatedAt, r.ID)
	return e
}
func (s *Service) finishTaskRun(r TaskRun) error {
	t, e := s.loadTask(r.TaskID)
	if e != nil {
		return e
	}
	r.UpdatedAt = time.Now().Unix()
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var pending int
	if e = tx.QueryRow("SELECT count(*) FROM operations WHERE plan_id IN (SELECT plan_id FROM automation_claims WHERE batch_id=?) AND state IN ('queued','running')", r.BatchID).Scan(&pending); e != nil {
		return e
	}
	r.Finalized = pending == 0
	var previous string
	if e = tx.QueryRow("SELECT state FROM task_runs WHERE id=?", r.ID).Scan(&previous); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE task_runs SET state=?,payload=?,updated_at=? WHERE id=?", r.State, runJSON(r), r.UpdatedAt, r.ID); e != nil {
		return e
	}
	if t.Version == r.TaskVersion && r.BatchID != "" && !slices.Contains([]string{"failed", "interrupted", "cancelled"}, r.State) {
		t.Cursor = r.NextCursor
		if _, e = tx.Exec("UPDATE automation_tasks SET payload=? WHERE id=? AND version=? AND NOT EXISTS (SELECT 1 FROM task_runs newer WHERE newer.task_id=? AND newer.rowid>(SELECT rowid FROM task_runs WHERE id=?))", taskJSON(t), t.ID, t.Version, t.ID, r.ID); e != nil {
			return e
		}
	}
	if previous != r.State {
		if e = store.Audit(tx, "mail.task.result", r.ID, r.State); e != nil {
			return e
		}
	}
	if r.Finalized {
		if e = s.enqueueNotification(tx, t, r); e != nil {
			return e
		}
	}
	return tx.Commit()
}
