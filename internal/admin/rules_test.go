package admin

import (
	"encoding/json"
	"mail-auto/internal/config"
	"mail-auto/internal/mail"
	"net/http"
	"testing"
)

func TestRulesAndPolicyHTTPBoundary(t *testing.T) {
	auth, session := formalAuth(t)
	svc := &mail.Service{Store: auth.Store, Vault: auth.Vault}
	account, err := svc.Create(mail.AccountInput{Name: "fixture", Provider: "qq", Username: "fixture@qq.com", Password: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"INBOX", "Archive"} {
		if _, err = auth.Store.DB.Exec("INSERT INTO mailboxes(account_id,name,uidvalidity,synced_at) VALUES(?,?,7,0)", account.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	b := &browser{t: t, h: New(auth, svc, config.Config{PublicURL: "http://mail.test"}).Handler(), cookies: map[string]*http.Cookie{}, csrf: session.CSRF}
	rule := mail.Rule{AccountID: account.ID, Name: "账单", SourceFolders: []string{"INBOX"}, Match: mail.RuleGroup{Mode: "all", Conditions: []mail.RuleCondition{}}, Action: "move", Destination: "Archive"}
	raw, _ := json.Marshal(rule)
	if w := b.request("POST", "/api/rules", string(raw), true); w.Code != 401 {
		t.Fatal("anonymous write", w.Code)
	}
	b.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: session.Token}
	if w := b.request("POST", "/api/rules", string(raw), false); w.Code != 403 {
		t.Fatal("CSRF", w.Code)
	}
	w := b.request("POST", "/api/rules", string(raw), true)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if json.Unmarshal(w.Body.Bytes(), &rule) != nil || rule.Version != 1 {
		t.Fatal(w.Body.String())
	}
	raw, _ = json.Marshal(rule)
	w = b.request("PUT", "/api/rules/"+rule.ID, string(raw), true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = b.request("PUT", "/api/rules/"+rule.ID, string(raw), true); w.Code != 409 {
		t.Fatal("stale version", w.Code)
	}
	for _, path := range []string{"/api/rules?account_id=" + account.ID, "/api/rules/" + rule.ID + "/history", "/api/automation/policy/" + account.ID, "/api/automation/capabilities/" + account.ID, "/api/automation/batches?account_id=" + account.ID} {
		if w = b.request("GET", path, "", false); w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	var p mail.AutomationPolicy
	w = b.request("GET", "/api/automation/policy/"+account.ID, "", false)
	json.Unmarshal(w.Body.Bytes(), &p)
	if p.Enabled || p.Mode != "observe" || p.AllowBody {
		t.Fatal("unsafe defaults", p)
	}
	p.SourceFolders = []string{"INBOX"}
	raw, _ = json.Marshal(p)
	if w = b.request("PUT", "/api/automation/policy/"+account.ID, string(raw), false); w.Code != 403 {
		t.Fatal("policy CSRF", w.Code)
	}
	if w = b.request("PUT", "/api/automation/policy/"+account.ID, string(raw), true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = b.request("PUT", "/api/automation/policy/"+account.ID, string(raw), true); w.Code != 409 {
		t.Fatal("policy lost update", w.Code)
	}
}
