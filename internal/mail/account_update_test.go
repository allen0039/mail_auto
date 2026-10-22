package mail

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

func updateService(t *testing.T) (*Service, Account, AccountInput) {
	t.Helper()
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	v, e := credentials.Open(t.TempDir(), true)
	if e != nil {
		t.Fatal(e)
	}
	s := &Service{Store: st, Vault: v, SecretDir: t.TempDir(), Remote: &fakeRemote{folders: []string{"INBOX", "Archive"}, snapshot: Snapshot{UIDValidity: 7, Messages: []Message{{UID: 1, Subject: "existing", SentAt: 1}}}}}
	in := AccountInput{Name: "original", Provider: "qq", Username: "a@qq.com", Password: "original-credential"}
	a, e := s.Create(in)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Sync(context.Background(), a.ID); e != nil {
		t.Fatal(e)
	}
	in.Password = ""
	return s, a, in
}

func TestUpdateAccountCredentialsAndIdentity(t *testing.T) {
	s, a, in := updateService(t)
	before, _, _ := s.Search(Search{})
	in.Name = "renamed"
	updated, e := s.Update(a.ID, in)
	if e != nil || updated.ID != a.ID || updated.CreatedAt != a.CreatedAt || updated.Name != "renamed" || !bytes.Equal(updated.Secret, a.Secret) {
		t.Fatal("rename failed or credential replaced", e)
	}
	if pw, e := s.password(updated); e != nil || pw != "original-credential" {
		t.Fatal("existing credential lost", e)
	}
	after, total, _ := s.Search(Search{})
	if total != 2 || after[0].Ref != before[0].Ref {
		t.Fatal("rename discarded index")
	}
	in.Password = "replacement-credential"
	updated, e = s.Update(a.ID, in)
	if e != nil {
		t.Fatal(e)
	}
	if pw, e := s.password(updated); e != nil || pw != in.Password {
		t.Fatal("credential not replaced", e)
	}
	if bytes.Contains(updated.Secret, []byte(in.Password)) {
		t.Fatal("plaintext credential stored")
	}
	after, total, _ = s.Search(Search{})
	if total != 2 || after[0].Ref != before[0].Ref {
		t.Fatal("credential rotation discarded index")
	}
	in.Username = "different@qq.com"
	in.Password = ""
	updated, e = s.Update(a.ID, in)
	if e != nil || updated.Username != in.Username {
		t.Fatal(e)
	}
	if _, total, e = s.Search(Search{}); e != nil || total != 0 {
		t.Fatal("old identity index retained", e)
	}
	if boxes, e := s.Mailboxes(a.ID); e != nil || len(boxes) != 0 {
		t.Fatal("old mailbox state retained", e)
	}
	if _, e = s.Read(context.Background(), before[0].Ref); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("old reference readable", e)
	}
	var audits, exposed int
	if e = s.Store.DB.QueryRow("SELECT count(*) FROM audits WHERE action='account.update' AND target=?", a.ID).Scan(&audits); e != nil || audits != 3 {
		t.Fatal("update audit missing", e)
	}
	if e = s.Store.DB.QueryRow("SELECT count(*) FROM audits WHERE target LIKE '%credential%' OR outcome LIKE '%credential%'").Scan(&exposed); e != nil || exposed != 0 {
		t.Fatal("audit exposed secret", e)
	}
}

func TestUpdateAccountFolderScope(t *testing.T) {
	s, a, in := updateService(t)
	before, _, _ := s.Search(Search{Folder: "INBOX"})
	in.Folders = []string{"INBOX"}
	if _, e := s.Update(a.ID, in); e != nil {
		t.Fatal(e)
	}
	after, total, _ := s.Search(Search{})
	if total != 1 || after[0].Ref != before[0].Ref {
		t.Fatal("allowed index not preserved")
	}
	boxes, e := s.Mailboxes(a.ID)
	if e != nil || len(boxes) != 1 || boxes[0].Name != "INBOX" {
		t.Fatal("disallowed mailbox retained", e)
	}
	in.Folders = nil
	if _, e = s.Update(a.ID, in); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Sync(context.Background(), a.ID); e != nil {
		t.Fatal(e)
	}
	if _, total, e = s.Search(Search{}); e != nil || total != 2 {
		t.Fatal("all-folder scope not restored", e)
	}
}

