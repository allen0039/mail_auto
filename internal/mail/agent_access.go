package mail

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/interaction"
	"mail-auto/internal/store"
)

// AgentOwnerPrefix marks the built-in management agent identity. Such an
// identity is valid only while its persisted grant is current and an active
// Telegram binding references it; constructing Access{Owner:"agent:..."}
// grants nothing by itself.
const AgentOwnerPrefix = "agent:"

// AgentActions are the actions an administrator may grant to the agent.
// task_run_manage is internal-only: it is never offered to MCP tokens.
var AgentActions = []string{"read", "rule_manage", "task_manage", "automation", "task_run_manage"}

type AgentGrant struct {
	ID           string              `json:"id"`
	Version      int                 `json:"version"`
	AdminVersion int                 `json:"-"`
	Enabled      bool                `json:"enabled"`
	Revoked      bool                `json:"revoked"`
	Expires      int64               `json:"expires"`
	Accounts     []string            `json:"accounts"`
	Folders      map[string][]string `json:"folders"`
	Actions      []string            `json:"actions"`
	CreatedAt    int64               `json:"created_at"`
	UpdatedAt    int64               `json:"updated_at"`
	Stale        bool                `json:"stale_admin_version"`
	AllFolders   []string            `json:"all_folders"`
}

// AgentGrantInput states folder scope explicitly per account: an account
// listed in AllFolders gets every folder (stored as an empty list, the
// existing Access meaning); any other account must name at least one folder.
// An empty selection is rejected instead of silently meaning "all". The admin
// HTTP API requires AllFolders to be present; only in-process callers that
// leave it nil keep the earlier contract (empty list = all folders).
type AgentGrantInput struct {
	Version    int                 `json:"version"`
	Enabled    bool                `json:"enabled"`
	Days       int                 `json:"days"`
	Accounts   []string            `json:"accounts"`
	AllFolders []string            `json:"all_folders"`
	Folders    map[string][]string `json:"folders"`
	Actions    []string            `json:"actions"`
}

func scanGrant(row interface{ Scan(...any) error }) (AgentGrant, error) {
	var g AgentGrant
	var accounts, folders, actions string
	var enabled, revoked int
	if e := row.Scan(&g.ID, &g.Version, &g.AdminVersion, &enabled, &revoked, &g.Expires, &accounts, &folders, &actions, &g.CreatedAt, &g.UpdatedAt); e != nil {
		return g, e
	}
	g.Enabled, g.Revoked = enabled != 0, revoked != 0
	if json.Unmarshal([]byte(accounts), &g.Accounts) != nil || json.Unmarshal([]byte(folders), &g.Folders) != nil || json.Unmarshal([]byte(actions), &g.Actions) != nil {
		return g, ErrConflict
	}
	return g, nil
}

const grantColumns = "id,version,admin_version,enabled,revoked,expires,accounts,folders,actions,created_at,updated_at"

// CurrentAgentGrant returns the single non-revoked grant, or sql.ErrNoRows.
func (s *Service) CurrentAgentGrant(a Access) (AgentGrant, error) {
	a, e := s.CheckAccess(a)
	if e != nil || a.Owner != "admin" {
		return AgentGrant{}, ErrDenied
	}
	g, e := scanGrant(s.Store.DB.QueryRow("SELECT " + grantColumns + " FROM agent_grants WHERE revoked=0"))
	if e != nil {
		return g, e
	}
	g.Stale = g.AdminVersion != a.Version
	g.AllFolders = []string{}
	for _, id := range g.Accounts {
		if len(g.Folders[id]) == 0 {
			g.AllFolders = append(g.AllFolders, id)
		}
	}
	return g, nil
}
func (s *Service) agentGrant(id string) (AgentGrant, error) {
	return scanGrant(s.Store.DB.QueryRow("SELECT "+grantColumns+" FROM agent_grants WHERE id=?", id))
}

