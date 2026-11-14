package store

import (
	"context"
	"database/sql"
	"time"
)

// migrateAgent creates the schema 10 tables for the built-in management agent
// and Telegram interaction. Every feature table starts empty, so an upgraded
// instance keeps the agent, Telegram polling and model calls disabled until an
// administrator configures them explicitly.
//
// Foreign keys never cascade into existing operation or task-run records: a
// deleted conversation or binding must not remove facts needed for remote
// verification.
func migrateAgent(tx *sql.Tx) error {
	_, e := tx.Exec(`
CREATE TABLE IF NOT EXISTS agent_provider_settings(id INTEGER PRIMARY KEY CHECK(id=1),version INTEGER NOT NULL,enabled INTEGER NOT NULL DEFAULT 0,instance_id TEXT NOT NULL,config TEXT NOT NULL,secret BLOB NOT NULL,key_generation INTEGER NOT NULL DEFAULT 0,capabilities TEXT NOT NULL DEFAULT '{}',updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS agent_grants(id TEXT PRIMARY KEY,version INTEGER NOT NULL,admin_version INTEGER NOT NULL,enabled INTEGER NOT NULL,revoked INTEGER NOT NULL DEFAULT 0,expires INTEGER NOT NULL,accounts TEXT NOT NULL,folders TEXT NOT NULL,actions TEXT NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS agent_grant_current ON agent_grants(revoked) WHERE revoked=0;
CREATE TABLE IF NOT EXISTS telegram_interaction_settings(id INTEGER PRIMARY KEY CHECK(id=1),version INTEGER NOT NULL,enabled INTEGER NOT NULL DEFAULT 0,bot_id INTEGER NOT NULL DEFAULT 0,bot_username TEXT NOT NULL DEFAULT '',credential_generation INTEGER NOT NULL DEFAULT 0,epoch INTEGER NOT NULL DEFAULT 0,activation_time INTEGER NOT NULL DEFAULT 0,state TEXT NOT NULL DEFAULT 'off',error_code TEXT NOT NULL DEFAULT '',updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS telegram_bind_codes(code_hash TEXT PRIMARY KEY,grant_id TEXT NOT NULL,grant_version INTEGER NOT NULL,admin_version INTEGER NOT NULL,bot_id INTEGER NOT NULL,epoch INTEGER NOT NULL,expires INTEGER NOT NULL,consumed_at INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS telegram_bindings(id TEXT PRIMARY KEY,bot_id INTEGER NOT NULL,user_id INTEGER NOT NULL,chat_id INTEGER NOT NULL,grant_id TEXT NOT NULL,admin_version INTEGER NOT NULL,epoch INTEGER NOT NULL,state TEXT NOT NULL,created_at INTEGER NOT NULL,revoked_at INTEGER NOT NULL DEFAULT 0,revoke_reason TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX IF NOT EXISTS telegram_binding_active ON telegram_bindings(state) WHERE state='active';
CREATE TABLE IF NOT EXISTS telegram_poll_state(bot_id INTEGER PRIMARY KEY,next_offset INTEGER NOT NULL DEFAULT 0,lease_owner TEXT NOT NULL DEFAULT '',lease_until INTEGER NOT NULL DEFAULT 0,generation INTEGER NOT NULL DEFAULT 0,last_poll_at INTEGER NOT NULL DEFAULT 0,error_code TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS telegram_inbox(id TEXT PRIMARY KEY,bot_id INTEGER NOT NULL,update_id INTEGER NOT NULL,epoch INTEGER NOT NULL,binding_id TEXT NOT NULL DEFAULT '',kind TEXT NOT NULL,user_id INTEGER NOT NULL DEFAULT 0,chat_id INTEGER NOT NULL DEFAULT 0,message_id INTEGER NOT NULL DEFAULT 0,payload BLOB,received_at INTEGER NOT NULL,event_at INTEGER NOT NULL,state TEXT NOT NULL,error_code TEXT NOT NULL DEFAULT '',updated_at INTEGER NOT NULL,UNIQUE(bot_id,update_id));
CREATE INDEX IF NOT EXISTS telegram_inbox_pending ON telegram_inbox(state,received_at);
CREATE TABLE IF NOT EXISTS agent_conversations(id TEXT PRIMARY KEY,binding_id TEXT NOT NULL UNIQUE,grant_version INTEGER NOT NULL,context_epoch INTEGER NOT NULL DEFAULT 1,intent_epoch INTEGER NOT NULL DEFAULT 1,stop_epoch INTEGER NOT NULL DEFAULT 0,next_seq INTEGER NOT NULL DEFAULT 1,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS agent_turns(id TEXT PRIMARY KEY,conversation_id TEXT NOT NULL REFERENCES agent_conversations(id),seq INTEGER NOT NULL,input_id TEXT NOT NULL UNIQUE,state TEXT NOT NULL,context_epoch INTEGER NOT NULL,intent_epoch INTEGER NOT NULL,stop_epoch INTEGER NOT NULL,grant_version INTEGER NOT NULL,admin_version INTEGER NOT NULL,config_version INTEGER NOT NULL DEFAULT 0,deadline INTEGER NOT NULL DEFAULT 0,lease_owner TEXT NOT NULL DEFAULT '',lease_until INTEGER NOT NULL DEFAULT 0,model_requests INTEGER NOT NULL DEFAULT 0,tool_calls INTEGER NOT NULL DEFAULT 0,error_code TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,UNIQUE(conversation_id,seq));
CREATE INDEX IF NOT EXISTS agent_turns_queue ON agent_turns(state,created_at);
CREATE TABLE IF NOT EXISTS agent_messages(id TEXT PRIMARY KEY,turn_id TEXT NOT NULL REFERENCES agent_turns(id),seq INTEGER NOT NULL,role TEXT NOT NULL,tool_call_id TEXT NOT NULL DEFAULT '',payload BLOB,context_epoch INTEGER NOT NULL,grant_version INTEGER NOT NULL,admin_version INTEGER NOT NULL,config_version INTEGER NOT NULL,created_at INTEGER NOT NULL,UNIQUE(turn_id,seq));
CREATE TABLE IF NOT EXISTS agent_tool_calls(id TEXT PRIMARY KEY,turn_id TEXT NOT NULL REFERENCES agent_turns(id),ordinal INTEGER NOT NULL,call_id TEXT NOT NULL,name TEXT NOT NULL,args_digest TEXT NOT NULL,action_id TEXT NOT NULL DEFAULT '',risk TEXT NOT NULL,state TEXT NOT NULL,result_code TEXT NOT NULL DEFAULT '',result BLOB,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,UNIQUE(turn_id,ordinal));
CREATE TABLE IF NOT EXISTS agent_actions(id TEXT PRIMARY KEY,conversation_id TEXT NOT NULL,intent_key TEXT NOT NULL,kind TEXT NOT NULL,args_digest TEXT NOT NULL,created_at INTEGER NOT NULL,UNIQUE(conversation_id,intent_key));
CREATE TABLE IF NOT EXISTS agent_mutations(mutation_id TEXT PRIMARY KEY,kind TEXT NOT NULL,actor TEXT NOT NULL,args_digest TEXT NOT NULL,target_id TEXT NOT NULL,target_version INTEGER NOT NULL,result TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS agent_proposals(id TEXT PRIMARY KEY,conversation_id TEXT NOT NULL,binding_id TEXT NOT NULL,turn_id TEXT NOT NULL DEFAULT '',token_hash TEXT NOT NULL UNIQUE,kind TEXT NOT NULL,state TEXT NOT NULL,payload BLOB,state_digest TEXT NOT NULL,grant_id TEXT NOT NULL,grant_version INTEGER NOT NULL,admin_version INTEGER NOT NULL,intent_epoch INTEGER NOT NULL,context_epoch INTEGER NOT NULL,stop_epoch INTEGER NOT NULL,expires INTEGER NOT NULL,chat_id INTEGER NOT NULL DEFAULT 0,message_id INTEGER NOT NULL DEFAULT 0,error_code TEXT NOT NULL DEFAULT '',accepted_by INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS agent_proposals_state ON agent_proposals(state,expires);
CREATE TABLE IF NOT EXISTS agent_proposal_steps(proposal_id TEXT NOT NULL REFERENCES agent_proposals(id),ordinal INTEGER NOT NULL,kind TEXT NOT NULL,mutation_id TEXT NOT NULL UNIQUE,state TEXT NOT NULL,target_id TEXT NOT NULL DEFAULT '',target_version INTEGER NOT NULL DEFAULT 0,error_code TEXT NOT NULL DEFAULT '',updated_at INTEGER NOT NULL,PRIMARY KEY(proposal_id,ordinal));
CREATE TABLE IF NOT EXISTS agent_run_links(task_run_id TEXT PRIMARY KEY,conversation_id TEXT NOT NULL,turn_id TEXT NOT NULL DEFAULT '',proposal_id TEXT NOT NULL DEFAULT '',binding_id TEXT NOT NULL,last_state TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS telegram_reply_outbox(id TEXT PRIMARY KEY,semantic_key TEXT NOT NULL,part INTEGER NOT NULL,bot_id INTEGER NOT NULL,chat_id INTEGER NOT NULL,binding_id TEXT NOT NULL,grant_id TEXT NOT NULL DEFAULT '',admin_version INTEGER NOT NULL,epoch INTEGER NOT NULL,context_epoch INTEGER NOT NULL DEFAULT 0,payload BLOB,edit_message_id INTEGER NOT NULL DEFAULT 0,message_id INTEGER NOT NULL DEFAULT 0,state TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL,lease_until INTEGER NOT NULL DEFAULT 0,error_code TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,UNIQUE(semantic_key,part));
CREATE INDEX IF NOT EXISTS telegram_reply_due ON telegram_reply_outbox(state,next_attempt);
`)
	return e
}

