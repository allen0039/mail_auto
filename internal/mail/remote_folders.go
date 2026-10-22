package mail

import (
	"context"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"mail-auto/internal/store"
)

type FolderCreator interface {
	CreateFolder(context.Context, Account, string, string) error
}

func ValidFolderName(name string) bool {
	return name != "" && name == strings.TrimSpace(name) && len(name) <= 256 && utf8.ValidString(name) && strings.IndexFunc(name, unicode.IsControl) < 0 && !strings.EqualFold(name, "INBOX")
}

// CreateRemoteFolder creates a real provider mailbox, never a local category.
func (s *Service) CreateRemoteFolder(ctx context.Context, access Access, id, name string) (FolderInfo, error) {
	access, e := s.CheckAccess(access)
	if e != nil {
		return FolderInfo{}, e
	}
	if !ValidFolderName(name) {
		return FolderInfo{}, ErrInput
	}
	if !access.Allows(id, name, "folder_manage") {
		return FolderInfo{}, ErrDenied
	}
	release, e := s.acquire(id)
	if e != nil {
		return FolderInfo{}, e
	}
	defer release()
	account, e := s.Account(id)
	if e != nil {
		return FolderInfo{}, e
	}
	if len(account.Folders) > 0 && !slices.Contains(account.Folders, name) {
		return FolderInfo{}, ErrDenied
	}
	settings, e := s.FolderSettings(id)
	if e != nil {
		return FolderInfo{}, e
	}
	if !settings.WritesEnabled {
		return FolderInfo{}, ErrDenied
	}
	remote, ok := s.Remote.(FolderCreator)
	if !ok {
		return FolderInfo{}, ErrCapability
	}
	pw, e := s.password(account)
	if e != nil {
		return FolderInfo{}, e
	}
	if e = remote.CreateFolder(ctx, account, pw, name); e != nil {
		return FolderInfo{}, e
	}
	// Record only a confirmed remote folder. The next sync obtains its UIDVALIDITY.
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return FolderInfo{}, ErrUncertain
	}
	defer tx.Rollback()
	if _, e = tx.Exec("INSERT OR IGNORE INTO mailboxes(account_id,name,uidvalidity,synced_at) VALUES(?,?,0,0)", id, name); e != nil {
		return FolderInfo{}, ErrUncertain
	}
	if e = store.Audit(tx, "mail.folder.create", id, "ok"); e != nil {
		return FolderInfo{}, ErrUncertain
	}
	if e = tx.Commit(); e != nil {
		return FolderInfo{}, ErrUncertain
	}
	s.revision.Add(1)
	s.wakeAutomatic()
	return FolderInfo{Name: name, Label: FolderLabel(name)}, nil
}
