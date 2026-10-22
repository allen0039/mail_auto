package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConsistentBackupAndSchemaGuard(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.DB.Exec("INSERT INTO instance VALUES(1,'fingerprint',1)"); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "backup.db")
	if e = s.Backup(path); e != nil {
		t.Fatal(e)
	}
	if e = s.Backup(path); e == nil {
		t.Fatal("overwrote backup")
	}
	restore := t.TempDir()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(restore, "mail-auto.db"), b, 0600)
	restored, e := Open(restore)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	fp, e := restored.Fingerprint()
	if e != nil || fp != "fingerprint" {
		t.Fatal("backup lost committed data", e)
	}
	if _, e = restored.DB.Exec("PRAGMA user_version=99"); e != nil {
		t.Fatal(e)
	}
	restored.Close()
	if next, e := Open(restore); e == nil {
		next.Close()
		t.Fatal("newer schema accepted")
	}
}

func TestUpgradePreservesHistoryAndExpandsFolders(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DB.Exec(`INSERT INTO accounts VALUES('a','account','qq','a@qq.com','imap.qq.com',993,'["INBOX"]',NULL,'',1);
 INSERT INTO messages VALUES('ref','a','INBOX',1,1,'old','sender',946684800,1,20);
 PRAGMA user_version=1;`)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var folders string
	if e = s.DB.QueryRow("SELECT folders FROM accounts WHERE id='a'").Scan(&folders); e != nil {
		t.Fatal(e)
	}
	var selected []string
	if e = json.Unmarshal([]byte(folders), &selected); e != nil || len(selected) != 0 {
		t.Fatal("scope migration", folders, e)
	}
	s.Cleanup(context.Background())
	var count int
	if e = s.DB.QueryRow("SELECT count(*) FROM messages WHERE ref='ref'").Scan(&count); e != nil || count != 1 {
		t.Fatal("historical metadata deleted", e)
	}
	// Maintenance reopen must not reset a scope explicitly configured after upgrade.
	if _, e = s.DB.Exec(`UPDATE accounts SET folders='["Archive"]' WHERE id='a'`); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.DB.QueryRow("SELECT folders FROM accounts WHERE id='a'").Scan(&folders); e != nil || folders != `["Archive"]` {
		t.Fatal("reopen changed scope", folders, e)
	}
}
