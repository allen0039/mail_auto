package store

import "testing"

func TestSchema11UpgradePreservesBusinessData(t *testing.T) {
	directory := t.TempDir()
	database, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.DB.Exec(`INSERT INTO accounts VALUES('a','account','qq','a@qq.com','imap.qq.com',993,'["INBOX"]',x'0102','',1);
INSERT INTO mailboxes VALUES('a','INBOX',7,1);
INSERT INTO messages(ref,account_id,folder,uidvalidity,uid,subject,sender,sent_at,unread,size) VALUES('ref','a','INBOX',7,1,'old','sender',1,1,20);
DROP TABLE account_network; DROP TABLE proxy_profiles; DROP TABLE network_settings;
PRAGMA user_version=11;`)
	if err != nil {
		t.Fatal(err)
	}
	database.Close()
	database, err = Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var version, count int
	database.DB.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 12 {
		t.Fatal(version)
	}
	for _, table := range []string{"accounts", "mailboxes", "messages"} {
		database.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count)
		if count != 1 {
			t.Fatal("migration discarded business data", table)
		}
	}
	for _, table := range []string{"account_network", "proxy_profiles", "network_settings"} {
		database.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count)
		if count != 0 {
			t.Fatal("migration changed default network behavior", table)
		}
	}
}
