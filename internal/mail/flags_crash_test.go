package mail

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mail-auto/internal/store"
)

// crashingFlags blocks the follow-up flag STORE of a normal move so a test can
// snapshot the database exactly as a process crash at that point would leave
// it. With applied set, the STORE first reaches the (simulated) server and
// only the local confirmation is lost.
type crashingFlags struct {
	*flagRemote
	applied bool
	entered chan struct{}
}

func (r *crashingFlags) SetFlags(ctx context.Context, a Account, pw string, m Message, change FlagChange, validate func(Message) error) (Message, error) {
	if r.applied {
		if _, err := r.flagRemote.SetFlags(ctx, a, pw, m, change, validate); err != nil {
			return Message{}, err
		}
	}
	close(r.entered)
	<-ctx.Done()
	return Message{}, ctx.Err()
}

// crashSnapshot runs op until the flag STORE blocks, then copies the
// database with Store.Backup. The original worker stays blocked until the
// snapshot exists, so nothing it writes later can leak into the copy.
func crashSnapshot(t *testing.T, s *Service, remote *crashingFlags, opID string) *store.Store {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runOperation(ctx, opID)
	}()
	select {
	case <-remote.entered:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("worker did not reach the follow-up flag write")
	}
	dir := t.TempDir()
	err := s.Store.Backup(filepath.Join(dir, "mail-auto.db"))
	cancel()
	<-done
	if err != nil {
		t.Fatal(err)
	}
	copyStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { copyStore.Close() })
	return copyStore
}

