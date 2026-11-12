package mail

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

type automationRemote struct {
	*organizerRemote
	flagged bool
}

func (r *automationRemote) RuleState(_ context.Context, _ Account, _ string, m Message) (Message, error) {
	m.Flagged = r.flagged
	return m, nil
}
func automationFixture(t *testing.T, mode string) (*Service, *automationRemote, Access, Access, Rule, AutomationPolicy, Message) {
	t.Helper()
	s, r, admin, m := organizerFixture(t)
	remote := &automationRemote{organizerRemote: r}
	s.Remote = remote
	now := time.Now().Unix()
	_, e := s.Store.DB.Exec("UPDATE messages SET received_at=?,flags_known=1,unread=0,changed_at=?,metadata_version=1 WHERE ref=?", now-60*86400, now-600, m.Ref)
	if e != nil {
		t.Fatal(e)
	}
	m, e = s.Message(m.Ref)
	if e != nil {
		t.Fatal(e)
	}
	rule, e := s.SaveRule(admin, Rule{AccountID: m.AccountID, Name: "订阅到期", Priority: 10, Enabled: true, SourceFolders: []string{"INBOX"}, Match: RuleGroup{Mode: "all", Conditions: []RuleCondition{{Field: "received_days", Operator: "gte", Value: "30"}}}, Action: "move", Destination: "Archive"})
	if e != nil {
		t.Fatal(e)
	}
	p := defaultPolicy(m.AccountID)
	p.Enabled = true
	p.Mode = mode
	p.IncludeHistory = true
	p.Since = 0
	p.StableSeconds = 0
	p.SourceFolders = []string{"INBOX"}
	p.Destinations = []string{"Archive"}
	p.Actions = []string{"move"}
	p, e = s.SaveAutomationPolicy(admin, p)
	if e != nil {
		t.Fatal(e)
	}
	_, secret, e := s.CreateToken(TokenInput{Name: "scheduled fixture", Accounts: []string{m.AccountID}, Actions: []string{"read", "automation"}})
	if e != nil {
		t.Fatal(e)
	}
	client, e := s.AuthenticateToken(secret)
	if e != nil {
		t.Fatal(e)
	}
	return s, remote, admin, client, rule, p, m
}
func decisionFor(b AIBatch) []AIDecision {
	out := []AIDecision{}
	for _, item := range b.Items {
		out = append(out, AIDecision{Ref: item.Message.Ref, Signature: item.Signature, Action: "move", Destination: "Archive", Reason: "满足已配置的保留天数"})
	}
	return out
}

