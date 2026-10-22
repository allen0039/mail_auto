package mail

import (
	"context"
	"errors"
	"strings"
	"testing"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

func TestStagingFailureHasSafeStageAndRetainsIndex(t *testing.T) {
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	v, e := credentials.Open(t.TempDir(), true)
	if e != nil {
		t.Fatal(e)
	}
	r := &fakeRemote{snapshot: Snapshot{UIDValidity: 1, Messages: []Message{{UID: 7, Subject: "private-subject"}}}}
	s := &Service{Store: st, Vault: v, Remote: r}
	a, e := s.Create(AccountInput{Name: "test", Provider: "qq", Username: "private@qq.com", Password: "private-password"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Sync(context.Background(), a.ID); e != nil {
		t.Fatal(e)
	}
	// A duplicate UID must fail in SQLite, with no partial publication or replay.
	r.snapshot.Messages = append(r.snapshot.Messages, r.snapshot.Messages[0])
	_, e = s.Sync(context.Background(), a.ID)
	if e == nil || syncErrorMessage(e) != "写入邮件暂存区失败" || strings.Contains(syncErrorMessage(e), "private") {
		t.Fatal("missing or unsafe storage diagnostic", e)
	}
	_, count, e := s.Search(Search{AccountID: a.ID})
	if e != nil || count != 1 {
		t.Fatal("original index lost", count, e)
	}
	if syncErrorMessage(errors.New("private-password private-subject")) != "同步处理失败" {
		t.Fatal("raw error exposed")
	}
}
