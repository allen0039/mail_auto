package mail

import (
	"context"
	"mail-auto/internal/network"
	"time"
)

type Account struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Provider         string         `json:"provider"`
	Username         string         `json:"username"`
	Host             string         `json:"host"`
	Port             int            `json:"port"`
	Folders          []string       `json:"folders"`
	SyncedFolders    []string       `json:"synced_folders"`
	SecretFile       string         `json:"secret_file,omitempty"`
	CredentialSource string         `json:"credential_source"`
	CreatedAt        int64          `json:"created_at"`
	Secret           []byte         `json:"-"`
	Proxy            network.Policy `json:"proxy"`
	NetworkRevision  string         `json:"network_revision"`
	NetworkLabel     string         `json:"network_label"`
}
type Message struct {
	Ref             string  `json:"ref"`
	AccountID       string  `json:"account_id"`
	Folder          string  `json:"folder"`
	FolderLabel     string  `json:"folder_label"`
	UIDValidity     uint32  `json:"-"`
	UID             uint32  `json:"-"`
	Subject         string  `json:"subject"`
	Sender          string  `json:"sender"`
	SentAt          int64   `json:"sent_at"`
	ReceivedAt      int64   `json:"received_at"`
	Flagged         bool    `json:"flagged"`
	FlagsKnown      bool    `json:"flags_known"`
	ChangedAt       int64   `json:"changed_at"`
	MetadataVersion int     `json:"-"`
	Unread          bool    `json:"unread"`
	Size            uint32  `json:"size"`
	Labels          []Label `json:"labels,omitempty"`
	// Metadata version 2 fields. Older rows keep empty values until the
	// automatic sync refreshes them; rules treat them as unknown until then.
	Recipients    string `json:"recipients,omitempty"`
	Cc            string `json:"cc,omitempty"`
	MessageID     string `json:"message_id,omitempty"`
	InReplyTo     string `json:"in_reply_to,omitempty"`
	References    string `json:"references,omitempty"`
	ListID        string `json:"list_id,omitempty"`
	HasAttachment bool   `json:"has_attachment"`
}

// CurrentMetadataVersion is the newest metadata layout written by remote reads.
const CurrentMetadataVersion = 2

// messageColumns lists the messages table in scanMessage order. Never use
// SELECT * because migrations append columns.
const messageColumns = "ref,account_id,folder,uidvalidity,uid,subject,sender,sent_at,unread,size,received_at,flagged,flags_known,changed_at,metadata_version,recipients,cc,message_id,in_reply_to,thread_refs,list_id,has_attachment"

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
	Flagged     map[uint32]bool
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
