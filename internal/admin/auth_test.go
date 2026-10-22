package admin

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

func newAuth(t *testing.T) (*Auth, string, string) {
	t.Helper()
	data := t.TempDir()
	keys := t.TempDir()
	st, e := store.Open(data)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	v, e := credentials.Open(keys, true)
	if e != nil {
		t.Fatal(e)
	}
	a := &Auth{Store: st, Vault: v, TTL: time.Hour}
	if e = a.Initialize(); e != nil {
		t.Fatal(e)
	}
	return a, data, keys
}

func formalAuth(t *testing.T) (*Auth, Session) {
	t.Helper()
	a, _, _ := newAuth(t)
	b, e := a.Show()
	if e != nil {
		t.Fatal(e)
	}
	s, e := a.Login(b.Username, b.Password)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Setup(s, "owner", "OriginalPass123"); e != nil {
		t.Fatal(e)
	}
	s, e = a.Login("owner", "OriginalPass123")
	if e != nil {
		t.Fatal(e)
	}
	return a, s
}

func TestChangeCredentials(t *testing.T) {
	a, session := formalAuth(t)
	for _, tc := range []struct {
		name, username, current, password string
		want                              error
	}{
		{"wrong current password", "renamed", "IncorrectPass123", "", ErrCurrentPassword},
		{"missing current password", "renamed", "", "", ErrCurrentPassword},
		{"invalid username", "bad name", "OriginalPass123", "", ErrCredentialsInput},
		{"short password", "owner", "OriginalPass123", "Abcdef12345", ErrCredentialsInput},
		{"unchanged", "owner", "OriginalPass123", "", ErrCredentialsInput},
		{"reused password", "owner", "OriginalPass123", "OriginalPass123", ErrCredentialsInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if e := a.ChangeCredentials(session, tc.username, tc.current, tc.password); !errors.Is(e, tc.want) {
				t.Fatalf("got %v, want %v", e, tc.want)
			}
			if _, e := a.Session(session.Token); e != nil {
				t.Fatal("failed change revoked session", e)
			}
		})
	}
	username, password := "owner", "OriginalPass123"
	for _, tc := range []struct{ name, username, password string }{
		{"username only", "renamed", ""},
		{"password only", "renamed", "Abcdef123456"},
		{"both", "final_owner", "FinalPassword123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other, e := a.Login(username, password)
			if e != nil {
				t.Fatal(e)
			}
			if e = a.ChangeCredentials(session, tc.username, password, tc.password); e != nil {
				t.Fatal(e)
			}
			for _, s := range []Session{session, other} {
				if _, e = a.Session(s.Token); e == nil {
					t.Fatal("old session survived credentials update")
				}
			}
			if _, e = a.Login(username, password); !errors.Is(e, ErrAuth) {
				t.Fatal("old credentials accepted", e)
			}
			if e = a.ChangeCredentials(session, "stale", password, "AnotherPass123"); !errors.Is(e, ErrSession) {
				t.Fatal("stale session accepted", e)
			}
			username = tc.username
			if tc.password != "" {
				password = tc.password
			}
			session, e = a.Login(username, password)
			if e != nil {
				t.Fatal("new credentials failed", e)
			}
		})
	}
	var hash string
	if e := a.Store.DB.QueryRow("SELECT password_hash FROM admin WHERE id=1").Scan(&hash); e != nil || !credentials.Verify(hash, password) || hash == password {
		t.Fatal("updated password was not hashed", e)
	}
	var count int
	if e := a.Store.DB.QueryRow("SELECT count(*) FROM audits WHERE action='admin.credentials' AND outcome='ok'").Scan(&count); e != nil || count != 3 {
		t.Fatal("missing successful audit events", count, e)
	}
	if e := a.Logout(session); e != nil {
		t.Fatal(e)
	}
	if e := a.ChangeCredentials(session, "logged_out", password, ""); !errors.Is(e, ErrSession) {
		t.Fatal("logged-out session accepted", e)
	}
}

