package mail

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

type PlanInput struct {
	Search      *SearchInput `json:"search,omitempty"`
	Refs        []string     `json:"refs"`
	Action      string       `json:"action"`
	Labels      []string     `json:"labels,omitempty"`
	Destination string       `json:"destination,omitempty"`
	// Also adds flag effects to a move/archive/trash or flag action.
	Also []string `json:"also,omitempty"`
}

const (
	// Fresh uncertain writes are verified after a short quiet period for a day.
	operationReconcileDelay  = 30 * time.Second
	operationReconcileWindow = 24 * time.Hour
	// Older ones are retried at most every six hours for 30 days; after that
	// they stay visible and can only be verified manually.
	operationSlowReconcileDelay  = 6 * time.Hour
	operationSlowReconcileWindow = 30 * 24 * time.Hour
)

type PlanItem struct {
	Message         Message `json:"message"`
	Destination     string  `json:"destination,omitempty"`
	Config          string  `json:"config"`
	SettingsVersion int     `json:"settings_version"`
	Skip            string  `json:"skip,omitempty"`
	// Flags is applied to the source (flag actions) or to the confirmed
	// destination after a move.
	Flags FlagChange `json:"flags,omitzero"`
}
type Plan struct {
	ID         string           `json:"id"`
	Owner      string           `json:"owner"`
	Version    int              `json:"version"`
	Action     string           `json:"action"`
	Labels     []string         `json:"labels,omitempty"`
	Also       []string         `json:"also,omitempty"`
	Automation *AutomationGuard `json:"automation,omitempty"`
	Items      []PlanItem       `json:"items"`
	CreatedAt  int64            `json:"created_at"`
	Expires    int64            `json:"expires"`
}

// Message deliberately hides IMAP identifiers in ordinary API responses. A frozen
// plan must persist them separately so reloading it cannot lose its preconditions.
func (item PlanItem) MarshalJSON() ([]byte, error) {
	type plain PlanItem
	return json.Marshal(struct {
		plain
		UID      uint32 `json:"source_uid"`
		Validity uint32 `json:"source_uidvalidity"`
	}{plain(item), item.Message.UID, item.Message.UIDValidity})
}
func (item *PlanItem) UnmarshalJSON(raw []byte) error {
	type plain PlanItem
	var wire struct {
		plain
		UID      uint32 `json:"source_uid"`
		Validity uint32 `json:"source_uidvalidity"`
	}
	if e := json.Unmarshal(raw, &wire); e != nil {
		return e
	}
	*item = PlanItem(wire.plain)
	item.Message.UID = wire.UID
	item.Message.UIDValidity = wire.Validity
	return nil
}

type OperationItem struct {
	Position     int        `json:"position"`
	Ref          string     `json:"ref"`
	State        string     `json:"state"`
	Error        string     `json:"error_code,omitempty"`
	BeforeLabels []string   `json:"before_labels"`
	Result       MoveResult `json:"destination"`
	// Sanitized diagnostics: the first write failure, and the latest automatic
	// verification outcome with its time.
	Detail         string `json:"detail,omitempty"`
	RecoveryCode   string `json:"recovery_code,omitempty"`
	RecoveryDetail string `json:"recovery_detail,omitempty"`
	CheckedAt      int64  `json:"checked_at,omitempty"`
}
type Operation struct {
	ID        string          `json:"id"`
	PlanID    string          `json:"plan_id"`
	State     string          `json:"state"`
	Cancelled bool            `json:"cancelled"`
	CreatedAt int64           `json:"created_at"`
	UpdatedAt int64           `json:"updated_at"`
	Items     []OperationItem `json:"items"`
	// For needs_verification only: "auto" (frequent, first 24 hours), "slow"
	// (every six hours up to 30 days) or "manual"; NextCheckAt is an estimate.
	ReconcileMode string `json:"reconcile_mode,omitempty"`
	NextCheckAt   int64  `json:"next_check_at,omitempty"`
}

func reconcileSchedule(o Operation, now time.Time) (string, int64) {
	if o.State != "needs_verification" {
		return "", 0
	}
	age := now.Sub(time.Unix(o.CreatedAt, 0))
	switch {
	case age < operationReconcileWindow:
		return "auto", max(o.UpdatedAt+int64(operationReconcileDelay/time.Second), now.Unix())
	case age < operationSlowReconcileWindow:
		return "slow", max(o.UpdatedAt+int64(operationSlowReconcileDelay/time.Second), now.Unix())
	}
	return "manual", 0
}

