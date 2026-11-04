package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
	"mail-auto/internal/store"
)

type bearerTransport struct{ secret string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.secret)
	return http.DefaultTransport.RoundTrip(r)
}
func TestStreamableMCPPermissionsAndRevocation(t *testing.T) {
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	v, e := credentials.Open(t.TempDir(), true)
	if e != nil {
		t.Fatal(e)
	}
	st.DB.Exec("INSERT INTO admin VALUES(1,'test','unused',0,1,0,1)")
	svc := &mail.Service{Store: st, Vault: v}
	a, e := svc.Create(mail.AccountInput{Name: "fixture", Provider: "qq", Username: "fixture@qq.com", Password: "fake"})
	if e != nil {
		t.Fatal(e)
	}
	token, secret, e := svc.CreateToken(mail.TokenInput{Name: "MCP fixture", Accounts: []string{a.ID}, Actions: []string{"read"}})
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(Handler(svc, "https://mail.test"))
	defer srv.Close()
	resp, e := http.Get(srv.URL + "/mcp")
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatal(resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{secret}}, DisableStandaloneSSE: true}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	listed, e := session.ListTools(ctx, nil)
	if e != nil || len(listed.Tools) < 10 {
		t.Fatal(listed, e)
	}
	for _, tool := range listed.Tools {
		if strings.Contains(tool.Name, "label") {
			t.Fatal("local classification tool exposed", tool.Name)
		}
		if tool.Name == "mail_execute" && (tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint) {
			t.Fatal("unsafe hint")
		}
	}
	result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "mail_accounts", Arguments: map[string]any{}})
	if e != nil || result.IsError {
		t.Fatal(result, e)
	}
	result, e = session.CallTool(ctx, &mcp.CallToolParams{Name: "mail_search", Arguments: map[string]any{"account_id": "other"}})
	if e != nil || !result.IsError {
		t.Fatal("cross account accepted", result, e)
	}
	result, e = session.CallTool(ctx, &mcp.CallToolParams{Name: "mail_create_folder", Arguments: map[string]any{"account_id": a.ID, "name": "denied"}})
	if e != nil || !result.IsError {
		t.Fatal("write accepted", result, e)
	}
	if e = svc.RevokeToken(token.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = session.ListTools(ctx, nil); e == nil {
		t.Fatal("revoked HTTP bearer accepted")
	}
}

