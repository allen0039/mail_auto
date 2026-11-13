package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

// SchemaVersion is the newest database layout this binary can open.
const SchemaVersion = 11

type Store struct {
	DB      *sql.DB
	reclaim chan struct{}
}

func Open(dir string) (*Store, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	path := filepath.Join(dir, "mail-auto.db")
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	f.Close()
	db, e := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "_txlock=immediate"}).String())
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA busy_timeout=10000", "PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON", "PRAGMA synchronous=FULL", "PRAGMA cache_size=-8192", "PRAGMA secure_delete=ON"} {
		if e = retryBusy(func() error { _, err := db.Exec(q); return err }); e != nil {
			db.Close()
			return nil, e
		}
	}
	s := &Store{DB: db, reclaim: make(chan struct{}, 1)}
	if e = retryBusy(s.migrate); e != nil {
		db.Close()
		return nil, e
	}
	if e = retryBusy(s.enableReclaim); e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}

// SQLite may return BUSY immediately for concurrent journal-mode changes even
// with busy_timeout. Retry only startup lock contention with a bounded budget.
func retryBusy(operation func() error) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := operation()
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || (sqliteErr.Code()&255 != 5 && sqliteErr.Code()&255 != 6) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
}
func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) migrate() error {
	var version int
	if e := s.DB.QueryRow("PRAGMA user_version").Scan(&version); e != nil {
		return e
	}
	if version > SchemaVersion {
		return errors.New("database schema is newer than this binary")
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	_, e = tx.Exec(`
 CREATE TABLE IF NOT EXISTS instance (id INTEGER PRIMARY KEY CHECK(id=1), key_fingerprint TEXT NOT NULL, created_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS admin (id INTEGER PRIMARY KEY CHECK(id=1), username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, must_change INTEGER NOT NULL DEFAULT 1, version INTEGER NOT NULL DEFAULT 1, temp_expires INTEGER NOT NULL, consumed INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS bootstrap (id INTEGER PRIMARY KEY CHECK(id=1), payload BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY, csrf TEXT NOT NULL, version INTEGER NOT NULL, restricted INTEGER NOT NULL, expires INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS accounts (id TEXT PRIMARY KEY, name TEXT NOT NULL, provider TEXT NOT NULL, username TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL, folders TEXT NOT NULL, secret BLOB, secret_file TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS mailboxes (account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE, name TEXT NOT NULL, uidvalidity INTEGER NOT NULL, synced_at INTEGER NOT NULL, PRIMARY KEY(account_id,name));
 CREATE TABLE IF NOT EXISTS messages (ref TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE, folder TEXT NOT NULL, uidvalidity INTEGER NOT NULL, uid INTEGER NOT NULL, subject TEXT NOT NULL, sender TEXT NOT NULL, sent_at INTEGER NOT NULL, unread INTEGER NOT NULL, size INTEGER NOT NULL, UNIQUE(account_id,folder,uidvalidity,uid));
 CREATE INDEX IF NOT EXISTS messages_search ON messages(account_id,folder,sent_at DESC);
 CREATE TABLE IF NOT EXISTS audits (id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL, outcome TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS sync_stage (token TEXT NOT NULL, ref TEXT NOT NULL, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE, folder TEXT NOT NULL, uidvalidity INTEGER NOT NULL, uid INTEGER NOT NULL, subject TEXT NOT NULL, sender TEXT NOT NULL, sent_at INTEGER NOT NULL, unread INTEGER NOT NULL, size INTEGER NOT NULL, PRIMARY KEY(token,uid));
 CREATE TABLE IF NOT EXISTS sync_jobs (account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE, state TEXT NOT NULL, folder TEXT NOT NULL DEFAULT '', folders_total INTEGER NOT NULL DEFAULT 0, folders_done INTEGER NOT NULL DEFAULT 0, messages INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, error TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS labels (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, color TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS mail_entries (id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE, detached_at INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS message_labels (message_ref TEXT NOT NULL REFERENCES mail_entries(id) ON DELETE CASCADE, label_id TEXT NOT NULL REFERENCES labels(id) ON DELETE CASCADE, PRIMARY KEY(message_ref,label_id));
 CREATE INDEX IF NOT EXISTS message_labels_label ON message_labels(label_id,message_ref);`)
	if e != nil {
		return e
	}
	// Existing installations now follow the user's all-folder policy as well.
	var current int
	if e = tx.QueryRow("PRAGMA user_version").Scan(&current); e != nil {
		return e
	}
	if current < 2 {
		if _, e = tx.Exec("UPDATE accounts SET folders='[]'"); e != nil {
			return e
		}
	}
	if e = migrateOrganizer(tx, current); e != nil {
		return e
	}
	if e = migrateAutomation(tx, current); e != nil {
		return e
	}
	if e = createSearchIndexes(tx); e != nil {
		return e
	}
	if e = migrateTasks(tx); e != nil {
		return e
	}
	if e = migrateMetadata(tx); e != nil {
		return e
	}
	if e = migrateDiagnostics(tx); e != nil {
		return e
	}
	if e = migrateAgent(tx); e != nil {
		return e
	}
	if e = migrateAgentFences(tx, current); e != nil {
		return e
	}
	if _, e = tx.Exec("PRAGMA user_version=11"); e != nil {
		return e
	}
	return tx.Commit()
}

func createSearchIndexes(tx *sql.Tx) error {
	_, e := tx.Exec(`
 CREATE INDEX IF NOT EXISTS messages_recent ON messages(sent_at DESC,ref);
 CREATE INDEX IF NOT EXISTS messages_account_recent ON messages(account_id,sent_at DESC,ref);
 CREATE INDEX IF NOT EXISTS messages_folder_recent ON messages(account_id,folder,sent_at DESC,ref);
 CREATE INDEX IF NOT EXISTS messages_account_unread_recent ON messages(account_id,unread,sent_at DESC,ref);
 CREATE INDEX IF NOT EXISTS messages_account_flagged_recent ON messages(account_id,flagged,sent_at DESC,ref);`)
	return e
}
func (s *Store) Fingerprint() (string, error) {
	var fp string
	e := s.DB.QueryRow("SELECT key_fingerprint FROM instance WHERE id=1").Scan(&fp)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	return fp, e
}
func (s *Store) Initialized() (bool, error) {
	var n int
	e := s.DB.QueryRow("SELECT count(*) FROM instance").Scan(&n)
	return n > 0, e
}
func Audit(tx interface {
	Exec(string, ...any) (sql.Result, error)
}, action, target, outcome string) error {
	_, e := tx.Exec("INSERT INTO audits(at,action,target,outcome) VALUES(?,?,?,?)", time.Now().Unix(), action, target, outcome)
	return e
}
func (s *Store) Cleanup(ctx context.Context) {
	now := time.Now().Unix()
	cutoff := time.Now().AddDate(0, 0, -90).Unix()
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM oauth_requests WHERE expires<?", now)
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM oauth_codes WHERE expires<?", now)
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM oauth_refresh WHERE expires<?", now)
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM mcp_tokens WHERE (expires<? OR revoked<>0) AND created_at<? AND id NOT IN (SELECT owner FROM operations WHERE state IN ('queued','running','needs_verification'))", now, cutoff)
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM operations WHERE updated_at<? AND state IN ('completed','partial','failed','cancelled')", cutoff)
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM operation_plans WHERE expires<? AND id NOT IN (SELECT plan_id FROM operations)", now)
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM mail_entries WHERE detached_at>0 AND detached_at<? AND id NOT IN (SELECT ref FROM operation_items)", cutoff)

	// Keep active or unresolved operations and their batches for reconciliation.
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM notification_outbox WHERE created_at<? AND state IN ('sent','failed','suppressed')", cutoff)
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM task_runs WHERE updated_at<? AND state IN ('completed','noop','observed','failed','partial','cancelled','interrupted','limited','requires_ai') AND id NOT IN (SELECT run_id FROM notification_outbox WHERE state IN ('pending','retry','sending')) AND id NOT IN (SELECT rb.run_id FROM task_run_batches rb JOIN automation_claims c ON c.batch_id=rb.batch_id JOIN operations o ON o.plan_id=c.plan_id WHERE o.state IN ('queued','running','needs_verification'))", cutoff)
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM notification_cooldown WHERE last_sent<?", cutoff)
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM automation_batches WHERE created_at<? AND id NOT IN (SELECT c.batch_id FROM automation_claims c JOIN operations o ON o.plan_id=c.plan_id WHERE o.state IN ('queued','running','needs_verification'))", cutoff)
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM automation_daily WHERE day<?", time.Now().AddDate(0, 0, -90).Format("2006-01-02"))
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM mail_rule_versions WHERE updated_at<? AND version NOT IN (SELECT version FROM mail_rules WHERE id=rule_id)", cutoff)
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM sessions WHERE expires < ?", time.Now().Unix())
	_, _ = s.DB.ExecContext(ctx, "DELETE FROM audits WHERE at < ?", time.Now().AddDate(0, 0, -90).Unix())
	s.cleanupAgent(ctx, time.Now())
}
func (s *Store) Backup(path string) error {
	abs, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	f.Close()
	// VACUUM INTO creates a consistent SQLite snapshot; an empty reserved destination is allowed.
	_, e = s.DB.Exec("VACUUM INTO ?", abs)
	if e != nil {
		os.Remove(abs)
	}
	return e
}