// SaveAgentGrant creates or updates the agent grant at the expected version.
// Any change invalidates pending agent proposals. Saving binds the grant to
// the current administrator version.
func (s *Service) SaveAgentGrant(a Access, in AgentGrantInput) (AgentGrant, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	a, e := s.CheckAccess(a)
	if e != nil || a.Owner != "admin" {
		return AgentGrant{}, ErrDenied
	}
	if in.Days == 0 {
		in.Days = 30
	}
	if in.Days < 1 || in.Days > 90 || len(in.Accounts) < 1 || len(in.Accounts) > 10 || len(in.Actions) < 1 || len(in.Actions) > len(AgentActions) {
		return AgentGrant{}, ErrInput
	}
	if in.Folders == nil {
		in.Folders = map[string][]string{}
	}
	seen := map[string]bool{}
	for _, action := range in.Actions {
		if !slices.Contains(AgentActions, action) || seen[action] {
			return AgentGrant{}, ErrInput
		}
		seen[action] = true
	}
	if !seen["read"] {
		return AgentGrant{}, ErrInput
	}
	for id := range in.Folders {
		if !slices.Contains(in.Accounts, id) {
			return AgentGrant{}, ErrInput
		}
	}
	for _, id := range in.AllFolders {
		if !slices.Contains(in.Accounts, id) || len(in.Folders[id]) > 0 {
			return AgentGrant{}, ErrInput
		}
	}
	for _, id := range in.Accounts {
		account, e := s.Account(id)
		if e != nil {
			return AgentGrant{}, ErrInput
		}
		fs := in.Folders[id]
		if len(fs) > 100 || (in.AllFolders != nil && len(fs) == 0 && !slices.Contains(in.AllFolders, id)) {
			return AgentGrant{}, ErrInput
		}
		for _, f := range fs {
			if !s.knownFolder(account, f) {
				return AgentGrant{}, ErrInput
			}
		}
		if fs == nil {
			fs = []string{}
		}
		in.Folders[id] = fs
	}
	now := time.Now().Unix()
	current, e := scanGrant(s.Store.DB.QueryRow("SELECT " + grantColumns + " FROM agent_grants WHERE revoked=0"))
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return AgentGrant{}, e
	}
	if current.Version != in.Version {
		return AgentGrant{}, ErrConflict
	}
	g := AgentGrant{ID: current.ID, Version: current.Version + 1, AdminVersion: a.Version, Enabled: in.Enabled, Expires: now + int64(in.Days)*86400, Accounts: in.Accounts, Folders: in.Folders, Actions: in.Actions, CreatedAt: current.CreatedAt, UpdatedAt: now}
	rawA, _ := json.Marshal(g.Accounts)
	rawF, _ := json.Marshal(g.Folders)
	rawAct, _ := json.Marshal(g.Actions)
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return g, e
	}
	defer tx.Rollback()
	if current.ID == "" {
		g.ID = credentials.Random(18)
		g.CreatedAt = now
		_, e = tx.Exec("INSERT INTO agent_grants("+grantColumns+") VALUES(?,?,?,?,0,?,?,?,?,?,?)", g.ID, g.Version, g.AdminVersion, g.Enabled, g.Expires, string(rawA), string(rawF), string(rawAct), g.CreatedAt, g.UpdatedAt)
	} else {
		var res sql.Result
		res, e = tx.Exec("UPDATE agent_grants SET version=?,admin_version=?,enabled=?,expires=?,accounts=?,folders=?,actions=?,updated_at=? WHERE id=? AND version=? AND revoked=0", g.Version, g.AdminVersion, g.Enabled, g.Expires, string(rawA), string(rawF), string(rawAct), now, g.ID, current.Version)
		if e == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				return g, ErrConflict
			}
		}
	}
	if e != nil {
		return g, e
	}
	if e = invalidateAgentWork(tx, g.ID, g.Version, "grant_changed", false); e != nil {
		return g, e
	}
	if b, err := interaction.ActiveBinding(tx); err == nil && b.GrantID == g.ID && s.Vault != nil {
		// A notice without mailbox data, produced under the new version.
		conv, _ := interaction.ConversationOf(tx, b.ID)
		if e = interaction.Enqueue(tx, s.Vault, b, interaction.Reply{SemanticKey: "grant:" + g.ID + ":" + strconv.Itoa(g.Version), Text: "Agent 授权已由管理员更新：此前的对话上下文和未发送的回复已作废，待确认卡片已失效。请重新发送需要的指令。", GrantVersion: g.Version, ContextEpoch: conv.ContextEpoch}); e != nil {
			return g, e
		}
	}
	if e = store.Audit(tx, "agent.grant.save", g.ID, "ok"); e != nil {
		return g, e
	}
	return g, tx.Commit()
}

