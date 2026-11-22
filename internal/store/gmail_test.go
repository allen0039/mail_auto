package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestGmailMigrationFromFourteen(t *testing.T) {
	directory := t.TempDir()
	database, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.DB.Exec(`INSERT INTO accounts(id,name,provider,username,host,port,folders,secret,secret_file,created_at) VALUES('old','legacy','qq','fixture@qq.com','imap.qq.com',993,'["INBOX"]',X'010203','external-secret',1);
INSERT INTO automation_tasks(id,account_id,version,service_owner,payload,enabled,trigger_mode,updated_at) VALUES('task','old',1,'owner','{"max_batch":17,"protect_unread":true}',1,'manual',1);
DROP TABLE gmail_run_claims; DROP TABLE gmail_folder_roles;
DROP INDEX messages_gmail_identity; ALTER TABLE messages DROP COLUMN gmail_message_id; ALTER TABLE sync_stage DROP COLUMN gmail_message_id; PRAGMA user_version=14`)
	if err != nil {
		t.Fatal(err)
	}
	database.Close()
	for count := 0; count < 2; count++ {
		database, err = Open(directory)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		var secret []byte
		var external, folders, payload string
		if err = database.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
			t.Fatal(version, err)
		}
		if err = database.DB.QueryRow("SELECT secret,secret_file,folders FROM accounts WHERE id='old'").Scan(&secret, &external, &folders); err != nil {
			t.Fatal(err)
		}
		if string(secret) != string([]byte{1, 2, 3}) || external != "external-secret" || folders != "[\"INBOX\"]" {
			t.Fatal("legacy credentials or folder policy changed")
		}
		if err = database.DB.QueryRow("SELECT payload FROM automation_tasks WHERE id='task'").Scan(&payload); err != nil || payload != "{\"max_batch\":17,\"protect_unread\":true}" {
			t.Fatal("task altered", err)
		}
		var count int
		if err = database.DB.QueryRow("SELECT count(*) FROM gmail_folder_roles").Scan(&count); err != nil || count != 0 {
			t.Fatal("unexpected Gmail folder mapping for legacy account")
		}
		database.Close()
	}
}

func TestGmailMigrationRollsBack(t *testing.T) {
	directory := t.TempDir()
	database, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.DB.Exec(`DROP TABLE gmail_run_claims;DROP TABLE gmail_folder_roles;DROP INDEX messages_gmail_identity;ALTER TABLE messages DROP COLUMN gmail_message_id;ALTER TABLE sync_stage DROP COLUMN gmail_message_id;CREATE TABLE messages_gmail_identity(blocker TEXT);PRAGMA user_version=14`)
	if err != nil {
		t.Fatal(err)
	}
	database.Close()
	if unexpected, err := Open(directory); err == nil {
		unexpected.Close()
		t.Fatal("broken migration accepted")
	}
	raw, err := sql.Open("sqlite", filepath.Join(directory, "mail-auto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version, count int
	if err = raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 14 {
		t.Fatal("schema version partially advanced", version, err)
	}
	if err = raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('gmail_folder_roles','gmail_run_claims')").Scan(&count); err != nil || count != 0 {
		t.Fatal("partial migration left tables", count, err)
	}
	if _, err = raw.Exec("DROP TABLE messages_gmail_identity"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	database, err = Open(directory)
	if err != nil {
		t.Fatal("restart failed", err)
	}
	database.Close()
}

func TestGmailMigrationDropsMailboxOAuthOnly(t *testing.T) {
	for _, prior := range []struct {
		schema         string
		accountColumns string
	}{
		{"15", ""},
		{"16", ", flow TEXT NOT NULL DEFAULT '', client_id TEXT NOT NULL DEFAULT ''"},
	} {
		t.Run(prior.schema, func(t *testing.T) {
			directory := t.TempDir()
			database, err := Open(directory)
			if err != nil {
				t.Fatal(err)
			}
			_, err = database.DB.Exec(`INSERT INTO accounts(id,name,provider,username,host,port,folders,secret,secret_file,created_at) VALUES
('gmail','Gmail','google','fixture@gmail.com','imap.gmail.com',993,'["INBOX"]',NULL,'',1),
('outlook','Outlook','microsoft','fixture@outlook.test','outlook.office365.com',993,'["INBOX"]',NULL,'',1);
ALTER TABLE gmail_folder_roles RENAME TO mail_oauth_folder_roles;
INSERT INTO mail_oauth_folder_roles VALUES('gmail','All Mail','archive');
INSERT INTO mailboxes(account_id,name,uidvalidity,synced_at) VALUES('outlook','INBOX',1,1);
INSERT INTO messages(ref,account_id,folder,uidvalidity,uid,subject,sender,sent_at,unread,size) VALUES('old-message','outlook','INBOX',1,1,'fixture','fixture@test',1,1,128);
INSERT INTO oauth_clients(id,name,redirects,created_at) VALUES('mcp-client','MCP','[]',1);
CREATE TABLE mail_oauth_apps(provider TEXT PRIMARY KEY, secret BLOB);
INSERT INTO mail_oauth_apps VALUES('google',X'03'),('microsoft',X'04');
CREATE TABLE mail_oauth_accounts(account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE, refresh BLOB` + prior.accountColumns + `);
INSERT INTO mail_oauth_accounts(account_id,refresh) VALUES('gmail',X'01'),('outlook',X'02');
CREATE TABLE mail_oauth_states(state_hash TEXT PRIMARY KEY, verifier BLOB);
CREATE INDEX mail_oauth_states_account ON mail_oauth_states(state_hash);
INSERT INTO mail_oauth_states VALUES('state',X'05');
PRAGMA user_version=` + prior.schema)
			database.Close()
			if err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				database, err = Open(directory)
				if err != nil {
					t.Fatal(err)
				}
				var retired, accounts, mail, clients, schema int
				var role string
				err = database.DB.QueryRow(`SELECT
 (SELECT count(*) FROM sqlite_master WHERE name GLOB 'mail_oauth_*'),
 (SELECT count(*) FROM accounts),
 (SELECT count(*) FROM messages WHERE ref='old-message'),
 (SELECT count(*) FROM oauth_clients WHERE id='mcp-client'),
 (SELECT user_version FROM pragma_user_version),
 (SELECT role FROM gmail_folder_roles WHERE account_id='gmail' AND name='All Mail')`).Scan(&retired, &accounts, &mail, &clients, &schema, &role)
				database.Close()
				if err != nil || retired != 0 || accounts != 2 || mail != 1 || clients != 1 || schema != SchemaVersion || role != "archive" {
					t.Fatal("migration did not remove only mailbox OAuth and preserve data", retired, accounts, mail, clients, schema, role, err)
				}
			}
		})
	}
}
