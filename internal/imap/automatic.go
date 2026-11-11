package imap

import (
	"context"
	"sort"
	"strings"
	"time"

	protocol "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-imap/responses"
	"mail-auto/internal/mail"
)

type automaticSession struct {
	conn    *connection
	account mail.Account
	push    bool
	stop    func() bool
	known   map[string]mail.Inventory
}

func (a Adapter) OpenAutomatic(ctx context.Context, account mail.Account, password string) (mail.AutomaticSession, error) {
	conn, e := a.open(ctx, account, password)
	if e != nil {
		return nil, e
	}
	s := &automaticSession{conn: conn, account: account, known: map[string]mail.Inventory{}}
	s.stop = context.AfterFunc(ctx, conn.close)
	conn.resetBudget()
	s.push, e = conn.c.Support("IDLE")
	if e != nil {
		s.Close()
		return nil, remoteFailure(conn, "查询推送能力", e)
	}
	return s, nil
}
func (s *automaticSession) Close()              { s.stop(); s.conn.close() }
func (s *automaticSession) PushSupported() bool { return s.push }
func (s *automaticSession) Folders(ctx context.Context) ([]string, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	s.conn.resetBudget()
	ch := make(chan *protocol.MailboxInfo, 16)
	done := make(chan error, 1)
	go func() { done <- s.conn.c.List("", "*", ch) }()
	folders := []string{}
	for f := range ch {
		selectable := true
		for _, attr := range f.Attributes {
			if strings.EqualFold(attr, protocol.NoSelectAttr) {
				selectable = false
			}
		}
		if selectable {
			folders = append(folders, f.Name)
		}
	}
	if e := <-done; e != nil {
		return nil, remoteFailure(s.conn, "读取文件夹列表", e)
	}
	sort.Strings(folders)
	return folders, nil
}

func (s *automaticSession) Refresh(ctx context.Context, folder string, previous mail.FolderState, emit func(mail.Snapshot) error) (mail.Inventory, error) {
	out := mail.Inventory{Unread: map[uint32]bool{}, Flagged: map[uint32]bool{}}
	if e := ctx.Err(); e != nil {
		return out, e
	}
	s.conn.resetBudget()
	status, e := s.conn.c.Select(folder, true)
	if e != nil {
		return out, remoteFailure(s.conn, "打开文件夹", e)
	}
	if status.UidValidity == 0 {
		return out, remoteProblem("打开文件夹", "服务器未返回有效 UIDVALIDITY")
	}
	out.UIDValidity = status.UidValidity
	out.Total = status.Messages
	s.conn.resetBudget()
	uids, e := s.conn.c.UidSearch(liveMessages())
	if e != nil {
		return out, remoteFailure(s.conn, "查询邮件 UID", e)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	var existing, added []uint32
	for i, uid := range uids {
		if uid == 0 || (i > 0 && uid == uids[i-1]) {
			return out, remoteProblem("查询邮件 UID", "服务器返回无效或重复的 UID")
		}
		if _, ok := previous.Unread[uid]; ok && previous.UIDValidity == out.UIDValidity {
			existing = append(existing, uid)
		} else {
			added = append(added, uid)
		}
	}
	if e = emit(mail.Snapshot{UIDValidity: out.UIDValidity}); e != nil {
		return out, e
	}
	batchSize := UIDBatchSize
	if s.account.Provider == "netease" {
		batchSize = 100
	}
	for start := 0; start < len(existing); start += batchSize {
		if e = ctx.Err(); e != nil {
			return out, e
		}
		set := new(protocol.SeqSet)
		set.AddNum(existing[start:min(start+batchSize, len(existing))]...)
		s.conn.resetBudget()
		ch := make(chan *protocol.Message, 16)
		done := make(chan error, 1)
		go func() {
			done <- s.conn.c.UidFetch(set, []protocol.FetchItem{protocol.FetchUid, protocol.FetchFlags}, ch)
		}()
		var invalid bool
		for m := range ch {
			if m == nil || m.Uid == 0 || !set.Contains(m.Uid) {
				invalid = true
				continue
			}
			if _, ok := m.Items[protocol.FetchFlags]; !ok {
				invalid = true
				continue
			}
			if hasFlag(m.Flags, protocol.DeletedFlag) {
				continue
			}
			if _, duplicate := out.Unread[m.Uid]; duplicate {
				invalid = true
				continue
			}
			out.Unread[m.Uid] = isUnread(m.Flags)
			out.Flagged[m.Uid] = hasFlag(m.Flags, protocol.FlaggedFlag)
			out.UIDs = append(out.UIDs, m.Uid)
		}
		if e = <-done; e != nil {
			return out, remoteFailure(s.conn, "读取邮件状态", e)
		}
		if invalid {
			return out, remoteProblem("读取邮件状态", "服务器返回无效或重复的 UID")
		}
	}
	for start := 0; start < len(added); start += batchSize {
		if e = ctx.Err(); e != nil {
			return out, e
		}
		set := new(protocol.SeqSet)
		set.AddNum(added[start:min(start+batchSize, len(added))]...)
		batch, e := fetchBatch(s.conn, s.account.ID, folder, out.UIDValidity, set)
		if e != nil {
			return out, e
		}
		for _, m := range batch.Messages {
			if _, duplicate := out.Unread[m.UID]; duplicate {
				return out, remoteProblem("读取邮件元数据", "服务器返回重复的 UID")
			}
			out.Unread[m.UID] = m.Unread
			if m.FlagsKnown {
				out.Flagged[m.UID] = m.Flagged
			}
			out.UIDs = append(out.UIDs, m.UID)
		}
		if e = emit(batch); e != nil {
			return out, e
		}
	}
	// UIDs absent from a successful FETCH have been expunged since SEARCH.
	s.known[folder] = out
	return out, nil
}
func liveMessages() *protocol.SearchCriteria {
	criteria := protocol.NewSearchCriteria()
	criteria.WithoutFlags = []string{protocol.DeletedFlag}
	return criteria
}
func hasFlag(flags []string, wanted string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, wanted) {
			return true
		}
	}
	return false
}
func isUnread(flags []string) bool { return !hasFlag(flags, protocol.SeenFlag) }

