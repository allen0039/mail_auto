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
	conn, e := a.open(ctx, account, password)
	if e != nil {
		return mail.Message{}, e
	}
	defer conn.close()
	status, e := conn.c.Select(original.Folder, true)
	if e != nil {
		return mail.Message{}, remoteFailure(conn, "复核整理邮件", e)
	}
	if status.UidValidity != original.UIDValidity {
		return mail.Message{}, mail.ErrConflict
	}
	set := new(protocol.SeqSet)
	set.AddNum(original.UID)
	batch, e := fetchBatch(conn, account.ID, original.Folder, status.UidValidity, set)
	if e != nil {
		return mail.Message{}, e
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

// Prefer native UID MOVE; UIDPLUS fallback expunges only the confirmed source UID.
// Never use Client.UidMove, which can fall back to a global EXPUNGE.
func (a Adapter) Move(ctx context.Context, account mail.Account, password string, m mail.Message, dest string, record func(mail.MoveResult) error) (mail.MoveResult, error) {
	conn, e := a.open(ctx, account, password)
	if e != nil {
		return mail.MoveResult{}, e
	}
	defer conn.close()
	native, e := conn.c.Support("MOVE")
	if e != nil {
		return mail.MoveResult{}, ErrRemote
	}
	uidplus, e := conn.c.Support("UIDPLUS")
	if e != nil || !uidplus {
		return mail.MoveResult{}, mail.ErrCapability
	}
	selected, e := conn.c.Select(m.Folder, false)
	if e != nil {
		return mail.MoveResult{}, ErrRemote
	}
	if selected.ReadOnly {
		return mail.MoveResult{}, mail.ErrCapability
	}
	if selected.UidValidity != m.UIDValidity {
		return mail.MoveResult{}, ErrStale
	}
	set := new(protocol.SeqSet)
	set.AddNum(m.UID)
	criteria := liveMessages()
	criteria.Uid = set
	uids, e := conn.c.UidSearch(criteria)
	if e != nil {
		return mail.MoveResult{}, ErrRemote
	}
	if len(uids) != 1 || uids[0] != m.UID {
		return mail.MoveResult{}, ErrStale
	}
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
			return mapping, mail.ErrUncertain
		}
	}
	// Any error after issuing MOVE is uncertain: tagged NO can include partial effects.
	if e != nil || status == nil || status.Err() != nil || !mapped {
		return mapping, mail.ErrUncertain
	}
	if !native {
		// The destination must be visible at the exact returned UID before removing
		// the source. Failure at any phase is never automatically retried.
		if e = a.verifyDestination(ctx, account, password, mapping); e != nil {
			return mapping, mail.ErrUncertain
		}
		conn.resetBudget()
		if e = conn.c.UidStore(set, protocol.FormatFlagsOp(protocol.AddFlags, true), []interface{}{protocol.DeletedFlag}, nil); e != nil {
			return mapping, mail.ErrUncertain
		}
		conn.resetBudget()
		st, err := conn.c.Execute(&commands.Uid{Cmd: &protocol.Command{Name: "EXPUNGE", Arguments: []interface{}{set}}}, nil)
		if err != nil || st == nil || st.Err() != nil {
			return mapping, mail.ErrUncertain
		}
	}
	if e = a.VerifyMove(ctx, account, password, m, mapping); e != nil {
		return mapping, mail.ErrUncertain
	}
	return mapping, nil
}
func (a Adapter) VerifyMove(ctx context.Context, account mail.Account, password string, m mail.Message, result mail.MoveResult) error {
	if result.UID == 0 || result.UIDValidity == 0 || result.Folder == "" {
		return mail.ErrUncertain
	}
	conn, e := a.open(ctx, account, password)
	if e != nil {
		return mail.ErrUncertain
	}
	defer conn.close()
	for _, check := range []struct {
		folder        string
		validity, uid uint32
		present       bool
	}{{m.Folder, m.UIDValidity, m.UID, false}, {result.Folder, result.UIDValidity, result.UID, true}} {
		conn.resetBudget()
		st, e := conn.c.Select(check.folder, true)
		if e != nil || st.UidValidity != check.validity {
			return mail.ErrUncertain
		}
		set := new(protocol.SeqSet)
		set.AddNum(check.uid)
		q := liveMessages()
		if !check.present {
			q = protocol.NewSearchCriteria()
		}
		q.Uid = set
		uids, e := conn.c.UidSearch(q)
		if e != nil || (len(uids) > 0) != check.present {
			return mail.ErrUncertain
		}
	}
	return nil
}

func (a Adapter) verifyDestination(ctx context.Context, account mail.Account, password string, r mail.MoveResult) error {
	c, e := a.open(ctx, account, password)
	if e != nil {
		return mail.ErrUncertain
	}
	defer c.close()
	c.resetBudget()
	st, e := c.c.Select(r.Folder, true)
	if e != nil || st.UidValidity != r.UIDValidity {
		return mail.ErrUncertain
	}
	set := new(protocol.SeqSet)
	set.AddNum(r.UID)
	q := liveMessages()
	q.Uid = set
	uids, e := c.c.UidSearch(q)
	if e != nil || len(uids) != 1 || uids[0] != r.UID {
		return mail.ErrUncertain
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
