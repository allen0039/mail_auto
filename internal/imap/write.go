package imap

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	protocol "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-imap/responses"
	"mail-auto/internal/mail"
)

func (a Adapter) RuleState(ctx context.Context, account mail.Account, password string, original mail.Message) (mail.Message, error) {
	var out mail.Message
	err := retryRemote(ctx, func() error {
		conn, err := a.open(ctx, account, password)
		if err != nil {
			return err
		}
		defer conn.close()
		out, err = ruleStateOnConnection(conn, account.ID, original)
		return err
	})
	return out, err
}

func ruleStateOnConnection(conn *connection, accountID string, original mail.Message) (mail.Message, error) {
	conn.resetBudget()
	status, err := conn.c.Select(original.Folder, true)
	if err != nil {
		return mail.Message{}, remoteFailure(conn, "复核整理邮件", err)
	}
	if status.UidValidity != original.UIDValidity {
		return mail.Message{}, mail.ErrConflict
	}
	set := new(protocol.SeqSet)
	set.AddNum(original.UID)
	batch, err := fetchBatch(conn, accountID, original.Folder, status.UidValidity, set)
	if err != nil {
		return mail.Message{}, err
	}
	if len(batch.Messages) != 1 || batch.Messages[0].UID != original.UID {
		return mail.Message{}, mail.ErrConflict
	}
	return batch.Messages[0], nil
}

func (a Adapter) Capabilities(ctx context.Context, account mail.Account, password string) (mail.Capabilities, error) {
	conn, e := a.open(ctx, account, password)
	if e != nil {
		return mail.Capabilities{}, e
	}
	defer conn.close()
	out := mail.Capabilities{Folders: []mail.FolderInfo{}}
	out.Move, e = conn.c.Support("MOVE")
	if e != nil {
		return out, ErrRemote
	}
	out.UIDPlus, e = conn.c.Support("UIDPLUS")
	if e != nil {
		return out, ErrRemote
	}
	ch := make(chan *protocol.MailboxInfo, 16)
	done := make(chan error, 1)
	go func() { done <- conn.c.List("", "*", ch) }()
	for f := range ch {
		selectable := true
		role := ""
		for _, attr := range f.Attributes {
			switch strings.ToLower(attr) {
			case "\\noselect":
				selectable = false
			case "\\archive":
				role = "archive"
			case "\\trash":
				role = "trash"
			case "\\junk":
				role = "junk"
			case "\\drafts":
				role = "drafts"
			case "\\sent":
				role = "sent"
			}
		}
		if selectable {
			if strings.EqualFold(f.Name, "INBOX") {
				role = "inbox"
			}
			out.Folders = append(out.Folders, mail.FolderInfo{Name: f.Name, Label: mail.FolderLabel(f.Name), Role: role})
		}
	}
	if e = <-done; e != nil {
		return out, ErrRemote
	}
	return out, nil
}
func copyUID(status *protocol.StatusResp, source uint32, dest string) (mail.MoveResult, bool) {
	if status == nil || !strings.EqualFold(string(status.Code), "COPYUID") || len(status.Arguments) != 3 {
		return mail.MoveResult{}, false
	}
	validity, e := strconv.ParseUint(fmt.Sprint(status.Arguments[0]), 10, 32)
	if e != nil || validity == 0 {
		return mail.MoveResult{}, false
	}
	src, e := protocol.ParseSeqSet(fmt.Sprint(status.Arguments[1]))
	if e != nil || len(src.Set) != 1 || src.Set[0].Start != source || src.Set[0].Stop != source {
		return mail.MoveResult{}, false
	}
	target, e := protocol.ParseSeqSet(fmt.Sprint(status.Arguments[2]))
	if e != nil || len(target.Set) != 1 || target.Set[0].Start == 0 || target.Set[0].Start != target.Set[0].Stop {
		return mail.MoveResult{}, false
	}
	return mail.MoveResult{Folder: dest, UIDValidity: uint32(validity), UID: target.Set[0].Start}, true
}

