package mail

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/network"
	"mail-auto/internal/store"
)

func digestBytesRegression(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func schema14RecoveryFixture(t *testing.T) (*Service, *organizerRemote, Access) {
	t.Helper()
	directory := t.TempDir()
	database, err := sql.Open("sqlite", filepath.Join(directory, "mail-auto.db"))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/schema14-recovery.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.Exec(string(fixture)); err != nil {
		t.Fatal(err)
	}
	var version int
	if err = database.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 14 {
		t.Fatal("fixture is not schema14", version, err)
	}
	if _, err = database.Exec("SELECT gmail_message_id FROM messages"); err == nil {
		t.Fatal("fixture already has candidate layout")
	}
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		upgraded, err := store.Open(directory)
		if err != nil {
			t.Fatal(err)
		}
		if err = upgraded.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != store.SchemaVersion {
			t.Fatal(version, err)
		}
		if err = upgraded.Close(); err != nil {
			t.Fatal(err)
		}
	}
	upgraded, err := store.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { upgraded.Close() })
	vault, err := credentials.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	remote := &organizerRemote{}
	service := &Service{Store: upgraded, Vault: vault, Remote: remote}
	service.Network = network.New(upgraded, vault)
	ciphertext, err := vault.Encrypt("synthetic-fixture", "account:qa-legacy")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, service, "UPDATE accounts SET secret=? WHERE id='qa-legacy'", ciphertext)
	access, err := service.AdminAccess()
	if err != nil {
		t.Fatal(err)
	}
	return service, remote, access
}

func TestSchema14PersistedRecoveryUpgrade(t *testing.T) {
	for _, scenario := range []string{"unchanged", "identity", "folders", "network", "oauth", "permission", "cleanup", "queued"} {
		t.Run(scenario, func(t *testing.T) {
			service, remote, access := schema14RecoveryFixture(t)
			var original string
			if err := service.Store.DB.QueryRow("SELECT payload FROM operation_plans WHERE id='qa-frozen'").Scan(&original); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "identity":
				mustExec(t, service, "UPDATE accounts SET username='other@qq.com' WHERE id='qa-legacy'")
			case "folders":
				mustExec(t, service, "UPDATE accounts SET folders='[\"INBOX\"]' WHERE id='qa-legacy'")
			case "network":
				setRegressionProxy(t, service, "qa-legacy")
			case "oauth":
				mustExec(t, service, "UPDATE accounts SET provider='google',host='imap.gmail.com' WHERE id='qa-legacy'")
				mustExec(t, service, "INSERT INTO mail_oauth_accounts(account_id,provider,login,config_version,status) VALUES('qa-legacy','google','fixture@gmail.com',0,'connected')")
			case "permission":
				mustExec(t, service, "UPDATE admin SET version=version+1")
			case "cleanup":
				remote.verifyErr = ErrUncertain
			case "queued":
				if _, err := service.ExecutePlan(access, "qa-unstarted", "new-key"); !errors.Is(err, ErrConflict) || err.Error() != "plan_identity_upgrade_required" {
					t.Fatal("unversioned preview admitted", err)
				}
				service.runOperation(context.Background(), "qa-queued")
				operation, err := service.Operation(access, "qa-queued")
				if err != nil || operation.State != "failed" || operation.Items[0].Error != "state_conflict" || remote.moves != 0 {
					t.Fatal("legacy queued write continued", operation, err)
				}
				return
			}
			result, err := service.Reconcile(context.Background(), access, "qa-unknown")
			if scenario == "unchanged" {
				if err != nil || result.State != "completed" {
					t.Fatal("unchanged historical identity rejected", result, err)
				}
			} else if scenario == "permission" {
				if !errors.Is(err, ErrDenied) {
					t.Fatal("revoked admin accepted", err)
				}
			} else {
				if err != nil || result.State != "needs_verification" {
					t.Fatal("historical safety check bypassed", result, err)
				}
				if scenario == "cleanup" && result.Items[0].RecoveryCode != "recovery_legacy_read_only" {
					t.Fatal(result.Items[0])
				}
			}
			if remote.moves != 0 || remote.finishCallCount != 0 {
				t.Fatal("historical verification issued a write")
			}
			var after string
			if err = service.Store.DB.QueryRow("SELECT payload FROM operation_plans WHERE id='qa-frozen'").Scan(&after); err != nil || after != original {
				t.Fatal("historical plan rewritten without evidence", err)
			}
		})
	}
}

