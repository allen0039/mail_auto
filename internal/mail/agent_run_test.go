package mail

import (
	"context"
	"errors"
	"testing"
)

// An agent-owned manual run executes through the normal automation path and
// stays fenced by the grant: revocation fails the next run's writes.
func TestAgentOwnedRunExecutesAndIsFencedByGrant(t *testing.T) {
	s, remote, admin, _, task, m := taskFixture(t, "execute", "manual", "rules")
	a, _ := bindAgent(t, s, admin, AgentGrantInput{Enabled: true, Accounts: []string{m.AccountID}, Actions: []string{"read", "task_manage", "automation"}})
	run, err := s.RunTask(a, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "agent:1"}, "manual")
	if err != nil || run.Owner != a.Owner {
		t.Fatal(run, err)
	}
	if err = s.StartOperations(); err != nil {
		t.Fatal(err)
	}
	s.runFixedTask(context.Background(), run.ID)
	s.Stop()
	run, err = s.TaskRun(admin, run.ID)
	if err != nil || run.State != "completed" || run.Completed != 1 || remote.moves != 1 {
		t.Fatal(run, err, remote.moves)
	}
	again, err := s.RunTask(a, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "agent:1"}, "manual")
	if err != nil || again.ID != run.ID {
		t.Fatal("same run key created a second run", again, err)
	}
	if err = s.RevokeAgentGrant(admin); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunTask(a, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "agent:2"}, "manual"); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked agent started a run", err)
	}
}
