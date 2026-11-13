package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mail-auto/internal/agent"
	"mail-auto/internal/interaction"
	"mail-auto/internal/mail"
	"mail-auto/internal/tglimit"
)

func enqueueVersioned(t *testing.T, e *tgEnv, key, text string) interaction.Binding {
	t.Helper()
	b, err := interaction.ActiveBinding(e.f.Store.DB)
	if err != nil {
		t.Fatal(err)
	}
	if err = interaction.Enqueue(e.f.Store.DB, e.f.Vault, b, interaction.Reply{SemanticKey: key, Text: text, GrantVersion: interaction.CurrentGrantVersion(e.f.Store.DB, b.GrantID)}); err != nil {
		t.Fatal(err)
	}
	return b
}

func replyState(t *testing.T, e *tgEnv, key string) (string, string) {
	t.Helper()
	var state, code string
	if err := e.f.Store.DB.QueryRow("SELECT state,error_code FROM telegram_reply_outbox WHERE semantic_key=?", key).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	return state, code
}

// AT-AUD-02: replies produced under a grant are never delivered after the
// grant is disabled, revoked, expired or bound to an older administrator
// version; a change during the rate-limit wait is caught by the final check.
func TestOutboxFencesForEveryGrantChange(t *testing.T) {
	changes := map[string]func(t *testing.T, e *tgEnv){
		"disabled": func(t *testing.T, e *tgEnv) {
			g, _ := e.f.Service.CurrentAgentGrant(e.f.Admin)
			if _, err := e.f.Service.SaveAgentGrant(e.f.Admin, mail.AgentGrantInput{Version: g.Version, Enabled: false, Accounts: g.Accounts, Actions: g.Actions}); err != nil {
				t.Fatal(err)
			}
		},
		"revoked": func(t *testing.T, e *tgEnv) {
			if err := e.f.Service.RevokeAgentGrant(e.f.Admin); err != nil {
				t.Fatal(err)
			}
		},
		"admin changed": func(t *testing.T, e *tgEnv) {
			if _, err := e.f.Store.DB.Exec("UPDATE admin SET version=version+1"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			e := newTGEnv(t, nil)
			e.bind(t)
			enqueueVersioned(t, e, "old", "synthetic old-scope content")
			change(t, e)
			e.tg.SendOne(context.Background())
			if state, _ := replyState(t, e, "old"); state != "suppressed" {
				t.Fatal("old content delivered after", name, state)
			}
			for _, text := range e.bot.texts() {
				if strings.Contains(text, "synthetic old-scope") {
					t.Fatal("old content reached Telegram")
				}
			}
		})
	}
	// Grant narrowed while the sender waits for rate pacing.
	e := newTGEnv(t, nil)
	e.bind(t)
	enqueueVersioned(t, e, "racing", "synthetic content during wait")
	tglimit.Block(400 * time.Millisecond)
	done := make(chan struct{})
	go func() { e.tg.SendOne(context.Background()); close(done) }()
	time.Sleep(100 * time.Millisecond)
	g, _ := e.f.Service.CurrentAgentGrant(e.f.Admin)
	if _, err := e.f.Service.SaveAgentGrant(e.f.Admin, mail.AgentGrantInput{Version: g.Version, Enabled: true, Accounts: g.Accounts, Folders: map[string][]string{e.f.Account.ID: {"Archive"}}, Actions: []string{"read"}}); err != nil {
		t.Fatal(err)
	}
	<-done
	if state, _ := replyState(t, e, "racing"); state != "suppressed" {
		t.Fatal("grant change during the rate wait was not caught", state)
	}
	// The re-authorization notice (no mailbox data) is still delivered.
	for e.tg.SendOne(context.Background()) {
	}
	notice := false
	for _, text := range e.bot.texts() {
		notice = notice || strings.Contains(text, "授权已由管理员更新")
	}
	if !notice {
		t.Fatal("valid binding did not receive the re-authorization notice")
	}
}

type fullQueueAgent struct {
	*stubAgent
	stops int
}

func (a *fullQueueAgent) Submit(string, string, string) (int, error) {
	return 100, errors.New("agent_queue_full")
}
func (a *fullQueueAgent) Stop(string) string { a.stops++; return "stopped" }

