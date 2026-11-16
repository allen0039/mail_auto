package mail

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func gmailServiceFixture(t *testing.T) (*Service, Access, Message, Message) {
	t.Helper()
	service, _, access, original := organizerFixture(t)
	mustExec(t, service, "DELETE FROM messages WHERE ref<>?", original.Ref)
	mustExec(t, service, "UPDATE accounts SET provider='google',host='imap.gmail.com' WHERE id=?", original.AccountID)
	mustExec(t, service, "INSERT INTO mail_oauth_accounts(account_id,provider,login,config_version,status) VALUES(?,'google','fixture@gmail.com',0,'connected')", original.AccountID)
	mustExec(t, service, "UPDATE messages SET gmail_message_id='18446744073709551614',metadata_version=2,flags_known=1 WHERE ref=?", original.Ref)
	mustExec(t, service, "INSERT INTO mailboxes(account_id,name,uidvalidity,synced_at) VALUES(?,'Bills',7,1)", original.AccountID)
	for name, role := range map[string]string{"INBOX": "inbox", "Bills": "", "Archive": "archive", "Trash": "trash"} {
		mustExec(t, service, "INSERT INTO mail_oauth_folder_roles(account_id,name,role) VALUES(?,?,?)", original.AccountID, name, role)
	}
	mustExec(t, service, "INSERT INTO messages("+messageColumns+") SELECT 'gmail-alias',account_id,'Bills',uidvalidity,99,subject,sender,sent_at,unread,size,received_at,flagged,flags_known,changed_at,metadata_version,recipients,cc,message_id,in_reply_to,thread_refs,list_id,has_attachment,gmail_message_id FROM messages WHERE ref=?", original.Ref)
	message, err := service.Message(original.Ref)
	if err != nil {
		t.Fatal(err)
	}
	alias, err := service.Message("gmail-alias")
	if err != nil {
		t.Fatal(err)
	}
	return service, access, message, alias
}