func TestAutomationRecoveryRechecksPolicyAndProtectedFlags(t *testing.T) {
	for _, mode := range []string{"allowed", "policy_revoked", "flagged"} {
		t.Run(mode, func(t *testing.T) {
			s, remote, admin, client, _, policy, message := automationFixture(t, "execute")
			remote.uncertain = true
			remote.verifyErr = ErrUncertain
			batch, err := s.ClaimAI(client, AIClaimInput{AccountID: message.AccountID, RunKey: "recovery"})
			if err != nil {
				t.Fatal(err)
			}
			batch, err = s.SubmitAI(context.Background(), client, AISubmitInput{BatchID: batch.ID, IdempotencyKey: "recovery", Decisions: decisionFor(batch)})
			if err != nil || len(batch.PlanIDs) != 1 {
				t.Fatal(batch, err)
			}
			operation, err := s.ExecuteAI(client, batch.ID, batch.PlanIDs[0])
			if err != nil {
				t.Fatal(err)
			}
			s.runOperation(context.Background(), operation.ID)
			if mode == "policy_revoked" {
				policy.Enabled = false
				if _, err = s.SaveAutomationPolicy(admin, policy); err != nil {
					t.Fatal(err)
				}
			}
			remote.flagged = mode == "flagged"
			operation, err = s.Reconcile(context.Background(), client, operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "allowed" {
				if operation.State != "completed" || remote.finishCallCount != 1 {
					t.Fatal(operation, remote.finishCallCount)
				}
			} else if operation.State != "needs_verification" || remote.finishCallCount != 0 {
				t.Fatal("recovery bypassed automation policy", operation, remote.finishCallCount)
			}
		})
	}
}

func TestAutomationClaimSubmitExecuteAndDedupe(t *testing.T) {
	s, r, _, a, _, _, m := automationFixture(t, "execute")
	ctx := context.Background()
	b, e := s.ClaimAI(a, AIClaimInput{AccountID: m.AccountID, RunKey: "occurrence-1"})
	if e != nil || len(b.Items) != 1 {
		t.Fatal(b, e)
	}
	retry, e := s.ClaimAI(a, AIClaimInput{AccountID: m.AccountID, RunKey: "occurrence-1"})
	if e != nil || retry.ID != b.ID {
		t.Fatal(retry, e)
	}
	_, secret, e := s.CreateToken(TokenInput{Name: "second client", Accounts: []string{m.AccountID}, Actions: []string{"read", "automation"}})
	if e != nil {
		t.Fatal(e)
	}
	second, e := s.AuthenticateToken(secret)
	if e != nil {
		t.Fatal(e)
	}
	other, e := s.ClaimAI(second, AIClaimInput{AccountID: m.AccountID, RunKey: "other"})
	if e != nil || len(other.Items) != 0 {
		t.Fatal("duplicate claim", other, e)
	}
	if _, e = s.AIGet(ctx, a, b.ID, m.Ref, true); !errors.Is(e, ErrDenied) {
		t.Fatal("body permission bypass", e)
	}
	if _, e = s.NewPlan(ctx, a, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: "Archive"}); !errors.Is(e, ErrDenied) {
		t.Fatal("raw write bypass", e)
	}
	input := AISubmitInput{BatchID: b.ID, IdempotencyKey: "submit-1", Decisions: decisionFor(b)}
	submitted, e := s.SubmitAI(ctx, a, input)
	if e != nil || len(submitted.PlanIDs) != 1 {
		t.Fatal(submitted, e)
	}
	repeat, e := s.SubmitAI(ctx, a, input)
	if e != nil || repeat.PlanIDs[0] != submitted.PlanIDs[0] {
		t.Fatal("submit dedupe", repeat, e)
	}
	changed := input
	changed.Decisions = append([]AIDecision{}, input.Decisions...)
	changed.Decisions[0].Reason = "different"
	if _, e = s.SubmitAI(ctx, a, changed); !errors.Is(e, ErrConflict) {
		t.Fatal("changed payload reused key", e)
	}
	op, e := s.ExecuteAI(a, b.ID, submitted.PlanIDs[0])
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.ExecuteAI(a, b.ID, submitted.PlanIDs[0])
	if e != nil || again.ID != op.ID {
		t.Fatal("execution dedupe", again, e)
	}
	s.runOperation(ctx, op.ID)
	op, e = s.Operation(a, op.ID)
	if e != nil || op.State != "completed" || r.moves != 1 {
		t.Fatal(op, r.moves, e)
	}
	moved, e := s.Message(m.Ref)
	if e != nil || moved.Folder != "Archive" || moved.ReceivedAt != m.ReceivedAt || !moved.FlagsKnown {
		t.Fatal("provider result / metadata", moved, e)
	}
	var count, claimed int
	if e = s.Store.DB.QueryRow("SELECT count,claimed FROM automation_daily WHERE account_id=?", m.AccountID).Scan(&count, &claimed); e != nil || count != 1 || claimed != 1 {
		t.Fatal(count, claimed, e)
	}
}
func TestAutomationObservePolicyChangeAndRevocation(t *testing.T) {
	s, r, admin, a, _, p, m := automationFixture(t, "observe")
	ctx := context.Background()
	b, e := s.ClaimAI(a, AIClaimInput{AccountID: m.AccountID, RunKey: "observe"})
	if e != nil {
		t.Fatal(e)
	}
	out, e := s.SubmitAI(ctx, a, AISubmitInput{BatchID: b.ID, IdempotencyKey: "preview", Decisions: decisionFor(b)})
	if e != nil || out.State != "observed" || len(out.PlanIDs) != 0 || r.moves != 0 {
		t.Fatal(out, r.moves, e)
	}
	p.Mode = "execute"
	p, e = s.SaveAutomationPolicy(admin, p)
	if e != nil {
		t.Fatal(e)
	}
	b, e = s.ClaimAI(a, AIClaimInput{AccountID: m.AccountID, RunKey: "execute"})
	if e != nil || len(b.Items) != 1 {
		t.Fatal(b, e)
	}
	out, e = s.SubmitAI(ctx, a, AISubmitInput{BatchID: b.ID, IdempotencyKey: "execute", Decisions: decisionFor(b)})
	if e != nil {
		t.Fatal(e)
	}
	op, e := s.ExecuteAI(a, b.ID, out.PlanIDs[0])
	if e != nil {
		t.Fatal(e)
	}
	p.Enabled = false
	if _, e = s.SaveAutomationPolicy(admin, p); e != nil {
		t.Fatal(e)
	}
	s.runOperation(ctx, op.ID)
	op, e = s.Operation(a, op.ID)
	if e != nil || op.State != "failed" || r.moves != 0 {
		t.Fatal("revocation failed", op, r.moves, e)
	}
}
func TestAutomationStaleRulesMessagesAndRemoteFlags(t *testing.T) {
	for _, change := range []string{"rule", "local", "remote", "cancel", "day"} {
		t.Run(change, func(t *testing.T) {
			s, r, admin, a, rule, _, m := automationFixture(t, "execute")
			ctx := context.Background()
			b, e := s.ClaimAI(a, AIClaimInput{AccountID: m.AccountID, RunKey: "test"})
			if e != nil {
				t.Fatal(e)
			}
			out, e := s.SubmitAI(ctx, a, AISubmitInput{BatchID: b.ID, IdempotencyKey: "test", Decisions: decisionFor(b)})
			if e != nil {
				t.Fatal(e)
			}
			op, e := s.ExecuteAI(a, b.ID, out.PlanIDs[0])
			if e != nil {
				t.Fatal(e)
			}
			switch change {
			case "rule":
				rule.Enabled = false
				if _, e = s.SaveRule(admin, rule); e != nil {
					t.Fatal(e)
				}
			case "local":
				if _, e = s.Store.DB.Exec("UPDATE messages SET unread=1 WHERE ref=?", m.Ref); e != nil {
					t.Fatal(e)
				}
			case "remote":
				r.flagged = true
			case "day":
				if _, e = s.Store.DB.Exec("UPDATE operations SET created_at=? WHERE id=?", time.Now().Unix()-86400, op.ID); e != nil {
					t.Fatal(e)
				}
			case "cancel":
				if e = s.CancelAIBatch(admin, b.ID); e != nil {
					t.Fatal(e)
				}
			}
			s.runOperation(ctx, op.ID)
			op, e = s.Operation(a, op.ID)
			if e != nil || r.moves != 0 || op.State == "completed" {
				t.Fatal("stale operation wrote", op, r.moves, e)
			}
		})
	}
}
func TestAutomationExpiryConcurrencyAndSharedBudget(t *testing.T) {
	s, _, admin, a, _, p, m := automationFixture(t, "execute")
	p.DailyLimit = 1
	p.MaxBatch = 1
	var e error
	p, e = s.SaveAutomationPolicy(admin, p)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	results := make(chan AIBatch, 2)
	errs := make(chan error, 2)
	for _, key := range []string{"one", "two"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			b, e := s.ClaimAI(a, AIClaimInput{AccountID: m.AccountID, RunKey: key})
			results <- b
			errs <- e
		}(key)
	}
	wg.Wait()
	close(results)
	close(errs)
	count := 0
	var claimed AIBatch
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	for b := range results {
		count += len(b.Items)
		if len(b.Items) > 0 {
			claimed = b
		}
	}
	if count != 1 {
		t.Fatal("overlapping lease", count)
	}
	// Expiry is represented both in the lease and frozen batch.
	if _, e = s.Store.DB.Exec("UPDATE automation_claims SET expires=0 WHERE batch_id=?", claimed.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ClaimAI(a, AIClaimInput{AccountID: m.AccountID, RunKey: "third"}); !errors.Is(e, ErrDailyLimit) {
		t.Fatal("shared daily budget", e)
	}
	expired := claimed
	expired.Expires = time.Now().Unix() - 1
	encoded, err := json.Marshal(expired)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(encoded)
	if _, e = s.Store.DB.Exec("UPDATE automation_batches SET payload=? WHERE id=?", raw, claimed.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.SubmitAI(context.Background(), a, AISubmitInput{BatchID: claimed.ID, IdempotencyKey: "late", Decisions: decisionFor(claimed)}); !errors.Is(e, ErrConflict) {
		t.Fatal("expired submission", e)
	}
}
func TestRuleGroupsUnknownDatesVersionsAndCycle(t *testing.T) {
	s, _, admin, _, rule, _, m := automationFixture(t, "observe")
	m.SentAt = time.Now().Unix()
	match, _ := groupMatch(rule.Match, m, time.Now().Unix())
	if match != 1 {
		t.Fatal("received time confused with sent time")
	}
	m.ReceivedAt = 0
	match, _ = groupMatch(rule.Match, m, time.Now().Unix())
	if match != -1 {
		t.Fatal("missing date assumed", match)
	}
	g := RuleGroup{Mode: "not", Groups: []RuleGroup{{Mode: "any", Conditions: []RuleCondition{{Field: "subject", Operator: "contains", Value: "other"}, {Field: "flagged", Operator: "equals", Value: "true"}}}}}
	m.FlagsKnown = true
	m.Flagged = false
	match, _ = groupMatch(g, m, time.Now().Unix())
	if match != 1 {
		t.Fatal("nested conditions", match)
	}
	first := rule
	rule.Name = "changed"
	rule, e := s.SaveRule(admin, rule)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.SaveRule(admin, first); !errors.Is(e, ErrConflict) {
		t.Fatal("lost update", e)
	}
	history, e := s.RuleHistory(admin, rule.ID)
	if e != nil || len(history) != 2 {
		t.Fatal(history, e)
	}
	reverse := Rule{AccountID: m.AccountID, Name: "reverse", Enabled: true, SourceFolders: []string{"Archive"}, Match: RuleGroup{Mode: "all", Conditions: []RuleCondition{}}, Action: "move", Destination: "INBOX"}
	if _, e = s.SaveRule(admin, reverse); !errors.Is(e, ErrConflict) {
		t.Fatal("folder loop accepted", e)
	}
}