func (a Adapter) movePreflight(ctx context.Context, account mail.Account, password string, original mail.Message) (*connection, bool, error) {
	conn, e := a.open(ctx, account, password)
	if e != nil {
		return nil, false, e
	}
	native, e := movePreflightOnConnection(conn, original)
	if e != nil {
		conn.close()
		return nil, false, e
	}
	return conn, native, nil
}

func movePreflightOnConnection(conn *connection, original mail.Message) (bool, error) {
	conn.resetBudget()
	native, e := conn.c.Support("MOVE")
	if e != nil {
		return false, remoteFailure(conn, "查询移动能力", e)
	}
	conn.resetBudget()
	uidplus, e := conn.c.Support("UIDPLUS")
	if e != nil {
		return false, remoteFailure(conn, "查询 UIDPLUS 能力", e)
	}
	if !uidplus {
		return false, mail.ErrCapability
	}
	conn.resetBudget()
	selected, e := conn.c.Select(original.Folder, false)
	if e != nil {
		return false, remoteFailure(conn, "打开整理源目录", e)
	}
	if selected.ReadOnly {
		return false, mail.ErrCapability
	}
	if selected.UidValidity != original.UIDValidity {
		return false, ErrStale
	}
	set := new(protocol.SeqSet)
	set.AddNum(original.UID)
	criteria := liveMessages()
	criteria.Uid = set
	conn.resetBudget()
	uids, e := conn.c.UidSearch(criteria)
	if e != nil {
		return false, remoteFailure(conn, "复核整理源邮件", e)
	}
	if len(uids) != 1 || uids[0] != original.UID {
		return false, ErrStale
	}
	return native, nil
}

// Prefer native UID MOVE; UIDPLUS fallback expunges only the confirmed source UID.
// Never use Client.UidMove, which can fall back to a global EXPUNGE. Everything
// before Execute is read-only, so a dropped preflight can safely reconnect. Once
// the write command is issued, every transport failure remains uncertain and is
// never replayed.
func (a Adapter) Move(ctx context.Context, account mail.Account, password string, m mail.Message, dest string, record func(mail.MoveResult) error) (mail.MoveResult, error) {
	var conn *connection
	var native bool
	err := retryRemote(ctx, func() error {
		if conn != nil {
			conn.close()
			conn = nil
		}
		var err error
		conn, native, err = a.movePreflight(ctx, account, password, m)
		return err
	})
	if err != nil {
		return mail.MoveResult{}, err
	}
	defer conn.close()
	return a.executeMove(conn, native, m, dest, record)
}

func (a Adapter) BeginWriteSession(accountID string) func() {
	if a.pool == nil {
		return func() {}
	}
	a.pool.mu.Lock()
	a.pool.active[accountID]++
	a.pool.mu.Unlock()
	return func() {
		a.pool.mu.Lock()
		if a.pool.active[accountID] <= 1 {
			delete(a.pool.active, accountID)
		} else {
			a.pool.active[accountID]--
		}
		conn := a.pool.conns[accountID]
		delete(a.pool.conns, accountID)
		a.pool.mu.Unlock()
		if conn != nil {
			conn.close()
		}
	}
}

// MoveWithRuleState validates the latest remote message state and performs the
// write on the same authenticated connection. During a task batch, consecutive
// messages reuse that connection instead of opening a new TLS session each time.
func (a Adapter) MoveWithRuleState(
	ctx context.Context,
	account mail.Account,
	password string,
	m mail.Message,
	dest string,
	validate func(mail.Message) error,
	record func(mail.MoveResult) error,
) (mail.MoveResult, error) {
	if a.pool != nil && a.pool.enabled(account.ID) {
		return a.moveWithRuleStatePooled(ctx, account, password, m, dest, validate, record)
	}
	return a.moveWithRuleStateDirect(ctx, account, password, m, dest, validate, record)
}

func (p *writePool) enabled(accountID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active[accountID] > 0
}

