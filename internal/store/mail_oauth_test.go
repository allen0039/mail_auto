package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMailOAuthMigrationFromFourteen(t *testing.T) {
	directory := t.TempDir()
	database, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.DB.Exec(`INSERT INTO accounts(id,name,provider,username,host,port,folders,secret,secret_file,created_at) VALUES('old','legacy','qq','fixture@qq.com','imap.qq.com',993,'["INBOX"]',X'010203','external-secret',1);
INSERT INTO automation_tasks(id,account_id,version,service_owner,payload,enabled,trigger_mode,updated_at) VALUES('task','old',1,'owner','{"max_batch":17,"protect_unread":true}',1,'manual',1);
DROP TABLE gmail_run_claims; DROP TABLE mail_oauth_folder_roles; DROP TABLE mail_oauth_states; DROP TABLE mail_oauth_accounts; DROP TABLE mail_oauth_apps;
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
		if err = database.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 15 {
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
		if err = database.DB.QueryRow("SELECT count(*) FROM mail_oauth_accounts").Scan(&count); err != nil || count != 0 {
			t.Fatal("legacy account became OAuth")
		}
		database.Close()
	}
}

func TestMailOAuthMigrationRollsBack(t *testing.T) {
	directory := t.TempDir()
	database, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.DB.Exec(`DROP TABLE gmail_run_claims;DROP TABLE mail_oauth_folder_roles;DROP TABLE mail_oauth_states;DROP TABLE mail_oauth_accounts;DROP TABLE mail_oauth_apps;DROP INDEX messages_gmail_identity;ALTER TABLE messages DROP COLUMN gmail_message_id;ALTER TABLE sync_stage DROP COLUMN gmail_message_id;CREATE TABLE messages_gmail_identity(blocker TEXT);PRAGMA user_version=14`)
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
	if err = raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('mail_oauth_accounts','mail_oauth_folder_roles','mail_oauth_apps','mail_oauth_states')").Scan(&count); err != nil || count != 0 {
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
