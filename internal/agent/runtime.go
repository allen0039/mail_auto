package agent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/interaction"
	"mail-auto/internal/mail"
	"mail-auto/internal/mailtools"
	"mail-auto/internal/modelprovider"
	"mail-auto/internal/store"
)

// Limits are per-turn work bounds, unrelated to mail processing quotas.
type Limits struct {
	TurnTimeout      time.Duration
	MaxModelRequests int
	MaxToolCalls     int
	MaxHistoryTurns  int
	MaxContextBytes  int
	MaxQueue         int
	ProposalTTL      time.Duration
	MaxOutputTokens  int
}

func DefaultLimits() Limits {
	return Limits{TurnTimeout: 120 * time.Second, MaxModelRequests: 8, MaxToolCalls: 20, MaxHistoryTurns: 20, MaxContextBytes: 64 << 10, MaxQueue: 100, ProposalTTL: 10 * time.Minute, MaxOutputTokens: 2048}
}

// Runtime is the in-process agent. It runs one model turn at a time.
type Runtime struct {
	Store       *store.Store
	Vault       *credentials.Vault
	Mail        *mail.Service
	Tools       *mailtools.Registry
	NewProvider func(modelprovider.Settings) (modelprovider.Provider, error)
	Limits      Limits

	settingsMu sync.Mutex
	// writeMu serializes stop/new/cancel with tool writes and proposal steps,
	// so a stop marker is observed before any later business write.
	writeMu  sync.Mutex
	mu       sync.Mutex
	current  string
	cancel   context.CancelFunc
	wake     chan struct{}
	workerID string
	wg       sync.WaitGroup
	started  bool
}

func New(st *store.Store, v *credentials.Vault, m *mail.Service) *Runtime {
	return &Runtime{Store: st, Vault: v, Mail: m, Tools: mailtools.Agent(), Limits: DefaultLimits(), wake: make(chan struct{}, 1), workerID: credentials.Random(12)}
}

var ErrQueueFull = errors.New("agent_queue_full")
var ErrNotReady = errors.New("provider_not_ready")

func (r *Runtime) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runtime) seal(kind, id string, v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return r.Vault.Encrypt(string(raw), kind+":"+id)
}
func (r *Runtime) open(kind, id string, sealed []byte, v any) error {
	plain, err := r.Vault.Decrypt(sealed, kind+":"+id)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(plain), v)
}

// Submit queues a plain-text instruction from a verified binding. inputID is
// the durable inbox event ID, so a redelivered update never creates a second
// turn. It returns the number of turns ahead of this one.
func (r *Runtime) Submit(bindingID, inputID, text string) (int, error) {
	// Identity checks run before the transaction: the store has a single
	// connection, so no other query may run while a transaction is open.
	b, err := interaction.BindingByID(r.Store.DB, bindingID)
	if err != nil || b.State != "active" {
		return 0, mail.ErrDenied
	}
	_, g, err := r.Mail.AgentIdentity(b.GrantID)
	if err != nil {
		return 0, mail.ErrDenied
	}
	tx, err := r.Store.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if b, err = interaction.BindingByID(tx, bindingID); err != nil || b.State != "active" || b.GrantID != g.ID {
		return 0, mail.ErrDenied
	}
	conv, err := interaction.EnsureConversation(tx, bindingID, g.Version)
	if err != nil {
		return 0, err
	}
	var existing string
	if tx.QueryRow("SELECT id FROM agent_turns WHERE input_id=?", inputID).Scan(&existing) == nil {
		return 0, nil
	}
	var queued int
	if err = tx.QueryRow("SELECT count(*) FROM agent_turns WHERE state IN ('queued','running')").Scan(&queued); err != nil {
		return 0, err
	}
	if queued >= r.Limits.MaxQueue {
		return queued, ErrQueueFull
	}
	now := time.Now().Unix()
	var seq int
	if err = tx.QueryRow("UPDATE agent_conversations SET next_seq=next_seq+1,updated_at=? WHERE id=? RETURNING next_seq-1", now, conv.ID).Scan(&seq); err != nil {
		return 0, err
	}
	id := credentials.Random(18)
	if _, err = tx.Exec("INSERT INTO agent_turns(id,conversation_id,seq,input_id,state,context_epoch,intent_epoch,stop_epoch,grant_version,admin_version,created_at,updated_at) VALUES(?,?,?,?,'queued',?,?,?,?,?,?,?)", id, conv.ID, seq, inputID, conv.ContextEpoch, conv.IntentEpoch, conv.StopEpoch, g.Version, b.AdminVersion, now, now); err != nil {
		return 0, err
	}
	if err = r.insertMessage(tx, id, 0, modelprovider.Message{Role: "user", Content: text}, conv.ContextEpoch, g.Version, b.AdminVersion, 0); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	r.notify()
	return queued, nil
}

