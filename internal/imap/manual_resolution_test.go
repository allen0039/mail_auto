package imap

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/client"
	"mail-auto/internal/mail"
)

func TestDuplicateVerifierFullPEEKAndFailClosed(t *testing.T) {
	for _, mode := range []string{"same", "different_bytes", "extra_target", "deleted", "epoch_changed", "flags_changed", "truncated", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			trace := &trafficLog{}
			body := "Message-ID: <pair@test>\r\nSubject: fixture\r\n\r\noriginal body\r\n"
			adapter := Adapter{connect: func(context.Context, mail.Account, string) (*connection, error) {
				left, right := net.Pipe()
				left.SetDeadline(time.Now().Add(5 * time.Second))
				go func() {
					defer right.Close()
					fmt.Fprint(right, "* OK [CAPABILITY IMAP4rev1] fixture\r\n")
					scan := bufio.NewScanner(right)
					folder := ""
					rawReads := 0
					for scan.Scan() {
						line := scan.Text()
						trace.Write([]byte(line + "\n"))
						parts := strings.SplitN(line, " ", 3)
						if len(parts) < 2 {
							return
						}
						tag, cmd := parts[0], parts[1]
						arg := ""
						if len(parts) > 2 {
							arg = parts[2]
						}
						uid := uint32(42)
						if folder == "Archive" {
							uid = 99
						}
						switch cmd {
						case "CAPABILITY":
							fmt.Fprintf(right, "* CAPABILITY IMAP4rev1\r\n%s OK cap\r\n", tag)
						case "LOGIN":
							fmt.Fprintf(right, "%s OK login\r\n", tag)
						case "EXAMINE":
							folder = strings.Trim(arg, "\"")
							epoch := 7
							if mode == "epoch_changed" && folder == "Archive" {
								epoch = 8
							}
							fmt.Fprintf(right, "* FLAGS (\\Seen \\Deleted \\Flagged)\r\n* 1 EXISTS\r\n* OK [UIDVALIDITY %d] validity\r\n%s OK [READ-ONLY] examine\r\n", epoch, tag)
						case "UID":
							if strings.HasPrefix(arg, "SEARCH ") {
								extra := ""
								if mode == "extra_target" && folder == "Archive" {
									extra = " 100"
								}
								fmt.Fprintf(right, "* SEARCH %d%s\r\n%s OK search\r\n", uid, extra, tag)
							} else if strings.HasPrefix(arg, "FETCH ") {
								flags := "\\Seen"
								if mode == "deleted" {
									flags += " \\Deleted"
								}
								if mode == "flags_changed" && rawReads >= 2 {
									flags += " \\Flagged"
								}
								if strings.Contains(arg, "BODY.PEEK[]") {
									rawReads++
									if mode == "disconnect" {
										return
									}
									raw := body
									if folder == "Archive" && mode == "different_bytes" {
										raw = strings.Replace(raw, "original", "modified", 1)
									}
									if mode == "truncated" {
										raw = raw[:len(raw)-2]
									}
									fmt.Fprintf(right, "* 1 FETCH (UID %d FLAGS (%s) RFC822.SIZE %d BODY[]<0> {%d}\r\n%s)\r\n%s OK body\r\n", uid, flags, len(body), len(raw), raw, tag)
								} else if strings.Contains(arg, "ENVELOPE") {
									fmt.Fprintf(right, "* 1 FETCH (UID %d ENVELOPE (NIL \"fixture\" ((NIL NIL \"sender\" \"example.com\")) NIL NIL NIL NIL NIL NIL \"<pair@test>\") FLAGS (%s) RFC822.SIZE %d INTERNALDATE \"01-Jan-2026 00:00:00 +0000\")\r\n%s OK metadata\r\n", uid, flags, len(body), tag)
								} else {
									fmt.Fprintf(right, "* 1 FETCH (UID %d FLAGS (%s))\r\n%s OK flags\r\n", uid, flags, tag)
								}
							} else {
								fmt.Fprintf(right, "%s BAD forbidden UID\r\n", tag)
							}
						case "LOGOUT":
							fmt.Fprintf(right, "* BYE done\r\n%s OK logout\r\n", tag)
							return
						default:
							fmt.Fprintf(right, "%s BAD forbidden\r\n", tag)
						}
					}
				}()
				c, err := client.New(left)
				if err != nil {
					return nil, err
				}
				c.ErrorLog = log.New(io.Discard, "", 0)
				c.Timeout = time.Second
				if err = c.Login("user", "fixture"); err != nil {
					return nil, err
				}
				return &connection{c: c, raw: left, stop: func() bool { return true }}, nil
			}}
			source := mail.Message{AccountID: "a", Ref: "s", Folder: "INBOX", UIDValidity: 7, UID: 42, Subject: "fixture", Sender: "sender@example.com", SentAt: 1767225600, ReceivedAt: 1767225600, Size: uint32(len(body)), MessageID: "<pair@test>", FlagsKnown: true}
			target := source
			target.Ref = "t"
			target.Folder = "Archive"
			target.UID = 99
			out, err := adapter.VerifyDuplicate(context.Background(), mail.Account{ID: "a"}, "fixture", source, target)
			if mode == "same" {
				if err != nil || out.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(body))) || out.Bytes != int64(len(body)) {
					t.Fatal(out, err, trace.String())
				}
			} else if err == nil {
				t.Fatal("ambiguous/changed evidence accepted", mode, out)
			}
			traffic := strings.ToUpper(trace.String())
			for _, bad := range []string{" SELECT ", "UID MOVE", "UID COPY", "UID STORE", "EXPUNGE", "BODY[] "} {
				if strings.Contains(traffic, bad) {
					t.Fatal("write or non-PEEK command", bad, traffic)
				}
			}
			if mode == "same" && strings.Count(traffic, "BODY.PEEK[]") != 2 {
				t.Fatal("did not read both complete originals", traffic)
			}
		})
	}
}
