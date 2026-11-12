package mail

import (
	"context"
	"testing"

	"mail-auto/internal/store"
)

func TestTaskIndexDoesNotRunFullSyncWhenAutomaticSyncOwnsFreshness(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const accountID = "account"
	if _, err = st.DB.Exec("INSERT INTO accounts(id,name,provider,username,host,port,folders,created_at) VALUES(?,?,?,?,?,?,?,?)", accountID, "163", "netease", "user@163.com", "imap.163.com", 993, "[]", 1); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB.Exec("INSERT INTO mailboxes(account_id,name,uidvalidity,synced_at) VALUES(?,?,?,?)", accountID, "INBOX", 1, 1); err != nil {
		t.Fatal(err)
	}

	s := &Service{Store: st}
	s.automatic = &automaticRunner{interval: 300}
	if err = s.ensureTaskIndex(context.Background(), AutomationPolicy{AccountID: accountID, SourceFolders: []string{"INBOX"}}); err != nil {
		t.Fatalf("stale automatic-owned index was accepted: %v", err)
	}
}

func TestFixedRuleTaskDeadlineAllowsFullBatchOverSlowIMAP(t *testing.T) {
	if got := taskRunTTL(Task{Engine: "rules"}); got != 1800 {
		t.Fatalf("fixed rule task TTL = %d, want 1800", got)
	}
	if got := taskRunTTL(Task{Engine: "ai"}); got != 600 {
		t.Fatalf("external AI task TTL = %d, want 600", got)
	}
}

func TestOlderTaskRecoveryCannotRewindLatestCursor(t *testing.T) {
	s, _, admin, _, task, _ := taskFixture(t, "observe", "manual", "rules")
	older, err := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "older"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	older.State = "needs_verification"
	older.BatchID = "older-batch"
	older.NextCursor = "older-cursor"
	if err = s.finishTaskRun(older); err != nil {
		t.Fatal(err)
	}
	newer, err := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "newer"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	newer.State = "completed"
	newer.BatchID = "newer-batch"
	newer.NextCursor = "newer-cursor"
	if err = s.finishTaskRun(newer); err != nil {
		t.Fatal(err)
	}
	older.State = "completed"
	if err = s.finishTaskRun(older); err != nil {
		t.Fatal(err)
	}
	current, err := s.loadTask(task.ID)
	if err != nil || current.Cursor != newer.NextCursor {
		t.Fatal("older reconciliation rewound task cursor", current.Cursor, err)
	}
}
