// Package telegram implements the interactive Telegram transport: Bot API
// client, binding, single-consumer long polling into a durable inbox,
// deterministic commands, callbacks and the reply outbox.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"mail-auto/internal/interaction"
	"mail-auto/internal/tglimit"
)

// Fixed error codes. Raw Bot API errors may contain the token URL and are
// never logged or returned.
const (
	CodeNetwork     = "telegram_network_error"
	CodeRateLimited = "telegram_rate_limited"
	CodeTokenBad    = "telegram_token_invalid"
	CodeForbidden   = "telegram_access_denied"
	CodeBadRequest  = "telegram_request_invalid"
	CodeConflict    = "telegram_consumer_conflict"
	CodeUnavailable = "telegram_service_unavailable"
	CodeInvalid     = "telegram_invalid_response"
	CodeUnknown     = "delivery_unknown"
	CodeNotModified = "telegram_not_modified"
)

type APIError struct {
	Code       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string { return e.Code }

func codeOf(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return CodeNetwork
}

// Client calls the Bot API. BaseURL is https://api.telegram.org in
// production; tests point it at a fake server.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewClient(token string) *Client {
	return &Client{BaseURL: "https://api.telegram.org", Token: token, HTTP: &http.Client{Timeout: 40 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type envelope struct {
	OK          bool            `json:"ok"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (c *Client) call(ctx context.Context, method string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return &APIError{Code: CodeBadRequest}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/bot"+c.Token+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return &APIError{Code: CodeBadRequest}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The *url.Error carries the token URL; only a fixed code escapes.
		if ctx.Err() != nil {
			return &APIError{Code: CodeUnknown}
		}
		return &APIError{Code: CodeNetwork}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return &APIError{Code: CodeUnknown}
	}
	var env envelope
	decodeErr := json.Unmarshal(data, &env)
	status := resp.StatusCode
	if env.ErrorCode != 0 {
		status = env.ErrorCode
	}
	switch {
	case status == 429:
		wait := time.Duration(env.Parameters.RetryAfter) * time.Second
		if wait == 0 {
			n, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			wait = time.Duration(n) * time.Second
		}
		if wait <= 0 {
			wait = time.Second
		}
		tglimit.Block(wait)
		return &APIError{Code: CodeRateLimited, RetryAfter: wait}
	case status == 401 || status == 404:
		return &APIError{Code: CodeTokenBad}
	case status == 403:
		return &APIError{Code: CodeForbidden}
	case status == 409:
		return &APIError{Code: CodeConflict}
	case status == 400:
		if bytes.Contains([]byte(env.Description), []byte("message is not modified")) {
			return &APIError{Code: CodeNotModified}
		}
		return &APIError{Code: CodeBadRequest}
	case status >= 500:
		return &APIError{Code: CodeUnavailable}
	case status < 200 || status >= 300 || decodeErr != nil || !env.OK:
		return &APIError{Code: CodeInvalid}
	}
	if out != nil && json.Unmarshal(env.Result, out) != nil {
		return &APIError{Code: CodeInvalid}
	}
	return nil
}

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type Message struct {
	MessageID     int64           `json:"message_id"`
	From          *User           `json:"from"`
	SenderChat    *Chat           `json:"sender_chat"`
	Chat          Chat            `json:"chat"`
	Date          int64           `json:"date"`
	Text          string          `json:"text"`
	ForwardOrigin json.RawMessage `json:"forward_origin"`
	ForwardFrom   *User           `json:"forward_from"`
	ForwardDate   int64           `json:"forward_date"`
	ViaBot        *User           `json:"via_bot"`
	EditDate      int64           `json:"edit_date"`
}
type CallbackQuery struct {
	ID              string   `json:"id"`
	From            User     `json:"from"`
	Message         *Message `json:"message"`
	InlineMessageID string   `json:"inline_message_id"`
	Data            string   `json:"data"`
}
type Update struct {
	UpdateID      int64           `json:"update_id"`
	Message       *Message        `json:"message"`
	CallbackQuery *CallbackQuery  `json:"callback_query"`
	Other         json.RawMessage `json:"-"`
}

func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", map[string]any{}, &u)
	if err == nil && (u.ID == 0 || !u.IsBot) {
		err = &APIError{Code: CodeInvalid}
	}
	return u, err
}

// WebhookURL reports a configured webhook. Polling never deletes it.
func (c *Client) WebhookURL(ctx context.Context) (string, error) {
	var info struct {
		URL string `json:"url"`
	}
	err := c.call(ctx, "getWebhookInfo", map[string]any{}, &info)
	return info.URL, err
}

// GetUpdates long-polls with the given offset. Only message and
// callback_query are requested. Raw updates are returned for validation.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]json.RawMessage, error) {
	var raw []json.RawMessage
	err := c.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": timeout, "limit": 100, "allowed_updates": []string{"message", "callback_query"}}, &raw)
	return raw, err
}

func keyboard(buttons []interaction.Button) map[string]any {
	row := []map[string]string{}
	for _, b := range buttons {
		row = append(row, map[string]string{"text": b.Text, "callback_data": b.Data})
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{row}}
}

// SendMessage sends plain text (no parse mode, so model output is never
// interpreted as HTML/Markdown) and returns the message ID.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, buttons []interaction.Button) (int64, error) {
	body := map[string]any{"chat_id": chatID, "text": text, "link_preview_options": map[string]bool{"is_disabled": true}}
	if len(buttons) > 0 {
		body["reply_markup"] = keyboard(buttons)
	}
	var m Message
	if err := c.call(ctx, "sendMessage", body, &m); err != nil {
		return 0, err
	}
	return m.MessageID, nil
}

// EditMessage replaces text and keyboard; no buttons removes the keyboard.
func (c *Client) EditMessage(ctx context.Context, chatID, messageID int64, text string, buttons []interaction.Button) error {
	body := map[string]any{"chat_id": chatID, "message_id": messageID, "text": text, "link_preview_options": map[string]bool{"is_disabled": true}}
	if len(buttons) > 0 {
		body["reply_markup"] = keyboard(buttons)
	}
	return c.call(ctx, "editMessageText", body, nil)
}

func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	if len([]rune(text)) > 190 {
		text = string([]rune(text)[:190])
	}
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}
