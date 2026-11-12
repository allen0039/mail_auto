package imap

import (
	"context"
	"strings"
	"testing"

	protocol "github.com/emersion/go-imap"
	"mail-auto/internal/mail"
)

const extendedFixture = "From: News <news@example.com>\r\n" +
	"To: Alice <alice@example.com>, bob@example.com\r\n" +
	"Cc: carol@example.org\r\n" +
	"Subject: Weekly digest\r\n" +
	"Date: Wed, 11 Nov 2026 10:00:00 +0800\r\n" +
	"Message-ID: <digest-1@example.com>\r\n" +
	"In-Reply-To: <root@example.com>\r\n" +
	"References: <root@example.com>\r\n <mid@example.com>\r\n" +
	"List-Id: Weekly News <Weekly.News.Example.COM>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=b1\r\n" +
	"\r\n" +
	"--b1\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"hello\r\n" +
	"--b1\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0=\r\n" +
	"--b1--\r\n"

func TestScanReadsExtendedMetadataWithoutMarkingSeen(t *testing.T) {
	adapter, trace, box := noUIDNextAdapter(t, 2, false)
	box.Messages[0].Body = []byte(extendedFixture)
	box.Messages[0].Size = uint32(len(extendedFixture))
	var got []mail.Message
	if e := adapter.Scan(context.Background(), mail.Account{ID: "a", Provider: "qq"}, "", "INBOX", func(b mail.Snapshot) error {
		got = append(got, b.Messages...)
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if len(got) != 2 {
		t.Fatal(got)
	}
	m := got[0]
	if m.MetadataVersion != mail.CurrentMetadataVersion || m.Recipients != "alice@example.com, bob@example.com" || m.Cc != "carol@example.org" {
		t.Fatalf("recipients %+v", m)
	}
	if m.MessageID != "<digest-1@example.com>" || m.InReplyTo != "<root@example.com>" || m.References != "<root@example.com> <mid@example.com>" {
		t.Fatalf("threading %+v", m)
	}
	if m.ListID != "weekly.news.example.com" || !m.HasAttachment {
		t.Fatalf("list/attachment %+v", m)
	}
	if got[1].HasAttachment || got[1].ListID != "" {
		t.Fatalf("plain message misdetected %+v", got[1])
	}
	for _, message := range box.Messages {
		for _, flag := range message.Flags {
			if flag == protocol.SeenFlag {
				t.Fatal("metadata read marked a message seen")
			}
		}
	}
	if strings.Contains(strings.ToUpper(trace.String()), "BODY[]") {
		t.Fatal("full body read during metadata scan")
	}
}

func TestNormalizeListID(t *testing.T) {
	for in, want := range map[string]string{
		"Weekly <Weekly.Example.COM>": "weekly.example.com",
		"plain.example.com":           "plain.example.com",
		"":                            "",
		"broken <no-end":              "broken <no-end",
	} {
		if got := normalizeListID(in); got != want {
			t.Fatalf("%q => %q, want %q", in, got, want)
		}
	}
}
