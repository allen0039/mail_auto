// Package imap contains the only remote-mail implementation. It exposes no write operations.
package imap

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	protocol "github.com/emersion/go-imap"
	id "github.com/emersion/go-imap-id"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/charset"
	"mail-auto/internal/mail"
	mailmime "mail-auto/internal/mime"
)

// UIDBatchSize bounds each protocol response, not the total number of messages.
const UIDBatchSize = 500

var ErrRemote = errors.New("邮箱连接或读取失败，请检查 IMAP 开通状态、授权码、文件夹和网络")
var ErrStale = errors.New("邮件引用已失效，请重新同步后查询")

type Adapter struct {
	connect func(context.Context, mail.Account, string) (*connection, error)
	pool    *writePool
}

type writePool struct {
	mu     sync.Mutex
	active map[string]int
	conns  map[string]*connection
}

func NewAdapter() *Adapter {
	return &Adapter{pool: &writePool{active: map[string]int{}, conns: map[string]*connection{}}}
}

func init() { protocol.CharsetReader = charset.Reader }
func (a Adapter) open(ctx context.Context, account mail.Account, password string) (*connection, error) {
	if a.connect != nil {
		return a.connect(ctx, account, password)
	}
	return dial(ctx, account, password)
}

// Each connection has an absolute deadline and read budget, including protocol metadata.
type boundedConn struct {
	net.Conn
	remaining atomic.Int64
	errMu     sync.Mutex
	err       error
}

func (c *boundedConn) remember(err error) {
	if err != nil {
		c.errMu.Lock()
		if c.err == nil {
			c.err = err
		}
		c.errMu.Unlock()
	}
}
func (c *boundedConn) lastError() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

func (c *boundedConn) Read(p []byte) (int, error) {
	remaining := c.remaining.Load()
	if remaining <= 0 {
		c.remember(errResponseLimit)
		return 0, errResponseLimit
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, e := c.Conn.Read(p)
	c.remaining.Add(-int64(n))
	c.remember(e)
	return n, e
}

type connection struct {
	budget *boundedConn
	c      *client.Client
	raw    net.Conn
	stop   func() bool
	once   sync.Once
}

func dial(ctx context.Context, a mail.Account, password string) (*connection, error) {
	initCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: &tls.Config{ServerName: a.Host, MinVersion: tls.VersionTLS12}}
	raw, e := d.DialContext(initCtx, "tcp", net.JoinHostPort(a.Host, strconv.Itoa(a.Port)))
	if e != nil {
		cancel()
		return nil, remoteFailure(nil, "建立 TLS 连接", e)
	}
	deadline, _ := initCtx.Deadline()
	_ = raw.SetDeadline(deadline)
	bc := &boundedConn{Conn: raw}
	bc.remaining.Store(32 << 20)
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	c, e := client.New(bc)
	if e != nil {
		stop()
		cancel()
		raw.Close()
		return nil, remoteFailure(nil, "等待 IMAP 欢迎响应", e)
	}
	c.Timeout = 45 * time.Second
	c.ErrorLog = log.New(io.Discard, "", 0)
	conn := &connection{c: c, raw: raw, budget: bc, stop: stop}
	// ID is required by some NetEase deployments. Ignore only an unsupported capability.
	conn.resetBudget()
	if supported, e := c.Support("ID"); e != nil {
		err := remoteFailure(conn, "查询服务器能力", e)
		conn.close()
		return nil, err
	} else if supported {
		conn.resetBudget()
		if _, e = id.NewClient(c).ID(id.ID{"name": "mail-auto", "version": "0.1.0"}); e != nil {
			err := remoteFailure(conn, "发送客户端标识", e)
			conn.close()
			return nil, err
		}
	}
	conn.resetBudget()
	if e = c.Login(a.Username, password); e != nil {
		err := remoteFailure(conn, "邮箱登录", e)
		conn.close()
		return nil, err
	}
	return conn, nil
}
func (c *connection) close() { c.once.Do(func() { c.stop(); _ = c.raw.Close() }) }
func (c *connection) resetBudget() {
	if c.budget != nil {
		c.budget.remaining.Store(32 << 20)
	}
	_ = c.raw.SetDeadline(time.Now().Add(45 * time.Second))
}
func (adapter Adapter) Folders(ctx context.Context, a mail.Account, password string) ([]string, error) {
	var folders []string
	e := retryRemote(ctx, func() error {
		var e error
		folders, e = adapter.folders(ctx, a, password)
		return e
	})
	return folders, e
}