// AT-AUD-06: control priority applies only to the bound sender in the
// current epoch, and natural-language events before /stop are dropped.
func TestControlPriorityOnlyForBoundSenderAndStopDropsEarlierWork(t *testing.T) {
	a := &fullQueueAgent{stubAgent: &stubAgent{}}
	e := newTGEnv(t, a)
	e.bind(t)
	now := time.Now().Unix()
	e.bot.push(textUpdate(1000, 42, 42, "private", "普通指令（队列已满）", now))
	e.bot.push(textUpdate(1001, 99, 99, "private", "/stop", now))
	e.bot.push(textUpdate(1002, 42, 42, "private", "/stop", now))
	e.bot.push(textUpdate(1003, 42, 42, "private", "停止之后的新指令", now))
	if _, err := e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var control int
	_ = e.f.Store.DB.QueryRow("SELECT control FROM telegram_inbox WHERE update_id=1001").Scan(&control)
	if control != 0 {
		t.Fatal("unbound sender's command got control priority")
	}
	for i := 0; i < 4; i++ {
		e.tg.ProcessOne(context.Background())
	}
	if a.stops != 1 {
		t.Fatal("bound /stop not dispatched promptly", a.stops)
	}
	states := map[int64]string{}
	rows, _ := e.f.Store.DB.Query("SELECT update_id,state FROM telegram_inbox WHERE update_id BETWEEN 1000 AND 1003")
	for rows.Next() {
		var id int64
		var st string
		_ = rows.Scan(&id, &st)
		states[id] = st
	}
	rows.Close()
	if states[1000] != "stale" || states[1001] != "rejected" || states[1002] != "processed" || states[1003] != "pending" {
		t.Fatal("unexpected inbox states", states)
	}
	// An old-epoch control event is not executed after a re-enable.
	if _, err := e.f.Store.DB.Exec("UPDATE telegram_inbox SET state='pending',epoch=epoch-1,control=1 WHERE update_id=1002"); err != nil {
		t.Fatal(err)
	}
	for e.tg.ProcessOne(context.Background()) {
	}
	if a.stops != 1 {
		t.Fatal("old-epoch control event executed")
	}
}

// AT-AUD-07: re-enabling keeps the binding usable for new commands, while
// inbox events and replies from before the switch-off stay dead.
func TestReenableRefreshesBindingWithoutRevivingOldWork(t *testing.T) {
	e := newTGEnv(t, nil)
	e.bind(t)
	enqueueVersioned(t, e, "before-off", "reply produced before switch-off")
	e.bot.push(textUpdate(1100, 42, 42, "private", "离线前的普通指令", time.Now().Unix()))
	if _, err := e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := mustSettings(t, e)
	if _, err := e.tg.SetEnabled(context.Background(), e.f.Admin, st.Version, false); err != nil {
		t.Fatal(err)
	}
	st = mustSettings(t, e)
	if _, err := e.tg.SetEnabled(context.Background(), e.f.Admin, st.Version, true); err != nil {
		t.Fatal(err)
	}
	st = mustSettings(t, e)
	b, err := interaction.ActiveBinding(e.f.Store.DB)
	if err != nil || b.Epoch != st.Epoch {
		t.Fatal("binding not carried into the new epoch", b.Epoch, st.Epoch, err)
	}
	e.bot.push(textUpdate(1101, 42, 42, "private", "/status", time.Now().Unix()))
	if _, err = e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if state, _ := replyState(t, e, "before-off"); state != "suppressed" {
		t.Fatal("pre-switch-off reply revived", state)
	}
	var oldState string
	_ = e.f.Store.DB.QueryRow("SELECT state FROM telegram_inbox WHERE update_id=1100").Scan(&oldState)
	if oldState != "stale" || len(e.agent.submitted) != 0 {
		t.Fatal("pre-switch-off inbox event executed", oldState, e.agent.submitted)
	}
	statusSent := false
	for _, text := range e.bot.texts() {
		statusSent = statusSent || strings.Contains(text, "agent status")
	}
	if !statusSent {
		t.Fatal("new /status after re-enable got no reply")
	}
}

// The /new acknowledgement (no mailbox data) is stamped with the context of
// the new session it announces, so its own reset does not suppress it; the
// grant version stays the pre-command snapshot.
func TestNewSessionAcknowledgementIsDelivered(t *testing.T) {
	e := newTGEnv(t, nil)
	e.bind(t)
	e.tg.Agent = agent.New(e.f.Store, e.f.Vault, e.f.Service)
	b, _ := interaction.ActiveBinding(e.f.Store.DB)
	if _, err := interaction.EnsureConversation(e.f.Store.DB, b.ID, interaction.CurrentGrantVersion(e.f.Store.DB, b.GrantID)); err != nil {
		t.Fatal(err)
	}
	e.bot.push(textUpdate(1200, 42, 42, "private", "/new", time.Now().Unix()))
	if _, err := e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	delivered := false
	for _, text := range e.bot.texts() {
		delivered = delivered || strings.Contains(text, "已开始新会话")
	}
	if !delivered {
		t.Fatal("/new acknowledgement not delivered")
	}
}

// /run is an execution intent: a stale one (unprocessed for more than
// TextMaxAge, e.g. while the service was offline) is not dispatched; a
// fresh one is.
func TestRunCommandMustBeFresh(t *testing.T) {
	e := newTGEnv(t, nil)
	e.bind(t)
	now := time.Now()
	if _, err := e.f.Store.DB.Exec("UPDATE telegram_interaction_settings SET activation_time=?", now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	e.bot.push(textUpdate(1300, 42, 42, "private", "/run abcdefgh", now.Add(-TextMaxAge-time.Minute).Unix()))
	if _, err := e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if e.agent.runs != 0 {
		t.Fatal("stale /run dispatched")
	}
	if !strings.Contains(strings.Join(e.bot.texts(), "\n"), "/run 指令已超过 10 分钟") {
		t.Fatal("stale /run not answered with a resend hint")
	}
	e.bot.push(textUpdate(1301, 42, 42, "private", "/run abcdefgh", now.Unix()))
	if _, err := e.tg.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if e.agent.runs != 1 {
		t.Fatal("fresh /run not dispatched", e.agent.runs)
	}
}
