package mail

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

type automaticFixture struct {
	*fakeRemote
	lock                    sync.Mutex
	boxes                   map[string]Snapshot
	fail                    bool
	malformed               bool
	opens, closed, metadata int
	changed                 chan struct{}
	afterEmit               func()
}
type fixtureSession struct{ remote *automaticFixture }

func (r *automaticFixture) OpenAutomatic(context.Context, Account, string) (AutomaticSession, error) {
	r.lock.Lock()
	r.opens++
	r.lock.Unlock()
	return &fixtureSession{r}, nil
}
func (f *fixtureSession) Close()            { f.remote.lock.Lock(); f.remote.closed++; f.remote.lock.Unlock() }
func (*fixtureSession) PushSupported() bool { return true }
func (f *fixtureSession) Folders(context.Context) ([]string, error) {
	f.remote.lock.Lock()
	defer f.remote.lock.Unlock()
	out := []string{}
	for folder := range f.remote.boxes {
		out = append(out, folder)
	}
	return out, nil
}
func (f *fixtureSession) Refresh(ctx context.Context, folder string, old FolderState, emit func(Snapshot) error) (Inventory, error) {
	r := f.remote
	r.lock.Lock()
	snapshot := r.boxes[folder]
	snapshot.Messages = append([]Message(nil), snapshot.Messages...)
	fail, malformed, after := r.fail, r.malformed, r.afterEmit
	r.lock.Unlock()
	out := Inventory{UIDValidity: snapshot.UIDValidity, Unread: map[uint32]bool{}}
	batch := Snapshot{UIDValidity: snapshot.UIDValidity}
	for _, m := range snapshot.Messages {
		out.UIDs = append(out.UIDs, m.UID)
		out.Unread[m.UID] = m.Unread
		if _, ok := old.Unread[m.UID]; !ok || old.UIDValidity != out.UIDValidity {
			batch.Messages = append(batch.Messages, m)
		}
	}
	if e := emit(batch); e != nil {
		return out, e
	}
	r.lock.Lock()
	r.metadata += len(batch.Messages)
	r.lock.Unlock()
	if after != nil {
		after()
	}
	if malformed {
		out.UIDs = nil
	}
	if fail {
		return out, errors.New("injected failed refresh")
	}
	return out, ctx.Err()
}
func (f *fixtureSession) Wait(ctx context.Context, _ string, d time.Duration) (bool, error) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, nil
	case <-f.remote.changed:
		return true, nil
	}
}
func newAutomaticFixture(t *testing.T) (*Service, *automaticFixture, Account) {
	t.Helper()
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	v, e := credentials.Open(t.TempDir(), true)
	if e != nil {
		t.Fatal(e)
	}
	r := &automaticFixture{fakeRemote: &fakeRemote{}, boxes: map[string]Snapshot{"INBOX": {UIDValidity: 7, Messages: []Message{{UID: 1, Unread: true}, {UID: 2, Unread: true}}}}, changed: make(chan struct{}, 1)}
	s := &Service{Store: st, Vault: v, Remote: r}
	t.Cleanup(func() { s.Stop(); st.Close() })
	a, e := s.Create(AccountInput{Name: "fixture", Provider: "qq", Username: "test@qq.com", Password: "fixture"})
	if e != nil {
		t.Fatal(e)
	}
	return s, r, a
}
func awaitAutomatic(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("automatic state did not converge")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestIncrementalDeletionMoveFlagsValidityAndFailure(t *testing.T) {
	s, r, a := newAutomaticFixture(t)
	session := &fixtureSession{r}
	ctx := context.Background()
	refresh := func(folder string) {
		t.Helper()
		if e := s.incrementalFolder(ctx, session, a, folder, 0); e != nil {
			t.Fatal(e)
		}
	}
	refresh("INBOX")
	found, total, e := s.Search(Search{})
	if e != nil || total != 2 {
		t.Fatal(total, e)
	}
	refs := map[uint32]string{}
	for _, m := range found {
		refs[m.UID] = m.Ref
	}
	revision := s.IndexRevision()
	refresh("INBOX")
	if r.metadata != 2 || s.IndexRevision() != revision {
		t.Fatal("old envelopes were fetched or unchanged index republished")
	}
	r.boxes["INBOX"] = Snapshot{UIDValidity: 7, Messages: []Message{{UID: 1}, {UID: 3, Unread: true}}}
	refresh("INBOX")
	found, total, e = s.Search(Search{Folder: "INBOX"})
	if e != nil || total != 2 {
		t.Fatal(total, e)
	}
	for _, m := range found {
		if m.UID == 2 || (m.UID == 1 && (m.Ref != refs[1] || m.Unread)) {
			t.Fatal("deletion/flags/ref reconciliation")
		}
	}
	if r.metadata != 3 {
		t.Fatal("incremental metadata count", r.metadata)
	}
	r.boxes["Archive"] = Snapshot{UIDValidity: 9, Messages: []Message{{UID: 5}}}
	r.boxes["INBOX"] = Snapshot{UIDValidity: 7}
	refresh("INBOX")
	refresh("Archive")
	_, total, _ = s.Search(Search{})
	if total != 1 {
		t.Fatal("move was not reconciled")
	}
	r.boxes["Archive"] = Snapshot{UIDValidity: 10, Messages: []Message{{UID: 5, Unread: true}}}
	r.fail = true
	if e = s.incrementalFolder(ctx, session, a, "Archive", 0); e == nil {
		t.Fatal("partial refresh succeeded")
	}
	found, _, _ = s.Search(Search{})
	if found[0].UIDValidity != 9 {
		t.Fatal("failure removed previous index")
	}
	r.fail = false
	r.malformed = true
	if e = s.incrementalFolder(ctx, session, a, "Archive", 0); e == nil {
		t.Fatal("inconsistent inventory accepted")
	}
	r.malformed = false
	oldRef := found[0].Ref
	refresh("Archive")
	found, _, _ = s.Search(Search{})
	if found[0].Ref == oldRef || found[0].UIDValidity != 10 {
		t.Fatal("changed UIDVALIDITY kept stale ref")
	}
	if e = s.reconcileAutomaticFolders(ctx, a, []string{"INBOX"}, 0); e != nil {
		t.Fatal(e)
	}
	_, total, _ = s.Search(Search{})
	if total != 0 {
		t.Fatal("removed folder retained")
	}
	var count int
	s.Store.DB.QueryRow("SELECT count(*) FROM sync_stage").Scan(&count)
	if count != 0 {
		t.Fatal("staging leak")
	}
}
func TestIncrementalRejectsResultsAfterConfigAndManualRequest(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "configuration"}[edit], func(t *testing.T) {
			s, r, a := newAutomaticFixture(t)
			var release func()
			r.afterEmit = func() {
				if edit {
					_, e := s.Update(a.ID, AccountInput{Name: "changed", Provider: a.Provider, Username: a.Username})
					if e != nil {
						t.Fatal(e)
					}
				} else {
					var e error
					release, e = s.acquire(a.ID)
					if e != nil {
						t.Fatal(e)
					}
					release()
				}
			}
			if e := s.incrementalFolder(context.Background(), &fixtureSession{r}, a, "INBOX", 0); !errors.Is(e, ErrBusy) {
				t.Fatal("stale background result accepted", e)
			}
			_, total, _ := s.Search(Search{})
			if total != 0 {
				t.Fatal("stale index published")
			}
		})
	}
}
func TestAutomaticLifecyclePushConfigDeleteAndManualPriority(t *testing.T) {
	s, r, a := newAutomaticFixture(t)
	if e := s.StartAutomaticSync(50 * time.Millisecond); e != nil {
		t.Fatal(e)
	}
	awaitAutomatic(t, func() bool { _, n, _ := s.Search(Search{}); return n == 2 })
	awaitAutomatic(t, func() bool {
		status := s.AutomaticStatus()
		return len(status.Accounts) == 1 && status.Accounts[0].State == "watching"
	})
	release, e := s.acquire(a.ID)
	if e != nil {
		t.Fatal(e)
	}
	r.lock.Lock()
	r.boxes["INBOX"] = Snapshot{UIDValidity: 7, Messages: []Message{{UID: 3}}}
	r.lock.Unlock()
	r.changed <- struct{}{}
	awaitAutomatic(t, func() bool { return s.AutomaticStatus().Accounts[0].State == "waiting" })
	_, n, _ := s.Search(Search{})
	if n != 2 {
		t.Fatal("manual priority violated")
	}
	release()
	awaitAutomatic(t, func() bool { rows, n, _ := s.Search(Search{}); return n == 1 && rows[0].UID == 3 })
	_, e = s.Update(a.ID, AccountInput{Name: "renamed", Provider: a.Provider, Username: a.Username})
	if e != nil {
		t.Fatal(e)
	}
	awaitAutomatic(t, func() bool { r.lock.Lock(); defer r.lock.Unlock(); return r.opens >= 2 && r.closed >= 1 })
	if e = s.Delete(a.ID); e != nil {
		t.Fatal(e)
	}
	awaitAutomatic(t, func() bool { return len(s.AutomaticStatus().Accounts) == 0 })
	_, n, _ = s.Search(Search{})
	if n != 0 {
		t.Fatal("deleted account republished")
	}
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked")
	}
}

