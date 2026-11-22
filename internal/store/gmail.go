package store

import "database/sql"

func migrateGmail(tx *sql.Tx) error {
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
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS messages_gmail_identity ON messages(account_id,gmail_message_id) WHERE gmail_message_id<>'';
CREATE TABLE IF NOT EXISTS gmail_run_claims (
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 message_id TEXT NOT NULL, run_id TEXT NOT NULL, PRIMARY KEY(account_id,message_id,run_id));`)
	if err != nil {
		return err
	}
	var legacyRoles bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='mail_oauth_folder_roles')").Scan(&legacyRoles); err != nil {
		return err
	}
	if legacyRoles {
		if _, err = tx.Exec("ALTER TABLE mail_oauth_folder_roles RENAME TO gmail_folder_roles"); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS gmail_folder_roles (
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 name TEXT NOT NULL, role TEXT NOT NULL, PRIMARY KEY(account_id,name));
DROP TABLE IF EXISTS mail_oauth_states;
DROP TABLE IF EXISTS mail_oauth_accounts;
DROP TABLE IF EXISTS mail_oauth_apps;`)
	return err
}