// RevokeAgentGrant revokes the current grant and, atomically, the binding
// that uses it, queued agent turns and every unfinished proposal. In-flight
// remote writes keep their normal verification path; nothing is replayed.
func (s *Service) RevokeAgentGrant(a Access) error {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	a, e := s.CheckAccess(a)
	if e != nil || a.Owner != "admin" {
		return ErrDenied
	}
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var id string
	if e = tx.QueryRow("SELECT id FROM agent_grants WHERE revoked=0").Scan(&id); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE agent_grants SET revoked=1,enabled=0,updated_at=? WHERE id=?", time.Now().Unix(), id); e != nil {
		return e
	}
	if e = invalidateAgentWork(tx, id, -1, "grant_revoked", true); e != nil {
		return e
	}
	if e = store.Audit(tx, "agent.grant.revoke", id, "ok"); e != nil {
		return e
	}
	return tx.Commit()
}

// invalidateAgentWork fences everything that depended on the previous grant
// version, inside the grant transaction (the linearization point): pending,
// accepted and applying proposals become invalid; queued and running turns
// are cancelled (a running turn re-checks before its next request or write);
// the conversation context epoch advances so results obtained under the old
// scope are never sent to the model again; and unsent replies produced under
// another grant version are suppressed. When revokeBinding is set the active
// binding is revoked too.
func invalidateAgentWork(tx *sql.Tx, grantID string, grantVersion int, reason string, revokeBinding bool) error {
	now := time.Now().Unix()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE agent_proposals SET state='invalidated',error_code=?,updated_at=? WHERE grant_id=? AND state IN ('pending','accepted','applying')", []any{reason, now, grantID}},
		{"UPDATE agent_turns SET state='cancelled',error_code=?,updated_at=? WHERE state IN ('queued','running') AND conversation_id IN (SELECT c.id FROM agent_conversations c JOIN telegram_bindings b ON b.id=c.binding_id WHERE b.grant_id=?)", []any{reason, now, grantID}},
		{"UPDATE agent_conversations SET context_epoch=context_epoch+1,grant_version=?,updated_at=? WHERE binding_id IN (SELECT id FROM telegram_bindings WHERE grant_id=?)", []any{grantVersion, now, grantID}},
		{"UPDATE telegram_reply_outbox SET state='suppressed',error_code=?,updated_at=? WHERE grant_id=? AND grant_version<>? AND state IN ('pending','retry','sending')", []any{reason, now, grantID, grantVersion}},
	} {
		if _, e := tx.Exec(q.sql, q.args...); e != nil {
			return e
		}
	}
	if revokeBinding {
		if _, e := tx.Exec("UPDATE telegram_bindings SET state='revoked',revoked_at=?,revoke_reason=? WHERE grant_id=? AND state='active'", now, reason, grantID); e != nil {
			return e
		}
		if _, e := tx.Exec("UPDATE telegram_reply_outbox SET state='suppressed',error_code=?,updated_at=? WHERE grant_id=? AND state IN ('pending','retry','sending')", reason, now, grantID); e != nil {
			return e
		}
	}
	return nil
}

// agentAccess validates an agent identity on every call: current grant,
// matching administrator version, expiry and an active binding using it.
func (s *Service) agentAccess(a Access) (Access, error) {
	id := strings.TrimPrefix(a.Owner, AgentOwnerPrefix)
	g, e := s.agentGrant(id)
	if e != nil || !g.Enabled || g.Revoked || g.Expires <= time.Now().Unix() || g.AdminVersion != a.Version {
		return Access{}, ErrDenied
	}
	var n int
	if e = s.Store.DB.QueryRow("SELECT count(*) FROM telegram_bindings WHERE grant_id=? AND state='active' AND admin_version=?", id, a.Version).Scan(&n); e != nil || n != 1 {
		return Access{}, ErrDenied
	}
	// Like MCP and task identities, the automation flag set by
	// authorizedAutomation survives re-validation; scope is always reloaded.
	return Access{Owner: a.Owner, Version: a.Version, Accounts: slices.Clone(g.Accounts), Folders: g.Folders, Actions: slices.Clone(g.Actions), automation: a.automation}, nil
}