func TestConcurrentCredentialChanges(t *testing.T) {
	a, s := formalAuth(t)
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		go func() {
			<-start
			errs <- a.ChangeCredentials(s, name, "OriginalPass123", "")
		}()
	}
	close(start)
	success := 0
	for range 2 {
		e := <-errs
		if e == nil {
			success++
		} else if !errors.Is(e, ErrSession) {
			t.Fatal(e)
		}
	}
	if success != 1 {
		t.Fatalf("expected exactly one change, got %d", success)
	}
}
func TestBootstrapLifecycle(t *testing.T) {
	a, data, keys := newAuth(t)
	b, e := a.Show()
	if e != nil {
		t.Fatal(e)
	}
	if len(b.Password) != 32 {
		t.Fatal("temporary password entropy format")
	}
	if e = a.Initialize(); e != nil {
		t.Fatal(e)
	}
	again, e := a.Show()
	if e != nil || again.Username != b.Username || again.Password != b.Password {
		t.Fatal("restart changed credentials")
	}
	st, e := store.Open(data)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	v, e := credentials.Open(keys, false)
	if e != nil {
		t.Fatal(e)
	}
	second := &Auth{Store: st, Vault: v, TTL: time.Hour}
	if e = second.Initialize(); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Login(b.Username, "wrong"); !errors.Is(e, ErrAuth) {
		t.Fatal("bad password accepted")
	}
	session, e := a.Login(b.Username, b.Password)
	if e != nil || !session.Restricted {
		t.Fatalf("restricted login: %v", e)
	}
	if _, e = a.Show(); e == nil {
		t.Fatal("consumed credential still recoverable")
	}
	if _, e = second.Login(b.Username, b.Password); !errors.Is(e, ErrAuth) {
		t.Fatal("temporary password reused")
	}
	if e = a.Setup(session, b.Username, "this-is-a-new-password"); e == nil {
		t.Fatal("same username accepted")
	}
	if e = a.Setup(session, "owner", b.Password); e == nil {
		t.Fatal("same password accepted")
	}
	if e = a.Setup(session, "owner", "Abcdef12345"); e == nil {
		t.Fatal("11-character password accepted")
	}
	if e = a.Setup(session, "owner", "Abcdef123456"); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Session(session.Token); e == nil {
		t.Fatal("setup session survived")
	}
	if _, e = a.Login(b.Username, b.Password); e == nil {
		t.Fatal("bootstrap still valid")
	}
	normal, e := a.Login("owner", "Abcdef123456")
	if e != nil || normal.Restricted {
		t.Fatal("formal login failed", e)
	}
	if e = second.Recover(); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Session(normal.Token); e == nil {
		t.Fatal("recovery did not revoke session")
	}
	if _, e = a.Login("owner", "Abcdef123456"); e == nil {
		t.Fatal("old password valid after recovery")
	}
	recovered, e := a.Show()
	if e != nil || recovered.Password == b.Password {
		t.Fatal("recovery did not rotate temporary credentials")
	}
	if e = a.Store.Backup(filepath.Join(t.TempDir(), "backup.db")); e != nil {
		t.Fatal("consistent backup failed", e)
	}
}
func TestExpiredAndMissingAdministrator(t *testing.T) {
	a, _, _ := newAuth(t)
	b, _ := a.Show()
	if _, e := a.Store.DB.Exec("UPDATE admin SET temp_expires=?", time.Now().Add(-time.Hour).Unix()); e != nil {
		t.Fatal(e)
	}
	if _, e := a.Login(b.Username, b.Password); e == nil {
		t.Fatal("expired login")
	}
	if _, e := a.Show(); e == nil {
		t.Fatal("expired show")
	}
	if _, e := a.Store.DB.Exec("DELETE FROM admin"); e != nil {
		t.Fatal(e)
	}
	if e := a.Initialize(); e == nil {
		t.Fatal("missing admin silently reinitialized")
	}
}
func TestKeyMismatchAndConcurrentInitialization(t *testing.T) {
	data := t.TempDir()
	keydir := t.TempDir()
	const n = 4
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			st, e := store.Open(data)
			if e != nil {
				errs <- e
				return
			}
			defer st.Close()
			v, e := credentials.Open(keydir, true)
			if e != nil {
				errs <- e
				return
			}
			a := &Auth{Store: st, Vault: v, TTL: time.Hour}
			errs <- a.Initialize()
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	st, e := store.Open(data)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	var count int
	if e = st.DB.QueryRow("SELECT count(*) FROM admin").Scan(&count); e != nil || count != 1 {
		t.Fatal("admin count", count, e)
	}
	other, e := credentials.Open(t.TempDir(), true)
	if e != nil {
		t.Fatal(e)
	}
	if e = (&Auth{Store: st, Vault: other}).Initialize(); e == nil {
		t.Fatal("wrong key accepted")
	}
	if _, e = credentials.Open(t.TempDir(), false); e == nil {
		t.Fatal("missing key silently recreated")
	}
	if _, e = st.DB.Exec("DELETE FROM bootstrap"); e != nil {
		t.Fatal(e)
	}
	var raw []byte
	if e = st.DB.QueryRow("SELECT payload FROM bootstrap").Scan(&raw); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal(e)
	}
}

func TestDatabaseAndKeyRestore(t *testing.T) {
	a, _, keys := newAuth(t)
	initial, e := a.Show()
	if e != nil {
		t.Fatal(e)
	}
	restore := t.TempDir()
	if e = a.Store.Backup(filepath.Join(restore, "mail-auto.db")); e != nil {
		t.Fatal(e)
	}
	key, e := os.ReadFile(filepath.Join(keys, "master.key"))
	if e != nil {
		t.Fatal(e)
	}
	restoredKeys := t.TempDir()
	if e = os.WriteFile(filepath.Join(restoredKeys, "master.key"), key, 0600); e != nil {
		t.Fatal(e)
	}
	st, e := store.Open(restore)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	v, e := credentials.Open(restoredKeys, false)
	if e != nil {
		t.Fatal(e)
	}
	restored := &Auth{Store: st, Vault: v, TTL: time.Hour}
	if e = restored.Initialize(); e != nil {
		t.Fatal(e)
	}
	b, e := restored.Show()
	if e != nil || b.Username != initial.Username || b.Password != initial.Password {
		t.Fatal("joint restore failed", e)
	}
	if _, e = restored.Login(b.Username, b.Password); e != nil {
		t.Fatal("restored login failed", e)
	}
}
