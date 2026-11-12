package mail

import (
	"context"
	"testing"
	"time"
)

type uncertainRun struct {
	s         *Service
	remote    *automationRemote
	admin     Access
	access    Access
	run       TaskRun
	batch     AIBatch
	operation string
}

// startUncertainMove issues one automated move whose COPYUID mapping was saved
// but whose completion is unknown, leaving the task run active.
func startUncertainMove(t *testing.T, key string) uncertainRun {
	t.Helper()
	s, remote, admin, _, task, message := taskFixture(t, "execute", "manual", "rules")
	remote.uncertain = true
	remote.verifyErr = ErrUncertain
	run, err := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: key}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.DB.Exec("UPDATE task_runs SET state='running',lease_owner=?,lease_until=? WHERE id=?", s.workerID, time.Now().Unix()+45, run.ID); err != nil {
		t.Fatal(err)
	}
	access := Access{Owner: run.Owner, Version: run.AdminVersion}
	batch, err := s.ClaimAI(access, AIClaimInput{RunID: run.ID, AccountID: message.AccountID})
	if err != nil {
		t.Fatal(err)
	}
	if batch, err = s.SubmitAI(context.Background(), access, AISubmitInput{BatchID: batch.ID, IdempotencyKey: key, Decisions: decisionFor(batch)}); err != nil || len(batch.PlanIDs) != 1 {
		t.Fatal(batch, err)
	}
	op, err := s.ExecuteAI(access, batch.ID, batch.PlanIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	s.runOperation(context.Background(), op.ID)
	op, err = s.Operation(access, op.ID)
	if err != nil || op.State != "needs_verification" || op.Items[0].Result.UID == 0 {
		t.Fatal("setup did not leave a mapped uncertain move", op, err)
	}
	return uncertainRun{s: s, remote: remote, admin: admin, access: access, run: run, batch: batch, operation: op.ID}
}

