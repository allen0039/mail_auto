package admin

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mail-auto/internal/agent"
	"mail-auto/internal/mail"
	"mail-auto/internal/modelprovider"
	"mail-auto/internal/telegram"
)

// agentError returns a fixed error code with a Chinese message. Provider
// and Telegram raw bodies never reach the response.
func agentError(w http.ResponseWriter, e error) {
	var pe *modelprovider.Error
	var te *telegram.APIError
	switch {
	case errors.Is(e, agent.ErrToolLoopUnverified):
		writeJSON(w, 422, map[string]string{"code": "tool_call_unsupported", "error": "当前配置尚未通过工具闭环检测，不能启用 Agent；请先保存配置并运行“工具闭环测试”"})
	case errors.Is(e, telegram.ErrGrantRequired):
		writeJSON(w, 422, map[string]string{"code": "grant_required", "error": "请先保存并启用有效的 Agent 授权（授权须在当前管理员凭据下保存）"})
	case errors.Is(e, telegram.ErrCredentialMissing):
		writeJSON(w, 422, map[string]string{"code": "telegram_credential_missing", "error": "请先在「TG 通知」保存有效的 Bot Token"})
	case errors.As(e, &pe):
		writeJSON(w, 422, map[string]string{"code": pe.Code, "error": "模型服务检测失败：" + pe.Code})
	case errors.As(e, &te):
		writeJSON(w, 422, map[string]string{"code": te.Code, "error": "Telegram 检测失败：" + te.Code})
	case errors.Is(e, mail.ErrConflict):
		writeJSON(w, 409, map[string]string{"code": "state_conflict", "error": "配置已被修改，请刷新后重试"})
	case errors.Is(e, mail.ErrInput):
		writeJSON(w, 400, map[string]string{"code": "invalid_input", "error": "参数无效，请检查后重试（未勾选“全部文件夹”时至少选择一个具体文件夹）"})
	case errors.Is(e, mail.ErrDenied):
		writeJSON(w, 403, map[string]string{"code": "permission_denied", "error": "当前操作未授权"})
	case errors.Is(e, sql.ErrNoRows):
		writeJSON(w, 404, map[string]string{"code": "not_found", "error": "记录不存在"})
	default:
		writeJSON(w, 422, map[string]string{"code": "operation_failed", "error": "操作未完成，请稍后重试"})
	}
}

