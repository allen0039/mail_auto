package agent

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/interaction"
	"mail-auto/internal/mailtools"
)

const (
	callbackAccept = "p:a:"
	callbackReject = "p:r:"
)

// createProposal freezes a server-built proposal: actor, binding, grant and
// admin versions, epochs, the account state digest, steps and expiry. The
// callback token is random (128 bit) and only its hash is stored.
func (r *Runtime) createProposal(s *session, p *mailtools.Proposal) (string, error) {
	id := credentials.Random(18)
	token := credentials.Random(16)
	sealed, err := r.seal("agent-proposal", id, p)
	if err != nil {
		return "", err
	}
	now := time.Now()
	expires := now.Add(r.Limits.ProposalTTL).Unix()
	tx, err := r.Store.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO agent_proposals(id,conversation_id,binding_id,turn_id,token_hash,kind,state,payload,state_digest,grant_id,grant_version,admin_version,intent_epoch,context_epoch,stop_epoch,expires,chat_id,created_at,updated_at) VALUES(?,?,?,?,?,?,'pending',?,?,?,?,?,?,?,?,?,?,?,?)", id, s.conv.ID, s.binding.ID, s.turn.ID, credentials.Digest(token), p.Kind, sealed, p.StateDigest, s.binding.GrantID, s.grant.Version, s.binding.AdminVersion, s.turn.IntentEpoch, s.conv.ContextEpoch, s.turn.StopEpoch, expires, s.binding.ChatID, now.Unix(), now.Unix()); err != nil {
		return "", err
	}
	for i, st := range p.Steps {
		if _, err = tx.Exec("INSERT INTO agent_proposal_steps(proposal_id,ordinal,kind,mutation_id,state,updated_at) VALUES(?,?,?,?,'pending',?)", id, i, st.Kind, mailtools.MutationID(id, i), now.Unix()); err != nil {
			return "", err
		}
	}
	text := "【待确认】" + p.Title + "\n" + p.Summary + "\n\n有效期 10 分钟。补充条件或发送新消息会使本卡片失效；点击“确认执行”后按上述顺序生效。"
	buttons := []interaction.Button{{Text: "确认执行", Data: callbackAccept + token}, {Text: "取消", Data: callbackReject + token}}
	if err = interaction.Enqueue(tx, r.Vault, s.binding, interaction.Reply{SemanticKey: "proposal:" + id, Text: text, Buttons: buttons, ContextEpoch: s.conv.ContextEpoch}); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

type proposalRow struct {
	ID           string
	ConvID       string
	BindingID    string
	TurnID       string
	Kind         string
	State        string
	Payload      []byte
	Digest       string
	GrantID      string
	GrantVersion int
	AdminVersion int
	IntentEpoch  int64
	ContextEpoch int64
	StopEpoch    int64
	Expires      int64
}

const proposalColumns = "id,conversation_id,binding_id,turn_id,kind,state,payload,state_digest,grant_id,grant_version,admin_version,intent_epoch,context_epoch,stop_epoch,expires"

func scanProposal(row *sql.Row) (proposalRow, error) {
	var p proposalRow
	err := row.Scan(&p.ID, &p.ConvID, &p.BindingID, &p.TurnID, &p.Kind, &p.State, &p.Payload, &p.Digest, &p.GrantID, &p.GrantVersion, &p.AdminVersion, &p.IntentEpoch, &p.ContextEpoch, &p.StopEpoch, &p.Expires)
	return p, err
}

// IsProposalCallback reports whether callback data belongs to proposals.
func IsProposalCallback(data string) bool {
	return strings.HasPrefix(data, callbackAccept) || strings.HasPrefix(data, callbackReject)
}