// migrateAgentFences adds the schema 11 fence columns. Rows written by a
// schema 10 binary get conservative defaults: version 0 never matches a live
// grant or provider version, so old proposals cannot be confirmed or applied
// and old queued replies are never sent. Unfinished ones are closed here
// explicitly as well, so nothing from before the upgrade is revived.
func migrateAgentFences(tx *sql.Tx, current int) error {
	if current >= 11 {
		return nil
	}
	for _, c := range []struct{ table, column string }{{"agent_proposals", "config_version"}, {"telegram_reply_outbox", "grant_version"}, {"telegram_inbox", "control"}} {
		var n int
		if e := tx.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name=?", c.table, c.column).Scan(&n); e != nil {
			return e
		}
		if n == 0 {
			if _, e := tx.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.column + " INTEGER NOT NULL DEFAULT 0"); e != nil {
				return e
			}
		}
	}
	_, e := tx.Exec(`
CREATE INDEX IF NOT EXISTS telegram_inbox_control ON telegram_inbox(state,control,received_at);
UPDATE agent_proposals SET state='invalidated',error_code='schema_upgrade' WHERE state IN ('pending','accepted','applying');
UPDATE telegram_reply_outbox SET state='suppressed',error_code='schema_upgrade' WHERE state IN ('pending','retry','sending');`)
	return e
}