func accountIdentity(a Account) string {
	raw, _ := json.Marshal([]any{a.Provider, a.Username, a.Host, a.Port, a.Folders})
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func (s *Service) NewPlan(ctx context.Context, access Access, in PlanInput) (Plan, error) {
	access, e := s.CheckAccess(access)
	if e != nil {
		return Plan{}, e
	}
	if access.Owner != "admin" && slices.Contains(access.Actions, "automation") && !access.automation {
		return Plan{}, ErrDenied
	}
	if in.Search != nil {
		if len(in.Refs) > 0 {
			return Plan{}, ErrInput
		}
		q := *in.Search
		q.Limit = 100
		q.Cursor = ""
		if q.Folder == "" {
			q.Folder = "INBOX"
		}
		for {
			out, err := s.SearchScoped(access, q)
			if err != nil {
				return Plan{}, err
			}
			if len(in.Refs)+len(out.Messages) > 500 || out.Total > 500 {
				return Plan{}, ErrInput
			}
			for _, m := range out.Messages {
				in.Refs = append(in.Refs, m.Ref)
			}
			if out.NextCursor == "" {
				break
			}
			q.Cursor = out.NextCursor
		}
	}
	if len(in.Refs) < 1 || len(in.Refs) > 500 || len(in.Labels) > 50 || !(slices.Contains([]string{"archive", "move", "trash", "restore"}, in.Action) || isFlagAction(in.Action)) {
		return Plan{}, ErrInput
	}
	if !validAlso(in.Action, in.Also) {
		return Plan{}, ErrInput
	}
	flagSet := slices.Clone(in.Also)
	if isFlagAction(in.Action) {
		flagSet = append(flagSet, in.Action)
	}
	flags, _ := flagChangeFor(flagSet...)
	if len(in.Labels) > 0 {
		return Plan{}, ErrInput
	}
	if len(in.Destination) > 256 || strings.ContainsAny(in.Destination, "\x00\r\n") {
		return Plan{}, ErrInput
	}
	for _, id := range in.Labels {
		var n int
		if e = s.Store.DB.QueryRow("SELECT count(*) FROM labels WHERE name=?", id).Scan(&n); e != nil {
			return Plan{}, e
		}
		if n != 1 {
			return Plan{}, ErrInput
		}
	}
	now := time.Now().Unix()
	p := Plan{ID: credentials.Random(24), Owner: access.Owner, Version: access.Version, Action: in.Action, Labels: in.Labels, Also: in.Also, CreatedAt: now, Expires: now + 600, Items: []PlanItem{}}
	seen := map[string]bool{}
	for _, ref := range in.Refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		m, e := s.AllowedMessage(access, ref, in.Action)
		if e != nil {
			return Plan{}, e
		}
		a, e := s.Account(m.AccountID)
		if e != nil {
			return Plan{}, e
		}
		item := PlanItem{Message: m, Config: accountIdentity(a), Flags: flags}
		if len(in.Also) > 0 && !access.Allows(a.ID, m.Folder, "flag") {
			return Plan{}, ErrDenied
		}
		if isFlagAction(in.Action) {
			settings, e := s.FolderSettings(a.ID)
			if e != nil {
				return Plan{}, e
			}
			if !settings.WritesEnabled {
				return Plan{}, ErrCapability
			}
			item.SettingsVersion = settings.Version
			if flags.Satisfied(m) {
				item.Skip = "already_applied"
			}
		} else if !strings.HasPrefix(in.Action, "label_") {
			settings, e := s.FolderSettings(a.ID)
			if e != nil {
				return Plan{}, e
			}
			if !settings.WritesEnabled {
				return Plan{}, ErrCapability
			}
			item.SettingsVersion = settings.Version
			switch in.Action {
			case "archive":
				item.Destination = settings.Archive
			case "trash":
				item.Destination = settings.Trash
			case "move":
				item.Destination = in.Destination
			case "restore":
				if m.Folder != settings.Trash {
					return Plan{}, ErrConflict
				}
				item.Destination = in.Destination
				if item.Destination == "" {
					var payload string
					e = s.Store.DB.QueryRow(`SELECT p.payload FROM operations o JOIN operation_plans p ON p.id=o.plan_id JOIN operation_items i ON i.operation_id=o.id WHERE i.ref=? AND i.state='completed' AND i.destination=? AND i.destination_validity=? AND i.destination_uid=? ORDER BY o.created_at DESC,o.id DESC LIMIT 1`, ref, m.Folder, m.UIDValidity, m.UID).Scan(&payload)
					if e != nil {
						return Plan{}, ErrInput
					}
					var old Plan
					if json.Unmarshal([]byte(payload), &old) != nil {
						return Plan{}, ErrInput
					}
					for _, x := range old.Items {
						if x.Message.Ref == ref {
							item.Destination = x.Message.Folder
							break
						}
					}
				}
			}
			if item.Destination == "" || !access.Allows(a.ID, item.Destination, in.Action) || (len(a.Folders) > 0 && !slices.Contains(a.Folders, item.Destination)) {
				return Plan{}, ErrDenied
			}
			boxes, e := s.Mailboxes(a.ID)
			if e != nil {
				return Plan{}, e
			}
			found := false
			for _, b := range boxes {
				if b.Name == item.Destination {
					found = true
				}
			}
			if !found {
				return Plan{}, ErrInput
			}
			if m.Folder == item.Destination {
				item.Skip = "already_in_destination"
			}
		}
		p.Items = append(p.Items, item)
	}
	raw, e := json.Marshal(p)
	if e != nil {
		return Plan{}, e
	}
	_, e = s.Store.DB.Exec("INSERT INTO operation_plans(id,owner,payload,created_at,expires,settings_version) VALUES(?,?,?,?,?,?)", p.ID, p.Owner, string(raw), now, p.Expires, p.Version)
	return p, e
}
func (s *Service) GetPlan(a Access, id string) (Plan, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return Plan{}, e
	}
	var raw string
	e = s.Store.DB.QueryRow("SELECT payload FROM operation_plans WHERE id=? AND (owner=? OR ?='admin')", id, a.Owner, a.Owner).Scan(&raw)
	if e != nil {
		return Plan{}, e
	}
	var p Plan
	e = json.Unmarshal([]byte(raw), &p)
	return p, e
}
func (s *Service) ExecutePlan(a Access, id, key string) (Operation, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	a, e := s.CheckAccess(a)
	if e != nil {
		return Operation{}, e
	}
	if key == "" || len(key) > 128 {
		return Operation{}, ErrInput
	}
	var existing, oldPlan string
	e = s.Store.DB.QueryRow("SELECT id,plan_id FROM operations WHERE owner=? AND idempotency_key=?", a.Owner, key).Scan(&existing, &oldPlan)
	if e == nil {
		if oldPlan != id {
			return Operation{}, ErrConflict
		}
		return s.Operation(a, existing)
	}
	if e != sql.ErrNoRows {
		return Operation{}, e
	}
	p, e := s.GetPlan(a, id)
	if e != nil {
		return Operation{}, e
	}
	if p.Owner != a.Owner || p.Version != a.Version || p.Expires <= time.Now().Unix() {
		return Operation{}, ErrConflict
	}
	var automationPolicy AutomationPolicy
	if p.Automation != nil {
		if len(p.Items) != 1 {
			return Operation{}, ErrConflict
		}
		m, err := s.AllowedMessage(a, p.Items[0].Message.Ref, "read")
		if err != nil {
			return Operation{}, err
		}
		a, automationPolicy, e = s.validateAutomationPlan(a, p, m)
		if e != nil {
			return Operation{}, e
		}
	}
	now := time.Now().Unix()
	opID := credentials.Random(24)
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return Operation{}, e
	}
	defer tx.Rollback()
	var planUses int
	if e = tx.QueryRow("SELECT count(*) FROM operations WHERE plan_id=?", p.ID).Scan(&planUses); e != nil {
		return Operation{}, e
	}
	if planUses > 0 {
		return Operation{}, ErrConflict
	}
	if p.Automation != nil {
		day := time.Now().In(automationLocation).Format("2006-01-02")
		if _, e = tx.Exec("INSERT OR IGNORE INTO automation_daily(account_id,day,count,claimed) VALUES(?,?,0,0)", automationPolicy.AccountID, day); e != nil {
			return Operation{}, e
		}
		if _, e = tx.Exec("UPDATE automation_daily SET count=count+? WHERE account_id=? AND day=?", len(p.Items), automationPolicy.AccountID, day); e != nil {
			return Operation{}, e
		}
	}
	for _, item := range p.Items {
		if item.Skip != "" {
			continue
		}
		var n int
		if e = tx.QueryRow("SELECT count(*) FROM operation_items WHERE ref=? AND state IN ('queued','running','needs_verification')", item.Message.Ref).Scan(&n); e != nil {
			return Operation{}, e
		}
		if n > 0 {
			return Operation{}, ErrBusy
		}
		if p.Automation == nil {
			if e = tx.QueryRow("SELECT count(*) FROM automation_claims WHERE account_id=? AND ref=? AND state IN ('claimed','planned') AND expires>? AND (plan_id='' OR plan_id NOT IN (SELECT plan_id FROM operations WHERE state IN ('completed','failed','partial','cancelled')))", item.Message.AccountID, item.Message.Ref, now).Scan(&n); e != nil {
				return Operation{}, e
			}
			if n > 0 {
				return Operation{}, ErrBusy
			}
		}
	}
	_, e = tx.Exec("INSERT INTO operations(id,plan_id,owner,idempotency_key,state,created_at,updated_at) VALUES(?,?,?,?,'queued',?,?)", opID, id, a.Owner, key, now, now)
	if e != nil {
		tx.Rollback()
		var winner, winnerPlan string
		if err := s.Store.DB.QueryRow("SELECT id,plan_id FROM operations WHERE owner=? AND idempotency_key=?", a.Owner, key).Scan(&winner, &winnerPlan); err == nil && winnerPlan == id {
			return s.Operation(a, winner)
		}
		return Operation{}, ErrConflict
	}
	for i, item := range p.Items {
		before := []string{}
		for _, l := range item.Message.Labels {
			before = append(before, l.Name)
		}
		raw, _ := json.Marshal(before)
		state := "queued"
		if item.Skip != "" {
			state = "skipped"
		}
		if _, e = tx.Exec("INSERT INTO operation_items(operation_id,position,ref,state,error_code,before_labels,destination) VALUES(?,?,?,?,?,?,?)", opID, i, item.Message.Ref, state, item.Skip, string(raw), item.Destination); e != nil {
			return Operation{}, e
		}
	}
	if e = store.Audit(tx, "mail.execute", opID, "queued"); e != nil {
		return Operation{}, e
	}
	if e = tx.Commit(); e != nil {
		return Operation{}, e
	}
	return s.Operation(a, opID)
}
func (s *Service) Operation(a Access, id string) (Operation, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return Operation{}, e
	}
	var o Operation
	e = s.Store.DB.QueryRow("SELECT id,plan_id,state,cancelled,created_at,updated_at FROM operations WHERE id=? AND (owner=? OR ?='admin')", id, a.Owner, a.Owner).Scan(&o.ID, &o.PlanID, &o.State, &o.Cancelled, &o.CreatedAt, &o.UpdatedAt)
	if e != nil {
		return o, e
	}
	rows, e := s.Store.DB.Query("SELECT position,ref,state,error_code,before_labels,destination,destination_validity,destination_uid,detail,recovery_code,recovery_detail,checked_at FROM operation_items WHERE operation_id=? ORDER BY position", id)
	if e != nil {
		return o, e
	}
	defer rows.Close()
	o.Items = []OperationItem{}
	for rows.Next() {
		var x OperationItem
		var raw string
		if e = rows.Scan(&x.Position, &x.Ref, &x.State, &x.Error, &raw, &x.Result.Folder, &x.Result.UIDValidity, &x.Result.UID, &x.Detail, &x.RecoveryCode, &x.RecoveryDetail, &x.CheckedAt); e != nil {
			return o, e
		}
		if e = json.Unmarshal([]byte(raw), &x.BeforeLabels); e != nil {
			return o, e
		}
		o.Items = append(o.Items, x)
	}
	o.ReconcileMode, o.NextCheckAt = reconcileSchedule(o, time.Now())
	return o, rows.Err()
}
func (s *Service) CancelOperation(a Access, id string) error {
	if _, e := s.Operation(a, id); e != nil {
		return e
	}
	_, e := s.Store.DB.Exec("UPDATE operations SET cancelled=1,cancel_source='user',updated_at=? WHERE id=?", time.Now().Unix(), id)
	return e
}
func (s *Service) Operations(a Access) ([]Operation, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return nil, e
	}
	rows, e := s.Store.DB.Query(`SELECT id FROM operations WHERE owner=? OR ?='admin' ORDER BY CASE WHEN state IN ('queued','running','needs_verification') THEN 0 ELSE 1 END,created_at DESC,id DESC LIMIT 100`, a.Owner, a.Owner)
	if e != nil {
		return nil, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	out := []Operation{}
	for _, id := range ids {
		o, e := s.Operation(a, id)
		if e != nil {
			return nil, e
		}
		out = append(out, o)
	}
	return out, nil
}
func errorCode(e error) string {
	switch {
	case errors.Is(e, ErrDailyLimit):
		return "daily_limit_reached"
	case errors.Is(e, ErrDenied):
		return "permission_denied"
	case errors.Is(e, ErrBusy):
		return "busy"
	case errors.Is(e, ErrCapability):
		return "capability_unavailable"
	case errors.Is(e, ErrUncertain):
		return "remote_state_unknown"
	case errors.Is(e, sql.ErrNoRows):
		return "stale_reference"
	case errors.Is(e, ErrConflict):
		return "state_conflict"
	case errors.Is(e, ErrInput):
		return "invalid_input"
	case errors.Is(e, errFlagsNotApplied):
		return "flags_not_applied"
	default:
		var provider interface{ OperationErrorCode() string }
		if errors.As(e, &provider) {
			return provider.OperationErrorCode()
		}
		return "operation_failed"
	}
}
func (s *Service) runOperation(ctx context.Context, id string) {
	var raw string
	var owner string
	if s.Store.DB.QueryRow("SELECT p.payload,o.owner FROM operations o JOIN operation_plans p ON p.id=o.plan_id WHERE o.id=?", id).Scan(&raw, &owner) != nil {
		return
	}
	var p Plan
	if json.Unmarshal([]byte(raw), &p) != nil {
		return
	}
	if len(p.Items) == 0 {
		return
	}
	accountID := p.Items[0].Message.AccountID
	release, e := s.acquire(accountID)
	if e != nil {
		return
	}
	defer func() { release() }()
	claim, e := s.Store.DB.Exec("UPDATE operations SET state='running',updated_at=? WHERE id=? AND state='queued'", time.Now().Unix(), id)
	if e == nil {
		n, err := claim.RowsAffected()
		if err != nil || n != 1 {
			return
		}
	}
	if e != nil {
		return
	}
	for i, item := range p.Items {
		if ctx.Err() != nil {
			return
		}
		var state string
		var cancelled bool
		if s.Store.DB.QueryRow("SELECT i.state,o.cancelled FROM operation_items i JOIN operations o ON o.id=i.operation_id WHERE i.operation_id=? AND i.position=?", id, i).Scan(&state, &cancelled) != nil {
			return
		}
		if state != "queued" {
			continue
		}
		if cancelled {
			_, _ = s.Store.DB.Exec("UPDATE operation_items SET state='cancelled' WHERE operation_id=? AND position=?", id, i)
			continue
		}
		if item.Message.AccountID != accountID {
			release()
			release = func() {}
			accountID = item.Message.AccountID
			releaseNext, err := s.acquire(accountID)
			if err != nil {
				_, _ = s.Store.DB.Exec("UPDATE operations SET state='queued',updated_at=? WHERE id=? AND state='running'", time.Now().Unix(), id)
				return
			}
			release = releaseNext
		}
		access, e := s.CheckAccess(Access{Owner: owner, Version: p.Version})
		if e == nil {
			e = s.executeItem(ctx, id, i, p, item, access)
		}
		state = "completed"
		code := ""
		detail := ""
		if errors.Is(e, errFlagsNotApplied) {
			// The move itself is confirmed; only the follow-up flag change failed
			// or could not be confirmed. Replace the provisional pending warning
			// stored with the move by the final explanation.
			code = errorCode(e)
			detail = flagsWarning(item, flagsFailed)
			if _, err := s.Store.DB.Exec("UPDATE operation_items SET state='completed',error_code=?,detail=?,recovery_code=?,recovery_detail=?,checked_at=? WHERE operation_id=? AND position=?", code, detail, code, detail, time.Now().Unix(), id, i); err != nil {
				return
			}
			continue
		}
		if e != nil {
			state = "failed"
			code = errorCode(e)
			slog.Warn("邮件整理未完成", "operation_id", id, "position", i, "error_code", code, "reason", syncErrorMessage(e))
			if errors.Is(e, ErrUncertain) {
				state = "needs_verification"
			}
			if errors.Is(e, ErrBusy) {
				state = "skipped"
			}
		}
		if e != nil {
			detail = syncErrorMessage(e)
		}
		if _, err := s.Store.DB.Exec("UPDATE operation_items SET state=?,error_code=?,detail=? WHERE operation_id=? AND position=?", state, code, detail, id, i); err != nil {
			return
		}
	}
	var success, failed, unknown, cancelled int
	if e = s.Store.DB.QueryRow("SELECT COALESCE(sum(state IN ('completed','skipped')),0),COALESCE(sum(state='failed'),0),COALESCE(sum(state IN ('needs_verification','running','queued')),0),COALESCE(sum(state='cancelled'),0) FROM operation_items WHERE operation_id=?", id).Scan(&success, &failed, &unknown, &cancelled); e != nil {
		return
	}
	final := "completed"
	if failed > 0 {
		final = "failed"
		if success > 0 {
			final = "partial"
		}
	}
	if cancelled > 0 {
		final = "cancelled"
		if success > 0 {
			final = "partial"
		}
	}
	if unknown > 0 {
		final = "needs_verification"
	}
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return
	}
	defer tx.Rollback()
	if _, e = tx.Exec("UPDATE operations SET state=?,updated_at=? WHERE id=?", final, time.Now().Unix(), id); e != nil {
		return
	}
	if e = store.Audit(tx, "mail.operation", id, final); e != nil {
		return
	}
	_ = tx.Commit()
}
func (s *Service) executeItem(ctx context.Context, id string, pos int, p Plan, item PlanItem, access Access) error {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	if p.Automation != nil {
		access.automation = true
	}
	m, e := s.AllowedMessage(access, item.Message.Ref, p.Action)
	if e != nil {
		return e
	}
	if m.Folder != item.Message.Folder || m.UID != item.Message.UID || m.UIDValidity != item.Message.UIDValidity {
		return ErrConflict
	}
	if p.Automation != nil {
		access, _, e = s.validateAutomationPlan(access, p, m)
		if e != nil {
			return e
		}
	}
	account, e := s.Account(m.AccountID)
	if e != nil {
		return e
	}
	if accountIdentity(account) != item.Config {
		return ErrConflict
	}
	if !strings.HasPrefix(p.Action, "label_") {
		settings, e := s.FolderSettings(account.ID)
		if e != nil {
			return e
		}
		if settings.Version != item.SettingsVersion || !settings.WritesEnabled {
			return ErrDenied
		}
		target := item.Destination
		if isFlagAction(p.Action) {
			target = m.Folder
		}
		if !access.Allows(account.ID, target, p.Action) || (len(p.Also) > 0 && !access.Allows(account.ID, m.Folder, "flag")) {
			return ErrDenied
		}
	}
	if _, e = s.Store.DB.Exec("UPDATE operation_items SET state='running' WHERE operation_id=? AND position=?", id, pos); e != nil {
		return e
	}
	if strings.HasPrefix(p.Action, "label_") && !sameLabelState(m.Labels, item.Message.Labels) {
		return ErrConflict
	}
	if p.Action == "label_add" {
		return s.setMessageLabels(m.Ref, p.Labels, nil, id, pos)
	}
	if p.Action == "label_remove" {
		return s.setMessageLabels(m.Ref, nil, p.Labels, id, pos)
	}
	if isFlagAction(p.Action) {
		return s.executeFlags(ctx, id, pos, p, item, access, account, m)
	}
	remote, ok := s.Remote.(WritableRemote)
	if !ok {
		return ErrCapability
	}
	pw, e := s.password(account)
	if e != nil {
		return e
	}
	record := func(r MoveResult) error {
		_, e := s.Store.DB.Exec("UPDATE operation_items SET destination_validity=?,destination_uid=? WHERE operation_id=? AND position=?", r.UIDValidity, r.UID, id, pos)
		return e
	}
	if p.Automation != nil {
		var queuedAt int64
		if err := s.Store.DB.QueryRow("SELECT created_at FROM operations WHERE id=?", id).Scan(&queuedAt); err != nil {
			return err
		}
		if time.Unix(queuedAt, 0).In(automationLocation).Format("2006-01-02") != time.Now().In(automationLocation).Format("2006-01-02") {
			return ErrConflict
		}
		validate := func(fresh Message) error {
			fresh.Ref = m.Ref
			fresh.ChangedAt = m.ChangedAt
			if messageSignature(fresh) != p.Automation.Signature {
				return ErrConflict
			}
			_, _, err := s.validateAutomationPlan(access, p, fresh)
			return err
		}
		if prepared, ok := s.Remote.(AutomationMoveRemote); ok {
			result, err := prepared.MoveWithRuleState(ctx, account, pw, m, item.Destination, validate, record)
			if err != nil {
				return err
			}
			if err = s.publishNormalMove(id, pos, m, item, result); err != nil {
				return err
			}
			return s.flagsAfterMove(ctx, id, pos, account, pw, m, item, result)
		}
		freshRemote, ok := s.Remote.(RuleStateRemote)
		if !ok {
			return ErrCapability
		}
		fresh, err := freshRemote.RuleState(ctx, account, pw, m)
		if err != nil {
			return err
		}
		if err = validate(fresh); err != nil {
			return err
		}
	}
	result, e := remote.Move(ctx, account, pw, m, item.Destination, record)
	if e != nil {
		return e
	}
	if e = s.publishNormalMove(id, pos, m, item, result); e != nil {
		return e
	}
	return s.flagsAfterMove(ctx, id, pos, account, pw, m, item, result)
}
func (s *Service) publishMove(id string, pos int, m Message, r MoveResult) error {
	return s.publishMoveWithWarning(id, pos, m, r, "", "")
}

