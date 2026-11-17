package imap

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	protocol "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
	"mail-auto/internal/network"
	"mail-auto/internal/store"
)

const keepTCPSubject = "FR06 synthetic"
const keepTCPMessageID = "<fr06-synthetic@mailauto.example.invalid>"
const keepTCPRaw = "From: sender@example.invalid\r\nSubject: FR06 synthetic\r\nMessage-ID: <fr06-synthetic@mailauto.example.invalid>\r\n\r\noriginal fixture\r\n"

type keepTCPServer struct {
	mu       sync.Mutex
	listener net.Listener
	clients  map[net.Conn]bool
	wait     sync.WaitGroup
	trace    []string
	copies   int
	mode     string
	fault    string
	uids     map[string]uint32
	epochs   map[string]uint32
	rawReads int
	rawGate  *keepTCPGate
}

type keepTCPGate struct {
	arrived   chan struct{}
	release   chan struct{}
	remaining int
}

func newKeepTCPServer(t *testing.T, mode string) *keepTCPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &keepTCPServer{listener: listener, clients: map[net.Conn]bool{}, mode: mode,
		uids:   map[string]uint32{"INBOX": 42, "QA-Target": 99},
		epochs: map[string]uint32{"INBOX": 7, "QA-Target": 7, "QA-Other": 7}}
	server.wait.Add(1)
	go func() {
		defer server.wait.Done()
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			server.mu.Lock()
			server.clients[connection] = true
			server.mu.Unlock()
			server.wait.Add(1)
			go func() {
				defer server.wait.Done()
				defer connection.Close()
				defer func() { server.mu.Lock(); delete(server.clients, connection); server.mu.Unlock() }()
				server.serve(connection)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		server.mu.Lock()
		for connection := range server.clients {
			connection.Close()
		}
		server.mu.Unlock()
		server.wait.Wait()
	})
	return server
}

