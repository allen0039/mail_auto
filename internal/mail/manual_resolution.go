package mail

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"mail-auto/internal/store"
)

// DuplicateVerifier must read complete raw messages with PEEK on read-only
// mailboxes. Matching metadata or rendered text is never sufficient.
type DuplicateVerifier interface {
	VerifyDuplicate(context.Context, Account, string, Message, Message) (DuplicateEvidence, error)
}

type DuplicateEvidence struct {
	SHA256      string   `json:"sha256"`
	Bytes       int64    `json:"bytes"`
	SourceFlags []string `json:"source_flags"`
	TargetFlags []string `json:"target_flags"`
}

type ManualLocation struct {
	Ref         string `json:"ref"`
	Folder      string `json:"folder"`
	UIDValidity uint32 `json:"uidvalidity"`
	UID         uint32 `json:"uid"`
}

func manualLocation(m Message) ManualLocation {
	return ManualLocation{m.Ref, m.Folder, m.UIDValidity, m.UID}
}

type ManualKeepEvidence struct {
	OperationID string         `json:"operation_id"`
	Source      ManualLocation `json:"source"`
	Target      ManualLocation `json:"target"`
	MessageID   string         `json:"message_id"`
	DuplicateEvidence
	CheckedAt int64 `json:"checked_at"`
}

type ManualResolution struct {
	ManualKeepEvidence
	Outcome   string `json:"outcome"`
	Actor     string `json:"actor"`
	Reason    string `json:"reason"`
	CreatedAt int64  `json:"created_at"`
	// Original operation and plan are immutable evidence, not replayable input.
	OriginalOperation Operation `json:"original_operation"`
	OriginalPlan      Plan      `json:"original_plan"`
}

type ManualKeepInput struct {
	Evidence ManualKeepEvidence `json:"evidence"`
	Confirm  bool               `json:"confirm"`
}

type manualQuery interface{ QueryRow(string, ...any) *sql.Row }

func manuallyKept(q manualQuery, m Message) (bool, error) {
	var n int
	err := q.QueryRow(`SELECT count(*) FROM manual_keep_blocks b WHERE account_id=? AND (ref=? OR (folder=? AND uidvalidity=? AND uid=?) OR (message_id<>'' AND message_id=?) OR (?<>'' AND EXISTS (SELECT 1 FROM messages sibling WHERE sibling.account_id=b.account_id AND sibling.gmail_message_id=? AND (sibling.ref=b.ref OR (sibling.folder=b.folder AND sibling.uidvalidity=b.uidvalidity AND sibling.uid=b.uid)))))`, m.AccountID, m.Ref, m.Folder, m.UIDValidity, m.UID, m.MessageID, m.GmailMessageID, m.GmailMessageID).Scan(&n)
	return n > 0, err
}

// Previews use the same persistent protection as actual automation claims.
func (s *Service) evaluateIndexedRules(m Message, rules []Rule, p AutomationPolicy, now int64) (RuleEvaluation, error) {
	kept, err := manuallyKept(s.Store.DB, m)
	if err != nil {
		return RuleEvaluation{}, err
	}
	if kept {
		return RuleEvaluation{Message: m, Signature: messageSignature(m), Status: "protected", Action: "keep", Reasons: []string{"人工核验保留，自动整理永久跳过"}}, nil
	}
	return s.evaluateMessageRules(m, rules, p, now)
}

func (s *Service) manualResolution(id string) (*ManualResolution, error) {
	var raw string
	err := s.Store.DB.QueryRow("SELECT payload FROM manual_resolutions WHERE operation_id=?", id).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out ManualResolution
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil, ErrConflict
	}
	return &out, nil
}

// PreviewManualKeep obtains current remote evidence without any remote mutation.
func (s *Service) PreviewManualKeep(ctx context.Context, a Access, id string) (ManualKeepEvidence, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	if a.Owner != "admin" {
		return ManualKeepEvidence{}, ErrDenied
	}
	if _, err := s.CheckAccess(a); err != nil {
		return ManualKeepEvidence{}, err
	}
	op, err := s.Operation(a, id)
	if err != nil {
		return ManualKeepEvidence{}, err
	}
	if op.State != "needs_verification" || len(op.Items) != 1 || op.Items[0].State != "needs_verification" {
		return ManualKeepEvidence{}, ErrConflict
	}
	p, err := s.GetPlan(a, op.PlanID)
	if err != nil {
		return ManualKeepEvidence{}, err
	}
	if len(p.Items) != 1 || p.Items[0].Skip != "" || !isMoveAction(p.Action) {
		return ManualKeepEvidence{}, ErrConflict
	}
	release, err := s.acquireRecovery(p.Items[0].Message.AccountID)
	if err != nil {
		return ManualKeepEvidence{}, err
	}
	defer release()
	evidence, _, _, err := s.inspectManualKeep(ctx, op, p)
	return evidence, err
}

