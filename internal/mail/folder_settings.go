package mail

import (
	"context"
	"database/sql"
	"mail-auto/internal/store"
	"slices"
	"strings"
)

type FolderInfo struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Role  string `json:"role,omitempty"`
}
type Capabilities struct {
	Folders  []FolderInfo `json:"folders"`
	Move     bool         `json:"move"`
	UIDPlus  bool         `json:"uid_plus"`
	MoveMode string       `json:"move_mode"`
}
type MoveResult struct {
	Folder      string `json:"folder"`
	UIDValidity uint32 `json:"uidvalidity"`
	UID         uint32 `json:"uid"`
}
type WritableRemote interface {
	Capabilities(context.Context, Account, string) (Capabilities, error)
	Move(context.Context, Account, string, Message, string, func(MoveResult) error) (MoveResult, error)
	VerifyMove(context.Context, Account, string, Message, MoveResult) error
}
type FolderSettings struct {
	AccountID     string `json:"account_id"`
	Archive       string `json:"archive_folder"`
	Trash         string `json:"trash_folder"`
	WritesEnabled bool   `json:"writes_enabled"`
	Version       int    `json:"version"`
}

func (s *Service) FolderSettings(id string) (FolderSettings, error) {
	out := FolderSettings{AccountID: id}
	if _, e := s.Account(id); e != nil {
		return out, e
	}
	e := s.Store.DB.QueryRow("SELECT archive_folder,trash_folder,writes_enabled,version FROM folder_settings WHERE account_id=?", id).Scan(&out.Archive, &out.Trash, &out.WritesEnabled, &out.Version)
	if e == sql.ErrNoRows {
		e = nil
	}
	return out, e
}
func (s *Service) ProbeCapabilities(ctx context.Context, id string) (Capabilities, error) {
	r, ok := s.Remote.(WritableRemote)
	if !ok {
		return Capabilities{}, ErrCapability
	}
	release, e := s.acquire(id)
	if e != nil {
		return Capabilities{}, e
	}
	defer release()
	a, e := s.Account(id)
	if e != nil {
		return Capabilities{}, e
	}
	pw, e := s.password(a)
	if e != nil {
		return Capabilities{}, e
	}
	caps, e := r.Capabilities(ctx, a, pw)
	if e != nil {
		return caps, e
	}
	filtered := []FolderInfo{}
	for _, f := range caps.Folders {
		if len(a.Folders) == 0 || slices.Contains(a.Folders, f.Name) {
			filtered = append(filtered, f)
		}
	}
	caps.Folders = filtered
	for _, f := range filtered {
		if _, err := s.Store.DB.Exec("INSERT OR IGNORE INTO mailboxes(account_id,name,uidvalidity,synced_at) VALUES(?,?,0,0)", id, f.Name); err != nil {
			return caps, err
		}
	}
	if caps.UIDPlus {
		caps.MoveMode = "uidplus_copy"
		if caps.Move {
			caps.MoveMode = "uid_move"
		}
	} else {
		caps.MoveMode = "unsupported"
	}
	return caps, nil
}
func (s *Service) SaveFolderSettings(ctx context.Context, in FolderSettings) error {
	if in.Archive == in.Trash && in.Archive != "" {
		return ErrInput
	}
	for _, f := range []string{in.Archive, in.Trash} {
		if len(f) > 256 || strings.ContainsAny(f, "\x00\r\n") {
			return ErrInput
		}
	}
	release, e := s.acquire(in.AccountID)
	if e != nil {
		return e
	}
	defer release()
	a, e := s.Account(in.AccountID)
	if e != nil {
		return e
	}
	if in.WritesEnabled {
		pw, e := s.password(a)
		if e != nil {
			return e
		}
		r, ok := s.Remote.(WritableRemote)
		if !ok {
			return ErrCapability
		}
		caps, e := r.Capabilities(ctx, a, pw)
		if e != nil {
			return e
		}
		for _, f := range []string{in.Archive, in.Trash} {
			if f == "" {
				continue
			}
			exists := false
			for _, actual := range caps.Folders {
				if actual.Name == f {
					exists = true
				}
			}
			if !exists || (len(a.Folders) > 0 && !slices.Contains(a.Folders, f)) {
				return ErrInput
			}
		}
		if in.WritesEnabled && !caps.UIDPlus {
			return ErrCapability
		}
	}
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	_, e = tx.Exec(`INSERT INTO folder_settings(account_id,archive_folder,trash_folder,writes_enabled) VALUES(?,?,?,?) ON CONFLICT(account_id) DO UPDATE SET archive_folder=excluded.archive_folder,trash_folder=excluded.trash_folder,writes_enabled=excluded.writes_enabled,version=version+1`, in.AccountID, in.Archive, in.Trash, in.WritesEnabled)
	if e != nil {
		return e
	}
	if e = store.Audit(tx, "mail.folder_settings", in.AccountID, "ok"); e != nil {
		return e
	}
	return tx.Commit()
}

var ErrCapability = &operationErrorCode{"capability_unavailable"}
var ErrUncertain = &operationErrorCode{"remote_state_unknown"}

type operationErrorCode struct{ code string }

func (e *operationErrorCode) Error() string { return e.code }
