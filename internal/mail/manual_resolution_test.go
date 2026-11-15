package mail

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mail-auto/internal/store"
)

type duplicateRemote struct {
	*automationRemote
	calls    int
	evidence DuplicateEvidence
	err      error
	onVerify func()
}

func (r *duplicateRemote) VerifyDuplicate(context.Context, Account, string, Message, Message) (DuplicateEvidence, error) {
	r.calls++
	if r.onVerify != nil {
		r.onVerify()
	}
	return r.evidence, r.err
}

func manualKeepFixture(t *testing.T) (*Service, *duplicateRemote, Access, TaskRun, Operation, Message, Message) {
	t.Helper()
	s, remote, admin, _, task, m := taskFixture(t, "execute", "manual", "rules")
	mustExec(t, s, "UPDATE messages SET message_id='<duplicate@test>',size=100 WHERE ref=?", m.Ref)
	m, _ = s.Message(m.Ref)
	remote.uncertain = true
	remote.omitMapping = true
	run, err := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "uncertain"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, "UPDATE task_runs SET state='running',lease_owner=?,lease_until=? WHERE id=?", s.workerID, time.Now().Unix()+600, run.ID)
	access := Access{Owner: run.Owner, Version: run.AdminVersion}
	batch, err := s.ClaimAI(access, AIClaimInput{RunID: run.ID, AccountID: m.AccountID})
	if err != nil {
		t.Fatal(err)
	}
	batch, err = s.SubmitAI(context.Background(), access, AISubmitInput{BatchID: batch.ID, IdempotencyKey: "submit", Decisions: decisionFor(batch)})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.PlanIDs) != 1 {
		t.Fatal(batch)
	}
	created, err := s.ExecuteAI(access, batch.ID, batch.PlanIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	s.runOperation(context.Background(), created.ID)
	s.reconcileTaskRun(run.ID)
	var opID string
	if err = s.Store.DB.QueryRow("SELECT id FROM operations ORDER BY created_at LIMIT 1").Scan(&opID); err != nil {
		t.Fatal(err)
	}
	op, err := s.Operation(admin, opID)
	if err != nil || op.State != "needs_verification" {
		t.Fatal(op, err)
	}
	target := m
	target.Ref = "duplicate-target"
	target.Folder = "Archive"
	target.UIDValidity = 7
	target.UID = 99
	mustExec(t, s, "INSERT INTO messages("+messageColumns+") SELECT ?,account_id,?, ?,?,subject,sender,sent_at,unread,size,received_at,flagged,flags_known,changed_at,metadata_version,recipients,cc,message_id,in_reply_to,thread_refs,list_id,has_attachment FROM messages WHERE ref=?", target.Ref, target.Folder, target.UIDValidity, target.UID, m.Ref)
	r := &duplicateRemote{automationRemote: remote, evidence: DuplicateEvidence{SHA256: strings.Repeat("a", 64), Bytes: int64(m.Size), SourceFlags: []string{"\\Seen"}, TargetFlags: []string{"\\Seen"}}}
	s.Remote = r
	return s, r, admin, run, op, m, target
}