func isMoveAction(action string) bool {
	return action == "move" || action == "archive" || action == "trash" || action == "restore"
}

func (s *Service) inspectManualKeep(ctx context.Context, op Operation, p Plan) (ManualKeepEvidence, Message, Message, error) {
	var out ManualKeepEvidence
	source, err := s.Message(op.Items[0].Ref)
	if err != nil {
		return out, source, Message{}, err
	}
	if source.GmailMessageID != "" {
		return out, source, Message{}, blocked("gmail_keep_both_unsupported", "Gmail 多个标签视图属于同一逻辑邮件，不能按保留两份结案", ErrCapability)
	}
	frozen := p.Items[0]
	if source.AccountID != frozen.Message.AccountID || source.Folder != frozen.Message.Folder || source.UID != frozen.Message.UID || source.UIDValidity != frozen.Message.UIDValidity || source.MessageID == "" || source.Size <= 0 || source.Subject != frozen.Message.Subject || source.Sender != frozen.Message.Sender || source.SentAt != frozen.Message.SentAt || source.Size != frozen.Message.Size {
		return out, source, Message{}, ErrConflict
	}
	var ref string
	var count int
	err = s.Store.DB.QueryRow(`SELECT count(*),COALESCE(min(ref),'') FROM messages WHERE account_id=? AND folder=? AND message_id=?`, source.AccountID, frozen.Destination, source.MessageID).Scan(&count, &ref)
	if err != nil {
		return out, source, Message{}, err
	}
	if count != 1 {
		return out, source, Message{}, ErrConflict
	}
	target, err := s.Message(ref)
	if err != nil {
		return out, source, target, err
	}
	if target.Ref == source.Ref || target.Folder == source.Folder || target.Subject != source.Subject || target.Sender != source.Sender || target.SentAt != source.SentAt || target.Size != source.Size {
		return out, source, target, ErrConflict
	}
	// Keep a foreign queued/uncertain operation from changing either copy while
	// resolving this one. The transaction below repeats this check before commit.
	if err = manualKeepConflicts(s.Store.DB, op.ID, source, target); err != nil {
		return out, source, target, err
	}
	verifier, ok := s.Remote.(DuplicateVerifier)
	if !ok {
		return out, source, target, ErrCapability
	}
	account, err := s.Account(source.AccountID)
	if err != nil {
		return out, source, target, err
	}
	if !recoveryIdentityMatches(account, frozen.Config) {
		return out, source, target, ErrConflict
	}
	pw, err := s.password(account)
	if err != nil {
		return out, source, target, err
	}
	evidence, err := verifier.VerifyDuplicate(ctx, account, pw, source, target)
	if err != nil {
		return out, source, target, err
	}
	digest, decodeErr := hex.DecodeString(evidence.SHA256)
	if decodeErr != nil || len(digest) != 32 || evidence.Bytes != int64(source.Size) || !sameManualFlags(evidence.SourceFlags, evidence.TargetFlags) {
		return out, source, target, ErrConflict
	}
	out = ManualKeepEvidence{OperationID: op.ID, Source: manualLocation(source), Target: manualLocation(target), MessageID: source.MessageID, DuplicateEvidence: evidence, CheckedAt: time.Now().Unix()}
	return out, source, target, nil
}

func sameManualFlags(a, b []string) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func manualKeepConflicts(q manualQuery, id string, source, target Message) error {
	var n int
	err := q.QueryRow(`SELECT count(*) FROM operation_items i JOIN operations o ON o.id=i.operation_id WHERE (i.ref IN (?,?) OR i.ref IN (SELECT ref FROM messages WHERE account_id=? AND message_id=?)) AND o.id<>? AND i.state IN ('queued','running','needs_verification')`, source.Ref, target.Ref, source.AccountID, source.MessageID, id).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrBusy
	}
	err = q.QueryRow(`SELECT count(*) FROM automation_claims WHERE account_id=? AND (ref IN (?,?) OR ref IN (SELECT ref FROM messages WHERE account_id=? AND message_id=?)) AND state IN ('claimed','planned') AND expires>? AND plan_id<>COALESCE((SELECT plan_id FROM operations WHERE id=?),'')`, source.AccountID, source.Ref, target.Ref, source.AccountID, source.MessageID, time.Now().Unix(), id).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrBusy
	}
	return nil
}

