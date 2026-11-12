package mail

import (
	"context"
	"testing"
	"time"
)

// age moves an operation's creation and last-attempt times into the past,
// which is how a real deployment sees operations left by an older version.
func age(t *testing.T, u uncertainRun, created, lastAttempt time.Duration) {
	t.Helper()
	now := time.Now()
	mustExec(t, u.s, "UPDATE operations SET created_at=?,updated_at=? WHERE id=?", now.Add(-created).Unix(), now.Add(-lastAttempt).Unix(), u.operation)
}

// tick runs one background pass exactly like the worker loop does.
func tick(s *Service) bool {
	now := time.Now()
	if s.reconcileNextOperation(context.Background(), now.Add(-operationReconcileDelay), now.Add(-operationReconcileWindow), now, false) {
		return true
	}
	return s.reconcileNextOperation(context.Background(), now.Add(-operationSlowReconcileDelay), now.Add(-operationSlowReconcileWindow), now.Add(-operationReconcileWindow), true)
}

func audits(t *testing.T, s *Service, action, target string) int {
	t.Helper()
	var n int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM audits WHERE action=? AND target=?", action, target).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReconcileWindowLanes(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		created, lastAttempt time.Duration
		picked, slow         bool
		mode                 string
	}{
		{"fresh_quiet", time.Hour, time.Minute, true, false, "auto"},
		{"fresh_just_tried", time.Hour, 0, false, false, "auto"},
		{"day_old_quiet", 25 * time.Hour, 7 * time.Hour, true, true, "slow"},
		{"day_old_recently_tried", 25 * time.Hour, time.Hour, false, false, "slow"},
		{"month_old", 31 * 24 * time.Hour, 30 * 24 * time.Hour, false, false, "manual"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := startUncertainMove(t, tc.name)
			u.s.reconcileTaskRun(u.run.ID)
			age(t, u, tc.created, tc.lastAttempt)
			before, err := u.s.Operation(u.admin, u.operation)
			if err != nil || before.ReconcileMode != tc.mode {
				t.Fatal("mode", before.ReconcileMode, err)
			}
			if tick(u.s) != tc.picked {
				t.Fatal("picked", !tc.picked)
			}
			op, _ := u.s.Operation(u.admin, u.operation)
			if tc.picked && (op.State != "completed" || u.remote.finishExpunges != 1) {
				t.Fatalf("picked but not recovered: %+v", op)
			}
			if !tc.picked && (op.State != "needs_verification" || u.remote.finishCallCount != 0) {
				t.Fatalf("not picked but touched: %+v", op)
			}
			if got := audits(t, u.s, "mail.operation.reconcile.slow", u.operation); (got == 1) != tc.slow {
				t.Fatal("slow-lane audit", got)
			}
			if u.remote.moves != 1 {
				t.Fatal("MOVE replayed", u.remote.moves)
			}
		})
	}
}

// The slow lane claims the operation before working on it, so a blocked old
// item is retried at most once per six hours instead of every second.
func TestSlowLaneRateLimitsBlockedOperation(t *testing.T) {
	u := startUncertainMove(t, "slow-blocked")
	u.s.reconcileTaskRun(u.run.ID)
	u.remote.flagged = true // stays blocked: fingerprint mismatch
	age(t, u, 30*time.Hour, 7*time.Hour)
	if !tick(u.s) {
		t.Fatal("first slow pass not taken")
	}
	for i := 0; i < 3; i++ {
		if tick(u.s) {
			t.Fatal("blocked old item retried without waiting")
		}
	}
	op, _ := u.s.Operation(u.admin, u.operation)
	if op.State != "needs_verification" || op.Items[0].RecoveryCode != "recovery_fingerprint_mismatch" || op.ReconcileMode != "slow" || op.NextCheckAt < time.Now().Add(5*time.Hour).Unix() {
		t.Fatalf("%+v", op)
	}
	if u.remote.finishStores != 0 || u.remote.moves != 1 {
		t.Fatal("blocked item touched mailbox")
	}
}

// Beyond the automatic windows an item is never dropped: it stays listed and a
// manual verification runs the full checks, is audited and never replays MOVE.
func TestManualVerificationOfOldOperations(t *testing.T) {
	t.Run("mapped", func(t *testing.T) {
		u := startUncertainMove(t, "old-mapped")
		u.s.reconcileTaskRun(u.run.ID)
		age(t, u, 40*24*time.Hour, 40*24*time.Hour)
		if tick(u.s) {
			t.Fatal("background touched a 40-day-old item")
		}
		ops, err := u.s.Operations(u.admin)
		if err != nil || len(ops) == 0 || ops[0].ID != u.operation || ops[0].ReconcileMode != "manual" {
			t.Fatal("old item not listed for manual handling", ops, err)
		}
		op, err := u.s.Reconcile(context.Background(), u.admin, u.operation)
		if err != nil || op.State != "completed" || u.remote.finishExpunges != 1 || u.remote.moves != 1 {
			t.Fatal(op, err)
		}
		if audits(t, u.s, "mail.operation.reconcile.manual", u.operation) != 1 {
			t.Fatal("manual verification not audited")
		}
	})
	t.Run("revoked_after_restart", func(t *testing.T) {
		u := startUncertainMove(t, "old-revoked")
		u.s.workerID = "after-restart"
		u.s.reconcileTaskRun(u.run.ID)
		task, err := u.s.loadTask(u.run.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = u.s.SaveTask(u.admin, task); err != nil {
			t.Fatal(err)
		}
		age(t, u, 26*time.Hour, 7*time.Hour)
		if !tick(u.s) {
			t.Fatal("slow lane skipped item")
		}
		op, _ := u.s.Operation(u.admin, u.operation)
		if op.State != "needs_verification" || op.Items[0].RecoveryCode != "recovery_access_revoked" {
			t.Fatalf("%+v", op.Items[0])
		}
		if op, _ = u.s.Reconcile(context.Background(), u.admin, u.operation); op.State != "needs_verification" || op.Items[0].RecoveryCode != "recovery_access_revoked" || u.remote.finishStores != 0 {
			t.Fatalf("manual verification bypassed revoked task: %+v", op.Items[0])
		}
	})
}
