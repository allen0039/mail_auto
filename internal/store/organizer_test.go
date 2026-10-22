package store

import (
	"context"
	"testing"
)

func TestOrganizerV2MigrationAndIndependentLabelLifecycle(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DB.Exec(`INSERT INTO accounts VALUES('a','fixture','qq','a@qq.com','imap.qq.com',993,'["INBOX"]',X'010203','',1);
 INSERT INTO messages VALUES('stable','a','INBOX',7,42,'fixture','sender',123,1,20);
 DROP TRIGGER message_entry_insert; DROP TRIGGER message_entry_delete;
 DROP TABLE message_labels; DROP TABLE mail_entries; DROP TABLE labels;
 PRAGMA user_version=2;`)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var folders, secret, ref string
	var version int
	if e = s.DB.QueryRow("SELECT folders,hex(secret) FROM accounts WHERE id='a'").Scan(&folders, &secret); e != nil {
		t.Fatal(e)
	}
	if folders != `["INBOX"]` || secret != "010203" {
		t.Fatal("scope or credential changed", folders, secret)
	}
	if e = s.DB.QueryRow("SELECT ref FROM messages").Scan(&ref); e != nil || ref != "stable" {
		t.Fatal(ref, e)
	}
	s.DB.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 5 {
		t.Fatal(version)
	}
	if _, e = s.DB.Exec(`INSERT INTO labels VALUES('l','fixture','',1); INSERT INTO message_labels VALUES('stable','l'); DELETE FROM messages WHERE ref='stable';`); e != nil {
		t.Fatal(e)
	}
	var n int
	s.DB.QueryRow("SELECT count(*) FROM message_labels").Scan(&n)
	if n != 1 {
		t.Fatal("detached labels lost")
	}
	if _, e = s.DB.Exec(`INSERT INTO messages VALUES('stable','a','Trash',7,99,'fixture','sender',123,1,20)`); e != nil {
		t.Fatal(e)
	}
	var detached int
	s.DB.QueryRow("SELECT detached_at FROM mail_entries WHERE id='stable'").Scan(&detached)
	if detached != 0 {
		t.Fatal("not reattached")
	}
	s.Cleanup(context.Background())
	s.DB.QueryRow("SELECT count(*) FROM message_labels").Scan(&n)
	if n != 1 {
		t.Fatal("active labels reclaimed")
	}
	if _, e = s.DB.Exec("DELETE FROM accounts WHERE id='a'"); e != nil {
		t.Fatal(e)
	}
	s.DB.QueryRow("SELECT count(*) FROM message_labels").Scan(&n)
	if n != 0 {
		t.Fatal("account cascade failed")
	}
	var integrity string
	s.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity)
	if integrity != "ok" {
		t.Fatal(integrity)
	}
}

func TestProviderFoldersV4ToV5PreservesState(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DB.Exec(`INSERT INTO accounts VALUES('a','fixture','qq','a@qq.com','imap.qq.com',993,'[]',X'010203','',1);
 INSERT INTO messages VALUES('stable','a','INBOX',7,42,'fixture','sender',123,1,20);
 INSERT INTO folder_settings(account_id,archive_folder,trash_folder,writes_enabled,version) VALUES('a','','Trash',1,8);
 DROP TABLE mcp_settings; PRAGMA user_version=4;`)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var version, n, enabled, settingsVersion int
	var secret, trash string
	if e = s.DB.QueryRow("PRAGMA user_version").Scan(&version); e != nil || version != 5 {
		t.Fatal(version, e)
	}
	s.DB.QueryRow("SELECT count(*) FROM messages WHERE ref='stable'").Scan(&n)
	if n != 1 {
		t.Fatal("ref lost")
	}
	s.DB.QueryRow("SELECT hex(secret) FROM accounts WHERE id='a'").Scan(&secret)
	if secret != "010203" {
		t.Fatal("secret changed")
	}
	s.DB.QueryRow("SELECT trash_folder,writes_enabled,version FROM folder_settings WHERE account_id='a'").Scan(&trash, &enabled, &settingsVersion)
	if trash != "Trash" || enabled != 1 || settingsVersion != 8 {
		t.Fatal(trash, enabled, settingsVersion)
	}
	if _, e = s.DB.Exec("INSERT INTO mcp_settings VALUES(1,0,'',1)"); e != nil {
		t.Fatal(e)
	}
}