// ResolveManualKeep never calls MOVE, COPY, STORE, EXPUNGE or Reconcile. Its
// only side effect is an atomic local evidence+block+terminal-state commit.
func (s *Service) ResolveManualKeep(ctx context.Context, a Access, id string, in ManualKeepInput) (*ManualResolution, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	if a.Owner != "admin" {
		return nil, ErrDenied
	}
	if _, err := s.CheckAccess(a); err != nil {
		return nil, err
	}
	if !in.Confirm || in.Evidence.OperationID != id || in.Evidence.SHA256 == "" {
		return nil, ErrInput
	}
	if saved, err := s.manualResolution(id); err != nil || saved != nil {
		if saved != nil && !sameKeepIdentity(saved.ManualKeepEvidence, in.Evidence) {
			return nil, ErrConflict
		}
		return saved, err
	}
	op, err := s.Operation(a, id)
	if err != nil {
		return nil, err
	}
	if op.State != "needs_verification" || len(op.Items) != 1 || op.Items[0].State != "needs_verification" {
		return nil, ErrConflict
	}
	p, err := s.GetPlan(a, op.PlanID)
	if err != nil {
		return nil, err
	}
	if len(p.Items) != 1 || p.Items[0].Skip != "" || !isMoveAction(p.Action) {
		return nil, ErrConflict
	}
	if in.Evidence.CheckedAt > time.Now().Unix() || time.Now().Unix()-in.Evidence.CheckedAt > 600 {
		return nil, ErrConflict
	}
	release, err := s.acquireRecovery(p.Items[0].Message.AccountID)
	if err != nil {
		return nil, err
	}
	defer release()
	before, err := s.Account(p.Items[0].Message.AccountID)
	if err != nil {
		return nil, err
	}
	evidence, source, target, err := s.inspectManualKeep(ctx, op, p)
	if err != nil {
		return nil, err
	}
	if !sameKeepIdentity(evidence, in.Evidence) {
		return nil, ErrConflict
	}
	resolution := &ManualResolution{ManualKeepEvidence: evidence, Outcome: "keep_both", Actor: "admin", Reason: "管理员确认两份完整邮件一致，保留两份并阻止自动整理再次领取", CreatedAt: time.Now().Unix(), OriginalOperation: op, OriginalPlan: p}
	tx, err := s.Store.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var adminVersion, restricted int
	if err = tx.QueryRow("SELECT username,version,must_change FROM admin WHERE id=1").Scan(&resolution.Actor, &adminVersion, &restricted); err != nil {
		return nil, err
	}
	if adminVersion != a.Version || restricted != 0 {
		return nil, ErrDenied
	}
	account, err := scanAccount(tx.QueryRow("SELECT "+accountColumns+" FROM accounts WHERE id=?", source.AccountID))
	if err != nil {
		return nil, err
	}
	if err = s.accountNetworkIn(tx, &account); err != nil {
		return nil, err
	}
	if accountIdentity(account) != accountIdentity(before) || !recoveryIdentityMatches(account, p.Items[0].Config) {
		return nil, ErrConflict
	}
	// A second process may have resolved or changed the operation while this
	// process performed network reads. Roll back rather than overwrite evidence.
	var state string
	if err = tx.QueryRow("SELECT state FROM operations WHERE id=?", id).Scan(&state); err != nil {
		return nil, err
	}
	if state != "needs_verification" {
		if state == "manual_kept" {
			var raw string
			if err = tx.QueryRow("SELECT payload FROM manual_resolutions WHERE operation_id=?", id).Scan(&raw); err == nil {
				var saved ManualResolution
				if json.Unmarshal([]byte(raw), &saved) == nil && sameKeepIdentity(saved.ManualKeepEvidence, in.Evidence) {
					return &saved, nil
				}
			}
		}
		return nil, ErrConflict
	}
	if err = manualKeepConflicts(tx, id, source, target); err != nil {
		return nil, err
	}
	for _, m := range []Message{source, target} {
		current, err := scanMessage(tx.QueryRow("SELECT "+messageColumns+" FROM messages WHERE ref=?", m.Ref))
		if err != nil {
			return nil, err
		}
		if current.AccountID != m.AccountID || current.Folder != m.Folder || current.UIDValidity != m.UIDValidity || current.UID != m.UID || current.MessageID != m.MessageID || messageSignature(current) != messageSignature(m) {
			return nil, ErrConflict
		}
		if _, err = tx.Exec(`INSERT INTO manual_keep_blocks(account_id,ref,folder,uidvalidity,uid,message_id,resolution_id) VALUES(?,?,?,?,?,?,?)`, m.AccountID, m.Ref, m.Folder, m.UIDValidity, m.UID, m.MessageID, id); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(resolution)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec("INSERT INTO manual_resolutions(operation_id,account_id,payload,created_at) VALUES(?,?,?,?)", id, source.AccountID, string(raw), resolution.CreatedAt); err != nil {
		return nil, err
	}
	result, err := tx.Exec("UPDATE operation_items SET state='manual_kept',recovery_code='manual_keep_both',recovery_detail='已人工核验两份原文一致，保留两份；自动整理永久跳过',checked_at=? WHERE operation_id=? AND state='needs_verification'", resolution.CreatedAt, id)
	if err != nil {
		return nil, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, ErrConflict
	}
	if _, err = tx.Exec("UPDATE operations SET state='manual_kept',updated_at=? WHERE id=?", resolution.CreatedAt, id); err != nil {
		return nil, err
	}
	if err = store.Audit(tx, "mail.operation.manual_keep", id, "keep_both"); err != nil {
		return nil, err
	}
	// Update the originating run in this same transaction: no completed write
	// is invented, and an older task cursor is never rewritten.
	var runID, runRaw string
	err = tx.QueryRow(`SELECT r.id,r.payload FROM task_runs r JOIN task_run_batches rb ON rb.run_id=r.id JOIN automation_claims c ON c.batch_id=rb.batch_id WHERE c.plan_id=? LIMIT 1`, op.PlanID).Scan(&runID, &runRaw)
	if err == nil {
		var r TaskRun
		if json.Unmarshal([]byte(runRaw), &r) != nil {
			return nil, ErrConflict
		}
		if err = tx.QueryRow(`SELECT COALESCE(sum(i.state='manual_kept'),0),COALESCE(sum(i.state='needs_verification'),0),COALESCE(sum(i.state='completed'),0),COALESCE(sum(i.state='failed'),0),COALESCE(sum(i.state='queued'),0),COALESCE(sum(i.state='running'),0),COALESCE(sum(i.state IN ('skipped','cancelled')),0) FROM operation_items i JOIN operations o ON o.id=i.operation_id WHERE o.plan_id IN (SELECT plan_id FROM automation_claims WHERE batch_id=?)`, r.BatchID).Scan(&r.ManualKept, &r.Unknown, &r.Completed, &r.Failed, &r.Queued, &r.Running, &r.Skipped); err != nil {
			return nil, err
		}
		if r.Unknown == 0 && r.State == "needs_verification" {
			r.State = "manual_kept"
			r.ErrorCode = ""
			if r.NeedsAI > 0 {
				r.State = "requires_ai"
			}
			if r.Failed > 0 {
				r.State = "failed"
				if r.Completed > 0 {
					r.State = "partial"
				}
				r.ErrorCode = "mail_operation_failed"
			}
			if r.Queued+r.Running > 0 {
				r.State = "running"
			}
		}
		r.Finalized = r.Queued+r.Running == 0
		r.UpdatedAt = resolution.CreatedAt
		if _, err = tx.Exec("UPDATE task_runs SET state=?,payload=?,updated_at=? WHERE id=?", r.State, manualRunJSON(r, runRaw), r.UpdatedAt, runID); err != nil {
			return nil, err
		}
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return resolution, nil
}

func manualRunJSON(r TaskRun, old string) string {
	var values map[string]any
	_ = json.Unmarshal([]byte(old), &values)
	for k, v := range map[string]any{"state": r.State, "manual_kept": r.ManualKept, "unknown": r.Unknown, "completed": r.Completed, "failed": r.Failed, "queued": r.Queued, "running": r.Running, "skipped": r.Skipped, "error_code": r.ErrorCode, "finalized": r.Finalized, "updated_at": r.UpdatedAt} {
		values[k] = v
	}
	raw, _ := json.Marshal(values)
	return string(raw)
}
func sameKeepIdentity(a, b ManualKeepEvidence) bool {
	return a.OperationID == b.OperationID && a.Source == b.Source && a.Target == b.Target && a.MessageID == b.MessageID && strings.EqualFold(a.SHA256, b.SHA256) && a.Bytes == b.Bytes && sameManualFlags(a.SourceFlags, b.SourceFlags) && sameManualFlags(a.TargetFlags, b.TargetFlags)
}