func (r *Runtime) insertMessage(q interaction.Querier, turnID string, seq int, m modelprovider.Message, contextEpoch int64, grantVersion, adminVersion, configVersion int) error {
	id := credentials.Random(18)
	sealed, err := r.seal("agent-msg", id, m)
	if err != nil {
		return err
	}
	_, err = q.Exec("INSERT INTO agent_messages(id,turn_id,seq,role,tool_call_id,payload,context_epoch,grant_version,admin_version,config_version,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)", id, turnID, seq, m.Role, m.ToolCallID, sealed, contextEpoch, grantVersion, adminVersion, configVersion, time.Now().Unix())
	return err
}

// Start recovers interrupted work and starts the turn and run monitors.
func (r *Runtime) Start(ctx context.Context) {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.mu.Unlock()
	r.recover()
	r.wg.Add(2)
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			for r.Step(ctx) {
				if ctx.Err() != nil {
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-r.wake:
			case <-ticker.C:
			}
		}
	}()
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.MonitorRuns()
				r.expireProposals()
			}
		}
	}()
}

// Wait blocks until workers exit after the start context is cancelled.
func (r *Runtime) Wait() { r.wg.Wait() }

func (r *Runtime) cancelCurrent(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
	_ = reason
}

type turnRow struct {
	ID           string
	ConvID       string
	Seq          int
	ContextEpoch int64
	IntentEpoch  int64
	StopEpoch    int64
	GrantVersion int
	AdminVersion int
}

// Step claims and runs the oldest queued turn. It returns false when idle.
func (r *Runtime) Step(ctx context.Context) bool {
	var t turnRow
	err := r.Store.DB.QueryRowContext(ctx, "SELECT id,conversation_id,seq,context_epoch,intent_epoch,stop_epoch,grant_version,admin_version FROM agent_turns WHERE state='queued' ORDER BY created_at,seq LIMIT 1").Scan(&t.ID, &t.ConvID, &t.Seq, &t.ContextEpoch, &t.IntentEpoch, &t.StopEpoch, &t.GrantVersion, &t.AdminVersion)
	if err != nil {
		return false
	}
	now := time.Now()
	deadline := now.Add(r.Limits.TurnTimeout)
	res, err := r.Store.DB.Exec("UPDATE agent_turns SET state='running',lease_owner=?,lease_until=?,deadline=?,updated_at=? WHERE id=? AND state='queued'", r.workerID, deadline.Unix()+30, deadline.Unix(), now.Unix(), t.ID)
	if err != nil {
		return false
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return true
	}
	turnCtx, cancel := context.WithDeadline(ctx, deadline)
	r.mu.Lock()
	r.current, r.cancel = t.ID, cancel
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		r.current, r.cancel = "", nil
		r.mu.Unlock()
	}()
	r.runTurn(turnCtx, t)
	return true
}

// session is the resolved, server-side state of one running turn.
type session struct {
	turn     turnRow
	binding  interaction.Binding
	conv     interaction.Conversation
	access   mail.Access
	grant    mail.AgentGrant
	provider storedProvider
	outcomes []string
}

// alive re-reads the turn, conversation, binding, grant and provider. Any
// stop, cancellation, epoch, grant, admin or config change ends the turn
// before the next model request or business write.
func (r *Runtime) alive(s *session) error {
	var state string
	if err := r.Store.DB.QueryRow("SELECT state FROM agent_turns WHERE id=?", s.turn.ID).Scan(&state); err != nil || state != "running" {
		return errTurnStopped
	}
	conv, err := interaction.ConversationOf(r.Store.DB, s.binding.ID)
	if err != nil || conv.StopEpoch != s.turn.StopEpoch || conv.ContextEpoch != s.conv.ContextEpoch {
		return errTurnStopped
	}
	b, err := interaction.BindingByID(r.Store.DB, s.binding.ID)
	if err != nil || b.State != "active" {
		return errBindingRevoked
	}
	a, g, err := r.Mail.AgentIdentity(b.GrantID)
	if err != nil || g.Version != s.grant.Version {
		return errGrantChanged
	}
	p, err := r.loadProvider()
	if err != nil || p.version != s.provider.version || !p.ready() {
		return ErrNotReady
	}
	s.access = a
	return nil
}