func TestGmailPersistentPendingFence(t *testing.T) {
	for _, state := range []string{"queued", "running", "needs_verification"} {
		t.Run(state, func(t *testing.T) {
			service, access, message, alias := gmailServiceFixture(t)
			first, err := service.NewPlan(context.Background(), access, PlanInput{Refs: []string{message.Ref}, Action: "move", Destination: "Bills"})
			if err != nil {
				t.Fatal(err)
			}
			operation, err := service.ExecutePlan(access, first.ID, "first")
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, service, "UPDATE operation_items SET state=? WHERE operation_id=?", state, operation.ID)
			mustExec(t, service, "UPDATE operations SET state=? WHERE id=?", state, operation.ID)
			mustExec(t, service, "DELETE FROM messages WHERE ref=?", message.Ref)
			mustExec(t, service, "UPDATE messages SET ref='reindexed-alias',uidvalidity=8,uid=12 WHERE ref=?", alias.Ref)
			restarted := reopenRegressionService(t, service)
			plan, err := restarted.NewPlan(context.Background(), access, PlanInput{Refs: []string{"reindexed-alias"}, Action: "move", Destination: "INBOX"})
			if err != nil {
				t.Fatal(err)
			}
			var workers sync.WaitGroup
			for index, key := range []string{"second", "third"} {
				contender := []*Service{service, restarted}[index]
				workers.Go(func() {
					if next, err := contender.ExecutePlan(access, plan.ID, key); !errors.Is(err, ErrBusy) {
						t.Errorf("lost durable logical fence: %s %v", next.State, err)
					}
				})
			}
			workers.Wait()
			available, err := restarted.claimAvailable(message.AccountID, "reindexed-alias", "new-fingerprint", time.Now().Unix())
			if err != nil || available {
				t.Fatalf("pending alias claim allowed: %v %v", available, err)
			}
			current, err := restarted.Operation(access, operation.ID)
			if err != nil || current.Items[0].State != state {
				t.Fatalf("pending evidence changed: %+v %v", current, err)
			}
			mustExec(t, service, "UPDATE operation_items SET state='completed' WHERE operation_id=?", operation.ID)
			mustExec(t, service, "UPDATE operations SET state='completed' WHERE id=?", operation.ID)
			if _, err = restarted.ExecutePlan(access, plan.ID, "after-resolution"); err != nil {
				t.Fatal("terminal evidence must release fence", err)
			}
		})
	}
}