// LegacyConnection marks a schema 12 provider whose configuration still has
// to be copied into a saved connection. The copy needs the vault (each
// secret is encrypted for its own record), so the agent runtime completes it
// at startup before the admin server listens.
const LegacyConnection = "legacy:pending"

// migrateAgentConnections adds the schema 13 saved model connections. The
// current provider row stays the only configuration the runtime reads; its
// version, enabled state and capabilities are untouched, so running state and
// frozen proposals survive the upgrade. No mail index is rebuilt.
func migrateAgentConnections(tx *sql.Tx, current int) error {
	if _, e := tx.Exec(`CREATE TABLE IF NOT EXISTS agent_provider_connections(id TEXT PRIMARY KEY,version INTEGER NOT NULL,name TEXT NOT NULL,config TEXT NOT NULL,secret BLOB NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL)`); e != nil {
		return e
	}
	var n int
	if e := tx.QueryRow("SELECT count(*) FROM pragma_table_info('agent_provider_settings') WHERE name='connection_id'").Scan(&n); e != nil {
		return e
	}
	if n == 0 {
		if _, e := tx.Exec("ALTER TABLE agent_provider_settings ADD COLUMN connection_id TEXT NOT NULL DEFAULT ''"); e != nil {
			return e
		}
	}
	if current >= 13 {
		return nil
	}
	_, e := tx.Exec("UPDATE agent_provider_settings SET connection_id=? WHERE id=1 AND connection_id=''", LegacyConnection)
	return e
}