func (adapter Adapter) folders(ctx context.Context, a mail.Account, password string) ([]string, error) {
	conn, e := adapter.open(ctx, a, password)
	if e != nil {
		return nil, e
	}
	defer conn.close()
	conn.resetBudget()
	ch := make(chan *protocol.MailboxInfo, 16)
	done := make(chan error, 1)
	go func() { done <- conn.c.List("", "*", ch) }()
	var folders []string
	for f := range ch {
		selectable := true
		for _, attr := range f.Attributes {
			if strings.EqualFold(attr, "\\Noselect") {
				selectable = false
			}
		}
		if selectable {
			folders = append(folders, f.Name)
		}
	}
	if e = <-done; e != nil {
		return nil, remoteFailure(conn, "读取文件夹列表", e)
	}
	sort.Strings(folders)
	return folders, nil
}

func (adapter Adapter) selectFolder(ctx context.Context, a mail.Account, password, folder string) (*connection, *protocol.MailboxStatus, error) {
	conn, e := adapter.open(ctx, a, password)
	if e != nil {
		return nil, nil, e
	}
	conn.resetBudget()
	status, e := conn.c.Select(folder, true)
	if e != nil {
		err := remoteFailure(conn, "打开文件夹", e)
		conn.close()
		return nil, nil, err
	}
	if status.UidValidity == 0 {
		conn.close()
		return nil, nil, remoteProblem("打开文件夹", "服务器未返回有效 UIDVALIDITY")
	}
	return conn, status, nil
}

