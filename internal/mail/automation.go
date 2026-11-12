package mail

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

var automationLocation = time.FixedZone("Asia/Shanghai", 8*3600)

var ErrDailyLimit = &operationErrorCode{"daily_limit_reached"}

type AIClaimInput struct {
	RunID     string `json:"run_id,omitempty"`
	AccountID string `json:"account_id"`
	RunKey    string `json:"run_key"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}
type AIDecision struct {
	Ref         string `json:"ref"`
	Signature   string `json:"signature"`
	Action      string `json:"action"`
	Destination string `json:"destination,omitempty"`
	Reason      string `json:"reason"`
}
type AISubmitInput struct {
	BatchID        string       `json:"batch_id"`
	IdempotencyKey string       `json:"idempotency_key"`
	Decisions      []AIDecision `json:"decisions"`
}
type AIBatch struct {
	RunID         string           `json:"run_id,omitempty"`
	WorkerID      string           `json:"worker_id,omitempty"`
	Protected     int              `json:"protected"`
	Unmatched     int              `json:"unmatched"`
	ID            string           `json:"id"`
	AccountID     string           `json:"account_id"`
	Owner         string           `json:"owner"`
	Version       int              `json:"admin_version"`
	RunKey        string           `json:"run_key"`
	PolicyVersion int              `json:"policy_version"`
	RulesDigest   string           `json:"rules_digest"`
	Mode          string           `json:"mode"`
	State         string           `json:"state"`
	CreatedAt     int64            `json:"created_at"`
	Expires       int64            `json:"expires"`
	NextCursor    string           `json:"next_cursor,omitempty"`
	Scanned       int              `json:"scanned"`
	Items         []RuleEvaluation `json:"items"`
	Decisions     []AIDecision     `json:"decisions"`
	PlanIDs       []string         `json:"plan_ids"`
	SubmitKey     string           `json:"submit_key,omitempty"`
	SubmitDigest  string           `json:"submit_digest,omitempty"`
}
type AutomationGuard struct {
	BatchID       string `json:"batch_id"`
	PolicyVersion int    `json:"policy_version"`
	RulesDigest   string `json:"rules_digest"`
	Signature     string `json:"signature"`
	RuleID        string `json:"rule_id"`
	RuleVersion   int    `json:"rule_version"`
}

func rulesDigest(rules []Rule) string { return digest(rules) }
func claimFingerprint(item RuleEvaluation, p AutomationPolicy, rules []Rule) string {
	if p.taskDigest != "" {
		return digest([]any{item.Signature, p.Version, rulesDigest(rules), item.RuleID, item.RuleVersion, p.taskDigest})
	}
	return digest([]any{item.Signature, p.Version, rulesDigest(rules), item.RuleID, item.RuleVersion})
}
func (s *Service) loadAIBatch(id string) (AIBatch, error) {
	var raw string
	e := s.Store.DB.QueryRow("SELECT payload FROM automation_batches WHERE id=?", id).Scan(&raw)
	if e != nil {
		return AIBatch{}, e
	}
	var b AIBatch
	if json.Unmarshal([]byte(raw), &b) != nil {
		return b, ErrConflict
	}
	return b, nil
}
func (s *Service) AIBatch(a Access, id string) (AIBatch, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return AIBatch{}, e
	}
	b, e := s.loadAIBatch(id)
	if e != nil {
		return b, e
	}
	if a.Owner != "admin" && (b.Owner != a.Owner || !a.Allows(b.AccountID, "", "automation")) {
		return AIBatch{}, ErrDenied
	}
	for _, item := range b.Items {
		if !a.Allows(b.AccountID, item.Message.Folder, "read") {
			return AIBatch{}, ErrDenied
		}
	}
	return b, nil
}
func (s *Service) AIBatches(a Access, accountID string) ([]AIBatch, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return nil, e
	}
	if !a.Allows(accountID, "", "read") {
		return nil, ErrDenied
	}
	rows, e := s.Store.DB.Query("SELECT id FROM automation_batches WHERE account_id=? AND (owner=? OR ?='admin') ORDER BY created_at DESC,id DESC LIMIT 30", accountID, a.Owner, a.Owner)
	if e != nil {
		return nil, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			break
		}
		ids = append(ids, id)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return nil, e
	}
	out := []AIBatch{}
	for _, id := range ids {
		b, e := s.AIBatch(a, id)
		if e != nil {
			return nil, e
		}
		out = append(out, b)
	}
	return out, nil
}
func (s *Service) claimAvailable(accountID, ref, fingerprint string, now int64) (bool, error) {
	var oldFingerprint, state, plan string
	var expires int64
	e := s.Store.DB.QueryRow("SELECT fingerprint,state,expires,plan_id FROM automation_claims WHERE account_id=? AND ref=?", accountID, ref).Scan(&oldFingerprint, &state, &expires, &plan)
	if e == sql.ErrNoRows {
		return true, nil
	}
	if e != nil {
		return false, e
	}
	if plan != "" {
		var opState string
		e = s.Store.DB.QueryRow("SELECT state FROM operations WHERE plan_id=?", plan).Scan(&opState)
		if e == nil {
			if slices.Contains([]string{"queued", "running", "needs_verification"}, opState) {
				return false, nil
			}
			return oldFingerprint != fingerprint, nil
		}
		if e != sql.ErrNoRows {
			return false, e
		}
	}
	if state == "done" && oldFingerprint == fingerprint && expires > now {
		return false, nil
	}
	if slices.Contains([]string{"claimed", "planned"}, state) && expires > now {
		return false, nil
	}
	return true, nil
}
func (s *Service) ClaimAI(a Access, in AIClaimInput) (AIBatch, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	a, p, e := s.authorizedAutomation(a, in.AccountID)
	if e != nil {
		return AIBatch{}, e
	}
	var rules []Rule
	if in.RunID != "" {
		p, e = s.taskBatchPolicy(a, p, in.RunID)
		if e != nil {
			return AIBatch{}, e
		}
		r, err := s.loadTaskRun(in.RunID)
		if err != nil {
			return AIBatch{}, err
		}
		if r.AccountID != in.AccountID {
			return AIBatch{}, ErrDenied
		}
		t, err := s.loadTask(r.TaskID)
		if err != nil {
			return AIBatch{}, err
		}
		rules, err = s.taskRules(a, t)
		if err != nil {
			return AIBatch{}, err
		}
		if in.Cursor != "" && in.Cursor != r.NextCursor {
			return AIBatch{}, ErrInput
		}
		in.Cursor = r.NextCursor
		in.RunKey = "taskrun:" + in.RunID
		ctx := s.Context
		if ctx == nil {
			ctx = context.Background()
		}
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err = s.ensureTaskIndex(ctx, p)
		cancel()
		if err != nil {
			return AIBatch{}, err
		}
	}
	if in.RunKey == "" || len(in.RunKey) > 128 || strings.ContainsAny(in.RunKey, "\x00\r\n") || in.Limit < 0 {
		return AIBatch{}, ErrInput
	}
	var existing string
	e = s.Store.DB.QueryRow("SELECT id FROM automation_batches WHERE owner=? AND run_key=?", a.Owner, in.RunKey).Scan(&existing)
	if e == nil {
		b, e := s.AIBatch(a, existing)
		if e == nil && (b.AccountID != in.AccountID || b.RunID != in.RunID) {
			return b, ErrConflict
		}
		return b, e
	}
	if e != sql.ErrNoRows {
		return AIBatch{}, e
	}
	if in.RunID == "" {
		rules, e = s.rulesForAccount(in.AccountID)
		if e != nil {
			return AIBatch{}, e
		}
	}
	for _, r := range rules {
		if r.Enabled && !ruleVisible(a, r) {
			return AIBatch{}, ErrDenied
		}
	}
	limit := in.Limit
	if limit == 0 {
		limit = p.MaxBatch
	}
	if limit > p.MaxBatch {
		return AIBatch{}, ErrInput
	}
	now := time.Now().Unix()
	b := AIBatch{ID: credentials.Random(24), AccountID: in.AccountID, Owner: a.Owner, Version: a.Version, RunKey: in.RunKey, PolicyVersion: p.Version, RulesDigest: rulesDigest(rules), Mode: p.Mode, State: "claimed", CreatedAt: now, Expires: now + 900, Items: []RuleEvaluation{}, Decisions: []AIDecision{}, PlanIDs: []string{}}
	b.RunID = in.RunID
	if b.RunID != "" {
		r, err := s.loadTaskRun(b.RunID)
		if err != nil {
			return b, err
		}
		t, err := s.loadTask(r.TaskID)
		if err != nil {
			return b, err
		}
		if t.Engine == "rules" {
			b.WorkerID = s.workerID
		}
		b.Expires = r.Expires
	}
	cursor := in.Cursor
	for b.Scanned < 500 && len(b.Items) < limit {
		messages, _, next, err := s.ruleCandidates(a, in.AccountID, p.SourceFolders, cursor, min(100, limit-len(b.Items), 500-b.Scanned))
		if err != nil {
			return b, err
		}
		for _, m := range messages {
			b.Scanned++
			item := evaluateRules(m, rules, p, now)
			if item.Status != "matched" {
				if item.Status == "protected" || item.Status == "unknown" {
					b.Protected++
				} else {
					b.Unmatched++
				}
				continue
			}
			var pending int
			if e = s.Store.DB.QueryRow("SELECT count(*) FROM operation_items WHERE ref=? AND state IN ('queued','running','needs_verification')", m.Ref).Scan(&pending); e != nil {
				return b, e
			}
			if pending > 0 {
				continue
			}
			ok, e := s.claimAvailable(in.AccountID, m.Ref, claimFingerprint(item, p, rules), now)
			if e != nil {
				return b, e
			}
			if ok {
				b.Items = append(b.Items, item)
			}
		}
		b.NextCursor = next
		if next == "" {
			break
		}
		cursor = next
	}
	if len(b.Items) == 0 {
		b.State = "empty"
	}
	raw, _ := json.Marshal(b)
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return b, e
	}
	defer tx.Rollback()
	// Claim checks and publication are fenced by an immediate SQLite transaction,
	// including conflicts originating from a second process sharing this DB.
	for _, item := range b.Items {
		var n int
		if e = tx.QueryRow("SELECT count(*) FROM automation_claims WHERE account_id=? AND ref=? AND ((state IN ('claimed','planned') AND expires>? AND (plan_id='' OR plan_id NOT IN (SELECT plan_id FROM operations WHERE state IN ('completed','failed','partial','cancelled')))) OR plan_id IN (SELECT plan_id FROM operations WHERE state IN ('queued','running','needs_verification')))", b.AccountID, item.Message.Ref, now).Scan(&n); e != nil {
			return b, e
		}
		if n > 0 {
			return b, ErrBusy
		}
		if e = tx.QueryRow("SELECT count(*) FROM operation_items WHERE ref=? AND state IN ('queued','running','needs_verification')", item.Message.Ref).Scan(&n); e != nil {
			return b, e
		}
		if n > 0 {
			return b, ErrBusy
		}
	}
	if len(b.Items) > 0 {
		day := time.Now().In(automationLocation).Format("2006-01-02")
		if _, e = tx.Exec("INSERT OR IGNORE INTO automation_daily(account_id,day,count,claimed) VALUES(?,?,0,0)", b.AccountID, day); e != nil {
			return b, e
		}
		if _, e = tx.Exec("UPDATE automation_daily SET claimed=claimed+? WHERE account_id=? AND day=?", len(b.Items), b.AccountID, day); e != nil {
			return b, e
		}
	}
	if _, e = tx.Exec("INSERT INTO automation_batches VALUES(?,?,?,?,?,?,?,?)", b.ID, b.AccountID, b.Owner, b.RunKey, string(raw), b.State, b.CreatedAt, b.Expires); e != nil {
		return b, e
	}
	if b.RunID != "" {
		if _, e = tx.Exec("INSERT INTO task_run_batches(run_id,batch_id) VALUES(?,?)", b.RunID, b.ID); e != nil {
			return b, ErrConflict
		}
	}
	for _, item := range b.Items {
		_, e = tx.Exec(`INSERT INTO automation_claims VALUES(?,?,?,?,'claimed',?,'') ON CONFLICT(account_id,ref) DO UPDATE SET fingerprint=excluded.fingerprint,batch_id=excluded.batch_id,state='claimed',expires=excluded.expires,plan_id=''`, b.AccountID, item.Message.Ref, claimFingerprint(item, p, rules), b.ID, b.Expires)
		if e != nil {
			return b, e
		}
	}
	if e = store.Audit(tx, "mail.ai.claim", b.ID, "ok"); e != nil {
		return b, e
	}
	return b, tx.Commit()
}
func (s *Service) checkAIBatch(a Access, b AIBatch) (Access, AutomationPolicy, []Rule, error) {
	a, p, e := s.authorizedAutomation(a, b.AccountID)
	if e != nil {
		return a, p, nil, e
	}
	var rules []Rule
	if b.RunID != "" {
		p, e = s.taskBatchPolicy(a, p, b.RunID)
		if e != nil {
			return a, p, nil, e
		}
		r, err := s.loadTaskRun(b.RunID)
		if err != nil {
			return a, p, nil, err
		}
		t, err := s.loadTask(r.TaskID)
		if err != nil {
			return a, p, nil, err
		}
		rules, err = s.taskRules(a, t)
		if err != nil {
			return a, p, nil, err
		}
		if t.Engine == "rules" {
			var owner string
			if err = s.Store.DB.QueryRow("SELECT lease_owner FROM task_runs WHERE id=?", r.ID).Scan(&owner); err != nil {
				return a, p, nil, err
			}
			if b.WorkerID != owner || owner != s.workerID {
				return a, p, nil, ErrConflict
			}
		}
	}
	if b.Owner != a.Owner || b.Version != a.Version || b.PolicyVersion != p.Version || b.Expires <= time.Now().Unix() || b.State == "cancelled" {
		return a, p, nil, ErrConflict
	}
	if b.RunID == "" {
		rules, e = s.rulesForAccount(b.AccountID)
		if e != nil {
			return a, p, nil, e
		}
	}
	if rulesDigest(rules) != b.RulesDigest {
		return a, p, rules, ErrConflict
	}
	return a, p, rules, nil
}
func (s *Service) SubmitAI(ctx context.Context, a Access, in AISubmitInput) (AIBatch, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	b, e := s.AIBatch(a, in.BatchID)
	if e != nil {
		return b, e
	}
	if in.IdempotencyKey == "" || len(in.IdempotencyKey) > 128 || len(in.Decisions) > 50 {
		return b, ErrInput
	}
	if b.SubmitKey != "" {
		if b.SubmitKey != in.IdempotencyKey || b.SubmitDigest != digest(in.Decisions) {
			return b, ErrConflict
		}
		return b, nil
	}
	a, p, rules, e := s.checkAIBatch(a, b)
	if e != nil {
		return b, e
	}
	if b.State != "claimed" || len(in.Decisions) != len(b.Items) {
		return b, ErrInput
	}
	decisions := map[string]AIDecision{}
	for _, d := range in.Decisions {
		if _, ok := decisions[d.Ref]; ok || len(d.Reason) < 1 || len(d.Reason) > 500 || !slices.Contains([]string{"keep", "move", "archive", "trash"}, d.Action) {
			return b, ErrInput
		}
		decisions[d.Ref] = d
	}
	newPlans := []string{}
	committed := false
	defer func() {
		if !committed {
			for _, id := range newPlans {
				s.Store.DB.Exec("DELETE FROM operation_plans WHERE id=? AND id NOT IN(SELECT plan_id FROM operations)", id)
			}
		}
	}()
	plansByRef := map[string]string{}
	for _, item := range b.Items {
		d, ok := decisions[item.Message.Ref]
		if !ok || d.Signature != item.Signature {
			return b, ErrConflict
		}
		m, e := s.AllowedMessage(a, d.Ref, "read")
		if e != nil {
			return b, e
		}
		if messageSignature(m) != item.Signature || policyProtection(p, m, time.Now().Unix()) != "" {
			return b, ErrConflict
		}
		var claimBatch string
		if e = s.Store.DB.QueryRow("SELECT batch_id FROM automation_claims WHERE account_id=? AND ref=?", b.AccountID, d.Ref).Scan(&claimBatch); e != nil || claimBatch != b.ID {
			return b, ErrConflict
		}
		evaluated := evaluateRules(m, rules, p, time.Now().Unix())
		if evaluated.Status != "matched" || evaluated.RuleID != item.RuleID {
			return b, ErrConflict
		}
		if d.Action == "keep" {
			if d.Destination != "" {
				return b, ErrInput
			}
			continue
		}
		if !slices.Contains(p.Actions, d.Action) {
			return b, ErrDenied
		}
		if item.Action != "ai" && (d.Action != item.Action || d.Destination != item.Destination) {
			return b, ErrDenied
		}
		if d.Action == "move" {
			if !slices.Contains(p.Destinations, d.Destination) || !a.Allows(b.AccountID, d.Destination, "read") {
				return b, ErrDenied
			}
		} else if d.Destination != "" {
			return b, ErrInput
		}
		if p.Mode == "execute" {
			plan, e := s.NewPlan(ctx, a, PlanInput{Refs: []string{d.Ref}, Action: d.Action, Destination: d.Destination})
			if e != nil {
				return b, e
			}
			newPlans = append(newPlans, plan.ID)
			plan.Automation = &AutomationGuard{BatchID: b.ID, PolicyVersion: p.Version, RulesDigest: b.RulesDigest, Signature: item.Signature, RuleID: item.RuleID, RuleVersion: item.RuleVersion}
			raw, _ := json.Marshal(plan)
			if _, e = s.Store.DB.Exec("UPDATE operation_plans SET payload=? WHERE id=?", string(raw), plan.ID); e != nil {
				return b, e
			}
			plansByRef[d.Ref] = plan.ID
		}
	}
	b.SubmitKey = in.IdempotencyKey
	b.SubmitDigest = digest(in.Decisions)
	b.Decisions = in.Decisions
	b.PlanIDs = newPlans
	b.State = "submitted"
	if p.Mode == "observe" {
		b.State = "observed"
	}
	raw, _ := json.Marshal(b)
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return b, e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("UPDATE automation_batches SET payload=?,state=? WHERE id=?", string(raw), b.State, b.ID); e != nil {
		return b, e
	}
	for _, item := range b.Items {
		state := "done"
		planID := plansByRef[item.Message.Ref]
		expires := nextAutomationDay()
		if planID != "" {
			state = "planned"
			expires = b.Expires
		}
		if _, e = tx.Exec("UPDATE automation_claims SET state=?,plan_id=?,expires=? WHERE account_id=? AND ref=? AND batch_id=?", state, planID, expires, b.AccountID, item.Message.Ref, b.ID); e != nil {
			return b, e
		}
	}
	if e = store.Audit(tx, "mail.ai.submit", b.ID, b.State); e != nil {
		return b, e
	}
	e = tx.Commit()
	committed = e == nil
	return b, e
}
func (s *Service) CancelAIBatch(a Access, id string) error {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	b, e := s.AIBatch(a, id)
	if e != nil {
		return e
	}
	b.State = "cancelled"
	raw, _ := json.Marshal(b)
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("UPDATE automation_batches SET state='cancelled',payload=? WHERE id=?", string(raw), id); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE automation_claims SET state='done',expires=0 WHERE batch_id=?", id); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE operations SET cancelled=1 WHERE plan_id IN(SELECT plan_id FROM automation_claims WHERE batch_id=?)", id); e != nil {
		return e
	}
	if e = store.Audit(tx, "mail.ai.cancel", id, "ok"); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Service) AIGet(ctx context.Context, a Access, batchID, ref string, body bool) (Content, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	b, e := s.AIBatch(a, batchID)
	if e != nil {
		return Content{}, e
	}
	a, p, _, e := s.checkAIBatch(a, b)
	if e != nil {
		return Content{}, e
	}
	if b.State != "claimed" {
		return Content{}, ErrConflict
	}
	var item *RuleEvaluation
	for i := range b.Items {
		if b.Items[i].Message.Ref == ref {
			item = &b.Items[i]
			break
		}
	}
	if item == nil {
		return Content{}, ErrDenied
	}
	check := func(m Message) error {
		if _, _, _, err := s.checkAIBatch(a, b); err != nil {
			return err
		}
		if messageSignature(m) != item.Signature {
			return ErrConflict
		}
		return nil
	}
	m, e := s.AllowedMessage(a, ref, "read")
	if e != nil {
		return Content{}, e
	}
	if e = check(m); e != nil {
		return Content{}, e
	}
	if !body {
		return Content{Message: m}, nil
	}
	if !p.AllowBody {
		return Content{}, ErrDenied
	}
	return s.read(ctx, ref, check)
}

// A fresh metadata read is required before automated writes; it never reads a body.
type RuleStateRemote interface {
	RuleState(context.Context, Account, string, Message) (Message, error)
}

func (s *Service) validateAutomationPlan(a Access, p Plan, m Message) (Access, AutomationPolicy, error) {
	g := p.Automation
	if g == nil {
		return a, AutomationPolicy{}, ErrInput
	}
	b, e := s.AIBatch(a, g.BatchID)
	if e != nil {
		return a, AutomationPolicy{}, e
	}
	a, policy, rules, e := s.checkAIBatch(a, b)
	if e != nil {
		return a, policy, e
	}
	if policy.Mode != "execute" || b.State != "submitted" || !slices.Contains(b.PlanIDs, p.ID) || g.PolicyVersion != policy.Version || g.RulesDigest != rulesDigest(rules) || messageSignature(m) != g.Signature || !slices.Contains(policy.Actions, p.Action) {
		return a, policy, ErrConflict
	}
	found := false
	for _, item := range b.Items {
		if item.Message.Ref == m.Ref && item.Signature == g.Signature && item.RuleID == g.RuleID && item.RuleVersion == g.RuleVersion {
			found = true
			break
		}
	}
	if !found || len(p.Items) != 1 {
		return a, policy, ErrConflict
	}
	evaluated := evaluateRules(m, rules, policy, time.Now().Unix())
	if evaluated.Status != "matched" || evaluated.RuleID != g.RuleID || evaluated.RuleVersion != g.RuleVersion {
		return a, policy, ErrConflict
	}
	if p.Action == "move" && !slices.Contains(policy.Destinations, p.Items[0].Destination) {
		return a, policy, ErrDenied
	}
	return a, policy, nil
}
func (s *Service) ExecuteAI(a Access, batchID, planID string) (Operation, error) {
	b, e := s.AIBatch(a, batchID)
	if e != nil {
		return Operation{}, e
	}
	if !slices.Contains(b.PlanIDs, planID) {
		return Operation{}, ErrDenied
	}
	return s.ExecutePlan(a, planID, "ai:"+planID)
}

// Uncertain keep decisions are reconsidered on the following Shanghai day.
func nextAutomationDay() int64 {
	now := time.Now().In(automationLocation)
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, automationLocation).Unix()
}