func (a Adapter) moveWithRuleStateDirect(
	ctx context.Context,
	account mail.Account,
	password string,
	m mail.Message,
	dest string,
	validate func(mail.Message) error,
	record func(mail.MoveResult) error,
) (mail.MoveResult, error) {
	var conn *connection
	var native bool
	err := retryRemote(ctx, func() error {
		if conn != nil {
			conn.close()
			conn = nil
		}
		var err error
		conn, err = a.open(ctx, account, password)
		if err != nil {
			return err
		}
		fresh, err := ruleStateOnConnection(conn, account.ID, m)
		if err != nil {
			return err
		}
		if err = validate(fresh); err != nil {
			return err
		}
		native, err = movePreflightOnConnection(conn, m)
		return err
	})
	if err != nil {
		if conn != nil {
			conn.close()
		}
		return mail.MoveResult{}, err
	}
	defer conn.close()
	return a.executeMove(conn, native, m, dest, record)
}

func (a Adapter) moveWithRuleStatePooled(
	ctx context.Context,
	account mail.Account,
	password string,
	m mail.Message,
	dest string,
	validate func(mail.Message) error,
	record func(mail.MoveResult) error,
) (mail.MoveResult, error) {
	pool := a.pool
	if a.Network != nil {
		route, err := a.Network.Resolve("account", account.ID)
		if err != nil {
			return mail.MoveResult{}, err
		}
		pool.mu.Lock()
		if old := pool.conns[account.ID]; old != nil && old.networkKey != route.Key {
			old.close()
			delete(pool.conns, account.ID)
		}
		pool.mu.Unlock()
	}
	pool.mu.Lock()
	if pool.active[account.ID] <= 0 {
		pool.mu.Unlock()
		return a.moveWithRuleStateDirect(ctx, account, password, m, dest, validate, record)
	}
	conn := pool.conns[account.ID]
	var native bool
	attempt := 0
	err := retryRemote(ctx, func() error {
		attempt++
		if attempt > 1 && conn != nil {
			conn.close()
			conn = nil
		}
		if conn == nil {
			var err error
			conn, err = a.open(ctx, account, password)
			if err != nil {
				return err
			}
		}
		fresh, err := ruleStateOnConnection(conn, account.ID, m)
		if err != nil {
			return err
		}
		if err = validate(fresh); err != nil {
			return err
		}
		native, err = movePreflightOnConnection(conn, m)
		return err
	})
	if err != nil {
		delete(pool.conns, account.ID)
		if conn != nil {
			conn.close()
		}
		pool.mu.Unlock()
		return mail.MoveResult{}, err
	}
	result, err := a.executeMove(conn, native, m, dest, record)
	if err != nil {
		delete(pool.conns, account.ID)
		conn.close()
		pool.mu.Unlock()
		return result, err
	}
	pool.conns[account.ID] = conn
	pool.mu.Unlock()
	return result, nil
}

