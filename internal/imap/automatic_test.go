package imap

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	protocol "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"mail-auto/internal/mail"
)

func TestAutomaticProtocolIncrementalSparseUIDs(t *testing.T) {
	adapter, trace, box := noUIDNextAdapter(t, 3, false)
	session, e := adapter.OpenAutomatic(context.Background(), mail.Account{Provider: "netease"}, "")
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	metadata := 0
	emit := func(batch mail.Snapshot) error { metadata += len(batch.Messages); return nil }
	first, e := session.Refresh(context.Background(), "INBOX", mail.FolderState{}, emit)
	if e != nil || len(first.UIDs) != 3 {
		t.Fatal("initial", len(first.UIDs), e)
	}
	second, e := session.Refresh(context.Background(), "INBOX", mail.FolderState{UIDValidity: first.UIDValidity, Unread: first.Unread}, emit)
	if e != nil || len(second.UIDs) != 3 || metadata != 3 {
		t.Fatal("not incremental", metadata, e)
	}
	removed := box.Messages[1].Uid
	box.Messages[0].Flags = []string{protocol.SeenFlag}
	box.Messages = append(box.Messages[:1], box.Messages[2:]...)
	newMessage := *box.Messages[1]
	newMessage.Uid += 100
	box.Messages = append(box.Messages, &newMessage)
	third, e := session.Refresh(context.Background(), "INBOX", mail.FolderState{UIDValidity: first.UIDValidity, Unread: first.Unread}, emit)
	if e != nil || len(third.UIDs) != 3 || metadata != 4 || third.Unread[box.Messages[0].Uid] {
		t.Fatal("flag/new/deletion", metadata, e)
	}
	if _, ok := third.Unread[removed]; ok {
		t.Fatal("deleted UID survived")
	}
	traffic := strings.ToUpper(trace.String())
	if strings.Count(traffic, "ENVELOPE") != 2 || strings.Count(traffic, " LOGIN ") != 1 || strings.Contains(traffic, " STORE ") || strings.Contains(traffic, " EXPUNGE ") {
		t.Fatal("wrong commands or connection reuse")
	}
}

// A scripted real TCP server tests unsolicited push, rejected IDLE and socket
// cancellation without ever modifying a real remote mailbox.
func idleFixture(t *testing.T, mode string) (Adapter, chan string, <-chan struct{}) {
	t.Helper()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	events := make(chan string, 1)
	started := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var conn net.Conn
	var lock sync.Mutex
	go func() {
		defer close(done)
		raw, e := listener.Accept()
		if e != nil {
			return
		}
		lock.Lock()
		conn = raw
		lock.Unlock()
		defer raw.Close()
		fmt.Fprint(raw, "* OK ready\r\n")
		scanner := bufio.NewScanner(raw)
		tag := ""
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) == 0 {
				continue
			}
			if fields[0] == "DONE" {
				fmt.Fprintf(raw, "%s OK idle done\r\n", tag)
				continue
			}
			if len(fields) < 2 {
				return
			}
			tag = fields[0]
			switch strings.ToUpper(fields[1]) {
			case "LOGIN":
				fmt.Fprintf(raw, "%s OK login\r\n", tag)
			case "CAPABILITY":
				caps := "IMAP4rev1"
				if mode != "unsupported" {
					caps += " IDLE"
				}
				fmt.Fprintf(raw, "* CAPABILITY %s\r\n%s OK caps\r\n", caps, tag)
			case "EXAMINE":
				fmt.Fprintf(raw, "* FLAGS (\\Seen)\r\n* 0 EXISTS\r\n* OK [UIDVALIDITY 1] validity\r\n%s OK [READ-ONLY] selected\r\n", tag)
			case "IDLE":
				once.Do(func() { close(started) })
				if mode == "rejected" {
					fmt.Fprintf(raw, "%s NO unavailable\r\n", tag)
					continue
				}
				if mode == "disconnect" {
					return
				}
				fmt.Fprint(raw, "+ idling\r\n")
				select {
				case event := <-events:
					fmt.Fprint(raw, event)
				case <-ctx.Done():
					return
				}
			case "NOOP":
				fmt.Fprintf(raw, "%s OK noop\r\n", tag)
			default:
				fmt.Fprintf(raw, "%s BAD unexpected\r\n", tag)
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		lock.Lock()
		if conn != nil {
			conn.Close()
		}
		lock.Unlock()
		<-done
	})
	adapter := Adapter{connect: func(ctx context.Context, _ mail.Account, _ string) (*connection, error) {
		raw, e := net.Dial("tcp", listener.Addr().String())
		if e != nil {
			return nil, e
		}
		c, e := client.New(raw)
		if e != nil {
			raw.Close()
			return nil, e
		}
		c.ErrorLog = log.New(io.Discard, "", 0)
		c.Timeout = time.Second
		if e = c.Login("fixture", "fixture"); e != nil {
			raw.Close()
			return nil, e
		}
		return &connection{c: c, raw: raw, stop: func() bool { return true }}, nil
	}}
	return adapter, events, started
}
func TestAutomaticIdlePushAndCancellation(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelWait), func(t *testing.T) {
			adapter, events, started := idleFixture(t, "push")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			session, e := adapter.OpenAutomatic(ctx, mail.Account{}, "")
			if e != nil {
				t.Fatal(e)
			}
			defer session.Close()
			session.(*automaticSession).known["INBOX"] = mail.Inventory{UIDValidity: 1}
			type result struct {
				changed bool
				err     error
			}
			done := make(chan result, 1)
			go func() { changed, e := session.Wait(ctx, "INBOX", time.Minute); done <- result{changed, e} }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("did not enter IDLE")
			}
			if cancelWait {
				cancel()
			} else {
				events <- strings.Repeat("* 1 EXISTS\r\n", 200)
			}
			select {
			case r := <-done:
				if cancelWait {
					if r.err == nil {
						t.Fatal("cancel ignored")
					}
				} else if r.err != nil || !r.changed {
					t.Fatal("push lost", r.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("IDLE failed to stop")
			}
		})
	}
}
func TestAutomaticIdleFallbackAndDisconnect(t *testing.T) {
	for _, mode := range []string{"unsupported", "rejected", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			adapter, _, _ := idleFixture(t, mode)
			session, e := adapter.OpenAutomatic(context.Background(), mail.Account{}, "")
			if e != nil {
				t.Fatal(e)
			}
			defer session.Close()
			session.(*automaticSession).known["INBOX"] = mail.Inventory{UIDValidity: 1}
			_, e = session.Wait(context.Background(), "INBOX", 10*time.Millisecond)
			if mode == "disconnect" {
				if e == nil {
					t.Fatal("socket failure treated as unsupported IDLE")
				}
				return
			}
			if e != nil || session.PushSupported() {
				t.Fatal("fallback unavailable", e)
			}
			if _, e = session.Wait(context.Background(), "INBOX", 10*time.Millisecond); e != nil {
				t.Fatal("fallback lost healthy socket", e)
			}
		})
	}
}

