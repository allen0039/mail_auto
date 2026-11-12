package mail

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

type providerTimeoutFixture struct{}

func (providerTimeoutFixture) Error() string              { return "private provider detail" }
func (providerTimeoutFixture) OperationErrorCode() string { return "provider_timeout" }

func TestOperationProviderCodePreservesUncertaintyAndRedaction(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", providerTimeoutFixture{})
	if errorCode(err) != "provider_timeout" {
		t.Fatal("provider timeout collapsed to generic failure", errorCode(err))
	}
	if code := errorCode(fmt.Errorf("%w: %w", ErrUncertain, err)); code != "remote_state_unknown" {
		t.Fatal("uncertain write must not be treated as safely retryable", code)
	}
	if syncErrorMessage(err) != "同步处理失败" {
		t.Fatal("raw provider error exposed")
	}
}

type organizerRemote struct {
	fakeRemote
	moves            int
	uncertain        bool
	omitMapping      bool
	verifyErr        error
	inspectEvidence  MoveEvidence
	inspectCallCount int
	finishErr        error
	finishCallCount  int
	finishResult     MoveResult
	// finishFresh simulates the source state read on the cleanup session:
	// check 0 runs before STORE \Deleted, check 1 before UID EXPUNGE.
	finishFresh    func(check int, m Message) Message
	finishStores   int
	finishUndos    int
	finishExpunges int
}