func TestGmailPersistentAutomationClaim(t *testing.T) {
	service, access, message, alias := gmailServiceFixture(t)
	batch := AIBatch{ID: "durable-batch", AccountID: message.AccountID, Items: []RuleEvaluation{{Message: message}}, Expires: time.Now().Unix() + 600}
	payload, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, service, "INSERT INTO automation_batches(id,account_id,owner,run_key,payload,state,created_at,expires) VALUES(?,?,?,'claim',?,'claimed',?,?)", batch.ID, batch.AccountID, access.Owner, string(payload), time.Now().Unix(), batch.Expires)
	mustExec(t, service, "INSERT INTO automation_claims(account_id,ref,fingerprint,batch_id,state,expires,plan_id) VALUES(?,?,'original',?,'claimed',?,'')", message.AccountID, message.Ref, batch.ID, batch.Expires)
	mustExec(t, service, "DELETE FROM messages WHERE ref=?", message.Ref)
	restarted := reopenRegressionService(t, service)
	available, err := restarted.claimAvailable(alias.AccountID, alias.Ref, "new-fingerprint", time.Now().Unix())
	if err != nil || available {
		t.Fatal("lost cross-label claim after index deletion", available, err)
	}
	plan, err := restarted.NewPlan(context.Background(), access, PlanInput{Refs: []string{alias.Ref}, Action: "move", Destination: "INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.ExecutePlan(access, plan.ID, "manual-v-claim"); !errors.Is(err, ErrBusy) {
		t.Fatal("manual queue bypassed automation claim", err)
	}
	mustExec(t, service, "UPDATE automation_claims SET expires=0 WHERE batch_id=?", batch.ID)
	if _, err = restarted.ExecutePlan(access, plan.ID, "expired-claim"); err != nil {
		t.Fatal("expired claim blocks future intentional action", err)
	}
}

func reopenRegressionService(t *testing.T, service *Service) *Service {
	t.Helper()
	var sequence int
	var name, path string
	if err := service.Store.DB.QueryRow("PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	return &Service{Store: reopened, Vault: service.Vault, Remote: service.Remote}
}

func TestGmailClaimAIAfterSourceRemoved(t *testing.T) {
	service, admin, message, alias := gmailServiceFixture(t)
	mustExec(t, service, "INSERT INTO mailboxes(account_id,name,uidvalidity,synced_at) VALUES(?,'QA',7,1)", message.AccountID)
	mustExec(t, service, "INSERT INTO mail_oauth_folder_roles(account_id,name,role) VALUES(?,'QA','')", message.AccountID)
	policy := defaultPolicy(message.AccountID)
	policy.Enabled, policy.Mode, policy.IncludeHistory, policy.StableSeconds = true, "execute", true, 0
	policy.Since = 0
	policy.SourceFolders, policy.Destinations, policy.Actions = []string{"INBOX", "Bills"}, []string{"QA"}, []string{"move"}
	if _, err := service.SaveAutomationPolicy(admin, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SaveRule(admin, Rule{AccountID: message.AccountID, Name: "QA", Enabled: true, SourceFolders: []string{"INBOX", "Bills"}, Match: RuleGroup{Mode: "all"}, Action: "move", Destination: "QA"}); err != nil {
		t.Fatal(err)
	}
	batch, err := service.ClaimAI(admin, AIClaimInput{AccountID: message.AccountID, RunKey: "first"})
	if err != nil || len(batch.Items) != 1 {
		t.Fatal("initial logical claim", batch, err)
	}
	mustExec(t, service, "DELETE FROM messages WHERE ref=?", batch.Items[0].Message.Ref)
	remaining := alias.Ref
	if batch.Items[0].Message.Ref == alias.Ref {
		remaining = message.Ref
	}
	available, err := service.claimAvailable(message.AccountID, remaining, "different", time.Now().Unix())
	if err != nil || available {
		t.Fatal("live claim lost after synchronization", available, err)
	}
	second, err := service.ClaimAI(admin, AIClaimInput{AccountID: message.AccountID, RunKey: "second"})
	if err != nil || len(second.Items) != 0 {
		t.Fatal("automation reclaims same Gmail identity", second, err)
	}
}

func TestLegacyManualKeepReadOnlyCompatibility(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "network-changed"}[change], func(t *testing.T) {
			service, remote, access, _, operation, source, _ := manualKeepFixture(t)
			account, err := service.Account(source.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := service.GetPlan(access, operation.PlanID)
			if err != nil {
				t.Fatal(err)
			}
			old, err := json.Marshal([]any{account.Provider, account.Username, account.Host, account.Port, account.Folders})
			if err != nil {
				t.Fatal(err)
			}
			plan.Items[0].Config = digestBytesRegression(old)
			payload, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, service, "UPDATE operation_plans SET payload=? WHERE id=?", string(payload), plan.ID)
			service.Network = network.New(service.Store, service.Vault)
			evidence, err := service.PreviewManualKeep(context.Background(), access, operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if change {
				setRegressionProxy(t, service, source.AccountID)
			}
			result, err := service.ResolveManualKeep(context.Background(), access, operation.ID, ManualKeepInput{Evidence: evidence, Confirm: true})
			if change {
				if !errors.Is(err, ErrConflict) {
					t.Fatal("legacy network change accepted", result, err)
				}
			} else if err != nil || result == nil {
				t.Fatal("legacy local settlement rejected", err)
			}
			if remote.moves != 1 || remote.finishCallCount != 0 {
				t.Fatal("legacy settlement replayed write")
			}
		})
	}
}