// AgentIdentity returns the access value for the agent bound to grantID. The
// caller (agent runtime) resolves grantID from a verified Telegram binding;
// the model never supplies it.
func (s *Service) AgentIdentity(grantID string) (Access, AgentGrant, error) {
	g, e := s.agentGrant(grantID)
	if e != nil {
		return Access{}, g, ErrDenied
	}
	a, e := s.CheckAccess(Access{Owner: AgentOwnerPrefix + grantID, Version: g.AdminVersion})
	return a, g, e
}

func auditActor(a Access) string {
	if strings.HasPrefix(a.Owner, AgentOwnerPrefix) {
		return a.Owner
	}
	return "ok"
}

// Mutation is a server-assigned idempotency key for a business write. Kind is
// a fixed label; the digest binds the key to the caller's normalized input.
type Mutation struct {
	ID   string
	Kind string
	// Guard, when set, runs inside the business transaction before the
	// write commits. It is the linearization point between an agent write
	// and lifecycle changes (grant, stop, provider, binding): those commit
	// in their own transactions, so the guard sees either the state before
	// them (and the write precedes them) or after them (and it fails).
	Guard  func(*sql.Tx) error
	digest string
}

// ErrFenced reports that a lifecycle fence (stop, grant/binding/provider
// change, interaction off) stopped an agent write before it committed.
var ErrFenced = errors.New("execution_fenced")

func runGuard(tx *sql.Tx, guard func(*sql.Tx) error) error {
	if guard == nil {
		return nil
	}
	if e := guard(tx); e != nil {
		return ErrFenced
	}
	return nil
}

func mutationDigest(kind string, args any) string { return digest([]any{kind, args}) }

// mutationResult returns (true, nil) and fills out when the mutation already
// committed with the same input, ErrConflict when the key was used for other
// input, and (false, nil) when it has not run.
func (s *Service) mutationResult(m *Mutation, args any, out any) (bool, error) {
	if m.ID == "" || len(m.ID) > 200 || m.Kind == "" {
		return false, ErrInput
	}
	if m.digest == "" {
		m.digest = mutationDigest(m.Kind, args)
	}
	var gotKind, gotDigest, result string
	e := s.Store.DB.QueryRow("SELECT kind,args_digest,result FROM agent_mutations WHERE mutation_id=?", m.ID).Scan(&gotKind, &gotDigest, &result)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if gotKind != m.Kind || gotDigest != m.digest {
		return false, ErrConflict
	}
	if json.Unmarshal([]byte(result), out) != nil {
		return false, ErrConflict
	}
	return true, nil
}
func recordMutation(tx *sql.Tx, m Mutation, a Access, target string, version int, result any) error {
	if e := runGuard(tx, m.Guard); e != nil {
		return e
	}
	raw, _ := json.Marshal(result)
	_, e := tx.Exec("INSERT INTO agent_mutations(mutation_id,kind,actor,args_digest,target_id,target_version,result,created_at) VALUES(?,?,?,?,?,?,?,?)", m.ID, m.Kind, a.Owner, m.digest, target, version, string(raw), time.Now().Unix())
	return e
}

