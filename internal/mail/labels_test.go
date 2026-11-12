package mail

import (
	"context"
	"testing"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

func TestLocalLabelsAndSearchFilters(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v, err := credentials.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{snapshot: Snapshot{UIDValidity: 5, Messages: []Message{
		{UID: 1, Subject: "账单一", Sender: "billing@example.com", SentAt: time.Now().Unix(), Unread: true},
		{UID: 2, Subject: "通知", Sender: "news@example.com", SentAt: time.Now().Unix(), Unread: false},
	}}}
	s := &Service{Store: st, Vault: v, Remote: remote}
	a, err := s.Create(AccountInput{Name: "QQ", Provider: "qq", Username: "a@qq.com", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Sync(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	labels, err := s.Labels()
	if err != nil || len(labels) != 0 {
		t.Fatal("initial labels", labels, err)
	}
	label, err := s.CreateLabel("账单", "#245fca")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetMessageLabels("missing", []string{"账单"}, nil); err == nil {
		t.Fatal("missing message accepted")
	}
	rows, _, err := s.Search(Search{AccountID: a.ID, Unread: true})
	if err != nil || len(rows) != 1 {
		t.Fatal("unread search", rows, err)
	}
	if err = s.SetMessageLabels(rows[0].Ref, []string{label.Name, label.Name}, nil); err != nil {
		t.Fatal(err)
	}
	labeledRef := rows[0].Ref
	rows, total, err := s.Search(Search{Label: "账单"})
	if err != nil || total != 1 || len(rows) != 1 || len(rows[0].Labels) != 1 {
		t.Fatal("label search", rows, total, err)
	}
	rows, total, err = s.Search(Search{WithoutLabel: "账单"})
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatal("without label search", rows, total, err)
	}
	if err = s.SetMessageLabels(labeledRef, nil, []string{"账单"}); err != nil {
		t.Fatal(err)
	}
	rows, total, err = s.Search(Search{Label: "账单"})
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatal("label removal", rows, total, err)
	}
	if err = s.DeleteLabel(label.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteLabel(label.ID); err == nil {
		t.Fatal("duplicate delete accepted")
	}
}

func TestLabelValidationAndMigrationTables(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, in := range [][2]string{{"", ""}, {"bad\nname", ""}, {"ok", string(make([]byte, 33))}} {
		if _, err = (&Service{Store: st}).CreateLabel(in[0], in[1]); err == nil {
			t.Fatal("invalid label accepted")
		}
	}
	var version int
	if err = st.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != store.SchemaVersion {
		t.Fatal("schema version", version, err)
	}
}
