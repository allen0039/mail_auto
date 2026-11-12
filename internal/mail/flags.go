package mail

import (
	"context"
	"slices"
	"strings"
	"time"
)

// Flag actions change only \Seen or \Flagged on the original message. They are
// idempotent IMAP STOREs, so an interrupted attempt is reported as failed and a
// later run can safely apply it again; no reconciliation state is needed.
var flagActions = []string{"mark_read", "mark_unread", "flag", "unflag"}

// FlagChange is the desired state after a flag action. Nil means unchanged.
type FlagChange struct {
	Seen    *bool `json:"seen,omitempty"`
	Flagged *bool `json:"flagged,omitempty"`
}

func isFlagAction(action string) bool { return slices.Contains(flagActions, action) }

// permissionFor maps every flag action to the single "flag" grant.
func permissionFor(action string) string {
	if isFlagAction(action) {
		return "flag"
	}
	return action
}

func (c FlagChange) Empty() bool { return c.Seen == nil && c.Flagged == nil }

// Satisfied reports whether m already has the requested flags. Unknown flags
// are never treated as satisfied.
func (c FlagChange) Satisfied(m Message) bool {
	if !m.FlagsKnown {
		return false
	}
	if c.Seen != nil && *c.Seen == m.Unread {
		return false
	}
	if c.Flagged != nil && *c.Flagged != m.Flagged {
		return false
	}
	return true
}

func (c FlagChange) Apply(m Message) Message {
	if c.Seen != nil {
		m.Unread = !*c.Seen
	}
	if c.Flagged != nil {
		m.Flagged = *c.Flagged
	}
	return m
}

// flagChangeFor combines flag actions; it fails on duplicates or contradictions.
func flagChangeFor(actions ...string) (FlagChange, bool) {
	var c FlagChange
	set := func(dst **bool, v bool) bool {
		if *dst != nil {
			return false
		}
		*dst = &v
		return true
	}
	for _, a := range actions {
		ok := true
		switch a {
		case "mark_read":
			ok = set(&c.Seen, true)
		case "mark_unread":
			ok = set(&c.Seen, false)
		case "flag":
			ok = set(&c.Flagged, true)
		case "unflag":
			ok = set(&c.Flagged, false)
		default:
			ok = false
		}
		if !ok {
			return FlagChange{}, false
		}
	}
	return c, true
}

// validAlso checks the optional flag effects attached to a primary action.
func validAlso(action string, also []string) bool {
	if len(also) == 0 {
		return true
	}
	if len(also) > 2 || !(isFlagAction(action) || slices.Contains([]string{"move", "archive", "trash"}, action)) {
		return false
	}
	for _, a := range also {
		if !isFlagAction(a) {
			return false
		}
	}
	all := slices.Clone(also)
	if isFlagAction(action) {
		all = append(all, action)
	}
	_, ok := flagChangeFor(all...)
	return ok
}

func oppositeFlagAction(action string) string {
	return map[string]string{"mark_read": "mark_unread", "mark_unread": "mark_read", "flag": "unflag", "unflag": "flag"}[action]
}

// FlagRemote stores flags on one exact message. validate, when set, receives a
// fresh read-only metadata snapshot before any write on the same connection.
type FlagRemote interface {
	SetFlags(context.Context, Account, string, Message, FlagChange, func(Message) error) (Message, error)
}

// errFlagsNotApplied marks a completed move whose follow-up flag STORE failed.
var errFlagsNotApplied = &operationErrorCode{"flags_not_applied"}

// executeFlags stores flags on the exact source message and mirrors the
// confirmed remote state into the local index.
func (s *Service) executeFlags(ctx context.Context, id string, pos int, p Plan, item PlanItem, access Access, account Account, m Message) error {
	remote, ok := s.Remote.(FlagRemote)
	if !ok {
		return ErrCapability
	}
	pw, e := s.password(account)
	if e != nil {
		return e
	}
	var validate func(Message) error
	if p.Automation != nil {
		if e = s.sameAutomationDay(id); e != nil {
			return e
		}
		validate = func(fresh Message) error {
			fresh.Ref = m.Ref
			fresh.ChangedAt = m.ChangedAt
			if messageSignature(fresh) != p.Automation.Signature {
				return ErrConflict
			}
			_, _, err := s.validateAutomationPlan(access, p, fresh)
			return err
		}
	}
	fresh, e := remote.SetFlags(ctx, account, pw, m, item.Flags, validate)
	if e != nil {
		return e
	}
	return s.publishFlags(m, fresh)
}

// flagsAfterMove applies optional flag effects at the confirmed destination.
// The move is already published together with a provisional flags_not_applied
// warning (publishNormalMove); only a confirmed STORE whose result is indexed
// clears it. A failure here keeps the warning code.
func (s *Service) flagsAfterMove(ctx context.Context, id string, pos int, account Account, pw string, m Message, item PlanItem, result MoveResult) error {
	if item.Flags.Empty() {
		return nil
	}
	remote, ok := s.Remote.(FlagRemote)
	if !ok {
		return errFlagsNotApplied
	}
	moved := m
	moved.Folder, moved.UIDValidity, moved.UID = result.Folder, result.UIDValidity, result.UID
	fresh, e := remote.SetFlags(ctx, account, pw, moved, item.Flags, nil)
	if e != nil {
		return errFlagsNotApplied
	}
	if e = s.publishMovedFlags(id, pos, moved, fresh); e != nil {
		return errFlagsNotApplied
	}
	return nil
}

