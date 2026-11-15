package imap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"slices"
	"strings"

	protocol "github.com/emersion/go-imap"
	"mail-auto/internal/mail"
	mailmime "mail-auto/internal/mime"
)

// VerifyDuplicate never opens a read-write mailbox. It checks unique live
// Message-ID matches, full raw bytes, and unchanged flags on the same session.
func (a Adapter) VerifyDuplicate(ctx context.Context, account mail.Account, password string, source, target mail.Message) (mail.DuplicateEvidence, error) {
	var out mail.DuplicateEvidence
	if source.AccountID != target.AccountID || source.Folder == target.Folder || source.MessageID == "" || source.MessageID != target.MessageID || source.Size <= 0 || source.Size != target.Size || source.Size > mailmime.MaxRaw {
		return out, mail.ErrConflict
	}
	conn, err := a.open(ctx, account, password)
	if err != nil {
		return out, err
	}
	defer conn.close()
	first, sourceFlags, err := duplicateDigest(conn, source)
	if err != nil {
		return out, err
	}
	second, targetFlags, err := duplicateDigest(conn, target)
	if err != nil {
		return out, err
	}
	if first != second || !slices.Equal(sourceFlags, targetFlags) {
		return out, mail.ErrConflict
	}
	// Check both again after reading their bodies, including mailbox epoch,
	// identity, metadata and flags. Concurrent remote changes fail closed.
	for i, m := range []mail.Message{source, target} {
		fresh, err := ruleStateOnConnection(conn, account.ID, m)
		if err != nil {
			return out, err
		}
		flags, err := duplicateFlags(conn, m)
		if err != nil {
			return out, err
		}
		expected := sourceFlags
		if i == 1 {
			expected = targetFlags
		}
		if !sameMoveMetadata(m, fresh) || fresh.MessageID != m.MessageID || !slices.Equal(flags, expected) {
			return out, mail.ErrConflict
		}
	}
	return mail.DuplicateEvidence{SHA256: first, Bytes: int64(source.Size), SourceFlags: sourceFlags, TargetFlags: targetFlags}, nil
}

func duplicateFlags(conn *connection, m mail.Message) ([]string, error) {
	set := new(protocol.SeqSet)
	set.AddNum(m.UID)
	ch := make(chan *protocol.Message, 1)
	done := make(chan error, 1)
	conn.resetBudget()
	go func() { done <- conn.c.UidFetch(set, []protocol.FetchItem{protocol.FetchUid, protocol.FetchFlags}, ch) }()
	var flags []string
	count := 0
	for v := range ch {
		if v != nil && v.Uid == m.UID {
			count++
			flags = append([]string{}, v.Flags...)
		}
	}
	if err := <-done; err != nil {
		return nil, remoteFailure(conn, "核对人工保留标记", err)
	}
	if count != 1 || hasFlag(flags, protocol.DeletedFlag) {
		return nil, mail.ErrConflict
	}
	slices.Sort(flags)
	return flags, nil
}

func duplicateDigest(conn *connection, m mail.Message) (string, []string, error) {
	fresh, err := ruleStateOnConnection(conn, m.AccountID, m)
	if err != nil {
		return "", nil, err
	}
	if !sameMoveMetadata(m, fresh) || fresh.MessageID != m.MessageID || fresh.Unread != m.Unread || fresh.Flagged != m.Flagged || !fresh.FlagsKnown {
		return "", nil, mail.ErrConflict
	}
	query := protocol.NewSearchCriteria()
	query.Header.Add("Message-ID", m.MessageID)
	conn.resetBudget()
	uids, err := conn.c.UidSearch(query)
	if err != nil {
		return "", nil, remoteFailure(conn, "查找人工核验副本", err)
	}
	if len(uids) != 1 || uids[0] != m.UID {
		return "", nil, mail.ErrConflict
	}
	section := &protocol.BodySectionName{Peek: true, Partial: []int{0, mailmime.MaxRaw + 1}}
	set := new(protocol.SeqSet)
	set.AddNum(m.UID)
	ch := make(chan *protocol.Message, 1)
	done := make(chan error, 1)
	conn.resetBudget()
	go func() {
		done <- conn.c.UidFetch(set, []protocol.FetchItem{protocol.FetchUid, protocol.FetchFlags, protocol.FetchRFC822Size, section.FetchItem()}, ch)
	}()
	var digest string
	var flags []string
	var readErr error
	count := 0
	for v := range ch {
		if v == nil {
			readErr = mail.ErrConflict
			continue
		}
		count++
		body := v.GetBody(section)
		if body == nil || v.Uid != m.UID || v.Size != m.Size || hasFlag(v.Flags, protocol.DeletedFlag) {
			readErr = mail.ErrConflict
			continue
		}
		h := sha256.New()
		n, e := io.Copy(h, io.LimitReader(body, mailmime.MaxRaw+1))
		if e != nil || n != int64(m.Size) {
			readErr = mail.ErrConflict
			continue
		}
		digest = hex.EncodeToString(h.Sum(nil))
		flags = append([]string{}, v.Flags...)
		slices.Sort(flags)
		if hasFlag(flags, protocol.SeenFlag) == m.Unread || hasFlag(flags, protocol.FlaggedFlag) != m.Flagged {
			readErr = mail.ErrConflict
		}
	}
	if err = <-done; err != nil {
		return "", nil, remoteFailure(conn, "只读核验完整原文", err)
	}
	if readErr != nil || count != 1 || strings.TrimSpace(digest) == "" {
		return "", nil, mail.ErrConflict
	}
	return digest, flags, nil
}
