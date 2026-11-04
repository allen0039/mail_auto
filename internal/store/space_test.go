package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLegacyDatabaseEnablesReclaimWithoutChangingRecords(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DB.Exec(`INSERT INTO accounts VALUES('a','account','qq','fixture@example.com','imap.qq.com',993,'["Archive"]',X'010203','',1); INSERT INTO messages(ref,account_id,folder,uidvalidity,uid,subject,sender,sent_at,unread,size) VALUES('stable-ref','a','Archive',7,1,'keep','fixture',1,0,50); PRAGMA auto_vacuum=NONE; VACUUM;`)
	if e != nil {
		t.Fatal(e)
	}
	var mode int
	if e = s.DB.QueryRow("PRAGMA auto_vacuum").Scan(&mode); e != nil || mode != 0 {
		t.Fatal("not a legacy fixture", mode, e)
	}
	s.Close()
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.DB.QueryRow("PRAGMA auto_vacuum").Scan(&mode); e != nil || mode != 2 {
		t.Fatal("incremental migration", mode, e)
	}
	var ref, folders string
	var secret []byte
	if e = s.DB.QueryRow("SELECT ref FROM messages").Scan(&ref); e != nil || ref != "stable-ref" {
		t.Fatal("message changed", e)
	}
	if e = s.DB.QueryRow("SELECT folders,secret FROM accounts").Scan(&folders, &secret); e != nil || folders != `["Archive"]` || len(secret) != 3 {
		t.Fatal("configuration changed", e)
	}
}

func TestReclamationReturnsDiskSpaceAndMaintainsLiveRecords(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.DB.Exec(`INSERT INTO accounts VALUES('a','account','qq','fixture@example.com','imap.qq.com',993,'[]',NULL,'',1); INSERT INTO messages(ref,account_id,folder,uidvalidity,uid,subject,sender,sent_at,unread,size) VALUES('keep','a','INBOX',1,1,'retained','fixture',1,0,20)`); e != nil {
		t.Fatal(e)
	}
	tx, e := s.DB.Begin()
	if e != nil {
		t.Fatal(e)
	}
	stmt, e := tx.Prepare("INSERT INTO messages(ref,account_id,folder,uidvalidity,uid,subject,sender,sent_at,unread,size) VALUES(?,'a','INBOX',1,?,?, 'fixture',1,0,4096)")
	if e != nil {
		t.Fatal(e)
	}
	for i := 2; i < 2202; i++ {
		if _, e = stmt.Exec(fmt.Sprint(i), i, strings.Repeat("x", 4096)); e != nil {
			t.Fatal(e)
		}
	}
	stmt.Close()
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); e != nil {
		t.Fatal(e)
	}
	before, e := os.Stat(filepath.Join(dir, "mail-auto.db"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec("DELETE FROM messages WHERE ref<>'keep'"); e != nil {
		t.Fatal(e)
	}
	result, e := s.ReclaimSpace(context.Background())
	if e != nil || result.PagesAfter >= result.PagesBefore || result.PagesBefore-result.PagesAfter > 258 {
		t.Fatal("bounded reclaim failed", result, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Maintain(ctx) }()
	defer func() { cancel(); <-done }()
	s.RequestReclaim()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var free int64
		if e = s.DB.QueryRow("PRAGMA freelist_count").Scan(&free); e != nil {
			t.Fatal(e)
		}
		if free == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background did not finish reclamation", free)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Join maintenance before measuring the final checkpointed files.
	cancel()
	<-done
	if _, e = s.ReclaimSpace(context.Background()); e != nil {
		t.Fatal(e)
	}
	after, e := os.Stat(filepath.Join(dir, "mail-auto.db"))
	if e != nil || after.Size() >= before.Size()/2 {
		t.Fatal("disk file did not shrink", before.Size(), after.Size(), e)
	}
	wal, e := os.Stat(filepath.Join(dir, "mail-auto.db-wal"))
	if e != nil || wal.Size() != 0 {
		t.Fatal("WAL not released", e)
	}
	var ref, subject, integrity string
	if e = s.DB.QueryRow("SELECT ref,subject FROM messages").Scan(&ref, &subject); e != nil || ref != "keep" || subject != "retained" {
		t.Fatal("live record lost", e)
	}
	if e = s.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); e != nil || integrity != "ok" {
		t.Fatal("integrity", e)
	}
	var secure int
	s.DB.QueryRow("PRAGMA secure_delete").Scan(&secure)
	if secure != 1 {
		t.Fatal("deleted page contents are retained")
	}
	t.Logf("database disk bytes before=%d after=%d", before.Size(), after.Size())
}
