package mail

import (
	"context"
	"time"
)

type Account struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Provider         string   `json:"provider"`
	Username         string   `json:"username"`
	Host             string   `json:"host"`
	Port             int      `json:"port"`
	Folders          []string `json:"folders"`
	SyncedFolders    []string `json:"synced_folders"`
	SecretFile       string   `json:"secret_file,omitempty"`
	CredentialSource string   `json:"credential_source"`
	CreatedAt        int64    `json:"created_at"`
	Secret           []byte   `json:"-"`
}
type Message struct {
	Ref         string  `json:"ref"`
	AccountID   string  `json:"account_id"`
	Folder      string  `json:"folder"`
	FolderLabel string  `json:"folder_label"`
	UIDValidity uint32  `json:"-"`
	UID         uint32  `json:"-"`
	Subject     string  `json:"subject"`
	Sender      string  `json:"sender"`
	SentAt      int64   `json:"sent_at"`
	Unread      bool    `json:"unread"`
	Size        uint32  `json:"size"`
	Labels      []Label `json:"labels,omitempty"`
}
type Label struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color,omitempty"`
	CreatedAt int64  `json:"created_at"`
}
type Attachment struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size uint32 `json:"size"`
}
type Content struct {
	Message     Message      `json:"message"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments"`
	Truncated   bool         `json:"truncated"`
	Notice      string       `json:"notice,omitempty"`
}
type Snapshot struct {
	UIDValidity uint32
	Messages    []Message
}

// FolderState contains only identities and flags, never cached bodies.
type FolderState struct {
	UIDValidity uint32
	Unread      map[uint32]bool
}

// Inventory is authoritative only after the entire remote operation succeeds.
type Inventory struct {
	UIDValidity uint32
	Total       uint32 // Server EXISTS count, including messages marked deleted.
	UIDs        []uint32
	Unread      map[uint32]bool
}

type AutomaticRemote interface {
	OpenAutomatic(context.Context, Account, string) (AutomaticSession, error)
}

// One authenticated connection is reused for all folders and push waiting.
type AutomaticSession interface {
	Folders(context.Context) ([]string, error)
	Refresh(context.Context, string, FolderState, func(Snapshot) error) (Inventory, error)
	Wait(context.Context, string, time.Duration) (bool, error)
	PushSupported() bool
	Close()
}
type Mailbox struct {
	Name        string `json:"name"`
	UIDValidity uint32 `json:"uidvalidity"`
	SyncedAt    int64  `json:"synced_at"`
}
type Search struct {
	AccountID, Folder, Query, Sender string
	Unread                           bool
	Since, Before                    time.Time
	Offset, Limit                    int
	Label, WithoutLabel              string
	Folders                          []string
	ReadState                        string
	SenderExact, Domain              string
	MinSize, MaxSize                 int64
	Cursor                           string
}
