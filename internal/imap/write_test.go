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
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"mail-auto/internal/mail"
)

func TestCopyUIDRequiresExactSingleMapping(t *testing.T) {
	for _, tc := range []struct {
		args []interface{}
		ok   bool
	}{
		{[]interface{}{uint32(8), "42", "99"}, true},
		{[]interface{}{uint32(0), "42", "99"}, false},
		{[]interface{}{uint32(8), "41:42", "98:99"}, false},
		{[]interface{}{uint32(8), "43", "99"}, false},
		{[]interface{}{uint32(8), "42", "*"}, false},
	} {
		_, ok := copyUID(&protocol.StatusResp{Code: "COPYUID", Arguments: tc.args}, 42, "Trash")
		if ok != tc.ok {
			t.Fatal(tc, ok)
		}
	}
}

func TestSameMoveMetadataRequiresStableIdentityFields(t *testing.T) {
	base := mail.Message{Subject: "fixture", Sender: "sender@example.com", SentAt: 123, Size: 456}
	if !sameMoveMetadata(base, base) {
		t.Fatal("identical message metadata did not match")
	}
	for _, changed := range []mail.Message{
		{Subject: "other", Sender: base.Sender, SentAt: base.SentAt, Size: base.Size},
		{Subject: base.Subject, Sender: "other@example.com", SentAt: base.SentAt, Size: base.Size},
		{Subject: base.Subject, Sender: base.Sender, SentAt: 124, Size: base.Size},
		{Subject: base.Subject, Sender: base.Sender, SentAt: base.SentAt, Size: 457},
	} {
		if sameMoveMetadata(base, changed) {
			t.Fatal("incomplete metadata matched", changed)
		}
	}
}

func TestNativeMoveProtocolAndUncertainMapping(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			trace := &trafficLog{}
			var moved atomic.Bool
			var opens atomic.Int32
			adapter := Adapter{connect: func(ctx context.Context, a mail.Account, password string) (*connection, error) {
				opens.Add(1)
				left, right := net.Pipe()
				left.SetDeadline(time.Now().Add(5 * time.Second))
				go func() {
					defer right.Close()
					fmt.Fprint(right, "* OK [CAPABILITY IMAP4rev1 MOVE UIDPLUS] fixture\r\n")
					scanner := bufio.NewScanner(right)
					folder := ""
					for scanner.Scan() {
						line := scanner.Text()
						trace.Write([]byte(line + "\n"))
						parts := strings.SplitN(line, " ", 3)
						if len(parts) < 2 {
							return
						}
						tag, cmd := parts[0], strings.ToUpper(parts[1])
						arg := ""
						if len(parts) > 2 {
							arg = parts[2]
						}
						switch cmd {
						case "CAPABILITY":
							fmt.Fprintf(right, "* CAPABILITY IMAP4rev1 MOVE UIDPLUS\r\n%s OK cap\r\n", tag)
						case "LOGIN":
							fmt.Fprintf(right, "%s OK login\r\n", tag)
						case "SELECT", "EXAMINE":
							folder = strings.Trim(arg, "\"")
							fmt.Fprintf(right, "* FLAGS (\\Seen \\Deleted)\r\n* 1 EXISTS\r\n* OK [UIDVALIDITY 7] validity\r\n%s OK [READ-WRITE] select\r\n", tag)
						case "UID":
							if strings.HasPrefix(strings.ToUpper(arg), "SEARCH ") {
								uid := ""
								if folder == "INBOX" && !moved.Load() {
									uid = " 42"
								}
								if folder == "Trash" && moved.Load() {
									uid = " 99"
								}
								fmt.Fprintf(right, "* SEARCH%s\r\n%s OK search\r\n", uid, tag)
							} else if arg == "MOVE 42 Trash" || arg == "MOVE 42 \"Trash\"" {
								moved.Store(true)
								fmt.Fprintf(right, "* OK [COPYUID 7 42 99] moved\r\n")
								if uncertain {
									return
								}
								fmt.Fprintf(right, "%s OK moved\r\n", tag)
							} else {
								fmt.Fprintf(right, "%s BAD unexpected UID\r\n", tag)
							}
						case "LOGOUT":
							fmt.Fprintf(right, "* BYE done\r\n%s OK logout\r\n", tag)
							return
						default:
							fmt.Fprintf(right, "%s BAD forbidden\r\n", tag)
						}
					}
				}()
				c, e := client.New(left)
				if e != nil {
					return nil, e
				}
				c.ErrorLog = log.New(io.Discard, "", 0)
				c.Timeout = 3 * time.Second
				if e = c.Login("user", "password"); e != nil {
					return nil, e
				}
				return &connection{c: c, raw: left, stop: func() bool { return true }}, nil
			}}
			var saved mail.MoveResult
			result, e := adapter.Move(context.Background(), mail.Account{}, "password", mail.Message{Folder: "INBOX", UIDValidity: 7, UID: 42}, "Trash", func(r mail.MoveResult) error { saved = r; return nil })
			if uncertain {
				if !errors.Is(e, mail.ErrUncertain) {
					t.Fatal(result, e)
				}
			} else if e != nil {
				t.Fatal(result, e)
			}
			if opens.Load() != 1 {
				t.Fatalf("move and verification should reuse one login, got %d", opens.Load())
			}
			if saved.UID != 99 || saved.UIDValidity != 7 {
				t.Fatal("mapping not saved", saved, e)
			}
			traffic := strings.ToUpper(trace.String())
			if strings.Count(traffic, "UID MOVE") != 1 || strings.Contains(traffic, "EXPUNGE") || strings.Contains(traffic, "STORE") || strings.Contains(traffic, "COPY ") {
				t.Fatal("unsafe commands", traffic)
			}
		})
	}
}

