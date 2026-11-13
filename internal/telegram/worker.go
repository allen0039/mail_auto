package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"mail-auto/internal/credentials"
	"mail-auto/internal/interaction"
	"mail-auto/internal/tglimit"
)

// MaxTextBytes bounds stored message text.
const MaxTextBytes = 4096

// TextMaxAge is how old a plain-text instruction may be before it is only
// answered with a request to resend.
const TextMaxAge = 10 * time.Minute

func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) restartPoll() {
	s.pollMu.Lock()
	if s.pollCancel != nil {
		s.pollCancel()
	}
	s.pollMu.Unlock()
	s.notify()
}

// Start runs the poller, the inbox processor and the reply sender until ctx
// is cancelled. Long polls use ctx, so shutdown never waits on the network.
func (s *Service) Start(ctx context.Context) {
	s.wg.Add(3)
	go func() {
		defer s.wg.Done()
		s.pollLoop(ctx)
	}()
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			for s.ProcessOne(ctx) {
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			for s.SendOne(ctx) {
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) setState(state, code string) {
	_, _ = s.Store.DB.Exec("UPDATE telegram_interaction_settings SET state=?,error_code=?,updated_at=? WHERE id=1 AND (state<>? OR error_code<>?)", state, code, time.Now().Unix(), state, code)
}

func (s *Service) pollLoop(ctx context.Context) {
	backoff := time.Duration(0)
	for ctx.Err() == nil {
		if backoff > 0 {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-time.After(backoff):
			}
		}
		pctx, cancel := context.WithCancel(ctx)
		s.pollMu.Lock()
		s.pollCancel = cancel
		s.pollMu.Unlock()
		wait, err := s.PollOnce(pctx)
		cancel()
		backoff = wait
		if err != nil && ctx.Err() == nil {
			slog.Warn("telegram poll", "error_code", codeOf(err))
		}
	}
}

// PollOnce performs one guarded getUpdates cycle and returns how long to
// wait before the next one.
func (s *Service) PollOnce(ctx context.Context) (time.Duration, error) {
	st, err := s.load(s.Store.DB)
	if err != nil || !st.Enabled || st.BotID == 0 {
		return 30 * time.Second, err
	}
	token, gen, err := s.Mail.TelegramCredential()
	if err != nil {
		s.setState("credential_missing", "credential_unavailable")
		return 30 * time.Second, nil
	}
	if gen != st.credGen {
		return s.credentialChanged(ctx, st, token, gen)
	}
	lease, err := s.acquireLease(st.BotID)
	if err != nil {
		s.setState("consumer_conflict", "lease_held")
		return 30 * time.Second, nil
	}
	client := s.NewClient(token)
	if st.State == "starting" || st.State == "webhook_conflict" || st.State == "consumer_conflict" {
		hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		url, err := client.WebhookURL(hctx)
		cancel()
		if err != nil {
			s.setState("error", codeOf(err))
			return 30 * time.Second, err
		}
		if url != "" {
			// Never delete a webhook or drop pending updates silently.
			s.setState("webhook_conflict", "webhook_configured")
			return 5 * time.Minute, nil
		}
	}
	var offset int64
	if err = s.Store.DB.QueryRow("SELECT next_offset FROM telegram_poll_state WHERE bot_id=?", st.BotID).Scan(&offset); err != nil {
		return 10 * time.Second, err
	}
	pctx, cancel := context.WithTimeout(ctx, time.Duration(s.PollTimeout+10)*time.Second)
	raw, err := client.GetUpdates(pctx, offset, s.PollTimeout)
	cancel()
	if ctx.Err() != nil {
		return 0, nil
	}
	if err != nil {
		code := codeOf(err)
		switch code {
		case CodeConflict:
			s.setState("consumer_conflict", code)
			return 5 * time.Minute, err
		case CodeTokenBad:
			s.setState("token_invalid", code)
			return 5 * time.Minute, err
		case CodeRateLimited:
			var ae *APIError
			errors.As(err, &ae)
			return ae.RetryAfter, err
		}
		s.setState("error", code)
		return 10 * time.Second, err
	}
	if err = s.Ingest(st, gen, lease, raw); err != nil {
		return 5 * time.Second, err
	}
	s.setState("online", "")
	return 0, nil
}

// credentialChanged handles a replaced or cleared token: the old consumer
// stops, the binding and its proposals are revoked (conservative re-bind),
// and a different bot gets its own offset namespace.
func (s *Service) credentialChanged(ctx context.Context, st Settings, token string, gen int) (time.Duration, error) {
	gctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	bot, err := s.NewClient(token).GetMe(gctx)
	cancel()
	if err != nil {
		s.setState("token_invalid", codeOf(err))
		return time.Minute, err
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	tx, err := s.Store.DB.Begin()
	if err != nil {
		return 10 * time.Second, err
	}
	defer tx.Rollback()
	if err = revokeBindings(tx, "credential_changed"); err != nil {
		return 10 * time.Second, err
	}
	res, err := tx.Exec("UPDATE telegram_interaction_settings SET bot_id=?,bot_username=?,credential_generation=?,epoch=epoch+1,activation_time=?,state='starting',error_code='',updated_at=? WHERE id=1 AND credential_generation=? AND version=?", bot.ID, bot.Username, gen, time.Now().Unix(), time.Now().Unix(), st.credGen, st.Version)
	if err != nil {
		return 10 * time.Second, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, nil
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO telegram_poll_state(bot_id) VALUES(?)", bot.ID); err != nil {
		return 10 * time.Second, err
	}
	return 0, tx.Commit()
}

// acquireLease takes the per-bot poller lease and returns its fencing
// generation. Another live consumer on the same database keeps it.
func (s *Service) acquireLease(botID int64) (int64, error) {
	now := time.Now().Unix()
	if _, err := s.Store.DB.Exec("INSERT OR IGNORE INTO telegram_poll_state(bot_id) VALUES(?)", botID); err != nil {
		return 0, err
	}
	var owner string
	var gen int64
	if err := s.Store.DB.QueryRow("SELECT lease_owner,generation FROM telegram_poll_state WHERE bot_id=?", botID).Scan(&owner, &gen); err != nil {
		return 0, err
	}
	if owner == s.workerID {
		_, err := s.Store.DB.Exec("UPDATE telegram_poll_state SET lease_until=? WHERE bot_id=? AND lease_owner=? AND generation=?", now+90, botID, s.workerID, gen)
		return gen, err
	}
	res, err := s.Store.DB.Exec("UPDATE telegram_poll_state SET lease_owner=?,lease_until=?,generation=generation+1 WHERE bot_id=? AND (lease_owner='' OR lease_until<?)", s.workerID, now+90, botID, now)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, errors.New("lease_held")
	}
	err = s.Store.DB.QueryRow("SELECT generation FROM telegram_poll_state WHERE bot_id=?", botID).Scan(&gen)
	return gen, err
}

// ReleaseLease frees the poller lease on shutdown.
func (s *Service) ReleaseLease() {
	_, _ = s.Store.DB.Exec("UPDATE telegram_poll_state SET lease_owner='',lease_until=0 WHERE lease_owner=?", s.workerID)
}

type inboxPayload struct {
	Text       string `json:"text,omitempty"`
	Data       string `json:"data,omitempty"`
	CallbackID string `json:"callback_id,omitempty"`
}

// Ingest stores one getUpdates batch atomically: each update becomes an
// inbox event or a minimal rejected tombstone, and next_offset advances to
// max(update_id)+1 in the same transaction, fenced by the lease generation,
// interaction version/epoch and credential generation. If anything fails,
// nothing commits and the offset does not move, so the next getUpdates
// returns the same batch and UNIQUE(bot_id,update_id) drops duplicates.
func (s *Service) Ingest(st Settings, credGen int, lease int64, raw []json.RawMessage) error {
	if len(raw) > 100 {
		return &APIError{Code: CodeInvalid}
	}
	type event struct {
		id      int64
		kind    string
		state   string
		code    string
		user    int64
		chat    int64
		message int64
		date    int64
		payload *inboxPayload
		text    bool
	}
	events := []event{}
	var maxID int64 = -1
	for _, r := range raw {
		var head struct {
			UpdateID *int64 `json:"update_id"`
		}
		if json.Unmarshal(r, &head) != nil || head.UpdateID == nil {
			return &APIError{Code: CodeInvalid}
		}
		var u Update
		ev := event{id: *head.UpdateID, kind: "other", state: "ignored", code: "unsupported_update"}
		if json.Unmarshal(r, &u) == nil {
			switch {
			case u.Message != nil:
				m := u.Message
				ev.kind, ev.chat, ev.message, ev.date = "message", m.Chat.ID, m.MessageID, m.Date
				if m.From != nil {
					ev.user = m.From.ID
				}
				switch {
				case m.Chat.Type != "private":
					ev.code = "not_private"
				case m.From == nil || m.From.IsBot || m.SenderChat != nil || m.From.ID != m.Chat.ID:
					ev.code = "sender_invalid"
				case len(m.ForwardOrigin) > 0 || m.ForwardFrom != nil || m.ForwardDate != 0 || m.ViaBot != nil:
					ev.code = "forwarded"
				case m.EditDate != 0:
					ev.code = "edited"
				case m.Text == "":
					ev.code = "unsupported_content"
				case len(m.Text) > MaxTextBytes || !utf8.ValidString(m.Text):
					ev.code = "text_too_large"
				default:
					ev.state, ev.code, ev.payload = "pending", "", &inboxPayload{Text: m.Text}
					ev.text = !strings.HasPrefix(m.Text, "/")
				}
			case u.CallbackQuery != nil:
				q := u.CallbackQuery
				ev.kind, ev.user = "callback", q.From.ID
				switch {
				case q.Message == nil || q.InlineMessageID != "":
					ev.code = "callback_without_message"
				case q.Message.Chat.Type != "private" || q.Message.Chat.ID != q.From.ID || q.From.IsBot:
					ev.code = "sender_invalid"
				case q.Data == "" || len(q.Data) > 64:
					ev.code = "callback_invalid"
				default:
					ev.chat, ev.message = q.Message.Chat.ID, q.Message.MessageID
					ev.state, ev.code, ev.payload = "pending", "", &inboxPayload{Data: q.Data, CallbackID: q.ID}
				}
			}
		}
		if ev.id > maxID {
			maxID = ev.id
		}
		events = append(events, ev)
	}
	if len(events) == 0 {
		_, err := s.Store.DB.Exec("UPDATE telegram_poll_state SET last_poll_at=? WHERE bot_id=? AND generation=?", time.Now().Unix(), st.BotID, lease)
		return err
	}
	binding, berr := interaction.ActiveBinding(s.Store.DB)
	var grantVersion int
	if berr == nil {
		_ = s.Store.DB.QueryRow("SELECT version FROM agent_grants WHERE id=?", binding.GrantID).Scan(&grantVersion)
	}
	tx, err := s.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ok int
	if err = tx.QueryRow("SELECT count(*) FROM telegram_interaction_settings s JOIN telegram_poll_state p ON p.bot_id=s.bot_id WHERE s.id=1 AND s.enabled=1 AND s.version=? AND s.epoch=? AND s.credential_generation=? AND p.bot_id=? AND p.generation=? AND p.lease_owner=?", st.Version, st.Epoch, credGen, st.BotID, lease, s.workerID).Scan(&ok); err != nil {
		return err
	}
	if ok != 1 {
		// A configuration, credential or lease change happened while the
		// request was in flight; its result must not enter the new state.
		return errors.New("stale_poll_generation")
	}
	now := time.Now().Unix()
	for _, ev := range events {
		bindingID := ""
		if ev.state == "pending" && berr == nil && binding.BotID == st.BotID && binding.UserID == ev.user && binding.ChatID == ev.chat {
			bindingID = binding.ID
		}
		var sealed []byte
		id := credentials.Random(18)
		if ev.payload != nil {
			raw, _ := json.Marshal(ev.payload)
			if sealed, err = s.Vault.Encrypt(string(raw), "tg-inbox:"+id); err != nil {
				return err
			}
		}
		res, err := tx.Exec("INSERT OR IGNORE INTO telegram_inbox(id,bot_id,update_id,epoch,binding_id,kind,user_id,chat_id,message_id,payload,received_at,event_at,state,error_code,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", id, st.BotID, ev.id, st.Epoch, bindingID, ev.kind, ev.user, ev.chat, ev.message, sealed, now, ev.date, ev.state, ev.code, now)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		// A new plain-text instruction from the bound user immediately blocks
		// confirmation of older cards, before it is even parsed.
		if n == 1 && bindingID != "" && ev.text {
			conv, err := interaction.EnsureConversation(tx, bindingID, grantVersion)
			if err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE agent_conversations SET intent_epoch=intent_epoch+1,updated_at=? WHERE id=?", now, conv.ID); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec("UPDATE telegram_poll_state SET next_offset=max(next_offset,?),last_poll_at=? WHERE bot_id=? AND generation=?", maxID+1, now, st.BotID, lease); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

type inboxRow struct {
	ID        string
	BotID     int64
	UpdateID  int64
	Epoch     int64
	BindingID string
	Kind      string
	UserID    int64
	ChatID    int64
	MessageID int64
	Payload   []byte
	EventAt   int64
}

func (s *Service) finishInbox(id, state, code string) {
	_, _ = s.Store.DB.Exec("UPDATE telegram_inbox SET state=?,error_code=?,payload=NULL,updated_at=? WHERE id=? AND state='pending'", state, code, time.Now().Unix(), id)
}

// ProcessOne handles the oldest pending inbox event. Commands, binding and
// callbacks are deterministic; only plain text from the bound user becomes
// an agent turn. Unbound input never reaches the model.
func (s *Service) ProcessOne(ctx context.Context) bool {
	var r inboxRow
	err := s.Store.DB.QueryRow("SELECT id,bot_id,update_id,epoch,binding_id,kind,user_id,chat_id,message_id,payload,event_at FROM telegram_inbox WHERE state='pending' ORDER BY received_at,update_id LIMIT 1").Scan(&r.ID, &r.BotID, &r.UpdateID, &r.Epoch, &r.BindingID, &r.Kind, &r.UserID, &r.ChatID, &r.MessageID, &r.Payload, &r.EventAt)
	if err != nil {
		return false
	}
	st, err := s.load(s.Store.DB)
	if err != nil {
		return false
	}
	if !st.Enabled || r.Epoch != st.Epoch || r.BotID != st.BotID {
		s.finishInbox(r.ID, "stale", "old_epoch")
		return true
	}
	var p inboxPayload
	plain, err := s.Vault.Decrypt(r.Payload, "tg-inbox:"+r.ID)
	if err != nil || json.Unmarshal([]byte(plain), &p) != nil {
		s.finishInbox(r.ID, "failed", "payload_unavailable")
		return true
	}
	if r.Kind == "message" && r.EventAt < st.ActivationTime {
		// Backlog from before activation never triggers management actions.
		s.finishInbox(r.ID, "stale", "before_activation")
		return true
	}
	if r.BindingID == "" {
		s.handleUnbound(ctx, st, r, p)
		return true
	}
	b, err := interaction.BindingByID(s.Store.DB, r.BindingID)
	if err != nil || b.State != "active" || b.UserID != r.UserID || b.ChatID != r.ChatID || b.BotID != r.BotID {
		s.finishInbox(r.ID, "rejected", "binding_required")
		return true
	}
	if r.Kind == "callback" {
		s.handleCallback(ctx, st, b, r, p)
		return true
	}
	cmd, arg := validCommand(p.Text)
	reply := func(text string) {
		_ = interaction.Enqueue(s.Store.DB, s.Vault, b, interaction.Reply{SemanticKey: "inbox:" + r.ID, Text: text, ContextEpoch: s.contextEpoch(b.ID)})
	}
	switch cmd {
	case "/help", "/start":
		reply(helpText)
	case "/status":
		reply(s.statusText(st) + "\n" + s.Agent.Status(b.ID))
	case "/new":
		reply(s.Agent.NewSession(b.ID))
	case "/stop":
		reply(s.Agent.Stop(b.ID))
	case "/cancel":
		reply(s.Agent.CancelRun(b.ID, arg))
	case "/bind":
		reply("当前聊天已绑定。如需更换，请先在 Web 解除绑定。")
	case "":
		if time.Since(time.Unix(r.EventAt, 0)) > TextMaxAge {
			reply("这条消息已超过 10 分钟未处理（例如服务曾离线），为安全起见不会执行。如仍需要，请重新发送。")
			break
		}
		ahead, err := s.Agent.Submit(b.ID, r.ID, p.Text)
		if err != nil {
			if err.Error() == "agent_queue_full" {
				// Keep the event pending: accepted work is not dropped.
				_ = interaction.Enqueue(s.Store.DB, s.Vault, b, interaction.Reply{SemanticKey: "busy:" + r.ID, Text: "当前排队较多，已保留你的消息，稍后自动处理。"})
				return false
			}
			reply("无法处理：Agent 授权或绑定已失效，请管理员在 Web 检查。")
			break
		}
		if ahead > 0 {
			reply(fmt.Sprintf("已收到，前面还有 %d 条在处理，已排队。", ahead))
		} else {
			reply("已收到，正在处理…")
		}
	default:
		reply("未知命令。" + helpText)
	}
	s.finishInbox(r.ID, "processed", "")
	return true
}

const helpText = `可用命令：
/status 查看服务、模型、授权和最近运行状态（不依赖模型）
/new 开始新会话（清除对话上下文，待确认卡片失效）
/stop 停止当前 Agent 处理和未开始的方案（不是回滚）
/cancel <run_id> 停止某次任务运行的剩余项
也可以直接用中文描述整理需求，例如：把 某发件人 超过 30 天的邮件移到回收站，每周五 20 点执行。`

func (s *Service) contextEpoch(bindingID string) int64 {
	conv, err := interaction.ConversationOf(s.Store.DB, bindingID)
	if err != nil {
		return 0
	}
	return conv.ContextEpoch
}

func (s *Service) statusText(st Settings) string {
	var last int64
	_ = s.Store.DB.QueryRow("SELECT last_poll_at FROM telegram_poll_state WHERE bot_id=?", st.BotID).Scan(&last)
	ago := "从未"
	if last > 0 {
		ago = time.Since(time.Unix(last, 0)).Round(time.Second).String() + " 前"
	}
	return "服务：运行中\nTelegram 交互：" + st.State + "（最近接收 " + ago + "）"
}

// handleCallback answers the button press first, then routes it.
func (s *Service) handleCallback(ctx context.Context, st Settings, b interaction.Binding, r inboxRow, p inboxPayload) {
	answer := "已失效"
	defer func() {
		token, _, err := s.Mail.TelegramCredential()
		if err == nil {
			actx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_ = s.NewClient(token).AnswerCallback(actx, p.CallbackID, answer)
			cancel()
		}
		s.finishInbox(r.ID, "processed", "")
	}()
	if isProposal(p.Data) {
		answer = s.Agent.Confirm(ctx, b.ID, r.MessageID, p.Data)
		return
	}
	answer = "未知按钮"
}

func isProposal(data string) bool {
	return strings.HasPrefix(data, "p:a:") || strings.HasPrefix(data, "p:r:")
}

// handleUnbound accepts only /bind (throttled per user and globally) and a
// short hint; nothing about mailboxes is revealed and nothing reaches the
// model.
func (s *Service) handleUnbound(ctx context.Context, st Settings, r inboxRow, p inboxPayload) {
	if r.Kind != "message" {
		s.finishInbox(r.ID, "rejected", "binding_required")
		return
	}
	target := interaction.Binding{BotID: st.BotID, ChatID: r.ChatID, Epoch: st.Epoch}
	cmd, arg := validCommand(p.Text)
	if cmd != "/bind" {
		s.throttleMu.Lock()
		if len(s.notices) > 1000 {
			s.notices = map[int64]time.Time{}
		}
		last := s.notices[r.ChatID]
		send := time.Since(last) > 10*time.Minute
		if send {
			s.notices[r.ChatID] = time.Now()
		}
		s.throttleMu.Unlock()
		if send {
			_ = interaction.Enqueue(s.Store.DB, s.Vault, target, interaction.Reply{SemanticKey: "unbound:" + r.ID, Text: "此 Bot 仅供已绑定的管理员使用。请在 Mail Auto Web 生成绑定码后发送 /bind <绑定码>。"})
		}
		s.finishInbox(r.ID, "rejected", "binding_required")
		return
	}
	if !s.allowBindAttempt(r.UserID) {
		s.finishInbox(r.ID, "rejected", "bind_throttled")
		return
	}
	result := s.consumeBindCode(st, r, arg)
	_ = interaction.Enqueue(s.Store.DB, s.Vault, target, interaction.Reply{SemanticKey: "bind:" + r.ID, Text: result})
	s.finishInbox(r.ID, "processed", "")
}

func (s *Service) allowBindAttempt(user int64) bool {
	s.throttleMu.Lock()
	defer s.throttleMu.Unlock()
	cut := time.Now().Add(-10 * time.Minute)
	keep := func(in []time.Time) []time.Time {
		out := in[:0]
		for _, t := range in {
			if t.After(cut) {
				out = append(out, t)
			}
		}
		return out
	}
	s.global = keep(s.global)
	s.attempts[user] = keep(s.attempts[user])
	if len(s.global) >= 30 || len(s.attempts[user]) >= 5 || len(s.attempts) > 1000 {
		return false
	}
	s.global = append(s.global, time.Now())
	s.attempts[user] = append(s.attempts[user], time.Now())
	return true
}

// consumeBindCode atomically consumes a valid code and creates the single
// active binding with the real sender ID and private chat ID.
func (s *Service) consumeBindCode(st Settings, r inboxRow, code string) string {
	const generic = "绑定失败：绑定码无效、已使用或已过期。请在 Web 重新生成。"
	if len(code) != 22 {
		return generic
	}
	var admin int
	if s.Store.DB.QueryRow("SELECT version FROM admin WHERE id=1").Scan(&admin) != nil {
		return generic
	}
	tx, err := s.Store.DB.Begin()
	if err != nil {
		return generic
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var grantID string
	var grantVersion, adminVersion int
	err = tx.QueryRow("SELECT grant_id,grant_version,admin_version FROM telegram_bind_codes WHERE code_hash=? AND consumed_at=0 AND expires>? AND bot_id=? AND epoch=?", credentials.Digest(code), now, st.BotID, st.Epoch).Scan(&grantID, &grantVersion, &adminVersion)
	if err != nil {
		return generic
	}
	res, err := tx.Exec("UPDATE telegram_bind_codes SET consumed_at=? WHERE code_hash=? AND consumed_at=0", now, credentials.Digest(code))
	if err != nil {
		return generic
	}
	if n, _ := res.RowsAffected(); n != 1 || adminVersion != admin {
		return generic
	}
	var gv, ga, enabled, revoked int
	var expires int64
	if tx.QueryRow("SELECT version,admin_version,enabled,revoked,expires FROM agent_grants WHERE id=?", grantID).Scan(&gv, &ga, &enabled, &revoked, &expires) != nil || gv != grantVersion || ga != admin || enabled != 1 || revoked != 0 || expires <= now {
		return generic
	}
	if _, err = tx.Exec("INSERT INTO telegram_bindings(id,bot_id,user_id,chat_id,grant_id,admin_version,epoch,state,created_at) VALUES(?,?,?,?,?,?,?,'active',?)", credentials.Random(18), st.BotID, r.UserID, r.ChatID, grantID, admin, st.Epoch, now); err != nil {
		return "绑定失败：已存在有效绑定，请先在 Web 解除。"
	}
	if _, err = tx.Exec("INSERT INTO audits(at,action,target,outcome) VALUES(?,?,?,?)", now, "telegram.bind", strconv.FormatInt(r.UserID, 10), "ok"); err != nil {
		return generic
	}
	if tx.Commit() != nil {
		return generic
	}
	_, g, err := s.Mail.AgentIdentity(grantID)
	scope := ""
	if err == nil {
		scope = "\n授权动作：" + strings.Join(g.Actions, "、")
	}
	return "绑定成功。此绑定不会自动打开邮箱写入、扩大自动化策略或启用任何任务。" + scope + "\n" + helpText
}

// SendOne delivers the next due reply. Before sending it re-checks that the
// binding, grant, administrator version, receive epoch and conversation
// context are unchanged; stale replies are suppressed rather than sent to a
// new binding. A timeout after sending marks the result unknown and is
// retried at most once more: a duplicate notice is possible, but no
// business operation is ever re-run from here.
func (s *Service) SendOne(ctx context.Context) bool {
	now := time.Now().Unix()
	var id, bindingID, grantID string
	var botID, chatID, editID, epoch, ctxEpoch int64
	var admin, attempts int
	var sealed []byte
	err := s.Store.DB.QueryRow("SELECT id,binding_id,grant_id,bot_id,chat_id,edit_message_id,epoch,context_epoch,admin_version,attempts,payload FROM telegram_reply_outbox WHERE (state IN ('pending','retry') AND next_attempt<=?) OR (state='sending' AND lease_until<?) ORDER BY next_attempt,created_at,part LIMIT 1", now, now).Scan(&id, &bindingID, &grantID, &botID, &chatID, &editID, &epoch, &ctxEpoch, &admin, &attempts, &sealed)
	if err != nil {
		return false
	}
	res, err := s.Store.DB.Exec("UPDATE telegram_reply_outbox SET state='sending',lease_until=? WHERE id=? AND ((state IN ('pending','retry') AND next_attempt<=?) OR (state='sending' AND lease_until<?))", now+60, id, now, now)
	if err != nil {
		return false
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return true
	}
	finish := func(state, code string, message int64, next int64) {
		_, _ = s.Store.DB.Exec("UPDATE telegram_reply_outbox SET state=?,error_code=?,message_id=CASE WHEN ?>0 THEN ? ELSE message_id END,attempts=attempts+1,next_attempt=?,lease_until=0,updated_at=? WHERE id=?", state, code, message, message, next, time.Now().Unix(), id)
	}
	st, err := s.load(s.Store.DB)
	if err != nil || !st.Enabled || st.BotID != botID || st.Epoch != epoch {
		finish("suppressed", "interaction_changed", 0, 0)
		return true
	}
	if bindingID != "" {
		b, err := interaction.BindingByID(s.Store.DB, bindingID)
		if err != nil || b.State != "active" || b.GrantID != grantID || b.AdminVersion != admin || b.ChatID != chatID {
			finish("suppressed", "binding_changed", 0, 0)
			return true
		}
		if conv, err := interaction.ConversationOf(s.Store.DB, bindingID); err == nil && ctxEpoch != 0 && conv.ContextEpoch != ctxEpoch {
			finish("suppressed", "context_reset", 0, 0)
			return true
		}
		var gAdmin, gRevoked int
		if s.Store.DB.QueryRow("SELECT admin_version,revoked FROM agent_grants WHERE id=?", grantID).Scan(&gAdmin, &gRevoked) != nil || gAdmin != admin || gRevoked != 0 {
			finish("suppressed", "grant_changed", 0, 0)
			return true
		}
	}
	text, buttons, err := interaction.DecodeReply(s.Vault, id, sealed)
	if err != nil {
		finish("failed", "payload_unavailable", 0, 0)
		return true
	}
	token, gen, err := s.Mail.TelegramCredential()
	if err != nil || gen != st.credGen {
		finish("suppressed", "credential_changed", 0, 0)
		return true
	}
	if tglimit.Wait(ctx, strconv.FormatInt(chatID, 10)) != nil {
		finish("retry", "shutdown", 0, now)
		return false
	}
	client := s.NewClient(token)
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var message int64
	if editID != 0 {
		err = client.EditMessage(sctx, chatID, editID, text, buttons)
		message = editID
	} else {
		message, err = client.SendMessage(sctx, chatID, text, buttons)
	}
	if err == nil || codeOf(err) == CodeNotModified {
		finish("sent", "", message, 0)
		return true
	}
	code := codeOf(err)
	switch code {
	case CodeRateLimited:
		var ae *APIError
		errors.As(err, &ae)
		finish("retry", code, 0, time.Now().Add(ae.RetryAfter).Unix())
	case CodeUnknown, CodeNetwork, CodeUnavailable:
		if attempts >= 2 {
			finish("unknown", CodeUnknown, 0, 0)
		} else {
			finish("retry", code, 0, now+int64(5*(attempts+1)))
		}
	case CodeTokenBad, CodeForbidden:
		finish("failed", code, 0, 0)
		s.setState("error", code)
	default:
		finish("failed", code, 0, 0)
	}
	return ctx.Err() == nil
}
