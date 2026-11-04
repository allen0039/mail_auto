package store

import (
	"testing"
)

func TestSchemaSixTasksMigrationPreservesExistingPolicy(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DB.Exec(`INSERT INTO accounts VALUES('account','fixture','qq','fixture@qq.com','imap.qq.com',993,'[]',X'010203','',1); INSERT INTO automation_policies VALUES('account',3,'existing-policy'); DROP TABLE task_run_batches; DROP TABLE notification_outbox; DROP TABLE notification_cooldown; DROP TABLE task_runs; DROP TABLE automation_tasks; DROP TABLE notification_channel; PRAGMA user_version=6;`)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var version, count int
	var payload, secret string
	if e = s.DB.QueryRow("PRAGMA user_version").Scan(&version); e != nil || version != 7 {
		t.Fatal(version, e)
	}
	if e = s.DB.QueryRow("SELECT payload FROM automation_policies WHERE account_id='account'").Scan(&payload); e != nil || payload != "existing-policy" {
		t.Fatal(payload, e)
	}
	if e = s.DB.QueryRow("SELECT hex(secret) FROM accounts WHERE id='account'").Scan(&secret); e != nil || secret != "010203" {
		t.Fatal(secret, e)
	}
	for _, table := range []string{"automation_tasks", "task_runs", "notification_channel", "notification_outbox"} {
		if e = s.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); e != nil || count != 0 {
			t.Fatal(table, count, e)
		}
	}
}