func TestGmailMoveUndoFailsClosed(t *testing.T) {
	for _, scenario := range []string{"target-existed", "target-new", "user-changed-labels", "missing-evidence", "expired-evidence", "unknown-write", "archive"} {
		t.Run(scenario, func(t *testing.T) {
			service, access, message, alias := gmailServiceFixture(t)
			capabilities, err := service.RuleCapabilities(access, message.AccountID)
			if err != nil || !strings.Contains(capabilities["restriction"].(string), "撤销") {
				t.Fatal("rule/task capabilities omitted undo restriction", capabilities, err)
			}
			if scenario == "target-new" {
				mustExec(t, service, "DELETE FROM messages WHERE ref=?", alias.Ref)
			}
			input := PlanInput{Refs: []string{message.Ref}, Action: "move", Destination: "Bills"}
			if scenario == "archive" {
				input.Action, input.Destination = "archive", ""
			}
			if scenario == "unknown-write" {
				service.Remote.(*organizerRemote).uncertain = true
			}
			operation := executeFixture(t, service, access, input)
			if scenario == "user-changed-labels" {
				mustExec(t, service, "UPDATE messages SET folder='Trash' WHERE ref=?", message.Ref)
			}
			if scenario == "expired-evidence" {
				mustExec(t, service, "UPDATE operations SET created_at=1,updated_at=1 WHERE id=?", operation.ID)
			}
			if scenario == "missing-evidence" {
				mustExec(t, service, "DELETE FROM messages WHERE account_id=?", message.AccountID)
			}
			var before int
			if err := service.Store.DB.QueryRow("SELECT count(*) FROM operation_plans").Scan(&before); err != nil {
				t.Fatal(err)
			}
			plans, err := service.UndoPlan(context.Background(), access, operation.ID)
			var refusal *recoveryBlock
			if !errors.Is(err, ErrCapability) || !errors.As(err, &refusal) || refusal.code != "gmail_undo_unsupported" || len(plans) != 0 {
				t.Fatalf("unsafe compensation returned: %+v %v", plans, err)
			}
			var after int
			if err = service.Store.DB.QueryRow("SELECT count(*) FROM operation_plans").Scan(&after); err != nil || before != after {
				t.Fatal("rejection persisted compensating plan", err)
			}
		})
	}
}

func setRegressionProxy(t *testing.T, service *Service, accountID string) {
	t.Helper()
	settings, err := service.Network.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings, err = service.Network.Save(network.Input{Version: settings.Version, Global: network.Policy{Mode: "direct"}, Telegram: network.Policy{Mode: "inherit"}, Agent: network.Policy{Mode: "inherit"}, Profiles: []network.ProfileInput{{Profile: network.Profile{ID: "qa-proxy", Name: "QA", Protocol: "http", Host: "127.0.0.1", Port: 18888}}}})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, service, "INSERT INTO account_network(account_id,mode,proxy_id) VALUES(?,'proxy',?) ON CONFLICT(account_id) DO UPDATE SET mode=excluded.mode,proxy_id=excluded.proxy_id", accountID, settings.Profiles[0].ID)
}