// Register the notification handler for the IDLE command itself. This avoids
// assigning Client.Updates while its reader goroutine is receiving responses.
type idleNotifications struct {
	responses.Idle
	changed chan struct{}
}

func (h *idleNotifications) Handle(resp protocol.Resp) error {
	if data, ok := resp.(*protocol.DataResp); ok {
		name, _, valid := protocol.ParseNamedResp(data)
		if valid && (name == "EXISTS" || name == "EXPUNGE" || name == "FETCH") {
			select {
			case h.changed <- struct{}{}:
			default:
			}
		}
	}
	return h.Idle.Handle(resp)
}
func (s *automaticSession) Wait(ctx context.Context, folder string, d time.Duration) (bool, error) {
	if d <= 0 {
		return false, nil
	}
	if !s.push {
		timer := time.NewTimer(min(d, 25*time.Second))
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			s.conn.resetBudget()
			if e := s.conn.c.Noop(); e != nil {
				return false, remoteFailure(s.conn, "保持邮箱连接", e)
			}
			return false, nil
		}
	}
	s.conn.resetBudget()
	status, e := s.conn.c.Select(folder, true)
	if e != nil {
		return false, remoteFailure(s.conn, "打开推送文件夹", e)
	}
	if known, ok := s.known[folder]; !ok || known.UIDValidity != status.UidValidity || known.Total != status.Messages {
		return true, nil
	}
	// Renew before the 45-second command deadline, even with a long check interval.
	d = min(d, 25*time.Second)
	type idleResult struct {
		err      error
		rejected bool
	}
	stop := make(chan struct{})
	done := make(chan idleResult, 1)
	h := &idleNotifications{Idle: responses.Idle{Stop: stop, RepliesCh: make(chan []byte, 10)}, changed: make(chan struct{}, 1)}
	s.conn.resetBudget()
	go func() {
		status, e := s.conn.c.Execute(&commands.Idle{}, h)
		rejected := e == nil && status.Err() != nil
		if e == nil {
			e = status.Err()
		}
		done <- idleResult{e, rejected}
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	changed := false
	select {
	case <-ctx.Done():
	case <-timer.C:
	case <-h.changed:
		changed = true
	case result := <-done:
		close(stop)
		if result.err != nil {
			failure := remoteFailure(s.conn, "等待邮箱推送", result.err)
			if result.rejected {
				// Advertised but rejected IDLE: keep a healthy socket and poll.
				s.push = false
				return false, nil
			}
			return false, failure
		}
		s.push = false
		return false, nil
	}
	close(stop)
	if result := <-done; result.err != nil {
		return false, remoteFailure(s.conn, "结束邮箱推送", result.err)
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return changed, nil
}
