package store

import "testing"

func TestSchemaFiveAutomationMigrationPreservesOldMetadata(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(`INSERT INTO accounts VALUES('a','fixture','qq','a@qq.com','imap.qq.com',993,'[]',X'010203','',1);
 INSERT INTO messages(ref,account_id,folder,uidvalidity,uid,subject,sender,sent_at,unread,size) VALUES('stable','a','INBOX',7,42,'fixture','sender',123,1,20);`)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-schema-6 database before dropping the columns that were
	// introduced by the automation metadata migration. The current version's
	// search indexes depend on those columns and would not exist in that old DB.
	if _, err = s.DB.Exec(`DROP INDEX messages_account_flagged_recent; DROP INDEX messages_account_unread_recent;`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"messages", "sync_stage"} {
		for _, col := range []string{"received_at", "flagged", "flags_known", "changed_at", "metadata_version"} {
			if _, err = s.DB.Exec("ALTER TABLE " + table + " DROP COLUMN " + col); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = s.DB.Exec("PRAGMA user_version=5"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var ref, secret string
	var flagsKnown, received, metadata, version int
	if err = s.DB.QueryRow("SELECT ref,flags_known,received_at,metadata_version FROM messages").Scan(&ref, &flagsKnown, &received, &metadata); err != nil {
		t.Fatal(err)
	}
	s.DB.QueryRow("SELECT hex(secret) FROM accounts").Scan(&secret)
	s.DB.QueryRow("PRAGMA user_version").Scan(&version)
	if ref != "stable" || secret != "010203" || flagsKnown != 0 || received != 0 || metadata != 0 || version != SchemaVersion {
		t.Fatal(ref, secret, flagsKnown, received, metadata, version)
	}
	var policies int
	s.DB.QueryRow("SELECT count(*) FROM automation_policies").Scan(&policies)
	if policies != 0 {
		t.Fatal("migration granted automation")
	}
}
