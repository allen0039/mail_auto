package store

import "testing"

func TestManualResolutionMigrationAndRetention(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(`INSERT INTO instance VALUES(1,'same-vault',1); DROP TABLE manual_keep_blocks; DROP TABLE manual_resolutions; PRAGMA user_version=13`)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var schema int
	if err = s.DB.QueryRow("PRAGMA user_version").Scan(&schema); err != nil || schema != 14 {
		t.Fatal(schema, err)
	}
	var fp string
	s.DB.QueryRow("SELECT key_fingerprint FROM instance").Scan(&fp)
	if fp != "same-vault" {
		t.Fatal(fp)
	}
	// No FK to operations or mail_entries: both can expire independently.
	_, err = s.DB.Exec(`INSERT INTO manual_resolutions VALUES('old-op','a','{"outcome":"keep_both"}',1); INSERT INTO manual_keep_blocks VALUES('a','old-ref','INBOX',1,42,'<same@test>','old-op')`)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	s.DB.QueryRow("SELECT count(*) FROM manual_keep_blocks").Scan(&n)
	if n != 1 {
		t.Fatal("restart lost keep evidence", n)
	}
}