func TestUIDPlusOnlyMoveSafetyAndInterruptions(t *testing.T) {
	for _, mode := range []string{"success", "copy_disconnect", "no_mapping", "record_failure", "target_missing", "store_disconnect", "expunge_disconnect", "expunge_noop"} {
		t.Run(mode, func(t *testing.T) {
			trace := &trafficLog{}
			var copied, deleted, expunged, verified, saved atomic.Bool
			var unsafe atomic.Bool
			var opens atomic.Int32
			adapter := Adapter{connect: func(ctx context.Context, a mail.Account, pw string) (*connection, error) {
				opens.Add(1)
				left, right := net.Pipe()
				left.SetDeadline(time.Now().Add(5 * time.Second))
				go func() {
					defer right.Close()
					fmt.Fprint(right, "* OK [CAPABILITY IMAP4rev1 UIDPLUS] fixture\r\n")
					scan := bufio.NewScanner(right)
					folder := ""
					for scan.Scan() {
						line := scan.Text()
						trace.Write([]byte(line + "\n"))
						parts := strings.SplitN(line, " ", 3)
						if len(parts) < 2 {
							return
						}
						tag, cmd := parts[0], strings.ToUpper(parts[1])
						arg := ""
						if len(parts) > 2 {
							arg = parts[2]
						}
						upper := strings.ToUpper(arg)
						switch cmd {
						case "CAPABILITY":
							fmt.Fprintf(right, "* CAPABILITY IMAP4rev1 UIDPLUS\r\n%s OK cap\r\n", tag)
						case "LOGIN":
							fmt.Fprintf(right, "%s OK login\r\n", tag)
						case "SELECT", "EXAMINE":
							folder = strings.Trim(arg, "\"")
							fmt.Fprintf(right, "* FLAGS (\\Seen \\Deleted)\r\n* 2 EXISTS\r\n* OK [UIDVALIDITY 7] validity\r\n%s OK [READ-WRITE] select\r\n", tag)
						case "UID":
							switch {
							case strings.HasPrefix(upper, "SEARCH "):
								uid := ""
								if folder == "INBOX" && !expunged.Load() && !(deleted.Load() && strings.Contains(upper, "UNDELETED")) {
									uid = " 42"
								}
								if folder == "Trash" && copied.Load() && mode != "target_missing" {
									uid = " 99"
									verified.Store(true)
								}
								fmt.Fprintf(right, "* SEARCH%s\r\n%s OK search\r\n", uid, tag)
							case arg == "COPY 42 Trash" || arg == "COPY 42 \"Trash\"":
								copied.Store(true)
								if mode == "copy_disconnect" {
									return
								}
								if mode == "no_mapping" {
									fmt.Fprintf(right, "%s OK copied\r\n", tag)
								} else {
									fmt.Fprintf(right, "%s OK [COPYUID 7 42 99] copied\r\n", tag)
								}
							case strings.HasPrefix(upper, "STORE "):
								if folder != "INBOX" || !verified.Load() || !saved.Load() || !strings.HasPrefix(upper, "STORE 42 +FLAGS.SILENT ") {
									unsafe.Store(true)
								}
								deleted.Store(true)
								if mode == "store_disconnect" {
									return
								}
								fmt.Fprintf(right, "%s OK flags\r\n", tag)
							case upper == "EXPUNGE 42":
								if folder != "INBOX" || !deleted.Load() || !verified.Load() || !saved.Load() {
									unsafe.Store(true)
								}
								if mode != "expunge_noop" {
									expunged.Store(true)
								}
								if mode == "expunge_disconnect" {
									return
								}
								fmt.Fprintf(right, "%s OK expunged\r\n", tag)
							default:
								unsafe.Store(true)
								fmt.Fprintf(right, "%s BAD unexpected\r\n", tag)
							}
						default:
							unsafe.Store(true)
							fmt.Fprintf(right, "%s BAD forbidden\r\n", tag)
						}
					}
				}()
				c, e := client.New(left)
				if e != nil {
					return nil, e
				}
				c.ErrorLog = log.New(io.Discard, "", 0)
				c.Timeout = 3 * time.Second
				if e = c.Login("fixture", "password"); e != nil {
					left.Close()
					return nil, e
				}
				return &connection{c: c, raw: left, stop: func() bool { return true }}, nil
			}}
			_, err := adapter.Move(context.Background(), mail.Account{}, "password", mail.Message{Folder: "INBOX", UID: 42, UIDValidity: 7}, "Trash", func(r mail.MoveResult) error {
				if r.UID != 99 || r.UIDValidity != 7 {
					t.Error("bad mapping", r)
				}
				if mode == "record_failure" {
					return errors.New("fixture disk failure")
				}
				saved.Store(true)
				return nil
			})
			if mode == "success" {
				if err != nil || !expunged.Load() {
					t.Fatal(err)
				}
			} else if !errors.Is(err, mail.ErrUncertain) {
				t.Fatal("must remain uncertain", err)
			}
			if opens.Load() != 1 {
				t.Fatalf("UIDPLUS move and verification should reuse one login, got %d", opens.Load())
			}
			traffic := strings.ToUpper(trace.String())
			if unsafe.Load() || strings.Contains(traffic, "UID MOVE") || strings.Count(traffic, "UID COPY") != 1 {
				t.Fatal("unsafe protocol", traffic)
			}
			// UID 77 represents unrelated already-deleted mail. A global EXPUNGE or
			// CLOSE would delete it; the fixture accepts only UID EXPUNGE 42.
			for _, line := range strings.Split(traffic, "\n") {
				fields := strings.Fields(line)
				if len(fields) > 1 && (fields[1] == "EXPUNGE" || fields[1] == "CLOSE") {
					t.Fatal("global deletion", line)
				}
			}
			early := mode == "copy_disconnect" || mode == "no_mapping" || mode == "record_failure" || mode == "target_missing"
			if early && (deleted.Load() || strings.Contains(traffic, "UID EXPUNGE")) {
				t.Fatal("source removed before copy confirmation", traffic)
			}
			if mode == "expunge_noop" && !strings.Contains(traffic, "UID 42\n") {
				t.Fatal("deleted source not verified", traffic)
			}
		})
	}
}

