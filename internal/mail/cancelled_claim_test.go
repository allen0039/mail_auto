package mail

import (
	"context"
	"testing"
	"time"
)

type cancelledClaimCase struct {
	service *Service
	remote  *automationRemote
	admin   Access
	task    Task
	message Message
	run     TaskRun
	op      Operation
}

func startRunningTask(test *testing.T, service *Service, admin Access, task Task, key string) TaskRun {
	test.Helper()
	run, err := service.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: key}, "manual")
	if err != nil {
		test.Fatal(err)
	}
	mustExec(test, service, "UPDATE task_runs SET state='running',lease_owner=?,lease_until=? WHERE id=?", service.workerID, time.Now().Unix()+120, run.ID)
	return run
}

func expireTaskRun(test *testing.T, service *Service, run TaskRun, cause string) {
	test.Helper()
	if cause == "task_timeout" {
		current, err := service.loadTaskRun(run.ID)
		if err != nil {
			test.Fatal(err)
		}
		current.Expires = time.Now().Unix() - 1
		if err = service.saveTaskRun(current); err != nil {
			test.Fatal(err)
		}
		return
	}
	mustExec(test, service, "UPDATE task_runs SET lease_until=? WHERE id=?", time.Now().Unix()-1, run.ID)
}

func claimNext(test *testing.T, service *Service, admin Access, task Task, accountID, key string) (TaskRun, AIBatch) {
	test.Helper()
	run := startRunningTask(test, service, admin, task, key)
	batch, err := service.ClaimAI(Access{Owner: run.Owner, Version: run.AdminVersion}, AIClaimInput{RunID: run.ID, AccountID: accountID})
	if err != nil {
		test.Fatal(err)
	}
	return run, batch
}

func setupSystemCancelled(test *testing.T, cause string, beforeCancel func(*cancelledClaimCase)) *cancelledClaimCase {
	test.Helper()
	service, remote, admin, _, task, message := taskFixture(test, "execute", "manual", "rules")
	run := startRunningTask(test, service, admin, task, "first")
	access := Access{Owner: run.Owner, Version: run.AdminVersion}
	batch, err := service.ClaimAI(access, AIClaimInput{RunID: run.ID, AccountID: message.AccountID})
	if err != nil || len(batch.Items) != 1 {
		test.Fatal("first claim", batch, err)
	}
	batch, err = service.SubmitAI(context.Background(), access, AISubmitInput{BatchID: batch.ID, IdempotencyKey: "first-submit", Decisions: decisionFor(batch)})
	if err != nil || len(batch.PlanIDs) != 1 {
		test.Fatal("submit", batch, err)
	}
	op, err := service.ExecuteAI(access, batch.ID, batch.PlanIDs[0])
	if err != nil {
		test.Fatal(err)
	}
	fixture := &cancelledClaimCase{service: service, remote: remote, admin: admin, task: task, message: message, run: run, op: op}
	if beforeCancel != nil {
		beforeCancel(fixture)
	}
	expireTaskRun(test, service, run, cause)
	service.reconcileTaskRun(run.ID)
	service.runOperation(context.Background(), op.ID)
	service.reconcileTaskRun(run.ID)
	fixture.op, err = service.Operation(admin, op.ID)
	if err != nil || fixture.op.State != "cancelled" || len(fixture.op.Items) != 1 || fixture.op.Items[0].State != "cancelled" || remote.moves != 0 {
		test.Fatal("cancelled before any write", fixture.op, err, remote.moves)
	}
	return fixture
}