func (u uncertainRun) reconcile(t *testing.T) Operation {
	t.Helper()
	op, err := u.s.Reconcile(context.Background(), u.access, u.operation)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestRecoveryFinishesMappedMoveAfterTaskStops(t *testing.T) {
	for _, scenario := range []string{"running", "needs_verification", "lease_expired", "deadline_passed", "restart"} {
		t.Run(scenario, func(t *testing.T) {
			u := startUncertainMove(t, scenario)
			switch scenario {
			case "needs_verification":
				u.s.reconcileTaskRun(u.run.ID)
			case "lease_expired":
				if _, err := u.s.Store.DB.Exec("UPDATE task_runs SET lease_until=? WHERE id=?", time.Now().Unix()-1, u.run.ID); err != nil {
					t.Fatal(err)
				}
				u.s.reconcileTaskRun(u.run.ID)
			case "deadline_passed":
				past := time.Now().Unix() - 1
				if _, err := u.s.Store.DB.Exec("UPDATE task_runs SET expires=?,payload=json_set(payload,'$.expires',?) WHERE id=?", past, past, u.run.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := u.s.Store.DB.Exec("UPDATE automation_batches SET expires=?,payload=json_set(payload,'$.expires',?) WHERE id=?", past, past, u.batch.ID); err != nil {
					t.Fatal(err)
				}
				u.s.reconcileTaskRun(u.run.ID)
			case "restart":
				// A new process has a new worker identity and resets running items.
				u.s.workerID = "after-restart"
				if _, err := u.s.Store.DB.Exec("UPDATE operation_items SET state='running' WHERE operation_id=?", u.operation); err != nil {
					t.Fatal(err)
				}
				if _, err := u.s.Store.DB.Exec(`UPDATE operation_items SET state='needs_verification',error_code='interrupted' WHERE state='running'; UPDATE operations SET state='needs_verification' WHERE id=?`, u.operation); err != nil {
					t.Fatal(err)
				}
				u.s.reconcileTaskRun(u.run.ID)
			}
			if scenario != "running" {
				if r, _ := u.s.loadTaskRun(u.run.ID); r.State == "running" {
					t.Fatal("scenario left the run active", r.State)
				}
			}
			op := u.reconcile(t)
			if op.State != "completed" || op.Items[0].State != "completed" || op.Items[0].RecoveryCode != "" {
				t.Fatalf("recovery did not finish: %+v", op)
			}
			if u.remote.finishCallCount != 1 || u.remote.finishStores != 1 || u.remote.finishExpunges != 1 || u.remote.moves != 1 {
				t.Fatal("recovery must clean the same source once and never re-issue MOVE", u.remote.finishCallCount, u.remote.finishExpunges, u.remote.moves)
			}
			m, err := u.s.Message(op.Items[0].Ref)
			if err != nil || m.Folder != "Archive" || m.UID != op.Items[0].Result.UID {
				t.Fatal("stable ref not rebound to the verified destination", m, err)
			}
			u.s.reconcileTaskRun(u.run.ID)
			if r, _ := u.s.loadTaskRun(u.run.ID); r.Unknown != 0 || r.Completed != 1 {
				t.Fatalf("run summary not settled: %+v", r)
			}
		})
	}
}

func TestRecoveryStaysUnknownWithReason(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		mutate     func(*testing.T, uncertainRun)
	}{
		{"flagged_before_check", "recovery_fingerprint_mismatch", func(t *testing.T, u uncertainRun) { u.remote.flagged = true }},
		{"rule_changed", "recovery_rules_changed", func(t *testing.T, u uncertainRun) {
			rules, err := u.s.Rules(u.admin, u.batch.AccountID)
			if err != nil || len(rules) == 0 {
				t.Fatal(rules, err)
			}
			rules[0].Priority++
			if _, err = u.s.SaveRule(u.admin, rules[0]); err != nil {
				t.Fatal(err)
			}
		}},
		{"user_stopped_batch", "recovery_operation_cancelled", func(t *testing.T, u uncertainRun) {
			if err := u.s.CancelAIBatch(u.access, u.batch.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"writes_disabled", "recovery_writes_disabled", func(t *testing.T, u uncertainRun) {
			fs, err := u.s.FolderSettings(u.batch.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			fs.WritesEnabled = false
			if err = u.s.SaveFolderSettings(context.Background(), fs); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := startUncertainMove(t, tc.name)
			u.s.reconcileTaskRun(u.run.ID)
			tc.mutate(t, u)
			op, err := u.s.Reconcile(context.Background(), u.access, u.operation)
			if err != nil {
				// Revoked access is recorded by the background reconciler instead.
				t.Fatal(err)
			}
			if op.State != "needs_verification" || op.Items[0].RecoveryCode != tc.code || op.Items[0].RecoveryDetail == "" || op.Items[0].CheckedAt == 0 {
				t.Fatalf("want %s, got %+v", tc.code, op.Items[0])
			}
			if u.remote.finishStores != 0 || u.remote.finishExpunges != 0 || u.remote.moves != 1 {
				t.Fatal("blocked recovery touched the mailbox", u.remote.finishStores, u.remote.finishExpunges, u.remote.moves)
			}
		})
	}
}

// Changes that happen after the first check passed but before the cleanup is
// written. stage 0 = noticed before STORE \Deleted, stage 1 = noticed after it
// (the \Deleted mark is rolled back) and before UID EXPUNGE.
func TestRecoveryRechecksOnCleanupSession(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		stage      int
		change     func(*testing.T, uncertainRun, *Message)
	}{
		{"unread_before_store", "recovery_fingerprint_mismatch", 0, func(_ *testing.T, _ uncertainRun, m *Message) { m.Unread = true }},
		{"flagged_before_expunge", "recovery_fingerprint_mismatch", 1, func(_ *testing.T, _ uncertainRun, m *Message) { m.Flagged = true }},
		{"policy_revoked_before_store", "recovery_access_revoked", 0, func(t *testing.T, u uncertainRun, _ *Message) {
			mustExec(t, u.s, "UPDATE automation_policies SET payload=json_set(payload,'$.policy.enabled',json('false'))")
		}},
		{"policy_revoked_before_expunge", "recovery_access_revoked", 1, func(t *testing.T, u uncertainRun, _ *Message) {
			mustExec(t, u.s, "UPDATE automation_policies SET payload=json_set(payload,'$.policy.enabled',json('false'))")
		}},
		{"rule_changed_before_expunge", "recovery_rules_changed", 1, func(t *testing.T, u uncertainRun, _ *Message) {
			mustExec(t, u.s, "UPDATE mail_rules SET payload=json_set(payload,'$.name','renamed')")
		}},
		{"folder_mapping_changed_before_store", "recovery_settings_changed", 0, func(t *testing.T, u uncertainRun, _ *Message) {
			mustExec(t, u.s, "UPDATE folder_settings SET version=version+1")
		}},
		{"writes_disabled_before_expunge", "recovery_writes_disabled", 1, func(t *testing.T, u uncertainRun, _ *Message) {
			mustExec(t, u.s, "UPDATE folder_settings SET writes_enabled=0")
		}},
		{"stopped_before_expunge", "recovery_operation_cancelled", 1, func(t *testing.T, u uncertainRun, _ *Message) {
			mustExec(t, u.s, "UPDATE operations SET cancelled=1,cancel_source='user'")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := startUncertainMove(t, tc.name)
			u.s.reconcileTaskRun(u.run.ID)
			u.remote.finishFresh = func(check int, m Message) Message {
				if check == tc.stage {
					tc.change(t, u, &m)
				}
				return m
			}
			// A revoked task identity also fails the final read-back; the item
			// reason is recorded before that, so inspect it as administrator.
			_, _ = u.s.Reconcile(context.Background(), u.access, u.operation)
			op, err := u.s.Operation(u.admin, u.operation)
			if err != nil {
				t.Fatal(err)
			}
			if op.State != "needs_verification" || op.Items[0].RecoveryCode != tc.code {
				t.Fatalf("want %s, got %+v", tc.code, op.Items[0])
			}
			wantStores, wantUndos := 0, 0
			if tc.stage == 1 {
				wantStores, wantUndos = 1, 1
			}
			if u.remote.finishStores != wantStores || u.remote.finishUndos != wantUndos || u.remote.finishExpunges != 0 || u.remote.moves != 1 {
				t.Fatal("cleanup not stopped before EXPUNGE", u.remote.finishStores, u.remote.finishUndos, u.remote.finishExpunges, u.remote.moves)
			}
		})
	}
}

// An administrator may trigger verification of an automated plan; it is still
// authorized as the task that created the plan.
func TestManualVerificationUsesTaskIdentity(t *testing.T) {
	u := startUncertainMove(t, "admin")
	u.s.reconcileTaskRun(u.run.ID)
	op, err := u.s.Reconcile(context.Background(), u.admin, u.operation)
	if err != nil || op.State != "completed" || u.remote.finishExpunges != 1 || u.remote.moves != 1 {
		t.Fatal(op, err, u.remote.finishExpunges)
	}
}

func mustExec(t *testing.T, s *Service, q string, args ...any) {
	t.Helper()
	if _, err := s.Store.DB.Exec(q, args...); err != nil {
		t.Fatal(q, err)
	}
}

func TestRecoveryWithoutMappingNeedsManualCheck(t *testing.T) {
	s, remote, admin, _, task, message := taskFixture(t, "execute", "manual", "rules")
	remote.uncertain = true
	remote.omitMapping = true
	remote.inspectEvidence = MoveEvidence{SourcePresent: true, DestinationPresent: true, Destination: MoveResult{Folder: "Archive", UIDValidity: 7, UID: 55}}
	run, err := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "nomap"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.DB.Exec("UPDATE task_runs SET state='running',lease_owner=?,lease_until=? WHERE id=?", s.workerID, time.Now().Unix()+45, run.ID); err != nil {
		t.Fatal(err)
	}
	access := Access{Owner: run.Owner, Version: run.AdminVersion}
	batch, err := s.ClaimAI(access, AIClaimInput{RunID: run.ID, AccountID: message.AccountID})
	if err != nil {
		t.Fatal(err)
	}
	if batch, err = s.SubmitAI(context.Background(), access, AISubmitInput{BatchID: batch.ID, IdempotencyKey: "nomap", Decisions: decisionFor(batch)}); err != nil {
		t.Fatal(err)
	}
	op, err := s.ExecuteAI(access, batch.ID, batch.PlanIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	s.runOperation(context.Background(), op.ID)
	s.reconcileTaskRun(run.ID)
	if op, err = s.Reconcile(context.Background(), access, op.ID); err != nil {
		t.Fatal(err)
	}
	if op.State != "needs_verification" || op.Items[0].RecoveryCode != "recovery_no_trusted_mapping" || remote.finishCallCount != 0 || remote.moves != 1 {
		t.Fatalf("metadata-only evidence must not settle a move: %+v finish=%d moves=%d", op.Items[0], remote.finishCallCount, remote.moves)
	}
}

func TestBackgroundRecoveryRecordsRevokedTaskAccess(t *testing.T) {
	for _, change := range []string{"task_saved", "policy_disabled"} {
		t.Run(change, func(t *testing.T) { backgroundRevoked(t, change) })
	}
}

func backgroundRevoked(t *testing.T, change string) {
	u := startUncertainMove(t, change)
	u.s.reconcileTaskRun(u.run.ID)
	var err error
	if change == "task_saved" {
		task, err := u.s.loadTask(u.run.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = u.s.SaveTask(u.admin, task); err != nil { // rotates the task identity
			t.Fatal(err)
		}
	} else {
		p, err := u.s.AutomationPolicy(u.admin, u.batch.AccountID)
		if err != nil {
			t.Fatal(err)
		}
		p.Enabled = false
		if _, err = u.s.SaveAutomationPolicy(u.admin, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = u.s.Store.DB.Exec("UPDATE operations SET updated_at=0 WHERE id=?", u.operation); err != nil {
		t.Fatal(err)
	}
	if !u.s.reconcileNextOperation(context.Background(), time.Now(), time.Now().Add(-24*time.Hour), time.Now().Add(time.Second), false) {
		t.Fatal("operation not picked up")
	}
	op, err := u.s.Operation(u.admin, u.operation)
	if err != nil || op.State != "needs_verification" || op.Items[0].RecoveryCode != "recovery_access_revoked" {
		t.Fatalf("%+v %v", op, err)
	}
	if u.remote.finishCallCount != 0 || u.remote.moves != 1 {
		t.Fatal("revoked task touched the mailbox")
	}
}

func TestSystemCancelDoesNotBlockRecoveryButUserStopDoes(t *testing.T) {
	u := startUncertainMove(t, "cancel-source")
	if _, err := u.s.Store.DB.Exec("UPDATE operations SET cancelled=1,cancel_source='system:task_timeout' WHERE id=?", u.operation); err != nil {
		t.Fatal(err)
	}
	plan, err := u.s.GetPlan(u.access, u.batch.PlanIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if stop, err := u.s.recoveryCancelled(u.operation, plan); err != nil || stop {
		t.Fatal("timeout cancellation blocked recovery", stop, err)
	}
	for _, source := range []string{"user", ""} {
		if _, err = u.s.Store.DB.Exec("UPDATE operations SET cancel_source=? WHERE id=?", source, u.operation); err != nil {
			t.Fatal(err)
		}
		if stop, err := u.s.recoveryCancelled(u.operation, plan); err != nil || !stop {
			t.Fatal("explicit stop ignored", source, stop, err)
		}
	}
	manual := plan
	manual.Automation = nil
	if _, err = u.s.Store.DB.Exec("UPDATE operations SET cancel_source='system:task_timeout' WHERE id=?", u.operation); err != nil {
		t.Fatal(err)
	}
	if stop, _ := u.s.recoveryCancelled(u.operation, manual); !stop {
		t.Fatal("manual operations keep the strict cancellation rule")
	}
}
