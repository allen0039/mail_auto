package mail

import (
	"encoding/base64"
	"encoding/json"
	"net/mail"
	"slices"
	"strings"
	"time"
)

type SearchInput struct {
	AccountID    string `json:"account_id"`
	Folder       string `json:"folder,omitempty"`
	Query        string `json:"query,omitempty"`
	Sender       string `json:"sender,omitempty"`
	Domain       string `json:"domain,omitempty"`
	Label        string `json:"-"`
	WithoutLabel string `json:"-"`
	ReadState    string `json:"read_state,omitempty"`
	Since        string `json:"since,omitempty"`
	Before       string `json:"before,omitempty"`
	MinSize      int64  `json:"min_size,omitempty"`
	MaxSize      int64  `json:"max_size,omitempty"`
	Cursor       string `json:"cursor,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}
type SearchOutput struct {
	Messages   []Message `json:"messages"`
	Total      int       `json:"total"`
	NextCursor string    `json:"next_cursor,omitempty"`
	Timezone   string    `json:"timezone"`
}
type searchCursor struct {
	Time   int64  `json:"time"`
	Ref    string `json:"ref"`
	Filter string `json:"filter"`
}

func (s *Service) SearchScoped(a Access, in SearchInput) (SearchOutput, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return SearchOutput{}, e
	}
	if in.AccountID == "" || !a.Allows(in.AccountID, in.Folder, "read") {
		return SearchOutput{}, ErrDenied
	}
	account, e := s.Account(in.AccountID)
	if e != nil {
		return SearchOutput{}, e
	}
	if len(in.Query) > 256 || len(in.Sender) > 254 || len(in.Domain) > 253 || len(in.Cursor) > 2048 || in.Limit < 0 || in.Limit > 100 || in.MinSize < 0 || in.MaxSize < 0 {
		return SearchOutput{}, ErrInput
	}
	if in.Sender != "" {
		addr, err := mail.ParseAddress(in.Sender)
		if err != nil || addr.Address != in.Sender {
			return SearchOutput{}, ErrInput
		}
	}
	if strings.ContainsAny(in.Domain, "@, \x00\r\n") {
		return SearchOutput{}, ErrInput
	}
	q := Search{AccountID: in.AccountID, Folder: in.Folder, Query: in.Query, SenderExact: in.Sender, Domain: in.Domain, Label: in.Label, WithoutLabel: in.WithoutLabel, ReadState: in.ReadState, MinSize: in.MinSize, MaxSize: in.MaxSize, Limit: in.Limit}
	if q.Limit == 0 {
		q.Limit = 50
	}
	fs := account.Folders
	if a.Owner != "admin" && len(a.Folders[account.ID]) > 0 {
		if len(fs) == 0 {
			fs = a.Folders[account.ID]
		} else {
			filtered := []string{}
			for _, f := range fs {
				if slices.Contains(a.Folders[account.ID], f) {
					filtered = append(filtered, f)
				}
			}
			if len(filtered) == 0 {
				return SearchOutput{Messages: []Message{}, Timezone: "Asia/Shanghai"}, nil
			}
			fs = filtered
		}
	}
	if in.Folder != "" && len(fs) > 0 && !slices.Contains(fs, in.Folder) {
		return SearchOutput{}, ErrDenied
	}
	q.Folders = fs
	loc, _ := time.LoadLocation("Asia/Shanghai")
	if in.Since != "" {
		q.Since, e = time.ParseInLocation("2006-01-02", in.Since, loc)
		if e != nil {
			return SearchOutput{}, ErrInput
		}
	}
	if in.Before != "" {
		q.Before, e = time.ParseInLocation("2006-01-02", in.Before, loc)
		if e != nil {
			return SearchOutput{}, ErrInput
		}
	}
	if !q.Since.IsZero() && !q.Before.IsZero() && !q.Since.Before(q.Before) {
		return SearchOutput{}, ErrInput
	}
	cursor := in.Cursor
	in.Cursor = ""
	in.Limit = 0
	filter, _ := json.Marshal(in)
	q.Cursor = cursor
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return SearchOutput{}, ErrInput
		}
		var c searchCursor
		if json.Unmarshal(raw, &c) != nil || c.Filter != string(filter) {
			return SearchOutput{}, ErrInput
		}
		q.Cursor = string(raw)
	}
	messages, total, e := s.Search(q)
	if e != nil {
		return SearchOutput{}, e
	}
	out := SearchOutput{Messages: messages, Total: total, Timezone: "Asia/Shanghai"}
	if len(messages) == q.Limit {
		last := messages[len(messages)-1]
		raw, _ := json.Marshal(searchCursor{Time: last.SentAt, Ref: last.Ref, Filter: string(filter)})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
}