func TestUpdateAccountExternalAndValidation(t *testing.T) {
	s, a, in := updateService(t)
	if e := os.WriteFile(filepath.Join(s.SecretDir, "mail_secret"), []byte("external-credential\n"), 0600); e != nil {
		t.Fatal(e)
	}
	in.SecretFile = "mail_secret"
	updated, e := s.Update(a.ID, in)
	if e != nil || updated.CredentialSource != "external" || len(updated.Secret) != 0 {
		t.Fatal("switch to external failed", e)
	}
	if pw, e := s.password(updated); e != nil || pw != "external-credential" {
		t.Fatal(e)
	}
	in.SecretFile = ""
	updated, e = s.Update(a.ID, in)
	if e != nil || updated.SecretFile != "mail_secret" {
		t.Fatal("blank credential failed to retain external", e)
	}
	in.Password = "new-encrypted-credential"
	updated, e = s.Update(a.ID, in)
	if e != nil || updated.SecretFile != "" || updated.CredentialSource != "encrypted" {
		t.Fatal("switch to encrypted failed", e)
	}
	for _, change := range []func(*AccountInput){
		func(a *AccountInput) { a.Host = "127.0.0.1" },
		func(a *AccountInput) { a.Port = -1 },
		func(a *AccountInput) { a.Port = 65536 },
		func(a *AccountInput) { a.SecretFile = "mail_secret" },
		func(a *AccountInput) { a.Folders = []string{"INBOX", "INBOX"} },
	} {
		bad := in
		change(&bad)
		if _, e = s.Update(a.ID, bad); !errors.Is(e, ErrAccountInput) {
			t.Fatal("invalid update accepted", e)
		}
	}
	in.Password, in.SecretFile = "", "missing_secret"
	if _, e = s.Update(a.ID, in); e == nil {
		t.Fatal("missing secret accepted")
	}
	unchanged, e := s.Account(a.ID)
	if e != nil || !bytes.Equal(unchanged.Secret, updated.Secret) || unchanged.Host != updated.Host {
		t.Fatal("failed update mutated configuration", e)
	}
	in.SecretFile = ""
	if _, e = s.Update("missing-account", in); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("missing account updated", e)
	}
	release, e := s.acquire(a.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Update(a.ID, in); !errors.Is(e, ErrBusy) {
		t.Fatal("active account changed", e)
	}
	if _, e = s.StartSync(a.ID); !errors.Is(e, ErrBusy) {
		t.Fatal("sync started during config edit", e)
	}
	release()
	s.mu.Lock()
	s.pending = map[string]bool{a.ID: true}
	s.mu.Unlock()
	if _, e = s.Update(a.ID, in); !errors.Is(e, ErrBusy) {
		t.Fatal("queued account changed", e)
	}
	if e = s.Delete(a.ID); !errors.Is(e, ErrBusy) {
		t.Fatal("queued account deleted", e)
	}
}

// Port changes must reach the connection layer, not just the saved card.
type portRemote struct {
	fakeRemote
	lastPort int
}

func (r *portRemote) Folders(_ context.Context, a Account, _ string) ([]string, error) {
	r.lastPort = a.Port
	return []string{"INBOX"}, nil
}
func TestCustomTLSPort(t *testing.T) {
	for _, port := range []int{0, 1, 143, 993, 1993, 65535, -1, 65536} {
		in := AccountInput{Name: "port", Provider: "qq", Username: "a@qq.com", Password: "fake", Port: port}
		e := ValidateAccount(&in)
		if port < 0 || port > 65535 {
			if e == nil {
				t.Fatalf("invalid port accepted: %d", port)
			}
		} else if e != nil || (port == 0 && in.Port != 993) || (port != 0 && in.Port != port) {
			t.Fatalf("valid/default port failed: %d: %v", port, e)
		}
	}
	s, _, in := updateService(t)
	r := &portRemote{}
	s.Remote = r
	in.Password, in.Port = "custom-port-secret", 1993
	a, e := s.Create(in)
	if e != nil || a.Port != 1993 {
		t.Fatal("create did not save custom port", e)
	}
	if _, e = s.Diagnose(context.Background(), a.ID); e != nil || r.lastPort != 1993 {
		t.Fatal("diagnosis ignored custom port", e)
	}
	in.Password, in.Port = "", 2993
	a, e = s.Update(a.ID, in)
	if e != nil || a.Port != 2993 {
		t.Fatal("update did not save custom port", e)
	}
	if pw, e := s.password(a); e != nil || pw != "custom-port-secret" {
		t.Fatal("port edit changed credential", e)
	}
	if _, e = s.Diagnose(context.Background(), a.ID); e != nil || r.lastPort != 2993 {
		t.Fatal("diagnosis ignored edited port", e)
	}
}