func freezeCurrentRegressionPlan(t *testing.T, service *Service, access Access, operation Operation, accountID string) {
	t.Helper()
	account, err := service.Account(accountID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.GetPlan(access, operation.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	plan.Items[0].Config = accountIdentity(account)
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, service, "UPDATE operation_plans SET payload=? WHERE id=?", string(payload), plan.ID)
}

func TestManualKeepProductionNetworkSnapshot(t *testing.T) {
	for _, route := range []string{"direct", "proxy"} {
		for _, change := range []string{"unchanged", "identity", "network-before-confirm", "network-during-read"} {
			t.Run(route+"/"+change, func(t *testing.T) {
				service, remote, access, _, operation, source, _ := manualKeepFixture(t)
				service.Network = network.New(service.Store, service.Vault)
				if route == "proxy" {
					setRegressionProxy(t, service, source.AccountID)
				}
				freezeCurrentRegressionPlan(t, service, access, operation, source.AccountID)
				evidence, err := service.PreviewManualKeep(context.Background(), access, operation.ID)
				if err != nil {
					t.Fatal(err)
				}
				mutateNetwork := func() {
					if route == "direct" {
						setRegressionProxy(t, service, source.AccountID)
					} else {
						mustExec(t, service, "UPDATE account_network SET mode='direct',proxy_id='' WHERE account_id=?", source.AccountID)
					}
				}
				switch change {
				case "identity":
					mustExec(t, service, "UPDATE accounts SET username='changed@qq.com' WHERE id=?", source.AccountID)
				case "network-before-confirm":
					mutateNetwork()
				case "network-during-read":
					remote.onVerify = mutateNetwork
				}
				result, err := service.ResolveManualKeep(context.Background(), access, operation.ID, ManualKeepInput{Evidence: evidence, Confirm: true})
				if change == "unchanged" {
					if err != nil || result == nil || result.Outcome != "keep_both" {
						t.Fatal("unchanged full snapshot rejected", err)
					}
				} else if !errors.Is(err, ErrConflict) {
					t.Fatal("changed snapshot accepted", result, err)
				}
				if remote.moves != 1 || remote.finishCallCount != 0 {
					t.Fatal("manual resolution emitted remote writes")
				}
				if change != "unchanged" {
					var count int
					if err = service.Store.DB.QueryRow("SELECT count(*) FROM manual_resolutions").Scan(&count); err != nil || count != 0 {
						t.Fatal("conflicting evidence persisted", count, err)
					}
				}
			})
		}
	}
}

func TestUnversionedCandidateRecoveryIsReadOnly(t *testing.T) {
	service, remote, access, message := organizerFixture(t)
	remote.uncertain = true
	operation := executeFixture(t, service, access, PlanInput{Refs: []string{message.Ref}, Action: "move", Destination: "Archive"})
	account, err := service.Account(message.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := json.Marshal([]any{account.Provider, account.Username, account.Host, account.Port, account.Folders, account.NetworkRevision, account.OAuth})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.GetPlan(access, operation.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	plan.Items[0].Config = digestBytesRegression(previous)
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, service, "UPDATE operation_plans SET payload=? WHERE id=?", string(payload), plan.ID)
	remote.verifyErr = ErrUncertain
	result, err := service.Reconcile(context.Background(), access, operation.ID)
	if err != nil || result.State != "needs_verification" || result.Items[0].RecoveryCode != "recovery_legacy_read_only" || remote.finishCallCount != 0 {
		t.Fatal("legacy plan cleaned source", result, err)
	}
	remote.verifyErr = nil
	result, err = service.Reconcile(context.Background(), access, operation.ID)
	if err != nil || result.State != "completed" || remote.moves != 1 {
		t.Fatal("legacy read-only proof rejected", result, err)
	}
	if !strings.HasPrefix(accountIdentity(account), "v2:") {
		t.Fatal("new snapshot is unversioned")
	}
}