// publishMovedFlags indexes the confirmed flags of a moved message and clears
// the provisional warning of its operation item in one transaction, so a
// restart sees either the warning or the confirmed, indexed result.
func (s *Service) publishMovedFlags(id string, pos int, moved Message, fresh Message) error {
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	res, e := tx.Exec("UPDATE messages SET unread=?,flagged=?,flags_known=1,changed_at=? WHERE ref=? AND folder=? AND uidvalidity=? AND uid=?", fresh.Unread, fresh.Flagged, time.Now().Unix(), moved.Ref, moved.Folder, moved.UIDValidity, moved.UID)
	if e != nil {
		return e
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		// The index no longer holds the confirmed destination; keep the warning.
		return ErrConflict
	}
	if _, e = tx.Exec("UPDATE operation_items SET error_code='',detail='',recovery_code='',recovery_detail='',checked_at=? WHERE operation_id=? AND position=? AND state='completed' AND error_code='flags_not_applied'", time.Now().Unix(), id, pos); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	s.revision.Add(1)
	return nil
}

func (s *Service) publishFlags(m Message, fresh Message) error {
	_, e := s.Store.DB.Exec("UPDATE messages SET unread=?,flagged=?,flags_known=1,changed_at=? WHERE ref=? AND folder=? AND uidvalidity=? AND uid=?", fresh.Unread, fresh.Flagged, time.Now().Unix(), m.Ref, m.Folder, m.UIDValidity, m.UID)
	if e != nil {
		return e
	}
	s.revision.Add(1)
	return nil
}

// sameAutomationDay keeps queued automated writes from crossing the Shanghai day.
func (s *Service) sameAutomationDay(operationID string) error {
	var queuedAt int64
	if err := s.Store.DB.QueryRow("SELECT created_at FROM operations WHERE id=?", operationID).Scan(&queuedAt); err != nil {
		return err
	}
	if time.Unix(queuedAt, 0).In(automationLocation).Format("2006-01-02") != time.Now().In(automationLocation).Format("2006-01-02") {
		return ErrConflict
	}
	return nil
}

// describeFlags names the requested flag changes for user-facing warnings.
func describeFlags(c FlagChange) string {
	parts := []string{}
	if c.Seen != nil {
		parts = append(parts, map[bool]string{true: "标为已读", false: "标为未读"}[*c.Seen])
	}
	if c.Flagged != nil {
		parts = append(parts, map[bool]string{true: "加星标", false: "取消星标"}[*c.Flagged])
	}
	return strings.Join(parts, "、")
}

// flagsWarningKind says at which point a follow-up flag change stopped.
type flagsWarningKind int

const (
	// flagsPending is stored with the move itself and stays only if the
	// process stops before the flag change is confirmed and indexed.
	flagsPending flagsWarningKind = iota
	// flagsFailed: the flag STORE returned an error or its result could not
	// be recorded.
	flagsFailed
	// flagsRecovered: the move was confirmed by verification after the
	// original write was interrupted in the move phase; no flag write was sent.
	flagsRecovered
)

// flagsWarning is stored with flags_not_applied.
func flagsWarning(item PlanItem, kind flagsWarningKind) string {
	change := describeFlags(item.Flags)
	var msg string
	switch kind {
	case flagsPending:
		msg = "移动已完成，但附加的标记修改（" + change + "）未确认完成：处理在移动完成后、标记修改确认前中断，标记可能未应用，也可能已在服务器生效但未记录。恢复流程不会补写标记或重新移动。请在邮箱中核对并按需手动修改。"
	case flagsRecovered:
		msg = "移动已核验完成，但附加的标记修改（" + change + "）未执行：原写入在移动阶段中断，恢复流程只补完源邮件清理，不新增标记写入。请在邮箱中手动修改。"
	default:
		msg = "移动已完成，但附加的标记修改（" + change + "）未确认完成：标记写入失败或结果未确认，标记可能未应用，也可能已在服务器生效但未记录。请在邮箱中核对并按需手动修改。"
	}
	if item.Flags.Satisfied(item.Message) {
		msg += "计划时邮件已是目标状态，若此后未改动可忽略。"
	}
	return msg
}

// publishRecoveredMove rebinds a move confirmed by verification. Recovery
// deliberately stays limited to verifying the move and cleaning the original
// source UID, so planned follow-up flag changes, which the interrupted write
// never reached, are not attempted here. The item is completed with an
// explicit flags_not_applied warning instead of reporting silent success.
func (s *Service) publishRecoveredMove(id string, pos int, item PlanItem, result MoveResult) error {
	if item.Flags.Empty() {
		return s.publishMove(id, pos, item.Message, result)
	}
	return s.publishMoveWithWarning(id, pos, item.Message, result, "flags_not_applied", flagsWarning(item, flagsRecovered))
}