func TestCreateFolderRequiresRemoteConfirmation(t *testing.T) {
	for _, mode := range []string{"success", "missing", "noselect", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			trace := &trafficLog{}
			adapter := Adapter{connect: func(ctx context.Context, a mail.Account, pw string) (*connection, error) {
				left, right := net.Pipe()
				left.SetDeadline(time.Now().Add(5 * time.Second))
				go func() {
					defer right.Close()
					fmt.Fprint(right, "* OK [CAPABILITY IMAP4rev1] fixture\r\n")
					scan := bufio.NewScanner(right)
					for scan.Scan() {
						line := scan.Text()
						trace.Write([]byte(line + "\n"))
						parts := strings.SplitN(line, " ", 3)
						if len(parts) < 2 {
							return
						}
						tag, cmd := parts[0], strings.ToUpper(parts[1])
						switch cmd {
						case "LOGIN":
							fmt.Fprintf(right, "%s OK login\r\n", tag)
						case "CREATE":
							if mode == "disconnect" {
								return
							}
							fmt.Fprintf(right, "%s OK create\r\n", tag)
						case "LIST":
							if mode != "missing" {
								attrs := ""
								if mode == "noselect" {
									attrs = "\\Noselect"
								}
								fmt.Fprintf(right, "* LIST (%s) \"/\" Bills\r\n", attrs)
							}
							fmt.Fprintf(right, "%s OK list\r\n", tag)
						default:
							fmt.Fprintf(right, "%s BAD unexpected\r\n", tag)
						}
					}
				}()
				c, e := client.New(left)
				if e != nil {
					return nil, e
				}
				c.ErrorLog = log.New(io.Discard, "", 0)
				c.Timeout = 3 * time.Second
				if e = c.Login("user", "pass"); e != nil {
					return nil, e
				}
				return &connection{c: c, raw: left, stop: func() bool { return true }}, nil
			}}
			err := adapter.CreateFolder(context.Background(), mail.Account{}, "pass", "Bills")
			if mode == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, mail.ErrUncertain) {
				t.Fatal(mode, err)
			}
			if strings.Count(strings.ToUpper(trace.String()), " CREATE ") != 1 {
				t.Fatal(trace.String())
			}
		})
	}
}

