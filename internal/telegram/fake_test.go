package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mail-auto/internal/agenttest"
	"mail-auto/internal/mail"
)

// fakeBot is an in-memory Bot API.
type fakeBot struct {
	mu          sync.Mutex
	botID       int64
	webhook     string
	updates     []map[string]any
	sent        []map[string]any
	edits       []map[string]any
	answers     []map[string]any
	calls       map[string]int
	conflict    bool
	rateLimit   int
	sendTimeout int
	nextMsg     int64
	offsets     []int64
	block       time.Duration
}

func (f *fakeBot) handler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	method := parts[len(parts)-1]
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.calls[method]++
	reply := func(result any) {
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}
	switch method {
	case "getMe":
		reply(map[string]any{"id": f.botID, "is_bot": true, "username": "mail_auto_test_bot"})
	case "getWebhookInfo":
		reply(map[string]any{"url": f.webhook})
	case "getUpdates":
		if f.conflict {
			f.mu.Unlock()
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 409, "description": "Conflict: terminated by other getUpdates request"})
			return
		}
		offset := int64(body["offset"].(float64))
		f.offsets = append(f.offsets, offset)
		out := []map[string]any{}
		for _, u := range f.updates {
			if int64(u["update_id"].(float64)) >= offset {
				out = append(out, u)
			}
		}
		reply(out)
	case "sendMessage", "editMessageText":
		if f.rateLimit > 0 {
			f.rateLimit--
			f.mu.Unlock()
			w.WriteHeader(429)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 429, "description": "Too Many Requests: bot123:SECRET", "parameters": map[string]any{"retry_after": 1}})
			return
		}
		if f.sendTimeout > 0 {
			f.sendTimeout--
			f.sent = append(f.sent, body)
			f.mu.Unlock()
			time.Sleep(f.block)
			return
		}
		if method == "editMessageText" {
			f.edits = append(f.edits, body)
			reply(true)
			return
		}
		f.nextMsg++
		f.sent = append(f.sent, body)
		reply(map[string]any{"message_id": f.nextMsg, "chat": map[string]any{"id": body["chat_id"], "type": "private"}, "date": time.Now().Unix()})
	case "answerCallbackQuery":
		f.answers = append(f.answers, body)
		reply(true)
	default:
		f.mu.Unlock()
		w.WriteHeader(404)
	}
}

func (f *fakeBot) push(u map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, u)
}

func (f *fakeBot) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, m := range f.sent {
		if t, ok := m["text"].(string); ok {
			out = append(out, t)
		}
	}
	return out
}

func textUpdate(id int64, user, chat int64, chatType, text string, date int64) map[string]any {
	return map[string]any{"update_id": float64(id), "message": map[string]any{"message_id": float64(id), "from": map[string]any{"id": float64(user), "is_bot": false, "first_name": "u"}, "chat": map[string]any{"id": float64(chat), "type": chatType}, "date": float64(date), "text": text}}
}

func callbackUpdate(id, user, chat, message int64, data string) map[string]any {
	return map[string]any{"update_id": float64(id), "callback_query": map[string]any{"id": "cb" + string(rune('a'+id%26)), "from": map[string]any{"id": float64(user), "is_bot": false, "first_name": "u"}, "message": map[string]any{"message_id": float64(message), "chat": map[string]any{"id": float64(chat), "type": "private"}, "date": float64(time.Now().Unix())}, "data": data}}
}

// stubAgent records calls; it never touches a model.
type stubAgent struct {
	mu        sync.Mutex
	submitted []string
	confirms  []string
	runs      int
}

func (a *stubAgent) Submit(bindingID, inputID, text string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.submitted = append(a.submitted, text)
	return 0, nil
}
func (a *stubAgent) Confirm(_ context.Context, bindingID string, messageID int64, data string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.confirms = append(a.confirms, data)
	return "ok"
}
func (a *stubAgent) Stop(string) string              { return "stopped" }
func (a *stubAgent) NewSession(string) string        { return "new" }
func (a *stubAgent) Status(string) string            { return "agent status" }
func (a *stubAgent) CancelRun(string, string) string { return "cancel" }
func (a *stubAgent) RunCommand(string, string, string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runs++
	return "run"
}

type tgEnv struct {
	f     *agenttest.Fixture
	bot   *fakeBot
	srv   *httptest.Server
	tg    *Service
	agent *stubAgent
}

const testToken = "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef" // gitleaks:allow -- synthetic test token

func newTGEnv(t *testing.T, agent Agent) *tgEnv {
	t.Helper()
	f := agenttest.New(t)
	bot := &fakeBot{botID: 777, calls: map[string]int{}, nextMsg: 1000}
	srv := httptest.NewServer(http.HandlerFunc(bot.handler))
	t.Cleanup(srv.Close)
	if _, err := f.Service.SaveNotifications(f.Admin, mail.NotificationInput{Policy: "failures", BotToken: testToken}); err != nil {
		t.Fatal(err)
	}
	stub, _ := agent.(*stubAgent)
	if agent == nil {
		stub = &stubAgent{}
		agent = stub
	}
	tg := New(f.Store, f.Vault, f.Service, agent)
	tg.PollTimeout = 0
	tg.NewClient = func(token string) *Client {
		c := NewClient(token)
		c.BaseURL = srv.URL
		c.HTTP.Timeout = 2 * time.Second
		return c
	}
	if _, err := tg.SetEnabled(context.Background(), f.Admin, 0, true); err != nil {
		t.Fatal(err)
	}
	return &tgEnv{f: f, bot: bot, srv: srv, tg: tg, agent: stub}
}

func (e *tgEnv) drain(t *testing.T) {
	t.Helper()
	for e.tg.ProcessOne(context.Background()) {
	}
	for e.tg.SendOne(context.Background()) {
	}
}

// bind creates a grant, a bind code and binds user 42 through /bind.
func (e *tgEnv) bind(t *testing.T, actions ...string) {
	t.Helper()
	if len(actions) == 0 {
		actions = []string{"read", "rule_manage", "task_manage", "automation", "task_run_manage"}
	}
	if _, err := e.f.Service.SaveAgentGrant(e.f.Admin, mail.AgentGrantInput{Enabled: true, Accounts: []string{e.f.Account.ID}, Actions: actions}); err != nil {
		t.Fatal(err)
	}
	code, _, err := e.tg.BindCode(e.f.Admin)
	if err != nil {
		t.Fatal(err)
	}
	e.bot.push(textUpdate(int64(len(e.bot.updates)+1), 42, 42, "private", "/bind "+code, time.Now().Unix()))
	if _, err = e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	var n int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM telegram_bindings WHERE state='active' AND user_id=42 AND chat_id=42").Scan(&n)
	if n != 1 {
		t.Fatal("binding not created", e.bot.texts())
	}
}
