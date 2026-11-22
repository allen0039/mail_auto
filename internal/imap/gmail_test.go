package imap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/client"
	"mail-auto/internal/mail"
)

type gmailFixture struct {
	mu     sync.Mutex
	labels map[string]bool
	trace  []string
	mode   string
}

const fixtureGmailID = "18446744073709551614"
const fixtureAllMail = "[Google Mail]/Alle Nachrichten"

func (fixture *gmailFixture) adapter(t *testing.T) Adapter {
	t.Helper()
	return Adapter{connect: func(context.Context, mail.Account, string) (*connection, error) {
		left, right := net.Pipe()
		left.SetDeadline(time.Now().Add(5 * time.Second))
		go func() {
			defer right.Close()
			fmt.Fprint(right, "* OK [CAPABILITY IMAP4rev1 X-GM-EXT-1] fixture\r\n")
			reader := bufio.NewScanner(right)
			folder := ""
			for reader.Scan() {
				line := reader.Text()
				parts := strings.SplitN(line, " ", 3)
				if len(parts) < 2 {
					return
				}
				tag, command := parts[0], parts[1]
				args := ""
				if len(parts) > 2 {
					args = parts[2]
				}
				fixture.mu.Lock()
				fixture.trace = append(fixture.trace, line)
				present := fixture.labels[folder]
				fixture.mu.Unlock()
				uid := uint32(42)
				if folder == "Bills" {
					uid = 99
				}
				if folder == fixtureAllMail {
					uid = 100
				}
				switch command {
				case "LOGIN":
					fmt.Fprintf(right, "%s OK login\r\n", tag)
				case "CAPABILITY":
					fmt.Fprintf(right, "* CAPABILITY IMAP4rev1 X-GM-EXT-1\r\n%s OK capability\r\n", tag)
				case "LIST":
					fmt.Fprintf(right, "* LIST () \"/\" \"INBOX\"\r\n* LIST () \"/\" \"Bills\"\r\n* LIST () \"/\" \"Unrelated\"\r\n* LIST (\\All) \"/\" \"%s\"\r\n* LIST (\\Trash) \"/\" \"Localized Trash\"\r\n* LIST (\\Flagged) \"/\" \"Localized Starred\"\r\n* LIST (\\Important) \"/\" \"Localized Important\"\r\n* LIST (\\FutureSpecial) \"/\" \"Unknown Special\"\r\n%s OK list\r\n", fixtureAllMail, tag)
				case "EXAMINE", "SELECT":
					folder = strings.Trim(args, "\"")
					mode := "READ-ONLY"
					validity := 7
					if command == "SELECT" {
						mode = "READ-WRITE"
						if folder == "Bills" && fixture.mode == "destination_readonly" {
							mode = "READ-ONLY"
						}
						if folder == "Bills" && fixture.mode == "destination_uidvalidity_changed" {
							validity = 8
						}
					}
					fmt.Fprintf(right, "* FLAGS (\\Seen \\Flagged)\r\n* 1 EXISTS\r\n* OK [UIDVALIDITY %d] validity\r\n%s OK [%s] selected\r\n", validity, tag, mode)
				case "UID":
					if strings.HasPrefix(args, "FETCH ") {
						id := fixtureGmailID
						if fixture.mode == "wrong_id" {
							id = "999"
						}
						fmt.Fprintf(right, "* 1 FETCH (UID %d ENVELOPE (NIL \"fixture\" ((NIL NIL \"sender\" \"example.test\")) NIL NIL NIL NIL NIL NIL \"<same-rfc-id@test>\") FLAGS (\\Seen) RFC822.SIZE 42 INTERNALDATE \"01-Jan-2026 00:00:00 +0000\" X-GM-MSGID %s)\r\n%s OK fetch\r\n", uid, id, tag)
					} else if strings.HasPrefix(args, "SEARCH ") {
						if !strings.Contains(args, fixtureGmailID) {
							fmt.Fprintf(right, "%s BAD wrong identity query\r\n", tag)
							continue
						}
						if present {
							fmt.Fprintf(right, "* SEARCH %d\r\n%s OK search\r\n", uid, tag)
						} else {
							fmt.Fprintf(right, "* SEARCH\r\n%s OK search\r\n", tag)
						}
					} else if strings.Contains(args, "X-GM-LABELS.SILENT") {
						if fixture.mode == "disconnect_store" || (fixture.mode == "disconnect_remove" && strings.Contains(args, "-X-GM-LABELS")) {
							return
						}
						fixture.mu.Lock()
						_, label, _ := strings.Cut(args, "X-GM-LABELS.SILENT ")
						label = strings.Trim(label, "()\"")
						label = strings.ReplaceAll(label, "\\\\", "\\")
						if label == "\\Inbox" {
							label = "INBOX"
						}
						if strings.Contains(args, "+X-GM-LABELS") {
							fixture.labels[label] = true
						} else if label != folder {
							delete(fixture.labels, label)
						}
						fixture.mu.Unlock()
						fmt.Fprintf(right, "%s OK store\r\n", tag)
					} else {
						fmt.Fprintf(right, "%s BAD forbidden UID\r\n", tag)
					}
				default:
					fmt.Fprintf(right, "%s BAD forbidden command\r\n", tag)
				}
			}
		}()
		imapClient, err := client.New(left)
		if err != nil {
			left.Close()
			return nil, err
		}
		imapClient.Timeout = time.Second
		imapClient.ErrorLog = log.New(io.Discard, "", 0)
		if err = imapClient.Login("fixture", "fixture"); err != nil {
			left.Close()
			return nil, err
		}
		return &connection{c: imapClient, raw: left, stop: func() bool { return true }, gmail: true}, nil
	}}
}