var (
	errTurnStopped    = errors.New("turn_stopped")
	errBindingRevoked = errors.New("binding_required")
	errGrantChanged   = errors.New("grant_changed")
)

func (r *Runtime) finishTurn(s *session, state, code, text string) {
	tx, err := r.Store.DB.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE agent_turns SET state=?,error_code=?,lease_until=0,updated_at=? WHERE id=? AND state='running'", state, code, time.Now().Unix(), s.turn.ID)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Stopped concurrently: the stop path owns the reply.
		return
	}
	if text != "" {
		if len(s.outcomes) > 0 {
			text += "\n\n操作记录（以服务端实际结果为准）：\n" + strings.Join(s.outcomes, "\n")
		}
		if err = interaction.Enqueue(tx, r.Vault, s.binding, interaction.Reply{SemanticKey: "turn:" + s.turn.ID + ":final", Text: text, ContextEpoch: s.conv.ContextEpoch}); err != nil {
			return
		}
	}
	_ = tx.Commit()
}

func (r *Runtime) runTurn(ctx context.Context, t turnRow) {
	s := &session{turn: t}
	var err error
	if err = r.Store.DB.QueryRow("SELECT b.id FROM agent_conversations c JOIN telegram_bindings b ON b.id=c.binding_id WHERE c.id=?", t.ConvID).Scan(&s.binding.ID); err != nil {
		_, _ = r.Store.DB.Exec("UPDATE agent_turns SET state='failed',error_code='binding_required',updated_at=? WHERE id=?", time.Now().Unix(), t.ID)
		return
	}
	s.binding, err = interaction.BindingByID(r.Store.DB, s.binding.ID)
	if err != nil {
		return
	}
	if s.conv, err = interaction.ConversationOf(r.Store.DB, s.binding.ID); err != nil {
		return
	}
	if s.conv.StopEpoch != t.StopEpoch {
		_, _ = r.Store.DB.Exec("UPDATE agent_turns SET state='cancelled',error_code='stopped',updated_at=? WHERE id=?", time.Now().Unix(), t.ID)
		return
	}
	if s.binding.State != "active" {
		r.finishTurn(s, "failed", "binding_required", "")
		return
	}
	s.access, s.grant, err = r.Mail.AgentIdentity(s.binding.GrantID)
	if err != nil {
		r.finishTurn(s, "failed", "grant_expired", "Agent 授权已失效或过期，请管理员在 Web 重新授权后再试。")
		return
	}
	if s.grant.Version != s.conv.GrantVersion {
		// Grant scope changed: drop the old context so results obtained under
		// the old scope are never sent to the model again.
		if r.resetContext(s.conv.ID, s.grant.Version, "grant_changed") == nil {
			s.conv, _ = interaction.ConversationOf(r.Store.DB, s.binding.ID)
		}
	}
	if s.conv.ContextEpoch != t.ContextEpoch {
		_, _ = r.Store.DB.Exec("UPDATE agent_turns SET context_epoch=? WHERE id=?", s.conv.ContextEpoch, t.ID)
		_, _ = r.Store.DB.Exec("UPDATE agent_messages SET context_epoch=?,grant_version=? WHERE turn_id=?", s.conv.ContextEpoch, s.grant.Version, t.ID)
	}
	s.provider, err = r.loadProvider()
	if err != nil || !s.provider.ready() {
		r.finishTurn(s, "failed", "provider_not_ready", "模型尚未配置或未通过工具调用检测，无法处理自然语言指令。/status、/stop、/cancel 等命令仍可使用。")
		return
	}
	client, err := r.providerClient(s.provider)
	if err != nil {
		r.finishTurn(s, "failed", modelprovider.CodeOf(err), "模型配置不可用，请管理员在 Web 检查。")
		return
	}
	msgs, err := r.context(s)
	if err != nil {
		r.finishTurn(s, "failed", "context_unavailable", "会话记录无法读取，请发送 /new 开始新会话。")
		return
	}
	defs := []modelprovider.ToolDef{}
	for _, tool := range r.Tools.Tools() {
		defs = append(defs, modelprovider.ToolDef{Name: tool.Name, Description: tool.Description, Parameters: tool.Schema})
	}
	seq := r.nextSeq(t.ID) // the user message is sequence 0
	toolCalls := 0
	for request := 0; request < r.Limits.MaxModelRequests; request++ {
		if err = r.alive(s); err != nil {
			r.finishTurn(s, "cancelled", err.Error(), "")
			return
		}
		_, _ = r.Store.DB.Exec("UPDATE agent_turns SET model_requests=model_requests+1,updated_at=? WHERE id=?", time.Now().Unix(), t.ID)
		resp, err := client.Complete(ctx, modelprovider.Request{Messages: msgs, Tools: defs, MaxOutputTokens: r.Limits.MaxOutputTokens})
		if err != nil {
			code := modelprovider.CodeOf(err)
			if ctx.Err() != nil {
				code = "turn_timeout"
				if errors.Is(ctx.Err(), context.Canceled) {
					code = "stopped"
				}
			}
			r.finishTurn(s, "failed", code, providerFailureText(code))
			return
		}
		// The response is accepted only if nothing changed while waiting.
		if err = r.alive(s); err != nil {
			r.finishTurn(s, "cancelled", err.Error(), "")
			return
		}
		assistant := modelprovider.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls}
		if len(resp.ToolCalls) == 0 {
			if err = r.insertMessage(r.Store.DB, t.ID, seq, assistant, s.conv.ContextEpoch, s.grant.Version, s.binding.AdminVersion, s.provider.version); err != nil {
				r.finishTurn(s, "failed", "storage_error", "")
				return
			}
			r.finishTurn(s, "completed", "", strings.TrimSpace(resp.Content))
			return
		}
		if toolCalls+len(resp.ToolCalls) > r.Limits.MaxToolCalls {
			r.finishTurn(s, "failed", "agent_limit_reached", "本轮工具调用次数达到上限，已停止。请把需求拆小后重新发送。")
			return
		}
		// Persist the assistant call message and the pending call records
		// before any tool runs, so recovery can see exactly what was asked.
		if err = r.persistCalls(s, seq, assistant); err != nil {
			r.finishTurn(s, "failed", "storage_error", "")
			return
		}
		msgs = append(msgs, assistant)
		seq++
		writeFailed := false
		for i, call := range resp.ToolCalls {
			toolCalls++
			out := r.executeCall(ctx, s, toolCalls-1, call, &writeFailed)
			msg := modelprovider.Message{Role: "tool", ToolCallID: call.ID, Content: out}
			if err = r.insertMessage(r.Store.DB, t.ID, seq, msg, s.conv.ContextEpoch, s.grant.Version, s.binding.AdminVersion, s.provider.version); err != nil {
				r.finishTurn(s, "failed", "storage_error", "")
				return
			}
			msgs = append(msgs, msg)
			seq++
			_ = i
		}
		_, _ = r.Store.DB.Exec("UPDATE agent_turns SET tool_calls=?,updated_at=? WHERE id=?", toolCalls, time.Now().Unix(), t.ID)
	}
	r.finishTurn(s, "failed", "agent_limit_reached", "本轮模型请求次数达到上限，已停止。请把需求拆小后重新发送。")
}

