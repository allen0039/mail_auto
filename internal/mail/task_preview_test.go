package mail

import (
	"fmt"
	"testing"
	"time"
)

// 600 INBOX messages, newest first; only the 26th and the 501st are old enough
// for the fixture rule (received_days >= 30).
func previewFixture(t *testing.T) (*Service, *automationRemote, Access, Task, []string) {
	t.Helper()
	s, remote, admin, _, task, message := taskFixture(t, "execute", "manual", "rules")
	now := time.Now().Unix()
	mustExec(t, s, "DELETE FROM messages")
	matches := []string{}
	for i := 1; i <= 600; i++ {
		ref := fmt.Sprintf("p%03d", i)
		received := now - 86400 // too new
		if i == 26 || i == 501 {
			received = now - 60*86400
			matches = append(matches, ref)
		}
		mustExec(t, s, "INSERT INTO messages(ref,account_id,folder,uidvalidity,uid,subject,sender,sent_at,unread,size,received_at,flagged,flags_known,changed_at,metadata_version) VALUES(?,?,?,?,?,?,?,?,0,100,?,0,1,?,2)",
			ref, message.AccountID, "INBOX", 7, i, ref, "a@example.com", now-int64(i)*60, received, now-3600)
	}
	return s, remote, admin, task, matches
}

func TestTaskPreviewPagesAndFindsSparseMatches(t *testing.T) {
	s, remote, admin, task, matches := previewFixture(t)
	// Plain paging: page 1 has no match; page 2 contains the 26th message.
	p1, err := s.PreviewTask(admin, task.ID, "", false)
	if err != nil || len(p1.Items) != 25 || p1.Total != 600 || p1.Counts["matched"] != 0 || p1.NextCursor == "" || p1.Scanned != 25 || p1.MaxBatch != 20 {
		t.Fatalf("page 1: items=%d total=%d counts=%v scanned=%d max=%d", len(p1.Items), p1.Total, p1.Counts, p1.Scanned, p1.MaxBatch)
	}
	p2, err := s.PreviewTask(admin, task.ID, p1.NextCursor, false)
	if err != nil || p2.Counts["matched"] != 1 || p2.Items[0].Message.Ref != matches[0] || p2.Items[0].Status != "matched" {
		t.Fatalf("page 2: %v %v", p2.Counts, err)
	}
	// Only matched: the first call scans up to 500 and finds the 26th; the
	// continuation finds the 501st and then reports the end of the folder.
	m1, err := s.PreviewTask(admin, task.ID, "", true)
	if err != nil || len(m1.Items) != 1 || m1.Items[0].Message.Ref != matches[0] || m1.Scanned != 500 || m1.NextCursor == "" {
		t.Fatalf("matched 1: items=%d scanned=%d next=%q err=%v", len(m1.Items), m1.Scanned, m1.NextCursor, err)
	}
	m2, err := s.PreviewTask(admin, task.ID, m1.NextCursor, true)
	if err != nil || len(m2.Items) != 1 || m2.Items[0].Message.Ref != matches[1] || m2.Scanned != 100 || m2.NextCursor != "" {
		t.Fatalf("matched 2: items=%d scanned=%d next=%q err=%v", len(m2.Items), m2.Scanned, m2.NextCursor, err)
	}
	if m1.Counts["unmatched"]+m1.Counts["matched"] != 500 {
		t.Fatal("counts must cover exactly what was scanned", m1.Counts)
	}
	// Preview reads only the index: no remote writes and no flag changes.
	if remote.moves != 0 || remote.finishCallCount != 0 {
		t.Fatal("preview touched the mailbox")
	}
	var unread int
	if err = s.Store.DB.QueryRow("SELECT count(*) FROM messages WHERE unread<>0").Scan(&unread); err != nil || unread != 0 {
		t.Fatal("preview changed flags", unread, err)
	}
	// A cursor from another scope is rejected rather than misapplied.
	if _, err = s.PreviewTask(admin, task.ID, "bogus", true); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}