func TestGmailSpecialUseCapabilities(t *testing.T) {
	fixture := &gmailFixture{labels: map[string]bool{}}
	adapter := fixture.adapter(t)
	capabilities, err := adapter.Capabilities(context.Background(), mail.Account{ID: "account", Provider: "google"}, "")
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{}
	for _, folder := range capabilities.Folders {
		roles[folder.Name] = folder.Role
	}
	for name, expected := range map[string]string{"INBOX": "inbox", "Bills": "", fixtureAllMail: "archive", "Localized Trash": "trash", "Localized Starred": "flagged", "Localized Important": "important", "Unknown Special": "special"} {
		role, exists := roles[name]
		if !exists || role != expected {
			t.Fatalf("SPECIAL-USE role for %s: got %q, expected %q", name, role, expected)
		}
	}
}

func TestGmailLabelMoveAndArchive(t *testing.T) {
	for _, mode := range []string{"move", "existing_target", "archive", "wrong_id", "disconnect_store", "disconnect_remove", "destination_readonly", "destination_uidvalidity_changed", "protected", "protected_after_add", "trash_rejected", "all_mail_source_rejected"} {
		t.Run(mode, func(t *testing.T) {
			fixture := &gmailFixture{labels: map[string]bool{"INBOX": true, "Unrelated": true, fixtureAllMail: true}, mode: mode}
			adapter := fixture.adapter(t)
			message := mail.Message{AccountID: "account", Folder: "INBOX", UID: 42, UIDValidity: 7, GmailMessageID: fixtureGmailID}
			destination := "Bills"
			if mode == "archive" {
				destination = fixtureAllMail
			}
			if mode == "existing_target" {
				fixture.labels["Bills"] = true
			}
			if mode == "trash_rejected" {
				destination = "Localized Trash"
			}
			if mode == "all_mail_source_rejected" {
				message.Folder = fixtureAllMail
				message.UID = 100
			}
			validations := 0
			validate := func(fresh mail.Message) error {
				validations++
				if fresh.GmailMessageID != fixtureGmailID {
					return mail.ErrConflict
				}
				if mode == "protected" || (mode == "protected_after_add" && validations == 2) {
					return mail.ErrDenied
				}
				return nil
			}
			var mapping mail.MoveResult
			result, err := adapter.MoveWithRuleState(context.Background(), mail.Account{ID: "account", Provider: "google"}, "", message, destination, validate, func(value mail.MoveResult) error { mapping = value; return nil })
			wantSuccess := mode == "move" || mode == "existing_target" || mode == "archive"
			if (err == nil) != wantSuccess {
				t.Fatal("Gmail outcome", err)
			}
			if wantSuccess && (result != mapping || result.UIDValidity != 7 || result.Folder != destination) {
				t.Fatal("real destination mapping missing")
			}
			if mode == "disconnect_store" || mode == "disconnect_remove" || mode == "destination_readonly" || mode == "destination_uidvalidity_changed" || mode == "protected_after_add" {
				if !errors.Is(err, mail.ErrUncertain) {
					t.Fatal("issued write lost unknown outcome")
				}
			}
			fixture.mu.Lock()
			trace := strings.Join(fixture.trace, "\n")
			unrelated := fixture.labels["Unrelated"]
			source := fixture.labels["INBOX"]
			fixture.mu.Unlock()
			if !unrelated {
				t.Fatal("unrelated label removed")
			}
			if wantSuccess && source {
				t.Fatal("source membership retained")
			}
			if (mode == "destination_readonly" || mode == "destination_uidvalidity_changed") && strings.Contains(trace, "-X-GM-LABELS.SILENT") {
				t.Fatal("source label removal issued against an unsafe destination snapshot")
			}
			if wantSuccess {
				targetUID := 99
				if mode == "archive" {
					targetUID = 100
				}
				if !strings.Contains(trace, fmt.Sprintf(" UID STORE %d -X-GM-LABELS.SILENT", targetUID)) {
					t.Fatal("source label was not removed using the destination view UID")
				}
			}
			for _, forbidden := range []string{"COPY ", "MOVE ", "EXPUNGE", "\\Deleted"} {
				if strings.Contains(trace, forbidden) {
					t.Fatal("unsafe Gmail fallback issued", forbidden)
				}
			}
			if mode == "trash_rejected" || mode == "all_mail_source_rejected" || mode == "protected" {
				if strings.Contains(trace, " UID STORE ") {
					t.Fatal("rejected action wrote")
				}
			}
		})
	}
}