func TestSystemCancelledUnattemptedItemIsReplannedOnce(test *testing.T) {
	for _, cause := range []string{"task_timeout", "worker_lease_expired"} {
		test.Run(cause, func(subtest *testing.T) {
			fixture := setupSystemCancelled(subtest, cause, nil)
			service := fixture.service
			var source string
			if err := service.Store.DB.QueryRow("SELECT cancel_source FROM operations WHERE id=?", fixture.op.ID).Scan(&source); err != nil || source != "system:"+cause {
				subtest.Fatal("cancel source", source, err)
			}
			same, err := service.RunTask(fixture.admin, TaskRunInput{TaskID: fixture.task.ID, Version: fixture.task.Version, RunKey: "first"}, "manual")
			if err != nil || same.ID != fixture.run.ID {
				subtest.Fatal("run key must stay idempotent", same.ID, fixture.run.ID, err)
			}
			next, batch := claimNext(subtest, service, fixture.admin, fixture.task, fixture.message.AccountID, "second")
			if len(batch.Items) != 1 || batch.Items[0].Message.Ref != fixture.message.Ref {
				subtest.Fatal("unattempted message must be claimable again", batch)
			}
			access := Access{Owner: next.Owner, Version: next.AdminVersion}
			batch, err = service.SubmitAI(context.Background(), access, AISubmitInput{BatchID: batch.ID, IdempotencyKey: "second-submit", Decisions: decisionFor(batch)})
			if err != nil || len(batch.PlanIDs) != 1 {
				subtest.Fatal("resubmit", batch, err)
			}
			if batch.PlanIDs[0] == fixture.op.PlanID {
				subtest.Fatal("old plan must not be replayed")
			}
			op, err := service.ExecuteAI(access, batch.ID, batch.PlanIDs[0])
			if err != nil {
				subtest.Fatal(err)
			}
			service.runOperation(context.Background(), op.ID)
			service.runOperation(context.Background(), fixture.op.ID)
			op, err = service.Operation(fixture.admin, op.ID)
			if err != nil || op.State != "completed" || fixture.remote.moves != 1 {
				subtest.Fatal("new plan must move exactly once", op, err, fixture.remote.moves)
			}
			old, err := service.Operation(fixture.admin, fixture.op.ID)
			if err != nil || old.State != "cancelled" || old.Items[0].State != "cancelled" {
				subtest.Fatal("old operation must stay cancelled", old, err)
			}
			mustExec(subtest, service, "UPDATE task_runs SET state='completed' WHERE id=?", next.ID)
			_, batch = claimNext(subtest, service, fixture.admin, fixture.task, fixture.message.AccountID, "third")
			if len(batch.Items) != 0 || fixture.remote.moves != 1 {
				subtest.Fatal("completed message must not be claimed again", batch, fixture.remote.moves)
			}
		})
	}
}

func TestSystemCancelledClaimRetryNegativeCases(test *testing.T) {
	cases := map[string]string{
		"empty_source":           "UPDATE operations SET cancel_source='' WHERE id=?",
		"other_system_source":    "UPDATE operations SET cancel_source='system:task_failed' WHERE id=?",
		"system_prefix_only":     "UPDATE operations SET cancel_source='system:' WHERE id=?",
		"destination_mapping":    "UPDATE operation_items SET destination_validity=7,destination_uid=500 WHERE operation_id=?",
		"destination_uid_only":   "UPDATE operation_items SET destination_uid=500 WHERE operation_id=?",
		"destination_validity":   "UPDATE operation_items SET destination_validity=7 WHERE operation_id=?",
		"item_completed":         "UPDATE operation_items SET state='completed' WHERE operation_id=?",
		"item_failed":            "UPDATE operation_items SET state='failed' WHERE operation_id=?",
		"item_skipped":           "UPDATE operation_items SET state='skipped' WHERE operation_id=?",
		"item_running":           "UPDATE operation_items SET state='running' WHERE operation_id=?",
		"item_needs_verify":      "UPDATE operation_items SET state='needs_verification' WHERE operation_id=?",
		"operation_queued":       "UPDATE operations SET state='queued' WHERE id=?",
		"operation_running":      "UPDATE operations SET state='running' WHERE id=?",
		"operation_needs_verify": "UPDATE operations SET state='needs_verification' WHERE id=?",
		"not_cancelled_flag":     "UPDATE operations SET cancelled=0 WHERE id=?",
	}
	for name, stmt := range cases {
		test.Run(name, func(subtest *testing.T) {
			fixture := setupSystemCancelled(subtest, "task_timeout", nil)
			mustExec(subtest, fixture.service, stmt, fixture.op.ID)
			_, batch := claimNext(subtest, fixture.service, fixture.admin, fixture.task, fixture.message.AccountID, "second")
			if len(batch.Items) != 0 || fixture.remote.moves != 0 {
				subtest.Fatal("message must stay excluded", name, batch, fixture.remote.moves)
			}
		})
	}
}

