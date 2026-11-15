package admin

import (
	"encoding/json"
	"errors"
	"mail-auto/internal/config"
	"mail-auto/internal/mail"
	"net/http"
	"testing"
	"time"
)

// Saving, enabling and running a task report a stable code next to the
// unchanged status and message; known reasons carry the service's own code.
func TestTaskErrorCodes(t *testing.T) {
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
	b.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: session.Token}
	expect := func(method, path string, body any, status int, code, area string) map[string]string {
		t.Helper()
		raw, _ := json.Marshal(body)
		w := b.request(method, path, string(raw), true)
		var out map[string]string
		if w.Code != status || json.Unmarshal(w.Body.Bytes(), &out) != nil || out["code"] != code || out["area"] != area || out["error"] == "" {
			t.Fatalf("%s %s: %d %s; want %d %s/%s", method, path, w.Code, w.Body.String(), status, code, area)
		}
		return out
	}
	base := mail.Task{AccountID: a.ID, Name: "task", SourceFolders: []string{"INBOX"}, Trigger: "manual", Engine: "rules"}
	raw, _ := json.Marshal(base)
	w := b.request("POST", "/api/tasks", string(raw), true)
	var task mail.Task
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &task) != nil {
		t.Fatal(w.Code, w.Body.String())
	}

	// Known policy reasons keep their status and name the setting.
	internal := base
	internal.Trigger = "internal"
	expect("POST", "/api/tasks", internal, 403, "internal_not_allowed", "policy")
	expect("POST", "/api/tasks/"+task.ID+"/run", map[string]any{"version": task.Version, "run_key": "k1"}, 403, "policy_disabled", "policy")
	expect("POST", "/api/tasks/"+task.ID+"/enabled", map[string]any{"version": task.Version, "enabled": true}, 403, "policy_disabled", "policy")

	// A stale version is a version conflict on every path, never "invalid input".
	stale := task
	stale.Version = task.Version - 1
	stale.Name = "renamed"
	expect("PUT", "/api/tasks/"+task.ID, stale, 409, "task_version_conflict", "task")
	expect("POST", "/api/tasks/"+task.ID+"/enabled", map[string]any{"version": task.Version + 1, "enabled": true}, 409, "task_version_conflict", "task")
	expect("POST", "/api/tasks/"+task.ID+"/run", map[string]any{"version": task.Version + 1, "run_key": "k2"}, 400, "task_version_conflict", "task")

	// Reasons without a specific code fall back to the sentinel's code.
	bad := base
	bad.Name = ""
	expect("POST", "/api/tasks", bad, 400, "invalid_input", "")
	expect("PUT", "/api/tasks/missing", base, 404, "not_found", "")

	// Nothing above changed the task or created a run.
	got, e := svc.Task(admin, task.ID)
	if e != nil || got.Version != task.Version || got.Name != task.Name || got.Enabled {
		t.Fatal(got, e)
	}
	var runs int
	_ = auth.Store.DB.QueryRow("SELECT count(*) FROM task_runs").Scan(&runs)
	if runs != 0 {
		t.Fatal("runs", runs)
	}
}

// TaskError keeps errors.Is and the error text of the wrapped sentinel, so
// MCP and Agent callers see exactly what they saw before.
func TestTaskErrorWrapsSentinel(t *testing.T) {
	e := error(&mail.TaskError{Code: "task_version_conflict", Area: "task", Err: mail.ErrConflict})
	if !errors.Is(e, mail.ErrConflict) || e.Error() != mail.ErrConflict.Error() {
		t.Fatal(e)
	}
	status, _, code := operationStatus(e)
	if status != 409 || code != "state_conflict" {
		t.Fatal(status, code)
	}
}
