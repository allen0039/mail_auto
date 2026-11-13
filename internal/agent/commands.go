package agent

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"mail-auto/internal/interaction"
	"mail-auto/internal/mail"
	"mail-auto/internal/mailtools"
)

// Stop atomically cancels the current and queued turns of the binding's
// conversation and every pending or not-yet-applied proposal, and advances
// the stop epoch so recovery cannot revive them. Task runs keep running;
// /cancel stops a run's remaining items. Nothing here needs the model.
func (r *Runtime) Stop(bindingID string) string {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	tx, err := r.Store.DB.Begin()
	if err != nil {
		return "暂时无法停止，请重试。"
	}
	defer tx.Rollback()
	conv, err := interaction.ConversationOf(tx, bindingID)
	if err == sql.ErrNoRows {
		return "当前没有进行中的 Agent 处理。"
	}
	if err != nil {
		return "暂时无法停止，请重试。"
	}
	now := time.Now().Unix()
	if _, err = tx.Exec("UPDATE agent_conversations SET stop_epoch=stop_epoch+1,updated_at=? WHERE id=?", now, conv.ID); err != nil {
		return "暂时无法停止，请重试。"
	}
	res, err := tx.Exec("UPDATE agent_turns SET state='cancelled',error_code='stopped',updated_at=? WHERE conversation_id=? AND state IN ('queued','running')", now, conv.ID)
	if err != nil {
		return "暂时无法停止，请重试。"
	}
	turns, _ := res.RowsAffected()
	var applying int
	_ = tx.QueryRow("SELECT count(*) FROM agent_proposals WHERE conversation_id=? AND state='applying'", conv.ID).Scan(&applying)
	// Applying proposals are cancelled too: their next step's guard sees the
	// new stop epoch and the cancelled state and does not commit.
	res, err = tx.Exec("UPDATE agent_proposals SET state='cancelled',error_code='stopped',updated_at=? WHERE conversation_id=? AND state IN ('pending','accepted','applying')", now, conv.ID)
	if err != nil {
		return "暂时无法停止，请重试。"
	}
	proposals, _ := res.RowsAffected()
	proposals -= int64(applying)
	if err = store_audit(tx, "agent.stop", conv.ID); err != nil {
		return "暂时无法停止，请重试。"
	}
	if err = tx.Commit(); err != nil {
		return "暂时无法停止，请重试。"
	}
	r.mu.Lock()
	if r.cancel != nil && r.current != "" {
		var convID string
		if r.Store.DB.QueryRow("SELECT conversation_id FROM agent_turns WHERE id=?", r.current).Scan(&convID) == nil && convID == conv.ID {
			r.cancel()
		}
	}
	r.mu.Unlock()
	text := fmt.Sprintf("已停止 Agent 处理：取消 %d 个排队或进行中的轮次、%d 个待确认或未开始的方案。", turns, proposals)
	if applying > 0 {
		text += fmt.Sprintf("\n%d 个正在执行的方案已停止：已提交的步骤保留，未开始的步骤不再执行。", applying)
	}
	return text + "\n已完成的邮件动作和已启动的任务运行不受影响；停止某次运行的剩余项请用 /cancel <run_id>。这不是回滚。"
}

// NewSession clears the usable context and invalidates pending proposals.
// Business tasks and audit records are kept.
func (r *Runtime) NewSession(bindingID string) string {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	conv, err := interaction.ConversationOf(r.Store.DB, bindingID)
	if err == sql.ErrNoRows {
		return "已开始新会话。"
	}
	if err != nil {
		return "暂时无法重置，请重试。"
	}
	if err = r.resetContext(conv.ID, conv.GrantVersion, "context_reset"); err != nil {
		return "暂时无法重置，请重试。"
	}
	return "已开始新会话：之前的对话不再提供给模型，待确认卡片已失效。规则、任务和运行记录不受影响。"
}

