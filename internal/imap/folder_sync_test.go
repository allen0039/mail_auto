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

	"github.com/emersion/go-imap/client"
	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
	"mail-auto/internal/store"
)

func folderSyncFixture(t *testing.T, initial string) (Adapter, *trafficLog, *atomic.Value) {
	t.Helper()
	mode := new(atomic.Value)
	mode.Store(initial)
	trace := new(trafficLog)
	adapter := Adapter{connect: func(context.Context, mail.Account, string) (*connection, error) {
		left, right := net.Pipe()
		left.SetDeadline(time.Now().Add(5 * time.Second))
		go func() {
			defer right.Close()
			fmt.Fprint(right, "* OK [CAPABILITY IMAP4rev1 IDLE] fixture\r\n")
			reader := bufio.NewScanner(right)
			for reader.Scan() {
				line := reader.Text()
				trace.Write([]byte(line + "\n"))
				parts := strings.SplitN(line, " ", 3)
				if len(parts) < 2 {
					return
				}
				tag, command := parts[0], parts[1]
				current := mode.Load().(string)
				switch command {
				case "LOGIN":
					fmt.Fprintf(right, "%s OK authenticated\r\n", tag)
				case "CAPABILITY":
					fmt.Fprintf(right, "* CAPABILITY IMAP4rev1 IDLE\r\n%s OK capabilities\r\n", tag)
				case "EXAMINE":
					if current == "transport" {
						return
					}
					if current == "present" {
						fmt.Fprintf(right, "* 0 EXISTS\r\n* OK [UIDVALIDITY 7] stable\r\n%s OK [READ-ONLY] examined\r\n", tag)
					} else {
						fmt.Fprintf(right, "%s NO private-subject secret-token\r\n", tag)
					}
				case "LIST":
					switch current {
					case "list_failed":
						fmt.Fprintf(right, "%s NO private-list-error\r\n", tag)
						continue
					case "listed_rejected", "present":
						fmt.Fprint(right, "* LIST () \"/\" \"MailAuto-QA\"\r\n")
					case "nested":
						fmt.Fprint(right, "* LIST () \"/\" \"Parent/MailAuto-QA\"\r\n")
					case "noselect":
						fmt.Fprint(right, "* LIST (\\Noselect) \"/\" \"MailAuto-QA\"\r\n")
					}
					fmt.Fprint(right, "* LIST () \"/\" \"INBOX\"\r\n")
					fmt.Fprintf(right, "%s OK listed\r\n", tag)
				case "UID":
					fmt.Fprintf(right, "* SEARCH\r\n%s OK searched\r\n", tag)
				default:
					fmt.Fprintf(right, "%s BAD unexpected-command\r\n", tag)
				}
			}
		}()
		connectionClient, err := client.New(left)
		if err != nil {
			left.Close()
			return nil, err
		}
		connectionClient.Timeout = 3 * time.Second
		connectionClient.ErrorLog = log.New(io.Discard, "", 0)
		if err = connectionClient.Login("fixture", "fixture"); err != nil {
			left.Close()
			return nil, err
		}
		return &connection{c: connectionClient, raw: left, stop: func() bool { return true }}, nil
	}}
	return adapter, trace, mode
}

func TestSyncFolderUnavailableDiagnostics(t *testing.T) {
	for _, path := range []string{"manual", "automatic", "idle"} {
		for _, mode := range []string{"missing", "nested", "noselect", "listed_rejected", "list_failed"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				adapter, trace, _ := folderSyncFixture(t, mode)
				account := mail.Account{ID: "fixture", Provider: "icloud"}
				emitted := 0
				emit := func(mail.Snapshot) error { emitted++; return nil }
				var err error
				if path == "manual" {
					err = adapter.Scan(t.Context(), account, "", "MailAuto-QA", emit)
				} else {
					session, openErr := adapter.OpenAutomatic(t.Context(), account, "")
					if openErr != nil {
						t.Fatal(openErr)
					}
					defer session.Close()
					if path == "idle" {
						_, err = session.Wait(t.Context(), "MailAuto-QA", time.Second)
					} else {
						_, err = session.Refresh(t.Context(), "MailAuto-QA", mail.FolderState{}, emit)
					}
				}
				var failure *remoteError
				if !errors.As(err, &failure) || emitted != 0 || failure.retryable || !errors.Is(err, ErrRemote) {
					t.Fatalf("unsafe scan or missing typed error: callbacks=%d err=%v", emitted, err)
				}
				unavailable := mode == "missing" || mode == "nested" || mode == "noselect"
				if (failure.OperationErrorCode() == "folder_unavailable") != unavailable {
					t.Fatalf("incorrect folder diagnosis: %s", failure.OperationErrorCode())
				}
				if unavailable && (!strings.Contains(err.Error(), "完整 IMAP 路径") || !strings.Contains(err.Error(), "不会自动改为同步全部邮件")) {
					t.Fatal("missing actionable guidance", err)
				}
				if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
					t.Fatal("provider error leaked", err)
				}
				traffic := strings.ToUpper(trace.String())
				if strings.Count(traffic, " EXAMINE ") != 1 || strings.Count(traffic, " LIST ") != 1 {
					t.Fatal("unexpected retry or missing LIST verification", traffic)
				}
				for _, forbidden := range []string{" SELECT ", " CREATE ", " UID SEARCH ", " UID FETCH ", " STORE ", " EXPUNGE", " MOVE ", " COPY ", " APPEND ", " IDLE"} {
					if strings.Contains(traffic, forbidden) {
						t.Fatalf("unexpected command %q", forbidden)
					}
				}
			})
		}
	}
}

