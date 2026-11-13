// Package interaction holds the persistence contract shared by the agent
// runtime and the Telegram transport: bindings, conversation epochs and the
// reply outbox. Neither side imports the other.
package interaction

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"mail-auto/internal/credentials"
)

// Binding is a verified Telegram private chat bound to an agent grant.
type Binding struct {
	ID           string `json:"id"`
	BotID        int64  `json:"bot_id"`
	UserID       int64  `json:"user_id"`
	ChatID       int64  `json:"chat_id"`
	GrantID      string `json:"grant_id"`
	AdminVersion int    `json:"-"`
	Epoch        int64  `json:"epoch"`
	State        string `json:"state"`
	CreatedAt    int64  `json:"created_at"`
}

type Querier interface {
	QueryRow(string, ...any) *sql.Row
	Exec(string, ...any) (sql.Result, error)
}

const bindingColumns = "id,bot_id,user_id,chat_id,grant_id,admin_version,epoch,state,created_at"

func scanBinding(row *sql.Row) (Binding, error) {
	var b Binding
	err := row.Scan(&b.ID, &b.BotID, &b.UserID, &b.ChatID, &b.GrantID, &b.AdminVersion, &b.Epoch, &b.State, &b.CreatedAt)
	return b, err
}

// ActiveBinding returns the single active binding or sql.ErrNoRows.
func ActiveBinding(q Querier) (Binding, error) {
	return scanBinding(q.QueryRow("SELECT " + bindingColumns + " FROM telegram_bindings WHERE state='active'"))
}

// BindingByID returns a binding in any state.
func BindingByID(q Querier, id string) (Binding, error) {
	return scanBinding(q.QueryRow("SELECT "+bindingColumns+" FROM telegram_bindings WHERE id=?", id))
}

// Conversation is the agent session of a binding.
type Conversation struct {
	ID           string
	BindingID    string
	GrantVersion int
	ContextEpoch int64
	IntentEpoch  int64
	StopEpoch    int64
}

// EnsureConversation returns the binding's conversation, creating it.
func EnsureConversation(q Querier, bindingID string, grantVersion int) (Conversation, error) {
	now := time.Now().Unix()
	if _, err := q.Exec("INSERT OR IGNORE INTO agent_conversations(id,binding_id,grant_version,created_at,updated_at) VALUES(?,?,?,?,?)", credentials.Random(18), bindingID, grantVersion, now, now); err != nil {
		return Conversation{}, err
	}
	return ConversationOf(q, bindingID)
}

func ConversationOf(q Querier, bindingID string) (Conversation, error) {
	var c Conversation
	err := q.QueryRow("SELECT id,binding_id,grant_version,context_epoch,intent_epoch,stop_epoch FROM agent_conversations WHERE binding_id=?", bindingID).Scan(&c.ID, &c.BindingID, &c.GrantVersion, &c.ContextEpoch, &c.IntentEpoch, &c.StopEpoch)
	return c, err
}

// Button is an inline keyboard button. Data must be at most 64 bytes.
type Button struct {
	Text string `json:"text"`
	Data string `json:"data"`
}

// MaxPartUnits keeps each message well below Telegram's 4096 limit.
const MaxPartUnits = 3500

// SplitText splits on rune boundaries so that each part has at most max
// UTF-16 code units, preferring line breaks. Surrogate pairs are never split.
func SplitText(text string, max int) []string {
	text = strings.ToValidUTF8(text, "�")
	if text == "" {
		return []string{""}
	}
	parts := []string{}
	for len(text) > 0 {
		units, cut, lastNewline := 0, 0, -1
		for i, r := range text {
			n := len(utf16.Encode([]rune{r}))
			if units+n > max {
				break
			}
			units += n
			cut = i + utf8.RuneLen(r)
			if r == '\n' {
				lastNewline = cut
			}
		}
		if cut < len(text) && lastNewline > len(text[:cut])/2 {
			cut = lastNewline
		}
		parts = append(parts, text[:cut])
		text = text[cut:]
	}
	return parts
}

// Reply is a queued Telegram reply.
type Reply struct {
	SemanticKey   string
	Text          string
	Buttons       []Button
	EditMessageID int64
	ContextEpoch  int64
}

type replyPayload struct {
	Text    string   `json:"text"`
	Buttons []Button `json:"buttons,omitempty"`
}

// Enqueue stores a reply for the binding's private chat. Each part is unique
// by (semantic key, part), so re-enqueueing after a crash is a no-op and a
// delivery retry never re-runs business work. Buttons go on the last part.
func Enqueue(q Querier, vault *credentials.Vault, b Binding, r Reply) error {
	if r.SemanticKey == "" || len(r.SemanticKey) > 200 {
		return errors.New("invalid semantic key")
	}
	for _, btn := range r.Buttons {
		if btn.Data == "" || len(btn.Data) > 64 || btn.Text == "" {
			return errors.New("invalid button")
		}
	}
	parts := SplitText(r.Text, MaxPartUnits)
	if r.EditMessageID != 0 && len(parts) > 1 {
		parts = parts[:1]
	}
	now := time.Now().Unix()
	for i, text := range parts {
		p := replyPayload{Text: text}
		if i == len(parts)-1 {
			p.Buttons = r.Buttons
		}
		raw, _ := json.Marshal(p)
		id := credentials.Random(18)
		sealed, err := vault.Encrypt(string(raw), "tg-reply:"+id)
		if err != nil {
			return err
		}
		if _, err = q.Exec("INSERT OR IGNORE INTO telegram_reply_outbox(id,semantic_key,part,bot_id,chat_id,binding_id,grant_id,admin_version,epoch,context_epoch,payload,edit_message_id,state,next_attempt,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'pending',?,?,?)", id, r.SemanticKey, i, b.BotID, b.ChatID, b.ID, b.GrantID, b.AdminVersion, b.Epoch, r.ContextEpoch, sealed, r.EditMessageID, now, now, now); err != nil {
			return err
		}
	}
	return nil
}

// DecodeReply opens a stored reply payload.
func DecodeReply(vault *credentials.Vault, id string, sealed []byte) (string, []Button, error) {
	plain, err := vault.Decrypt(sealed, "tg-reply:"+id)
	if err != nil {
		return "", nil, err
	}
	var p replyPayload
	if err = json.Unmarshal([]byte(plain), &p); err != nil {
		return "", nil, err
	}
	return p.Text, p.Buttons, nil
}