func TestUserCancelledClaimIsNotOverriddenBySystemCancel(test *testing.T) {
	for _, cause := range []string{"task_timeout", "worker_lease_expired"} {
		test.Run(cause, func(subtest *testing.T) {
			fixture := setupSystemCancelled(subtest, cause, func(pending *cancelledClaimCase) {
				if err := pending.service.CancelOperation(pending.admin, pending.op.ID); err != nil {
					subtest.Fatal(err)
				}
			})
			var source string
			if err := fixture.service.Store.DB.QueryRow("SELECT cancel_source FROM operations WHERE id=?", fixture.op.ID).Scan(&source); err != nil || source != "user" {
				subtest.Fatal("manual cancel source overwritten", source, err)
			}
			_, batch := claimNext(subtest, fixture.service, fixture.admin, fixture.task, fixture.message.AccountID, "second")
			if len(batch.Items) != 0 || fixture.remote.moves != 0 {
				subtest.Fatal("manually stopped message must not be reclaimed", batch, fixture.remote.moves)
			}
		})
	}
}

func TestSystemCancelledRetryIsJudgedPerItem(test *testing.T) {
	service, remote, admin, _, task, message := taskFixture(test, "execute", "manual", "rules")
	remote.snapshot.Messages = append(remote.snapshot.Messages, Message{UID: 2, Subject: "fixture extra", Sender: "a@example.com", SentAt: 124})
	if _, err := service.Sync(context.Background(), message.AccountID); err != nil {
		test.Fatal(err)
	}
	now := time.Now().Unix()
	mustExec(test, service, "UPDATE messages SET received_at=?,flags_known=1,unread=0,changed_at=?,metadata_version=1 WHERE account_id=? AND folder='INBOX'", now-60*86400, now-600, message.AccountID)
	run := startRunningTask(test, service, admin, task, "first")
	access := Access{Owner: run.Owner, Version: run.AdminVersion}
	batch, err := service.ClaimAI(access, AIClaimInput{RunID: run.ID, AccountID: message.AccountID})
	if err != nil || len(batch.Items) != 2 {
		test.Fatal("first claim", batch, err)
	}
	batch, err = service.SubmitAI(context.Background(), access, AISubmitInput{BatchID: batch.ID, IdempotencyKey: "first-submit", Decisions: decisionFor(batch)})
	if err != nil || len(batch.PlanIDs) != 2 {
		test.Fatal("submit", batch, err)
	}
	ops := []Operation{}
	for _, planID := range batch.PlanIDs {
		op, err := service.ExecuteAI(access, batch.ID, planID)
		if err != nil {
			test.Fatal(err)
		}
		ops = append(ops, op)
	}
	expireTaskRun(test, service, run, "worker_lease_expired")
	service.reconcileTaskRun(run.ID)
	for _, op := range ops {
		service.runOperation(context.Background(), op.ID)
	}
	service.reconcileTaskRun(run.ID)
	if remote.moves != 0 {
		test.Fatal("no write expected before cancellation", remote.moves)
	}
	mustExec(test, service, "UPDATE operation_items SET state='completed',destination='Archive',destination_validity=7,destination_uid=800 WHERE operation_id=?", ops[0].ID)
	done, err := service.Operation(admin, ops[0].ID)
	if err != nil || done.Items[0].State != "completed" {
		test.Fatal("first item fixture", done, err)
	}
	stopped, err := service.Operation(admin, ops[1].ID)
	if err != nil || stopped.State != "cancelled" || stopped.Items[0].State != "cancelled" {
		test.Fatal("second item must be cancelled before writing", stopped, err)
	}
	var source string
	if err = service.Store.DB.QueryRow("SELECT cancel_source FROM operations WHERE id=?", ops[0].ID).Scan(&source); err != nil || source != "system:worker_lease_expired" {
		test.Fatal("sibling cancel source", source, err)
	}
	_, next := claimNext(test, service, admin, task, message.AccountID, "second")
	if len(next.Items) != 1 || next.Items[0].Message.Ref != stopped.Items[0].Ref {
		test.Fatal("only the unattempted item may be reclaimed", next, done.Items[0].Ref, stopped.Items[0].Ref)
	}
}

func TestSystemCancelledRetryRequiresEveryItemForRef(test *testing.T) {
	fixture := setupSystemCancelled(test, "task_timeout", nil)
	mustExec(test, fixture.service, "INSERT INTO operation_items(operation_id,position,ref,state,destination,destination_validity,destination_uid) VALUES(?,1,?,'completed','Archive',7,700)", fixture.op.ID, fixture.message.Ref)
	_, batch := claimNext(test, fixture.service, fixture.admin, fixture.task, fixture.message.AccountID, "second")
	if len(batch.Items) != 0 || fixture.remote.moves != 0 {
		test.Fatal("ref with an attempted item must stay excluded", batch, fixture.remote.moves)
	}
}
