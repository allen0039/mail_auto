package telegram

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/interaction"
	"mail-auto/internal/mail"
	"mail-auto/internal/store"
)

// Agent is the narrow interface to the agent runtime.
type Agent interface {
	Submit(bindingID, inputID, text string) (int, error)
	Confirm(ctx context.Context, bindingID string, messageID int64, data string) string
	Stop(bindingID string) string
	NewSession(bindingID string) string
	Status(bindingID string) string
	CancelRun(bindingID, runID string) string
	RunCommand(bindingID, inputID, taskRef string) string
}

// Service owns interaction settings, bindings, the poller, the inbox
// processor and the reply sender.
type Service struct {
	Store *store.Store
	Vault *credentials.Vault
	Mail  *mail.Service
	Agent Agent
	// NewClient builds the Bot API client for a token; tests inject a fake.
	NewClient func(token string) *Client
	// PollTimeout is the getUpdates long-poll timeout in seconds.
	PollTimeout int

	settingsMu sync.Mutex
	workerID   string
	wake       chan struct{}
	pollMu     sync.Mutex
	pollCancel context.CancelFunc
	throttleMu sync.Mutex
	attempts   map[int64][]time.Time
	global     []time.Time
	notices    map[int64]time.Time
	wg         sync.WaitGroup
}

func New(st *store.Store, v *credentials.Vault, m *mail.Service, a Agent) *Service {
	service := &Service{Store: st, Vault: v, Mail: m, Agent: a, PollTimeout: 25, workerID: credentials.Random(12), wake: make(chan struct{}, 1), attempts: map[int64][]time.Time{}, notices: map[int64]time.Time{}}
	service.NewClient = func(token string) *Client {
		client := NewClient(token)
		client.ResolveHTTP = func() (*http.Client, error) { return m.TelegramHTTP(40 * time.Second) }
		return client
	}
	return service
}

func (s *Service) NetworkChanged() { s.restartPoll() }

// Settings is the interaction state shown to administrators. The token is
// never returned; it lives in the notification settings only.
type Settings struct {
	Version         int    `json:"version"`
	Enabled         bool   `json:"enabled"`
	BotID           int64  `json:"bot_id"`
	BotUsername     string `json:"bot_username"`
	Epoch           int64  `json:"epoch"`
	ActivationTime  int64  `json:"activation_time"`
	State           string `json:"state"`
	ErrorCode       string `json:"error_code"`
	CredentialReady bool   `json:"credential_ready"`
	LastPollAt      int64  `json:"last_poll_at"`
	Binding         *struct {
		ID        string `json:"id"`
		UserID    int64  `json:"user_id"`
		ChatID    int64  `json:"chat_id"`
		GrantID   string `json:"grant_id"`
		CreatedAt int64  `json:"created_at"`
	} `json:"binding"`
	credGen int
}

func (s *Service) load(q interaction.Querier) (Settings, error) {
	var st Settings
	err := q.QueryRow("SELECT version,enabled,bot_id,bot_username,credential_generation,epoch,activation_time,state,error_code FROM telegram_interaction_settings WHERE id=1").Scan(&st.Version, &st.Enabled, &st.BotID, &st.BotUsername, &st.credGen, &st.Epoch, &st.ActivationTime, &st.State, &st.ErrorCode)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{State: "off"}, nil
	}
	return st, err
}

func (s *Service) requireAdmin(a mail.Access) error {
	a, err := s.Mail.CheckAccess(a)
	if err != nil || a.Owner != "admin" {
		return mail.ErrDenied
	}
	return nil
}

// Get returns interaction settings, poller state and the active binding.
func (s *Service) Get(a mail.Access) (Settings, error) {
	if err := s.requireAdmin(a); err != nil {
		return Settings{}, err
	}
	st, err := s.load(s.Store.DB)
	if err != nil {
		return st, err
	}
	_, _, cerr := s.Mail.TelegramCredential()
	st.CredentialReady = cerr == nil
	_ = s.Store.DB.QueryRow("SELECT last_poll_at FROM telegram_poll_state WHERE bot_id=?", st.BotID).Scan(&st.LastPollAt)
	if b, err := interaction.ActiveBinding(s.Store.DB); err == nil {
		st.Binding = &struct {
			ID        string `json:"id"`
			UserID    int64  `json:"user_id"`
			ChatID    int64  `json:"chat_id"`
			GrantID   string `json:"grant_id"`
			CreatedAt int64  `json:"created_at"`
		}{b.ID, b.UserID, b.ChatID, b.GrantID, b.CreatedAt}
	}
	return st, nil
}