func (f *organizerRemote) Capabilities(context.Context, Account, string) (Capabilities, error) {
	return Capabilities{Move: true, UIDPlus: true, Folders: []FolderInfo{{Name: "INBOX"}, {Name: "Archive"}, {Name: "Trash"}}}, nil
}
func (f *organizerRemote) Move(_ context.Context, _ Account, _ string, m Message, dest string, record func(MoveResult) error) (MoveResult, error) {
	f.moves++
	r := MoveResult{Folder: dest, UIDValidity: 7, UID: uint32(100 + f.moves)}
	if f.omitMapping {
		r = MoveResult{Folder: dest}
	}
	if r.UID != 0 {
		if e := record(r); e != nil {
			return r, e
		}
	}
	if f.uncertain {
		return r, ErrUncertain
	}
	return r, nil
}
func (f *organizerRemote) VerifyMove(context.Context, Account, string, Message, MoveResult) error {
	return f.verifyErr
}
func (f *organizerRemote) InspectMove(context.Context, Account, string, Message, string) (MoveEvidence, error) {
	f.inspectCallCount++
	return f.inspectEvidence, nil
}
func (f *organizerRemote) FinishMove(_ context.Context, _ Account, _ string, m Message, result MoveResult, validate func(Message) error) error {
	f.finishCallCount++
	f.finishResult = result
	if f.finishErr != nil {
		return f.finishErr
	}
	for check := 0; check < 2; check++ {
		fresh := m
		if f.finishFresh != nil {
			fresh = f.finishFresh(check, fresh)
		}
		if err := validate(fresh); err != nil {
			if check == 1 {
				f.finishUndos++
			}
			return err
		}
		if check == 0 {
			f.finishStores++
		}
	}
	f.finishExpunges++
	return nil
}
func organizerFixture(t *testing.T) (*Service, *organizerRemote, Access, Message) {
	t.Helper()
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	v, e := credentials.Open(t.TempDir(), true)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = st.DB.Exec("INSERT INTO admin VALUES(1,'test','unused',0,1,0,1)"); e != nil {
		t.Fatal(e)
	}
	r := &organizerRemote{fakeRemote: fakeRemote{folders: []string{"INBOX", "Archive", "Trash"}, snapshot: Snapshot{UIDValidity: 7, Messages: []Message{{UID: 1, Subject: "fixture", Sender: "a@example.com", SentAt: 123}}}}}
	s := &Service{Store: st, Vault: v, Remote: r}
	a, e := s.Create(AccountInput{Name: "test", Provider: "qq", Username: "test@qq.com", Password: "fixture"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Sync(context.Background(), a.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.SaveFolderSettings(context.Background(), FolderSettings{AccountID: a.ID, Archive: "Archive", Trash: "Trash", WritesEnabled: true}); e != nil {
		t.Fatal(e)
	}
	ms, _, e := s.Search(Search{AccountID: a.ID, Folder: "INBOX"})
	if e != nil || len(ms) != 1 {
		t.Fatal(ms, e)
	}
	access, e := s.AdminAccess()
	if e != nil {
		t.Fatal(e)
	}
	return s, r, access, ms[0]
}
func executeFixture(t *testing.T, s *Service, a Access, in PlanInput) Operation {
	t.Helper()
	p, e := s.NewPlan(context.Background(), a, in)
	if e != nil {
		t.Fatal(e)
	}
	o, e := s.ExecutePlan(a, p.ID, p.ID)
	if e != nil {
		t.Fatal(e)
	}
	retry, e := s.ExecutePlan(a, p.ID, p.ID)
	if e != nil || retry.ID != o.ID {
		t.Fatal("idempotency", retry, e)
	}
	if _, e = s.ExecutePlan(a, p.ID, "another-key"); !errors.Is(e, ErrConflict) {
		t.Fatal("duplicate plan", e)
	}
	s.runOperation(context.Background(), o.ID)
	o, e = s.Operation(a, o.ID)
	if e != nil {
		t.Fatal(e)
	}
	return o
}
func TestOrganizerMoveRestoreAndStableLabels(t *testing.T) {
	s, r, a, m := organizerFixture(t)
	if _, e := s.CreateLabel("账单", "#123abc"); e != nil {
		t.Fatal(e)
	}
	// Historical local metadata is preserved through migration and remote moves.
	if e := s.SetMessageLabels(m.Ref, []string{"账单"}, nil); e != nil {
		t.Fatal(e)
	}
	o := executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "trash"})
	if o.State != "completed" || r.moves != 1 {
		t.Fatal(o, r.moves)
	}
	moved, e := s.Message(m.Ref)
	if e != nil || moved.Folder != "Trash" || len(moved.Labels) != 1 {
		t.Fatal(moved, e)
	}
	// A subsequent metadata refresh at the destination must retain the stable ref.
	r.snapshot = Snapshot{UIDValidity: 7, Messages: []Message{{UID: moved.UID, Subject: "refreshed", SentAt: 123}}}
	if _, e = s.Sync(context.Background(), m.AccountID); e != nil {
		t.Fatal(e)
	}
	moved, e = s.Message(m.Ref)
	if e != nil || len(moved.Labels) != 1 {
		t.Fatal("sync lost labels", moved, e)
	}
	o = executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "restore"})
	if o.State != "completed" {
		t.Fatal(o)
	}
	moved, e = s.Message(m.Ref)
	if e != nil || moved.Folder != "INBOX" || len(moved.Labels) != 1 {
		t.Fatal(moved, e)
	}
}
func TestOrganizerUncertainReconcileNeverRetriesMove(t *testing.T) {
	s, r, a, m := organizerFixture(t)
	r.uncertain = true
	o := executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "archive"})
	if o.State != "needs_verification" {
		t.Fatal(o)
	}
	o, e := s.Reconcile(context.Background(), a, o.ID)
	if e != nil || o.State != "completed" || r.moves != 1 {
		t.Fatal(o, e, r.moves)
	}
	if _, e = s.Reconcile(context.Background(), a, o.ID); e != nil || r.moves != 1 {
		t.Fatal("reissued MOVE", e, r.moves)
	}
}