// restartFrom starts a new Service on st, as after a process restart, and
// waits until the recovered operation reaches a final state.
func restartFrom(t *testing.T, s *Service, st *store.Store, remote Remote, a Access, opID string) (*Service, Operation) {
	t.Helper()
	restarted := &Service{Store: st, Vault: s.Vault, Remote: remote, workerID: s.workerID}
	if err := restarted.StartOperations(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Stop)
	deadline := time.Now().Add(5 * time.Second)
	for {
		op, err := restarted.Operation(a, opID)
		if err != nil {
			t.Fatal(err)
		}
		if !slicesContains([]string{"queued", "running"}, op.State) {
			return restarted, op
		}
		if time.Now().After(deadline) {
			t.Fatal("restarted operation did not settle", op.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// queueFixture freezes and queues a plan without running it.
func queueFixture(t *testing.T, s *Service, a Access, in PlanInput) Operation {
	t.Helper()
	p, err := s.NewPlan(context.Background(), a, in)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.ExecutePlan(a, p.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func slicesContains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func assertPendingFlagWarning(t *testing.T, op Operation, want string) {
	t.Helper()
	if len(op.Items) != 1 {
		t.Fatal(op.Items)
	}
	x := op.Items[0]
	if op.State != "completed" || x.State != "completed" || x.Error != "flags_not_applied" || x.RecoveryCode != "flags_not_applied" {
		t.Fatalf("restarted move must complete with flags_not_applied: op=%s %+v", op.State, x)
	}
	for _, part := range []string{want, "未确认完成", "可能未应用", "不会补写"} {
		if !strings.Contains(x.RecoveryDetail, part) {
			t.Fatalf("warning must contain %q: %q", part, x.RecoveryDetail)
		}
	}
	if x.Result.Folder != "Archive" || x.Result.UIDValidity != 7 || x.Result.UID != 101 {
		t.Fatalf("warning must keep the confirmed destination identity: %+v", x.Result)
	}
}

// Crash after the move is published and before (or during) the follow-up
// STORE: the restart reports a pending warning and replays nothing.
func TestNormalMoveCrashBeforeFlagConfirmationKeepsWarning(t *testing.T) {
	for _, tc := range []struct {
		also, want      string
		unread, flagged bool
		satisfied       bool
	}{
		{"mark_read", "标为已读", true, false, false},
		{"mark_unread", "标为未读", false, false, false},
		{"flag", "加星标", false, false, false},
		{"unflag", "取消星标", false, true, false},
		{"flag", "加星标", false, true, true}, // already flagged when planned
	} {
		for _, applied := range []bool{false, true} {
			name := tc.also + map[bool]string{false: "/before_store", true: "/store_applied_unconfirmed"}[applied] + map[bool]string{true: "/already_satisfied"}[tc.satisfied]
			t.Run(name, func(t *testing.T) {
				s, base, admin, m := organizerFixture(t)
				inner := &flagRemote{automationRemote: &automationRemote{organizerRemote: base}}
				remote := &crashingFlags{flagRemote: inner, applied: applied, entered: make(chan struct{})}
				s.Remote = remote
				mustExec(t, s, "UPDATE messages SET flags_known=1,unread=?,flagged=? WHERE ref=?", tc.unread, tc.flagged, m.Ref)
				op := queueFixture(t, s, admin, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: "Archive", Also: []string{tc.also}})
				st := crashSnapshot(t, s, remote, op.ID)
				wantStores := map[bool]int{false: 0, true: 1}[applied]
				if base.moves != 1 || len(inner.stores) != wantStores {
					t.Fatal("setup writes", base.moves, inner.stores)
				}
				restarted, op := restartFrom(t, s, st, inner, admin, op.ID)
				assertPendingFlagWarning(t, op, tc.want)
				if tc.satisfied != strings.Contains(op.Items[0].RecoveryDetail, "已是目标状态") {
					t.Fatal("already-satisfied note mismatch", op.Items[0].RecoveryDetail)
				}
				if base.moves != 1 || len(inner.stores) != wantStores || base.finishCallCount != 0 {
					t.Fatal("restart replayed MOVE, flags or cleanup", base.moves, inner.stores, base.finishCallCount)
				}
				// The index is rebound to the destination; unconfirmed flags are
				// not claimed in it.
				current, err := restarted.Message(m.Ref)
				if err != nil || current.Folder != "Archive" || current.UID != 101 || current.Unread != tc.unread || current.Flagged != tc.flagged {
					t.Fatalf("index after restart: %+v %v", current, err)
				}
				// Verification has nothing to do and keeps the warning.
				if op, err = restarted.Reconcile(context.Background(), admin, op.ID); err != nil {
					t.Fatal(err)
				}
				assertPendingFlagWarning(t, op, tc.want)
				if base.moves != 1 || len(inner.stores) != wantStores {
					t.Fatal("reconcile replayed writes", base.moves, inner.stores)
				}
			})
		}
	}
}

// A move without follow-up flags keeps the plain completion across the same
// restart.
func TestNormalMoveWithoutAlsoCrashAfterPublish(t *testing.T) {
	s, base, admin, m := organizerFixture(t)
	op := executeFixture(t, s, admin, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: "Archive"})
	// Crash after the move transaction, before the operation summary.
	mustExec(t, s, "UPDATE operations SET state='running' WHERE id=?", op.ID)
	_, op = restartFrom(t, s, s.Store, s.Remote, admin, op.ID)
	x := op.Items[0]
	if op.State != "completed" || x.State != "completed" || x.Error != "" || x.RecoveryCode != "" || x.RecoveryDetail != "" || base.moves != 1 {
		t.Fatalf("%s %+v moves=%d", op.State, x, base.moves)
	}
}

// Confirmed and indexed flags clear the provisional warning atomically: a
// crash right after that (before the item/operation summary) and a later
// restart leave a clean completion without replays.
func TestNormalMoveConfirmedFlagsSurviveRestartWithoutWarning(t *testing.T) {
	for _, tc := range []struct {
		also                  string
		unread, flagged       bool
		wantUnread, wantFlags bool
	}{
		{"mark_read", true, false, false, false},
		{"mark_unread", false, false, true, false},
		{"flag", false, false, false, true},
		{"unflag", false, true, false, false},
	} {
		t.Run(tc.also, func(t *testing.T) {
			s, base, admin, m := organizerFixture(t)
			remote := &flagRemote{automationRemote: &automationRemote{organizerRemote: base}}
			s.Remote = remote
			mustExec(t, s, "UPDATE messages SET flags_known=1,unread=?,flagged=? WHERE ref=?", tc.unread, tc.flagged, m.Ref)
			op := executeFixture(t, s, admin, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: "Archive", Also: []string{tc.also}})
			op, err := s.Operation(admin, op.ID)
			if err != nil || op.State != "completed" || op.Items[0].Error != "" || op.Items[0].RecoveryCode != "" || op.Items[0].RecoveryDetail != "" || op.Items[0].Detail != "" {
				t.Fatalf("confirmed flags must leave no warning: %+v %v", op.Items, err)
			}
			mustExec(t, s, "UPDATE operations SET state='running' WHERE id=?", op.ID)
			restarted, op := restartFrom(t, s, s.Store, remote, admin, op.ID)
			if op.State != "completed" || op.Items[0].State != "completed" || op.Items[0].Error != "" || op.Items[0].RecoveryDetail != "" {
				t.Fatalf("false warning after restart: %s %+v", op.State, op.Items[0])
			}
			if base.moves != 1 || len(remote.stores) != 1 {
				t.Fatal("restart replayed writes", base.moves, remote.stores)
			}
			current, err := restarted.Message(m.Ref)
			if err != nil || current.Folder != "Archive" || !current.FlagsKnown || current.Unread != tc.wantUnread || current.Flagged != tc.wantFlags {
				t.Fatalf("index must reflect confirmed flags: %+v %v", current, err)
			}
		})
	}
}

// When the STORE succeeds but the index no longer holds the confirmed
// destination, the result is not reliably recorded and the warning stays.
func TestNormalMoveFlagsUnindexedKeepsWarning(t *testing.T) {
	s, base, admin, m := organizerFixture(t)
	remote := &flagRemote{automationRemote: &automationRemote{organizerRemote: base}}
	s.Remote = remote
	op := executeFixture(t, s, admin, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: "Archive", Also: []string{"flag"}})
	plan, err := s.GetPlan(admin, op.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	moved := plan.Items[0].Message
	moved.Folder, moved.UIDValidity, moved.UID = "Archive", 7, 999 // not indexed
	if err = s.publishMovedFlags(op.ID, 0, moved, moved); err == nil {
		t.Fatal("unindexed flags must not clear the warning")
	}
}

// The normal STORE failure is reported with the final (not the pending) text.
func TestNormalMoveFlagFailureReplacesPendingWarning(t *testing.T) {
	s, base, admin, m := organizerFixture(t)
	remote := &flagRemote{automationRemote: &automationRemote{organizerRemote: base}, failNext: true}
	s.Remote = remote
	op := executeFixture(t, s, admin, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: "Archive", Also: []string{"unflag"}})
	op, err := s.Operation(admin, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	x := op.Items[0]
	if op.State != "completed" || x.State != "completed" || x.Error != "flags_not_applied" || x.RecoveryCode != "flags_not_applied" || x.Detail != x.RecoveryDetail || !strings.Contains(x.RecoveryDetail, "标记写入失败或结果未确认") || !strings.Contains(x.RecoveryDetail, "未确认完成") || strings.Contains(x.RecoveryDetail, "未应用：") || base.moves != 1 {
		t.Fatalf("%s %+v", op.State, x)
	}
}

type appliedFlagsWithLostResponse struct {
	*flagRemote
	serverState Message
}

func (remote *appliedFlagsWithLostResponse) SetFlags(ctx context.Context, account Account, password string, message Message, change FlagChange, validate func(Message) error) (Message, error) {
	fresh, err := remote.flagRemote.SetFlags(ctx, account, password, message, change, validate)
	if err != nil {
		return Message{}, err
	}
	remote.serverState = fresh
	return Message{}, errors.New("response lost after STORE")
}

func TestNormalMoveFlagsAppliedWithLostResponseReportsUnconfirmed(t *testing.T) {
	service, base, admin, message := organizerFixture(t)
	remote := &appliedFlagsWithLostResponse{flagRemote: &flagRemote{automationRemote: &automationRemote{organizerRemote: base}}}
	service.Remote = remote
	mustExec(t, service, "UPDATE messages SET flags_known=1,unread=0,flagged=0 WHERE ref=?", message.Ref)
	operation := executeFixture(t, service, admin, PlanInput{Refs: []string{message.Ref}, Action: "move", Destination: "Archive", Also: []string{"flag"}})
	operation, err := service.Operation(admin, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := operation.Items[0]
	if !remote.serverState.Flagged || base.moves != 1 || len(remote.stores) != 1 || base.finishCallCount != 0 {
		t.Fatalf("unexpected remote effects: flagged=%t moves=%d stores=%d cleanup=%d", remote.serverState.Flagged, base.moves, len(remote.stores), base.finishCallCount)
	}
	if operation.State != "completed" || item.State != "completed" || item.Error != "flags_not_applied" || item.RecoveryCode != "flags_not_applied" || item.Detail != item.RecoveryDetail {
		t.Fatalf("unexpected operation result: %s %+v", operation.State, item)
	}
	for _, expected := range []string{"加星标", "未确认完成", "可能未应用", "也可能已在服务器生效"} {
		if !strings.Contains(item.RecoveryDetail, expected) {
			t.Fatalf("warning must contain %q: %q", expected, item.RecoveryDetail)
		}
	}
	if strings.Contains(item.RecoveryDetail, "未应用：") || strings.Contains(item.RecoveryDetail, "未执行") {
		t.Fatal("unknown outcome reported as definitely unapplied", item.RecoveryDetail)
	}
	current, err := service.Message(message.Ref)
	if err != nil || current.Folder != "Archive" || current.UIDValidity != 7 || current.UID != 101 || current.Flagged {
		t.Fatalf("unconfirmed index state: %+v %v", current, err)
	}
	if _, err = service.Reconcile(context.Background(), admin, operation.ID); err != nil {
		t.Fatal(err)
	}
	if base.moves != 1 || len(remote.stores) != 1 || base.finishCallCount != 0 {
		t.Fatal("verification replayed remote writes")
	}
}

// Automated rule "move + flag" crashing before the follow-up STORE: after the
// restart the run is completed, counts the move and reports the flag warning.
func TestAutomatedMoveCrashBeforeFlagsCountsWarningInRun(t *testing.T) {
	s, base, admin, _, task, message := taskFixture(t, "execute", "manual", "rules")
	inner := &flagRemote{automationRemote: base}
	remote := &crashingFlags{flagRemote: inner, entered: make(chan struct{})}
	s.Remote = remote
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
	if task, err = s.SaveTask(admin, task); err != nil {
		t.Fatal(err)
	}
	run, err := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "crash-flags"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, "UPDATE task_runs SET state='running',lease_owner=?,lease_until=strftime('%s','now')+60 WHERE id=?", s.workerID, run.ID)
	access := Access{Owner: run.Owner, Version: run.AdminVersion}
	batch, err := s.ClaimAI(access, AIClaimInput{RunID: run.ID, AccountID: message.AccountID})
	if err != nil || len(batch.Items) != 1 {
		t.Fatal(batch, err)
	}
	if batch, err = s.SubmitAI(context.Background(), access, AISubmitInput{BatchID: batch.ID, IdempotencyKey: "crash-flags", Decisions: decisionFor(batch)}); err != nil {
		t.Fatal(err)
	}
	op, err := s.ExecuteAI(access, batch.ID, batch.PlanIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	st := crashSnapshot(t, s, remote, op.ID)
	restarted, op := restartFrom(t, s, st, inner, access, op.ID)
	assertPendingFlagWarning(t, op, "加星标")
	if base.moves != 1 || len(inner.stores) != 0 || base.finishExpunges != 0 {
		t.Fatal("restart replayed writes", base.moves, inner.stores, base.finishExpunges)
	}
	restarted.reconcileTaskRun(run.ID)
	r, err := restarted.loadTaskRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "completed" || r.Completed != 1 || r.Failed != 0 || r.Unknown != 0 || r.FlagWarnings != 1 {
		t.Fatalf("run summary: state=%s completed=%d failed=%d unknown=%d flag_warnings=%d", r.State, r.Completed, r.Failed, r.Unknown, r.FlagWarnings)
	}
}
