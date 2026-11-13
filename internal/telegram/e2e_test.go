package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"mail-auto/internal/agent"
	"mail-auto/internal/modelprovider"
)

// scriptModel is a fake OpenAI-compatible model: it answers probes and
// follows the conversation by looking at the last tool result.
type scriptModel struct {
	mu   sync.Mutex
	f    func(req modelprovider.Request) modelprovider.Response
	reqs int
}

func (m *scriptModel) Complete(ctx context.Context, req modelprovider.Request) (modelprovider.Response, error) {
	if len(req.Tools) == 1 && req.Tools[0].Name == "echo" {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "tool" {
			return modelprovider.Response{Content: "ok"}, nil
		}
		i := strings.Index(last.Content, "probe-")
		args, _ := json.Marshal(map[string]string{"text": last.Content[i : i+18]})
		return modelprovider.Response{ToolCalls: []modelprovider.ToolCall{{ID: "p", Name: "echo", Arguments: string(args)}}}, nil
	}
	m.mu.Lock()
	m.reqs++
	f := m.f
	m.mu.Unlock()
	if f == nil {
		return modelprovider.Response{Content: "OK"}, nil
	}
	return f(req), nil
}

func tc(id, name string, args any) modelprovider.Response {
	raw, _ := json.Marshal(args)
	return modelprovider.Response{ToolCalls: []modelprovider.ToolCall{{ID: id, Name: name, Arguments: string(raw)}}}
}

func TestEndToEndTelegramAgentWorkflow(t *testing.T) {
	e := newTGEnv(t, &stubAgent{})
	rt := agent.New(e.f.Store, e.f.Vault, e.f.Service)
	e.tg.Agent = rt
	model := &scriptModel{}
	rt.NewProvider = func(modelprovider.Settings) (modelprovider.Provider, error) { return model, nil }
	p, err := rt.SaveProvider(e.f.Admin, agent.ProviderInput{BaseURL: "https://api.model.example/v1", Model: "fake", APIKey: "sk-e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if _, code, err := rt.CheckProvider(context.Background(), e.f.Admin, "chat"); err != nil || code != "ok" {
		t.Fatal(code, err)
	}
	if _, code, err := rt.CheckProvider(context.Background(), e.f.Admin, "tool_loop"); err != nil || code != "ok" {
		t.Fatal(code, err)
	}
	if _, err = rt.SaveProvider(e.f.Admin, agent.ProviderInput{Version: p.Version, Enabled: true, BaseURL: "https://api.model.example/v1", Model: "fake"}); err != nil {
		t.Fatal(err)
	}
	e.bind(t)
	var taskID string
	model.f = func(req modelprovider.Request) modelprovider.Response {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" {
			switch {
			case strings.Contains(last.Content, "回收站"):
				return tc("c1", "save_rule_draft", map[string]any{"account_id": e.f.Account.ID, "rule": map[string]any{"name": "测试发件人回收", "priority": 50, "source_folders": []string{"INBOX"}, "match": map[string]any{"mode": "all", "conditions": []any{map[string]any{"field": "sender", "operator": "contains", "value": "test@example.com"}}}, "action": "trash"}})
			case strings.Contains(last.Content, "立即执行"):
				return tc("r1", "run_task", map[string]any{"task_id": taskID})
			}
			return modelprovider.Response{Content: "请补充条件"}
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(last.Content), &out)
		if rule, ok := out["rule"].(map[string]any); ok && out["state"] == "draft_disabled" {
			return tc("c2", "propose_workflow", map[string]any{"rule_id": rule["id"], "rule_version": rule["version"], "mode": "execute", "enable_task": true, "task": map[string]any{"name": "每周五回收", "trigger_mode": "internal", "schedule": map[string]any{"frequency": "weekly", "weekday": 5, "hour": 20}}})
		}
		if out["status"] == "confirmation_required" {
			return modelprovider.Response{Content: "已生成方案，请在卡片中确认。"}
		}
		return modelprovider.Response{Content: "已处理：" + last.Content[:min(40, len(last.Content))]}
	}
	now := time.Now().Unix()
	e.bot.push(textUpdate(20, 42, 42, "private", "把 test@example.com 的邮件移到回收站，每周五 20 点执行", now))
	if _, err = e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	for rt.Step(context.Background()) {
	}
	e.drain(t)
	// Find the card and its accept button.
	var cardID int64
	var accept string
	e.bot.mu.Lock()
	for i, m := range e.bot.sent {
		if markup, ok := m["reply_markup"].(map[string]any); ok {
			row := markup["inline_keyboard"].([]any)[0].([]any)
			accept = row[0].(map[string]any)["callback_data"].(string)
			cardID = int64(1001 + i)
		}
	}
	e.bot.mu.Unlock()
	if accept == "" || !strings.Contains(strings.Join(e.bot.texts(), "\n"), "【待确认】") {
		t.Fatal("no card", e.bot.texts())
	}
	var tasks int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM automation_tasks").Scan(&tasks)
	if tasks != 0 {
		t.Fatal("task created before confirmation")
	}
	var stored int64
	_ = e.f.Store.DB.QueryRow("SELECT message_id FROM telegram_reply_outbox WHERE semantic_key LIKE 'proposal:%' AND semantic_key NOT LIKE '%:%:%' ORDER BY part DESC LIMIT 1").Scan(&stored)
	if stored != cardID {
		t.Fatal("card message id not recorded", stored, cardID)
	}
	// Double click: two callback updates with the same button.
	e.bot.push(callbackUpdate(21, 42, 42, cardID, accept))
	e.bot.push(callbackUpdate(22, 42, 42, cardID, accept))
	if _, err = e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	all, _ := e.f.Service.Tasks(e.f.Admin, e.f.Account.ID)
	if len(all) != 1 || !all[0].Enabled || all[0].Mode != "execute" {
		t.Fatal("confirmed workflow state", all)
	}
	taskID = all[0].ID
	if len(e.bot.answers) != 2 || !strings.Contains(e.bot.answers[1]["text"].(string), "已处理") {
		t.Fatal("double click answers", e.bot.answers)
	}
	joined := strings.Join(e.bot.texts(), "\n")
	if !strings.Contains(joined, "方案已全部执行") {
		t.Fatal("no result message", joined)
	}
	// Explicit run request; the same update redelivered creates no second run.
	// A natural-language execution request only yields a confirmation card.
	e.bot.push(textUpdate(23, 42, 42, "private", "立即执行刚才的任务", time.Now().Unix()))
	if _, err = e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	for rt.Step(context.Background()) {
	}
	e.drain(t)
	var runs int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM task_runs").Scan(&runs)
	if runs != 0 {
		t.Fatal("natural-language request queued a run without confirmation", runs)
	}
	// The deterministic /run command is the user's explicit execution
	// intent; the same update redelivered does not create a second run.
	run := textUpdate(24, 42, 42, "private", "/run "+taskID[:8], time.Now().Unix())
	e.bot.push(run)
	if _, err = e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = e.tg.Ingest(mustSettings(t, e), credGen(t, e), lease(t, e), []json.RawMessage{mustRaw(run)}); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM task_runs").Scan(&runs)
	if runs != 1 {
		t.Fatal("runs", runs)
	}
	var link int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM agent_run_links").Scan(&link)
	if link != 1 || !strings.Contains(strings.Join(e.bot.texts(), "\n"), "已按 /run 指令入队一次运行") {
		t.Fatal("run not linked or reported", link)
	}
}