func TestAutomaticSyncWaitsForWholeTaskBatch(t *testing.T) {
	s, r, a := newAutomaticFixture(t)
	if e := s.StartAutomaticSync(50 * time.Millisecond); e != nil {
		t.Fatal(e)
	}
	awaitAutomatic(t, func() bool {
		_, n, _ := s.Search(Search{})
		return n == 2
	})
	awaitAutomatic(t, func() bool {
		status := s.AutomaticStatus()
		return len(status.Accounts) == 1 && status.Accounts[0].State == "watching"
	})
	release := s.holdAutomatic(a.ID)
	r.lock.Lock()
	r.boxes["INBOX"] = Snapshot{UIDValidity: 7, Messages: []Message{{UID: 3}}}
	r.lock.Unlock()
	r.changed <- struct{}{}
	awaitAutomatic(t, func() bool { return s.AutomaticStatus().Accounts[0].State == "waiting" })
	_, n, _ := s.Search(Search{})
	if n != 2 {
		t.Fatal("automatic sync changed the index while a task batch held the account")
	}
	release()
	awaitAutomatic(t, func() bool {
		rows, n, _ := s.Search(Search{})
		return n == 1 && rows[0].UID == 3
	})
}

func TestAutomaticReconnectPreservesLastIndex(t *testing.T) {
	s, r, _ := newAutomaticFixture(t)
	if e := s.StartAutomaticSync(time.Hour); e != nil {
		t.Fatal(e)
	}
	awaitAutomatic(t, func() bool { _, n, _ := s.Search(Search{}); return n == 2 })
	r.lock.Lock()
	r.fail = true
	r.boxes["INBOX"] = Snapshot{UIDValidity: 7}
	r.lock.Unlock()
	r.changed <- struct{}{}
	awaitAutomatic(t, func() bool {
		status := s.AutomaticStatus()
		return len(status.Accounts) == 1 && status.Accounts[0].State == "retrying"
	})
	_, n, _ := s.Search(Search{})
	if n != 2 {
		t.Fatal("failed connection erased last successful index")
	}
	r.lock.Lock()
	r.fail = false
	r.boxes["INBOX"] = Snapshot{UIDValidity: 7, Messages: []Message{{UID: 4}}}
	r.lock.Unlock()
	deadline := time.Now().Add(8 * time.Second)
	for {
		rows, n, _ := s.Search(Search{})
		if n == 1 && rows[0].UID == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("automatic reconnect did not recover")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.opens < 2 || r.closed < 1 {
		t.Fatal("connection not replaced")
	}
}
