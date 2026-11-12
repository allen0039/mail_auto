package mail

import (
	"context"
	"strings"
	"testing"
)

// manualMoveWithAlso runs a manual "move to Archive + also" plan whose move is
// interrupted after the server returned COPYUID.
func manualMoveWithAlso(t *testing.T, also string, verifyErr error, unread, flagged bool) (*Service, *organizerRemote, *flagRemote, Access, Operation, Message) {
	t.Helper()
	s, base, admin, m := organizerFixture(t)
	remote := &flagRemote{automationRemote: &automationRemote{organizerRemote: base}}
	s.Remote = remote
	mustExec(t, s, "UPDATE messages SET flags_known=1,unread=?,flagged=? WHERE ref=?", unread, flagged, m.Ref)
	base.uncertain = true
	base.verifyErr = verifyErr
	op := executeFixture(t, s, admin, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: "Archive", Also: []string{also}})
	s.runOperation(context.Background(), op.ID)
	op, err := s.Operation(admin, op.ID)
	if err != nil || op.State != "needs_verification" {
		t.Fatal("setup", op, err)
	}
	base.uncertain = false
	return s, base, remote, admin, op, m
}

func assertFlagWarning(t *testing.T, op Operation, want string) {
	t.Helper()
	x := op.Items[0]
	if op.State != "completed" || x.State != "completed" || x.Error != "flags_not_applied" || x.RecoveryCode != "flags_not_applied" {
		t.Fatalf("recovered move must be completed with flags_not_applied: %+v", x)
	}
	if !strings.Contains(x.RecoveryDetail, want) || !strings.Contains(x.RecoveryDetail, "未执行") {
		t.Fatalf("warning must name the skipped change %q: %q", want, x.RecoveryDetail)
	}
}

func TestRecoveredMoveReportsSkippedFollowUpFlags(t *testing.T) {
	for _, tc := range []struct {
		also, want       string
		unread, flagged  bool
		alreadySatisfied bool
	}{
		{"mark_read", "标为已读", true, false, false},
		{"mark_unread", "标为未读", false, false, false},
		{"flag", "加星标", false, false, false},
		{"unflag", "取消星标", false, true, false},
		{"mark_read", "标为已读", false, false, true}, // already read when planned
	} {
		for _, path := range []string{"cleanup_finished", "source_already_gone"} {
			t.Run(tc.also+"/"+path+map[bool]string{true: "/already_satisfied"}[tc.alreadySatisfied], func(t *testing.T) {
				var verifyErr error = ErrUncertain // VerifyMove fails -> FinishMove cleans the source
				if path == "source_already_gone" {
					verifyErr = nil // VerifyMove confirms directly
				}
				s, base, remote, admin, op, m := manualMoveWithAlso(t, tc.also, verifyErr, tc.unread, tc.flagged)
				op, err := s.Reconcile(context.Background(), admin, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				assertFlagWarning(t, op, tc.want)
				if tc.alreadySatisfied != strings.Contains(op.Items[0].RecoveryDetail, "已是目标状态") {
					t.Fatal("already-satisfied note mismatch", op.Items[0].RecoveryDetail)
				}
				if len(remote.stores) != 0 {
					t.Fatal("recovery must not add flag writes", remote.stores)
				}
				wantCleanup := 1
				if path == "source_already_gone" {
					wantCleanup = 0
				}
				if base.moves != 1 || base.finishCallCount != wantCleanup {
					t.Fatal("recovery replayed MOVE or cleanup", base.moves, base.finishCallCount)
				}
				current, err := s.Message(m.Ref)
				if err != nil || current.Folder != "Archive" || current.Unread != tc.unread || current.Flagged != tc.flagged {
					t.Fatalf("index must show the moved message with unchanged flags: %+v %v", current, err)
				}
				// Repeated verification and a restart must keep the warning and
				// must not replay anything.
				if op, err = s.Reconcile(context.Background(), admin, op.ID); err != nil {
					t.Fatal(err)
				}
				mustExec(t, s, `UPDATE operation_items SET state='needs_verification',error_code='interrupted' WHERE state='running'; UPDATE operations SET state=CASE WHEN EXISTS(SELECT 1 FROM operation_items i WHERE i.operation_id=operations.id AND i.state='needs_verification') THEN 'needs_verification' ELSE 'queued' END WHERE state='running'`)
				if op, err = s.Operation(admin, op.ID); err != nil {
					t.Fatal(err)
				}
				assertFlagWarning(t, op, tc.want)
				if base.moves != 1 || base.finishCallCount != wantCleanup || len(remote.stores) != 0 {
					t.Fatal("re-verification replayed writes", base.moves, base.finishCallCount, remote.stores)
				}
			})
		}
	}
}

// Without follow-up flags a recovered move stays a plain completion.
func TestRecoveredMoveWithoutAlsoHasNoWarning(t *testing.T) {
	s, base, admin, m := organizerFixture(t)
	base.uncertain = true
	base.verifyErr = ErrUncertain
	op := executeFixture(t, s, admin, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: "Archive"})
	s.runOperation(context.Background(), op.ID)
	op, err := s.Reconcile(context.Background(), admin, op.ID)
	if err != nil || op.State != "completed" || op.Items[0].Error != "" || op.Items[0].RecoveryCode != "" {
		t.Fatalf("%+v %v", op, err)
	}
}

