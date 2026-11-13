package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

var agentTables = []string{"agent_provider_settings", "agent_grants", "telegram_interaction_settings", "telegram_bind_codes", "telegram_bindings", "telegram_poll_state", "telegram_inbox", "agent_conversations", "agent_turns", "agent_messages", "agent_tool_calls", "agent_actions", "agent_mutations", "agent_proposals", "agent_proposal_steps", "agent_run_links", "telegram_reply_outbox"}

// downgradeToSchema9 removes the schema 10 tables, as a 0.4.20 database has.
func downgradeToSchema9(t *testing.T, s *Store) {
	t.Helper()
	for i := len(agentTables) - 1; i >= 0; i-- {
		if _, err := s.DB.Exec("DROP TABLE " + agentTables[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.Exec("PRAGMA user_version=9"); err != nil {
		t.Fatal(err)
	}
}

func TestSchema9UpgradeKeepsDataAndStartsDisabled(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`INSERT INTO accounts VALUES('acc','fixture','qq','f@qq.com','imap.qq.com',993,'[]',X'01','',1);
INSERT INTO automation_policies VALUES('acc',3,'policy-payload');
INSERT INTO automation_tasks VALUES('task','acc',2,'svc:x','task-payload',1,'internal',5,1);
INSERT INTO notification_channel VALUES(1,4,'{"enabled":true,"chat_id":"1","policy":"failures"}',X'0203');`); err != nil {
		t.Fatal(err)
	}
	downgradeToSchema9(t, s)
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	_ = s.DB.QueryRow("PRAGMA user_version").Scan(&version)
	if version != SchemaVersion {
		t.Fatal("schema", version)
	}
	var policy, task, channel string
	if err = s.DB.QueryRow("SELECT (SELECT payload FROM automation_policies),(SELECT payload FROM automation_tasks),(SELECT payload FROM notification_channel)").Scan(&policy, &task, &channel); err != nil || policy != "policy-payload" || task != "task-payload" || channel == "" {
		t.Fatal("existing data changed", policy, task, channel, err)
	}
	for _, table := range agentTables {
		var n int
		if err = s.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal("new feature table not empty after upgrade", table, n, err)
		}
	}
	s.Close()
	// Re-opening an upgraded database is a no-op.
	if s, err = Open(dir); err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestSchema10MigrationFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("INSERT INTO accounts VALUES('acc','fixture','qq','f@qq.com','imap.qq.com',993,'[]',X'01','',1)"); err != nil {
		t.Fatal(err)
	}
	downgradeToSchema9(t, s)
	// A conflicting object makes the schema 10 migration fail midway.
	if _, err = s.DB.Exec("CREATE VIEW agent_turns AS SELECT 1 AS id"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err = Open(dir); err == nil {
		t.Fatal("conflicting migration succeeded")
	}
	raw, err := Open(dir)
	if err == nil {
		raw.Close()
		t.Fatal("expected failure again")
	}
	// Inspect with a plain connection: still schema 9, nothing half-created.
	s2, err := openRaw(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var version, n, tables int
	_ = s2.QueryRow("PRAGMA user_version").Scan(&version)
	_ = s2.QueryRow("SELECT count(*) FROM accounts").Scan(&n)
	_ = s2.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('agent_grants','telegram_inbox')").Scan(&tables)
	if version != 9 || n != 1 || tables != 0 {
		t.Fatal("failed migration was not rolled back", version, n, tables)
	}
}

func TestAgentRetentionKeepsActiveRecords(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := time.Now().AddDate(0, 0, -90).Unix()
	if _, err = s.DB.Exec(`INSERT INTO agent_conversations(id,binding_id,grant_version,created_at,updated_at) VALUES('c','b',1,1,1);
INSERT INTO agent_turns(id,conversation_id,seq,input_id,state,context_epoch,intent_epoch,stop_epoch,grant_version,admin_version,created_at,updated_at) VALUES('done','c',1,'i1','completed',1,1,0,1,1,?1,?1),('queued','c',2,'i2','queued',1,1,0,1,1,?1,?1),('waiting','c',3,'i3','completed',1,1,0,1,1,?1,?1);
INSERT INTO agent_messages(id,turn_id,seq,role,payload,context_epoch,grant_version,admin_version,config_version,created_at) VALUES('m1','done',0,'user',X'00',1,1,1,1,?1),('m2','queued',0,'user',X'00',1,1,1,1,?1);
INSERT INTO agent_proposals(id,conversation_id,binding_id,turn_id,token_hash,kind,state,payload,state_digest,grant_id,grant_version,admin_version,intent_epoch,context_epoch,stop_epoch,expires,created_at,updated_at) VALUES('p-old','c','b','done','h1','workflow','expired',X'00','d','g',1,1,1,1,0,?1,?1,?1),('p-live','c','b','waiting','h2','workflow','applying',X'00','d','g',1,1,1,1,0,?1,?1,?1);
INSERT INTO telegram_reply_outbox(id,semantic_key,part,bot_id,chat_id,binding_id,admin_version,epoch,payload,state,next_attempt,created_at,updated_at) VALUES('o1','k1',0,1,1,'b',1,1,X'00','sent',0,?1,?1),('o2','k2',0,1,1,'b',1,1,X'00','pending',0,?1,?1);
INSERT INTO telegram_inbox(id,bot_id,update_id,epoch,kind,payload,received_at,event_at,state,updated_at) VALUES('in1',1,1,1,'message',X'00',?1,?1,'processed',?1),('in2',1,2,1,'message',X'00',?1,?1,'pending',?1);
INSERT INTO agent_run_links(task_run_id,conversation_id,binding_id,last_state,created_at,updated_at) VALUES('r-final','c','b','final:completed',?1,?1),('r-active','c','b','running',?1,?1);`, old); err != nil {
		t.Fatal(err)
	}
	s.cleanupAgent(context.Background(), time.Now())
	count := func(q string) int {
		var n int
		if err := s.DB.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(q, err)
		}
		return n
	}
	checks := map[string]int{
		"SELECT count(*) FROM agent_turns WHERE id='queued'":                1,
		"SELECT count(*) FROM agent_messages WHERE turn_id='queued'":        1,
		"SELECT count(*) FROM agent_messages WHERE turn_id='done'":          0,
		"SELECT count(*) FROM agent_proposals WHERE id='p-live'":            1,
		"SELECT count(*) FROM agent_proposals WHERE id='p-old'":             0,
		"SELECT count(*) FROM telegram_reply_outbox WHERE id='o2'":          1,
		"SELECT count(*) FROM telegram_reply_outbox WHERE id='o1'":          0,
		"SELECT count(*) FROM telegram_inbox WHERE id='in2'":                1,
		"SELECT count(*) FROM telegram_inbox WHERE id='in1'":                0,
		"SELECT count(*) FROM agent_run_links WHERE task_run_id='r-active'": 1,
		"SELECT count(*) FROM agent_run_links WHERE task_run_id='r-final'":  0,
	}
	for q, want := range checks {
		if got := count(q); got != want {
			t.Fatal(q, got, want)
		}
	}
}

func openRaw(dir string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+filepath.Join(dir, "mail-auto.db"))
}

// Schema 10 → 11 adds the fence columns with conservative defaults and
// closes unfinished proposals and unsent replies from the old binary.
func TestSchema10To11ClosesOldAgentWork(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`ALTER TABLE agent_proposals DROP COLUMN config_version; ALTER TABLE telegram_reply_outbox DROP COLUMN grant_version; DROP INDEX telegram_inbox_control; ALTER TABLE telegram_inbox DROP COLUMN control;
INSERT INTO agent_proposals(id,conversation_id,binding_id,turn_id,token_hash,kind,state,state_digest,grant_id,grant_version,admin_version,intent_epoch,context_epoch,stop_epoch,expires,created_at,updated_at) VALUES('p','c','b','t','h','workflow','applying','d','g',1,1,1,1,0,9999999999,1,1);
INSERT INTO telegram_reply_outbox(id,semantic_key,part,bot_id,chat_id,binding_id,admin_version,epoch,payload,state,next_attempt,created_at,updated_at) VALUES('o','k',0,1,1,'b',1,1,X'00','pending',0,1,1);
PRAGMA user_version=10;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(dir); err != nil {
		t.Fatal(err)
	}
	var version, gv int
	var pState, oState string
	_ = s.DB.QueryRow("PRAGMA user_version").Scan(&version)
	_ = s.DB.QueryRow("SELECT state FROM agent_proposals WHERE id='p'").Scan(&pState)
	_ = s.DB.QueryRow("SELECT state,grant_version FROM telegram_reply_outbox WHERE id='o'").Scan(&oState, &gv)
	if version != SchemaVersion || pState != "invalidated" || oState != "suppressed" || gv != 0 {
		t.Fatal(version, pState, oState, gv)
	}
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal("re-open after upgrade", err)
	}
	reopened.Close()
}
