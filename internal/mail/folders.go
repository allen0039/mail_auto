package mail

import "strings"

// INBOX is IMAP's reserved name. All other labels follow the mailbox verbatim.
// Display names never replace the actual IMAP folder identity used for reads.
func FolderLabel(folder string) string {
	if strings.EqualFold(folder, "INBOX") {
		return "收件箱"
	}
	return folder
}