func TestAutomationRuleStateReadsFreshMetadataWithoutBodyOrFlagsWrites(t *testing.T) {
	adapter, trace, box := noUIDNextAdapter(t, 1, false)
	account := mail.Account{ID: "fixture", Provider: "qq"}
	var original mail.Message
	if e := adapter.Scan(context.Background(), account, "", "INBOX", func(b mail.Snapshot) error {
		if len(b.Messages) > 0 {
			original = b.Messages[0]
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if !original.FlagsKnown || original.ReceivedAt <= 0 {
		t.Fatal("metadata incomplete", original)
	}
	box.Messages[0].Flags = []string{protocol.SeenFlag, protocol.FlaggedFlag}
	fresh, e := adapter.RuleState(context.Background(), account, "", original)
	if e != nil || !fresh.Flagged || fresh.Unread || !fresh.FlagsKnown || fresh.ReceivedAt != original.ReceivedAt {
		t.Fatal(fresh, e)
	}
	changed := original
	changed.UIDValidity++
	if _, e = adapter.RuleState(context.Background(), account, "", changed); !errors.Is(e, mail.ErrConflict) {
		t.Fatal("stale UIDVALIDITY", e)
	}
	traffic := strings.ToUpper(trace.String())
	if !strings.Contains(traffic, " EXAMINE ") || strings.Contains(traffic, "BODY[") || strings.Contains(traffic, "BODY.PEEK") || strings.Contains(traffic, " STORE ") || strings.Contains(traffic, " EXPUNGE ") {
		t.Fatal("unexpected metadata read commands")
	}
}
