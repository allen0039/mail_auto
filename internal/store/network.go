package store

import "database/sql"

func migrateNetwork(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS network_settings (id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL, payload TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS proxy_profiles (id TEXT PRIMARY KEY, payload TEXT NOT NULL, secret BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS account_network (account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE, mode TEXT NOT NULL, proxy_id TEXT NOT NULL DEFAULT '');
`)
	return err
}