func (a Adapter) executeMove(conn *connection, native bool, m mail.Message, dest string, record func(mail.MoveResult) error) (mail.MoveResult, error) {

	set := new(protocol.SeqSet)
	set.AddNum(m.UID)
	var mapping mail.MoveResult
	var mapped bool
	handler := responses.HandlerFunc(func(resp protocol.Resp) error {
		if st, ok := resp.(*protocol.StatusResp); ok {
			if r, yes := copyUID(st, m.UID, dest); yes {
				mapping, mapped = r, true
			}
		}
		return responses.ErrUnhandled
	})
	var command protocol.Commander = &commands.Move{SeqSet: set, Mailbox: dest}
	if !native {
		command = &commands.Copy{SeqSet: set, Mailbox: dest}
	}
	conn.resetBudget()
	status, e := conn.c.Execute(&commands.Uid{Cmd: command}, handler)
	if r, yes := copyUID(status, m.UID, dest); yes {
		mapping, mapped = r, true
	}
	if mapped {
		if save := record(mapping); save != nil {
			return mapping, uncertainProblem("保存目标映射", "目标 UID 已返回但本地保存失败")
		}
	}
	// Any error after issuing MOVE is uncertain: tagged NO can include partial effects.
	if e != nil {
		return mapping, uncertainFailure(conn, "移动邮件", e)
	}
	if status == nil || status.Err() != nil {
		return mapping, uncertainProblem("移动邮件", "服务器未确认移动命令")
	}
	if !mapped {
		return mapping, uncertainProblem("移动邮件", "服务器未返回目标 UID（COPYUID）")
	}
	if !native {
		// The destination must be visible at the exact returned UID before removing
		// the source. Failure at any phase is never automatically retried.
		if e = verifyDestination(conn, mapping); e != nil {
			return mapping, e
		}
		// Destination verification selects another mailbox. Reselect and validate
		// the source before STORE/UID EXPUNGE, using this same login throughout.
		conn.resetBudget()
		selected, err := conn.c.Select(m.Folder, false)
		if err != nil {
			return mapping, uncertainFailure(conn, "重新打开整理源目录", err)
		}
		if selected.ReadOnly || selected.UidValidity != m.UIDValidity {
			return mapping, uncertainProblem("重新打开整理源目录", "源目录只读或 UIDVALIDITY 已变化")
		}
		conn.resetBudget()
		if e = conn.c.UidStore(set, protocol.FormatFlagsOp(protocol.AddFlags, true), []interface{}{protocol.DeletedFlag}, nil); e != nil {
			return mapping, uncertainFailure(conn, "标记已复制的源邮件", e)
		}
		conn.resetBudget()
		st, err := conn.c.Execute(&commands.Uid{Cmd: &protocol.Command{Name: "EXPUNGE", Arguments: []interface{}{set}}}, nil)
		if err != nil {
			return mapping, uncertainFailure(conn, "移除已复制的源邮件", err)
		}
		if st == nil || st.Err() != nil {
			return mapping, uncertainProblem("移除已复制的源邮件", "服务器拒绝 UID EXPUNGE")
		}
	}
	// A successful native MOVE or UIDPLUS COPY + targeted UID EXPUNGE is decided
	// by the tagged server response. The destination was verified before source
	// cleanup for the fallback path; repeating both mailbox searches here adds
	// two failure points without adding evidence that changes the decision.
	return mapping, nil
}
func (a Adapter) VerifyMove(ctx context.Context, account mail.Account, password string, m mail.Message, result mail.MoveResult) error {
	if result.UID == 0 || result.UIDValidity == 0 || result.Folder == "" {
		return mail.ErrUncertain
	}
	return retryRemote(ctx, func() error {
		conn, err := a.open(ctx, account, password)
		if err != nil {
			return fmt.Errorf("%w: %w", mail.ErrUncertain, err)
		}
		defer conn.close()
		return verifyMove(conn, m, result)
	})
}

