package store

import (
	"database/sql"
	"strings"
)

func migrateAutomation(tx *sql.Tx, version int) error {
	if version < 6 {
		for _, table := range []string{"messages", "sync_stage"} {
			rows, err := tx.Query("PRAGMA table_info(" + table + ")")
			if err != nil {
				return err
			}
			columns := map[string]bool{}
			for rows.Next() {
				var cid, notnull, pk int
				var name, kind string
				var value any
				if err = rows.Scan(&cid, &name, &kind, &notnull, &value, &pk); err != nil {
					break
				}
				columns[name] = true
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
			if err != nil {
				return err
			}
			for _, field := range []string{"received_at INTEGER NOT NULL DEFAULT 0", "flagged INTEGER NOT NULL DEFAULT 0", "flags_known INTEGER NOT NULL DEFAULT 0", "changed_at INTEGER NOT NULL DEFAULT 0", "metadata_version INTEGER NOT NULL DEFAULT 0"} {
				if columns[strings.Fields(field)[0]] {
					continue
				}
				if _, err := tx.Exec("ALTER TABLE " + table + " ADD COLUMN " + field); err != nil {
					return err
				}
			}
		}
	}
	_, err := tx.Exec(`
 CREATE TABLE IF NOT EXISTS mail_rules(id TEXT PRIMARY KEY,account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,version INTEGER NOT NULL,payload TEXT NOT NULL,updated_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS mail_rules_account ON mail_rules(account_id);
 CREATE TABLE IF NOT EXISTS mail_rule_versions(rule_id TEXT NOT NULL REFERENCES mail_rules(id) ON DELETE CASCADE,version INTEGER NOT NULL,payload TEXT NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(rule_id,version));
 CREATE TABLE IF NOT EXISTS automation_policies(account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,version INTEGER NOT NULL,payload TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS automation_batches(id TEXT PRIMARY KEY,account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,owner TEXT NOT NULL,run_key TEXT NOT NULL,payload TEXT NOT NULL,state TEXT NOT NULL,created_at INTEGER NOT NULL,expires INTEGER NOT NULL,UNIQUE(owner,run_key));
 CREATE INDEX IF NOT EXISTS automation_batches_account ON automation_batches(account_id,created_at DESC);
 CREATE TABLE IF NOT EXISTS automation_claims(account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,ref TEXT NOT NULL,fingerprint TEXT NOT NULL,batch_id TEXT NOT NULL REFERENCES automation_batches(id) ON DELETE CASCADE,state TEXT NOT NULL,expires INTEGER NOT NULL,plan_id TEXT NOT NULL DEFAULT '',PRIMARY KEY(account_id,ref));
 CREATE TABLE IF NOT EXISTS automation_daily(account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,day TEXT NOT NULL,count INTEGER NOT NULL,claimed INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(account_id,day));
 `)
	return err
}