// Confirm handles a card button. The caller has already matched the real
// sender and private chat to the active binding. Here the token, the card
// message, the binding, expiry, epochs, grant/admin versions and the frozen
// account state are re-checked, and the proposal is consumed by CAS. The
// model cannot reach this path.
func (r *Runtime) Confirm(ctx context.Context, bindingID string, messageID int64, data string) string {
	accept := strings.HasPrefix(data, callbackAccept)
	token := strings.TrimPrefix(strings.TrimPrefix(data, callbackAccept), callbackReject)
	if len(token) != 22 {
		return "按钮无效。"
	}
	p, err := scanProposal(r.Store.DB.QueryRow("SELECT "+proposalColumns+" FROM agent_proposals WHERE token_hash=?", credentials.Digest(token)))
	if err != nil || p.BindingID != bindingID {
		return "该按钮已失效。"
	}
	var cardMessage int64
	_ = r.Store.DB.QueryRow("SELECT message_id FROM telegram_reply_outbox WHERE semantic_key=? ORDER BY part DESC LIMIT 1", "proposal:"+p.ID).Scan(&cardMessage)
	if cardMessage == 0 || cardMessage != messageID {
		return "该按钮与原方案消息不匹配，已拒绝。"
	}
	if p.State != "pending" {
		return "该方案已处理（" + proposalStateLabel(p.State) + "）。"
	}
	if !accept {
		r.setProposal(p.ID, "pending", "rejected", "user_rejected")
		r.closeCard(p, "已取消，未做任何修改。")
		return "已取消该方案，未做任何修改。"
	}
	reason := r.proposalStale(p)
	if reason != "" {
		r.setProposal(p.ID, "pending", "invalidated", reason)
		r.closeCard(p, "方案已失效："+staleLabel(reason))
		return "方案已失效：" + staleLabel(reason) + "。请重新描述需求以生成新方案。"
	}
	r.writeMu.Lock()
	// Re-check under the write lock, then consume the proposal by CAS.
	if reason = r.proposalStale(p); reason != "" {
		r.writeMu.Unlock()
		r.setProposal(p.ID, "pending", "invalidated", reason)
		return "方案已失效：" + staleLabel(reason) + "。"
	}
	res, err := r.Store.DB.Exec("UPDATE agent_proposals SET state='accepted',updated_at=? WHERE id=? AND state='pending'", time.Now().Unix(), p.ID)
	r.writeMu.Unlock()
	if err != nil {
		return "暂时无法处理，请稍后重试。"
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "该方案已处理。"
	}
	r.closeCard(p, "已确认，正在按顺序执行。")
	// Steps are local database writes; the result message is queued by apply.
	r.apply(context.WithoutCancel(ctx), p.ID)
	return "已确认。"
}

func proposalStateLabel(s string) string {
	return map[string]string{"accepted": "已确认", "applying": "执行中", "applied": "已完成", "partial": "部分完成", "failed": "失败", "rejected": "已取消", "expired": "已过期", "invalidated": "已失效", "cancelled": "已停止"}[s]
}
func staleLabel(code string) string {
	return map[string]string{"expired": "已超过有效期", "superseded": "之后收到了新的消息或补充条件", "context_reset": "会话已重置", "stopped": "已执行 /stop", "grant_changed": "Agent 授权已变化", "binding_changed": "绑定已变化", "state_changed": "规则、任务、策略或文件夹映射已被修改"}[code]
}

// proposalStale returns a reason when the proposal may no longer be applied.
func (r *Runtime) proposalStale(p proposalRow) string {
	if p.Expires <= time.Now().Unix() {
		return "expired"
	}
	conv, err := interaction.ConversationOf(r.Store.DB, p.BindingID)
	if err != nil {
		return "binding_changed"
	}
	if conv.StopEpoch != p.StopEpoch {
		return "stopped"
	}
	if conv.ContextEpoch != p.ContextEpoch {
		return "context_reset"
	}
	if conv.IntentEpoch != p.IntentEpoch {
		return "superseded"
	}
	var waiting int
	_ = r.Store.DB.QueryRow("SELECT count(*) FROM agent_turns WHERE conversation_id=? AND state='queued'", conv.ID).Scan(&waiting)
	if waiting > 0 {
		return "superseded"
	}
	b, err := interaction.BindingByID(r.Store.DB, p.BindingID)
	if err != nil || b.State != "active" || b.AdminVersion != p.AdminVersion {
		return "binding_changed"
	}
	_, g, err := r.Mail.AgentIdentity(p.GrantID)
	if err != nil || g.Version != p.GrantVersion {
		return "grant_changed"
	}
	var prop mailtools.Proposal
	if r.open("agent-proposal", p.ID, p.Payload, &prop) != nil {
		return "state_changed"
	}
	digest, err := r.Mail.AccountStateDigest(prop.AccountID)
	if err != nil || digest != p.Digest {
		return "state_changed"
	}
	return ""
}