// Scan enumerates existing UIDs once, then fetches finite batches of metadata.
// This supports missing UIDNEXT and sparse UID spaces without sequence-number
// pagination, which can shift when messages are expunged during a scan.
func (adapter Adapter) Scan(ctx context.Context, a mail.Account, password, folder string, emit func(mail.Snapshot) error) error {
	var conn *connection
	var status *protocol.MailboxStatus
	var uids []uint32
	defer func() {
		if conn != nil {
			conn.close()
		}
	}()
	e := retryRemote(ctx, func() error {
		var e error
		conn, status, e = adapter.selectFolder(ctx, a, password, folder)
		if e != nil {
			return e
		}
		conn.resetBudget()
		uids, e = conn.c.UidSearch(liveMessages())
		if e != nil {
			err := remoteFailure(conn, "查询邮件 UID", e)
			conn.close()
			conn = nil
			return err
		}
		return nil
	})
	if e != nil {
		return e
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	for i, uid := range uids {
		if uid == 0 || (i > 0 && uid == uids[i-1]) {
			return remoteProblem("查询邮件 UID", "服务器返回无效或重复的 UID")
		}
	}
	if e = emit(mail.Snapshot{UIDValidity: status.UidValidity}); e != nil {
		return e
	}
	batchSize := UIDBatchSize
	if a.Provider == "netease" {
		batchSize = 100
	}
	for start := 0; start < len(uids); start += batchSize {
		if e = ctx.Err(); e != nil {
			return e
		}
		set := new(protocol.SeqSet)
		set.AddNum(uids[start:min(start+batchSize, len(uids))]...)
		var batch mail.Snapshot
		e = retryRemote(ctx, func() error {
			if conn == nil {
				var current *protocol.MailboxStatus
				var e error
				conn, current, e = adapter.selectFolder(ctx, a, password, folder)
				if e != nil {
					return e
				}
				if current.UidValidity != status.UidValidity {
					return remoteProblem("恢复文件夹读取", "重连后 UIDVALIDITY 已变化，请重新同步")
				}
			}
			var e error
			batch, e = fetchBatch(conn, a.ID, folder, status.UidValidity, set)
			if e != nil {
				conn.close()
				conn = nil
			}
			return e
		})
		if e != nil {
			return e
		}
		if e = emit(batch); e != nil {
			return e
		}
	}
	return nil
}

func fetchBatch(conn *connection, accountID, folder string, validity uint32, set *protocol.SeqSet) (mail.Snapshot, error) {
	return fetchMetadata(conn, accountID, folder, validity, set, false)
}

func fetchMetadata(conn *connection, accountID, folder string, validity uint32, set *protocol.SeqSet, includeDeleted bool) (mail.Snapshot, error) {
	conn.resetBudget()
	ch := make(chan *protocol.Message, 16)
	done := make(chan error, 1)
	go func() {
		done <- conn.c.UidFetch(set, []protocol.FetchItem{protocol.FetchUid, protocol.FetchEnvelope, protocol.FetchFlags, protocol.FetchRFC822Size, protocol.FetchInternalDate}, ch)
	}()
	batch := mail.Snapshot{UIDValidity: validity, Messages: []mail.Message{}}
	var decodeErr error
	for m := range ch {
		if m == nil || m.Envelope == nil || m.Uid == 0 || !set.Contains(m.Uid) {
			decodeErr = remoteProblem("读取邮件元数据", "服务器返回无效的邮件元数据")
			continue
		}
		// A message can acquire \\Deleted between SEARCH and FETCH.
		if !includeDeleted && hasFlag(m.Flags, protocol.DeletedFlag) {
			continue
		}
		date := m.InternalDate
		if date.IsZero() {
			date = m.Envelope.Date
		}
		sender := []string{}
		for _, from := range m.Envelope.From {
			sender = append(sender, from.Address())
		}
		unread := true
		for _, flag := range m.Flags {
			if flag == protocol.SeenFlag {
				unread = false
			}
		}
		subject, _ := mailmime.Truncate(strings.ToValidUTF8(m.Envelope.Subject, "�"), 4096)
		from, _ := mailmime.Truncate(strings.ToValidUTF8(strings.Join(sender, ", "), "�"), 2048)
		received := int64(0)
		if !m.InternalDate.IsZero() {
			received = m.InternalDate.Unix()
		}
		_, flagsKnown := m.Items[protocol.FetchFlags]
		batch.Messages = append(batch.Messages, mail.Message{ReceivedAt: received, Flagged: hasFlag(m.Flags, protocol.FlaggedFlag), FlagsKnown: flagsKnown, MetadataVersion: 1, AccountID: accountID, Folder: folder, UIDValidity: validity, UID: m.Uid, Subject: subject, Sender: from, SentAt: date.Unix(), Unread: unread, Size: m.Size})
	}
	if e := <-done; e != nil {
		return mail.Snapshot{}, remoteFailure(conn, "读取邮件元数据", e)
	}
	if decodeErr != nil {
		return mail.Snapshot{}, decodeErr
	}
	return batch, nil
}
func (adapter Adapter) Read(ctx context.Context, a mail.Account, password string, m mail.Message) (mail.Content, error) {
	out := mail.Content{Message: m, Attachments: []mail.Attachment{}}
	conn, e := adapter.open(ctx, a, password)
	if e != nil {
		return out, e
	}
	defer conn.close()
	status, e := conn.c.Select(m.Folder, true)
	if e != nil {
		return out, ErrRemote
	}
	if status.UidValidity != m.UIDValidity {
		return out, ErrStale
	}
	set := new(protocol.SeqSet)
	set.AddNum(m.UID)
	ch := make(chan *protocol.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- conn.c.UidFetch(set, []protocol.FetchItem{protocol.FetchUid, protocol.FetchFlags, protocol.FetchRFC822Size, protocol.FetchBodyStructure}, ch)
	}()
	var metadata *protocol.Message
	for v := range ch {
		metadata = v
	}
	if e = <-done; e != nil {
		return out, ErrRemote
	}
	if metadata == nil || hasFlag(metadata.Flags, protocol.DeletedFlag) {
		return out, ErrStale
	}
	parts := 0
	var structureErr error
	var attachments func(*protocol.BodyStructure, int)
	attachments = func(b *protocol.BodyStructure, depth int) {
		if b == nil || structureErr != nil {
			return
		}
		parts++
		if depth > 12 || parts > 128 {
			structureErr = errors.New("邮件分段超过限制")
			return
		}
		name, _ := b.Filename()
		if b.Disposition == "attachment" || name != "" {
			name, _ = mailmime.Truncate(name, 512)
			out.Attachments = append(out.Attachments, mail.Attachment{Name: name, Type: b.MIMEType + "/" + b.MIMESubType, Size: b.Size})
			return
		}
		for _, p := range b.Parts {
			attachments(p, depth+1)
		}
	}
	attachments(metadata.BodyStructure, 0)
	if structureErr != nil {
		return out, structureErr
	}
	if metadata.Size > mailmime.MaxRaw {
		out.Truncated = true
		out.Notice = "邮件超过 4 MiB，仅显示元数据和附件信息"
		return out, nil
	}
	section := &protocol.BodySectionName{Peek: true, Partial: []int{0, mailmime.MaxRaw + 1}}
	ch = make(chan *protocol.Message, 1)
	go func() { done <- conn.c.UidFetch(set, []protocol.FetchItem{protocol.FetchUid, section.FetchItem()}, ch) }()
	var raw []byte
	var readErr error
	for v := range ch {
		r := v.GetBody(section)
		if r == nil {
			readErr = ErrStale
			continue
		}
		raw, readErr = io.ReadAll(io.LimitReader(r, mailmime.MaxRaw+1))
	}
	if e = <-done; e != nil {
		return out, ErrRemote
	}
	if readErr != nil {
		return out, readErr
	}
	if raw == nil {
		return out, ErrStale
	}
	if len(raw) > mailmime.MaxRaw {
		out.Truncated = true
		out.Notice = "正文超过读取上限"
		return out, nil
	}
	out.Text, out.Truncated, e = mailmime.Parse(strings.NewReader(string(raw)))
	return out, e
}
