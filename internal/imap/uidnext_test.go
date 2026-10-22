package imap

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	protocol "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"
	"mail-auto/internal/mail"
)

type noUIDNextBackend struct {
	backend.Backend
	failFetch bool
	validity  func() uint32
}

func (b noUIDNextBackend) Login(info *protocol.ConnInfo, username, password string) (backend.User, error) {
	u, e := b.Backend.Login(info, username, password)
	return noUIDNextUser{User: u, failFetch: b.failFetch, validity: b.validity}, e
}

type noUIDNextUser struct {
	backend.User
	failFetch bool
	validity  func() uint32
}

func (u noUIDNextUser) GetMailbox(name string) (backend.Mailbox, error) {
	m, e := u.User.GetMailbox(name)
	return noUIDNextMailbox{Mailbox: m, failFetch: u.failFetch, validity: u.validity}, e
}

type noUIDNextMailbox struct {
	backend.Mailbox
	failFetch bool
	validity  func() uint32
}

func (m noUIDNextMailbox) Status(items []protocol.StatusItem) (*protocol.MailboxStatus, error) {
	s, e := m.Mailbox.Status(items)
	if e == nil {
		s.UidNext = 0
		delete(s.Items, protocol.StatusUidNext)
		if m.validity != nil {
			s.UidValidity = m.validity()
		}
	}
	return s, e
}
func (m noUIDNextMailbox) SearchMessages(uid bool, criteria *protocol.SearchCriteria) ([]uint32, error) {
	if m.failFetch {
		return nil, errors.New("simulated UID lookup failure")
	}
	return m.Mailbox.SearchMessages(uid, criteria)
}
func noUIDNextAdapter(t *testing.T, count int, fail bool) (Adapter, *trafficLog, *memory.Mailbox) {
	return noUIDNextAdapterWithConn(t, count, fail, nil)
}

func noUIDNextAdapterWithConn(t *testing.T, count int, fail bool, wrap func(net.Conn) net.Conn, validity ...func() uint32) (Adapter, *trafficLog, *memory.Mailbox) {
	t.Helper()
	mem := memory.New()
	u, e := mem.Login(nil, "username", "password")
	if e != nil {
		t.Fatal(e)
	}
	box, e := u.GetMailbox("INBOX")
	if e != nil {
		t.Fatal(e)
	}
	mbox := box.(*memory.Mailbox)
	original := mbox.Messages[0]
	mbox.Messages = nil
	for i := 0; i < count; i++ {
		m := *original
		m.Uid = uint32(1582817720 + i*2)
		m.Flags = nil
		mbox.Messages = append(mbox.Messages, &m)
	}
	b := noUIDNextBackend{Backend: mem, failFetch: fail}
	if len(validity) > 0 {
		b.validity = validity[0]
	}
	srv := server.New(b)
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
	t.Cleanup(func() { srv.Close(); listener.Close(); <-done })
	a := Adapter{connect: func(ctx context.Context, a mail.Account, password string) (*connection, error) {
		raw, e := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
		if e != nil {
			return nil, e
		}
		if wrap != nil {
			raw = wrap(raw)
		}
		raw.SetDeadline(time.Now().Add(5 * time.Second))
		bc := &boundedConn{Conn: raw}
		bc.remaining.Store(32 << 20)
		c, e := client.New(bc)
		if e != nil {
			raw.Close()
			return nil, e
		}
		c.Timeout = 3 * time.Second
		c.ErrorLog = log.New(io.Discard, "", 0)
		if e = c.Login("username", "password"); e != nil {
			raw.Close()
			return nil, e
		}
		return &connection{c: c, raw: raw, budget: bc, stop: func() bool { return true }}, nil
	}}
	return a, trace, mbox
}
func TestScanWithoutUIDNext(t *testing.T) {
	for _, tc := range []struct {
		provider         string
		count, batchSize int
	}{{"qq", 0, 500}, {"qq", 701, 500}, {"netease", 701, 100}} {
		count := tc.count
		t.Run(tc.provider+strconv.Itoa(count), func(t *testing.T) {
			a, trace, mbox := noUIDNextAdapter(t, count, false)
			messages, batches, maxBatch := 0, 0, 0
			var validity uint32
			var last uint32
			e := a.Scan(context.Background(), mail.Account{Provider: tc.provider}, "", "INBOX", func(s mail.Snapshot) error {
				validity = s.UIDValidity
				batches++
				messages += len(s.Messages)
				maxBatch = max(maxBatch, len(s.Messages))
				for _, m := range s.Messages {
					if m.UID <= last {
						t.Fatal("UID range lost ordering")
					}
					last = m.UID
				}
				return nil
			})
			if e != nil || messages != count || validity != 1 || maxBatch > tc.batchSize || batches < 1 {
				t.Fatal("missing UIDNEXT scan", messages, validity, maxBatch, e)
			}
			if count > 0 && last != mbox.Messages[count-1].Uid {
				t.Fatal("last UID missing")
			}
			traffic := strings.ToUpper(trace.String())
			if strings.Contains(traffic, "UIDNEXT") {
				t.Fatal("fixture did not omit UIDNEXT")
			}
			if !strings.Contains(traffic, "EXAMINE") {
				t.Fatal("read-only open missing")
			}
			if !strings.Contains(traffic, "UID SEARCH ") {
				t.Fatal("actual UID enumeration missing")
			}
			if count > 0 && strings.Count(traffic, " UID FETCH ") != (count+tc.batchSize-1)/tc.batchSize {
				t.Fatal("sparse UID scan requested empty ranges")
			}
		})
	}
}
func TestUIDNextFallbackFailureAndOverflow(t *testing.T) {
	a, _, _ := noUIDNextAdapter(t, 1, true)
	emitted := false
	if e := a.Scan(context.Background(), mail.Account{}, "", "INBOX", func(mail.Snapshot) error { emitted = true; return nil }); e == nil || emitted {
		t.Fatal("failed UID lookup emitted an empty snapshot")
	}
	a, _, mbox := noUIDNextAdapter(t, 1, false)
	mbox.Messages[0].Uid = ^uint32(0)
	var found []mail.Message
	e := a.Scan(context.Background(), mail.Account{}, "", "INBOX", func(s mail.Snapshot) error { found = append(found, s.Messages...); return nil })
	if e != nil || len(found) != 1 || found[0].UID != ^uint32(0) {
		t.Fatal("maximum UID scan failed", found, e)
	}
}
