package mail

import (
	"context"
	"database/sql"
	"time"

	"mail-auto/internal/credentials"
)

// publishStageSQL moves one staged folder snapshot into the index. Metadata-only
// refreshes (for example a version 2 re-read) intentionally do not touch
// changed_at, so they never restart automation stability windows.
const publishStageSQL = `INSERT INTO messages(` + messageColumns + `) SELECT ` + messageColumns + ` FROM sync_stage WHERE token=? ON CONFLICT(account_id,folder,uidvalidity,uid) DO UPDATE SET changed_at=CASE WHEN messages.subject<>excluded.subject OR messages.sender<>excluded.sender OR messages.unread<>excluded.unread OR messages.flagged<>excluded.flagged OR messages.flags_known<>excluded.flags_known OR messages.received_at<>excluded.received_at THEN excluded.changed_at ELSE messages.changed_at END,subject=excluded.subject,sender=excluded.sender,sent_at=excluded.sent_at,unread=excluded.unread,size=excluded.size,received_at=excluded.received_at,flagged=excluded.flagged,flags_known=excluded.flags_known,metadata_version=excluded.metadata_version,recipients=excluded.recipients,cc=excluded.cc,message_id=excluded.message_id,in_reply_to=excluded.in_reply_to,thread_refs=excluded.thread_refs,list_id=excluded.list_id,has_attachment=excluded.has_attachment`

func stageMessage(ctx context.Context, tx *sql.Tx, token, accountID, folder string, validity uint32, m Message) error {
	version := m.MetadataVersion
	if version == 0 {
		version = CurrentMetadataVersion
	}
	_, e := tx.ExecContext(ctx, "INSERT INTO sync_stage(token,"+messageColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		token, credentials.Random(24), accountID, folder, validity, m.UID, m.Subject, m.Sender, m.SentAt, m.Unread, m.Size,
		m.ReceivedAt, m.Flagged, m.FlagsKnown, time.Now().Unix(), version,
		m.Recipients, m.Cc, m.MessageID, m.InReplyTo, m.References, m.ListID, m.HasAttachment)
	return e
}

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	e := row.Scan(&m.Ref, &m.AccountID, &m.Folder, &m.UIDValidity, &m.UID, &m.Subject, &m.Sender, &m.SentAt, &m.Unread, &m.Size, &m.ReceivedAt, &m.Flagged, &m.FlagsKnown, &m.ChangedAt, &m.MetadataVersion,
		&m.Recipients, &m.Cc, &m.MessageID, &m.InReplyTo, &m.References, &m.ListID, &m.HasAttachment)
	m.FolderLabel = FolderLabel(m.Folder)
	return m, e
}