func TestManualKeepAtomicIdempotentAndPermanent(t *testing.T) {
	s, r, admin, run, op, source, target := manualKeepFixture(t)
	evidence, err := s.PreviewManualKeep(context.Background(), admin, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeMoves := r.moves
	resolved, err := s.ResolveManualKeep(context.Background(), admin, op.ID, ManualKeepInput{Evidence: evidence, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Outcome != "keep_both" || resolved.OriginalOperation.State != "needs_verification" || resolved.OriginalOperation.Items[0].Error != "remote_state_unknown" || resolved.Source.UID != source.UID || resolved.Target.UID != target.UID {
		t.Fatal(resolved)
	}
	if r.calls != 2 || r.moves != beforeMoves || r.finishCallCount != 0 {
		t.Fatal("resolution issued a mail mutation", r)
	}
	saved, err := s.Operation(admin, op.ID)
	if err != nil || saved.State != "manual_kept" || saved.Items[0].State != "manual_kept" || saved.ManualResolution == nil {
		t.Fatal(saved, err)
	}
	// Original absent server COPYUID must remain absent, rather than forged.
	if saved.Items[0].Result.UID != 0 {
		t.Fatal("forged COPYUID")
	}
	retry, err := s.ResolveManualKeep(context.Background(), admin, op.ID, ManualKeepInput{Evidence: evidence, Confirm: true})
	if err != nil || retry.CreatedAt != resolved.CreatedAt || r.calls != 2 {
		t.Fatal(retry, err, r.calls)
	}
	var count int
	s.Store.DB.QueryRow("SELECT count(*) FROM audits WHERE action='mail.operation.manual_keep'").Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
	s.reconcileTaskRun(run.ID)
	actual, err := s.TaskRun(admin, run.ID)
	if err != nil || actual.Unknown != 0 || actual.ManualKept != 1 || actual.Completed != 0 || actual.Failed != 0 || actual.State != "manual_kept" || !actual.Finalized {
		t.Fatal(actual, err)
	}
	for _, m := range []Message{source, target} {
		available, err := s.claimAvailable(m.AccountID, m.Ref, "changed-policy-and-rule-fingerprint", time.Now().Unix())
		if err != nil || available {
			t.Fatal("protected copy became claimable", m, err)
		}
	}
	preview, err := s.PreviewTask(admin, run.TaskID, "", false)
	if err != nil || len(preview.Items) != 1 || preview.Items[0].Status != "protected" || !strings.Contains(preview.Items[0].Reasons[0], "人工核验") {
		t.Fatal("preview hides manual protection", preview, err)
	}
	rulePreview, err := s.PreviewRules(admin, RulePreviewInput{AccountID: source.AccountID})
	if err != nil || len(rulePreview.Items) != 1 || rulePreview.Items[0].Status != "protected" {
		t.Fatal("rule preview hides protection", rulePreview, err)
	}
	// A different source scope and policy/rule digest must also obey protection.
	policy, err := s.AutomationPolicy(admin, source.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, "UPDATE folder_settings SET archive_folder='' WHERE account_id=?", source.AccountID)
	policy.SourceFolders = []string{"INBOX", "Archive"}
	policy.ProtectUnread = false
	policy.ProtectFlagged = false
	if _, err = s.SaveAutomationPolicy(admin, policy); err != nil {
		t.Fatal(err)
	}
	batch, err := s.ClaimAI(admin, AIClaimInput{AccountID: source.AccountID, RunKey: "after-policy-change"})
	if err != nil || len(batch.Items) != 0 || batch.Protected != 2 {
		t.Fatal("policy change bypassed keep blocks", batch, err)
	}
	// Durable evidence and blocks survive removal of expiring history, claims,
	// and sync reindexing that creates new physical UIDs/refs for same Message-ID.
	mustExec(t, s, "DELETE FROM automation_claims")
	mustExec(t, s, "UPDATE operations SET state='completed',updated_at=1 WHERE id=?", op.ID)
	s.Store.Cleanup(context.Background())
	mustExec(t, s, "UPDATE messages SET ref='reindexed',folder='Trash',uidvalidity=88,uid=123 WHERE ref=?", source.Ref)
	reindexed, err := s.Message("reindexed")
	if err != nil {
		t.Fatal(err)
	}
	if keep, err := manuallyKept(s.Store.DB, reindexed); err != nil || !keep {
		t.Fatal("reindex lost protection", err)
	}
	directory := t.TempDir()
	if err = s.Store.Backup(directory + "/mail-auto.db"); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if keep, err := manuallyKept(reopened.DB, reindexed); err != nil || !keep {
		t.Fatal("restart lost protection", err)
	}
	var payload string
	if err = reopened.DB.QueryRow("SELECT payload FROM manual_resolutions WHERE operation_id=?", op.ID).Scan(&payload); err != nil || !strings.Contains(payload, "remote_state_unknown") {
		t.Fatal("cleanup lost original evidence", err)
	}
}

func TestManualKeepRejectsChangedEvidenceAndPreservesUnknown(t *testing.T) {
	for _, mode := range []string{"digest_changed", "flags_changed", "timeout", "no_confirm", "stale_preview", "wrong_target", "extra_target", "source_moved", "foreign_pending", "non_admin", "changed_index_during_read"} {
		t.Run(mode, func(t *testing.T) {
			s, r, admin, _, op, source, target := manualKeepFixture(t)
			evidence, err := s.PreviewManualKeep(context.Background(), admin, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			input := ManualKeepInput{Evidence: evidence, Confirm: true}
			access := admin
			switch mode {
			case "digest_changed":
				r.evidence.SHA256 = strings.Repeat("b", 64)
			case "flags_changed":
				r.evidence.TargetFlags = []string{"\\Flagged", "\\Seen"}
			case "timeout":
				r.err = context.DeadlineExceeded
			case "no_confirm":
				input.Confirm = false
			case "stale_preview":
				input.Evidence.CheckedAt = time.Now().Unix() - 601
			case "wrong_target":
				input.Evidence.Target.UID++
			case "extra_target":
				mustExec(t, s, "INSERT INTO messages("+messageColumns+") SELECT 'third-copy',account_id,folder,uidvalidity,100,subject,sender,sent_at,unread,size,received_at,flagged,flags_known,changed_at,metadata_version,recipients,cc,message_id,in_reply_to,thread_refs,list_id,has_attachment FROM messages WHERE ref=?", target.Ref)
			case "source_moved":
				mustExec(t, s, "UPDATE messages SET folder='Other',uid=900 WHERE ref=?", source.Ref)
			case "foreign_pending":
				mustExec(t, s, "INSERT INTO operation_items(operation_id,position,ref,state) VALUES(?,1,?,'running')", op.ID, target.Ref)
			case "non_admin":
				access.Owner = "external"
			case "changed_index_during_read":
				r.onVerify = func() { mustExec(t, s, "UPDATE messages SET flagged=1 WHERE ref=?", source.Ref) }
			}
			_, err = s.ResolveManualKeep(context.Background(), access, op.ID, input)
			if err == nil {
				t.Fatal("unsafe evidence accepted")
			}
			var blocks, resolutions int
			s.Store.DB.QueryRow("SELECT count(*) FROM manual_keep_blocks").Scan(&blocks)
			s.Store.DB.QueryRow("SELECT count(*) FROM manual_resolutions").Scan(&resolutions)
			saved, _ := s.Operation(admin, op.ID)
			if blocks != 0 || resolutions != 0 || saved.State != "needs_verification" || r.finishCallCount != 0 || r.moves != 1 {
				t.Fatal("failed verification changed durable state or mail", blocks, resolutions, saved, r.moves)
			}
		})
	}
}

func TestManualKeepBlocksPreparedWriteAndReconcile(t *testing.T) {
	s, r, admin, _, op, source, target := manualKeepFixture(t)
	// Prepare a target plan before resolution, but do not execute it yet.
	p, err := s.NewPlan(context.Background(), admin, PlanInput{Refs: []string{target.Ref}, Action: "trash"})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := s.PreviewManualKeep(context.Background(), admin, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveManualKeep(context.Background(), admin, op.ID, ManualKeepInput{Evidence: evidence, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ExecutePlan(admin, p.ID, "stale-plan"); !errors.Is(err, ErrDenied) {
		t.Fatal("prepared write bypassed block", err)
	}
	before := r.inspectCallCount
	if out, err := s.Reconcile(context.Background(), admin, op.ID); err != nil || out.State != "manual_kept" || r.inspectCallCount != before || r.finishCallCount != 0 {
		t.Fatal("terminal manual resolution recovered remotely", out, err)
	}
	// Other messages do not inherit the protection.
	other := source
	other.Ref = "unrelated"
	other.UID++
	other.MessageID = "<other@test>"
	if kept, err := manuallyKept(s.Store.DB, other); err != nil || kept {
		t.Fatal("unrelated mail blocked", err)
	}
}

// Distinct service mutexes model two processes sharing the same database.
func TestManualKeepConcurrentConfirmAndPreparedExecution(t *testing.T) {
	for _, mode := range []string{"double_confirm", "queued_during_read", "admin_changed", "account_changed", "expired_claim"} {
		t.Run(mode, func(t *testing.T) {
			s, r, admin, _, op, source, target := manualKeepFixture(t)
			evidence, err := s.PreviewManualKeep(context.Background(), admin, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			r2 := &duplicateRemote{automationRemote: r.automationRemote, evidence: r.evidence}
			other := &Service{Store: s.Store, Vault: s.Vault, Remote: r2, Context: context.Background()}
			input := ManualKeepInput{Evidence: evidence, Confirm: true}
			switch mode {
			case "double_confirm":
				r.onVerify = func() {
					if _, e := other.ResolveManualKeep(context.Background(), admin, op.ID, input); e != nil {
						t.Fatal(e)
					}
				}
			case "queued_during_read":
				p, e := s.NewPlan(context.Background(), admin, PlanInput{Refs: []string{target.Ref}, Action: "trash"})
				if e != nil {
					t.Fatal(e)
				}
				r.onVerify = func() {
					if _, e := other.ExecutePlan(admin, p.ID, "other-process"); e != nil {
						t.Fatal(e)
					}
				}
			case "admin_changed":
				r.onVerify = func() { mustExec(t, s, "UPDATE admin SET version=version+1") }
			case "account_changed":
				r.onVerify = func() { mustExec(t, s, "UPDATE accounts SET host='imap.126.com' WHERE id=?", source.AccountID) }
			case "expired_claim":
				mustExec(t, s, "INSERT INTO automation_claims(account_id,ref,fingerprint,state,expires,plan_id,batch_id) SELECT ?,?,'other','claimed',1,'',batch_id FROM automation_claims LIMIT 1", source.AccountID, target.Ref)
			}
			_, err = s.ResolveManualKeep(context.Background(), admin, op.ID, input)
			wantSuccess := mode == "double_confirm" || mode == "expired_claim"
			if (err == nil) != wantSuccess {
				t.Fatal(mode, err)
			}
			var resolutions, blocks, audits int
			s.Store.DB.QueryRow("SELECT count(*) FROM manual_resolutions").Scan(&resolutions)
			s.Store.DB.QueryRow("SELECT count(*) FROM manual_keep_blocks").Scan(&blocks)
			s.Store.DB.QueryRow("SELECT count(*) FROM audits WHERE action='mail.operation.manual_keep'").Scan(&audits)
			want := 0
			if wantSuccess {
				want = 1
			}
			if resolutions != want || blocks != want*2 || audits != want || r.moves != 1 || r.finishCallCount != 0 {
				t.Fatal(resolutions, blocks, audits, r.moves)
			}
		})
	}
}

func TestManualKeepDoesNotHideOtherFailureInRun(t *testing.T) {
	s, _, admin, run, op, _, target := manualKeepFixture(t)
	evidence, err := s.PreviewManualKeep(context.Background(), admin, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, "INSERT INTO operation_items(operation_id,position,ref,state,error_code) VALUES(?,1,?,'failed','other_failure')", op.ID, target.Ref)
	// Keep the original operation single-item (the API deliberately only resolves
	// single-item operations), and attach the other failure via another plan.
	mustExec(t, s, "INSERT INTO operation_plans SELECT 'other-plan',owner,payload,created_at,expires,settings_version FROM operation_plans WHERE id=?", op.PlanID)
	mustExec(t, s, "INSERT INTO operations(id,plan_id,owner,idempotency_key,state,created_at,updated_at) VALUES('other-operation','other-plan','admin','other-key','failed',1,1)")
	mustExec(t, s, "UPDATE operation_items SET operation_id='other-operation',position=0 WHERE operation_id=? AND position=1", op.ID)
	mustExec(t, s, "INSERT INTO automation_claims(account_id,ref,fingerprint,state,expires,plan_id,batch_id) SELECT account_id,?,'other','planned',1,'other-plan',batch_id FROM automation_claims LIMIT 1", target.Ref)
	if _, err = s.ResolveManualKeep(context.Background(), admin, op.ID, ManualKeepInput{Evidence: evidence, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	r, err := s.TaskRun(admin, run.ID)
	if err != nil || r.State != "failed" || r.Failed != 1 || r.ManualKept != 1 || r.Completed != 0 || r.Unknown != 0 {
		t.Fatal(r, err)
	}
}

func TestManualKeepBlocksRecoveryOfAnotherHistoricalCopy(t *testing.T) {
	s, r, admin, _, op, source, _ := manualKeepFixture(t)
	evidence, err := s.PreviewManualKeep(context.Background(), admin, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveManualKeep(context.Background(), admin, op.ID, ManualKeepInput{Evidence: evidence, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	// A stale third-copy operation must not bypass protection via FinishMove.
	p, err := s.GetPlan(admin, op.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	account, err := s.Account(source.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	before := r.finishCallCount
	err = s.finishRecoveredMove(context.Background(), admin, op, p, p.Items[0], account, "unused", MoveResult{Folder: p.Items[0].Destination, UIDValidity: 7, UID: 99})
	if !errors.Is(err, ErrDenied) || r.finishCallCount != before {
		t.Fatal("recovery bypassed block", err)
	}
}
