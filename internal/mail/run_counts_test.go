package mail

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// slowRemote holds every move until released, like a slow IMAP round trip.
type slowRemote struct {
	*automationRemote
	entered chan struct{}
	release chan struct{}
}

func (r *slowRemote) Move(ctx context.Context, a Account, pw string, m Message, dest string, record func(MoveResult) error) (MoveResult, error) {
	r.entered <- struct{}{}
	<-r.release
	return r.automationRemote.Move(ctx, a, pw, m, dest, record)
}

func TestRunCountsSeparateInFlightFromUnknown(t *testing.T) {
	s, remote, admin, _, task, message := taskFixture(t, "execute", "manual", "rules")
	// Four more messages matching the same rule: a batch of five.
	for i := 2; i <= 5; i++ {
		mustExec(t, s, "INSERT INTO messages("+messageColumns+") SELECT ?,account_id,folder,uidvalidity,?,subject,sender,sent_at-?,unread,size,received_at,flagged,flags_known,changed_at,metadata_version,recipients,cc,message_id,in_reply_to,thread_refs,list_id,has_attachment FROM messages WHERE ref=?", fmt.Sprintf("extra-%d", i), i, i, message.Ref)
		mustExec(t, s, "INSERT OR IGNORE INTO mail_entries(id,account_id,detached_at) VALUES(?,?,0)", fmt.Sprintf("extra-%d", i), message.AccountID)
	}
	slow := &slowRemote{automationRemote: remote, entered: make(chan struct{}, 1), release: make(chan struct{})}
	s.Remote = slow
	run, err := s.RunTask(admin, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "slow"}, "manual")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, "UPDATE task_runs SET state='running',lease_owner=?,lease_until=? WHERE id=?", s.workerID, time.Now().Unix()+600, run.ID)
	access := Access{Owner: run.Owner, Version: run.AdminVersion}
	batch, err := s.ClaimAI(access, AIClaimInput{RunID: run.ID, AccountID: message.AccountID})
	if err != nil || len(batch.Items) != 5 {
		t.Fatal(len(batch.Items), err)
	}
	if batch, err = s.SubmitAI(context.Background(), access, AISubmitInput{BatchID: batch.ID, IdempotencyKey: "slow", Decisions: decisionFor(batch)}); err != nil {
		t.Fatal(err)
	}
	ops := []string{}
	for _, id := range batch.PlanIDs {
		op, err := s.ExecuteAI(access, batch.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op.ID)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, id := range ops {
			s.runOperation(context.Background(), id)
		}
	}()
	<-slow.entered // the first write is now in flight
	s.reconcileTaskRun(run.ID)
	r, _ := s.loadTaskRun(run.ID)
	if r.Running != 1 || r.Queued != 4 || r.Unknown != 0 || r.Finalized || r.State != "running" {
		t.Fatalf("in-flight write misreported: running=%d queued=%d unknown=%d finalized=%v state=%s", r.Running, r.Queued, r.Unknown, r.Finalized, r.State)
	}
	if r.ProgressAt == 0 {
		t.Fatal("progress time missing")
	}
	// Let the rest finish, the last write being uncertain.
	for i := 0; i < 4; i++ {
		slow.release <- struct{}{}
		<-slow.entered
	}
	remote.uncertain = true
	slow.release <- struct{}{}
	<-done
	s.reconcileTaskRun(run.ID)
	r, _ = s.loadTaskRun(run.ID)
	if r.Running != 0 || r.Queued != 0 || r.Unknown != 1 || r.Completed != 4 || r.State != "needs_verification" {
		t.Fatalf("final counts: %+v", r)
	}
	if r.Completed+r.Failed+r.Unknown+r.Skipped+r.Kept != r.Claimed || r.Claimed != 5 {
		t.Fatalf("final counts do not add up to the batch: %+v", r)
	}
}