func TestOrganizerWorkerReconcilesUncertainOperation(t *testing.T) {
	s, r, a, m := organizerFixture(t)
	r.uncertain = true
	o := executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "archive"})
	if o.State != "needs_verification" {
		t.Fatal(o)
	}
	if !s.reconcileNextOperation(context.Background(), time.Now().Add(time.Second), time.Now().Add(-time.Hour), time.Now().Add(time.Second), false) {
		t.Fatal("uncertain operation was not selected for recovery")
	}
	o, e := s.Operation(a, o.ID)
	if e != nil || o.State != "completed" {
		t.Fatal(o, e)
	}
	if r.moves != 1 {
		t.Fatal("worker replayed MOVE", r.moves)
	}
	if s.reconcileNextOperation(context.Background(), time.Now().Add(2*time.Second), time.Now().Add(-time.Hour), time.Now().Add(2*time.Second), false) {
		t.Fatal("completed operation was selected again")
	}
}
func TestOrganizerMissingMappingReconcileUsesLiveEvidence(t *testing.T) {
	t.Run("source_still_present_marks_failed", func(t *testing.T) {
		s, r, a, m := organizerFixture(t)
		r.omitMapping = true
		r.uncertain = true
		r.inspectEvidence = MoveEvidence{SourcePresent: true}
		o := executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "archive"})
		if o.State != "needs_verification" {
			t.Fatal(o)
		}
		o, e := s.Reconcile(context.Background(), a, o.ID)
		if e != nil || o.State != "failed" || o.Items[0].State != "failed" || o.Items[0].Error != "move_not_applied" {
			t.Fatal(o, e)
		}
		if r.moves != 1 || r.inspectCallCount != 1 {
			t.Fatal("reconcile reissued MOVE", r.moves, r.inspectCallCount)
		}
	})

	t.Run("metadata_match_without_copyuid_stays_uncertain", func(t *testing.T) {
		s, r, a, m := organizerFixture(t)
		r.omitMapping = true
		r.uncertain = true
		r.inspectEvidence = MoveEvidence{
			DestinationPresent: true,
			Destination:        MoveResult{Folder: "Archive", UIDValidity: 7, UID: 99},
		}
		o := executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "archive"})
		if o.State != "needs_verification" {
			t.Fatal(o)
		}
		o, e := s.Reconcile(context.Background(), a, o.ID)
		if e != nil || o.State != "needs_verification" || o.Items[0].State != "needs_verification" {
			t.Fatal(o, e)
		}
		moved, e := s.Message(m.Ref)
		if e != nil || moved.Folder != m.Folder || moved.UID != m.UID {
			t.Fatal(moved, e)
		}
		if r.moves != 1 || r.finishCallCount != 0 {
			t.Fatal("reconcile reissued MOVE", r.moves)
		}
	})
}

func TestOrganizerReconcileFinishesCopyWhenSourceCleanupWasInterrupted(t *testing.T) {
	s, r, a, m := organizerFixture(t)
	r.uncertain = true
	r.verifyErr = ErrUncertain
	r.inspectEvidence = MoveEvidence{
		SourcePresent:      true,
		DestinationPresent: true,
		Destination:        MoveResult{Folder: "Archive", UIDValidity: 7, UID: 101},
	}
	o := executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "archive"})
	if o.State != "needs_verification" {
		t.Fatal(o)
	}
	o, e := s.Reconcile(context.Background(), a, o.ID)
	if e != nil || o.State != "completed" || o.Items[0].State != "completed" {
		t.Fatal(o, e)
	}
	if r.moves != 1 || r.finishCallCount != 1 || r.inspectCallCount != 0 || r.finishResult != o.Items[0].Result {
		t.Fatal("reconcile reissued MOVE or skipped verified cleanup", r.moves, r.finishCallCount, r.inspectCallCount)
	}
	moved, e := s.Message(m.Ref)
	if e != nil || moved.Folder != "Archive" || moved.UID != 101 {
		t.Fatal(moved, e)
	}
}

func TestOrganizerReconcileKeepsCleanupFailureUncertain(t *testing.T) {
	s, r, a, m := organizerFixture(t)
	r.uncertain = true
	r.verifyErr = ErrUncertain
	r.inspectEvidence = MoveEvidence{
		SourcePresent:      true,
		DestinationPresent: true,
		Destination:        MoveResult{Folder: "Archive", UIDValidity: 7, UID: 101},
	}
	r.finishErr = ErrUncertain
	o := executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "archive"})
	o, e := s.Reconcile(context.Background(), a, o.ID)
	if e != nil || o.State != "needs_verification" || o.Items[0].State != "needs_verification" {
		t.Fatal(o, e)
	}
	if r.moves != 1 || r.finishCallCount != 1 {
		t.Fatal("cleanup failure reissued MOVE", r.moves, r.finishCallCount)
	}
}

func TestOrganizerReconcileNeverSubstitutesSavedMapping(t *testing.T) {
	s, r, a, m := organizerFixture(t)
	r.uncertain = true
	r.verifyErr = ErrUncertain
	r.finishErr = ErrUncertain
	r.inspectEvidence = MoveEvidence{
		DestinationPresent: true,
		Destination:        MoveResult{Folder: "Archive", UIDValidity: 7, UID: 999},
	}
	o := executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "archive"})
	saved := o.Items[0].Result
	o, err := s.Reconcile(context.Background(), a, o.ID)
	if err != nil || o.State != "needs_verification" || o.Items[0].Result != saved || r.finishResult != saved || r.inspectCallCount != 0 {
		t.Fatal("saved COPYUID was replaced", o, err, r.finishResult, r.inspectCallCount)
	}
}

