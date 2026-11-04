package store

import "database/sql"

func migrateTasks(tx *sql.Tx) error {
	_, e := tx.Exec(`
CREATE TABLE IF NOT EXISTS automation_tasks(id TEXT PRIMARY KEY,account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,version INTEGER NOT NULL,service_owner TEXT NOT NULL UNIQUE,payload TEXT NOT NULL,enabled INTEGER NOT NULL,trigger_mode TEXT NOT NULL,next_run_at INTEGER NOT NULL DEFAULT 0,updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS task_due ON automation_tasks(enabled,trigger_mode,next_run_at);
CREATE TABLE IF NOT EXISTS task_runs(id TEXT PRIMARY KEY,task_id TEXT NOT NULL REFERENCES automation_tasks(id) ON DELETE CASCADE,account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,owner TEXT NOT NULL,run_key TEXT NOT NULL,task_version INTEGER NOT NULL,source TEXT NOT NULL,state TEXT NOT NULL,payload TEXT NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,expires INTEGER NOT NULL,lease_owner TEXT NOT NULL DEFAULT '',lease_until INTEGER NOT NULL DEFAULT 0,UNIQUE(task_id,run_key));
CREATE UNIQUE INDEX IF NOT EXISTS task_account_active ON task_runs(account_id) WHERE state IN ('queued','running','awaiting_ai');
CREATE INDEX IF NOT EXISTS task_runs_history ON task_runs(task_id,created_at DESC,id);
CREATE TABLE IF NOT EXISTS task_run_batches(run_id TEXT PRIMARY KEY REFERENCES task_runs(id) ON DELETE CASCADE,batch_id TEXT NOT NULL UNIQUE REFERENCES automation_batches(id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS notification_channel(id INTEGER PRIMARY KEY CHECK(id=1),version INTEGER NOT NULL,payload TEXT NOT NULL,secret BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS notification_outbox(id TEXT PRIMARY KEY,run_id TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,event_key TEXT NOT NULL UNIQUE,channel_version INTEGER NOT NULL,state TEXT NOT NULL,payload TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL,lease_until INTEGER NOT NULL DEFAULT 0,error_code TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS notification_due ON notification_outbox(state,next_attempt);
CREATE TABLE IF NOT EXISTS notification_cooldown(task_id TEXT NOT NULL,event TEXT NOT NULL,last_sent INTEGER NOT NULL,PRIMARY KEY(task_id,event));
`)
	return e
}