func TestGmailLogicalPlansAndConcurrency(t *testing.T) {
	service, access, message, alias := gmailServiceFixture(t)
	plan, err := service.NewPlan(context.Background(), access, PlanInput{Refs: []string{message.Ref, alias.Ref}, Action: "move", Destination: "INBOX"})
	if err != nil || len(plan.Items) != 1 {
		t.Fatal("logical message planned twice", len(plan.Items), err)
	}
	encoded, _ := json.Marshal(plan)
	if !strings.Contains(string(encoded), `"gmail_message_id":"18446744073709551614"`) {
		t.Fatal("stable ID lost string precision")
	}
	first, err := service.NewPlan(context.Background(), access, PlanInput{Refs: []string{message.Ref}, Action: "move", Destination: "Bills"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.NewPlan(context.Background(), access, PlanInput{Refs: []string{alias.Ref}, Action: "move", Destination: "INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ExecutePlan(access, first.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err = service.ExecutePlan(access, second.ID, "second"); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrent aliases were not fenced", err)
	}
	accounts, err := service.Accounts()
	if err != nil || accounts[0].UniqueMessages != 1 || accounts[0].MessageEntries != 2 {
		t.Fatal("Gmail count basis wrong", err)
	}
}

func TestGmailRestrictedActionsAndFlags(t *testing.T) {
	service, access, message, alias := gmailServiceFixture(t)
	for _, input := range []PlanInput{{Refs: []string{message.Ref}, Action: "trash"}, {Refs: []string{message.Ref}, Action: "restore", Destination: "INBOX"}, {Refs: []string{alias.Ref}, Action: "archive"}, {Refs: []string{message.Ref}, Action: "move", Destination: "Trash"}} {
		if _, err := service.NewPlan(context.Background(), access, input); !errors.Is(err, ErrCapability) {
			t.Fatal("unsupported Gmail action accepted", input.Action, err)
		}
	}
	fresh := message
	fresh.Flagged = true
	fresh.Unread = false
	if err := service.publishFlags(message, fresh); err != nil {
		t.Fatal(err)
	}
	current, err := service.Message(alias.Ref)
	if err != nil || !current.Flagged || current.Unread {
		t.Fatal("flags not propagated to label views", err)
	}
}

func TestGmailFolderRolesAndStaleProbe(t *testing.T) {
	service, _, message, _ := gmailServiceFixture(t)
	account, err := service.Account(message.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	folders := []FolderInfo{{Name: "INBOX", Role: "inbox"}, {Name: "Bills"}, {Name: "Localized All", Role: "archive"}, {Name: "Localized Starred", Role: "flagged"}, {Name: "Unknown Special", Role: "special"}}
	if err = service.saveGmailFolderRoles(account, folders); err != nil {
		t.Fatal(err)
	}
	settings := FolderSettings{Archive: "Localized All"}
	for _, folder := range []string{"Localized Starred", "Unknown Special", "Not Probed"} {
		if err = service.gmailFolderAction(account, "INBOX", "move", folder, settings); !errors.Is(err, ErrCapability) {
			t.Fatal("special or unknown destination accepted", folder, err)
		}
		if err = service.gmailFolderAction(account, folder, "move", "Bills", settings); !errors.Is(err, ErrCapability) {
			t.Fatal("special or unknown source accepted", folder, err)
		}
	}
	if err = service.gmailFolderAction(account, "INBOX", "archive", "", settings); err != nil {
		t.Fatal("localized All Mail archive blocked", err)
	}
	mustExec(t, service, "UPDATE mail_oauth_accounts SET generation=generation+1 WHERE account_id=?", account.ID)
	if err = service.saveGmailFolderRoles(account, []FolderInfo{{Name: "stale"}}); !errors.Is(err, ErrConflict) {
		t.Fatal("stale probe published roles", err)
	}
	var role string
	if err = service.Store.DB.QueryRow("SELECT role FROM mail_oauth_folder_roles WHERE account_id=? AND name='Localized All'", account.ID).Scan(&role); err != nil || role != "archive" {
		t.Fatal("stale probe replaced live capability state", err)
	}
}

func TestGmailManualProtectionAcrossViewsWithoutRFCIdentity(t *testing.T) {
	service, access, message, alias := gmailServiceFixture(t)
	mustExec(t, service, "UPDATE messages SET message_id='' WHERE account_id=?", message.AccountID)
	mustExec(t, service, "INSERT INTO manual_keep_blocks(account_id,ref,folder,uidvalidity,uid,message_id,resolution_id) VALUES(?,?,?,?,?,'','fixture')", alias.AccountID, alias.Ref, alias.Folder, alias.UIDValidity, alias.UID)
	message.MessageID = ""
	kept, err := manuallyKept(service.Store.DB, message)
	if err != nil || !kept {
		t.Fatal("Gmail sibling manual protection ignored", err)
	}
	if _, err = service.NewPlan(context.Background(), access, PlanInput{Refs: []string{message.Ref}, Action: "move", Destination: "Bills"}); !errors.Is(err, ErrDenied) {
		t.Fatal("manual protection bypassed through another label", err)
	}
}

func TestGmailMultiSourceRuleOrderAndProtection(t *testing.T) {
	service, _, message, alias := gmailServiceFixture(t)
	now := time.Now().Unix()
	policy := AutomationPolicy{SourceFolders: []string{"INBOX", "Bills"}, ProtectUnread: true, ProtectFlagged: true, IncludeHistory: true}
	first := Rule{ID: "first", Enabled: true, Priority: 1, SourceFolders: []string{"Bills"}, Action: "keep", Match: RuleGroup{Mode: "all"}}
	second := Rule{ID: "second", Enabled: true, Priority: 2, SourceFolders: []string{"INBOX"}, Action: "archive", Match: RuleGroup{Mode: "all"}}
	for _, view := range []Message{message, alias} {
		result, err := service.evaluateMessageRules(view, []Rule{first, second}, policy, now)
		if err != nil || result.RuleID != "first" || result.Message.Ref != alias.Ref {
			t.Fatal("source order changed winning action", result.RuleID, err)
		}
	}
	mustExec(t, service, "UPDATE messages SET unread=1,flags_known=1 WHERE ref=?", alias.Ref)
	result, err := service.evaluateMessageRules(message, []Rule{second}, policy, now)
	if err != nil || result.Status != "protected" {
		t.Fatal("unread sibling view bypassed protection", result, err)
	}
	mustExec(t, service, "UPDATE messages SET unread=0,flagged=1 WHERE ref=?", alias.Ref)
	result, err = service.evaluateMessageRules(message, []Rule{second}, policy, now)
	if err != nil || result.Status != "protected" {
		t.Fatal("starred sibling view bypassed protection", result, err)
	}
}

func TestOAuthAccountLifecycleAndLegacyCompatibility(t *testing.T) {
	service, _, _, message := organizerFixture(t)
	for _, provider := range []string{"google", "microsoft"} {
		account, err := service.Create(AccountInput{Name: provider, Provider: provider, Username: "fixture@example.test"})
		if err != nil || account.Authentication != "oauth2" || account.OAuth.Status != "configuration_missing" {
			t.Fatal("pending account creation", err)
		}
		if secret, err := service.password(account); err != nil || secret != "" {
			t.Fatal("token masqueraded as password")
		}
		if _, err = service.Update(account.ID, AccountInput{Name: provider, Provider: provider, Username: "other@example.test"}); !errors.Is(err, ErrAccountInput) {
			t.Fatal("OAuth account silently replaced")
		}
		if _, err = service.Create(AccountInput{Name: provider, Provider: provider, Username: "fixture@example.test", Password: "password"}); err == nil {
			t.Fatal("OAuth password accepted")
		}
		if err = service.Delete(account.ID); err != nil {
			t.Fatal(err)
		}
		var count int
		if err = service.Store.DB.QueryRow("SELECT count(*) FROM mail_oauth_accounts WHERE account_id=?", account.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal("OAuth account orphaned", err)
		}
	}
	legacy, err := service.Account(message.AccountID)
	if err != nil || legacy.Authentication != "password" || legacy.OAuth != nil {
		t.Fatal("legacy auth changed", err)
	}
}

func TestGmailUnknownWithoutMappingUsesStableIdentityEvidence(t *testing.T) {
	service, access, message, _ := gmailServiceFixture(t)
	remote := service.Remote.(*organizerRemote)
	remote.uncertain, remote.omitMapping = true, true
	remote.inspectEvidence = MoveEvidence{SourcePresent: false, DestinationPresent: true, Destination: MoveResult{Folder: "Bills", UIDValidity: 7, UID: 88}}
	operation := executeFixture(t, service, access, PlanInput{Refs: []string{message.Ref}, Action: "move", Destination: "Bills"})
	service.runOperation(context.Background(), operation.ID)
	operation, err := service.Reconcile(context.Background(), access, operation.ID)
	if err != nil || operation.State != "completed" || remote.moves != 1 || remote.finishCallCount != 0 {
		t.Fatal("stable ID evidence did not resolve without replay", operation.State, err)
	}
}