func TestSyncFolderTransportFailureNotReportedMissing(t *testing.T) {
	adapter, trace, _ := folderSyncFixture(t, "transport")
	connection, err := adapter.open(t.Context(), mail.Account{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.close()
	_, err = readOnlyFolder(connection, "MailAuto-QA", "打开文件夹")
	var failure *remoteError
	if !errors.As(err, &failure) || !failure.retryable || failure.OperationErrorCode() == "folder_unavailable" || strings.Contains(trace.String(), " LIST ") {
		t.Fatal("transport failure misdiagnosed or extra command sent", err)
	}
}

func TestSyncFolderInboxNameComparison(t *testing.T) {
	adapter, _, _ := folderSyncFixture(t, "listed_rejected")
	connection, err := adapter.open(t.Context(), mail.Account{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.close()
	_, err = readOnlyFolder(connection, "inbox", "打开文件夹")
	var failure *remoteError
	if !errors.As(err, &failure) || failure.OperationErrorCode() == "folder_unavailable" {
		t.Fatal("case-insensitive INBOX treated as missing", err)
	}
}

func TestSyncFolderFailurePreservesIndexAndCanRecover(t *testing.T) {
	adapter, trace, mode := folderSyncFixture(t, "missing")
	database, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	vault, err := credentials.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	service := &mail.Service{Store: database, Vault: vault, Remote: adapter}
	defer service.Stop()
	account, err := service.Create(mail.AccountInput{Name: "QA", Provider: "netease", Username: "fixture", Password: "fixture", Folders: []string{"MailAuto-QA"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.DB.Exec(`INSERT INTO messages(ref,account_id,folder,uidvalidity,uid,subject,sender,sent_at,unread,size) VALUES('old-ref',?,'MailAuto-QA',7,42,'synthetic','fixture@example.test',1,1,100)`, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = service.StartSync(account.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		jobs, jobErr := service.SyncJobs()
		if jobErr != nil {
			t.Fatal(jobErr)
		}
		if len(jobs) == 1 && jobs[0].State == "failed" {
			if jobs[0].FoldersDone != 0 || !strings.Contains(jobs[0].Error, "IMAP 可读取列表") || !strings.Contains(jobs[0].Error, "保留原索引") {
				t.Fatal("job lost safe error or progress", jobs)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed job not persisted", jobs)
		}
		time.Sleep(time.Millisecond)
	}
	var count int
	if err = database.DB.QueryRow("SELECT count(*) FROM messages WHERE account_id=?", account.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("old index was lost", count, err)
	}
	service.Stop()
	service = &mail.Service{Store: database, Vault: vault, Remote: adapter}
	defer service.Stop()
	mode.Store("present")
	result, err := service.Sync(t.Context(), account.ID)
	if err != nil || result.Folders != 1 || result.Messages != 0 {
		t.Fatal("folder creation/restart retry failed", result, err)
	}
	if err = database.DB.QueryRow("SELECT count(*) FROM messages WHERE account_id=?", account.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("successful empty snapshot not published", count, err)
	}
	traffic := strings.ToUpper(trace.String())
	if strings.Count(traffic, " EXAMINE ") != 2 || strings.Count(traffic, " LIST ") != 1 || strings.Count(traffic, " UID SEARCH ") != 1 || strings.Contains(traffic, " SELECT ") || strings.Contains(traffic, " CREATE ") {
		t.Fatal("scope widened or mutating command used", traffic)
	}
}
