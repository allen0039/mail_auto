package imap

import (
	"context"

	protocol "github.com/emersion/go-imap"
	"mail-auto/internal/mail"
)

// SetFlags changes only \Seen and/or \Flagged on one exact UID. All steps are
// either read-only or idempotent STOREs, so a dropped connection may safely
// reconnect and repeat them. It never issues EXPUNGE.
func (a Adapter) SetFlags(ctx context.Context, account mail.Account, password string, m mail.Message, change mail.FlagChange, validate func(mail.Message) error) (mail.Message, error) {
	if change.Empty() {
		return mail.Message{}, mail.ErrInput
	}
	var out mail.Message
	err := a.withWriteConnection(ctx, account, password, func(conn *connection) error {
		var err error
		out, err = setFlagsOnConnection(conn, account.ID, m, change, validate)
		return err
	})
	return out, err
}

// withWriteConnection reuses the pooled task connection when a write session is
// active, otherwise it opens a dedicated connection for this call.
func (a Adapter) withWriteConnection(ctx context.Context, account mail.Account, password string, fn func(*connection) error) error {
	if a.pool == nil || !a.pool.enabled(account.ID) {
		var conn *connection
		defer func() {
			if conn != nil {
				conn.close()
			}
		}()
		return retryRemote(ctx, func() error {
			if conn != nil {
				conn.close()
			}
			var err error
			if conn, err = a.open(ctx, account, password); err != nil {
				conn = nil
				return err
			}
			return fn(conn)
		})
	}
	pool := a.pool
	pool.mu.Lock()
	defer pool.mu.Unlock()
	conn := pool.conns[account.ID]
	attempt := 0
	err := retryRemote(ctx, func() error {
		attempt++
		if attempt > 1 && conn != nil {
			conn.close()
			conn = nil
		}
		if conn == nil {
			var err error
			if conn, err = a.open(ctx, account, password); err != nil {
				conn = nil
				return err
			}
		}
		return fn(conn)
	})
	if err != nil {
		delete(pool.conns, account.ID)
		if conn != nil {
			conn.close()
		}
		return err
	}
	pool.conns[account.ID] = conn
	return nil
}

func setFlagsOnConnection(conn *connection, accountID string, m mail.Message, change mail.FlagChange, validate func(mail.Message) error) (mail.Message, error) {
	if validate != nil {
		fresh, err := ruleStateOnConnection(conn, accountID, m)
		if err != nil {
			return mail.Message{}, err
		}
		if err = validate(fresh); err != nil {
			return mail.Message{}, err
		}
	}
	conn.resetBudget()
	selected, err := conn.c.Select(m.Folder, false)
	if err != nil {
		return mail.Message{}, remoteFailure(conn, "打开邮件目录", err)
	}
	if selected.ReadOnly {
		return mail.Message{}, mail.ErrCapability
	}
	if selected.UidValidity != m.UIDValidity {
		return mail.Message{}, ErrStale
	}
	set := new(protocol.SeqSet)
	set.AddNum(m.UID)
	criteria := liveMessages()
	criteria.Uid = set
	conn.resetBudget()
	uids, err := conn.c.UidSearch(criteria)
	if err != nil {
		return mail.Message{}, remoteFailure(conn, "复核邮件", err)
	}
	if len(uids) != 1 || uids[0] != m.UID {
		return mail.Message{}, ErrStale
	}
	var add, remove []interface{}
	for _, f := range []struct {
		want *bool
		flag string
	}{{change.Seen, protocol.SeenFlag}, {change.Flagged, protocol.FlaggedFlag}} {
		if f.want == nil {
			continue
		}
		if *f.want {
			add = append(add, f.flag)
		} else {
			remove = append(remove, f.flag)
		}
	}
	for _, op := range []struct {
		mode  protocol.FlagsOp
		flags []interface{}
	}{{protocol.AddFlags, add}, {protocol.RemoveFlags, remove}} {
		if len(op.flags) == 0 {
			continue
		}
		conn.resetBudget()
		if err = conn.c.UidStore(set, protocol.FormatFlagsOp(op.mode, true), op.flags, nil); err != nil {
			return mail.Message{}, remoteFailure(conn, "修改邮件标记", err)
		}
	}
	// Confirm the server-side result instead of trusting the STORE response.
	conn.resetBudget()
	ch := make(chan *protocol.Message, 1)
	done := make(chan error, 1)
	go func() { done <- conn.c.UidFetch(set, []protocol.FetchItem{protocol.FetchUid, protocol.FetchFlags}, ch) }()
	var confirmed *protocol.Message
	for v := range ch {
		if v != nil && v.Uid == m.UID {
			confirmed = v
		}
	}
	if err = <-done; err != nil {
		return mail.Message{}, remoteFailure(conn, "确认邮件标记", err)
	}
	if confirmed == nil || hasFlag(confirmed.Flags, protocol.DeletedFlag) {
		return mail.Message{}, ErrStale
	}
	out := m
	out.Unread = isUnread(confirmed.Flags)
	out.Flagged = hasFlag(confirmed.Flags, protocol.FlaggedFlag)
	out.FlagsKnown = true
	if !change.Satisfied(out) {
		return mail.Message{}, remoteProblem("确认邮件标记", "服务器未应用请求的标记")
	}
	return out, nil
}
