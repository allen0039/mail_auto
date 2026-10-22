package mail

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"time"
)

type AutomaticAccountStatus struct {
	AccountID string `json:"account_id"`
	State     string `json:"state"`
	Mode      string `json:"mode"`
	Folder    string `json:"folder,omitempty"`
	CheckedAt int64  `json:"checked_at"`
	Error     string `json:"error,omitempty"`
}
type AutomaticStatus struct {
	Enabled         bool                     `json:"enabled"`
	IntervalSeconds int                      `json:"interval_seconds"`
	Accounts        []AutomaticAccountStatus `json:"accounts"`
}
type automaticWorker struct {
	account Account
	cancel  context.CancelFunc
	done    chan struct{}
	status  AutomaticAccountStatus
}
type automaticRunner struct {
	interval time.Duration
	wake     chan struct{}
	workers  map[string]*automaticWorker
}

func (s *Service) StartAutomaticSync(interval time.Duration) error {
	if interval <= 0 {
		return errors.New("自动同步间隔必须大于 0")
	}
	remote, ok := s.Remote.(AutomaticRemote)
	if !ok {
		return errors.New("邮箱适配器不支持自动同步")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return context.Canceled
	}
	if s.automatic != nil {
		return nil
	}
	if s.Context == nil {
		s.Context = context.Background()
	}
	if s.cancel == nil {
		s.Context, s.cancel = context.WithCancel(s.Context)
	}
	runner := &automaticRunner{interval: interval, wake: make(chan struct{}, 1), workers: map[string]*automaticWorker{}}
	s.automatic = runner
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.superviseAutomatic(s.Context, runner, remote) }()
	return nil
}

func (s *Service) AutomaticStatus() AutomaticStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := AutomaticStatus{Accounts: []AutomaticAccountStatus{}}
	if s.automatic == nil {
		return out
	}
	out.Enabled, out.IntervalSeconds = true, int(s.automatic.interval/time.Second)
	for _, w := range s.automatic.workers {
		out.Accounts = append(out.Accounts, w.status)
	}
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].AccountID < out.Accounts[j].AccountID })
	return out
}
func (s *Service) IndexRevision() uint64 { return s.revision.Load() }
func (s *Service) wakeAutomatic() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.automatic != nil {
		select {
		case s.automatic.wake <- struct{}{}:
		default:
		}
	}
}
func (s *Service) automaticAccounts(ctx context.Context) ([]Account, error) {
	rows, e := s.Store.DB.QueryContext(ctx, "SELECT * FROM accounts ORDER BY created_at,id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, e := scanAccount(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Service) superviseAutomatic(ctx context.Context, r *automaticRunner, remote AutomaticRemote) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		accounts, e := s.automaticAccounts(ctx)
		if e == nil {
			s.mu.Lock()
			if s.stopping {
				s.mu.Unlock()
				return
			}
			wanted := map[string]Account{}
			for _, a := range accounts {
				wanted[a.ID] = a
			}
			for id, w := range r.workers {
				a, exists := wanted[id]
				if !exists || !reflect.DeepEqual(a, w.account) {
					w.cancel()
				}
				select {
				case <-w.done:
					delete(r.workers, id)
				default:
				}
			}
			for _, a := range accounts {
				if r.workers[a.ID] != nil {
					continue
				}
				workerCtx, cancel := context.WithCancel(ctx)
				w := &automaticWorker{account: a, cancel: cancel, done: make(chan struct{}), status: AutomaticAccountStatus{AccountID: a.ID, State: "connecting"}}
				r.workers[a.ID] = w
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					defer func() { close(w.done); s.wakeAutomatic() }()
					s.runAutomatic(workerCtx, r, remote, w)
				}()
			}
			s.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.wake:
		}
	}
}