func TestDeletedFlagIsExcludedAndTrashRemainsReadable(t *testing.T) {
	adapter, trace, box := noUIDNextAdapter(t, 3, false)
	box.Messages[1].Flags = []string{protocol.DeletedFlag}
	a := mail.Account{Provider: "netease"}
	metadata := 0
	if e := adapter.Scan(context.Background(), a, "", "INBOX", func(batch mail.Snapshot) error { metadata += len(batch.Messages); return nil }); e != nil || metadata != 2 {
		t.Fatal("manual sync retained deleted flag", metadata, e)
	}
	session, e := adapter.OpenAutomatic(context.Background(), a, "")
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	first, e := session.Refresh(context.Background(), "INBOX", mail.FolderState{}, func(mail.Snapshot) error { return nil })
	if e != nil || len(first.UIDs) != 2 || first.Total != 3 {
		t.Fatal("automatic deletion filter", first.Total, e)
	}
	if _, ok := first.Unread[box.Messages[1].Uid]; ok {
		t.Fatal("deleted UID present")
	}
	if _, e = adapter.Read(context.Background(), a, "", mail.Message{Folder: "INBOX", UIDValidity: 1, UID: box.Messages[1].Uid}); e != ErrStale {
		t.Fatal("deleted body remained readable", e)
	}
	box.Messages[0].Flags = []string{protocol.DeletedFlag}
	box.Messages[1].Flags = nil
	second, e := session.Refresh(context.Background(), "INBOX", mail.FolderState{UIDValidity: first.UIDValidity, Unread: first.Unread}, func(batch mail.Snapshot) error {
		if len(batch.Messages) > 0 && batch.Messages[0].UID != box.Messages[1].Uid {
			t.Fatal("wrong restored message")
		}
		return nil
	})
	if e != nil || len(second.UIDs) != 2 {
		t.Fatal("deletion/restore", e)
	}
	if _, ok := second.Unread[box.Messages[0].Uid]; ok {
		t.Fatal("new deletion remained")
	}
	// SEARCH/FETCH race: even if SEARCH included a UID, a later Deleted flag
	// in FETCH must prevent metadata from being published.
	conn, _, e := adapter.selectFolder(context.Background(), a, "", "INBOX")
	if e != nil {
		t.Fatal(e)
	}
	defer conn.close()
	set := new(protocol.SeqSet)
	set.AddNum(box.Messages[0].Uid)
	batch, e := fetchBatch(conn, "a", "INBOX", 1, set)
	if e != nil || len(batch.Messages) != 0 {
		t.Fatal("late Deleted flag was ignored", e)
	}
	if !strings.Contains(strings.ToUpper(trace.String()), "UNDELETED") {
		t.Fatal("deleted UIDs repeatedly fetched as new metadata")
	}
}
