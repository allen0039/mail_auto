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

func TestSchemaEightToNineAddsDiagnostics(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	stmts := []string{"ALTER TABLE operations DROP COLUMN cancel_source"}
	for _, column := range diagnosticColumns {
		stmts = append(stmts, "ALTER TABLE operation_items DROP COLUMN "+strings.Fields(column)[0])
	}
	stmts = append(stmts,
		"PRAGMA foreign_keys=OFF",
		"INSERT INTO operations(id,plan_id,owner,idempotency_key,state,created_at,updated_at,cancelled) VALUES('o','p','admin','k','needs_verification',1,1,1)",
		"INSERT INTO operation_items(operation_id,position,ref,state,error_code,destination,destination_validity,destination_uid) VALUES('o',0,'r','needs_verification','remote_state_unknown','TestFlight测试',4723716,1582975748)",
		"PRAGMA user_version=8")
	for _, q := range stmts {
		if _, e = s.DB.Exec(q); e != nil {
			t.Fatal(q, e)
		}
	}
	s.Close()
	if s, e = Open(dir); e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var source, code, detail, folder string
	var checked int64
	var validity, uid uint32
	var cancelled bool
	if e = s.DB.QueryRow("SELECT o.cancelled,o.cancel_source,i.error_code,i.recovery_detail,i.checked_at,i.destination,i.destination_validity,i.destination_uid FROM operations o JOIN operation_items i ON i.operation_id=o.id").Scan(&cancelled, &source, &code, &detail, &checked, &folder, &validity, &uid); e != nil {
		t.Fatal(e)
	}
	if folder != "TestFlight测试" || validity != 4723716 || uid != 1582975748 {
		t.Fatal("target UID mapping lost in migration", folder, validity, uid)
	}
	// An old cancellation has no source and is therefore treated as explicit.
	if !cancelled || source != "" || code != "remote_state_unknown" || detail != "" || checked != 0 {
		t.Fatal(cancelled, source, code, detail, checked)
	}
}
