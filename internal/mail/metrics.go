package mail

import "context"

// AutomationMetrics is intentionally numeric and contains no message content,
// credentials or task names. It is safe to expose through an authenticated
// status endpoint and useful for an external readiness/alerting loop.
type AutomationMetrics struct {
	ActiveRuns           int `json:"active_runs"`
	QueuedRuns           int `json:"queued_runs"`
	AwaitingAI           int `json:"awaiting_ai"`
	NeedsVerification    int `json:"needs_verification"`
	PendingNotifications int `json:"pending_notifications"`
}

func (s *Service) AutomationMetrics(ctx context.Context) (AutomationMetrics, error) {
	var out AutomationMetrics
	queries := []struct {
		dest *int
		q    string
	}{
		{&out.ActiveRuns, "SELECT count(*) FROM task_runs WHERE state IN ('running','awaiting_ai')"},
		{&out.QueuedRuns, "SELECT count(*) FROM task_runs WHERE state='queued'"},
		{&out.AwaitingAI, "SELECT count(*) FROM task_runs WHERE state='awaiting_ai'"},
		{&out.NeedsVerification, "SELECT count(*) FROM operation_items WHERE state='needs_verification'"},
		{&out.PendingNotifications, "SELECT count(*) FROM notification_outbox WHERE state IN ('pending','retry','sending')"},
	}
	for _, item := range queries {
		if err := s.Store.DB.QueryRowContext(ctx, item.q).Scan(item.dest); err != nil {
			return AutomationMetrics{}, err
		}
	}
	return out, nil
}
