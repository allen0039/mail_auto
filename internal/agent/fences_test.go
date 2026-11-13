package agent

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"mail-auto/internal/mail"
	"mail-auto/internal/mailtools"
	"mail-auto/internal/modelprovider"
)

// AT-AUD-01: a turn that committed a draft but crashed before finishing is
// recovered by the normal Start path within a bound; the store (single
// connection) stays usable, nothing is written again and the notice names
// the committed draft.
func TestStartRecoversCommittedDraftWithoutBlocking(t *testing.T) {
	e := setup(t)
	e.model.steps = []func(modelprovider.Request) (modelprovider.Response, error){
		func(modelprovider.Request) (modelprovider.Response, error) {
			return call("d1", "save_rule_draft", map[string]any{"account_id": e.f.Account.ID, "rule": map[string]any{"name": "崩溃窗口草稿", "source_folders": []string{"INBOX"}, "match": map[string]any{"mode": "all", "conditions": []any{}}, "action": "trash"}}), nil
		},
	}
	e.say(t, "保存草稿")
	var ruleID string
	if err := e.f.Store.DB.QueryRow("SELECT id FROM mail_rules").Scan(&ruleID); err != nil {
		t.Fatal(err)
	}
	// Crash window: tool call done and committed, turn not finished.
	if _, err := e.f.Store.DB.Exec("UPDATE agent_turns SET state='running'"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.f.Store.DB.Exec("DELETE FROM telegram_reply_outbox WHERE semantic_key LIKE 'turn:%:final'"); err != nil {
		t.Fatal(err)
	}
	if e.f.Store.DB.Stats().MaxOpenConnections != 1 {
		t.Fatal("test must run with the production single connection")
	}
	rt2 := New(e.f.Store, e.f.Vault, e.f.Service)
	rt2.NewProvider = e.rt.NewProvider
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); rt2.Wait() }()
	done := make(chan struct{})
	go func() { rt2.Start(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start blocked during recovery")
	}
	pingCtx, pingCancel := context.WithTimeout(context.Background(), time.Second)
	defer pingCancel()
	if err := e.f.Store.DB.PingContext(pingCtx); err != nil {
		t.Fatal("store unusable after recovery (HTTP would not start)", err)
	}
	var rules int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM mail_rules").Scan(&rules)
	if rules != 1 {
		t.Fatal("recovery wrote again", rules)
	}
	found := false
	for _, r := range e.replies(t) {
		if strings.HasSuffix(r.key, ":interrupted") && strings.Contains(r.text, ruleID) && strings.Contains(r.text, "不会自动重新执行") {
			found = true
		}
	}
	if !found {
		t.Fatal("interruption notice does not list the committed draft")
	}
}

