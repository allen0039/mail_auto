package store

import "database/sql"

func migrateOrganizer(tx *sql.Tx, version int) error {
	// Version 3 was an unreleased intermediate schema. Preserve its labels too.
	if version == 3 {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO mail_entries SELECT ref,account_id,0 FROM messages;
   ALTER TABLE message_labels RENAME TO message_labels_old;
   CREATE TABLE message_labels (message_ref TEXT NOT NULL REFERENCES mail_entries(id) ON DELETE CASCADE,label_id TEXT NOT NULL REFERENCES labels(id) ON DELETE CASCADE,PRIMARY KEY(message_ref,label_id));
   INSERT INTO message_labels SELECT * FROM message_labels_old;
   DROP TABLE message_labels_old;
   CREATE INDEX IF NOT EXISTS message_labels_label ON message_labels(label_id,message_ref);`); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`
 CREATE TABLE IF NOT EXISTS mcp_settings(id INTEGER PRIMARY KEY CHECK(id=1),enabled INTEGER NOT NULL,public_url TEXT NOT NULL,version INTEGER NOT NULL);
 INSERT OR IGNORE INTO mail_entries SELECT ref,account_id,0 FROM messages;
 CREATE TRIGGER IF NOT EXISTS message_entry_insert AFTER INSERT ON messages BEGIN
  INSERT INTO mail_entries(id,account_id,detached_at) VALUES(NEW.ref,NEW.account_id,0) ON CONFLICT(id) DO UPDATE SET detached_at=0;
 END;
 CREATE TRIGGER IF NOT EXISTS message_entry_delete AFTER DELETE ON messages BEGIN
  UPDATE mail_entries SET detached_at=unixepoch() WHERE id=OLD.ref;
 END;
 CREATE TABLE IF NOT EXISTS folder_settings(account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,archive_folder TEXT NOT NULL DEFAULT '',trash_folder TEXT NOT NULL DEFAULT '',writes_enabled INTEGER NOT NULL DEFAULT 0,version INTEGER NOT NULL DEFAULT 1);
 CREATE TABLE IF NOT EXISTS mcp_tokens(id TEXT PRIMARY KEY,name TEXT NOT NULL,hash TEXT NOT NULL UNIQUE,accounts TEXT NOT NULL,folders TEXT NOT NULL,actions TEXT NOT NULL,admin_version INTEGER NOT NULL,expires INTEGER NOT NULL,revoked INTEGER NOT NULL DEFAULT 0,last_used INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS operation_plans(id TEXT PRIMARY KEY,owner TEXT NOT NULL,payload TEXT NOT NULL,created_at INTEGER NOT NULL,expires INTEGER NOT NULL,settings_version INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS operations(id TEXT PRIMARY KEY,plan_id TEXT NOT NULL UNIQUE REFERENCES operation_plans(id),owner TEXT NOT NULL,idempotency_key TEXT NOT NULL,state TEXT NOT NULL,cancelled INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,UNIQUE(owner,idempotency_key));
 CREATE TABLE IF NOT EXISTS operation_items(operation_id TEXT NOT NULL REFERENCES operations(id) ON DELETE CASCADE,position INTEGER NOT NULL,ref TEXT NOT NULL,state TEXT NOT NULL,error_code TEXT NOT NULL DEFAULT '',before_labels TEXT NOT NULL DEFAULT '[]',destination TEXT NOT NULL DEFAULT '',destination_validity INTEGER NOT NULL DEFAULT 0,destination_uid INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(operation_id,position));
 CREATE INDEX IF NOT EXISTS operations_owner ON operations(owner,created_at DESC);
 CREATE TABLE IF NOT EXISTS oauth_clients(id TEXT PRIMARY KEY,name TEXT NOT NULL,redirects TEXT NOT NULL,created_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS oauth_requests(id TEXT PRIMARY KEY,client_id TEXT NOT NULL,redirect_uri TEXT NOT NULL,state TEXT NOT NULL,challenge TEXT NOT NULL,scope TEXT NOT NULL,expires INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS oauth_codes(hash TEXT PRIMARY KEY,client_id TEXT NOT NULL,redirect_uri TEXT NOT NULL,challenge TEXT NOT NULL,payload TEXT NOT NULL,admin_version INTEGER NOT NULL,scope TEXT NOT NULL,expires INTEGER NOT NULL,consumed INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS oauth_refresh(hash TEXT PRIMARY KEY,client_id TEXT NOT NULL,token_id TEXT NOT NULL,payload TEXT NOT NULL,admin_version INTEGER NOT NULL,scope TEXT NOT NULL,expires INTEGER NOT NULL,consumed INTEGER NOT NULL DEFAULT 0);
 `)
	return err
}