func (s *Service) setAutomaticStatus(w *automaticWorker, state, mode, folder, message string, checked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.status.State, w.status.Mode, w.status.Folder, w.status.Error = state, mode, folder, message
	if checked {
		w.status.CheckedAt = time.Now().Unix()
	}
}
func automaticPause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Idle sockets do not occupy a read slot. Foreground requests and active
// automatic reads share the same two-request budget; manual work takes priority.
func (s *Service) automaticRead(ctx context.Context, w *automaticWorker, mode, folder string, fn func(uint64) error) error {
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		s.mu.Lock()
		busy := s.stopping || s.active[w.account.ID] || s.pending[w.account.ID] || s.running >= 2
		epoch := s.epoch[w.account.ID]
		if !busy {
			s.running++
		}
		s.mu.Unlock()
		if busy {
			s.setAutomaticStatus(w, "waiting", mode, folder, "", false)
			if !automaticPause(ctx, 200*time.Millisecond) {
				return ctx.Err()
			}
			continue
		}
		s.setAutomaticStatus(w, "syncing", mode, folder, "", false)
		e := fn(epoch)
		s.mu.Lock()
		s.running--
		s.mu.Unlock()
		if errors.Is(e, ErrBusy) {
			if !automaticPause(ctx, 200*time.Millisecond) {
				return ctx.Err()
			}
			continue
		}
		return e
	}
}
func (s *Service) runAutomatic(ctx context.Context, r *automaticRunner, remote AutomaticRemote, w *automaticWorker) {
	failures := 0
	for ctx.Err() == nil {
		s.setAutomaticStatus(w, "connecting", "", "", "", false)
		pw, e := s.password(w.account)
		var session AutomaticSession
		if e == nil {
			e = s.automaticRead(ctx, w, "", "", func(uint64) error {
				var e error
				session, e = remote.OpenAutomatic(ctx, w.account, pw)
				return e
			})
		} else {
			e = syncStep("读取邮箱凭据", e)
		}
		if e == nil {
			e = s.followAutomatic(ctx, r, w, session)
			session.Close()
		}
		if ctx.Err() != nil || errors.Is(e, errAutomaticConfig) {
			return
		}
		failures++
		delay := min(time.Duration(1<<min(failures-1, 4))*5*time.Second, time.Minute)
		message := syncErrorMessage(e)
		s.setAutomaticStatus(w, "retrying", "", "", message, false)
		slog.Warn("自动同步等待重连", "account_id", w.account.ID, "reason", message, "retry_after_seconds", int(delay/time.Second))
		if !automaticPause(ctx, delay) {
			return
		}
	}
}
func (s *Service) followAutomatic(ctx context.Context, r *automaticRunner, w *automaticWorker, session AutomaticSession) error {
	mode := "poll"
	if session.PushSupported() {
		mode = "idle"
	}
	refresh := func(folder string) error {
		return s.automaticRead(ctx, w, mode, folder, func(epoch uint64) error { return s.incrementalFolder(ctx, session, w.account, folder, epoch) })
	}
	for ctx.Err() == nil {
		var folders []string
		e := s.automaticRead(ctx, w, mode, "", func(epoch uint64) error {
			folders = w.account.Folders
			if len(folders) == 0 {
				var e error
				folders, e = session.Folders(ctx)
				if e != nil {
					return e
				}
			}
			return s.reconcileAutomaticFolders(ctx, w.account, folders, epoch)
		})
		if e != nil {
			return e
		}
		folders = append([]string(nil), folders...)
		sort.SliceStable(folders, func(i, j int) bool {
			if strings.EqualFold(folders[i], "INBOX") {
				return !strings.EqualFold(folders[j], "INBOX")
			}
			if strings.EqualFold(folders[j], "INBOX") {
				return false
			}
			return folders[i] < folders[j]
		})
		for _, folder := range folders {
			if e = refresh(folder); e != nil {
				return e
			}
		}
		s.setAutomaticStatus(w, "watching", mode, "", "", true)
		if len(folders) == 0 {
			if !automaticPause(ctx, r.interval) {
				return ctx.Err()
			}
			continue
		}
		deadline := time.Now().Add(r.interval)
		for time.Now().Before(deadline) {
			changed, e := session.Wait(ctx, folders[0], time.Until(deadline))
			if e != nil {
				return e
			}
			mode = "poll"
			if session.PushSupported() {
				mode = "idle"
			}
			s.setAutomaticStatus(w, "watching", mode, "", "", false)
			if changed {
				if !automaticPause(ctx, 200*time.Millisecond) {
					return ctx.Err()
				}
				if e = refresh(folders[0]); e != nil {
					return e
				}
				s.setAutomaticStatus(w, "watching", mode, "", "", true)
			}
		}
	}
	return ctx.Err()
}