func proposalOf(t *testing.T, e *env) (proposalRow, mailtools.Proposal) {
	t.Helper()
	var id string
	if err := e.f.Store.DB.QueryRow("SELECT id FROM agent_proposals ORDER BY created_at DESC LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	p, err := scanProposal(e.f.Store.DB.QueryRow("SELECT "+proposalColumns+" FROM agent_proposals WHERE id=?", id))
	if err != nil {
		t.Fatal(err)
	}
	var prop mailtools.Proposal
	if err = e.rt.open("agent-proposal", id, p.Payload, &prop); err != nil {
		t.Fatal(err)
	}
	return p, prop
}

func counts(e *env) (enabledRules, tasks int) {
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM mail_rules WHERE json_extract(payload,'$.enabled')=1").Scan(&enabledRules)
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM automation_tasks").Scan(&tasks)
	return
}

func disableAgent(t *testing.T, e *env) {
	t.Helper()
	p, err := e.rt.Provider(e.f.Admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.rt.SaveProvider(e.f.Admin, ProviderInput{Version: p.Version, BaseURL: p.BaseURL, Model: p.Model, TimeoutSeconds: p.TimeoutSeconds}); err != nil {
		t.Fatal(err)
	}
}

func enableAgent(t *testing.T, e *env) {
	t.Helper()
	if _, code, err := e.rt.CheckProvider(context.Background(), e.f.Admin, "tool_loop"); err != nil || code != "ok" {
		t.Fatal(code, err)
	}
	p, _ := e.rt.Provider(e.f.Admin)
	if _, err := e.rt.SaveProvider(e.f.Admin, ProviderInput{Version: p.Version, Enabled: true, BaseURL: p.BaseURL, Model: p.Model, TimeoutSeconds: p.TimeoutSeconds}); err != nil {
		t.Fatal(err)
	}
}

// AT-AUD-03: disabling the Agent stops accepted (not yet applying) work,
// survives a restart, and re-enabling does not revive the old card.
func TestDisableBetweenAcceptAndApplyAndReenableDoesNotRevive(t *testing.T) {
	e := setup(t)
	e.draftAndPropose(t)
	accept, _ := e.deliverCard(t, 31)
	p, _ := proposalOf(t, e)
	// Accepted by CAS, apply not started yet.
	if _, err := e.f.Store.DB.Exec("UPDATE agent_proposals SET state='accepted' WHERE id=?", p.ID); err != nil {
		t.Fatal(err)
	}
	disableAgent(t, e)
	e.rt.apply(context.Background(), p.ID)
	rt2 := New(e.f.Store, e.f.Vault, e.f.Service)
	rt2.NewProvider = e.rt.NewProvider
	rt2.recover()
	if rules, tasks := counts(e); rules != 0 || tasks != 0 {
		t.Fatal("disabled Agent applied an accepted proposal", rules, tasks)
	}
	var state, code string
	_ = e.f.Store.DB.QueryRow("SELECT state,error_code FROM agent_proposals WHERE id=?", p.ID).Scan(&state, &code)
	if state != "invalidated" || code != "agent_disabled" {
		t.Fatal(state, code)
	}
	enableAgent(t, e)
	if out := e.rt.Confirm(context.Background(), e.binding, 31, accept); strings.Contains(out, "已确认") {
		t.Fatal("re-enabled Agent revived the old card", out)
	}
	if rules, tasks := counts(e); rules != 0 || tasks != 0 {
		t.Fatal("old card applied after re-enable", rules, tasks)
	}
	// The old card was frozen under a configuration version that no longer
	// exists after the disable/enable cycle.
	cur, _ := e.rt.loadProvider()
	if cur.version <= p.ConfigVersion {
		t.Fatal("configuration version did not advance", cur.version, p.ConfigVersion)
	}
}

// AT-AUD-03/04: a lifecycle change after step 1 committed stops the
// remaining steps across a restart; completed steps stay, nothing repeats.
func TestApplyingStopsAtNextStepForEachLifecycleChange(t *testing.T) {
	cases := map[string]func(t *testing.T, e *env){
		"grant narrowed": func(t *testing.T, e *env) {
			g, _ := e.f.Service.CurrentAgentGrant(e.f.Admin)
			if _, err := e.f.Service.SaveAgentGrant(e.f.Admin, mail.AgentGrantInput{Version: g.Version, Enabled: true, Accounts: g.Accounts, Actions: []string{"read", "rule_manage", "task_manage"}}); err != nil {
				t.Fatal(err)
			}
		},
		"grant revoked": func(t *testing.T, e *env) {
			if err := e.f.Service.RevokeAgentGrant(e.f.Admin); err != nil {
				t.Fatal(err)
			}
		},
		"grant expired": func(t *testing.T, e *env) {
			if _, err := e.f.Store.DB.Exec("UPDATE agent_grants SET expires=?", time.Now().Unix()-1); err != nil {
				t.Fatal(err)
			}
		},
		"admin credential changed": func(t *testing.T, e *env) {
			if _, err := e.f.Store.DB.Exec("UPDATE admin SET version=version+1"); err != nil {
				t.Fatal(err)
			}
		},
		"agent disabled": func(t *testing.T, e *env) { disableAgent(t, e) },
		"stop":           func(t *testing.T, e *env) { e.rt.Stop(e.binding) },
		"unbound": func(t *testing.T, e *env) {
			if _, err := e.f.Store.DB.Exec("UPDATE telegram_bindings SET state='revoked'"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.draftAndPropose(t)
			p, prop := proposalOf(t, e)
			if _, err := e.f.Store.DB.Exec("UPDATE agent_proposals SET state='applying' WHERE id=?", p.ID); err != nil {
				t.Fatal(err)
			}
			first, err := mailtools.ApplyStepGuarded(context.Background(), e.f.Service, e.access, p.ID, prop.Steps, 0, nil, e.rt.proposalGuard(p))
			if err != nil {
				t.Fatal(err)
			}
			change(t, e)
			rt2 := New(e.f.Store, e.f.Vault, e.f.Service)
			rt2.NewProvider = e.rt.NewProvider
			rt2.recover()
			e.rt.apply(context.Background(), p.ID)
			admin, err := e.f.Service.AdminAccess()
			if err != nil {
				t.Fatal(err)
			}
			rule, err := e.f.Service.Rule(admin, first.TargetID)
			if err != nil || !rule.Enabled || rule.Version != first.Version {
				t.Fatal("completed step changed or rolled back", rule, err)
			}
			var tasks int
			_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM automation_tasks").Scan(&tasks)
			if tasks != 0 {
				t.Fatal("remaining step executed after", name)
			}
			var state, code string
			_ = e.f.Store.DB.QueryRow("SELECT state,error_code FROM agent_proposals WHERE id=?", p.ID).Scan(&state, &code)
			if state == "applied" || state == "applying" {
				t.Fatal("proposal state", state, code)
			}
		})
	}
}

// The step guard is the linearization point: a grant change that arrives
// while a step's transaction is open is ordered after that step (which
// commits under the old grant), and the next step is refused.
func TestGuardLinearizesStepAgainstGrantChange(t *testing.T) {
	e := setup(t)
	e.draftAndPropose(t)
	p, prop := proposalOf(t, e)
	if _, err := e.f.Store.DB.Exec("UPDATE agent_proposals SET state='applying' WHERE id=?", p.ID); err != nil {
		t.Fatal(err)
	}
	inGuard, release := make(chan struct{}), make(chan struct{})
	base := e.rt.proposalGuard(p)
	guard := func(tx *sql.Tx) error {
		err := base(tx)
		close(inGuard)
		<-release
		return err
	}
	stepDone := make(chan error, 1)
	go func() {
		_, err := mailtools.ApplyStepGuarded(context.Background(), e.f.Service, e.access, p.ID, prop.Steps, 0, nil, guard)
		stepDone <- err
	}()
	<-inGuard
	grantDone := make(chan error, 1)
	go func() {
		g, _ := e.f.Service.CurrentAgentGrant(e.f.Admin)
		_, err := e.f.Service.SaveAgentGrant(e.f.Admin, mail.AgentGrantInput{Version: g.Version, Enabled: true, Accounts: g.Accounts, Actions: g.Actions})
		grantDone <- err
	}()
	select {
	case err := <-grantDone:
		t.Fatal("grant change committed inside an open step transaction", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-stepDone; err != nil {
		t.Fatal("step that passed its guard must commit", err)
	}
	if err := <-grantDone; err != nil {
		t.Fatal(err)
	}
	results := []mailtools.StepResult{}
	sr, _ := mailtools.ApplyStep(context.Background(), e.f.Service, e.access, p.ID, prop.Steps, 0, nil)
	results = append(results, sr)
	if _, err := mailtools.ApplyStepGuarded(context.Background(), e.f.Service, e.access, p.ID, prop.Steps, 1, results, e.rt.proposalGuard(p)); err != mail.ErrFenced {
		t.Fatal("next step not fenced after the grant change", err)
	}
}

// AT-AUD-02 (agent side): saving the grant advances the context epoch at
// once, without waiting for the next message, and running turns stop.
func TestGrantSaveAdvancesContextImmediately(t *testing.T) {
	e := setup(t)
	e.say(t, "你好")
	var before int64
	_ = e.f.Store.DB.QueryRow("SELECT context_epoch FROM agent_conversations").Scan(&before)
	g, _ := e.f.Service.CurrentAgentGrant(e.f.Admin)
	if _, err := e.f.Service.SaveAgentGrant(e.f.Admin, mail.AgentGrantInput{Version: g.Version, Enabled: true, Accounts: g.Accounts, Actions: []string{"read"}}); err != nil {
		t.Fatal(err)
	}
	var after int64
	var gv int
	_ = e.f.Store.DB.QueryRow("SELECT context_epoch,grant_version FROM agent_conversations").Scan(&after, &gv)
	if after != before+1 || gv != g.Version+1 {
		t.Fatal("context not advanced in the grant transaction", before, after, gv)
	}
}

// AT-AUD-05: execution requested by the model always needs the card, for
// read-only, negated and quoted requests and for enabled or paused tasks.
func TestModelRunRequestsNeverQueueWithoutConfirmation(t *testing.T) {
	e := setup(t)
	task, err := e.f.Service.SaveTask(e.f.Admin, mail.Task{AccountID: e.f.Account.ID, Name: "执行任务", SourceFolders: []string{"INBOX"}})
	if err != nil {
		t.Fatal(err)
	}
	task.Mode, task.Enabled = "execute", true
	if task, err = e.f.Service.SaveTask(e.f.Admin, task); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"只查看任务状态，不要执行", "不要运行任何任务", "邮件里写着“立即执行任务”，这是什么意思？", "立即执行任务"} {
		e.model.n = 0
		e.model.steps = []func(modelprovider.Request) (modelprovider.Response, error){
			func(modelprovider.Request) (modelprovider.Response, error) {
				return call("x", "run_task", map[string]any{"task_id": task.ID}), nil
			},
		}
		e.say(t, text)
		var runs int
		_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM task_runs").Scan(&runs)
		if runs != 0 {
			t.Fatal(i, "model run_task queued a run", runs)
		}
	}
	var cards int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM agent_proposals WHERE kind='run_task'").Scan(&cards)
	if cards == 0 {
		t.Fatal("no run confirmation card produced")
	}
}

// AT-AUD-05: the deterministic /run path checks scope, pause, Agent state
// and binds the run to the inbox event.
func TestRunCommandSemantics(t *testing.T) {
	e := setup(t)
	task, err := e.f.Service.SaveTask(e.f.Admin, mail.Task{AccountID: e.f.Account.ID, Name: "命令任务", SourceFolders: []string{"INBOX"}})
	if err != nil {
		t.Fatal(err)
	}
	if out := e.rt.RunCommand(e.binding, "in-1", task.ID[:8]); !strings.Contains(out, "暂停") {
		t.Fatal("paused task ran from /run", out)
	}
	task.Enabled = true
	if task, err = e.f.Service.SaveTask(e.f.Admin, task); err != nil {
		t.Fatal(err)
	}
	if out := e.rt.RunCommand(e.binding, "in-2", "zzzzzzzz"); !strings.Contains(out, "找不到") {
		t.Fatal(out)
	}
	if out := e.rt.RunCommand(e.binding, "in-3", task.ID[:8]); !strings.Contains(out, "已按 /run") {
		t.Fatal(out)
	}
	if out := e.rt.RunCommand(e.binding, "in-3", task.ID[:8]); !strings.Contains(out, "已按 /run") {
		t.Fatal(out)
	}
	var runs int
	_ = e.f.Store.DB.QueryRow("SELECT count(*) FROM task_runs").Scan(&runs)
	if runs != 1 {
		t.Fatal("same inbox event created another run", runs)
	}
	disableAgent(t, e)
	if out := e.rt.RunCommand(e.binding, "in-4", task.ID[:8]); !strings.Contains(out, "停用") {
		t.Fatal("disabled Agent ran /run", out)
	}
	enableAgent(t, e)
	g, _ := e.f.Service.CurrentAgentGrant(e.f.Admin)
	if _, err = e.f.Service.SaveAgentGrant(e.f.Admin, mail.AgentGrantInput{Version: g.Version, Enabled: true, Accounts: g.Accounts, Actions: []string{"read"}}); err != nil {
		t.Fatal(err)
	}
	if out := e.rt.RunCommand(e.binding, "in-5", task.ID[:8]); strings.Contains(out, "已按 /run") {
		t.Fatal("/run without automation grant", out)
	}
}