func (r *Runtime) setProposal(id, from, to, code string) {
	_, _ = r.Store.DB.Exec("UPDATE agent_proposals SET state=?,error_code=?,updated_at=? WHERE id=? AND state=?", to, code, time.Now().Unix(), id, from)
}

// closeCard edits the card to remove its buttons.
func (r *Runtime) closeCard(p proposalRow, note string) {
	var message int64
	if r.Store.DB.QueryRow("SELECT message_id FROM telegram_reply_outbox WHERE semantic_key=? ORDER BY part DESC LIMIT 1", "proposal:"+p.ID).Scan(&message) != nil || message == 0 {
		return
	}
	b, err := interaction.BindingByID(r.Store.DB, p.BindingID)
	if err != nil {
		return
	}
	_ = interaction.Enqueue(r.Store.DB, r.Vault, b, interaction.Reply{SemanticKey: "proposal:" + p.ID + ":closed", Text: "【方案】" + note, EditMessageID: message, ContextEpoch: p.ContextEpoch})
}

// apply executes an accepted proposal step by step. Each step re-checks the
// stop marker and identity and uses a server mutation ID, so a crash and
// replay returns committed results instead of writing again. Completed steps
// are never rolled back.
func (r *Runtime) apply(ctx context.Context, id string) {
	p, err := scanProposal(r.Store.DB.QueryRow("SELECT "+proposalColumns+" FROM agent_proposals WHERE id=?", id))
	if err != nil || (p.State != "accepted" && p.State != "applying") {
		return
	}
	var prop mailtools.Proposal
	if err = r.open("agent-proposal", p.ID, p.Payload, &prop); err != nil {
		r.setProposal(p.ID, p.State, "failed", "payload_unavailable")
		return
	}
	_, _ = r.Store.DB.Exec("UPDATE agent_proposals SET state='applying',updated_at=? WHERE id=? AND state IN ('accepted','applying')", time.Now().Unix(), p.ID)
	results := []mailtools.StepResult{}
	lines := []string{}
	failedCode := ""
	for i := range prop.Steps {
		r.writeMu.Lock()
		conv, err := interaction.ConversationOf(r.Store.DB, p.BindingID)
		var state string
		_ = r.Store.DB.QueryRow("SELECT state FROM agent_proposals WHERE id=?", p.ID).Scan(&state)
		if err != nil || conv.StopEpoch != p.StopEpoch || state != "applying" {
			r.writeMu.Unlock()
			failedCode = "stopped"
			break
		}
		access, _, err := r.Mail.AgentIdentity(p.GrantID)
		if err != nil {
			r.writeMu.Unlock()
			failedCode = "grant_expired"
			break
		}
		sr, err := mailtools.ApplyStep(ctx, r.Mail, access, p.ID, prop.Steps, i, results)
		now := time.Now().Unix()
		if err != nil {
			code := mailtools.CodeOf(err).Code
			_, _ = r.Store.DB.Exec("UPDATE agent_proposal_steps SET state='failed',error_code=?,updated_at=? WHERE proposal_id=? AND ordinal=?", code, now, p.ID, i)
			r.writeMu.Unlock()
			failedCode = code
			lines = append(lines, fmt.Sprintf("✗ 第 %d 步（%s）未完成：%s", i+1, stepLabel(prop.Steps[i].Kind), code))
			break
		}
		_, _ = r.Store.DB.Exec("UPDATE agent_proposal_steps SET state='done',target_id=?,target_version=?,updated_at=? WHERE proposal_id=? AND ordinal=?", sr.TargetID, sr.Version, now, p.ID, i)
		if sr.Run != nil {
			_, _ = r.Store.DB.Exec("INSERT OR IGNORE INTO agent_run_links(task_run_id,conversation_id,turn_id,proposal_id,binding_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", sr.Run.ID, p.ConvID, p.TurnID, p.ID, p.BindingID, now, now)
		}
		r.writeMu.Unlock()
		results = append(results, sr)
		lines = append(lines, stepOutcome(prop.Steps[i], sr))
	}
	final := "applied"
	header := "方案已全部执行："
	if failedCode != "" {
		final = "failed"
		header = "方案未完成："
		if len(results) > 0 {
			final = "partial"
			header = "方案部分完成（已完成的步骤保留，未自动回滚；请查看后发送新的明确指令）："
		}
		if failedCode == "stopped" {
			final = "cancelled"
			header = "方案已按 /stop 停止，未开始的步骤不再执行："
		}
	}
	tx, err := r.Store.DB.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE agent_proposals SET state=?,error_code=?,updated_at=? WHERE id=? AND state='applying'", final, failedCode, time.Now().Unix(), p.ID)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return
	}
	if b, err := interaction.BindingByID(tx, p.BindingID); err == nil && b.State == "active" {
		text := header + "\n" + strings.Join(lines, "\n")
		if final == "applied" {
			text += "\n可在 Web 管理界面查看一致的规则与任务状态。"
		}
		_ = interaction.Enqueue(tx, r.Vault, b, interaction.Reply{SemanticKey: "proposal:" + p.ID + ":result", Text: text, ContextEpoch: p.ContextEpoch})
	}
	_ = tx.Commit()
}

