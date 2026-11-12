package mail

import (
	"context"
	"errors"
	"testing"
)

func accountEpoch(service *Service, accountID string) uint64 {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.epoch[accountID]
}

func refreshWith(t *testing.T, service *Service, automatic *automaticFixture, accountID string) error {
	t.Helper()
	account, err := service.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	return service.incrementalFolder(context.Background(), &fixtureSession{remote: automatic}, account, "INBOX", accountEpoch(service, accountID))
}

func unreadInbox(message Message) *automaticFixture {
	flagged := message
	flagged.Unread = true
	return &automaticFixture{boxes: map[string]Snapshot{"INBOX": {UIDValidity: message.UIDValidity, Messages: []Message{flagged}}}}
}

func TestReadOnlyUnknownRecoveryKeepsInflightFlagRefresh(t *testing.T) {
	cases := map[string]func(*organizerRemote){
		"metadata_match_without_mapping": func(remote *organizerRemote) {
			remote.inspectEvidence = MoveEvidence{DestinationPresent: true, Destination: MoveResult{Folder: "Archive", UIDValidity: 7, UID: 99}}
		},
		"inspection_error_without_mapping": func(remote *organizerRemote) {
			remote.inspectErr = ErrUncertain
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			service, remote, admin, message := organizerFixture(t)
			remote.omitMapping = true
			remote.uncertain = true
			prepare(remote)
			operation := executeFixture(t, service, admin, PlanInput{Refs: []string{message.Ref}, Action: "archive"})
			if operation.State != "needs_verification" {
				t.Fatal(operation.State)
			}
			automatic := unreadInbox(message)
			automatic.afterEmit = func() {
				before := accountEpoch(service, message.AccountID)
				settled, err := service.Reconcile(context.Background(), admin, operation.ID)
				if err != nil || settled.State != "needs_verification" || settled.Items[0].RecoveryCode != "recovery_no_trusted_mapping" {
					t.Fatal("unknown recovery settled without trusted mapping", settled, err)
				}
				if remote.moves != 1 || remote.finishCallCount != 0 {
					t.Fatal("read-only recovery replayed a write", remote.moves, remote.finishCallCount)
				}
				if accountEpoch(service, message.AccountID) != before {
					t.Fatal("read-only recovery invalidated automatic snapshots")
				}
				current, err := service.Message(message.Ref)
				if err != nil || current.Folder != message.Folder || current.UID != message.UID {
					t.Fatal("read-only recovery changed the index", current, err)
				}
			}
			for attempt := 1; attempt <= 3; attempt++ {
				if err := refreshWith(t, service, automatic, message.AccountID); err != nil {
					t.Fatalf("attempt %d: in-flight refresh discarded: %v", attempt, err)
				}
			}
			refreshed, err := service.Message(message.Ref)
			if err != nil || !refreshed.Unread || refreshed.Folder != message.Folder {
				t.Fatal("flag-only refresh was not published", refreshed, err)
			}
			if remote.inspectCallCount != 3 || remote.moves != 1 || remote.finishCallCount != 0 {
				t.Fatal("unexpected recovery effects", remote.inspectCallCount, remote.moves, remote.finishCallCount)
			}
		})
	}
}

func TestSourcePresentRecoveryKeepsInflightFlagRefresh(t *testing.T) {
	service, remote, admin, message := organizerFixture(t)
	remote.omitMapping = true
	remote.uncertain = true
	remote.inspectEvidence = MoveEvidence{SourcePresent: true}
	operation := executeFixture(t, service, admin, PlanInput{Refs: []string{message.Ref}, Action: "archive"})
	automatic := unreadInbox(message)
	automatic.afterEmit = func() {
		settled, err := service.Reconcile(context.Background(), admin, operation.ID)
		if err != nil || settled.State != "failed" || settled.Items[0].Error != "move_not_applied" {
			t.Fatal(settled, err)
		}
	}
	if err := refreshWith(t, service, automatic, message.AccountID); err != nil {
		t.Fatal("operation metadata update discarded an in-flight refresh", err)
	}
	refreshed, err := service.Message(message.Ref)
	if err != nil || !refreshed.Unread || remote.moves != 1 || remote.finishCallCount != 0 {
		t.Fatal(refreshed, err, remote.moves, remote.finishCallCount)
	}
}

func TestVerifiedRecoveryPublishInvalidatesInflightRefresh(t *testing.T) {
	service, remote, admin, message := organizerFixture(t)
	remote.uncertain = true
	operation := executeFixture(t, service, admin, PlanInput{Refs: []string{message.Ref}, Action: "archive"})
	if operation.State != "needs_verification" || operation.Items[0].Result.UID == 0 {
		t.Fatal("setup did not leave a mapped uncertain move", operation)
	}
	saved := operation.Items[0].Result
	automatic := unreadInbox(message)
	automatic.afterEmit = func() {
		before := accountEpoch(service, message.AccountID)
		settled, err := service.Reconcile(context.Background(), admin, operation.ID)
		if err != nil || settled.State != "completed" {
			t.Fatal(settled, err)
		}
		if accountEpoch(service, message.AccountID) == before {
			t.Fatal("recovered index publication left automatic snapshots valid")
		}
	}
	if err := refreshWith(t, service, automatic, message.AccountID); !errors.Is(err, ErrBusy) {
		t.Fatal("stale snapshot accepted after recovered publication", err)
	}
	moved, err := service.Message(message.Ref)
	if err != nil || moved.Folder != saved.Folder || moved.UID != saved.UID || moved.Unread {
		t.Fatal("stale refresh overwrote the recovered move", moved, err)
	}
	if remote.moves != 1 || remote.finishCallCount != 0 {
		t.Fatal("verified recovery replayed writes", remote.moves, remote.finishCallCount)
	}
}