// cleanupAgent applies the agent/Telegram retention: conversation and reply
// payloads after 7 days, idempotency and minimal records after 60 days. It
// only touches terminal records; pending proposals, queued or running turns,
// undelivered replies, unprocessed inbox events and unfinished run links are
// kept regardless of age. The per-bot next_offset watermark stays, so old
// updates cannot be re-ingested after their tombstones are removed.
func (s *Store) cleanupAgent(ctx context.Context, now time.Time) {
	payload := now.AddDate(0, 0, -7).Unix()
	minimal := now.AddDate(0, 0, -60).Unix()
	terminalTurn := "state IN ('completed','failed','cancelled','interrupted')"
	for _, q := range []struct {
		sql  string
		args []any
	}{
		// Whole turns lose their messages together, so no tool message is orphaned.
		{"DELETE FROM agent_messages WHERE turn_id IN (SELECT id FROM agent_turns WHERE " + terminalTurn + " AND updated_at<?)", []any{payload}},
		{"UPDATE agent_tool_calls SET result=NULL WHERE result IS NOT NULL AND turn_id IN (SELECT id FROM agent_turns WHERE " + terminalTurn + " AND updated_at<?)", []any{payload}},
		{"UPDATE agent_proposals SET payload=NULL WHERE payload IS NOT NULL AND state IN ('applied','partial','failed','rejected','expired','invalidated','cancelled') AND updated_at<?", []any{payload}},
		{"DELETE FROM telegram_reply_outbox WHERE state IN ('sent','failed','suppressed','unknown') AND updated_at<?", []any{payload}},
		{"UPDATE telegram_inbox SET payload=NULL WHERE payload IS NOT NULL AND state<>'pending' AND updated_at<?", []any{payload}},
		{"DELETE FROM telegram_bind_codes WHERE expires<? OR (consumed_at>0 AND consumed_at<?)", []any{now.Unix() - 86400, now.Unix() - 86400}},
		{"DELETE FROM telegram_inbox WHERE state<>'pending' AND updated_at<?", []any{minimal}},
		{"DELETE FROM agent_tool_calls WHERE turn_id IN (SELECT id FROM agent_turns WHERE " + terminalTurn + " AND updated_at<?)", []any{minimal}},
		{"DELETE FROM agent_proposal_steps WHERE proposal_id IN (SELECT id FROM agent_proposals WHERE state IN ('applied','partial','failed','rejected','expired','invalidated','cancelled') AND updated_at<?)", []any{minimal}},
		{"DELETE FROM agent_proposals WHERE state IN ('applied','partial','failed','rejected','expired','invalidated','cancelled') AND updated_at<?", []any{minimal}},
		{"DELETE FROM agent_turns WHERE " + terminalTurn + " AND updated_at<? AND id NOT IN (SELECT turn_id FROM agent_proposals) AND id NOT IN (SELECT turn_id FROM agent_messages)", []any{minimal}},
		{"DELETE FROM agent_actions WHERE created_at<?", []any{minimal}},
		{"DELETE FROM agent_mutations WHERE created_at<?", []any{minimal}},
		{"DELETE FROM agent_run_links WHERE last_state LIKE 'final:%' AND updated_at<?", []any{minimal}},
	} {
		_, _ = s.DB.ExecContext(ctx, q.sql, q.args...)
	}
}