// MutationTarget reports a committed mutation's target for recovery.
func (s *Service) MutationTarget(id string) (string, int, bool, error) {
	var target string
	var version int
	e := s.Store.DB.QueryRow("SELECT target_id,target_version FROM agent_mutations WHERE mutation_id=?", id).Scan(&target, &version)
	if errors.Is(e, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	return target, version, e == nil, e
}

// ---- Scoped cross-owner task-run management for the agent ----

func (s *Service) agentRunScope(a Access, runID string) (Access, TaskRun, Task, error) {
	if !strings.HasPrefix(a.Owner, AgentOwnerPrefix) {
		return a, TaskRun{}, Task{}, ErrDenied
	}
	a, e := s.CheckAccess(a)
	if e != nil {
		return a, TaskRun{}, Task{}, e
	}
	r, e := s.loadTaskRun(runID)
	if e != nil {
		return a, r, Task{}, e
	}
	t, e := s.loadTask(r.TaskID)
	if e != nil {
		return a, r, t, e
	}
	if r.AccountID != t.AccountID || !taskVisible(a, t) {
		return a, TaskRun{}, Task{}, ErrDenied
	}
	if r.Owner != a.Owner && !a.Allows(t.AccountID, "", "task_run_manage") {
		return a, TaskRun{}, Task{}, ErrDenied
	}
	// A historical run is reported under the current visible task name; its
	// counts contain no message details.
	r.TaskName = t.Name
	return a, r, t, nil
}

// AgentTaskRun reads one run. With task_run_manage the agent may read runs of
// other owners (for example the task's own internal schedule) within its
// grant; without it the normal owner isolation applies.
func (s *Service) AgentTaskRun(a Access, runID string) (TaskRun, error) {
	_, r, _, e := s.agentRunScope(a, runID)
	return r, e
}

// AgentTaskRuns lists the latest runs of a visible task across owners when
// the grant includes task_run_manage; otherwise only the agent's own runs.
func (s *Service) AgentTaskRuns(a Access, taskID string) ([]TaskRun, error) {
	if !strings.HasPrefix(a.Owner, AgentOwnerPrefix) {
		return nil, ErrDenied
	}
	a, e := s.CheckAccess(a)
	if e != nil {
		return nil, e
	}
	t, e := s.Task(a, taskID)
	if e != nil {
		return nil, e
	}
	all := a.Allows(t.AccountID, "", "task_run_manage")
	rows, e := s.Store.DB.Query("SELECT id FROM task_runs WHERE task_id=? AND (owner=? OR ?) ORDER BY created_at DESC,id DESC LIMIT 30", taskID, a.Owner, all)
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
	out := []TaskRun{}
	for _, id := range ids {
		r, e := s.loadTaskRun(id)
		if e != nil {
			return nil, e
		}
		r.TaskName = t.Name
		out = append(out, r)
	}
	return out, nil
}

// AgentCancelTaskRun requests that a fixed-rule run stop its not-yet-started
// items. It never changes the run owner, takes over its lease, or executes
// its frozen plan. AI batches of other owners are not cancelled here.
func (s *Service) AgentCancelTaskRun(a Access, runID string) (TaskRun, error) {
	return s.AgentCancelTaskRunGuarded(a, runID, nil)
}

// AgentCancelTaskRunGuarded applies guard inside the cancellation transaction.
func (s *Service) AgentCancelTaskRunGuarded(a Access, runID string, guard func(*sql.Tx) error) (TaskRun, error) {
	a, r, t, e := s.agentRunScope(a, runID)
	if e != nil {
		return r, e
	}
	if r.Owner != a.Owner && !a.Allows(t.AccountID, "", "task_run_manage") {
		return r, ErrDenied
	}
	if t.Engine == "ai" || r.State == "awaiting_ai" {
		return r, ErrCapability
	}
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	r, e = s.loadTaskRun(runID)
	if e != nil {
		return r, e
	}
	r.TaskName = t.Name
	if !activeTaskState(r.State) {
		return r, nil
	}
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return r, e
	}
	defer tx.Rollback()
	if e = runGuard(tx, guard); e != nil {
		return r, e
	}
	r.State = "cancelled"
	r.ErrorCode = ""
	r.Finalized = false
	r.UpdatedAt = time.Now().Unix()
	res, e := tx.Exec("UPDATE task_runs SET state='cancelled',payload=?,updated_at=? WHERE id=? AND state IN ('queued','running')", runJSON(r), r.UpdatedAt, r.ID)
	if e != nil {
		return r, e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return r, ErrConflict
	}
	if r.BatchID != "" {
		if _, e = tx.Exec("UPDATE operations SET cancel_source=CASE WHEN cancelled=1 THEN cancel_source ELSE ? END,cancelled=1 WHERE plan_id IN (SELECT plan_id FROM automation_claims WHERE batch_id=?)", a.Owner, r.BatchID); e != nil {
			return r, e
		}
		if _, e = tx.Exec("UPDATE automation_claims SET state='done',expires=0 WHERE batch_id=? AND plan_id=''", r.BatchID); e != nil {
			return r, e
		}
	}
	if e = store.Audit(tx, "agent.task_run.cancel", r.ID, a.Owner); e != nil {
		return r, e
	}
	return r, tx.Commit()
}

// ---- Draft workflow preview and impact analysis ----