// Status reports service, agent and recent run state without the model.
func (r *Runtime) Status(bindingID string) string {
	lines := []string{}
	p, err := r.loadProvider()
	switch {
	case err != nil:
		lines = append(lines, "模型：状态读取失败")
	case !p.enabled:
		lines = append(lines, "模型：未启用（自然语言指令不可用；命令仍可用）")
	case !p.ready():
		lines = append(lines, "模型：已启用但工具调用检测未通过或已过期")
	default:
		lines = append(lines, "模型：可用（"+p.cfg.Model+"）")
	}
	b, err := interaction.BindingByID(r.Store.DB, bindingID)
	if err != nil || b.State != "active" {
		return strings.Join(append(lines, "绑定：无效"), "\n")
	}
	access, g, err := r.Mail.AgentIdentity(b.GrantID)
	if err != nil {
		lines = append(lines, "Agent 授权：已失效或过期，请管理员在 Web 重新授权")
	} else {
		lines = append(lines, fmt.Sprintf("Agent 授权：%s；有效期至 %s", strings.Join(g.Actions, "、"), time.Unix(g.Expires, 0).In(shanghai).Format("2006-01-02 15:04")))
	}
	var queued, running, pending int
	_ = r.Store.DB.QueryRow("SELECT COALESCE(sum(t.state='queued'),0),COALESCE(sum(t.state='running'),0) FROM agent_turns t JOIN agent_conversations c ON c.id=t.conversation_id WHERE c.binding_id=?", bindingID).Scan(&queued, &running)
	_ = r.Store.DB.QueryRow("SELECT count(*) FROM agent_proposals WHERE binding_id=? AND state='pending' AND expires>?", bindingID, time.Now().Unix()).Scan(&pending)
	lines = append(lines, fmt.Sprintf("Agent 轮次：处理中 %d，排队 %d；待确认方案 %d", running, queued, pending))
	if err == nil {
		rows, qerr := r.Store.DB.Query("SELECT task_run_id FROM agent_run_links WHERE binding_id=? ORDER BY created_at DESC LIMIT 5", bindingID)
		if qerr == nil {
			ids := []string{}
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			for _, id := range ids {
				if run, e := r.Mail.AgentTaskRun(access, id); e == nil {
					lines = append(lines, runSummary(run))
				}
			}
		}
	}
	return strings.Join(lines, "\n")
}

// CancelRun requests that a run stop its remaining items.
func (r *Runtime) CancelRun(bindingID, runID string) string {
	runID = strings.TrimSpace(runID)
	if len(runID) < 8 || len(runID) > 64 || strings.ContainsAny(runID, " \t\r\n") {
		return "用法：/cancel <run_id>（运行 ID 至少 8 位）"
	}
	b, err := interaction.BindingByID(r.Store.DB, bindingID)
	if err != nil || b.State != "active" {
		return "绑定无效。"
	}
	access, _, err := r.Mail.AgentIdentity(b.GrantID)
	if err != nil {
		return "Agent 授权已失效，请管理员在 Web 重新授权。"
	}
	if len(runID) < 32 {
		// Notifications show an 8-character prefix; resolve it uniquely.
		var full string
		var n int
		_ = r.Store.DB.QueryRow("SELECT count(*),COALESCE(max(id),'') FROM task_runs WHERE substr(id,1,?)=?", len(runID), runID).Scan(&n, &full)
		if n != 1 {
			return "找不到唯一匹配的运行，请提供完整运行 ID。"
		}
		runID = full
	}
	r.writeMu.Lock()
	run, err := r.Mail.AgentCancelTaskRun(access, runID)
	r.writeMu.Unlock()
	if err != nil {
		code := mailtools.CodeOf(err)
		if code.Code == "capability_unavailable" {
			return "该运行属于外部 AI 批次，首版不能从 Telegram 停止，请在原入口处理。"
		}
		return "无法停止该运行：" + code.Code
	}
	conv, cerr := interaction.ConversationOf(r.Store.DB, bindingID)
	if cerr == nil {
		now := time.Now().Unix()
		_, _ = r.Store.DB.Exec("INSERT OR IGNORE INTO agent_run_links(task_run_id,conversation_id,binding_id,created_at,updated_at) VALUES(?,?,?,?,?)", run.ID, conv.ID, bindingID, now, now)
	}
	return "已请求停止剩余项：" + runSummary(run) + "\n已完成的动作保留，不是回滚；正在执行的写入会按原有核验结束。"
}

var runStateLabels = map[string]string{"queued": "等待运行", "running": "运行中", "observed": "观察完成", "completed": "已核验完成", "noop": "本轮无变更", "partial": "部分完成/异常", "failed": "运行失败", "interrupted": "运行中断", "needs_verification": "待核验", "cancelled": "已取消", "limited": "达到本轮限制", "requires_ai": "部分邮件需外部 AI", "awaiting_ai": "等待外部 AI"}

