package imap

import (
	"context"
	protocol "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-imap/responses"
	"github.com/emersion/go-imap/utf7"
	"mail-auto/internal/mail"
	"strings"
)

func gmailFolders(conn *connection) (map[string]string, error) {
	conn.resetBudget()
	channel := make(chan *protocol.MailboxInfo, 16)
	done := make(chan error, 1)
	go func() { done <- conn.c.List("", "*", channel) }()
	folders := map[string]string{}
	for folder := range channel {
		role := "label"
		for _, attr := range folder.Attributes {
			if strings.HasPrefix(attr, "\\") && !strings.EqualFold(attr, "\\HasNoChildren") && !strings.EqualFold(attr, "\\HasChildren") && !strings.EqualFold(attr, "\\Subscribed") {
				role = strings.ToLower(attr)
			}
		}
		if strings.EqualFold(folder.Name, "INBOX") {
			role = "inbox"
		}
		folders[folder.Name] = role
	}
	if err := <-done; err != nil {
		return nil, remoteFailure(conn, "读取 Gmail 标签能力", err)
	}
	return folders, nil
}

func gmailLocation(conn *connection, folder, messageID string) (mail.MoveResult, bool, error) {
	if gmailID(messageID) == "" {
		return mail.MoveResult{}, false, mail.ErrCapability
	}
	conn.resetBudget()
	selected, err := conn.c.Select(folder, true)
	if err != nil {
		return mail.MoveResult{}, false, remoteFailure(conn, "打开 Gmail 标签", err)
	}
	if selected.UidValidity == 0 {
		return mail.MoveResult{}, false, mail.ErrUncertain
	}
	conn.resetBudget()
	response := &responses.Search{}
	status, err := conn.c.Execute(&commands.Uid{Cmd: &protocol.Command{Name: "SEARCH", Arguments: []interface{}{protocol.RawString("X-GM-MSGID"), protocol.RawString(messageID)}}}, response)
	if err != nil {
		return mail.MoveResult{}, false, remoteFailure(conn, "核验 Gmail 消息成员关系", err)
	}
	if status == nil || status.Err() != nil || len(response.Ids) > 1 {
		return mail.MoveResult{}, false, mail.ErrUncertain
	}
	if len(response.Ids) == 0 {
		return mail.MoveResult{}, false, nil
	}
	if response.Ids[0] == 0 {
		return mail.MoveResult{}, false, mail.ErrUncertain
	}
	return mail.MoveResult{Folder: folder, UIDValidity: selected.UidValidity, UID: response.Ids[0]}, true, nil
}

func gmailStoreLabel(conn *connection, uid uint32, operation, label string) error {
	encoded, err := utf7.Encoding.NewEncoder().String(label)
	if err != nil {
		return mail.ErrInput
	}
	set := new(protocol.SeqSet)
	set.AddNum(uid)
	conn.resetBudget()
	status, err := conn.c.Execute(&commands.Uid{Cmd: &protocol.Command{Name: "STORE", Arguments: []interface{}{set, protocol.RawString(operation + "X-GM-LABELS.SILENT"), []interface{}{encoded}}}}, nil)
	if err != nil {
		return uncertainFailure(conn, "更新 Gmail 标签", err)
	}
	if status == nil || status.Err() != nil {
		return uncertainProblem("更新 Gmail 标签", "服务商未确认标签更新；请核验，不能重放")
	}
	return nil
}

func (adapter Adapter) gmailMove(ctx context.Context, account mail.Account, password string, message mail.Message, destination string, validate func(mail.Message) error, record func(mail.MoveResult) error) (mail.MoveResult, error) {
	if message.GmailMessageID == "" {
		return mail.MoveResult{}, mail.ErrCapability
	}
	conn, err := adapter.open(ctx, account, password)
	if err != nil {
		return mail.MoveResult{}, err
	}
	defer conn.close()
	folders, err := gmailFolders(conn)
	if err != nil {
		return mail.MoveResult{}, err
	}
	sourceRole, targetRole := folders[message.Folder], folders[destination]
	archive := targetRole == "\\all" || targetRole == "\\archive"
	if (sourceRole != "label" && sourceRole != "inbox") || (targetRole != "label" && targetRole != "inbox" && !archive) || (archive && sourceRole != "inbox") || message.Folder == destination {
		return mail.MoveResult{}, mail.ErrCapability
	}
	fresh, err := ruleStateOnConnection(conn, account.ID, message)
	if err != nil {
		return mail.MoveResult{}, err
	}
	if fresh.GmailMessageID != message.GmailMessageID {
		return mail.MoveResult{}, mail.ErrConflict
	}
	if validate != nil {
		if err = validate(fresh); err != nil {
			return mail.MoveResult{}, err
		}
	}
	conn.resetBudget()
	selected, err := conn.c.Select(message.Folder, false)
	if err != nil {
		return mail.MoveResult{}, remoteFailure(conn, "打开 Gmail 源标签", err)
	}
	if selected.ReadOnly || selected.UidValidity != message.UIDValidity {
		return mail.MoveResult{}, mail.ErrConflict
	}
	if !archive {
		label := destination
		if targetRole == "inbox" {
			label = "\\Inbox"
		}
		if err = gmailStoreLabel(conn, message.UID, "+", label); err != nil {
			return mail.MoveResult{}, err
		}
		if validate != nil {
			fresh, err := ruleStateOnConnection(conn, account.ID, message)
			if err != nil {
				return mail.MoveResult{}, mail.ErrUncertain
			}
			if err = validate(fresh); err != nil {
				return mail.MoveResult{}, mail.ErrUncertain
			}
			conn.resetBudget()
			selected, err := conn.c.Select(message.Folder, false)
			if err != nil || selected.ReadOnly || selected.UidValidity != message.UIDValidity {
				return mail.MoveResult{}, mail.ErrUncertain
			}
		}
	}
	result, present, err := gmailLocation(conn, destination, message.GmailMessageID)
	if err != nil || !present {
		return result, mail.ErrUncertain
	}
	conn.resetBudget()
	selected, err = conn.c.Select(destination, false)
	if err != nil || selected.ReadOnly || selected.UidValidity != result.UIDValidity {
		return result, mail.ErrUncertain
	}
	label := message.Folder
	if sourceRole == "inbox" {
		label = "\\Inbox"
	}
	if err = gmailStoreLabel(conn, result.UID, "-", label); err != nil {
		return mail.MoveResult{}, err
	}
	result, present, err = gmailLocation(conn, destination, message.GmailMessageID)
	if err != nil || !present {
		return result, mail.ErrUncertain
	}
	if err = record(result); err != nil {
		return result, mail.ErrUncertain
	}
	_, sourcePresent, err := gmailLocation(conn, message.Folder, message.GmailMessageID)
	if err != nil || sourcePresent {
		return result, mail.ErrUncertain
	}
	return result, nil
}

func (adapter Adapter) gmailInspect(ctx context.Context, account mail.Account, password string, message mail.Message, destination string) (mail.MoveEvidence, error) {
	var evidence mail.MoveEvidence
	conn, err := adapter.open(ctx, account, password)
	if err != nil {
		return evidence, err
	}
	defer conn.close()
	_, evidence.SourcePresent, err = gmailLocation(conn, message.Folder, message.GmailMessageID)
	if err != nil {
		return evidence, err
	}
	evidence.Destination, evidence.DestinationPresent, err = gmailLocation(conn, destination, message.GmailMessageID)
	return evidence, err
}
