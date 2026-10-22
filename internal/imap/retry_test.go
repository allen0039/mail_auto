package imap

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
	"mail-auto/internal/store"
)

// Cut a FETCH response in the middle, after seven complete batches. The client
// must discard the partial batch and resume on a new read-only connection.
type interruptedFetchConn struct {
	net.Conn
	fetches int
	cut     atomic.Bool
}

func (c *interruptedFetchConn) Write(p []byte) (int, error) {
	if strings.Contains(strings.ToUpper(string(p)), " UID FETCH ") {
		c.fetches++
		if c.fetches == 8 {
			c.cut.Store(true)
		}
	}
	return c.Conn.Write(p)
}
func (c *interruptedFetchConn) Read(p []byte) (int, error) {
	n, e := c.Conn.Read(p)
	if c.cut.Load() {
		c.Conn.Close()
		return min(n, 20), io.ErrUnexpectedEOF
	}
	return n, e
}

func TestSyncResumesAfter700WithRealProtocolAndSQLite(t *testing.T) {
	var opens atomic.Int32
	adapter, trace, _ := noUIDNextAdapterWithConn(t, 1001, false, func(conn net.Conn) net.Conn {
		if opens.Add(1) == 1 {
			return &interruptedFetchConn{Conn: conn}
		}
		return conn
	})
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	v, e := credentials.Open(t.TempDir(), true)
	if e != nil {
		t.Fatal(e)
	}
	svc := &mail.Service{Store: st, Vault: v, Remote: adapter}
	defer svc.Stop()
	a, e := svc.Create(mail.AccountInput{Name: "retry", Provider: "netease", Username: "username", Password: "password", Folders: []string{"INBOX"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = svc.StartSync(a.ID); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		jobs, e := svc.SyncJobs()
		if e != nil {
			t.Fatal(e)
		}
		if len(jobs) == 1 && jobs[0].State == "completed" {
			if jobs[0].Messages != 1001 || jobs[0].FoldersDone != 1 {
				t.Fatal("incorrect progress", jobs)
			}
			break
		}
		if len(jobs) == 1 && jobs[0].State == "failed" {
			t.Fatal("sync failed", jobs)
		}
		if time.Now().After(deadline) {
			t.Fatal("sync did not finish", jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, total, e := svc.Search(mail.Search{AccountID: a.ID})
	if e != nil || total != 1001 || opens.Load() != 2 {
		t.Fatal("incomplete index or missing reconnect", total, opens.Load(), e)
	}
	var pending int
	if e = st.DB.QueryRow("SELECT count(*) FROM sync_stage").Scan(&pending); e != nil || pending != 0 {
		t.Fatal("staging not cleared", pending, e)
	}
	traffic := strings.ToUpper(trace.String())
	if strings.Count(traffic, " UID FETCH ") != 12 || strings.Count(traffic, " UID SEARCH ") != 1 || strings.Count(traffic, " EXAMINE ") != 2 {
		t.Fatal("batches were replayed or mailbox was not revalidated")
	}
}

func TestScanDoesNotRetryCallbackFailure(t *testing.T) {
	adapter, trace, _ := noUIDNextAdapter(t, 201, false)
	want := errors.New("local database failure")
	calls := 0
	e := adapter.Scan(context.Background(), mail.Account{Provider: "netease"}, "", "INBOX", func(s mail.Snapshot) error {
		if len(s.Messages) > 0 {
			calls++
			return want
		}
		return nil
	})
	if !errors.Is(e, want) || calls != 1 || strings.Count(strings.ToUpper(trace.String()), " EXAMINE ") != 1 {
		t.Fatal("callback was retried", calls, e)
	}
}

func TestScanRejectsChangedUIDValidityAfterReconnect(t *testing.T) {
	var opens atomic.Int32
	adapter, _, _ := noUIDNextAdapterWithConn(t, 1001, false, func(conn net.Conn) net.Conn {
		if opens.Add(1) == 1 {
			return &interruptedFetchConn{Conn: conn}
		}
		return conn
	}, func() uint32 { return uint32(opens.Load()) })
	count := 0
	e := adapter.Scan(context.Background(), mail.Account{Provider: "netease"}, "", "INBOX", func(s mail.Snapshot) error {
		count += len(s.Messages)
		return nil
	})
	if e == nil || !strings.Contains(e.Error(), "UIDVALIDITY 已变化") || count != 700 || opens.Load() != 2 {
		t.Fatal("changed mailbox identity accepted", count, opens.Load(), e)
	}
}

func TestTransientConnectionRetryBoundAndCancellation(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		a := Adapter{connect: func(context.Context, mail.Account, string) (*connection, error) {
			calls++
			if cancelEarly {
				cancel()
			}
			return nil, remoteFailure(nil, "等待 IMAP 欢迎响应", os.ErrDeadlineExceeded)
		}}
		_, e := a.Folders(ctx, mail.Account{}, "")
		cancel()
		if cancelEarly {
			if !errors.Is(e, context.Canceled) || calls != 1 {
				t.Fatal("cancellation ignored", calls, e)
			}
		} else if calls != 3 || !errors.Is(e, ErrRemote) || !strings.Contains(e.Error(), "已自动重试 2 次") || !strings.Contains(e.Error(), "超时") {
			t.Fatal("retry bound or diagnostics", calls, e)
		}
	}
}

func TestRemoteFailureRedactsServerTextAndKeepsSocketCause(t *testing.T) {
	bc := &boundedConn{}
	bc.remember(os.ErrDeadlineExceeded)
	e := remoteFailure(&connection{budget: bc}, "读取邮件元数据", errors.New("imap: connection closed: secret-user secret-password private-subject"))
	if !strings.Contains(e.Error(), "超时") || strings.Contains(e.Error(), "secret") || strings.Contains(e.Error(), "private") {
		t.Fatal("unsafe or missing diagnostic", e)
	}
	e = remoteFailure(nil, "邮箱登录", errors.New("AUTH failed secret-password"))
	var failure *remoteError
	if !errors.As(e, &failure) || failure.retryable || strings.Contains(e.Error(), "secret") {
		t.Fatal("authentication error retried or exposed", e)
	}
}