func (server *keepTCPServer) serve(connection net.Conn) {
	connection.SetDeadline(time.Now().Add(30 * time.Second))
	fmt.Fprint(connection, "* OK [CAPABILITY IMAP4rev1 UIDPLUS] isolated fixture\r\n")
	scanner := bufio.NewScanner(connection)
	folder := ""
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 2 {
			return
		}
		tag, command := parts[0], strings.ToUpper(parts[1])
		argument := ""
		if len(parts) == 3 {
			argument = parts[2]
		}
		server.mu.Lock()
		server.trace = append(server.trace, line)
		switch command {
		case "CAPABILITY":
			fmt.Fprintf(connection, "* CAPABILITY IMAP4rev1 UIDPLUS\r\n%s OK capabilities\r\n", tag)
		case "LOGIN":
			fmt.Fprintf(connection, "%s OK synthetic login\r\n", tag)
		case "LIST":
			for _, name := range []string{"INBOX", "QA-Target", "QA-Other"} {
				fmt.Fprintf(connection, "* LIST () \"/\" %q\r\n", name)
			}
			fmt.Fprintf(connection, "%s OK listed\r\n", tag)
		case "SELECT", "EXAMINE":
			folder = strings.Trim(argument, "\"")
			epoch := server.epochs[folder]
			if server.fault == "epoch" && folder == "QA-Target" {
				epoch++
			}
			count := 0
			if folder == "INBOX" || (folder == "QA-Target" && server.copies > 0) {
				count = 1
			}
			access := "READ-ONLY"
			if command == "SELECT" {
				access = "READ-WRITE"
			}
			fmt.Fprintf(connection, "* FLAGS (\\Seen \\Flagged \\Deleted)\r\n* %d EXISTS\r\n* OK [UIDVALIDITY %d] epoch\r\n%s OK [%s] selected\r\n", count, epoch, tag, access)
		case "UID":
			fields := strings.Fields(argument)
			if len(fields) < 2 {
				fmt.Fprintf(connection, "%s BAD malformed UID\r\n", tag)
				break
			}
			uid := server.uids[folder]
			present := folder == "INBOX" || (folder == "QA-Target" && server.copies > 0)
			switch strings.ToUpper(fields[0]) {
			case "SEARCH":
				if present {
					fmt.Fprintf(connection, "* SEARCH %d\r\n%s OK search\r\n", uid, tag)
				} else {
					fmt.Fprintf(connection, "* SEARCH\r\n%s OK search\r\n", tag)
				}
			case "FETCH":
				set, err := protocol.ParseSeqSet(fields[1])
				if err != nil || !present || !set.Contains(uid) {
					fmt.Fprintf(connection, "%s OK absent\r\n", tag)
					break
				}
				flags := "\\Seen"
				if folder == "QA-Target" && (server.fault == "flags" || (server.fault == "flags_after_peek" && server.rawReads >= 2)) {
					flags += " \\Flagged"
				}
				upper := strings.ToUpper(argument)
				if strings.Contains(upper, "BODY.PEEK[]") {
					if gate := server.rawGate; gate != nil && gate.remaining > 0 {
						gate.remaining--
						server.mu.Unlock()
						gate.arrived <- struct{}{}
						<-gate.release
						server.mu.Lock()
					}
					server.rawReads++
					raw := keepTCPRaw
					if folder == "QA-Target" && server.fault == "content" {
						raw = strings.Replace(raw, "original", "modified", 1)
					}
					fmt.Fprintf(connection, "* 1 FETCH (UID %d FLAGS (%s) RFC822.SIZE %d BODY[]<0> {%d}\r\n%s)\r\n%s OK raw\r\n", uid, flags, len(keepTCPRaw), len(raw), raw, tag)
				} else if strings.Contains(upper, "ENVELOPE") {
					headers := "References: <fr06-parent@mailauto.example.invalid>\r\nList-ID: <fr06.example.invalid>\r\n\r\n"
					fmt.Fprintf(connection, "* 1 FETCH (UID %d FLAGS (%s) RFC822.SIZE %d INTERNALDATE \"01-Jan-2026 00:00:00 +0000\" ENVELOPE (\"01-Jan-2026 00:00:00 +0000\" %q ((NIL NIL \"sender\" \"example.invalid\")) NIL NIL NIL NIL NIL NIL %q) BODYSTRUCTURE (\"TEXT\" \"PLAIN\" (\"CHARSET\" \"UTF-8\") NIL NIL \"7BIT\" 18 1) BODY[HEADER.FIELDS (REFERENCES LIST-ID)] {%d}\r\n%s)\r\n%s OK metadata\r\n", uid, flags, len(keepTCPRaw), keepTCPSubject, keepTCPMessageID, len(headers), headers, tag)
				} else {
					fmt.Fprintf(connection, "* 1 FETCH (UID %d FLAGS (%s))\r\n%s OK flags\r\n", uid, flags, tag)
				}
			case "COPY":
				if folder != "INBOX" || fields[1] != "42" || len(fields) != 3 || strings.Trim(fields[2], "\"") != "QA-Target" {
					fmt.Fprintf(connection, "%s BAD unexpected COPY\r\n", tag)
					break
				}
				server.copies++
				if server.mode == "lost_confirmation" {
					server.mu.Unlock()
					return
				}
				fmt.Fprintf(connection, "%s OK copied without COPYUID\r\n", tag)
			default:
				fmt.Fprintf(connection, "%s BAD forbidden UID mutation\r\n", tag)
			}
		case "LOGOUT":
			fmt.Fprintf(connection, "* BYE fixture\r\n%s OK logout\r\n", tag)
			server.mu.Unlock()
			return
		default:
			fmt.Fprintf(connection, "%s BAD unexpected command\r\n", tag)
		}
		server.mu.Unlock()
	}
}

func (server *keepTCPServer) adapter() *Adapter {
	adapter := NewAdapter()
	adapter.connect = func(ctx context.Context, account mail.Account, password string) (*connection, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", server.listener.Addr().String())
		if err != nil {
			return nil, err
		}
		raw.SetDeadline(time.Now().Add(10 * time.Second))
		stop := context.AfterFunc(ctx, func() { raw.Close() })
		connectionClient, err := client.New(raw)
		if err == nil {
			connectionClient.Timeout = 5 * time.Second
			connectionClient.ErrorLog = log.New(io.Discard, "", 0)
			err = connectionClient.Login(account.Username, password)
		}
		if err != nil {
			stop()
			raw.Close()
			return nil, err
		}
		return &connection{c: connectionClient, raw: raw, stop: stop, networkKey: account.NetworkRevision}, nil
	}
	return adapter
}

