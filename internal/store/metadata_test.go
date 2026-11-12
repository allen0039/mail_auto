package store

import (
	"strings"
	"testing"
)

// A schema 7 database gains the metadata columns without losing rows or refs.
func TestSchemaSevenToEightAddsMetadataColumns(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	stmts := []string{
		"INSERT INTO accounts VALUES('a','account','qq','fixture@example.com','imap.qq.com',993,'[]',NULL,'',1)",
		"INSERT INTO messages(ref,account_id,folder,uidvalidity,uid,subject,sender,sent_at,unread,size,metadata_version) VALUES('keep','a','INBOX',7,42,'subject','s@example.com',1,1,20,1)",
		"DROP INDEX messages_message_id",
	}
	for _, table := range []string{"messages", "sync_stage"} {
		for _, column := range metadataColumns {
			stmts = append(stmts, "ALTER TABLE "+table+" DROP COLUMN "+strings.Fields(column)[0])
		}
	}
	stmts = append(stmts, "PRAGMA user_version=7")
	for _, q := range stmts {
		if _, e = s.DB.Exec(q); e != nil {
			t.Fatal(q, e)
		}
	}
	s.Close()

	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	var version, metadata int
	var ref, recipients, listID string
	var attachment bool
	if e = s.DB.QueryRow("PRAGMA user_version").Scan(&version); e != nil || version != SchemaVersion {
		t.Fatal(version, e)
	}
	if e = s.DB.QueryRow("SELECT ref,metadata_version,recipients,list_id,has_attachment FROM messages").Scan(&ref, &metadata, &recipients, &listID, &attachment); e != nil {
		t.Fatal(e)
	}
	if ref != "keep" || metadata != 1 || recipients != "" || listID != "" || attachment {
		t.Fatal(ref, metadata, recipients, listID, attachment)
	}
	for _, table := range []string{"messages", "sync_stage"} {
		tx, e := s.DB.Begin()
		if e != nil {
			t.Fatal(e)
		}
		columns, e := tableColumns(tx, table)
		tx.Rollback()
		if e != nil {
			t.Fatal(e)
		}
		for _, column := range metadataColumns {
			if !columns[strings.Fields(column)[0]] {
				t.Fatal(table, "missing", column)
			}
		}
	}
	// Re-opening a current database is idempotent.
	s.Close()
	if s, e = Open(dir); e != nil {
		t.Fatal(e)
	}
	s.Close()
}

func TestNewerSchemaIsRejected(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec("PRAGMA user_version=99"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	if s, e = Open(dir); e == nil {
		s.Close()
		t.Fatal("newer schema opened")
	}
}
