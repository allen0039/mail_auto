package store

import "database/sql"

// These records deliberately have no foreign keys to expiring operations,
// claims or mail entries. They remain effective after cleanup and resync.
func migrateManualResolution(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS manual_resolutions(operation_id TEXT PRIMARY KEY,account_id TEXT NOT NULL,payload TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS manual_keep_blocks(account_id TEXT NOT NULL,ref TEXT NOT NULL,folder TEXT NOT NULL,uidvalidity INTEGER NOT NULL,uid INTEGER NOT NULL,message_id TEXT NOT NULL,resolution_id TEXT NOT NULL,PRIMARY KEY(account_id,ref));
CREATE INDEX IF NOT EXISTS manual_keep_message_id ON manual_keep_blocks(account_id,message_id);
CREATE INDEX IF NOT EXISTS manual_keep_location ON manual_keep_blocks(account_id,folder,uidvalidity,uid);
`)
	return err
}
