package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

type fakeRemote struct {
	mu             sync.Mutex
	snapshot       Snapshot
	read           Message
	password       string
	folders        []string
	failAfterBatch bool
	block          <-chan struct{}
}

func (f *fakeRemote) Folders(context.Context, Account, string) ([]string, error) {
	if f.folders != nil {
		return f.folders, nil
	}
	return []string{"INBOX"}, nil
}
func (f *fakeRemote) Scan(ctx context.Context, _ Account, pw, _ string, emit func(Snapshot) error) error {
	f.mu.Lock()
	f.password = pw
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if e := emit(Snapshot{UIDValidity: f.snapshot.UIDValidity}); e != nil {
		return e
	}
	for start := 0; start < len(f.snapshot.Messages); start += 500 {
		if e := emit(Snapshot{UIDValidity: f.snapshot.UIDValidity, Messages: f.snapshot.Messages[start:min(start+500, len(f.snapshot.Messages))]}); e != nil {
			return e
		}
		if f.failAfterBatch {
			return errors.New("simulated disconnect")
		}
	}
	return nil
}
func (f *fakeRemote) Read(_ context.Context, _ Account, _ string, m Message) (Content, error) {
	f.read = m
	return Content{Message: m, Text: "body"}, nil
}
func TestSnapshotIdentityAndRead(t *testing.T) {
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	v, e := credentials.Open(t.TempDir(), true)
	if e != nil {
		t.Fatal(e)
	}
	remote := &fakeRemote{}
	s := &Service{Store: st, Vault: v, Remote: remote}
	a, e := s.Create(AccountInput{Name: "QQ", Provider: "qq", Username: "a@qq.com", Password: "private-app-password"})
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(a.Secret), "private-app-password") {
		t.Fatal("plaintext storage")
	}
	now := time.Now().Unix()
	remote.snapshot = Snapshot{UIDValidity: 42, Messages: []Message{{UID: 1, Subject: "同一个 Message-ID", Sender: "a@example.com", SentAt: now, Unread: true}, {UID: 2, Subject: "通知", Sender: "b@example.com", SentAt: now, Unread: false}}}
	if _, e = s.Sync(context.Background(), a.ID); e != nil {
		t.Fatal(e)
	}
	if remote.password != "private-app-password" {
		t.Fatal("wrong decrypted credential")
	}
	found, total, e := s.Search(Search{AccountID: a.ID, Unread: true})
	if e != nil || total != 1 {
		t.Fatal("unread search", total, e)
	}
	oldRef := found[0].Ref
	if _, e = s.Read(context.Background(), oldRef); e != nil || remote.read.UIDValidity != 42 || remote.read.UID != 1 {
		t.Fatal("read identity", e)
	}
	remote.snapshot.Messages[0].Unread = false
	remote.snapshot.Messages = remote.snapshot.Messages[:1]
	if _, e = s.Sync(context.Background(), a.ID); e != nil {
		t.Fatal(e)
	}
	found, total, e = s.Search(Search{AccountID: a.ID})
	if e != nil || total != 1 || found[0].Ref != oldRef || found[0].Unread {
		t.Fatal("flag/deletion reconciliation failed", found, total, e)
	}
	remote.snapshot.UIDValidity = 43
	if _, e = s.Sync(context.Background(), a.ID); e != nil {
		t.Fatal(e)
	}
	found, total, e = s.Search(Search{})
	if e != nil || total != 1 || found[0].Ref == oldRef {
		t.Fatal("UIDVALIDITY did not invalidate refs")
	}
	if _, e = s.Read(context.Background(), oldRef); e == nil {
		t.Fatal("stale ref still readable")
	}
	before := found[0].Ref
	remote.snapshot.UIDValidity = 0
	if _, e = s.Sync(context.Background(), a.ID); e == nil {
		t.Fatal("bad snapshot accepted")
	}
	found, _, e = s.Search(Search{})
	if e != nil || found[0].Ref != before {
		t.Fatal("failed sync destroyed good snapshot")
	}
	release, e := s.acquire(a.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Sync(context.Background(), a.ID); !errors.Is(e, ErrBusy) {
		t.Fatal("concurrent account operation allowed")
	}
	release()
	if e = s.Delete(a.ID); e != nil {
		t.Fatal(e)
	}
	_, total, e = s.Search(Search{})
	if e != nil || total != 0 {
		t.Fatal("cascade delete failed")
	}
}
func TestProviderBoundary(t *testing.T) {
	for _, in := range []AccountInput{{Name: "x", Provider: "custom", Username: "a@x.com", Password: "p"}, {Name: "x", Provider: "qq", Host: "127.0.0.1", Username: "a@qq.com", Password: "p"}, {Name: "x", Provider: "qq", Port: -1, Username: "a@qq.com", Password: "p"}, {Name: "x", Provider: "qq", Username: "a@qq.com", Password: "p", SecretFile: "qq"}} {
		if e := ValidateAccount(&in); e == nil {
			t.Fatal("unsafe config accepted", in.Provider)
		}
	}
}