func (server *keepTCPServer) setFault(fault string) {
	server.mu.Lock()
	server.fault = fault
	server.mu.Unlock()
}

func (server *keepTCPServer) pauseRaw(t *testing.T, count int) (*keepTCPGate, func()) {
	t.Helper()
	gate := &keepTCPGate{arrived: make(chan struct{}, count), release: make(chan struct{}), remaining: count}
	server.mu.Lock()
	server.rawGate = gate
	server.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { close(gate.release) }) }
	t.Cleanup(release)
	return gate, release
}

func waitKeepTCPRead(t *testing.T, gate *keepTCPGate) {
	t.Helper()
	select {
	case <-gate.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("confirmation did not reach actual BODY.PEEK[] barrier")
	}
}

func (server *keepTCPServer) commands() []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]string{}, server.trace...)
}

func (server *keepTCPServer) assertNoReplay(t *testing.T) {
	t.Helper()
	commands := strings.ToUpper(strings.Join(server.commands(), "\n"))
	if strings.Count(commands, "UID COPY ") != 1 {
		t.Fatal("uncertain COPY replayed or never issued", commands)
	}
	for _, forbidden := range []string{"UID MOVE ", "UID STORE ", "EXPUNGE", "BODY[]"} {
		if strings.Contains(commands, forbidden) {
			t.Fatal("unsafe protocol command", forbidden, commands)
		}
	}
}

type keepTCPFixture struct {
	service   *mail.Service
	storage   *store.Store
	vault     *credentials.Vault
	server    *keepTCPServer
	admin     mail.Access
	accountID string
	operation mail.Operation
	source    mail.Message
	target    mail.Message
	directory string
	planHash  [32]byte
}

func (fixture *keepTCPFixture) freshService() *mail.Service {
	networks := network.New(fixture.storage, fixture.vault)
	return &mail.Service{Store: fixture.storage, Vault: fixture.vault, Network: networks, Remote: fixture.server.adapter(), Context: context.Background()}
}

