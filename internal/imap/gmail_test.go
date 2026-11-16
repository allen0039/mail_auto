package imap

import (
	"bufio"
	"context"
	"encoding/base64"
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

func TestXOAUTH2WireAndChallenge(t *testing.T) {
	for _, mode := range []string{"google", "microsoft", "challenge", "disconnect", "bad_username"} {
		t.Run(mode, func(t *testing.T) {
			auth := &xoauth2{username: "fixture@example.test", token: "fixture-access"}
			if mode == "bad_username" {
				auth.username = "injected\x01user"
				if _, _, err := auth.Start(); err == nil {
					t.Fatal("SASL injection")
				}
				return
			}
			left, right := net.Pipe()
			left.SetDeadline(time.Now().Add(3 * time.Second))
			defer left.Close()
			done := make(chan error, 1)
			go func() {
				defer right.Close()
				fmt.Fprint(right, "* OK [CAPABILITY IMAP4rev1 AUTH=XOAUTH2] fixture\r\n")
				reader := bufio.NewReader(right)
				line, err := reader.ReadString('\n')
				if err != nil {
					done <- err
					return
				}
				fields := strings.Fields(line)
				if len(fields) < 3 || fields[1] != "AUTHENTICATE" || fields[2] != "XOAUTH2" {
					done <- errors.New("wrong SASL mechanism")
					return
				}
				fmt.Fprint(right, "+ \r\n")
				wire, err := reader.ReadString('\n')
				decoded, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(wire))
				if err != nil || decodeErr != nil || string(decoded) != "user=fixture@example.test\x01auth=Bearer fixture-access\x01\x01" {
					done <- errors.New("incorrect XOAUTH2 frame")
					return
				}
				if mode == "disconnect" {
					done <- nil
					return
				}
				if mode == "challenge" {
					fmt.Fprintf(right, "+ %s\r\n", base64.StdEncoding.EncodeToString([]byte(`{"status":"401","sensitive":"fixture"}`)))
					answer, _ := reader.ReadString('\n')
					if strings.TrimSpace(answer) != "" {
						done <- errors.New("SASL error was not acknowledged with empty response")
						return
					}
					fmt.Fprintf(right, "%s NO authentication failed\r\n", fields[0])
				} else {
					fmt.Fprintf(right, "%s OK authenticated\r\n", fields[0])
				}
				done <- nil
			}()
			imapClient, err := client.New(left)
			if err != nil {
				t.Fatal(err)
			}
			imapClient.ErrorLog = log.New(io.Discard, "", 0)
			err = imapClient.Authenticate(auth)
			if (err == nil) != (mode == "google" || mode == "microsoft") {
				t.Fatal("SASL result", err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

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
					if command == "SELECT" {
						mode = "READ-WRITE"
					}
					fmt.Fprintf(right, "* FLAGS (\\Seen \\Flagged)\r\n* 1 EXISTS\r\n* OK [UIDVALIDITY 7] validity\r\n%s OK [%s] selected\r\n", tag, mode)
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
						if fixture.mode == "disconnect_store" {
							return
						}
						fixture.mu.Lock()
						if strings.Contains(args, "+X-GM-LABELS") {
							fixture.labels["Bills"] = true
						} else {
							delete(fixture.labels, folder)
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
	for _, mode := range []string{"move", "existing_target", "archive", "wrong_id", "disconnect_store", "protected", "protected_after_add", "trash_rejected", "all_mail_source_rejected"} {
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
			if mode == "disconnect_store" || mode == "protected_after_add" {
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
