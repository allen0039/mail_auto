package imap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	protocol "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"
	"mail-auto/internal/mail"
)

type trafficLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *trafficLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *trafficLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }
func TestReadOnlyProtocol(t *testing.T) {
	backend := memory.New()
	user, e := backend.Login(nil, "username", "password")
	if e != nil {
		t.Fatal(e)
	}
	box, e := user.GetMailbox("INBOX")
	if e != nil {
		t.Fatal(e)
	}
	mbox := box.(*memory.Mailbox)
	mbox.Messages[0].Flags = nil
	srv := server.New(backend)
	srv.AllowInsecureAuth = true
	srv.ErrorLog = log.New(io.Discard, "", 0)
	trace := &trafficLog{}
	srv.Debug = protocol.NewDebugWriter(io.Discard, trace)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	defer func() { srv.Close(); listener.Close(); <-done }()
	adapter := Adapter{connect: func(ctx context.Context, a mail.Account, password string) (*connection, error) {
		raw, e := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
		if e != nil {
			return nil, e
		}
		raw.SetDeadline(time.Now().Add(5 * time.Second))
		c, e := client.New(raw)
		if e != nil {
			raw.Close()
			return nil, e
		}
		c.Timeout = 3 * time.Second
		c.ErrorLog = log.New(io.Discard, "", 0)
		if e = c.Login(a.Username, password); e != nil {
			raw.Close()
			return nil, e
		}
		return &connection{c: c, raw: raw, stop: func() bool { return true }}, nil
	}}
	a := mail.Account{ID: "a", Username: "username"}
	folders, e := adapter.Folders(context.Background(), a, "password")
	if e != nil || len(folders) != 1 || folders[0] != "INBOX" {
		t.Fatal(folders, e)
	}
	snap := mail.Snapshot{}
	e = adapter.Scan(context.Background(), a, "password", "INBOX", func(batch mail.Snapshot) error {
		snap.UIDValidity = batch.UIDValidity
		snap.Messages = append(snap.Messages, batch.Messages...)
		return nil
	})
	if e != nil || len(snap.Messages) != 1 || !snap.Messages[0].Unread {
		t.Fatalf("snapshot: %+v %v", snap, e)
	}
	m := snap.Messages[0]
	out, e := adapter.Read(context.Background(), a, "password", m)
	if e != nil || out.Text != "Hi there :)" {
		t.Fatalf("body: %+v %v", out, e)
	}
	stale := m
	stale.UIDValidity++
	if _, e = adapter.Read(context.Background(), a, "password", stale); !errors.Is(e, ErrStale) {
		t.Fatal("stale reference accepted", e)
	}
	// EXAMINE and PEEK must both appear; no command may mutate remote state.
	traffic := strings.ToUpper(trace.String())
	if !strings.Contains(traffic, "EXAMINE") || !strings.Contains(traffic, "BODY.PEEK[") {
		t.Fatal("missing readonly protocol", traffic)
	}
	for _, command := range []string{" STORE ", " EXPUNGE", " MOVE ", " COPY ", " APPEND ", " SELECT "} {
		if regexp.MustCompile(`(?m)^[^ \r\n]+(?: UID)?` + regexp.QuoteMeta(command)).MatchString(traffic) {
			t.Fatalf("write-capable command %q found", command)
		}
	}
	// Reconnect after all reads and verify the original unseen flag is unchanged.
	conn, e := adapter.open(context.Background(), a, "password")
	if e != nil {
		t.Fatal(e)
	}
	defer conn.close()
	if _, e = conn.c.Select("INBOX", true); e != nil {
		t.Fatal(e)
	}
	criteria := protocol.NewSearchCriteria()
	criteria.WithoutFlags = []string{protocol.SeenFlag}
	uids, e := conn.c.UidSearch(criteria)
	if e != nil || len(uids) != 1 {
		t.Fatal("reading altered Seen flag", uids, e)
	}
	conn.close()
	original := mbox.Messages[0]
	mbox.Messages = nil
	for i := 1; i <= 2501; i++ {
		message := *original
		message.Uid = uint32(i)
		message.Date = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		mbox.Messages = append(mbox.Messages, &message)
	}
	count, maxBatch := 0, 0
	if e = adapter.Scan(context.Background(), a, "password", "INBOX", func(batch mail.Snapshot) error {
		count += len(batch.Messages)
		maxBatch = max(maxBatch, len(batch.Messages))
		for _, m := range batch.Messages {
			if m.SentAt != 946684800 {
				t.Fatal("old date omitted")
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if count != 2501 || maxBatch > UIDBatchSize {
		t.Fatal("history truncated or unbounded", count, maxBatch)
	}
	if strings.Contains(strings.ToUpper(trace.String()), " SINCE ") {
		t.Fatal("date restriction remains")
	}

}