// InspectMove resolves an interrupted MOVE that did not return COPYUID. It is
// read-only: a source message still present under the original UID plus no
// unique destination match proves the write was not applied. Ambiguous or
// incomplete evidence stays unresolved so callers cannot replay MOVE.
func (a Adapter) InspectMove(ctx context.Context, account mail.Account, password string, m mail.Message, destination string) (mail.MoveEvidence, error) {
	var out mail.MoveEvidence
	if m.Folder == "" || m.UID == 0 || m.UIDValidity == 0 || destination == "" || strings.EqualFold(m.Folder, destination) {
		return out, mail.ErrUncertain
	}
	err := retryRemote(ctx, func() error {
		conn, err := a.open(ctx, account, password)
		if err != nil {
			return err
		}
		defer conn.close()
		conn.resetBudget()
		source, err := conn.c.Select(m.Folder, true)
		if err != nil {
			return remoteFailure(conn, "打开移动核验源目录", err)
		}
		if source.UidValidity != m.UIDValidity {
			return mail.ErrUncertain
		}
		set := new(protocol.SeqSet)
		set.AddNum(m.UID)
		query := liveMessages()
		query.Uid = set
		conn.resetBudget()
		sourceUIDs, err := conn.c.UidSearch(query)
		if err != nil {
			return remoteFailure(conn, "核验移动源邮件", err)
		}
		if len(sourceUIDs) > 1 || (len(sourceUIDs) == 1 && sourceUIDs[0] != m.UID) {
			return mail.ErrUncertain
		}
		if len(sourceUIDs) == 0 {
			query = protocol.NewSearchCriteria()
			query.Uid = set
			conn.resetBudget()
			physicalUIDs, err := conn.c.UidSearch(query)
			if err != nil {
				return remoteFailure(conn, "核验已删除源邮件", err)
			}
			if len(physicalUIDs) != 0 {
				return mail.ErrUncertain
			}
		}

		conn.resetBudget()
		target, err := conn.c.Select(destination, true)
		if err != nil {
			return remoteFailure(conn, "打开移动核验目标目录", err)
		}
		if target.UidValidity == 0 {
			return remoteProblem("打开移动核验目标目录", "服务器未返回有效 UIDVALIDITY")
		}
		conn.resetBudget()
		targetUIDs, err := conn.c.UidSearch(liveMessages())
		if err != nil {
			return remoteFailure(conn, "查询移动核验目标邮件", err)
		}
		matches := []uint32{}
		for start := 0; start < len(targetUIDs); start += UIDBatchSize {
			chunk := new(protocol.SeqSet)
			chunk.AddNum(targetUIDs[start:min(start+UIDBatchSize, len(targetUIDs))]...)
			batch, err := fetchBatch(conn, account.ID, destination, target.UidValidity, chunk)
			if err != nil {
				return err
			}
			expected := targetUIDs[start:min(start+UIDBatchSize, len(targetUIDs))]
			if len(batch.Messages) != len(expected) {
				return mail.ErrUncertain
			}
			seen := map[uint32]bool{}
			for _, candidate := range batch.Messages {
				if seen[candidate.UID] {
					return mail.ErrUncertain
				}
				seen[candidate.UID] = true
				if sameMoveMetadata(m, candidate) {
					matches = append(matches, candidate.UID)
				}
			}
		}
		if len(matches) > 1 {
			return mail.ErrUncertain
		}
		out = mail.MoveEvidence{SourcePresent: len(sourceUIDs) == 1}
		if len(matches) == 1 {
			out.DestinationPresent = true
			out.Destination = mail.MoveResult{Folder: destination, UIDValidity: target.UidValidity, UID: matches[0]}
		}
		return nil
	})
	return out, err
}