func TestOrganizerReconcileRespectsWriteRevocationAndCancellation(t *testing.T) {
	for _, mode := range []string{"disabled", "settings_changed", "cancelled", "missing_mapping"} {
		t.Run(mode, func(t *testing.T) {
			s, r, a, m := organizerFixture(t)
			r.uncertain = true
			r.verifyErr = ErrUncertain
			r.omitMapping = mode == "missing_mapping"
			r.inspectEvidence = MoveEvidence{SourcePresent: true, DestinationPresent: true, Destination: MoveResult{Folder: "Archive", UIDValidity: 7, UID: 101}}
			o := executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "archive"})
			switch mode {
			case "disabled":
				_, err := s.Store.DB.Exec("UPDATE folder_settings SET writes_enabled=0 WHERE account_id=?", m.AccountID)
				if err != nil {
					t.Fatal(err)
				}
			case "settings_changed":
				_, err := s.Store.DB.Exec("UPDATE folder_settings SET version=version+1 WHERE account_id=?", m.AccountID)
				if err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				if err := s.CancelOperation(a, o.ID); err != nil {
					t.Fatal(err)
				}
			}
			o, err := s.Reconcile(context.Background(), a, o.ID)
			if err != nil || o.State != "needs_verification" || r.finishCallCount != 0 || r.moves != 1 {
				t.Fatal("recovery bypassed write preconditions", o, err, r.finishCallCount)
			}
		})
	}
}

