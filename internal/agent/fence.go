package agent

import (
	"database/sql"
	"errors"
	"time"

	"mail-auto/internal/interaction"
)

// execution is the frozen set of conditions an agent write was authorized
// under: the binding and its grant/admin versions, the provider (Agent)
// configuration version, and the conversation stop epoch.
type execution struct {
	BindingID     string
	GrantID       string
	GrantVersion  int
	AdminVersion  int
	ConfigVersion int
	ConvID        string
	StopEpoch     int64
}

// fenceReason checks the frozen conditions against live state using q (a
// transaction when used as a write guard). It returns "" when the write may
// proceed. Every lifecycle change that must stop agent work commits in its
// own transaction and changes at least one of these values:
//   - grant save/revoke/expiry/disable, admin credential change → grant_changed
//   - unbind, rebind, credential change, interaction off   → binding_changed
//   - provider disable or any provider configuration save  → agent_disabled
//   - /stop                                                 → stopped
func fenceReason(q interaction.Querier, x execution, now int64) string {
	b, err := interaction.BindingByID(q, x.BindingID)
	if err != nil || b.State != "active" || b.GrantID != x.GrantID || b.AdminVersion != x.AdminVersion {
		return "binding_changed"
	}
	var settingsEnabled int
	var settingsEpoch int64
	switch err = q.QueryRow("SELECT enabled,epoch FROM telegram_interaction_settings WHERE id=1").Scan(&settingsEnabled, &settingsEpoch); {
	case errors.Is(err, sql.ErrNoRows):
		// No interaction settings row: no binding can have been created
		// through Telegram (offline tests insert bindings directly).
	case err != nil || settingsEnabled != 1 || settingsEpoch != b.Epoch:
		return "binding_changed"
	}
	var version, gAdmin, enabled, revoked, admin int
	var expires int64
	if q.QueryRow("SELECT version,admin_version,enabled,revoked,expires FROM agent_grants WHERE id=?", x.GrantID).Scan(&version, &gAdmin, &enabled, &revoked, &expires) != nil {
		return "grant_changed"
	}
	if q.QueryRow("SELECT version FROM admin WHERE id=1").Scan(&admin) != nil {
		return "grant_changed"
	}
	if version != x.GrantVersion || enabled != 1 || revoked != 0 || expires <= now || gAdmin != x.AdminVersion || admin != x.AdminVersion {
		return "grant_changed"
	}
	var pVersion, pEnabled int
	if q.QueryRow("SELECT version,enabled FROM agent_provider_settings WHERE id=1").Scan(&pVersion, &pEnabled) != nil || pEnabled != 1 || pVersion != x.ConfigVersion {
		return "agent_disabled"
	}
	var stop int64
	if q.QueryRow("SELECT stop_epoch FROM agent_conversations WHERE id=?", x.ConvID).Scan(&stop) != nil || stop != x.StopEpoch {
		return "stopped"
	}
	return ""
}

var errFenced = errors.New("fenced")

// turnGuard is the in-transaction guard for tool writes of a running turn:
// the frozen fence plus the turn still running in its context epoch.
func (r *Runtime) turnGuard(s *session) func(*sql.Tx) error {
	x := execution{BindingID: s.binding.ID, GrantID: s.binding.GrantID, GrantVersion: s.grant.Version, AdminVersion: s.binding.AdminVersion, ConfigVersion: s.provider.version, ConvID: s.conv.ID, StopEpoch: s.turn.StopEpoch}
	turnID, ctxEpoch := s.turn.ID, s.conv.ContextEpoch
	return func(tx *sql.Tx) error {
		if fenceReason(tx, x, time.Now().Unix()) != "" {
			return errFenced
		}
		var state string
		var epoch int64
		if tx.QueryRow("SELECT t.state,c.context_epoch FROM agent_turns t JOIN agent_conversations c ON c.id=t.conversation_id WHERE t.id=?", turnID).Scan(&state, &epoch) != nil || state != "running" || epoch != ctxEpoch {
			return errFenced
		}
		return nil
	}
}

func (p proposalRow) execution() execution {
	return execution{BindingID: p.BindingID, GrantID: p.GrantID, GrantVersion: p.GrantVersion, AdminVersion: p.AdminVersion, ConfigVersion: p.ConfigVersion, ConvID: p.ConvID, StopEpoch: p.StopEpoch}
}

// proposalGuard is the in-transaction guard for each proposal step: the
// confirmation's frozen fence plus the proposal still in state applying.
// Lifecycle changes move applying proposals to invalidated/cancelled in the
// same transaction that changes the fence, so either check stops the step.
func (r *Runtime) proposalGuard(p proposalRow) func(*sql.Tx) error {
	x := p.execution()
	id := p.ID
	return func(tx *sql.Tx) error {
		if fenceReason(tx, x, time.Now().Unix()) != "" {
			return errFenced
		}
		var state string
		if tx.QueryRow("SELECT state FROM agent_proposals WHERE id=?", id).Scan(&state) != nil || state != "applying" {
			return errFenced
		}
		return nil
	}
}

// fencedReason explains why a proposal can no longer execute.
func (r *Runtime) fencedReason(p proposalRow) string {
	if reason := fenceReason(r.Store.DB, p.execution(), time.Now().Unix()); reason != "" {
		return reason
	}
	var state, code string
	_ = r.Store.DB.QueryRow("SELECT state,error_code FROM agent_proposals WHERE id=?", p.ID).Scan(&state, &code)
	if code != "" {
		return code
	}
	return "state_changed"
}