func (s *Server) agentAPI(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/api/agent/") && !strings.HasPrefix(path, "/api/telegram/") {
		return false
	}
	if s.Agent == nil || s.Telegram == nil {
		writeJSON(w, 503, map[string]string{"code": "agent_unavailable", "error": "Agent 未初始化"})
		return true
	}
	a, e := s.Mail.AdminAccess()
	if e != nil {
		agentError(w, e)
		return true
	}
	respond := func(v any, e error) {
		if e != nil {
			agentError(w, e)
		} else {
			writeJSON(w, 200, v)
		}
	}
	switch {
	case path == "/api/agent/provider" && r.Method == http.MethodGet:
		respond(s.Agent.Provider(a))
	case path == "/api/agent/provider" && r.Method == http.MethodPut:
		var in agent.ProviderInput
		if decode(w, r, &in) {
			respond(s.Agent.SaveProvider(a, in))
		}
	case path == "/api/agent/provider/check" && r.Method == http.MethodPost:
		var in struct {
			Kind string `json:"kind"`
		}
		if decode(w, r, &in) {
			out, code, e := s.Agent.CheckProvider(r.Context(), a, in.Kind)
			respond(map[string]any{"provider": out, "kind": in.Kind, "result": code, "ok": code == "ok"}, e)
		}
	case path == "/api/agent/grant" && r.Method == http.MethodGet:
		g, e := s.Mail.CurrentAgentGrant(a)
		if errors.Is(e, sql.ErrNoRows) {
			writeJSON(w, 200, map[string]any{"grant": nil, "actions": mail.AgentActions})
			return true
		}
		respond(map[string]any{"grant": g, "actions": mail.AgentActions}, e)
	case path == "/api/agent/grant" && r.Method == http.MethodPut:
		var in mail.AgentGrantInput
		if decode(w, r, &in) {
			// Folder scope must be explicit: all_folders names accounts with
			// every folder; others need at least one folder.
			if in.AllFolders == nil {
				writeJSON(w, 400, map[string]string{"code": "folder_scope_required", "error": "请为每个账号选择“全部文件夹”或至少一个具体文件夹"})
				return true
			}
			respond(s.Mail.SaveAgentGrant(a, in))
		}
	case path == "/api/agent/grant/revoke" && r.Method == http.MethodPost:
		var in struct{}
		if decode(w, r, &in) {
			e := s.Mail.RevokeAgentGrant(a)
			respond(map[string]bool{"revoked": e == nil}, e)
		}
	case path == "/api/telegram/interaction" && r.Method == http.MethodGet:
		respond(s.Telegram.Get(a))
	case path == "/api/telegram/interaction" && r.Method == http.MethodPut:
		var in struct {
			Version int  `json:"version"`
			Enabled bool `json:"enabled"`
		}
		if decode(w, r, &in) {
			respond(s.Telegram.SetEnabled(r.Context(), a, in.Version, in.Enabled))
		}
	case path == "/api/telegram/bind-code" && r.Method == http.MethodPost:
		var in struct{}
		if decode(w, r, &in) {
			code, expires, e := s.Telegram.BindCode(a)
			respond(map[string]any{"code": code, "command": "/bind " + code, "expires": expires}, e)
		}
	case path == "/api/telegram/unbind" && r.Method == http.MethodPost:
		var in struct{}
		if decode(w, r, &in) {
			e := s.Telegram.Unbind(a)
			respond(map[string]bool{"unbound": e == nil}, e)
		}
	case path == "/api/agent/conversations" && r.Method == http.MethodGet:
		s.agentConversations(w, r)
	case strings.HasPrefix(path, "/api/agent/turns/"):
		parts := strings.Split(strings.TrimPrefix(path, "/api/agent/turns/"), "/")
		if len(parts) == 1 && r.Method == http.MethodGet {
			s.agentTurn(w, r, parts[0])
		} else if len(parts) == 2 && parts[1] == "stop" && r.Method == http.MethodPost {
			var binding string
			if e := s.Auth.Store.DB.QueryRowContext(r.Context(), "SELECT c.binding_id FROM agent_turns t JOIN agent_conversations c ON c.id=t.conversation_id WHERE t.id=?", parts[0]).Scan(&binding); e != nil {
				agentError(w, e)
				return true
			}
			writeJSON(w, 200, map[string]string{"result": s.Agent.Stop(binding)})
		} else {
			fail(w, 405, "此接口不支持该方法")
		}
	default:
		fail(w, 405, "此接口不支持该方法")
	}
	return true
}

// agentConversations lists recent turns, proposals and linked runs as
// redacted summaries. Message content is never included.
func (s *Server) agentConversations(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 || offset > 10000 {
		fail(w, 400, "无效页码")
		return
	}
	type turn struct {
		ID            string `json:"id"`
		Seq           int    `json:"seq"`
		State         string `json:"state"`
		ErrorCode     string `json:"error_code"`
		ModelRequests int    `json:"model_requests"`
		ToolCalls     int    `json:"tool_calls"`
		CreatedAt     int64  `json:"created_at"`
		UpdatedAt     int64  `json:"updated_at"`
	}
	rows, e := s.Auth.Store.DB.QueryContext(r.Context(), "SELECT id,seq,state,error_code,model_requests,tool_calls,created_at,updated_at FROM agent_turns ORDER BY created_at DESC,seq DESC LIMIT 30 OFFSET ?", offset)
	if e != nil {
		fail(w, 500, "读取失败")
		return
	}
	turns := []turn{}
	for rows.Next() {
		var t turn
		if rows.Scan(&t.ID, &t.Seq, &t.State, &t.ErrorCode, &t.ModelRequests, &t.ToolCalls, &t.CreatedAt, &t.UpdatedAt) == nil {
			turns = append(turns, t)
		}
	}
	rows.Close()
	type proposal struct {
		ID        string `json:"id"`
		TurnID    string `json:"turn_id"`
		Kind      string `json:"kind"`
		State     string `json:"state"`
		ErrorCode string `json:"error_code"`
		Expires   int64  `json:"expires"`
		CreatedAt int64  `json:"created_at"`
	}
	props := []proposal{}
	if rows, e = s.Auth.Store.DB.QueryContext(r.Context(), "SELECT id,turn_id,kind,state,error_code,expires,created_at FROM agent_proposals ORDER BY created_at DESC LIMIT 30"); e == nil {
		for rows.Next() {
			var p proposal
			if rows.Scan(&p.ID, &p.TurnID, &p.Kind, &p.State, &p.ErrorCode, &p.Expires, &p.CreatedAt) == nil {
				props = append(props, p)
			}
		}
		rows.Close()
	}
	type link struct {
		RunID      string `json:"task_run_id"`
		TurnID     string `json:"turn_id"`
		ProposalID string `json:"proposal_id"`
		LastState  string `json:"last_state"`
		RunState   string `json:"run_state"`
		TaskID     string `json:"task_id"`
		CreatedAt  int64  `json:"created_at"`
	}
	links := []link{}
	if rows, e = s.Auth.Store.DB.QueryContext(r.Context(), "SELECT l.task_run_id,l.turn_id,l.proposal_id,l.last_state,COALESCE(r.state,''),COALESCE(r.task_id,''),l.created_at FROM agent_run_links l LEFT JOIN task_runs r ON r.id=l.task_run_id ORDER BY l.created_at DESC LIMIT 30"); e == nil {
		for rows.Next() {
			var l link
			if rows.Scan(&l.RunID, &l.TurnID, &l.ProposalID, &l.LastState, &l.RunState, &l.TaskID, &l.CreatedAt) == nil {
				links = append(links, l)
			}
		}
		rows.Close()
	}
	var queued int
	_ = s.Auth.Store.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM agent_turns WHERE state='queued'").Scan(&queued)
	writeJSON(w, 200, map[string]any{"turns": turns, "proposals": props, "run_links": links, "queued": queued, "offset": offset})
}