func stepLabel(kind string) string {
	return map[string]string{"rule_save": "保存/启用规则", "task_create": "创建暂停观察任务", "task_save": "更新任务", "task_enable": "启用任务", "task_run": "运行任务"}[kind]
}

func stepOutcome(st mailtools.Step, sr mailtools.StepResult) string {
	switch st.Kind {
	case "rule_save":
		state := "禁用"
		if sr.Rule != nil && sr.Rule.Enabled {
			state = "启用"
		}
		return fmt.Sprintf("✔ 规则 %s 已保存为%s（版本 %d）", sr.TargetID, state, sr.Version)
	case "task_create":
		return fmt.Sprintf("✔ 任务 %s 已创建：暂停、观察模式（版本 %d）", sr.TargetID, sr.Version)
	case "task_save":
		mode, state := "观察", "暂停"
		if sr.Task != nil && sr.Task.Mode == "execute" {
			mode = "执行"
		}
		if sr.Task != nil && sr.Task.Enabled {
			state = "启用"
		}
		return fmt.Sprintf("✔ 任务 %s 已更新：%s、%s模式（版本 %d）", sr.TargetID, state, mode, sr.Version)
	case "task_enable":
		return fmt.Sprintf("✔ 任务 %s 已启用（版本 %d）", sr.TargetID, sr.Version)
	case "task_run":
		return fmt.Sprintf("▶ 已入队运行 %s，完成后推送真实结果", sr.TargetID)
	}
	return "✔ " + st.Kind
}

func (r *Runtime) resumeProposals() {
	rows, err := r.Store.DB.Query("SELECT id FROM agent_proposals WHERE state IN ('accepted','applying')")
	if err != nil {
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		r.apply(context.Background(), id)
	}
}

func (r *Runtime) expireProposals() {
	_, _ = r.Store.DB.Exec("UPDATE agent_proposals SET state='expired',error_code='expired',updated_at=? WHERE state='pending' AND expires<=?", time.Now().Unix(), time.Now().Unix())
}