// runSummary is fixed server text from the real run record. Unknown results
// are listed separately and never counted as success.
func runSummary(run mail.TaskRun) string {
	label := runStateLabels[run.State]
	if label == "" {
		label = run.State
	}
	if !run.Finalized && !(run.State == "queued" || run.State == "running") {
		label += "（仍有写入落定中）"
	}
	notDone := run.Queued + run.Running + run.Skipped
	text := fmt.Sprintf("任务「%s」运行 %s：%s\n已核验完成 %d，失败 %d，待核验（结果未知）%d，保护跳过 %d，未执行/已取消 %d，扫描 %d", run.TaskName, shortID(run.ID), label, run.Completed, run.Failed, run.Unknown, run.Protected, notDone, run.Scanned)
	if run.Mode == "observe" {
		text += "\n观察模式：只记录候选，不写原邮箱"
	}
	if run.HasMore {
		text += "\n仍有候选，下次运行继续"
	}
	if run.ErrorCode != "" {
		text += "\n错误码：" + run.ErrorCode
	}
	return text
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// MonitorRuns reports state changes of runs started or touched from
// Telegram, using only real run records. The final card is skipped when the
// task's own notification already goes to the same chat.
func (r *Runtime) MonitorRuns() {
	rows, err := r.Store.DB.Query("SELECT l.task_run_id,l.binding_id,l.last_state,c.context_epoch FROM agent_run_links l JOIN agent_conversations c ON c.id=l.conversation_id WHERE l.last_state NOT LIKE 'final:%' LIMIT 50")
	if err != nil {
		return
	}
	type link struct {
		run, binding, last string
		epoch              int64
	}
	links := []link{}
	for rows.Next() {
		var l link
		if rows.Scan(&l.run, &l.binding, &l.last, &l.epoch) == nil {
			links = append(links, l)
		}
	}
	rows.Close()
	for _, l := range links {
		b, err := interaction.BindingByID(r.Store.DB, l.binding)
		if err != nil || b.State != "active" {
			_, _ = r.Store.DB.Exec("UPDATE agent_run_links SET last_state='final:unbound',updated_at=? WHERE task_run_id=?", time.Now().Unix(), l.run)
			continue
		}
		access, g, err := r.Mail.AgentIdentity(b.GrantID)
		if err != nil {
			_, _ = r.Store.DB.Exec("UPDATE agent_run_links SET last_state='final:unauthorized',updated_at=? WHERE task_run_id=?", time.Now().Unix(), l.run)
			_ = interaction.Enqueue(r.Store.DB, r.Vault, b, interaction.Reply{SemanticKey: "run:" + l.run + ":unauthorized", Text: "Agent 授权已变化，不再推送该运行的结果。请在 Web 查看。", ContextEpoch: l.epoch, GrantVersion: interaction.CurrentGrantVersion(r.Store.DB, b.GrantID)})
			continue
		}
		run, err := r.Mail.AgentTaskRun(access, l.run)
		if err != nil {
			continue
		}
		active := run.State == "queued" || run.State == "running"
		if !active && run.Finalized {
			state := "final:" + run.State
			duplicate := r.notifiedElsewhere(b, run.ID)
			tx, err := r.Store.DB.Begin()
			if err != nil {
				continue
			}
			res, err := tx.Exec("UPDATE agent_run_links SET last_state=?,updated_at=? WHERE task_run_id=? AND last_state=?", state, time.Now().Unix(), l.run, l.last)
			if err == nil {
				if n, _ := res.RowsAffected(); n == 1 && !duplicate {
					err = interaction.Enqueue(tx, r.Vault, b, interaction.Reply{SemanticKey: "run:" + l.run + ":final", Text: "运行结束\n" + runSummary(run), ContextEpoch: l.epoch, GrantVersion: g.Version})
				}
			}
			if err == nil {
				_ = tx.Commit()
			} else {
				tx.Rollback()
			}
			continue
		}
		if run.State != l.last && run.State == "running" {
			_, _ = r.Store.DB.Exec("UPDATE agent_run_links SET last_state=?,updated_at=? WHERE task_run_id=?", run.State, time.Now().Unix(), l.run)
			_ = interaction.Enqueue(r.Store.DB, r.Vault, b, interaction.Reply{SemanticKey: "run:" + l.run + ":running", Text: "运行开始\n" + runSummary(run), ContextEpoch: l.epoch, GrantVersion: g.Version})
		}
	}
}

// notifiedElsewhere reports whether the task notification for this run is
// queued to the same chat, to avoid a duplicate final message.
func (r *Runtime) notifiedElsewhere(b interaction.Binding, runID string) bool {
	var n int
	_ = r.Store.DB.QueryRow("SELECT count(*) FROM notification_outbox o JOIN notification_channel c ON c.id=1 WHERE o.run_id=? AND json_extract(c.payload,'$.chat_id')=?", runID, fmt.Sprint(b.ChatID)).Scan(&n)
	return n > 0
}

func store_audit(tx *sql.Tx, action, target string) error {
	_, err := tx.Exec("INSERT INTO audits(at,action,target,outcome) VALUES(?,?,?,?)", time.Now().Unix(), action, target, "ok")
	return err
}

// RunCommand handles the deterministic "/run <task_id>" command: the user's
// own Telegram command is the server-side execution intent. It runs an
// enabled fixed-rule task once at its current version; the run key is bound
// to the inbox event, so a redelivered update returns the same run and a new
// /run message is a new run. Paused tasks are refused (confirmation via a
// card is required for them). Model-originated execution never comes here.
func (r *Runtime) RunCommand(bindingID, inputID, taskRef string) string {
	taskRef = strings.TrimSpace(taskRef)
	if len(taskRef) < 6 || len(taskRef) > 64 || strings.ContainsAny(taskRef, " \t\r\n") {
		return "用法：/run <task_id>（任务 ID 至少 6 位，可在 /status、Web 或对话中查看）"
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	b, err := interaction.BindingByID(r.Store.DB, bindingID)
	if err != nil || b.State != "active" {
		return "绑定无效。"
	}
	access, g, err := r.Mail.AgentIdentity(b.GrantID)
	if err != nil {
		return "Agent 授权已失效，请管理员在 Web 重新授权。"
	}
	p, err := r.loadProvider()
	if err != nil || !p.enabled {
		return "Agent 已停用，不能从 Telegram 执行任务。"
	}
	conv, err := interaction.EnsureConversation(r.Store.DB, bindingID, g.Version)
	if err != nil {
		return "暂时无法处理，请重试。"
	}
	var matches []mail.Task
	for _, account := range g.Accounts {
		tasks, err := r.Mail.Tasks(access, account)
		if err != nil {
			continue
		}
		for _, t := range tasks {
			if strings.HasPrefix(t.ID, taskRef) {
				matches = append(matches, t)
			}
		}
	}
	if len(matches) != 1 {
		return "找不到唯一匹配的任务，请提供更完整的任务 ID。"
	}
	t := matches[0]
	if t.Engine != "rules" {
		return "首版不从 Telegram 触发 AI 引擎任务。"
	}
	if !t.Enabled {
		return "任务「" + t.Name + "」当前暂停。执行暂停任务需要确认卡片：请用文字描述需求，确认后执行；或在 Web 手动运行。"
	}
	x := execution{BindingID: b.ID, GrantID: b.GrantID, GrantVersion: g.Version, AdminVersion: b.AdminVersion, ConfigVersion: p.version, ConvID: conv.ID, StopEpoch: conv.StopEpoch}
	guard := func(tx *sql.Tx) error {
		if fenceReason(tx, x, time.Now().Unix()) != "" {
			return errFenced
		}
		return nil
	}
	run, err := r.Mail.RunTaskGuarded(access, mail.TaskRunInput{TaskID: t.ID, Version: t.Version, RunKey: "agent:cmd:" + inputID}, "manual", guard)
	if err != nil {
		return "无法运行：" + mailtools.CodeOf(err).Code
	}
	now := time.Now().Unix()
	_, _ = r.Store.DB.Exec("INSERT OR IGNORE INTO agent_run_links(task_run_id,conversation_id,binding_id,created_at,updated_at) VALUES(?,?,?,?,?)", run.ID, conv.ID, bindingID, now, now)
	return "已按 /run 指令入队一次运行：\n" + runSummary(run) + "\n完成后推送真实结果；未知结果不算成功。"
}
