package imap

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	protocol "github.com/emersion/go-imap"
	"mail-auto/internal/mail"
)

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