// SetEnabled turns interaction on or off at the expected version. Turning
// it on validates the shared bot token with getMe (outside any transaction)
// and starts a new receive epoch with a fresh activation time, so backlog
// from before activation never triggers management actions. Turning it off
// cancels not-yet-started interaction work; re-enabling does not revive it.
func (s *Service) SetEnabled(ctx context.Context, a mail.Access, version int, enabled bool) (Settings, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if err := s.requireAdmin(a); err != nil {
		return Settings{}, err
	}
	old, err := s.load(s.Store.DB)
	if err != nil {
		return old, err
	}
	if old.Version != version {
		return old, mail.ErrConflict
	}
	var bot User
	gen := old.credGen
	if enabled {
		token, g, err := s.Mail.TelegramCredential()
		if err != nil {
			return old, ErrCredentialMissing
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		bot, err = s.NewClient(token).GetMe(ctx)
		if err != nil {
			return old, &APIError{Code: codeOf(err)}
		}
		gen = g
	}
	now := time.Now().Unix()
	tx, err := s.Store.DB.Begin()
	if err != nil {
		return old, err
	}
	defer tx.Rollback()
	next := old
	next.Version = old.Version + 1
	next.Enabled = enabled
	if enabled {
		if old.BotID != 0 && (old.BotID != bot.ID || old.credGen != gen) {
			if err = revokeBindings(tx, "credential_changed"); err != nil {
				return old, err
			}
		}
		next.BotID, next.BotUsername, next.credGen = bot.ID, bot.Username, gen
		next.Epoch = old.Epoch + 1
		next.ActivationTime = now
		// Same bot and credential: the active binding is carried into the new
		// receive epoch, so inbound, execution and outbound agree. Work and
		// replies from before were cancelled/suppressed when interaction was
		// turned off and keep their old epoch, so they are never revived.
		if old.BotID == bot.ID && old.credGen == gen {
			if _, err = tx.Exec("UPDATE telegram_bindings SET epoch=? WHERE state='active' AND bot_id=?", next.Epoch, bot.ID); err != nil {
				return old, err
			}
		}
		next.State, next.ErrorCode = "starting", ""
		if _, err = tx.Exec("INSERT OR IGNORE INTO telegram_poll_state(bot_id) VALUES(?)", bot.ID); err != nil {
			return old, err
		}
	} else {
		next.State, next.ErrorCode = "off", ""
		if err = cancelInteractionWork(tx, "interaction_disabled"); err != nil {
			return old, err
		}
	}
	if old.Version == 0 {
		_, err = tx.Exec("INSERT INTO telegram_interaction_settings(id,version,enabled,bot_id,bot_username,credential_generation,epoch,activation_time,state,error_code,updated_at) VALUES(1,?,?,?,?,?,?,?,?,?,?)", next.Version, next.Enabled, next.BotID, next.BotUsername, next.credGen, next.Epoch, next.ActivationTime, next.State, next.ErrorCode, now)
	} else {
		var res sql.Result
		res, err = tx.Exec("UPDATE telegram_interaction_settings SET version=?,enabled=?,bot_id=?,bot_username=?,credential_generation=?,epoch=?,activation_time=?,state=?,error_code=?,updated_at=? WHERE id=1 AND version=?", next.Version, next.Enabled, next.BotID, next.BotUsername, next.credGen, next.Epoch, next.ActivationTime, next.State, next.ErrorCode, now, old.Version)
		if err == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				return old, mail.ErrConflict
			}
		}
	}
	if err != nil {
		return old, err
	}
	if err = store.Audit(tx, "telegram.interaction", map[bool]string{true: "enable", false: "disable"}[enabled], "ok"); err != nil {
		return old, err
	}
	if err = tx.Commit(); err != nil {
		return old, err
	}
	s.restartPoll()
	return s.Get(a)
}

// cancelInteractionWork stops every not-yet-started piece of interaction
// work: queued turns, unapplied proposals, unprocessed inbox events and
// unsent replies.
func cancelInteractionWork(tx *sql.Tx, reason string) error {
	now := time.Now().Unix()
	for _, q := range []string{
		// A running turn re-checks its state before every model request and
		// write, so cancelling it here stops it at the next boundary.
		"UPDATE agent_turns SET state='cancelled',error_code=?,updated_at=? WHERE state IN ('queued','running')",
		"UPDATE agent_proposals SET state='invalidated',error_code=?,updated_at=? WHERE state IN ('pending','accepted','applying')",
		"UPDATE telegram_inbox SET state='stale',error_code=?,payload=NULL,updated_at=? WHERE state='pending'",
		"UPDATE telegram_reply_outbox SET state='suppressed',error_code=?,updated_at=? WHERE state IN ('pending','retry')",
	} {
		if _, err := tx.Exec(q, reason, now); err != nil {
			return err
		}
	}
	return nil
}

