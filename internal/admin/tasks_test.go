package admin

import (
	"encoding/json"
	"mail-auto/internal/config"
	"mail-auto/internal/mail"
	"net/http"
	"testing"
	"time"
)

func TestTasksAndNotificationsHTTPBoundary(t *testing.T) {
	auth, session := formalAuth(t)
	svc := &mail.Service{Store: auth.Store, Vault: auth.Vault}
	a, e := svc.Create(mail.AccountInput{Name: "fixture", Provider: "qq", Username: "fixture@qq.com", Password: "fixture"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = auth.Store.DB.Exec("INSERT INTO mailboxes VALUES(?,'INBOX',7,?)", a.ID, time.Now().Unix()); e != nil {
		t.Fatal(e)
	}
	admin, _ := svc.AdminAccess()
	p, _ := svc.AutomationPolicy(admin, a.ID)
	p.SourceFolders = []string{"INBOX"}
	if _, e = svc.SaveAutomationPolicy(admin, p); e != nil {
		t.Fatal(e)
	}
	b := &browser{t: t, h: New(auth, svc, config.Config{PublicURL: "http://mail.test"}).Handler(), cookies: map[string]*http.Cookie{}, csrf: session.CSRF}
	for _, path := range []string{"/api/tasks?account_id=" + a.ID, "/api/notifications/telegram", "/api/notifications/deliveries"} {
		if w := b.request("GET", path, "", false); w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
	b.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: session.Token}
	raw, _ := json.Marshal(mail.Task{AccountID: a.ID, Name: "task", SourceFolders: []string{"INBOX"}, Trigger: "manual", Engine: "rules"})
	if w := b.request("POST", "/api/tasks", string(raw), false); w.Code != 403 {
		t.Fatal("task CSRF", w.Code)
	}
	w := b.request("POST", "/api/tasks", string(raw), true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var task mail.Task
	if e = json.Unmarshal(w.Body.Bytes(), &task); e != nil || task.Enabled || task.Mode != "observe" {
		t.Fatal(task, e)
	}
	if w = b.request("POST", "/api/tasks/"+task.ID+"/preview", "{}", true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = b.request("PUT", "/api/notifications/telegram", `{"version":0,"enabled":false,"chat_id":"123456789","policy":"failures","bot_token":"123456789:abcdefghijklmnopqrstuvwxyz0123456789"}`, false); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w = b.request("PUT", "/api/notifications/telegram", `{"version":0,"enabled":false,"chat_id":"123456789","policy":"failures","bot_token":"123456789:abcdefghijklmnopqrstuvwxyz0123456789"}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = b.request("GET", "/api/notifications/telegram", "", false)
	if w.Code != 200 || json.Valid(w.Body.Bytes()) == false {
		t.Fatal(w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if _, ok := out["bot_token"]; ok || out["configured"] != true {
		t.Fatal(out)
	}
	if w = b.request("POST", "/api/notifications/telegram/test", "{}", false); w.Code != 403 {
		t.Fatal("test send CSRF", w.Code)
	}
}