func TestGmailStableIDAndReadOnlyReconciliation(t *testing.T) {
	fixture := &gmailFixture{labels: map[string]bool{"INBOX": false, "Bills": true}, mode: "move"}
	adapter := fixture.adapter(t)
	message := mail.Message{AccountID: "account", Folder: "INBOX", UID: 42, UIDValidity: 7, GmailMessageID: fixtureGmailID}
	evidence, err := adapter.InspectMove(context.Background(), mail.Account{ID: "account", Provider: "google"}, "", message, "Bills")
	if err != nil || evidence.SourcePresent || !evidence.DestinationPresent || evidence.Destination.UID != 99 {
		t.Fatal("Gmail evidence", evidence, err)
	}
	if err = adapter.VerifyMove(context.Background(), mail.Account{ID: "account", Provider: "google"}, "", message, evidence.Destination); err != nil {
		t.Fatal(err)
	}
	if err = adapter.FinishMove(context.Background(), mail.Account{Provider: "google"}, "", message, evidence.Destination, func(mail.Message) error { return nil }); !errors.Is(err, mail.ErrCapability) {
		t.Fatal("Gmail expunge finisher accepted")
	}
	if _, err = adapter.VerifyDuplicate(context.Background(), mail.Account{Provider: "google"}, "", message, message); !errors.Is(err, mail.ErrCapability) {
		t.Fatal("Gmail views treated as duplicate copies")
	}
	for _, value := range []any{fixtureGmailID, uint64(18446744073709551614)} {
		if gmailID(value) != fixtureGmailID {
			t.Fatal("Gmail ID precision lost")
		}
	}
	for _, value := range []any{float64(42), "0", "-1", "18446744073709551616", "001"} {
		if gmailID(value) != "" {
			t.Fatal("invalid stable ID accepted")
		}
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, line := range fixture.trace {
		if strings.Contains(line, "STORE") {
			t.Fatal("reconciliation wrote")
		}
	}
}

func TestUncertainWriteNeverRetries(t *testing.T) {
	calls := 0
	err := retryRemote(context.Background(), func() error { calls++; return uncertainFailure(nil, "fixture", io.EOF) })
	if calls != 1 || !errors.Is(err, mail.ErrUncertain) {
		t.Fatal("unknown write replayed", calls, err)
	}
}
