package admin

import (
	"mail-auto/internal/mail"
	"net/http"
	"strings"
)

func (s *Server) organizerAPI(w http.ResponseWriter, r *http.Request, session Session) bool {
	path := r.URL.Path
	if path == "/api/mcp/settings" || path == "/api/mcp/settings/check" {
		s.mcpSettingsAPI(w, r)
		return true
	}
	if path == "/api/remote-folders" && r.Method == "POST" {
		var in struct {
			AccountID string `json:"account_id"`
			Name      string `json:"name"`
		}
		if !decode(w, r, &in) {
			return true
		}
		a, e := s.Mail.AdminAccess()
		if e != nil {
			operationError(w, e)
			return true
		}
		out, e := s.Mail.CreateRemoteFolder(r.Context(), a, in.AccountID, in.Name)
		if e != nil {
			operationError(w, e)
		} else {
			writeJSON(w, 201, out)
		}
		return true
	}
	if strings.HasPrefix(path, "/api/oauth/requests/") {
		if s.requestMCP(r).handler == nil {
			fail(w, 503, "MCP 尚未启用")
		} else {
			s.oauthRequest(w, r, session)
		}
		return true
	}
	if path == "/api/mcp/tokens" {
		switch r.Method {
		case http.MethodGet:
			out, e := s.Mail.Tokens()
			if e != nil {
				operationError(w, e)
			} else {
				writeJSON(w, 200, out)
			}
		case http.MethodPost:
			var in mail.TokenInput
			if !decode(w, r, &in) {
				return true
			}
			out, secret, e := s.Mail.CreateToken(in)
			if e != nil {
				operationError(w, e)
			} else {
				writeJSON(w, 201, map[string]any{"token": out, "secret": secret})
			}
		default:
			fail(w, 405, "此接口不支持该方法")
		}
		return true
	}
	if strings.HasPrefix(path, "/api/mcp/tokens/") && r.Method == http.MethodDelete {
		e := s.Mail.RevokeToken(strings.TrimPrefix(path, "/api/mcp/tokens/"))
		if e != nil {
			operationError(w, e)
		} else {
			writeJSON(w, 200, map[string]bool{"revoked": true})
		}
		return true
	}
	if strings.HasPrefix(path, "/api/folder-settings/") {
		id := strings.TrimPrefix(path, "/api/folder-settings/")
		if strings.HasSuffix(id, "/probe") && r.Method == http.MethodPost {
			out, e := s.Mail.ProbeCapabilities(r.Context(), strings.TrimSuffix(id, "/probe"))
			if e != nil {
				operationError(w, e)
			} else {
				writeJSON(w, 200, out)
			}
			return true
		}
		if r.Method == http.MethodGet {
			out, e := s.Mail.FolderSettings(id)
			if e != nil {
				operationError(w, e)
			} else {
				writeJSON(w, 200, out)
			}
			return true
		}
		if r.Method == http.MethodPut {
			var in mail.FolderSettings
			if !decode(w, r, &in) {
				return true
			}
			in.AccountID = id
			e := s.Mail.SaveFolderSettings(r.Context(), in)
			if e != nil {
				operationError(w, e)
			} else {
				writeJSON(w, 200, map[string]bool{"saved": true})
			}
			return true
		}
		fail(w, 405, "此接口不支持该方法")
		return true
	}
	if path != "/api/plans" && !strings.HasPrefix(path, "/api/plans/") && path != "/api/operations" && !strings.HasPrefix(path, "/api/operations/") {
		return false
	}
	access, e := s.Mail.AdminAccess()
	if e != nil {
		operationError(w, e)
		return true
	}
	if path == "/api/plans" && r.Method == http.MethodPost {
		var in mail.PlanInput
		if !decode(w, r, &in) {
			return true
		}
		out, e := s.Mail.NewPlan(r.Context(), access, in)
		if e != nil {
			operationError(w, e)
		} else {
			writeJSON(w, 201, out)
		}
		return true
	}
	if strings.HasPrefix(path, "/api/plans/") {
		id := strings.TrimPrefix(path, "/api/plans/")
		if r.Method == http.MethodGet {
			out, e := s.Mail.GetPlan(access, id)
			if e != nil {
				operationError(w, e)
			} else {
				writeJSON(w, 200, out)
			}
			return true
		}
		if strings.HasSuffix(id, "/execute") && r.Method == http.MethodPost {
			var in struct {
				Key string `json:"idempotency_key"`
			}
			if !decode(w, r, &in) {
				return true
			}
			out, e := s.Mail.ExecutePlan(access, strings.TrimSuffix(id, "/execute"), in.Key)
			if e != nil {
				operationError(w, e)
			} else {
				writeJSON(w, 202, out)
			}
			return true
		}
	}
	if path == "/api/operations" && r.Method == http.MethodGet {
		out, e := s.Mail.Operations(access)
		if e != nil {
			operationError(w, e)
		} else {
			writeJSON(w, 200, out)
		}
		return true
	}
	if strings.HasPrefix(path, "/api/operations/") {
		tail := strings.TrimPrefix(path, "/api/operations/")
		parts := strings.Split(tail, "/")
		id := parts[0]
		if len(parts) == 1 && r.Method == http.MethodGet {
			out, e := s.Mail.Operation(access, id)
			if e != nil {
				operationError(w, e)
			} else {
				writeJSON(w, 200, out)
			}
			return true
		}
		if len(parts) == 2 && r.Method == http.MethodPost {
			switch parts[1] {
			case "cancel":
				e := s.Mail.CancelOperation(access, id)
				if e != nil {
					operationError(w, e)
				} else {
					writeJSON(w, 200, map[string]bool{"stopping": true})
				}
			case "reconcile":
				out, e := s.Mail.Reconcile(r.Context(), access, id)
				if e != nil {
					operationError(w, e)
				} else {
					writeJSON(w, 200, out)
				}
			case "undo":
				out, e := s.Mail.UndoPlan(r.Context(), access, id)
				if e != nil {
					operationError(w, e)
				} else {
					writeJSON(w, 200, out)
				}
			default:
				fail(w, 404, "接口不存在")
			}
			return true
		}
	}
	fail(w, 405, "此接口不支持该方法")
	return true
}