// RuleClosure returns the explicit binding for selected rules plus every
// enabled rule in an overlapping source folder with equal or higher priority
// (lower number), so a task can never skip a higher-priority protection.
// rules is the account rule snapshot to evaluate against.
func RuleClosure(rules []Rule, selected []string, folders []string) ([]string, map[string]int, error) {
	if len(selected) == 0 {
		return nil, nil, nil
	}
	byID := map[string]Rule{}
	for _, r := range rules {
		byID[r.ID] = r
	}
	maxPriority := -1
	for _, id := range selected {
		r, ok := byID[id]
		if !ok {
			return nil, nil, ErrDenied
		}
		if r.Priority > maxPriority {
			maxPriority = r.Priority
		}
	}
	chosen := map[string]bool{}
	for _, id := range selected {
		chosen[id] = true
	}
	for _, r := range rules {
		if r.Enabled && r.Priority <= maxPriority && taskRuleOverlap(folders, r.SourceFolders) {
			chosen[r.ID] = true
		}
	}
	ids := []string{}
	versions := map[string]int{}
	for id := range chosen {
		ids = append(ids, id)
		versions[id] = byID[id].Version
	}
	sort.Strings(ids)
	return ids, versions, nil
}

// DraftWorkflow describes a disabled draft rule and a task that does not yet
// exist, evaluated as if the draft rule were enabled. Nothing is written.
type DraftWorkflow struct {
	RuleID      string `json:"rule_id"`
	RuleVersion int    `json:"rule_version"`
	Task        Task   `json:"task"`
	Cursor      string `json:"cursor,omitempty"`
	OnlyMatched bool   `json:"only_matched,omitempty"`
}

// PreviewDraftWorkflow previews a not-yet-created task bound to a disabled
// draft rule. It builds an in-memory snapshot with the draft enabled and
// reuses the task preview engine; no rule enabled state is changed.
func (s *Service) PreviewDraftWorkflow(a Access, in DraftWorkflow) (RulePreview, Task, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return RulePreview{}, Task{}, e
	}
	draft, e := s.Rule(a, in.RuleID)
	if e != nil {
		return RulePreview{}, Task{}, e
	}
	t := in.Task
	if draft.AccountID != t.AccountID || draft.Version != in.RuleVersion {
		return RulePreview{}, Task{}, ErrConflict
	}
	if a.Owner != "admin" && (!a.Allows(t.AccountID, "", "automation") || !a.Allows(t.AccountID, "", "task_manage")) {
		return RulePreview{}, Task{}, ErrDenied
	}
	p, e := s.loadAutomationPolicy(t.AccountID)
	if e != nil {
		return RulePreview{}, Task{}, e
	}
	if len(t.SourceFolders) == 0 || len(t.SourceFolders) > 20 || !taskVisible(a, t) || !taskRuleOverlap(t.SourceFolders, draft.SourceFolders) {
		return RulePreview{}, Task{}, ErrInput
	}
	for _, f := range t.SourceFolders {
		if !slices.Contains(p.SourceFolders, f) {
			return RulePreview{}, Task{}, ErrDenied
		}
	}
	all, e := s.rulesForAccount(t.AccountID)
	if e != nil {
		return RulePreview{}, Task{}, e
	}
	snapshot := make([]Rule, 0, len(all))
	for _, r := range all {
		if r.ID == draft.ID {
			r.Enabled = true
		}
		snapshot = append(snapshot, r)
	}
	ids, versions, e := RuleClosure(snapshot, []string{draft.ID}, t.SourceFolders)
	if e != nil {
		return RulePreview{}, Task{}, e
	}
	t.RuleIDs, t.RuleVersions = ids, versions
	rules, _, _, e := resolveTaskRulesFrom(a, t, false, snapshot)
	if e != nil {
		return RulePreview{}, Task{}, e
	}
	if t.MaxBatch == 0 {
		t.MaxBatch = min(20, p.MaxBatch)
	}
	if t.MaxBatch < 1 || t.MaxBatch > p.MaxBatch {
		return RulePreview{}, Task{}, ErrInput
	}
	out, e := s.previewTaskRules(a, t, rules, p, in.Cursor, in.OnlyMatched)
	return out, t, e
}