// A revoked flag grant cannot cause a flag write either: recovery never adds
// one, and the confirmed move is reported with the warning.
func TestRecoveredMoveAfterFlagGrantRevoked(t *testing.T) {
	s, base, admin, m := organizerFixture(t)
	remote := &flagRemote{automationRemote: &automationRemote{organizerRemote: base}}
	s.Remote = remote
	token, secret, err := s.CreateToken(TokenInput{Name: "mover", Accounts: []string{m.AccountID}, Actions: []string{"read", "move", "flag"}})
	if err != nil {
		t.Fatal(err)
	}
	client, err := s.AuthenticateToken(secret)
	if err != nil {
		t.Fatal(err)
	}
	base.uncertain = true
	base.verifyErr = ErrUncertain
	op := executeFixture(t, s, client, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: "Archive", Also: []string{"flag"}})
	s.runOperation(context.Background(), op.ID)
	if err = s.RevokeToken(token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Reconcile(context.Background(), client, op.ID); err == nil {
		t.Fatal("revoked client verified its operation")
	}
	op, err = s.Reconcile(context.Background(), admin, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertFlagWarning(t, op, "加星标")
	if len(remote.stores) != 0 || base.moves != 1 {
		t.Fatal("flag written after grant revoked", remote.stores, base.moves)
	}
}

// Automated rule "move + mark_read": the recovered item carries the warning
// and the run summary counts it as a flag warning, not as a failure.
func TestAutomatedRecoveryCountsFlagWarningInRun(t *testing.T) {
	s, remote, admin, _, task, message := taskFixture(t, "execute", "manual", "rules")
	fr := &flagRemote{automationRemote: remote}
	s.Remote = fr
	p, err := s.AutomationPolicy(admin, message.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	p.Actions = []string{"move", "flag"}
	if _, err = s.SaveAutomationPolicy(admin, p); err != nil {
		t.Fatal(err)
	}
	rules, err := s.Rules(admin, message.AccountID)
	if err != nil || len(rules) != 1 {
		t.Fatal(rules, err)
	}
	rules[0].Also = []string{"flag"}
	if _, err = s.SaveRule(admin, rules[0]); err != nil {
		t.Fatal(err)
	}
	if task, err = s.SaveTask(admin, task); err != nil { // re-bind the changed rule version
		t.Fatal(err)
	}
	remote.uncertain = true
	remote.verifyErr = ErrUncertain
	run, err := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "auto-also"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, "UPDATE task_runs SET state='running',lease_owner=?,lease_until=strftime('%s','now')+60 WHERE id=?", s.workerID, run.ID)
	access := Access{Owner: run.Owner, Version: run.AdminVersion}
	batch, err := s.ClaimAI(access, AIClaimInput{RunID: run.ID, AccountID: message.AccountID})
	if err != nil || len(batch.Items) != 1 || len(batch.Items[0].Also) != 1 {
		t.Fatal(batch, err)
	}
	if batch, err = s.SubmitAI(context.Background(), access, AISubmitInput{BatchID: batch.ID, IdempotencyKey: "auto-also", Decisions: decisionFor(batch)}); err != nil {
		t.Fatal(err)
	}
	op, err := s.ExecuteAI(access, batch.ID, batch.PlanIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	s.runOperation(context.Background(), op.ID)
	s.reconcileTaskRun(run.ID)
	if op, err = s.Reconcile(context.Background(), access, op.ID); err != nil {
		t.Fatal(err)
	}
	assertFlagWarning(t, op, "加星标")
	if len(fr.stores) != 0 || remote.moves != 1 || remote.finishExpunges != 1 {
		t.Fatal("automated recovery wrote flags or replayed", fr.stores, remote.moves, remote.finishExpunges)
	}
	s.reconcileTaskRun(run.ID)
	r, _ := s.loadTaskRun(run.ID)
	if r.State != "completed" || r.Completed != 1 || r.Failed != 0 || r.Unknown != 0 || r.FlagWarnings != 1 {
		t.Fatalf("run summary: state=%s completed=%d failed=%d unknown=%d flag_warnings=%d", r.State, r.Completed, r.Failed, r.Unknown, r.FlagWarnings)
	}
}

// The normal path keeps applying follow-up flags; when that STORE fails the
// item now also explains the warning.
func TestNormalMoveFlagFailureExplainsWarning(t *testing.T) {
	s, base, admin, m := organizerFixture(t)
	remote := &flagRemote{automationRemote: &automationRemote{organizerRemote: base}, failNext: true}
	s.Remote = remote
	op := executeFixture(t, s, admin, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: "Archive", Also: []string{"mark_read"}})
	s.runOperation(context.Background(), op.ID)
	op, err := s.Operation(admin, op.ID)
	if err != nil || op.State != "completed" || op.Items[0].Error != "flags_not_applied" || !strings.Contains(op.Items[0].Detail, "标为已读") {
		t.Fatalf("%+v %v", op.Items, err)
	}
}

// With the "all" policy a completed run notifies; its text names the warning.
func TestNotificationMentionsFlagWarnings(t *testing.T) {
	s, _, admin, _, task, _ := taskFixture(t, "observe", "manual", "rules")
	configureNotification(t, s, admin, "all")
	r, err := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "notify-warning"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	r.State, r.Completed, r.FlagWarnings = "completed", 3, 2
	if err = s.finishTaskRun(r); err != nil {
		t.Fatal(err)
	}
	var payload string
	if err = s.Store.DB.QueryRow("SELECT payload FROM notification_outbox WHERE run_id=?", r.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, "附加标记未应用或未确认 2 封") {
		t.Fatal(payload)
	}
}