// agentTurn shows one turn's redacted state: tool names, states, result
// codes, server action IDs and committed business targets.
func (s *Server) agentTurn(w http.ResponseWriter, r *http.Request, id string) {
	var state, code string
	var created int64
	if e := s.Auth.Store.DB.QueryRowContext(r.Context(), "SELECT state,error_code,created_at FROM agent_turns WHERE id=?", id).Scan(&state, &code, &created); e != nil {
		agentError(w, e)
		return
	}
	type call struct {
		Ordinal    int    `json:"ordinal"`
		Name       string `json:"name"`
		Risk       string `json:"risk"`
		State      string `json:"state"`
		ResultCode string `json:"result_code"`
		ActionID   string `json:"action_id"`
		Target     string `json:"target_id,omitempty"`
		Version    int    `json:"target_version,omitempty"`
	}
	calls := []call{}
	rows, e := s.Auth.Store.DB.QueryContext(r.Context(), "SELECT t.ordinal,t.name,t.risk,t.state,t.result_code,t.action_id,COALESCE(m.target_id,''),COALESCE(m.target_version,0) FROM agent_tool_calls t LEFT JOIN agent_mutations m ON m.mutation_id='action:'||t.action_id WHERE t.turn_id=? ORDER BY t.ordinal", id)
	if e == nil {
		for rows.Next() {
			var c call
			if rows.Scan(&c.Ordinal, &c.Name, &c.Risk, &c.State, &c.ResultCode, &c.ActionID, &c.Target, &c.Version) == nil {
				calls = append(calls, c)
			}
		}
		rows.Close()
	}
	type step struct {
		ProposalID string `json:"proposal_id"`
		Ordinal    int    `json:"ordinal"`
		Kind       string `json:"kind"`
		State      string `json:"state"`
		Target     string `json:"target_id"`
		Version    int    `json:"target_version"`
		ErrorCode  string `json:"error_code"`
	}
	steps := []step{}
	if rows, e = s.Auth.Store.DB.QueryContext(r.Context(), "SELECT s.proposal_id,s.ordinal,s.kind,s.state,s.target_id,s.target_version,s.error_code FROM agent_proposal_steps s JOIN agent_proposals p ON p.id=s.proposal_id WHERE p.turn_id=? ORDER BY s.proposal_id,s.ordinal", id); e == nil {
		for rows.Next() {
			var st step
			if rows.Scan(&st.ProposalID, &st.Ordinal, &st.Kind, &st.State, &st.Target, &st.Version, &st.ErrorCode) == nil {
				steps = append(steps, st)
			}
		}
		rows.Close()
	}
	writeJSON(w, 200, map[string]any{"id": id, "state": state, "error_code": code, "created_at": time.Unix(created, 0).Unix(), "tool_calls": calls, "proposal_steps": steps})
}
