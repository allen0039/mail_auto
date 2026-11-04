package admin

import (
	"mail-auto/internal/mail"
	"net/http"
	"strings"
)

func (s *Server) tasksAPI(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if path != "/api/tasks" && !strings.HasPrefix(path, "/api/tasks/") && !strings.HasPrefix(path, "/api/task-runs/") && !strings.HasPrefix(path, "/api/notifications/") {
		return false
	}
	a, e := s.Mail.AdminAccess()
	if e != nil {
		operationError(w, e)
		return true
	}
	respond := func(v any, e error) {
		if e != nil {
			operationError(w, e)
		} else {
			writeJSON(w, 200, v)
		}
	}
	switch {
	case path == "/api/tasks" && r.Method == "GET":
		out, e := s.Mail.Tasks(a, r.URL.Query().Get("account_id"))
		respond(out, e)
	case path == "/api/tasks" && r.Method == "POST":
		var in mail.Task
		if decode(w, r, &in) {
			out, e := s.Mail.SaveTask(a, in)
			respond(out, e)
		}
	case strings.HasPrefix(path, "/api/tasks/"):
		parts := strings.Split(strings.TrimPrefix(path, "/api/tasks/"), "/")
		id := parts[0]
		if len(parts) == 1 && r.Method == "GET" {
			out, e := s.Mail.Task(a, id)
			respond(out, e)
		} else if len(parts) == 1 && r.Method == "PUT" {
			var in mail.Task
			if decode(w, r, &in) {
				in.ID = id
				out, e := s.Mail.SaveTask(a, in)
				respond(out, e)
			}
		} else if len(parts) == 2 && parts[1] == "preview" && r.Method == "POST" {
			var in struct {
				Cursor string `json:"cursor"`
			}
			if decode(w, r, &in) {
				out, e := s.Mail.PreviewTask(a, id, in.Cursor)
				respond(out, e)
			}
		} else if len(parts) == 2 && parts[1] == "enabled" && r.Method == "POST" {
			var in struct {
				Version int  `json:"version"`
				Enabled bool `json:"enabled"`
			}
			if decode(w, r, &in) {
				out, e := s.Mail.SetTaskEnabled(a, id, in.Version, in.Enabled)
				respond(out, e)
			}
		} else if len(parts) == 2 && parts[1] == "run" && r.Method == "POST" {
			var in mail.TaskRunInput
			if decode(w, r, &in) {
				in.TaskID = id
				out, e := s.Mail.RunTask(a, in, "manual")
				respond(out, e)
			}
		} else if len(parts) == 2 && parts[1] == "runs" && r.Method == "GET" {
			out, e := s.Mail.TaskRuns(a, id)
			respond(out, e)
		} else {
			fail(w, 405, "此接口不支持该方法")
		}
	case strings.HasPrefix(path, "/api/task-runs/"):
		parts := strings.Split(strings.TrimPrefix(path, "/api/task-runs/"), "/")
		if len(parts) == 1 && r.Method == "GET" {
			out, e := s.Mail.TaskRun(a, parts[0])
			respond(out, e)
		} else if len(parts) == 2 && parts[1] == "cancel" && r.Method == "POST" {
			e := s.Mail.CancelTaskRun(a, parts[0])
			respond(map[string]bool{"cancelled": e == nil}, e)
		} else {
			fail(w, 405, "此接口不支持该方法")
		}
	case path == "/api/notifications/telegram" && r.Method == "GET":
		out, e := s.Mail.Notifications(a)
		respond(out, e)
	case path == "/api/notifications/telegram" && r.Method == "PUT":
		var in mail.NotificationInput
		if decode(w, r, &in) {
			out, e := s.Mail.SaveNotifications(a, in)
			respond(out, e)
		}
	case (path == "/api/notifications/telegram/check" || path == "/api/notifications/telegram/test") && r.Method == "POST":
		out, e := s.Mail.CheckNotifications(r.Context(), a, strings.HasSuffix(path, "/test"))
		respond(out, e)
	case path == "/api/notifications/deliveries" && r.Method == "GET":
		out, e := s.Mail.NotificationDeliveries(a)
		respond(out, e)
	default:
		fail(w, 405, "此接口不支持该方法")
	}
	return true
}
