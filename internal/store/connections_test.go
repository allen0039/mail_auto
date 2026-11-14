package store

import "testing"

// A schema 12 database keeps its provider row exactly (version, enabled,
// key, capabilities) and gains the connection table plus a pending-migration
// marker that the agent runtime completes with the vault at startup.
func TestSchema12UpgradeKeepsProviderAndMarksMigration(t *testing.T) {
	directory := t.TempDir()
	database, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.DB.Exec(`INSERT INTO accounts VALUES('a','account','qq','a@qq.com','imap.qq.com',993,'["INBOX"]',x'0102','',1);
INSERT INTO messages(ref,account_id,folder,uidvalidity,uid,subject,sender,sent_at,unread,size) VALUES('ref','a','INBOX',7,1,'old','sender',1,1,20);
DROP TABLE agent_provider_connections;
DROP TABLE agent_provider_settings;
CREATE TABLE agent_provider_settings(id INTEGER PRIMARY KEY CHECK(id=1),version INTEGER NOT NULL,enabled INTEGER NOT NULL DEFAULT 0,instance_id TEXT NOT NULL,config TEXT NOT NULL,secret BLOB NOT NULL,key_generation INTEGER NOT NULL DEFAULT 0,capabilities TEXT NOT NULL DEFAULT '{}',updated_at INTEGER NOT NULL);
INSERT INTO agent_provider_settings VALUES(1,7,1,'inst','{"base_url":"https://a.example/v1","model":"m"}',x'0a0b',3,'{"digest":"d","tool_loop":"ok"}',5);
PRAGMA user_version=12;`)
	if err != nil {
		t.Fatal(err)
	}
	database.Close()
	for i := 0; i < 2; i++ { // reopening is idempotent
		database, err = Open(directory)
		if err != nil {
			t.Fatal(err)
		}
		var version, ver, enabled, keyGen, messages, connections int
		var connection, caps string
		var secret []byte
		database.DB.QueryRow("PRAGMA user_version").Scan(&version)
		if err = database.DB.QueryRow("SELECT version,enabled,key_generation,secret,capabilities,connection_id FROM agent_provider_settings WHERE id=1").Scan(&ver, &enabled, &keyGen, &secret, &caps, &connection); err != nil {
			t.Fatal(err)
		}
		database.DB.QueryRow("SELECT count(*) FROM messages").Scan(&messages)
		database.DB.QueryRow("SELECT count(*) FROM agent_provider_connections").Scan(&connections)
		if version != SchemaVersion || ver != 7 || enabled != 1 || keyGen != 3 || string(secret) != "\x0a\x0b" || caps != `{"digest":"d","tool_loop":"ok"}` || connection != LegacyConnection || messages != 1 || connections != 0 {
			t.Fatal(version, ver, enabled, keyGen, caps, connection, messages, connections)
		}
		database.Close()
	}
}

func TestFreshDatabaseHasNoPendingConnectionMigration(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var n int
	database.DB.QueryRow("SELECT count(*) FROM agent_provider_settings").Scan(&n)
	if n != 0 {
		t.Fatal("fresh database got a provider row")
	}
}
