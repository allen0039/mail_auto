package mail

import (
	"context"
	"testing"
)

type uncertainFlagRemote struct {
	*organizerRemote
	stores    int
	applied   bool
	omitFlags bool
	fresh     Message
}

func (remote *uncertainFlagRemote) SetFlags(_ context.Context, _ Account, _ string, message Message, change FlagChange, _ func(Message) error) (Message, error) {
	remote.stores++
	remote.fresh = message
	if remote.applied {
		remote.fresh = change.Apply(message)
	}
	remote.fresh.FlagsKnown = !remote.omitFlags
	return Message{}, ErrUncertain
}
func (remote *uncertainFlagRemote) RuleState(context.Context, Account, string, Message) (Message, error) {
	return remote.fresh, nil
}

func TestUnknownFlagWriteIsOnlyReadBack(t *testing.T) {
	for _, scenario := range []struct {
		name               string
		applied, omitFlags bool
	}{{"applied", true, false}, {"unconfirmed", false, false}, {"unknown_flags", true, true}} {
		t.Run(scenario.name, func(t *testing.T) {
			service, base, access, message := organizerFixture(t)
			remote := &uncertainFlagRemote{organizerRemote: base, applied: scenario.applied, omitFlags: scenario.omitFlags}
			service.Remote = remote
			operation := executeFixture(t, service, access, PlanInput{Refs: []string{message.Ref}, Action: "flag"})
			service.runOperation(context.Background(), operation.ID)
			operation, err := service.Operation(access, operation.ID)
			if err != nil || operation.State != "needs_verification" || remote.stores != 1 {
				t.Fatal("unknown flag was not retained", operation.State, err)
			}
			operation, err = service.Reconcile(context.Background(), access, operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if remote.stores != 1 {
				t.Fatal("verification replayed STORE")
			}
			if scenario.applied && !scenario.omitFlags && operation.State != "completed" {
				t.Fatal("confirmed flags not resolved", operation)
			}
			if (!scenario.applied || scenario.omitFlags) && operation.State != "needs_verification" {
				t.Fatal("unconfirmed flags reported success")
			}
		})
	}
}