// publishNormalMove publishes a move confirmed by the original write. When the
// plan has follow-up flags, the completion is stored together with a
// provisional flags_not_applied warning: from this commit until the flag
// change is confirmed and indexed (see publishMovedFlags), a process restart
// must leave a visible warning instead of a silent success. Recovery never
// replays the flag STORE.
func (s *Service) publishNormalMove(id string, pos int, m Message, item PlanItem, r MoveResult) error {
	if item.Flags.Empty() {
		return s.publishMove(id, pos, m, r)
	}
	return s.publishMoveWithWarning(id, pos, m, r, "flags_not_applied", flagsWarning(item, flagsPending))
}

// publishMoveWithWarning rebinds the stable ref to the confirmed destination
// and completes the item in one transaction; a non-empty warning code (such as
// flags_not_applied) is stored atomically with the completion.
func (s *Service) publishMoveWithWarning(id string, pos int, m Message, r MoveResult, warning, detail string) error {
	if r.UID == 0 || r.UIDValidity == 0 || r.Folder == "" {
		return ErrUncertain
	}
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return ErrUncertain
	}
	defer tx.Rollback()
	// A background scan may already have indexed the confirmed destination. Remove
	// only that exact remote position before rebinding the stable local identity.
	if _, e = tx.Exec("DELETE FROM messages WHERE account_id=? AND folder=? AND uidvalidity=? AND uid=? AND ref<>?", m.AccountID, r.Folder, r.UIDValidity, r.UID, m.Ref); e != nil {
		return ErrUncertain
	}
	_, e = tx.Exec(`INSERT INTO messages(`+messageColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(ref) DO UPDATE SET folder=excluded.folder,uidvalidity=excluded.uidvalidity,uid=excluded.uid,changed_at=excluded.changed_at`, m.Ref, m.AccountID, r.Folder, r.UIDValidity, r.UID, m.Subject, m.Sender, m.SentAt, m.Unread, m.Size, m.ReceivedAt, m.Flagged, m.FlagsKnown, time.Now().Unix(), m.MetadataVersion, m.Recipients, m.Cc, m.MessageID, m.InReplyTo, m.References, m.ListID, m.HasAttachment)
	if e != nil {
		return ErrUncertain
	}
	if _, e = tx.Exec("UPDATE operation_items SET state='completed',error_code=?,recovery_code=?,recovery_detail=?,checked_at=? WHERE operation_id=? AND position=?", warning, warning, detail, time.Now().Unix(), id, pos); e != nil {
		return ErrUncertain
	}
	if e = tx.Commit(); e != nil {
		return ErrUncertain
	}
	s.revision.Add(1)
	s.wakeAutomatic()
	return nil
}
