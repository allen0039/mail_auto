package mcpserver

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"mail-auto/internal/mail"
)

type TaskIDInput struct {
	TaskID string `json:"task_id"`
}
type TaskSaveInput struct {
	Task mail.Task `json:"task"`
}
type TaskEnabledInput struct {
	TaskID  string `json:"task_id"`
	Version int    `json:"version"`
	Enabled bool   `json:"enabled"`
}
type TaskPreviewInput struct {
	TaskID      string `json:"task_id"`
	Cursor      string `json:"cursor,omitempty"`
	OnlyMatched bool   `json:"only_matched,omitempty"`
}
type TaskRunIDInput struct {
	RunID string `json:"run_id"`
}

func addTasks(server *mcp.Server, s *mail.Service, a mail.Access) {
	add(server, s, a, "mail_task_list", "List up to 100 visible tasks in an authorized account. Modes manual/internal/external. Saving external instructions does not create an external schedule.", true, func(ctx context.Context, in AccountInput) (map[string]any, error) {
		out, e := s.Tasks(a, in.AccountID)
		return map[string]any{"tasks": out, "limit": 100}, e
	})
	add(server, s, a, "mail_task_get", "Read a task, version, limits, mode and internal schedule preview. No mail bodies or notification credentials.", true, func(ctx context.Context, in TaskIDInput) (map[string]any, error) {
		out, e := s.Task(a, in.TaskID)
		return map[string]any{"task": out}, e
	})
	add(server, s, a, "mail_task_upsert", "Save with task_manage permission. Empty id/version=0 creates a PAUSED OBSERVE draft. Updates need current version. Empty rule_ids follows all enabled account rules; rule_ids with rule_versions binds an explicit versioned set and cannot skip higher-priority rules in the task folders. Scope must fit administrator policy; internal scheduling needs prior administrator allow_internal. Never grants body or provider write permission.", false, func(ctx context.Context, in TaskSaveInput) (map[string]any, error) {
		out, e := s.SaveTask(a, in.Task)
		return map[string]any{"task": out}, e
	})
	add(server, s, a, "mail_task_set_enabled", "Enable or pause at expected version. Requires task_manage. Version changes invalidate pending task work. Does not grant provider writes or create an external client schedule.", false, func(ctx context.Context, in TaskEnabledInput) (map[string]any, error) {
		out, e := s.SetTaskEnabled(a, in.TaskID, in.Version, in.Enabled)
		return map[string]any{"task": out}, e
	})
	add(server, s, a, "mail_task_preview", "Preview up to 25 indexed messages with current rules, history, stable-delay and protections. No writes or body reads. Pass next_cursor to continue. only_matched=true scans up to 500 messages and returns only matched ones; counts cover this call only, not the whole folder. max_batch is the per-run limit. This metadata snapshot is not a freshness guarantee.", true, func(ctx context.Context, in TaskPreviewInput) (map[string]any, error) {
		out, e := s.PreviewTask(a, in.TaskID, in.Cursor, in.OnlyMatched)
		return map[string]any{"preview": out}, e
	})
	add(server, s, a, "mail_task_run", "Trigger an enabled external task with automation+read, current version and stable run_key per occurrence; retries reuse it. manual=true explicitly invokes a one-off run for any trigger mode, including paused tasks. Fixed rules run server-side within policy. AI tasks return run_id: mail_ai_claim with run_id/account_id, inspect batch pages, submit decisions, inspect plans, execute, poll mail_task_run_get. One bounded batch per run; later occurrences continue next_cursor. Never starts AI or external scheduling.", false, func(ctx context.Context, in mail.TaskRunInput) (map[string]any, error) {
		source := "external"
		if in.Manual {
			source = "manual"
		}
		out, e := s.RunTask(a, in, source)
		return map[string]any{"run": out, "ai_next": "AI: call mail_ai_claim with account_id,run_id,run_key. Fixed rules: poll run_get."}, e
	})
	add(server, s, a, "mail_task_run_get", "Read this client's run: state, verified counts, next_cursor and redacted notification status. Unknown outcomes require reconciliation; never retry a mail MOVE blindly.", true, func(ctx context.Context, in TaskRunIDInput) (map[string]any, error) {
		out, e := s.TaskRun(a, in.RunID)
		return map[string]any{"run": out}, e
	})
	add(server, s, a, "mail_task_runs", "List the latest 30 runs for a task owned by the caller. Old 0.3 independent batches remain through mail_ai_runs.", true, func(ctx context.Context, in TaskIDInput) (map[string]any, error) {
		out, e := s.TaskRuns(a, in.TaskID)
		return map[string]any{"runs": out, "limit": 30}, e
	})
	add(server, s, a, "mail_task_cancel", "Cancel this client's active run and stop not-yet-started actions. Completed mailbox changes remain; cancellation is not rollback.", false, func(ctx context.Context, in TaskRunIDInput) (map[string]any, error) {
		e := s.CancelTaskRun(a, in.RunID)
		return map[string]any{"cancelled": e == nil}, e
	})
}
