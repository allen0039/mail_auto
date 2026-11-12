package store

import (
	"database/sql"
	"strings"
)

// metadataColumns are appended by schema 8. Values stay empty for older rows
// until the automatic sync re-reads them as metadata version 2.
var metadataColumns = []string{
	"recipients TEXT NOT NULL DEFAULT ''",
	"cc TEXT NOT NULL DEFAULT ''",
	"message_id TEXT NOT NULL DEFAULT ''",
	"in_reply_to TEXT NOT NULL DEFAULT ''",
	"thread_refs TEXT NOT NULL DEFAULT ''",
	"list_id TEXT NOT NULL DEFAULT ''",
	"has_attachment INTEGER NOT NULL DEFAULT 0",
}

func tableColumns(tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var value any
		if err = rows.Scan(&cid, &name, &kind, &notnull, &value, &pk); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func migrateMetadata(tx *sql.Tx) error {
	for _, table := range []string{"messages", "sync_stage"} {
		columns, err := tableColumns(tx, table)
		if err != nil {
			return err
		}
		for _, field := range metadataColumns {
			if columns[strings.Fields(field)[0]] {
				continue
			}
			if _, err := tx.Exec("ALTER TABLE " + table + " ADD COLUMN " + field); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS messages_message_id ON messages(account_id,message_id) WHERE message_id<>''`)
	return err
}

// diagnosticColumns are added by schema 9. They hold only fixed, sanitized
// messages (never provider text, credentials or mail content).
var diagnosticColumns = []string{
	"detail TEXT NOT NULL DEFAULT ''",
	"recovery_code TEXT NOT NULL DEFAULT ''",
	"recovery_detail TEXT NOT NULL DEFAULT ''",
	"checked_at INTEGER NOT NULL DEFAULT 0",
}

func migrateDiagnostics(tx *sql.Tx) error {
	ops, err := tableColumns(tx, "operations")
	if err != nil {
		return err
	}
	// "user" for an explicit stop; "system:<reason>" when a task deadline or
	// lease ended the run. Older cancelled rows stay "" and count as explicit.
	if !ops["cancel_source"] {
		if _, err = tx.Exec("ALTER TABLE operations ADD COLUMN cancel_source TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	columns, err := tableColumns(tx, "operation_items")
	if err != nil {
		return err
	}
	for _, field := range diagnosticColumns {
		if columns[strings.Fields(field)[0]] {
			continue
		}
		if _, err := tx.Exec("ALTER TABLE operation_items ADD COLUMN " + field); err != nil {
			return err
		}
	}
	return nil
}