// FinishMove completes the source cleanup of an interrupted UIDPLUS MOVE. It
// never copies the message again and never issues a global EXPUNGE.
//
// The saved destination must still match the original metadata. On the same
// authenticated session that performs the cleanup, the source UID is selected
// read-write and its metadata and flags are read again; validate receives that
// fresh state immediately before STORE \Deleted and again immediately before
// UID EXPUNGE. If the second check fails, the \Deleted flag added by this call
// is removed and nothing is expunged.
//
// IMAP has no conditional expunge, so a flag change made by another client in
// the few milliseconds between the last FETCH and UID EXPUNGE cannot be
// excluded. CONDSTORE's UNCHANGEDSINCE would only guard the STORE, not the
// EXPUNGE, so the post-STORE re-read gives the same guarantee without
// depending on a capability several providers lack.
func (a Adapter) FinishMove(ctx context.Context, account mail.Account, password string, m mail.Message, result mail.MoveResult, validate func(mail.Message) error) error {
	if result.UID == 0 || result.UIDValidity == 0 || result.Folder == "" ||
		m.Folder == "" || m.UID == 0 || m.UIDValidity == 0 ||
		strings.EqualFold(m.Folder, result.Folder) || validate == nil {
		return mail.ErrUncertain
	}
	return retryRemote(ctx, func() error {
		conn, err := a.open(ctx, account, password)
		if err != nil {
			return fmt.Errorf("%w: %w", mail.ErrUncertain, err)
		}
		defer conn.close()

		// A previous attempt may already have removed the source. In that case
		// the exact saved destination mapping is sufficient.
		if verifyMove(conn, m, result) == nil {
			return nil
		}
		conn.resetBudget()
		uidplus, err := conn.c.Support("UIDPLUS")
		if err != nil {
			return uncertainFailure(conn, "查询恢复移动能力", err)
		}
		if !uidplus {
			return uncertainProblem("查询恢复移动能力", "服务器不再支持 UIDPLUS")
		}
		if err = verifyMessageFingerprint(conn, account.ID, m, result.Folder, result.UIDValidity, result.UID); err != nil {
			return err
		}

		conn.resetBudget()
		source, err := conn.c.Select(m.Folder, false)
		if err != nil {
			return uncertainFailure(conn, "打开移动源目录", err)
		}
		if source.ReadOnly || source.UidValidity != m.UIDValidity {
			return uncertainProblem("打开移动源目录", "源目录只读或 UIDVALIDITY 已变化")
		}
		fresh, wasDeleted, err := readRecoverySource(conn, account.ID, m)
		if err != nil {
			return err
		}
		if err = validate(fresh); err != nil {
			return err
		}
		set := new(protocol.SeqSet)
		set.AddNum(m.UID)
		conn.resetBudget()
		if err = conn.c.UidStore(set, protocol.FormatFlagsOp(protocol.AddFlags, true), []interface{}{protocol.DeletedFlag}, nil); err != nil {
			return uncertainFailure(conn, "标记已复制的源邮件", err)
		}
		// Re-read after marking: a concurrent client may have changed the
		// message between the first check and the STORE.
		fresh, _, err = readRecoverySource(conn, account.ID, m)
		if err == nil {
			err = validate(fresh)
		}
		if err != nil {
			if !wasDeleted {
				conn.resetBudget()
				if undo := conn.c.UidStore(set, protocol.FormatFlagsOp(protocol.RemoveFlags, true), []interface{}{protocol.DeletedFlag}, nil); undo != nil {
					return uncertainFailure(conn, "撤回源邮件删除标记", undo)
				}
			}
			return err
		}
		conn.resetBudget()
		status, err := conn.c.Execute(&commands.Uid{Cmd: &protocol.Command{Name: "EXPUNGE", Arguments: []interface{}{set}}}, nil)
		if err != nil {
			return uncertainFailure(conn, "移除已复制的源邮件", err)
		}
		if status == nil || status.Err() != nil {
			return uncertainProblem("移除已复制的源邮件", "服务器拒绝 UID EXPUNGE")
		}
		return nil
	})
}

// readRecoverySource reads the selected source UID, including a message already
// flagged \Deleted, and checks that it is still the planned message.
func readRecoverySource(conn *connection, accountID string, m mail.Message) (mail.Message, bool, error) {
	set := new(protocol.SeqSet)
	set.AddNum(m.UID)
	conn.resetBudget()
	ch := make(chan *protocol.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- conn.c.UidFetch(set, []protocol.FetchItem{protocol.FetchUid, protocol.FetchFlags}, ch)
	}()
	var deleted bool
	for v := range ch {
		if v != nil && v.Uid == m.UID {
			deleted = hasFlag(v.Flags, protocol.DeletedFlag)
		}
	}
	if err := <-done; err != nil {
		return mail.Message{}, false, uncertainFailure(conn, "复核待清理的源邮件", err)
	}
	batch, err := fetchMetadata(conn, accountID, m.Folder, m.UIDValidity, set, true)
	if err != nil {
		return mail.Message{}, false, err
	}
	if len(batch.Messages) != 1 || batch.Messages[0].UID != m.UID || !sameMoveMetadata(m, batch.Messages[0]) {
		return mail.Message{}, false, uncertainProblem("复核待清理的源邮件", "源邮件元数据与计划不一致或已不存在")
	}
	return batch.Messages[0], deleted, nil
}

func verifyMessageFingerprint(conn *connection, accountID string, expected mail.Message, folder string, validity, uid uint32) error {
	conn.resetBudget()
	status, err := conn.c.Select(folder, true)
	if err != nil {
		return remoteFailure(conn, "打开移动核验目录", err)
	}
	if status.UidValidity != validity {
		return uncertainProblem("打开移动核验目录", "目录 UIDVALIDITY 已变化")
	}
	set := new(protocol.SeqSet)
	set.AddNum(uid)
	batch, err := fetchMetadata(conn, accountID, folder, validity, set, folder == expected.Folder)
	if err != nil {
		return err
	}
	if len(batch.Messages) != 1 || batch.Messages[0].UID != uid || !sameMoveMetadata(expected, batch.Messages[0]) {
		return uncertainProblem("核验邮件指纹", "邮件元数据与计划不一致或已不存在")
	}
	return nil
}