func (r *Runtime) nextSeq(turnID string) int {
	var n int
	_ = r.Store.DB.QueryRow("SELECT COALESCE(max(seq),0)+1 FROM agent_messages WHERE turn_id=?", turnID).Scan(&n)
	return n
}

func providerFailureText(code string) string {
	switch code {
	case "turn_timeout":
		return "处理超时，已停止本轮。没有未完成的写入会被自动重做；请重新发送指令。"
	case "stopped":
		return ""
	case modelprovider.CodeAuth:
		return "模型服务认证失败，请管理员在 Web 检查 API Key。"
	case modelprovider.CodeRateLimited:
		return "模型服务限流，请稍后再试。"
	case modelprovider.CodeTruncated:
		return "模型输出被截断，本轮未执行任何操作。请简化需求后重试。"
	}
	return "模型服务暂时不可用（" + code + "），本轮已停止。邮箱同步和既有任务不受影响。"
}

func (r *Runtime) persistCalls(s *session, seq int, assistant modelprovider.Message) error {
	tx, err := r.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = r.insertMessage(tx, s.turn.ID, seq, assistant, s.conv.ContextEpoch, s.grant.Version, s.binding.AdminVersion, s.provider.version); err != nil {
		return err
	}
	var base int
	if err = tx.QueryRow("SELECT COALESCE(max(ordinal)+1,0) FROM agent_tool_calls WHERE turn_id=?", s.turn.ID).Scan(&base); err != nil {
		return err
	}
	now := time.Now().Unix()
	for i, c := range assistant.ToolCalls {
		risk := "unknown"
		if tool, ok := r.Tools.Lookup(c.Name); ok {
			risk = string(tool.Risk)
		}
		sum := sha256.Sum256([]byte(c.Arguments))
		if _, err = tx.Exec("INSERT INTO agent_tool_calls(id,turn_id,ordinal,call_id,name,args_digest,risk,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'pending',?,?)", credentials.Random(18), s.turn.ID, base+i, c.ID, c.Name, hex.EncodeToString(sum[:]), risk, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// actionID returns the server-side business intent ID for a validated call:
// the same tool with the same canonical arguments within one user turn maps
// to one action, so a regenerated or repeated call reuses its result.
func (r *Runtime) actionID(s *session, name string, canonical []byte) (string, error) {
	sum := sha256.Sum256(canonical)
	key := s.turn.ID + ":" + name + ":" + hex.EncodeToString(sum[:])
	if _, err := r.Store.DB.Exec("INSERT OR IGNORE INTO agent_actions(id,conversation_id,intent_key,kind,args_digest,created_at) VALUES(?,?,?,?,?,?)", credentials.Random(18), s.conv.ID, key, name, hex.EncodeToString(sum[:]), time.Now().Unix()); err != nil {
		return "", err
	}
	var id string
	err := r.Store.DB.QueryRow("SELECT id FROM agent_actions WHERE conversation_id=? AND intent_key=?", s.conv.ID, key).Scan(&id)
	return id, err
}

func toolError(code, hint string) string {
	raw, _ := json.Marshal(map[string]any{"ok": false, "error": code, "hint": hint})
	return string(raw)
}

// executeCall validates and runs one tool call serially. Only complete,
// validated calls reach the tool layer. Once a write in this response fails,
// later writes of the same response are skipped (they may depend on it).
func (r *Runtime) executeCall(ctx context.Context, s *session, ordinal int, call modelprovider.ToolCall, writeFailed *bool) string {
	setState := func(state, code string, result string) {
		var sealed []byte
		if result != "" {
			sealed, _ = r.seal("agent-tool", s.turn.ID+":"+call.ID, result)
		}
		_, _ = r.Store.DB.Exec("UPDATE agent_tool_calls SET state=?,result_code=?,result=?,updated_at=? WHERE turn_id=? AND call_id=?", state, code, sealed, time.Now().Unix(), s.turn.ID, call.ID)
	}
	tool, ok := r.Tools.Lookup(call.Name)
	if !ok {
		setState("rejected", "unknown_tool", "")
		return toolError("unknown_tool", "只能使用提供的工具")
	}
	canonical, err := r.Tools.Canonical(call.Name, call.Arguments)
	if err != nil {
		code := mailtools.CodeOf(err)
		setState("rejected", code.Code, "")
		return toolError(code.Code, code.Hint)
	}
	write := tool.Risk != mailtools.RiskRead
	if write && *writeFailed {
		setState("skipped", "previous_write_failed", "")
		return toolError("skipped_previous_write_failed", "同一批中前一个写入失败，本调用未执行")
	}
	action, err := r.actionID(s, call.Name, canonical)
	if err != nil {
		setState("failed", "storage_error", "")
		return toolError("storage_error", "")
	}
	_, _ = r.Store.DB.Exec("UPDATE agent_tool_calls SET action_id=?,state='running' WHERE turn_id=? AND call_id=?", action, s.turn.ID, call.ID)
	// Stop/new/cancel take writeMu: holding it across the liveness check and
	// the write means a stop is either fully before (and seen) or after.
	r.writeMu.Lock()
	if err = r.alive(s); err != nil {
		r.writeMu.Unlock()
		setState("skipped", err.Error(), "")
		return toolError("turn_stopped", "本轮已停止")
	}
	res, encoded, err := r.Tools.Invoke(ctx, mailtools.Call{Service: r.Mail, Access: s.access, ActionID: action}, call.Name, canonical)
	var proposalID string
	if err == nil && res.Proposal != nil {
		proposalID, err = r.createProposal(s, res.Proposal)
	}
	r.writeMu.Unlock()
	if err != nil {
		code := mailtools.CodeOf(err)
		if write {
			*writeFailed = true
			s.outcomes = append(s.outcomes, fmt.Sprintf("✗ %s 未完成：%s", call.Name, code.Code))
		}
		setState("failed", code.Code, "")
		return toolError(code.Code, code.Hint)
	}
	out := string(encoded)
	if proposalID != "" {
		raw, _ := json.Marshal(map[string]any{"ok": true, "status": "confirmation_required", "proposal_id": proposalID, "result": json.RawMessage(encoded), "note": "已向用户发送确认卡片。用户点击“确认执行”之前，任何变更都没有生效；不要声称已生效。"})
		out = string(raw)
		s.outcomes = append(s.outcomes, "⏳ 已生成确认卡片（"+res.Proposal.Title+"），尚未生效，需点击卡片按钮")
	} else if write {
		s.outcomes = append(s.outcomes, describeOutcome(call.Name, res))
		if run, ok := runFromResult(res); ok {
			r.linkRun(s, run, "")
		}
	}
	setState("done", "ok", out)
	return out
}

func describeOutcome(name string, res mailtools.Result) string {
	data, _ := res.Data.(map[string]any)
	switch name {
	case "save_rule_draft":
		if rule, ok := data["rule"].(map[string]any); ok {
			return fmt.Sprintf("✔ 已保存禁用规则草稿 %v（版本 %v）", rule["id"], rule["version"])
		}
	case "save_task_draft":
		if task, ok := data["task"].(map[string]any); ok {
			return fmt.Sprintf("✔ 已保存暂停观察任务草稿 %v（版本 %v）", task["id"], task["version"])
		}
	case "run_task":
		if run, ok := data["run"].(map[string]any); ok {
			return fmt.Sprintf("▶ 已入队运行 %v（状态 %v），完成后会推送真实结果", run["id"], run["state"])
		}
	case "pause_task":
		return fmt.Sprintf("⏸ 任务 %v：%v", data["task_id"], data["status"])
	case "cancel_task_run":
		if run, ok := data["run"].(map[string]any); ok {
			return fmt.Sprintf("■ 已请求停止运行 %v 的剩余项（状态 %v），已完成动作保留", run["id"], run["state"])
		}
	}
	return "✔ " + name + " 已完成"
}

func runFromResult(res mailtools.Result) (string, bool) {
	data, _ := res.Data.(map[string]any)
	if data == nil || data["status"] != "queued" {
		return "", false
	}
	run, _ := data["run"].(map[string]any)
	id, _ := run["id"].(string)
	return id, id != ""
}

func (r *Runtime) linkRun(s *session, runID, proposalID string) {
	now := time.Now().Unix()
	_, _ = r.Store.DB.Exec("INSERT OR IGNORE INTO agent_run_links(task_run_id,conversation_id,turn_id,proposal_id,binding_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", runID, s.conv.ID, s.turn.ID, proposalID, s.binding.ID, now, now)
}

// context builds the model input: system prompt, complete earlier turns of
// the current context epoch and grant version (bounded by turns and bytes),
// then this turn's persisted messages.
func (r *Runtime) context(s *session) ([]modelprovider.Message, error) {
	rows, err := r.Store.DB.Query("SELECT t.id,t.seq,m.id,m.payload FROM agent_turns t JOIN agent_messages m ON m.turn_id=t.id WHERE t.conversation_id=? AND (t.state='completed' OR t.id=?) AND m.context_epoch=? AND m.grant_version=? ORDER BY t.seq,m.seq", s.conv.ID, s.turn.ID, s.conv.ContextEpoch, s.grant.Version)
	if err != nil {
		return nil, err
	}
	type turnMsgs struct {
		id   string
		msgs []modelprovider.Message
		size int
	}
	turns := []*turnMsgs{}
	byID := map[string]*turnMsgs{}
	for rows.Next() {
		var tid, mid string
		var tseq int
		var sealed []byte
		if err = rows.Scan(&tid, &tseq, &mid, &sealed); err != nil {
			rows.Close()
			return nil, err
		}
		var m modelprovider.Message
		if err = r.open("agent-msg", mid, sealed, &m); err != nil {
			rows.Close()
			return nil, err
		}
		tm := byID[tid]
		if tm == nil {
			tm = &turnMsgs{id: tid}
			byID[tid] = tm
			turns = append(turns, tm)
		}
		tm.msgs = append(tm.msgs, m)
		tm.size += len(m.Content) + len(m.ToolCallID)
		for _, c := range m.ToolCalls {
			tm.size += len(c.Arguments) + len(c.Name) + len(c.ID)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// Keep the current turn plus the newest complete turns within budget;
	// whole turns only, so no tool message is ever orphaned.
	var current *turnMsgs
	history := []*turnMsgs{}
	for _, tm := range turns {
		if tm.id == s.turn.ID {
			current = tm
		} else {
			history = append(history, tm)
		}
	}
	if current == nil {
		return nil, sql.ErrNoRows
	}
	if len(history) > r.Limits.MaxHistoryTurns {
		history = history[len(history)-r.Limits.MaxHistoryTurns:]
	}
	budget := r.Limits.MaxContextBytes - current.size
	start := len(history)
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].size > budget {
			break
		}
		budget -= history[i].size
		start = i
	}
	out := []modelprovider.Message{{Role: "system", Content: systemPrompt(time.Now())}}
	for _, tm := range history[start:] {
		out = append(out, tm.msgs...)
	}
	return append(out, current.msgs...), nil
}

// resetContext starts a new context epoch and invalidates pending proposals.
func (r *Runtime) resetContext(convID string, grantVersion int, reason string) error {
	tx, err := r.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err = tx.Exec("UPDATE agent_conversations SET context_epoch=context_epoch+1,grant_version=?,updated_at=? WHERE id=?", grantVersion, now, convID); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE agent_proposals SET state='invalidated',error_code=?,updated_at=? WHERE conversation_id=? AND state='pending'", reason, now, convID); err != nil {
		return err
	}
	return tx.Commit()
}

// recover runs once at start. Running turns are interrupted, not resumed:
// a half-finished free-form generation is never re-run automatically. The
// user is told which writes completed. Accepted proposals resume through
// their idempotent steps unless a stop marker supersedes them.
func (r *Runtime) recover() {
	rows, err := r.Store.DB.Query("SELECT t.id,t.conversation_id,c.binding_id,c.context_epoch FROM agent_turns t JOIN agent_conversations c ON c.id=t.conversation_id WHERE t.state='running'")
	if err == nil {
		type item struct {
			id, conv, binding string
			epoch             int64
		}
		items := []item{}
		for rows.Next() {
			var it item
			if rows.Scan(&it.id, &it.conv, &it.binding, &it.epoch) == nil {
				items = append(items, it)
			}
		}
		rows.Close()
		for _, it := range items {
			done := r.completedWrites(it.id)
			tx, err := r.Store.DB.Begin()
			if err != nil {
				continue
			}
			res, err := tx.Exec("UPDATE agent_turns SET state='interrupted',error_code='service_restarted',updated_at=? WHERE id=? AND state='running'", time.Now().Unix(), it.id)
			if err == nil {
				if n, _ := res.RowsAffected(); n == 1 {
					if b, err := interaction.BindingByID(tx, it.binding); err == nil && b.State == "active" {
						text := "上一条指令在处理中因服务重启而中断，不会自动重新执行。"
						if len(done) > 0 {
							text += "\n已完成的操作：\n" + strings.Join(done, "\n")
						} else {
							text += "\n没有已完成的写入。"
						}
						text += "\n如需继续，请重新发送明确指令。"
						_ = interaction.Enqueue(tx, r.Vault, b, interaction.Reply{SemanticKey: "turn:" + it.id + ":interrupted", Text: text, ContextEpoch: it.epoch})
					}
				}
			}
			if err == nil {
				_ = tx.Commit()
			} else {
				tx.Rollback()
			}
		}
	}
	r.resumeProposals()
}

func (r *Runtime) completedWrites(turnID string) []string {
	rows, err := r.Store.DB.Query("SELECT name,action_id FROM agent_tool_calls WHERE turn_id=? AND state='done' AND risk IN ('draft','run') ORDER BY ordinal", turnID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name, action string
		if rows.Scan(&name, &action) == nil {
			target, version, ok, _ := r.Mail.MutationTarget("action:" + action)
			if ok {
				out = append(out, fmt.Sprintf("· %s → %s（版本 %d）", name, target, version))
			} else {
				out = append(out, "· "+name)
			}
		}
	}
	return out
}