// TaskImpact is an existing task whose evaluation changes when a rule is
// enabled or modified.
type TaskImpact struct {
	TaskID  string `json:"task_id"`
	Name    string `json:"name"`
	Version int    `json:"version"`
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
}

// RuleImpact lists tasks affected if rule r (its new form) takes effect.
// Tasks that follow all enabled rules start evaluating it; explicitly bound
// tasks in an overlapping folder whose highest bound priority is not above
// the rule lose a valid binding. hidden counts affected tasks outside a.
func (s *Service) RuleImpact(a Access, r Rule) ([]TaskImpact, int, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return nil, 0, e
	}
	rows, e := s.Store.DB.Query("SELECT id FROM automation_tasks WHERE account_id=? ORDER BY id", r.AccountID)
	if e != nil {
		return nil, 0, e
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
		return nil, 0, e
	}
	all, e := s.rulesForAccount(r.AccountID)
	if e != nil {
		return nil, 0, e
	}
	byID := map[string]Rule{}
	for _, x := range all {
		byID[x.ID] = x
	}
	out := []TaskImpact{}
	hidden := 0
	for _, id := range ids {
		t, e := s.loadTask(id)
		if e != nil {
			return nil, 0, e
		}
		if !taskRuleOverlap(t.SourceFolders, r.SourceFolders) {
			continue
		}
		reason := ""
		if len(t.RuleIDs) == 0 {
			reason = "跟随全部启用规则：将开始评估此规则"
		} else if slices.Contains(t.RuleIDs, r.ID) {
			reason = "已显式绑定此规则：规则版本改变后需重新确认任务绑定"
		} else {
			maxPriority := -1
			for _, rid := range t.RuleIDs {
				if x, ok := byID[rid]; ok && x.Priority > maxPriority {
					maxPriority = x.Priority
				}
			}
			if r.Enabled && r.Priority <= maxPriority {
				reason = "显式绑定的规则闭包将缺少此高优先级规则，任务运行会因绑定冲突而停止"
			}
		}
		if reason == "" {
			continue
		}
		if !taskVisible(a, t) {
			hidden++
			continue
		}
		out = append(out, TaskImpact{TaskID: t.ID, Name: t.Name, Version: t.Version, Enabled: t.Enabled, Reason: reason})
	}
	return out, hidden, nil
}

// AccountStateDigest summarizes the automation configuration of an account:
// rule and task versions, policy version and folder mapping version. Agent
// proposals freeze it; any change invalidates a pending confirmation.
func (s *Service) AccountStateDigest(accountID string) (string, error) {
	rules, e := s.rulesForAccount(accountID)
	if e != nil {
		return "", e
	}
	ruleVersions := []any{}
	for _, r := range rules {
		ruleVersions = append(ruleVersions, []any{r.ID, r.Version})
	}
	rows, e := s.Store.DB.Query("SELECT id,version FROM automation_tasks WHERE account_id=? ORDER BY id", accountID)
	if e != nil {
		return "", e
	}
	taskVersions := []any{}
	for rows.Next() {
		var id string
		var v int
		if e = rows.Scan(&id, &v); e != nil {
			break
		}
		taskVersions = append(taskVersions, []any{id, v})
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return "", e
	}
	p, e := s.loadAutomationPolicy(accountID)
	if e != nil {
		return "", e
	}
	fs, e := s.FolderSettings(accountID)
	if e != nil {
		return "", e
	}
	return digest([]any{accountID, ruleVersions, taskVersions, p.Version, p.AdminVersion, fs.Version, fs.WritesEnabled, fs.Archive, fs.Trash}), nil
}

// ScheduleTimes returns the next five occurrences of an internal schedule.
func ScheduleTimes(c TaskSchedule) ([]int64, error) { return scheduleTimes(c) }

// RuleFieldLabel returns the Chinese label of a rule condition field.
func RuleFieldLabel(field string) string {
	if v, ok := ruleFieldLabels[field]; ok {
		return v
	}
	return field
}

// EffectivePolicy returns the account policy without the caller-visible
// narrowing, for computing effective task protections server-side.
func (s *Service) EffectivePolicy(a Access, accountID string) (AutomationPolicy, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return AutomationPolicy{}, e
	}
	if !a.Allows(accountID, "", "read") {
		return AutomationPolicy{}, ErrDenied
	}
	return s.loadAutomationPolicy(accountID)
}
