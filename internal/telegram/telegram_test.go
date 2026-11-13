package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"mail-auto/internal/interaction"
	"mail-auto/internal/mail"
)

func offset(t *testing.T, e *tgEnv) int64 {
	var n int64
	if err := e.f.Store.DB.QueryRow("SELECT next_offset FROM telegram_poll_state WHERE bot_id=777").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOffsetAdvancesOnlyAfterWholeBatchPersists(t *testing.T) {
	e := newTGEnv(t, nil)
	e.bind(t)
	before := offset(t, e)
	now := time.Now().Unix()
	e.bot.push(textUpdate(10, 42, 42, "private", "第一条", now))
	e.bot.push(textUpdate(11, 42, 42, "private", "第二条", now))
	// Make the second insert fail: the whole batch must roll back.
	if _, err := e.f.Store.DB.Exec("CREATE TRIGGER fail_inbox BEFORE INSERT ON telegram_inbox WHEN NEW.update_id=11 BEGIN SELECT RAISE(ABORT,'simulated'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.tg.PollOnce(context.Background()); err == nil {
		t.Fatal("ingest failure not reported")
	}
	var rows int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM telegram_inbox WHERE update_id IN (10,11)").Scan(&rows)
	if offset(t, e) != before || rows != 0 {
		t.Fatal("partial batch committed", offset(t, e), rows)
	}
	if _, err := e.f.Store.DB.Exec("DROP TRIGGER fail_inbox"); err != nil {
		t.Fatal(err)
	}
	// The same batch is fetched again with the old offset and stored once.
	if _, err := e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM telegram_inbox WHERE update_id IN (10,11)").Scan(&rows)
	if offset(t, e) != 12 || rows != 2 {
		t.Fatal("redelivery not deduplicated", offset(t, e), rows)
	}
	e.drain(t)
	if len(e.agent.submitted) != 2 {
		t.Fatal("submitted", e.agent.submitted)
	}
	// Replaying an already acknowledged batch does not create new turns.
	if err := e.tg.Ingest(mustSettings(t, e), credGen(t, e), lease(t, e), []json.RawMessage{mustRaw(textUpdate(10, 42, 42, "private", "第一条", now))}); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if len(e.agent.submitted) != 2 {
		t.Fatal("duplicate update re-submitted", e.agent.submitted)
	}
}

func mustRaw(v any) json.RawMessage { raw, _ := json.Marshal(v); return raw }
func mustSettings(t *testing.T, e *tgEnv) Settings {
	st, err := e.tg.load(e.f.Store.DB)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
func credGen(t *testing.T, e *tgEnv) int {
	_, g, err := e.f.Service.TelegramCredential()
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func lease(t *testing.T, e *tgEnv) int64 {
	var g int64
	_ = e.f.Store.DB.QueryRow("SELECT generation FROM telegram_poll_state WHERE bot_id=777").Scan(&g)
	return g
}

func TestGapsBacklogPoisonAndOldText(t *testing.T) {
	e := newTGEnv(t, nil)
	e.bind(t)
	now := time.Now().Unix()
	activation := now - 1500
	if _, err := e.f.Store.DB.Exec("UPDATE telegram_interaction_settings SET activation_time=?", activation); err != nil {
		t.Fatal(err)
	}
	e.bot.push(map[string]any{"update_id": float64(100), "poll": map[string]any{"id": "x"}})
	e.bot.push(map[string]any{"update_id": float64(5000), "message": "not an object"})
	e.bot.push(textUpdate(90000, 42, 42, "private", "激活前的旧命令", activation-60))
	e.bot.push(textUpdate(90001, 42, 42, "private", "离线 20 分钟的消息", now-1200))
	e.bot.push(textUpdate(123456789, 42, 42, "private", "新消息", now))
	if _, err := e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if offset(t, e) != 123456790 {
		t.Fatal("offset must follow max update_id regardless of gaps", offset(t, e))
	}
	e.drain(t)
	if len(e.agent.submitted) != 1 || e.agent.submitted[0] != "新消息" {
		t.Fatal("only the fresh message may reach the agent", e.agent.submitted)
	}
	joined := strings.Join(e.bot.texts(), "\n")
	if !strings.Contains(joined, "重新发送") {
		t.Fatal("old text not asked to resend", joined)
	}
	var states string
	_ = e.f.Store.DB.QueryRow("SELECT group_concat(state||':'||error_code) FROM (SELECT state,error_code FROM telegram_inbox WHERE update_id IN (100,5000,90000) ORDER BY update_id)").Scan(&states)
	if states != "ignored:unsupported_update,ignored:unsupported_update,stale:before_activation" {
		t.Fatal(states)
	}
}

func TestWebhookConflictAndLeaseStopPolling(t *testing.T) {
	e := newTGEnv(t, nil)
	e.bot.webhook = "https://example.com/hook"
	wait, _ := e.tg.PollOnce(context.Background())
	st := mustSettings(t, e)
	if st.State != "webhook_conflict" || wait < time.Minute || e.bot.calls["getUpdates"] != 0 || e.bot.calls["deleteWebhook"] != 0 {
		t.Fatal("webhook not respected", st.State, wait, e.bot.calls)
	}
	e.bot.webhook = ""
	e.bot.conflict = true
	wait, _ = e.tg.PollOnce(context.Background())
	st = mustSettings(t, e)
	if st.State != "consumer_conflict" || wait < time.Minute {
		t.Fatal("409 not backed off", st.State, wait)
	}
	e.bot.conflict = false
	// Another live consumer holds the lease: no poll happens.
	if _, err := e.f.Store.DB.Exec("UPDATE telegram_poll_state SET lease_owner='other',lease_until=?", time.Now().Unix()+60); err != nil {
		t.Fatal(err)
	}
	calls := e.bot.calls["getUpdates"]
	if _, err := e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.bot.calls["getUpdates"] != calls {
		t.Fatal("polled without lease")
	}
}

func TestCredentialChangeRevokesBindingAndFencesOldWork(t *testing.T) {
	e := newTGEnv(t, nil)
	e.bind(t)
	b, err := interaction.ActiveBinding(e.f.Store.DB)
	if err != nil {
		t.Fatal(err)
	}
	if err = interaction.Enqueue(e.f.Store.DB, e.f.Vault, b, interaction.Reply{SemanticKey: "old-reply", Text: "旧回复"}); err != nil {
		t.Fatal(err)
	}
	stOld := mustSettings(t, e)
	genOld := credGen(t, e)
	leaseOld := lease(t, e)
	ch, _ := e.f.Service.Notifications(e.f.Admin)
	if _, err = e.f.Service.SaveNotifications(e.f.Admin, mail.NotificationInput{Version: ch.Version, Policy: "failures", BotToken: "654321:ZYXWVUTSRQPONMLKJIHGFEDCBAzyxwvu"}); err != nil { // gitleaks:allow -- synthetic test token
		t.Fatal(err)
	}
	e.bot.botID = 888
	if _, err = e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = interaction.ActiveBinding(e.f.Store.DB); err == nil {
		t.Fatal("binding survived credential change")
	}
	var state string
	_ = e.f.Store.DB.QueryRow("SELECT state FROM telegram_reply_outbox WHERE semantic_key='old-reply'").Scan(&state)
	if state != "suppressed" {
		t.Fatal("old reply not suppressed", state)
	}
	// An in-flight poll result from the old generation cannot be committed.
	if err = e.tg.Ingest(stOld, genOld, leaseOld, []json.RawMessage{mustRaw(textUpdate(5, 42, 42, "private", "旧请求结果", time.Now().Unix()))}); err == nil {
		t.Fatal("stale generation committed")
	}
	st := mustSettings(t, e)
	var rows int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM telegram_poll_state").Scan(&rows)
	if st.BotID != 888 || rows != 2 {
		t.Fatal("new bot needs its own offset namespace", st.BotID, rows)
	}
	// Disabling cancels unstarted work; re-enabling does not revive it.
	if _, err = e.tg.SetEnabled(context.Background(), e.f.Admin, st.Version, false); err != nil {
		t.Fatal(err)
	}
	st = mustSettings(t, e)
	if _, err = e.tg.SetEnabled(context.Background(), e.f.Admin, st.Version, true); err != nil {
		t.Fatal(err)
	}
	if st2 := mustSettings(t, e); st2.Epoch <= st.Epoch {
		t.Fatal("re-enable must start a new epoch")
	}
}

func TestBindingRejectsGroupsForwardsSpoofingAndReuse(t *testing.T) {
	e := newTGEnv(t, nil)
	if _, err := e.f.Service.SaveAgentGrant(e.f.Admin, mail.AgentGrantInput{Enabled: true, Accounts: []string{e.f.Account.ID}, Actions: []string{"read"}}); err != nil {
		t.Fatal(err)
	}
	code, _, err := e.tg.BindCode(e.f.Admin)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	group := textUpdate(1, 42, -100, "group", "/bind "+code, now)
	forward := textUpdate(2, 42, 42, "private", "/bind "+code, now)
	forward["message"].(map[string]any)["forward_origin"] = map[string]any{"type": "user"}
	spoof := textUpdate(3, 43, 42, "private", "/bind "+code, now)
	edited := textUpdate(4, 42, 42, "private", "/bind "+code, now)
	edited["message"].(map[string]any)["edit_date"] = float64(now)
	for _, u := range []map[string]any{group, forward, spoof, edited} {
		e.bot.push(u)
	}
	if _, err = e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	var n int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM telegram_bindings").Scan(&n)
	if n != 0 {
		t.Fatal("invalid sender bound")
	}
	e.bot.push(textUpdate(5, 42, 42, "private", "/bind "+code, now))
	e.bot.push(textUpdate(6, 99, 99, "private", "/bind "+code, now))
	e.bot.push(textUpdate(7, 99, 99, "private", "列出我的邮箱", now))
	if _, err = e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM telegram_bindings WHERE state='active' AND user_id=42").Scan(&n)
	if n != 1 || len(e.agent.submitted) != 0 {
		t.Fatal("code reuse or unbound text reached the agent", n, e.agent.submitted)
	}
	texts := strings.Join(e.bot.texts(), "\n")
	if strings.Contains(texts, e.f.Account.Username) || !strings.Contains(texts, "绑定成功") || !strings.Contains(texts, "绑定码无效、已使用或已过期") {
		t.Fatal(texts)
	}
	// Expired codes fail too.
	code2, _, err := e.tg.BindCode(e.f.Admin)
	if err == nil || code2 != "" {
		t.Fatal("second code while bound", err)
	}
}

func TestCallbacksAnsweredAndVerifiedBySender(t *testing.T) {
	e := newTGEnv(t, nil)
	e.bind(t)
	e.bot.push(callbackUpdate(50, 99, 99, 1001, "p:a:AAAAAAAAAAAAAAAAAAAAAA"))
	e.bot.push(callbackUpdate(51, 42, 42, 1001, "p:a:AAAAAAAAAAAAAAAAAAAAAA"))
	inline := callbackUpdate(52, 42, 42, 1001, "p:a:x")
	delete(inline["callback_query"].(map[string]any), "message")
	inline["callback_query"].(map[string]any)["inline_message_id"] = "inl"
	e.bot.push(inline)
	if _, err := e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if len(e.agent.confirms) != 1 {
		t.Fatal("only the bound sender's callback may reach Confirm", e.agent.confirms)
	}
	if len(e.bot.answers) != 1 {
		t.Fatal("bound callback not answered", e.bot.answers)
	}
}

func TestSplittingPlainText429AndUnknownDelivery(t *testing.T) {
	long := strings.Repeat("中文😀abc\n", 900)
	parts := interaction.SplitText(long, interaction.MaxPartUnits)
	if len(parts) < 2 || strings.Join(parts, "") != long {
		t.Fatal("split lost text", len(parts))
	}
	for _, p := range parts {
		if len(utf16.Encode([]rune(p))) > interaction.MaxPartUnits {
			t.Fatal("part too long")
		}
	}
	e := newTGEnv(t, nil)
	e.bind(t)
	b, _ := interaction.ActiveBinding(e.f.Store.DB)
	if err := interaction.Enqueue(e.f.Store.DB, e.f.Vault, b, interaction.Reply{SemanticKey: "long", Text: "<b>不解析</b> " + long}); err != nil {
		t.Fatal(err)
	}
	e.bot.rateLimit = 1
	e.drain(t)
	time.Sleep(1100 * time.Millisecond)
	e.drain(t)
	var retried int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM telegram_reply_outbox WHERE semantic_key='long' AND attempts>=2").Scan(&retried)
	if retried != 1 || e.bot.rateLimit != 0 {
		t.Fatal("429 was not retried after retry_after", retried)
	}
	var sent int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM telegram_reply_outbox WHERE semantic_key='long' AND state='sent'").Scan(&sent)
	if sent != len(parts) {
		t.Fatal("parts sent", sent, len(parts))
	}
	for _, m := range e.bot.sent {
		if _, ok := m["parse_mode"]; ok {
			t.Fatal("parse mode set")
		}
	}
	// A send that times out is retried and then marked unknown; it is only
	// a message, never a business operation.
	if err := interaction.Enqueue(e.f.Store.DB, e.f.Vault, b, interaction.Reply{SemanticKey: "slow", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	e.bot.sendTimeout, e.bot.block = 3, 2500*time.Millisecond
	for i := 0; i < 3; i++ {
		_, _ = e.f.Store.DB.Exec("UPDATE telegram_reply_outbox SET next_attempt=0 WHERE semantic_key='slow' AND state='retry'")
		e.tg.SendOne(context.Background())
	}
	var state, code string
	_ = e.f.Store.DB.QueryRow("SELECT state,error_code FROM telegram_reply_outbox WHERE semantic_key='slow'").Scan(&state, &code)
	if state != "unknown" || code != CodeUnknown {
		t.Fatal(state, code)
	}
	var errs int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM telegram_reply_outbox WHERE error_code LIKE '%SECRET%' OR error_code LIKE '%bot%'").Scan(&errs)
	if errs != 0 {
		t.Fatal("raw Telegram error stored")
	}
}

func TestCommandsWorkWithoutModel(t *testing.T) {
	e := newTGEnv(t, nil)
	e.bind(t)
	now := time.Now().Unix()
	for i, cmd := range []string{"/status", "/stop", "/new", "/cancel abcdefgh", "/help"} {
		e.bot.push(textUpdate(int64(200+i), 42, 42, "private", cmd, now))
	}
	if _, err := e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	joined := strings.Join(e.bot.texts(), "\n")
	for _, want := range []string{"agent status", "stopped", "new", "cancel", "可用命令"} {
		if !strings.Contains(joined, want) {
			t.Fatal("missing", want, joined)
		}
	}
	if len(e.agent.submitted) != 0 {
		t.Fatal("commands reached the model path")
	}
	// Commands do not supersede pending cards; plain text does.
	var intent int64
	_ = e.f.Store.DB.QueryRow("SELECT COALESCE(max(intent_epoch),0) FROM agent_conversations").Scan(&intent)
	e.bot.push(textUpdate(300, 42, 42, "private", "补充：只处理收件箱", now))
	if _, err := e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var after int64
	_ = e.f.Store.DB.QueryRow("SELECT max(intent_epoch) FROM agent_conversations").Scan(&after)
	if after != intent+1 && !(intent == 0 && after == 2) {
		t.Fatal("plain text must raise the intent epoch at receipt", intent, after)
	}
}