func revokeBindings(tx *sql.Tx, reason string) error {
	now := time.Now().Unix()
	if _, err := tx.Exec("UPDATE telegram_bindings SET state='revoked',revoked_at=?,revoke_reason=? WHERE state='active'", now, reason); err != nil {
		return err
	}
	return cancelInteractionWork(tx, reason)
}

// BindCode creates a one-time binding code (128-bit, 5 minutes). Only its
// hash is stored; it cannot be read again.
func (s *Service) BindCode(a mail.Access) (string, int64, error) {
	if err := s.requireAdmin(a); err != nil {
		return "", 0, err
	}
	st, err := s.load(s.Store.DB)
	if err != nil {
		return "", 0, err
	}
	if !st.Enabled || st.BotID == 0 {
		return "", 0, mail.ErrInput
	}
	g, err := s.Mail.CurrentAgentGrant(a)
	if err != nil || !g.Enabled || g.Stale || g.Expires <= time.Now().Unix() {
		return "", 0, ErrGrantRequired
	}
	if _, err = interaction.ActiveBinding(s.Store.DB); err == nil {
		return "", 0, mail.ErrConflict
	}
	code := credentials.Random(16)
	expires := time.Now().Add(5 * time.Minute).Unix()
	if _, err = s.Store.DB.Exec("INSERT INTO telegram_bind_codes(code_hash,grant_id,grant_version,admin_version,bot_id,epoch,expires,created_at) VALUES(?,?,?,?,?,?,?,?)", credentials.Digest(code), g.ID, g.Version, g.AdminVersion, st.BotID, st.Epoch, expires, time.Now().Unix()); err != nil {
		return "", 0, err
	}
	_ = store.Audit(s.Store.DB, "telegram.bind_code", g.ID, "ok")
	return code, expires, nil
}

var ErrGrantRequired = errors.New("grant_required")

// ErrCredentialMissing means no usable bot token is saved in notifications.
var ErrCredentialMissing = errors.New("telegram_credential_missing")

// Unbind revokes the active binding and cancels its unstarted work.
func (s *Service) Unbind(a mail.Access) error {
	if err := s.requireAdmin(a); err != nil {
		return err
	}
	tx, err := s.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = revokeBindings(tx, "unbound"); err != nil {
		return err
	}
	if err = store.Audit(tx, "telegram.unbind", "binding", "ok"); err != nil {
		return err
	}
	return tx.Commit()
}

func validCommand(text string) (string, string) {
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	cmd, arg, _ := strings.Cut(strings.TrimSpace(text), " ")
	if at := strings.Index(cmd, "@"); at > 0 {
		cmd = cmd[:at]
	}
	return strings.ToLower(cmd), strings.TrimSpace(arg)
}

// RecoverOffline runs during `bootstrap recover`, before any worker starts.
// After a snapshot restore it revokes every binding, unapplied proposal and
// queued or running turn from the snapshot, invalidates outstanding bind
// codes and starts a new receive epoch with a fresh activation time, so no
// command accumulated after the snapshot is executed automatically and no
// old binding regains write access.
func RecoverOffline(st *store.Store) error {
	tx, err := st.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if err = revokeBindings(tx, "bootstrap_recover"); err != nil {
		return err
	}
	for _, q := range []string{
		"UPDATE agent_turns SET state='cancelled',error_code='bootstrap_recover',updated_at=? WHERE state IN ('queued','running')",
		"UPDATE agent_proposals SET state='invalidated',error_code='bootstrap_recover',updated_at=? WHERE state IN ('pending','accepted','applying')",
		"UPDATE telegram_bind_codes SET consumed_at=? WHERE consumed_at=0",
		"UPDATE telegram_poll_state SET lease_owner='',lease_until=0 WHERE ?>0",
	} {
		if _, err = tx.Exec(q, now); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("UPDATE telegram_interaction_settings SET epoch=epoch+1,activation_time=?,state=CASE WHEN enabled=1 THEN 'starting' ELSE 'off' END,updated_at=? WHERE id=1", now, now); err != nil {
		return err
	}
	if err = store.Audit(tx, "telegram.recover", "bootstrap", "ok"); err != nil {
		return err
	}
	return tx.Commit()
}