func sameMoveMetadata(a, b mail.Message) bool {
	return a.Subject == b.Subject &&
		a.Sender == b.Sender &&
		a.SentAt == b.SentAt &&
		a.Size == b.Size
}

func verifyMove(conn *connection, m mail.Message, result mail.MoveResult) error {
	for _, check := range []struct {
		folder        string
		validity, uid uint32
		present       bool
	}{{m.Folder, m.UIDValidity, m.UID, false}, {result.Folder, result.UIDValidity, result.UID, true}} {
		conn.resetBudget()
		st, e := conn.c.Select(check.folder, true)
		if e != nil {
			return uncertainFailure(conn, "打开移动核验目录", e)
		}
		if st.UidValidity != check.validity {
			return uncertainProblem("打开移动核验目录", "目录 UIDVALIDITY 已变化")
		}
		set := new(protocol.SeqSet)
		set.AddNum(check.uid)
		q := liveMessages()
		if !check.present {
			q = protocol.NewSearchCriteria()
		}
		q.Uid = set
		conn.resetBudget()
		uids, e := conn.c.UidSearch(q)
		if e != nil {
			return uncertainFailure(conn, "核验移动邮件位置", e)
		}
		if (len(uids) > 0) != check.present {
			return uncertainProblem("核验移动邮件位置", "源邮件仍在或目标邮件缺失")
		}
	}
	return nil
}

// uncertainProblem reports an issued or partially verified write whose effect
// cannot be decided, with a fixed stage and reason for diagnostics.
func uncertainProblem(stage, reason string) error {
	return fmt.Errorf("%w: %w", mail.ErrUncertain, remoteProblem(stage, reason))
}

func uncertainFailure(c *connection, stage string, err error) error {
	return fmt.Errorf("%w: %w", mail.ErrUncertain, remoteFailure(c, stage, err))
}

func verifyDestination(c *connection, r mail.MoveResult) error {
	c.resetBudget()
	st, e := c.c.Select(r.Folder, true)
	if e != nil {
		return uncertainFailure(c, "打开复制目标目录", e)
	}
	if st.UidValidity != r.UIDValidity {
		return uncertainProblem("打开复制目标目录", "目标目录 UIDVALIDITY 与返回映射不一致")
	}
	set := new(protocol.SeqSet)
	set.AddNum(r.UID)
	q := liveMessages()
	q.Uid = set
	c.resetBudget()
	uids, e := c.c.UidSearch(q)
	if e != nil {
		return uncertainFailure(c, "核验复制目标邮件", e)
	}
	if len(uids) != 1 || uids[0] != r.UID {
		return uncertainProblem("核验复制目标邮件", "目标目录中找不到返回的 UID")
	}
	return nil
}

func (a Adapter) CreateFolder(ctx context.Context, account mail.Account, password, name string) error {
	if !mail.ValidFolderName(name) {
		return mail.ErrInput
	}
	c, e := a.open(ctx, account, password)
	if e != nil {
		return e
	}
	defer c.close()
	c.resetBudget()
	if e = c.c.Create(name); e != nil {
		return mail.ErrUncertain
	}
	// LIST verifies the exact provider name; no local-only folder is published.
	c.resetBudget()
	ch := make(chan *protocol.MailboxInfo, 16)
	done := make(chan error, 1)
	go func() { done <- c.c.List("", "*", ch) }()
	found := false
	for box := range ch {
		selectable := true
		for _, attr := range box.Attributes {
			if strings.EqualFold(attr, "\\Noselect") {
				selectable = false
			}
		}
		if box.Name == name && selectable {
			found = true
		}
	}
	if e = <-done; e != nil || !found {
		return mail.ErrUncertain
	}
	return nil
}