func newKeepTCPFixture(t *testing.T, mode string) *keepTCPFixture {
	t.Helper()
	fixture := &keepTCPFixture{server: newKeepTCPServer(t, mode), directory: t.TempDir()}
	var err error
	fixture.storage, err = store.Open(filepath.Join(fixture.directory, "data"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.vault, err = credentials.Open(filepath.Join(fixture.directory, "keys"), true)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service = fixture.freshService()
	t.Cleanup(func() { fixture.service.Stop(); fixture.service.Network.CloseIdle(); fixture.storage.Close() })
	if _, err = fixture.storage.DB.Exec("INSERT INTO admin VALUES(1,'synthetic','unused',0,1,0,1)"); err != nil {
		t.Fatal(err)
	}
	fixture.admin, err = fixture.service.AdminAccess()
	if err != nil {
		t.Fatal(err)
	}
	account, err := fixture.service.Create(mail.AccountInput{Name: "isolated TCP fixture", Provider: "qq", Username: "fixture@qq.com", Password: "synthetic-only", Folders: []string{"INBOX", "QA-Target", "QA-Other"}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.accountID = account.ID
	if _, err = fixture.service.Sync(context.Background(), account.ID); err != nil {
		t.Fatal(err, fixture.server.commands())
	}
	if err = fixture.service.SaveFolderSettings(context.Background(), mail.FolderSettings{AccountID: account.ID, WritesEnabled: true}); err != nil {
		t.Fatal(err)
	}
	fixture.source = fixture.message(t, "INBOX")
	plan, err := fixture.service.NewPlan(context.Background(), fixture.admin, mail.PlanInput{Refs: []string{fixture.source.Ref}, Action: "move", Destination: "QA-Target"})
	if err != nil {
		t.Fatal(err)
	}
	fixture.operation, err = fixture.service.ExecutePlan(fixture.admin, plan.ID, "single-controlled-copy")
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.service.StartOperations(); err != nil {
		t.Fatal(err)
	}
	if err = fixture.service.StartOperations(); !errors.Is(err, mail.ErrBusy) {
		t.Fatal("duplicate Worker accepted", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		fixture.operation, err = fixture.service.Operation(fixture.admin, fixture.operation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fixture.operation.State == "needs_verification" {
			break
		}
		if time.Now().After(deadline) || (fixture.operation.State != "queued" && fixture.operation.State != "running") {
			t.Fatal("background Worker did not preserve unknown", fixture.operation, fixture.server.commands())
		}
		time.Sleep(10 * time.Millisecond)
	}
	fixture.service.Stop()
	fixture.service.Network.CloseIdle()
	fixture.service = fixture.freshService()
	if len(fixture.operation.Items) != 1 || fixture.operation.Items[0].Error != "remote_state_unknown" || fixture.operation.Items[0].Result.UID != 0 {
		t.Fatal("fixture did not preserve missing COPYUID", fixture.operation)
	}
	if _, err = fixture.service.Sync(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	fixture.target = fixture.message(t, "QA-Target")
	fixture.planHash = fixture.frozenHash(t)
	fixture.server.assertNoReplay(t)
	return fixture
}

func (fixture *keepTCPFixture) message(t *testing.T, folder string) mail.Message {
	t.Helper()
	messages, total, err := fixture.service.Search(mail.Search{AccountID: fixture.accountID, Folder: folder})
	if err != nil || total != 1 || len(messages) != 1 || messages[0].Subject != keepTCPSubject || messages[0].MessageID != keepTCPMessageID || !messages[0].FlagsKnown {
		t.Fatal("missing protocol-synchronized fixture", messages, total, err)
	}
	return messages[0]
}

func (fixture *keepTCPFixture) frozenHash(t *testing.T) [32]byte {
	t.Helper()
	var payload string
	if err := fixture.storage.DB.QueryRow("SELECT payload FROM operation_plans WHERE id=?", fixture.operation.PlanID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256([]byte(payload))
}

func (fixture *keepTCPFixture) assertDurable(t *testing.T, expected string) {
	t.Helper()
	operation, err := fixture.service.Operation(fixture.admin, fixture.operation.ID)
	if err != nil || operation.State != expected || operation.Cancelled || operation.Items[0].State != expected || operation.Items[0].Error != "remote_state_unknown" || operation.Items[0].Result.UID != 0 || operation.Items[0].Result.UIDValidity != 0 {
		t.Fatal("incorrect durable unknown/settlement", operation, err)
	}
	want := 0
	if expected == "manual_kept" {
		want = 1
	}
	var resolutions, blocks, audits int
	for query, destination := range map[string]*int{
		"SELECT count(*) FROM manual_resolutions":                               &resolutions,
		"SELECT count(*) FROM manual_keep_blocks":                               &blocks,
		"SELECT count(*) FROM audits WHERE action='mail.operation.manual_keep'": &audits,
	} {
		if err := fixture.storage.DB.QueryRow(query).Scan(destination); err != nil {
			t.Fatal(err)
		}
	}
	if resolutions != want || blocks != 2*want || audits != want || fixture.frozenHash(t) != fixture.planHash {
		t.Fatal("non-atomic evidence/blocks/audit or changed frozen plan", resolutions, blocks, audits)
	}
	if want == 1 {
		var payload string
		if err := fixture.storage.DB.QueryRow("SELECT payload FROM manual_resolutions WHERE operation_id=?", operation.ID).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var saved mail.ManualResolution
		if err := json.Unmarshal([]byte(payload), &saved); err != nil || saved.OriginalOperation.State != "needs_verification" || saved.OriginalOperation.Items[0].Error != "remote_state_unknown" || saved.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(keepTCPRaw))) || saved.Bytes != int64(len(keepTCPRaw)) {
			t.Fatal("lost actual raw/frozen unknown evidence", err, saved)
		}
	}
	fixture.server.assertNoReplay(t)
}

func (fixture *keepTCPFixture) preview(t *testing.T) mail.ManualKeepEvidence {
	t.Helper()
	evidence, err := fixture.service.PreviewManualKeep(context.Background(), fixture.admin, fixture.operation.ID)
	if err != nil || evidence.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(keepTCPRaw))) || evidence.Source.UID != fixture.source.UID || evidence.Target.UID != fixture.target.UID || evidence.Bytes != int64(len(keepTCPRaw)) {
		t.Fatal("full protocol preview failed", evidence, err, fixture.server.commands())
	}
	return evidence
}

func (fixture *keepTCPFixture) configureTasks(t *testing.T, engine, trigger string) []mail.Task {
	t.Helper()
	policy, err := fixture.service.AutomationPolicy(fixture.admin, fixture.accountID)
	if err != nil {
		t.Fatal(err)
	}
	policy.Enabled, policy.AllowInternal, policy.IncludeHistory = true, true, true
	policy.Mode, policy.SourceFolders, policy.Destinations, policy.Actions = "execute", []string{"INBOX", "QA-Target"}, []string{"QA-Other"}, []string{"move"}
	policy.Since, policy.StableSeconds = 0, 0
	if _, err = fixture.service.SaveAutomationPolicy(fixture.admin, policy); err != nil {
		t.Fatal(err)
	}
	rule, err := fixture.service.SaveRule(fixture.admin, mail.Rule{AccountID: fixture.accountID, Name: "synthetic only", Enabled: true, SourceFolders: []string{"INBOX", "QA-Target"}, Match: mail.RuleGroup{Mode: "all", Conditions: []mail.RuleCondition{{Field: "subject", Operator: "equals", Value: keepTCPSubject}}}, Action: "move", Destination: "QA-Other"})
	if err != nil {
		t.Fatal(err)
	}
	tasks := []mail.Task{}
	for _, name := range []string{"first", "second"} {
		task, err := fixture.service.SaveTask(fixture.admin, mail.Task{AccountID: fixture.accountID, Name: name, Engine: engine, Trigger: trigger, SourceFolders: []string{"INBOX", "QA-Target"}, RuleIDs: []string{rule.ID}, MaxBatch: 20})
		if err != nil {
			t.Fatal(err)
		}
		task.Mode, task.Enabled = "execute", true
		task, err = fixture.service.SaveTask(fixture.admin, task)
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	return tasks
}

func TestManualKeepTCPWorkerDurableProtection(t *testing.T) {
	for _, mode := range []string{"missing_mapping", "lost_confirmation"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newKeepTCPFixture(t, mode)
			prepared, err := fixture.service.NewPlan(context.Background(), fixture.admin, mail.PlanInput{Refs: []string{fixture.target.Ref}, Action: "move", Destination: "QA-Other"})
			if err != nil {
				t.Fatal(err)
			}
			evidence := fixture.preview(t)
			before := len(fixture.server.commands())
			resolved, err := fixture.service.ResolveManualKeep(context.Background(), fixture.admin, fixture.operation.ID, mail.ManualKeepInput{Evidence: evidence, Confirm: true})
			if err != nil || resolved.Outcome != "keep_both" {
				t.Fatal(resolved, err)
			}
			fixture.assertDurable(t, "manual_kept")
			if count := strings.Count(strings.Join(fixture.server.commands(), "\n"), "BODY.PEEK[]"); count != 4 {
				t.Fatal("preview and confirmation did not each read two complete originals", count)
			}
			for _, command := range fixture.server.commands()[before:] {
				if strings.Contains(command, " SELECT ") || strings.Contains(command, "UID COPY ") {
					t.Fatal("confirmation opened writable mailbox", command)
				}
			}
			if _, err = fixture.service.ExecutePlan(fixture.admin, prepared.ID, "prepared-before-settlement"); !errors.Is(err, mail.ErrDenied) {
				t.Fatal("prepared write bypassed permanent block", err)
			}
			before = len(fixture.server.commands())
			if _, err = fixture.service.ResolveManualKeep(context.Background(), fixture.admin, fixture.operation.ID, mail.ManualKeepInput{Evidence: evidence, Confirm: true}); err != nil || len(fixture.server.commands()) != before {
				t.Fatal("idempotent confirmation repeated remote work", err)
			}
			fixture.service.Stop()
			fixture.service.Network.CloseIdle()
			fixture.storage.Close()
			fixture.storage, err = store.Open(filepath.Join(fixture.directory, "data"))
			if err != nil {
				t.Fatal(err)
			}
			fixture.vault, err = credentials.Open(filepath.Join(fixture.directory, "keys"), false)
			if err != nil {
				t.Fatal(err)
			}
			fixture.service = fixture.freshService()
			fixture.server.mu.Lock()
			fixture.server.uids["INBOX"], fixture.server.uids["QA-Target"] = 84, 199
			fixture.server.epochs["INBOX"], fixture.server.epochs["QA-Target"] = 8, 9
			fixture.server.mu.Unlock()
			if _, err = fixture.service.Sync(context.Background(), fixture.accountID); err != nil {
				t.Fatal(err)
			}
			for _, folder := range []string{"INBOX", "QA-Target"} {
				message := fixture.message(t, folder)
				if message.Ref == fixture.source.Ref || message.Ref == fixture.target.Ref {
					t.Fatal("reindex did not change physical reference")
				}
				if _, err = fixture.service.NewPlan(context.Background(), fixture.admin, mail.PlanInput{Refs: []string{message.Ref}, Action: "move", Destination: "QA-Other"}); !errors.Is(err, mail.ErrDenied) {
					t.Fatal("restart/reindex lost permanent protection", err)
				}
			}
			tasks := fixture.configureTasks(t, "rules", "manual")
			for _, task := range tasks {
				preview, err := fixture.service.PreviewTask(fixture.admin, task.ID, "", false)
				if err != nil || len(preview.Items) != 2 {
					t.Fatal(preview, err)
				}
				for _, item := range preview.Items {
					if item.Status != "protected" || !strings.Contains(strings.Join(item.Reasons, " "), "人工核验") {
						t.Fatal("cross-task preview lost manual protection", item)
					}
				}
			}
			batch, err := fixture.service.ClaimAI(fixture.admin, mail.AIClaimInput{AccountID: fixture.accountID, RunKey: "after-restart-and-task-change"})
			if err != nil || len(batch.Items) != 0 || batch.Protected != 2 {
				t.Fatal("cross-task reclaim bypassed durable blocks", batch, err)
			}
			fixture.assertDurable(t, "manual_kept")
		})
	}
}

func TestManualKeepTCPRejectsChangedEvidence(t *testing.T) {
	for _, fault := range []string{"content", "flags", "epoch", "flags_after_peek", "stale_evidence", "network", "atomic_rollback"} {
		t.Run(fault, func(t *testing.T) {
			fixture := newKeepTCPFixture(t, "missing_mapping")
			evidence := fixture.preview(t)
			switch fault {
			case "stale_evidence":
				evidence.CheckedAt = time.Now().Unix() - 601
			case "atomic_rollback":
				if _, err := fixture.storage.DB.Exec("CREATE TRIGGER reject_settlement BEFORE INSERT ON manual_resolutions BEGIN SELECT RAISE(ABORT,'controlled fixture rollback'); END"); err != nil {
					t.Fatal(err)
				}
			case "network":
				transaction, err := fixture.storage.DB.Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer transaction.Rollback()
				if err = network.SetAccount(transaction, fixture.accountID, network.Policy{Mode: "direct"}); err != nil {
					t.Fatal(err)
				}
				if err = transaction.Commit(); err != nil {
					t.Fatal(err)
				}
			default:
				fixture.server.mu.Lock()
				fixture.server.rawReads = 0
				fixture.server.mu.Unlock()
				fixture.server.setFault(fault)
			}
			_, err := fixture.service.ResolveManualKeep(context.Background(), fixture.admin, fixture.operation.ID, mail.ManualKeepInput{Evidence: evidence, Confirm: true})
			if err == nil || (fault != "atomic_rollback" && !errors.Is(err, mail.ErrConflict)) {
				t.Fatal("changed protocol evidence or failed atomic commit accepted", fault)
			}
			if fault == "atomic_rollback" && !strings.Contains(err.Error(), "controlled fixture rollback") {
				t.Fatal("atomic rollback did not reach injected database failure", err)
			}
			fixture.assertDurable(t, "needs_verification")
		})
	}
}

func TestManualKeepTCPConcurrentConfirm(t *testing.T) {
	fixture := newKeepTCPFixture(t, "missing_mapping")
	evidence := fixture.preview(t)
	other := fixture.freshService()
	defer other.Stop()
	defer other.Network.CloseIdle()
	start := make(chan struct{})
	results := make(chan error, 2)
	gate, release := fixture.server.pauseRaw(t, 2)
	defer release()
	for _, service := range []*mail.Service{fixture.service, other} {
		go func() {
			<-start
			_, err := service.ResolveManualKeep(context.Background(), fixture.admin, fixture.operation.ID, mail.ManualKeepInput{Evidence: evidence, Confirm: true})
			results <- err
		}()
	}
	close(start)
	waitKeepTCPRead(t, gate)
	waitKeepTCPRead(t, gate)
	release()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal("concurrent confirmation not idempotent", err)
		}
	}
	fixture.assertDurable(t, "manual_kept")
}

func TestManualKeepTCPQueuedDuringConfirm(t *testing.T) {
	fixture := newKeepTCPFixture(t, "missing_mapping")
	evidence := fixture.preview(t)
	other := fixture.freshService()
	defer other.Stop()
	defer other.Network.CloseIdle()
	prepared, err := other.NewPlan(context.Background(), fixture.admin, mail.PlanInput{Refs: []string{fixture.target.Ref}, Action: "move", Destination: "QA-Other"})
	if err != nil {
		t.Fatal(err)
	}
	gate, release := fixture.server.pauseRaw(t, 1)
	defer release()
	result := make(chan error, 1)
	go func() {
		_, confirmErr := fixture.service.ResolveManualKeep(context.Background(), fixture.admin, fixture.operation.ID, mail.ManualKeepInput{Evidence: evidence, Confirm: true})
		result <- confirmErr
	}()
	waitKeepTCPRead(t, gate)
	queued, err := other.ExecutePlan(fixture.admin, prepared.ID, "queued-during-confirm")
	if err != nil || queued.State != "queued" {
		t.Fatal("controlled concurrent queue was not created", queued, err)
	}
	release()
	if err = <-result; !errors.Is(err, mail.ErrBusy) {
		t.Fatal("transaction failed to reject newly queued conflict", err)
	}
	fixture.assertDurable(t, "needs_verification")
}

func TestManualKeepTCPCrossTaskClaimConflict(t *testing.T) {
	fixture := newKeepTCPFixture(t, "missing_mapping")
	evidence := fixture.preview(t)
	tasks := fixture.configureTasks(t, "ai", "external")
	_, secret, err := fixture.service.CreateToken(mail.TokenInput{Name: "isolated external client", Accounts: []string{fixture.accountID}, Actions: []string{"read", "automation"}})
	if err != nil {
		t.Fatal(err)
	}
	access, err := fixture.service.AuthenticateToken(secret)
	if err != nil {
		t.Fatal(err)
	}
	var firstRun string
	for index, task := range tasks {
		run, err := fixture.service.RunTask(access, mail.TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "conflict"}, "external")
		if index == 1 {
			if !errors.Is(err, mail.ErrBusy) {
				t.Fatal("second task started a conflicting account run", run, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		batch, err := fixture.service.ClaimAI(access, mail.AIClaimInput{AccountID: fixture.accountID, RunID: run.ID})
		if err != nil {
			t.Fatal(err)
		}
		firstRun = run.ID
		if len(batch.Items) != 1 || batch.Items[0].Message.Ref != fixture.target.Ref {
			t.Fatal("foreign task did not claim exact target", batch)
		}
	}
	if _, err = fixture.service.ResolveManualKeep(context.Background(), fixture.admin, fixture.operation.ID, mail.ManualKeepInput{Evidence: evidence, Confirm: true}); !errors.Is(err, mail.ErrBusy) {
		t.Fatal("foreign task claim did not block settlement", err)
	}
	fixture.assertDurable(t, "needs_verification")
	if err = fixture.service.CancelTaskRun(access, firstRun); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.service.ResolveManualKeep(context.Background(), fixture.admin, fixture.operation.ID, mail.ManualKeepInput{Evidence: evidence, Confirm: true}); err != nil {
		t.Fatal("released foreign claim permanently blocked settlement", err)
	}
	run, err := fixture.service.RunTask(access, mail.TaskRunInput{TaskID: tasks[1].ID, Version: tasks[1].Version, RunKey: "after-settlement"}, "external")
	if err != nil {
		t.Fatal(err)
	}
	batch, err := fixture.service.ClaimAI(access, mail.AIClaimInput{AccountID: fixture.accountID, RunID: run.ID})
	if err != nil || len(batch.Items) != 0 || batch.Protected != 2 {
		t.Fatal("different task reclaimed manually retained copies", batch, err)
	}
	fixture.assertDurable(t, "manual_kept")
}