func TestAutomationMCPToolSchemaAndScopedFlow(t *testing.T) {
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	v, e := credentials.Open(t.TempDir(), true)
	if e != nil {
		t.Fatal(e)
	}
	st.DB.Exec("INSERT INTO admin VALUES(1,'test','unused',0,1,0,1)")
	svc := &mail.Service{Store: st, Vault: v}
	account, e := svc.Create(mail.AccountInput{Name: "fixture", Provider: "qq", Username: "fixture@qq.com", Password: "fake"})
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"INBOX", "Archive"} {
		if _, e = st.DB.Exec("INSERT INTO mailboxes(account_id,name,uidvalidity,synced_at) VALUES(?,?,7,0)", account.ID, name); e != nil {
			t.Fatal(e)
		}
	}
	now := time.Now().Unix()
	if _, e = st.DB.Exec("INSERT INTO messages(ref,account_id,folder,uidvalidity,uid,subject,sender,sent_at,unread,size,received_at,flags_known,changed_at,metadata_version) VALUES('fixture',?,'INBOX',7,1,'bill','billing@example.test',?,0,100,?,1,?,1)", account.ID, now-100000, now-100000, now-600); e != nil {
		t.Fatal(e)
	}
	admin, _ := svc.AdminAccess()
	policy, _ := svc.AutomationPolicy(admin, account.ID)
	policy.Enabled = true
	policy.SourceFolders = []string{"INBOX"}
	policy.Destinations = []string{"Archive"}
	policy.Actions = []string{"move"}
	policy.IncludeHistory = true
	policy.Since = 0
	policy.StableSeconds = 0
	if _, e = svc.SaveAutomationPolicy(admin, policy); e != nil {
		t.Fatal(e)
	}
	_, secret, e := svc.CreateToken(mail.TokenInput{Name: "scheduled MCP", Accounts: []string{account.ID}, Actions: []string{"read", "automation", "rule_manage", "task_manage"}})
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(Handler(svc, "https://mail.test"))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{secret}}, DisableStandaloneSSE: true}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	call := func(name string, args any, wantError bool) *mcp.CallToolResult {
		t.Helper()
		r, e := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if e != nil || r.IsError != wantError {
			t.Fatalf("%s: %+v %v", name, r, e)
		}
		return r
	}
	rule := map[string]any{"account_id": account.ID, "name": "nested", "version": 0, "priority": 1, "enabled": true, "source_folders": []string{"INBOX"}, "match": map[string]any{"mode": "all", "conditions": []any{}, "groups": []any{map[string]any{"mode": "not", "conditions": []any{map[string]any{"field": "flagged", "operator": "equals", "value": "true"}}}}}, "action": "move", "destination": "Archive"}
	call("mail_rule_upsert", map[string]any{"rule": rule}, false)
	call("mail_search", map[string]any{"account_id": account.ID}, true)
	call("mail_get", map[string]any{"ref": "fixture", "body": true}, true)
	call("mail_plan", map[string]any{"refs": []string{"fixture"}, "action": "move", "destination": "Archive"}, true)
	r := call("mail_ai_claim", map[string]any{"account_id": account.ID, "run_key": "run-1"}, false)
	var batch map[string]any
	raw, _ := json.Marshal(r.StructuredContent)
	if json.Unmarshal(raw, &batch) != nil {
		t.Fatal(string(raw))
	}
	id, _ := batch["id"].(string)
	if id == "" {
		t.Fatal(batch)
	}
	call("mail_ai_get", map[string]any{"batch_id": id, "ref": "fixture", "body": true}, true)
	call("mail_ai_get", map[string]any{"batch_id": id, "ref": "fixture", "body": false}, false)
	items := batch["items"].([]any)
	item := items[0].(map[string]any)
	submitted := call("mail_ai_submit_decisions", map[string]any{"batch_id": id, "idempotency_key": "submit-1", "decisions": []any{map[string]any{"ref": "fixture", "signature": item["signature"], "action": "move", "destination": "Archive", "reason": "matches saved rule"}}}, false)
	raw, _ = json.Marshal(submitted.StructuredContent)
	if !strings.Contains(string(raw), `"state":"observed"`) {
		t.Fatal(string(raw))
	}
	runs := call("mail_ai_runs", map[string]any{"account_id": account.ID}, false)
	raw, _ = json.Marshal(runs.StructuredContent)
	if !strings.Contains(string(raw), "next_cursor") {
		t.Fatal("missing checkpoint", string(raw))
	}
	call("mail_ai_cancel", map[string]any{"batch_id": id}, false)
	// Task schemas travel through the real SDK HTTP validation, not direct service calls.
	taskResult := call("mail_task_upsert", map[string]any{"task": mail.Task{AccountID: account.ID, Name: "外部任务", Trigger: "external", Engine: "ai", SourceFolders: []string{"INBOX"}, MaxBatch: 20, Notify: "inherit"}}, false)
	var saved struct {
		Task mail.Task `json:"task"`
	}
	raw, _ = json.Marshal(taskResult.StructuredContent)
	if json.Unmarshal(raw, &saved) != nil || saved.Task.Enabled || saved.Task.Mode != "observe" {
		t.Fatal(string(raw))
	}
	call("mail_task_preview", map[string]any{"task_id": saved.Task.ID}, false)
	enabledResult := call("mail_task_set_enabled", map[string]any{"task_id": saved.Task.ID, "version": saved.Task.Version, "enabled": true}, false)
	raw, _ = json.Marshal(enabledResult.StructuredContent)
	if json.Unmarshal(raw, &saved) != nil || !saved.Task.Enabled {
		t.Fatal(string(raw))
	}
	runResult := call("mail_task_run", map[string]any{"task_id": saved.Task.ID, "version": saved.Task.Version, "run_key": "task-occurrence"}, false)
	var run struct {
		Run mail.TaskRun `json:"run"`
	}
	raw, _ = json.Marshal(runResult.StructuredContent)
	if json.Unmarshal(raw, &run) != nil || run.Run.State != "awaiting_ai" {
		t.Fatal(string(raw))
	}
	if _, e = st.DB.Exec("UPDATE mailboxes SET synced_at=?", time.Now().Unix()); e != nil {
		t.Fatal(e)
	}
	call("mail_ai_claim", map[string]any{"account_id": account.ID, "run_id": run.Run.ID, "run_key": "batch"}, false)
	call("mail_task_run_get", map[string]any{"run_id": run.Run.ID}, false)
	call("mail_task_runs", map[string]any{"task_id": saved.Task.ID}, false)
	call("mail_task_cancel", map[string]any{"run_id": run.Run.ID}, false)
}
