package mail

import (
	"database/sql"
	"slices"
)

func logicalKey(message Message) string {
	if message.GmailMessageID != "" {
		return message.AccountID + ":gmail:" + message.GmailMessageID
	}
	return message.Ref
}

func (service *Service) gmailAction(account Account, message Message, action, destination string, settings FolderSettings) error {
	if account.Provider == "google" && message.GmailMessageID == "" {
		return blocked("gmail_identity_missing", "Gmail 稳定消息身份尚未同步，请先同步后再整理", ErrCapability)
	}
	return service.gmailFolderAction(account, message.Folder, action, destination, settings)
}

func (service *Service) gmailFolderAction(account Account, folder, action, destination string, settings FolderSettings) error {
	if account.Provider != "google" {
		return nil
	}
	if action == "trash" || action == "restore" || (action == "archive" && folder != "INBOX") || (action == "move" && (destination == settings.Trash || destination == settings.Archive || folder == settings.Trash || folder == settings.Archive)) {
		return blocked("gmail_action_unsupported", "Gmail 当前支持标记、普通标签之间分类及从 INBOX 归档；回收站、恢复、特殊目录移动和保留两份尚未验证，已安全拒绝", ErrCapability)
	}
	if action == "move" || action == "archive" {
		if action == "archive" {
			destination = settings.Archive
		}
		var sourceRole, targetRole string
		if err := service.Store.DB.QueryRow("SELECT role FROM mail_oauth_folder_roles WHERE account_id=? AND name=?", account.ID, folder).Scan(&sourceRole); err != nil {
			return blocked("gmail_folder_roles_missing", "请先在整理设置中探测 Gmail SPECIAL-USE 目录，再预览或执行", ErrCapability)
		}
		if err := service.Store.DB.QueryRow("SELECT role FROM mail_oauth_folder_roles WHERE account_id=? AND name=?", account.ID, destination).Scan(&targetRole); err != nil {
			return blocked("gmail_folder_roles_missing", "Gmail 目标目录能力尚未探测，请重新探测文件夹", ErrCapability)
		}
		if (sourceRole != "" && sourceRole != "inbox") || (action == "move" && targetRole != "" && targetRole != "inbox") || (action == "archive" && targetRole != "archive") {
			return blocked("gmail_action_unsupported", "Gmail 特殊目录不支持该移动；仅普通标签分类和 INBOX 归档可用", ErrCapability)
		}
	}
	return nil
}

func (service *Service) saveGmailFolderRoles(account Account, folders []FolderInfo) error {
	if account.Provider != "google" {
		return nil
	}
	tx, err := service.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = service.checkSnapshotIn(tx, account); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM mail_oauth_folder_roles WHERE account_id=?", account.ID); err != nil {
		return err
	}
	for _, folder := range folders {
		role := folder.Role
		if folder.Name == "INBOX" {
			role = "inbox"
		}
		if _, err = tx.Exec("INSERT INTO mail_oauth_folder_roles(account_id,name,role) VALUES(?,?,?)", account.ID, folder.Name, role); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (service *Service) gmailMembers(message Message) ([]Message, error) {
	if message.GmailMessageID == "" {
		return []Message{message}, nil
	}
	rows, err := service.Store.DB.Query("SELECT "+messageColumns+" FROM messages WHERE account_id=? AND gmail_message_id=? ORDER BY folder,ref", message.AccountID, message.GmailMessageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []Message
	for rows.Next() {
		member, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		if member.Ref == message.Ref {
			member = message
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

func (service *Service) evaluateMessageRules(message Message, rules []Rule, policy AutomationPolicy, now int64) (RuleEvaluation, error) {
	if message.GmailMessageID == "" {
		return evaluateRules(message, rules, policy, now), nil
	}
	members, err := service.gmailMembers(message)
	if err != nil {
		return RuleEvaluation{}, err
	}
	for _, member := range members {
		if protected := policyProtection(policy, member, now); protected != "" {
			return RuleEvaluation{Message: message, Signature: messageSignature(message), Status: "protected", Action: "keep", Reasons: []string{"Gmail 其他标签视图受保护：" + protected}}, nil
		}
	}
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		for _, member := range members {
			if !slices.Contains(rule.SourceFolders, member.Folder) || !slices.Contains(policy.SourceFolders, member.Folder) {
				continue
			}
			evaluation := evaluateRules(member, []Rule{rule}, policy, now)
			if evaluation.Status != "unmatched" {
				return evaluation, nil
			}
		}
	}
	return evaluateRules(message, nil, policy, now), nil
}

func logicalPending(query interface{ QueryRow(string, ...any) *sql.Row }, message Message) (int, error) {
	var pending int
	err := query.QueryRow(`SELECT count(*) FROM operation_items i WHERE i.state IN ('queued','running','needs_verification') AND (i.ref=? OR (?<>'' AND i.ref IN (SELECT ref FROM messages WHERE account_id=? AND gmail_message_id=?)))`, message.Ref, message.GmailMessageID, message.AccountID, message.GmailMessageID).Scan(&pending)
	return pending, err
}
