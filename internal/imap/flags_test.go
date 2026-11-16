package imap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	protocol "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"mail-auto/internal/mail"
)

func TestFlagConfirmationRequiresObservedFlags(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprintf("known_%t", known), func(t *testing.T) {
			left, right := net.Pipe()
			left.SetDeadline(time.Now().Add(3 * time.Second))
			defer left.Close()
			done := make(chan int, 1)
			go func() {
				defer right.Close()
				stores := 0
				defer func() { done <- stores }()
				fmt.Fprint(right, "* OK [CAPABILITY IMAP4rev1] fixture\r\n")
				reader := bufio.NewScanner(right)
				for reader.Scan() {
					parts := strings.Fields(reader.Text())
					if len(parts) < 2 {
						return
					}
					tag := parts[0]
					switch parts[1] {
					case "LOGIN":
						fmt.Fprintf(right, "%s OK login\r\n", tag)
					case "SELECT":
						fmt.Fprintf(right, "* 1 EXISTS\r\n* OK [UIDVALIDITY 7] validity\r\n%s OK [READ-WRITE] select\r\n", tag)
					case "UID":
						if strings.Contains(reader.Text(), " SEARCH ") {
							fmt.Fprintf(right, "* SEARCH 42\r\n%s OK search\r\n", tag)
						} else if strings.Contains(reader.Text(), " STORE ") {
							stores++
							fmt.Fprintf(right, "%s OK store\r\n", tag)
						} else if strings.Contains(reader.Text(), " FETCH ") {
							flags := ""
							if known {
								flags = " FLAGS ()"
							}
							fmt.Fprintf(right, "* 1 FETCH (UID 42%s)\r\n%s OK fetch\r\n", flags, tag)
							return
						}
					}
				}
			}()
			imapClient, err := client.New(left)
			if err != nil {
				t.Fatal(err)
			}
			imapClient.ErrorLog = log.New(io.Discard, "", 0)
			if err = imapClient.Login("fixture", "fixture"); err != nil {
				t.Fatal(err)
			}
			unflag := false
			_, err = setFlagsOnConnection(&connection{c: imapClient, raw: left}, "account", mail.Message{Folder: "INBOX", UID: 42, UIDValidity: 7}, mail.FlagChange{Flagged: &unflag}, nil)
			if known && err != nil || !known && !errors.Is(err, mail.ErrUncertain) {
				t.Fatal("missing FLAGS was treated as a confirmed empty flag set", err)
			}
			if stores := <-done; stores != 1 {
				t.Fatal("unexpected STORE count", stores)
			}
		})
	}
}

func scanFirst(t *testing.T, adapter Adapter, account mail.Account) mail.Message {
	t.Helper()
	var out mail.Message
	if e := adapter.Scan(context.Background(), account, "", "INBOX", func(b mail.Snapshot) error {
		if len(b.Messages) > 0 && out.UID == 0 {
			out = b.Messages[0]
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	return out
}

func TestSetFlagsStoresOnlyRequestedFlagsAndConfirms(t *testing.T) {
	adapter, trace, box := noUIDNextAdapter(t, 2, false)
	account := mail.Account{ID: "a", Provider: "qq"}
	m := scanFirst(t, adapter, account)
	yes := true
	fresh, e := adapter.SetFlags(context.Background(), account, "", m, mail.FlagChange{Seen: &yes, Flagged: &yes}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if fresh.Unread || !fresh.Flagged || !fresh.FlagsKnown {
		t.Fatalf("%+v", fresh)
	}
	if !slices.Contains(box.Messages[0].Flags, protocol.SeenFlag) || !slices.Contains(box.Messages[0].Flags, protocol.FlaggedFlag) {
		t.Fatal(box.Messages[0].Flags)
	}
	if len(box.Messages[1].Flags) != 0 {
		t.Fatal("another message changed", box.Messages[1].Flags)
	}
	traffic := strings.ToUpper(trace.String())
	if strings.Contains(traffic, "EXPUNGE") || strings.Contains(traffic, "(\\DELETED") {
		t.Fatal("flag change touched deletion", traffic)
	}
	no := false
	if fresh, e = adapter.SetFlags(context.Background(), account, "", m, mail.FlagChange{Flagged: &no}, nil); e != nil || fresh.Flagged || fresh.Unread {
		t.Fatal(fresh, e)
	}
	if slices.Contains(box.Messages[0].Flags, protocol.FlaggedFlag) || !slices.Contains(box.Messages[0].Flags, protocol.SeenFlag) {
		t.Fatal("unflag changed seen or kept flag", box.Messages[0].Flags)
	}
}

func TestSetFlagsValidatesBeforeWriting(t *testing.T) {
	adapter, trace, box := noUIDNextAdapter(t, 1, false)
	account := mail.Account{ID: "a", Provider: "qq"}
	m := scanFirst(t, adapter, account)
	yes := true
	_, e := adapter.SetFlags(context.Background(), account, "", m, mail.FlagChange{Seen: &yes}, func(mail.Message) error { return mail.ErrConflict })
	if !errors.Is(e, mail.ErrConflict) {
		t.Fatal(e)
	}
	if strings.Contains(strings.ToUpper(trace.String()), " STORE ") || len(box.Messages[0].Flags) != 0 {
		t.Fatal("rejected validation still wrote flags")
	}
	stale := m
	stale.UIDValidity++
	if _, e = adapter.SetFlags(context.Background(), account, "", stale, mail.FlagChange{Seen: &yes}, nil); !errors.Is(e, ErrStale) {
		t.Fatal("stale UIDVALIDITY", e)
	}
	if _, e = adapter.SetFlags(context.Background(), account, "", m, mail.FlagChange{}, nil); !errors.Is(e, mail.ErrInput) {
		t.Fatal("empty change", e)
	}
}