func TestOrganizerReconcileResumesQueuedItemsAfterRestart(t *testing.T) {
	s, r, a, m := organizerFixture(t)
	var second string
	if err := s.Store.DB.QueryRow("SELECT ref FROM messages WHERE account_id=? AND folder='Trash'", m.AccountID).Scan(&second); err != nil {
		t.Fatal(err)
	}
	p, err := s.NewPlan(context.Background(), a, PlanInput{Refs: []string{m.Ref, second}, Action: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	o, err := s.ExecutePlan(a, p.ID, "restart-remaining")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.DB.Exec("UPDATE operations SET state='needs_verification' WHERE id=?; UPDATE operation_items SET state='needs_verification',destination_validity=7,destination_uid=101 WHERE operation_id=? AND position=0", o.ID, o.ID); err != nil {
		t.Fatal(err)
	}
	o, err = s.Reconcile(context.Background(), a, o.ID)
	if err != nil || o.State != "queued" || o.Items[1].State != "queued" {
		t.Fatal("remaining items were stranded", o, err)
	}
	s.runOperation(context.Background(), o.ID)
	o, err = s.Operation(a, o.ID)
	if err != nil || o.State != "completed" || r.moves != 1 {
		t.Fatal(o, err, r.moves)
	}
}

func TestOrganizerPlanRunsAcrossAccounts(t *testing.T) {
	s, r, a, m := organizerFixture(t)
	second, err := s.Create(AccountInput{Name: "second", Provider: "qq", Username: "second@qq.com", Password: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Sync(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveFolderSettings(context.Background(), FolderSettings{AccountID: second.ID, Archive: "Archive", Trash: "Trash", WritesEnabled: true}); err != nil {
		t.Fatal(err)
	}
	messages, _, err := s.Search(Search{AccountID: second.ID, Folder: "INBOX"})
	if err != nil || len(messages) != 1 {
		t.Fatal(messages, err)
	}
	o := executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref, messages[0].Ref}, Action: "archive"})
	if o.State != "completed" || r.moves != 2 {
		t.Fatal("multi-account operation did not finish", o, r.moves)
	}
}

func TestOrganizerFrozenStateAndUndoConflict(t *testing.T) {
	s, _, a, m := organizerFixture(t)
	for _, action := range []string{"label_add", "label_remove"} {
		if _, e := s.NewPlan(context.Background(), a, PlanInput{Refs: []string{m.Ref}, Action: action, Labels: []string{"A"}}); !errors.Is(e, ErrInput) {
			t.Fatal("local classification accepted", e)
		}
	}
	o := executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: "Archive"})
	if _, e := s.Store.DB.Exec("UPDATE messages SET uid=999 WHERE ref=?", m.Ref); e != nil {
		t.Fatal(e)
	}
	plans, e := s.UndoPlan(context.Background(), a, o.ID)
	if e != nil || len(plans) != 0 {
		t.Fatal("stale move was undoable", plans, e)
	}
	p, e := s.NewPlan(context.Background(), a, PlanInput{Refs: []string{m.Ref}, Action: "trash"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Store.DB.Exec("UPDATE messages SET uid=1000 WHERE ref=?", m.Ref); e != nil {
		t.Fatal(e)
	}
	op, e := s.ExecutePlan(a, p.ID, "stale")
	if e != nil {
		t.Fatal(e)
	}
	s.runOperation(context.Background(), op.ID)
	op, _ = s.Operation(a, op.ID)
	if op.State != "failed" || op.Items[0].Error != "state_conflict" {
		t.Fatal(op)
	}
}
func TestOrganizerScopedRevokedAndCancelled(t *testing.T) {
	s, r, a, m := organizerFixture(t)
	token, secret, e := s.CreateToken(TokenInput{Name: "limited", Accounts: []string{m.AccountID}, Folders: map[string][]string{m.AccountID: {"INBOX"}}, Actions: []string{"read", "trash"}})
	if e != nil {
		t.Fatal(e)
	}
	scoped, e := s.AuthenticateToken(secret)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.NewPlan(context.Background(), scoped, PlanInput{Refs: []string{m.Ref}, Action: "trash"}); !errors.Is(e, ErrDenied) {
		t.Fatal("destination escaped scope", e)
	}
	if e = s.RevokeToken(token.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AuthenticateToken(secret); !errors.Is(e, ErrDenied) {
		t.Fatal("revoked accepted", e)
	}
	p, _ := s.NewPlan(context.Background(), a, PlanInput{Refs: []string{m.Ref}, Action: "trash"})
	o, _ := s.ExecutePlan(a, p.ID, "cancel")
	s.CancelOperation(a, o.ID)
	s.runOperation(context.Background(), o.ID)
	o, _ = s.Operation(a, o.ID)
	if o.State != "cancelled" || r.moves != 0 {
		t.Fatal(o, r.moves)
	}
}

func TestOrganizerRestartDoesNotReissueInterruptedMove(t *testing.T) {
	s, r, a, m := organizerFixture(t)
	p, e := s.NewPlan(context.Background(), a, PlanInput{Refs: []string{m.Ref}, Action: "trash"})
	if e != nil {
		t.Fatal(e)
	}
	o, e := s.ExecutePlan(a, p.ID, "interrupted")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Store.DB.Exec("UPDATE operations SET state='running' WHERE id=?; UPDATE operation_items SET state='running' WHERE operation_id=?", o.ID, o.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.StartOperations(); e != nil {
		t.Fatal(e)
	}
	if e = s.StartOperations(); !errors.Is(e, ErrBusy) {
		t.Fatal("duplicate worker", e)
	}
	s.Stop()
	o, e = s.Operation(a, o.ID)
	if e != nil || o.State != "needs_verification" || o.Items[0].State != "needs_verification" || r.moves != 0 {
		t.Fatal(o, e, r.moves)
	}
}

func TestOrganizerConcurrentIdempotency(t *testing.T) {
	s, _, a, m := organizerFixture(t)
	p, e := s.NewPlan(context.Background(), a, PlanInput{Refs: []string{m.Ref}, Action: "archive"})
	if e != nil {
		t.Fatal(e)
	}
	type answer struct {
		id  string
		err error
	}
	results := make(chan answer, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); o, e := s.ExecutePlan(a, p.ID, "same-key"); results <- answer{o.ID, e} }()
	}
	wg.Wait()
	close(results)
	id := ""
	for result := range results {
		if result.err != nil || result.id == "" {
			t.Fatal(result)
		}
		if id == "" {
			id = result.id
		}
		if result.id != id {
			t.Fatal("multiple tasks", id, result.id)
		}
	}
}

func TestOrganizerRestoreUsesExactConfirmedDestination(t *testing.T) {
	s, _, a, m := organizerFixture(t)
	executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "trash"})
	executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "restore"})
	executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "archive"})
	executeFixture(t, s, a, PlanInput{Refs: []string{m.Ref}, Action: "trash"})
	p, e := s.NewPlan(context.Background(), a, PlanInput{Refs: []string{m.Ref}, Action: "restore"})
	if e != nil || p.Items[0].Destination != "Archive" {
		t.Fatal("wrong original folder", p, e)
	}
}