func TestFullHistoryBatchesAndFailure(t *testing.T) {
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	v, e := credentials.Open(t.TempDir(), true)
	if e != nil {
		t.Fatal(e)
	}
	remote := &fakeRemote{folders: []string{"INBOX", "Archive"}, snapshot: Snapshot{UIDValidity: 7}}
	for i := 1; i <= 2501; i++ {
		remote.snapshot.Messages = append(remote.snapshot.Messages, Message{UID: uint32(i), Subject: fmt.Sprintf("old-%d", i), SentAt: 946684800, Unread: true})
	}
	s := &Service{Store: st, Vault: v, Remote: remote}
	a, e := s.Create(AccountInput{Name: "all", Provider: "qq", Username: "a@qq.com", Password: "test"})
	if e != nil {
		t.Fatal(e)
	}
	if len(a.Folders) != 0 {
		t.Fatal("default must include all folders")
	}
	result, e := s.Sync(context.Background(), a.ID)
	if e != nil || result.Messages != 5002 || result.Folders != 2 {
		t.Fatal(result, e)
	}
	st.Cleanup(context.Background())
	_, total, e := s.Search(Search{})
	if e != nil || total != 5002 {
		t.Fatal("history was lost", total, e)
	}
	found, _, e := s.Search(Search{Folder: "Archive", Offset: 2500, Limit: 50})
	if e != nil || len(found) != 1 {
		t.Fatal("old messages inaccessible", e)
	}
	if _, e = s.Read(context.Background(), found[0].Ref); e != nil {
		t.Fatal("all-folder body denied", e)
	}
	remote.failAfterBatch = true
	remote.snapshot.UIDValidity = 8
	if _, e = s.Sync(context.Background(), a.ID); e == nil {
		t.Fatal("partial sync accepted")
	}
	_, total, e = s.Search(Search{})
	if e != nil || total != 5002 {
		t.Fatal("failure erased index", total, e)
	}
	var stage int
	st.DB.QueryRow("SELECT count(*) FROM sync_stage").Scan(&stage)
	if stage != 0 {
		t.Fatal("staging leaked")
	}
	remote.failAfterBatch = false
	remote.folders = []string{"INBOX"}
	if _, e = s.Sync(context.Background(), a.ID); e != nil {
		t.Fatal(e)
	}
	_, total, _ = s.Search(Search{Folder: "Archive"})
	if total != 0 {
		t.Fatal("removed folder not reconciled")
	}
}
func TestAsyncSyncSurvivesRequestAndShutdown(t *testing.T) {
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	v, e := credentials.Open(t.TempDir(), true)
	if e != nil {
		t.Fatal(e)
	}
	block := make(chan struct{})
	remote := &fakeRemote{block: block, snapshot: Snapshot{UIDValidity: 1}}
	s := &Service{Store: st, Vault: v, Remote: remote}
	defer s.Stop()
	ids := []string{}
	for i := 0; i < 3; i++ {
		a, e := s.Create(AccountInput{Name: fmt.Sprint(i), Provider: "qq", Username: "a@qq.com", Password: "p"})
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, a.ID)
		if _, e = s.StartSync(a.ID); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.StartSync(ids[0]); e != nil {
		t.Fatal("duplicate submission", e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		jobs, e := s.SyncJobs()
		if e != nil {
			t.Fatal(e)
		}
		running, queued := 0, 0
		for _, job := range jobs {
			if job.State == "running" {
				running++
			}
			if job.State == "queued" {
				queued++
			}
		}
		if running == 2 && queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queue/concurrency", jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.Stop()
	jobs, e := s.SyncJobs()
	if e != nil {
		t.Fatal(e)
	}
	for _, job := range jobs {
		if job.State != "interrupted" {
			t.Fatal("shutdown not recorded", job)
		}
	}
	if _, e = s.StartSync(ids[0]); e == nil {
		t.Fatal("started after shutdown")
	}
}
