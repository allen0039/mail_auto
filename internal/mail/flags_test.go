package mail

import (
	"context"
	"errors"
	"testing"
)

// flagRemote records STOREs and applies them like a server would.
type flagRemote struct {
	*automationRemote
	stores   []FlagChange
	failNext bool
}

func (r *flagRemote) SetFlags(_ context.Context, _ Account, _ string, m Message, change FlagChange, validate func(Message) error) (Message, error) {
	if validate != nil {
		fresh := m
		fresh.Flagged = r.flagged
		if err := validate(fresh); err != nil {
			return Message{}, err
		}
	}
	if r.failNext {
		r.failNext = false
		return Message{}, errors.New("store failed")
	}
	r.stores = append(r.stores, change)
	out := change.Apply(m)
	out.FlagsKnown = true
	return out, nil
}

func TestFlagChangeCombination(t *testing.T) {
	if _, ok := flagChangeFor("mark_read", "mark_unread"); ok {
		t.Fatal("contradiction accepted")
	}
	if _, ok := flagChangeFor("flag", "flag"); ok {
		t.Fatal("duplicate accepted")
	}
	c, ok := flagChangeFor("mark_read", "flag")
	if !ok || c.Seen == nil || !*c.Seen || c.Flagged == nil || !*c.Flagged {
		t.Fatal(c, ok)
	}
	if c.Satisfied(Message{Unread: false, Flagged: true}) {
		t.Fatal("unknown flags treated as satisfied")
	}
	if !c.Satisfied(Message{Unread: false, Flagged: true, FlagsKnown: true}) || c.Satisfied(Message{Unread: true, Flagged: true, FlagsKnown: true}) {
		t.Fatal("satisfied mismatch")
	}
	for _, tc := range []struct {
		action string
		also   []string
		ok     bool
	}{
		{"move", []string{"mark_read"}, true},
		{"trash", []string{"mark_read", "unflag"}, true},
		{"mark_read", []string{"flag"}, true},
		{"mark_read", []string{"mark_unread"}, false},
		{"keep", []string{"mark_read"}, false},
		{"ai", []string{"flag"}, false},
		{"move", []string{"archive"}, false},
		{"move", []string{"mark_read", "flag", "unflag"}, false},
	} {
		if validAlso(tc.action, tc.also) != tc.ok {
			t.Fatal(tc)
		}
	}
}