func TestRecoveryCleanupAttemptInvalidatesInflightRefresh(t *testing.T) {
	cases := map[string]error{
		"uncertain": ErrUncertain,
		"failed":    errors.New("injected cleanup failure"),
		"succeeded": nil,
	}
	for name, finishErr := range cases {
		t.Run(name, func(t *testing.T) {
			service, remote, admin, message := organizerFixture(t)
			remote.uncertain = true
			remote.verifyErr = ErrUncertain
			remote.finishErr = finishErr
			operation := executeFixture(t, service, admin, PlanInput{Refs: []string{message.Ref}, Action: "archive"})
			automatic := unreadInbox(message)
			automatic.afterEmit = func() {
				before := accountEpoch(service, message.AccountID)
				settled, err := service.Reconcile(context.Background(), admin, operation.ID)
				if err != nil {
					t.Fatal(err)
				}
				wantState := "needs_verification"
				if finishErr == nil {
					wantState = "completed"
				}
				if settled.State != wantState || remote.finishCallCount != 1 || remote.moves != 1 {
					t.Fatal("unexpected cleanup outcome", settled.State, remote.finishCallCount, remote.moves)
				}
				if accountEpoch(service, message.AccountID) == before {
					t.Fatal("remote cleanup attempt left automatic snapshots valid")
				}
			}
			if err := refreshWith(t, service, automatic, message.AccountID); !errors.Is(err, ErrBusy) {
				t.Fatal("stale snapshot accepted after remote cleanup attempt", err)
			}
			current, err := service.Message(message.Ref)
			if err != nil || current.Unread {
				t.Fatal("stale flags published after cleanup attempt", current, err)
			}
		})
	}
}

func TestBlockedCleanupWithoutRemoteWriteKeepsInflightRefresh(t *testing.T) {
	service, remote, admin, message := organizerFixture(t)
	remote.uncertain = true
	remote.verifyErr = ErrUncertain
	operation := executeFixture(t, service, admin, PlanInput{Refs: []string{message.Ref}, Action: "archive"})
	if _, err := service.Store.DB.Exec("UPDATE folder_settings SET writes_enabled=0 WHERE account_id=?", message.AccountID); err != nil {
		t.Fatal(err)
	}
	automatic := unreadInbox(message)
	automatic.afterEmit = func() {
		settled, err := service.Reconcile(context.Background(), admin, operation.ID)
		if err != nil || settled.State != "needs_verification" || remote.finishCallCount != 0 || remote.moves != 1 {
			t.Fatal("blocked recovery wrote remotely", settled, err, remote.finishCallCount, remote.moves)
		}
	}
	if err := refreshWith(t, service, automatic, message.AccountID); err != nil {
		t.Fatal("recovery without a remote write discarded an in-flight refresh", err)
	}
}

func TestRecoveryGateKeepsAccountExclusionAndReadBudget(t *testing.T) {
	service, _, _, message := organizerFixture(t)
	before := accountEpoch(service, message.AccountID)
	releaseRecovery, err := service.acquireRecovery(message.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if accountEpoch(service, message.AccountID) != before {
		t.Fatal("recovery gate advanced the epoch")
	}
	if _, err := service.acquire(message.AccountID); !errors.Is(err, ErrBusy) {
		t.Fatal("foreground request ran beside recovery", err)
	}
	if _, err := service.acquireRecovery(message.AccountID); !errors.Is(err, ErrBusy) {
		t.Fatal("second recovery ran beside recovery", err)
	}
	if _, err := service.StartSync(message.AccountID); !errors.Is(err, ErrBusy) {
		t.Fatal("manual sync started beside recovery", err)
	}
	automatic := unreadInbox(message)
	if err := refreshWith(t, service, automatic, message.AccountID); !errors.Is(err, ErrBusy) {
		t.Fatal("automatic commit published while recovery held the account", err)
	}
	if current, err := service.Message(message.Ref); err != nil || current.Unread {
		t.Fatal("automatic commit published while recovery held the account", current, err)
	}
	releaseRecovery()
	if err := refreshWith(t, service, automatic, message.AccountID); err != nil {
		t.Fatal("refresh failed after read-only recovery released", err)
	}

	other, err := service.Create(AccountInput{Name: "other", Provider: "qq", Username: "other@qq.com", Password: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	third, err := service.Create(AccountInput{Name: "third", Provider: "qq", Username: "third@qq.com", Password: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	releaseFirst, err := service.acquire(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	releaseSecond, err := service.acquireRecovery(third.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()
	if _, err := service.acquireRecovery(message.AccountID); !errors.Is(err, ErrBusy) {
		t.Fatal("recovery exceeded the shared read budget", err)
	}
	if _, err := service.acquire(message.AccountID); !errors.Is(err, ErrBusy) {
		t.Fatal("foreground request exceeded the shared read budget", err)
	}
}
