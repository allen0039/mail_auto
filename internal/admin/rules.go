package admin

import (
	"net/http"
	"strings"

	"mail-auto/internal/mail"
)

func (s *Server) rulesAPI(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if path != "/api/rules" && !strings.HasPrefix(path, "/api/rules/") && !strings.HasPrefix(path, "/api/automation/") {
		return false
	}
	a, e := s.Mail.AdminAccess()
	if e != nil {
		operationError(w, e)
		return true
	}
	respond := func(status int, v any, e error) {
		if e != nil {
			operationError(w, e)
		} else {
			writeJSON(w, status, v)
		}
	}
	if path == "/api/rules" {
		switch r.Method {
		case "GET":
			out, e := s.Mail.Rules(a, r.URL.Query().Get("account_id"))
			respond(200, out, e)
		case "POST":
			var in mail.Rule
			if !decode(w, r, &in) {
				return true
			}
			out, e := s.Mail.SaveRule(a, in)
			respond(201, out, e)
		default:
			fail(w, 405, "此接口不支持该方法")
		}
		return true
	}
	if path == "/api/rules/preview" && r.Method == "POST" {
		var in mail.RulePreviewInput
		if !decode(w, r, &in) {
			return true
		}
		out, e := s.Mail.PreviewRules(a, in)
		respond(200, out, e)
		return true
	}
	if strings.HasPrefix(path, "/api/rules/") {
		id := strings.TrimPrefix(path, "/api/rules/")
		switch r.Method {
		case "GET":
			if strings.HasSuffix(id, "/history") {
				out, e := s.Mail.RuleHistory(a, strings.TrimSuffix(id, "/history"))
				respond(200, out, e)
			} else {
				out, e := s.Mail.Rule(a, id)
				respond(200, out, e)
			}
		case "PUT":
			var in mail.Rule
			if !decode(w, r, &in) {
				return true
			}
			in.ID = id
			out, e := s.Mail.SaveRule(a, in)
			respond(200, out, e)
		default:
			fail(w, 405, "此接口不支持该方法")
		}
		return true
	}
	if strings.HasPrefix(path, "/api/automation/policy/") {
		id := strings.TrimPrefix(path, "/api/automation/policy/")
		switch r.Method {
		case "GET":
			out, e := s.Mail.AutomationPolicy(a, id)
			respond(200, out, e)
		case "PUT":
			var in mail.AutomationPolicy
			if !decode(w, r, &in) {
				return true
			}
			in.AccountID = id
			out, e := s.Mail.SaveAutomationPolicy(a, in)
			respond(200, out, e)
		default:
			fail(w, 405, "此接口不支持该方法")
		}
		return true
	}
	if strings.HasPrefix(path, "/api/automation/capabilities/") && r.Method == "GET" {
		out, e := s.Mail.RuleCapabilities(a, strings.TrimPrefix(path, "/api/automation/capabilities/"))
		respond(200, out, e)
		return true
	}
	if path == "/api/automation/batches" && r.Method == "GET" {
		out, e := s.Mail.AIBatches(a, r.URL.Query().Get("account_id"))
		respond(200, out, e)
		return true
	}
	if strings.HasPrefix(path, "/api/automation/batches/") {
		id := strings.TrimPrefix(path, "/api/automation/batches/")
		if r.Method == "GET" {
			out, e := s.Mail.AIBatch(a, id)
			respond(200, out, e)
			return true
		}
		if strings.HasSuffix(id, "/cancel") && r.Method == "POST" {
			e := s.Mail.CancelAIBatch(a, strings.TrimSuffix(id, "/cancel"))
			respond(200, map[string]bool{"cancelled": e == nil}, e)
			return true
		}
	}
	fail(w, 405, "此接口不支持该方法")
	return true
}
