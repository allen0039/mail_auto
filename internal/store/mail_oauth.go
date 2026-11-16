package store

import "database/sql"

func migrateMailOAuth(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS mail_oauth_apps (
 provider TEXT PRIMARY KEY, client_id TEXT NOT NULL, secret BLOB NOT NULL,
 authority TEXT NOT NULL, origin TEXT NOT NULL, version INTEGER NOT NULL,
 secret_expires INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS mail_oauth_accounts (
 account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
 provider TEXT NOT NULL, subject TEXT NOT NULL DEFAULT '', tenant TEXT NOT NULL DEFAULT '',
 login TEXT NOT NULL, scope TEXT NOT NULL DEFAULT '', refresh BLOB,
 generation INTEGER NOT NULL DEFAULT 1, config_version INTEGER NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending', error_code TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS mail_oauth_states (
 state_hash TEXT PRIMARY KEY, session_hash TEXT NOT NULL REFERENCES sessions(token_hash) ON DELETE CASCADE,
 provider TEXT NOT NULL, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 generation INTEGER NOT NULL, config_version INTEGER NOT NULL, network_revision TEXT NOT NULL,
 verifier BLOB NOT NULL, nonce TEXT NOT NULL, expires INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS mail_oauth_states_account ON mail_oauth_states(account_id);
CREATE TABLE IF NOT EXISTS mail_oauth_folder_roles (
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 name TEXT NOT NULL, role TEXT NOT NULL, PRIMARY KEY(account_id,name));
`)
	if err != nil {
		return err
	}
	for _, table := range []string{"messages", "sync_stage"} {
		columns, err := tableColumns(tx, table)
		if err != nil {
			return err
		}
		if !columns["gmail_message_id"] {
			if _, err = tx.Exec("ALTER TABLE " + table + " ADD COLUMN gmail_message_id TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS messages_gmail_identity ON messages(account_id,gmail_message_id) WHERE gmail_message_id<>'';
CREATE TABLE IF NOT EXISTS gmail_run_claims (
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 message_id TEXT NOT NULL, run_id TEXT NOT NULL, PRIMARY KEY(account_id,message_id,run_id));`)
	return err
}