func TestManualFlagPlanUpdatesIndexSkipsAndUndoes(t *testing.T) {
	s, r, admin, m := organizerFixture(t)
	remote := &flagRemote{automationRemote: &automationRemote{organizerRemote: r}}
	s.Remote = remote
	if _, e := s.Store.DB.Exec("UPDATE messages SET flags_known=1,unread=1,flagged=0 WHERE ref=?", m.Ref); e != nil {
		t.Fatal(e)
	}
	o := executeFixture(t, s, admin, PlanInput{Refs: []string{m.Ref}, Action: "mark_read", Also: []string{"flag"}})
	s.runOperation(context.Background(), o.ID)
	o, e := s.Operation(admin, o.ID)
	if e != nil || o.State != "completed" || len(remote.stores) != 1 || r.moves != 0 {
		t.Fatal(o, e, remote.stores)
	}
	current, e := s.Message(m.Ref)
	if e != nil || current.Unread || !current.Flagged || current.Folder != m.Folder {
		t.Fatalf("%+v %v", current, e)
	}
	// Already satisfied: frozen as skipped, no STORE.
	p, e := s.NewPlan(context.Background(), admin, PlanInput{Refs: []string{m.Ref}, Action: "mark_read"})
	if e != nil || p.Items[0].Skip != "already_applied" {
		t.Fatal(p, e)
	}
	undo, e := s.UndoPlan(context.Background(), admin, o.ID)
	if e != nil || len(undo) != 1 || undo[0].Action != "mark_unread" {
		t.Fatal(undo, e)
	}
	// Writes disabled blocks flag plans like moves.
	if e = s.SaveFolderSettings(context.Background(), FolderSettings{AccountID: m.AccountID, Archive: "Archive", Trash: "Trash", WritesEnabled: false}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.NewPlan(context.Background(), admin, PlanInput{Refs: []string{m.Ref}, Action: "mark_unread"}); !errors.Is(e, ErrCapability) {
		t.Fatal("flag plan with writes disabled", e)
	}
}

func TestFlagPermissionIsRequiredForTokens(t *testing.T) {
	s, r, _, m := organizerFixture(t)
	s.Remote = &flagRemote{automationRemote: &automationRemote{organizerRemote: r}}
	_, secret, e := s.CreateToken(TokenInput{Name: "mover", Accounts: []string{m.AccountID}, Actions: []string{"read", "move"}})
	if e != nil {
		t.Fatal(e)
	}
	mover, e := s.AuthenticateToken(secret)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.NewPlan(context.Background(), mover, PlanInput{Refs: []string{m.Ref}, Action: "mark_read"}); !errors.Is(e, ErrDenied) {
		t.Fatal("flag without permission", e)
	}
	if _, e = s.NewPlan(context.Background(), mover, PlanInput{Refs: []string{m.Ref}, Action: "move", Destination: "Archive", Also: []string{"mark_read"}}); !errors.Is(e, ErrDenied) {
		t.Fatal("also without flag permission", e)
	}
	_, secret, e = s.CreateToken(TokenInput{Name: "flagger", Accounts: []string{m.AccountID}, Actions: []string{"read", "flag"}})
	if e != nil {
		t.Fatal(e)
	}
	flagger, e := s.AuthenticateToken(secret)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.NewPlan(context.Background(), flagger, PlanInput{Refs: []string{m.Ref}, Action: "flag"}); e != nil {
		t.Fatal(e)
	}
}

func TestAutomationRuleMovesThenAppliesAlsoFlags(t *testing.T) {
	s, remote, admin, client, rule, p, m := automationFixture(t, "execute")
	fr := &flagRemote{automationRemote: remote}
	s.Remote = fr
	if _, e := s.Store.DB.Exec("UPDATE messages SET unread=1,metadata_version=2 WHERE ref=?", m.Ref); e != nil {
		t.Fatal(e)
	}
	p.ProtectUnread = false
	p.Actions = []string{"move", "mark_read"}
	var e error
	if p, e = s.SaveAutomationPolicy(admin, p); e != nil {
		t.Fatal(e)
	}
	rule.Also = []string{"mark_read"}
	if rule, e = s.SaveRule(admin, rule); e != nil {
		t.Fatal(e)
	}
	batch, e := s.ClaimAI(client, AIClaimInput{AccountID: m.AccountID, RunKey: "also"})
	if e != nil || len(batch.Items) != 1 || len(batch.Items[0].Also) != 1 {
		t.Fatal(batch, e)
	}
	batch, e = s.SubmitAI(context.Background(), client, AISubmitInput{BatchID: batch.ID, IdempotencyKey: "also", Decisions: decisionFor(batch)})
	if e != nil {
		t.Fatal(e)
	}
	op, e := s.ExecuteAI(client, batch.ID, batch.PlanIDs[0])
	if e != nil {
		t.Fatal(e)
	}
	s.runOperation(context.Background(), op.ID)
	current, e := s.Message(m.Ref)
	if e != nil || current.Folder != "Archive" || current.Unread {
		t.Fatalf("%+v %v", current, e)
	}
	if len(fr.stores) != 1 || fr.stores[0].Seen == nil || !*fr.stores[0].Seen {
		t.Fatal(fr.stores)
	}
	op, _ = s.Operation(client, op.ID)
	if op.State != "completed" || op.Items[0].Error != "" {
		t.Fatal(op)
	}
}

func TestAutomationAlsoFailureKeepsMoveCompletedWithWarning(t *testing.T) {
	s, remote, admin, client, rule, p, m := automationFixture(t, "execute")
	fr := &flagRemote{automationRemote: remote, failNext: true}
	s.Remote = fr
	p.Actions = []string{"move", "flag"}
	var e error
	if p, e = s.SaveAutomationPolicy(admin, p); e != nil {
		t.Fatal(e)
	}
	rule.Also = []string{"flag"}
	if _, e = s.SaveRule(admin, rule); e != nil {
		t.Fatal(e)
	}
	batch, e := s.ClaimAI(client, AIClaimInput{AccountID: m.AccountID, RunKey: "warn"})
	if e != nil || len(batch.Items) != 1 {
		t.Fatal(batch, e)
	}
	if batch, e = s.SubmitAI(context.Background(), client, AISubmitInput{BatchID: batch.ID, IdempotencyKey: "warn", Decisions: decisionFor(batch)}); e != nil {
		t.Fatal(e)
	}
	op, e := s.ExecuteAI(client, batch.ID, batch.PlanIDs[0])
	if e != nil {
		t.Fatal(e)
	}
	s.runOperation(context.Background(), op.ID)
	op, _ = s.Operation(client, op.ID)
	if op.State != "completed" || op.Items[0].Error != "flags_not_applied" {
		t.Fatal(op)
	}
	if current, _ := s.Message(m.Ref); current.Folder != "Archive" {
		t.Fatal("move not published", current)
	}
}

func TestAutomationFlagRuleSkipsSatisfiedAndRequiresPolicy(t *testing.T) {
	s, remote, admin, client, rule, p, m := automationFixture(t, "execute")
	fr := &flagRemote{automationRemote: remote}
	s.Remote = fr
	rule.Action, rule.Destination = "flag", ""
	var e error
	if rule, e = s.SaveRule(admin, rule); e != nil {
		t.Fatal(e)
	}
	p.ProtectFlagged = false
	if p, e = s.SaveAutomationPolicy(admin, p); e != nil {
		t.Fatal(e)
	}
	// Policy only allows move: a flag rule is matched but the plan is denied.
	batch, e := s.ClaimAI(client, AIClaimInput{AccountID: m.AccountID, RunKey: "deny"})
	if e != nil || len(batch.Items) != 1 || batch.Items[0].Action != "flag" {
		t.Fatal(batch, e)
	}
	if _, e = s.SubmitAI(context.Background(), client, AISubmitInput{BatchID: batch.ID, IdempotencyKey: "deny", Decisions: []AIDecision{{Ref: m.Ref, Signature: batch.Items[0].Signature, Action: "flag", Reason: "rule"}}}); !errors.Is(e, ErrDenied) {
		t.Fatal("flag outside policy", e)
	}
	if e = s.CancelAIBatch(client, batch.ID); e != nil {
		t.Fatal(e)
	}
	// Already flagged: the rule is satisfied and nothing is claimed.
	if _, e = s.Store.DB.Exec("UPDATE messages SET flagged=1 WHERE ref=?", m.Ref); e != nil {
		t.Fatal(e)
	}
	current, _ := s.Message(m.Ref)
	rules, _ := s.Rules(admin, m.AccountID)
	if got := evaluateRules(current, rules, p, current.ChangedAt+3600); got.Status != "satisfied" {
		t.Fatal(got)
	}
	batch, e = s.ClaimAI(client, AIClaimInput{AccountID: m.AccountID, RunKey: "satisfied"})
	if e != nil || len(batch.Items) != 0 || batch.State != "empty" {
		t.Fatal(batch, e)
	}
}
