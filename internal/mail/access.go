package mail

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

var ErrDenied = errors.New("permission_denied")
var ErrInput = errors.New("invalid_input")
var ErrConflict = errors.New("state_conflict")

type Access struct {
	automation bool
	Owner      string              `json:"owner"`
	Version    int                 `json:"version"`
	Accounts   []string            `json:"accounts"`
	Folders    map[string][]string `json:"folders"`
	Actions    []string            `json:"actions"`
}

func (a Access) Allows(account, folder, action string) bool {
	if a.Owner == "admin" {
		return true
	}
	allowedAction := slices.Contains(a.Actions, action) || (a.automation && slices.Contains(a.Actions, "automation") && slices.Contains([]string{"move", "archive", "trash"}, action))
	if !allowedAction || !slices.Contains(a.Accounts, account) {
		return false
	}
	allowed, ok := a.Folders[account]
	return ok && (len(allowed) == 0 || slices.Contains(allowed, folder) || folder == "")
}

type TokenInput struct {
	Name     string              `json:"name"`
	Accounts []string            `json:"accounts"`
	Folders  map[string][]string `json:"folders"`
	Actions  []string            `json:"actions"`
	Days     int                 `json:"days"`
}
type Token struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Access   Access `json:"access"`
	Expires  int64  `json:"expires"`
	Revoked  bool   `json:"revoked"`
	LastUsed int64  `json:"last_used"`
}

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
func (s *Service) AdminAccess() (Access, error) {
	var version, restricted int
	e := s.Store.DB.QueryRow("SELECT version,must_change FROM admin WHERE id=1").Scan(&version, &restricted)
	if e != nil || restricted != 0 {
		return Access{}, ErrDenied
	}
	return Access{Owner: "admin", Version: version}, nil
}
func (s *Service) CheckAccess(a Access) (Access, error) {
	admin, e := s.AdminAccess()
	if e != nil || admin.Version != a.Version {
		return Access{}, ErrDenied
	}
	if a.Owner == "admin" {
		return admin, nil
	}
	if strings.HasPrefix(a.Owner, "svc:") {
		return s.taskServiceAccess(a)
	}
	var rawAccounts, rawFolders, rawActions string
	var expires int64
	var version, revoked int
	e = s.Store.DB.QueryRow("SELECT accounts,folders,actions,admin_version,expires,revoked FROM mcp_tokens WHERE id=?", a.Owner).Scan(&rawAccounts, &rawFolders, &rawActions, &version, &expires, &revoked)
	if e != nil || version != a.Version || revoked != 0 || expires <= time.Now().Unix() {
		return Access{}, ErrDenied
	}
	if json.Unmarshal([]byte(rawAccounts), &a.Accounts) != nil || json.Unmarshal([]byte(rawFolders), &a.Folders) != nil || json.Unmarshal([]byte(rawActions), &a.Actions) != nil {
		return Access{}, ErrDenied
	}
	return a, nil
}
func (s *Service) AuthenticateToken(token string) (Access, error) {
	if len(token) != 43 {
		return Access{}, ErrDenied
	}
	var a Access
	if e := s.Store.DB.QueryRow("SELECT id,admin_version FROM mcp_tokens WHERE hash=?", tokenHash(token)).Scan(&a.Owner, &a.Version); e != nil {
		return Access{}, ErrDenied
	}
	a, e := s.CheckAccess(a)
	if e != nil {
		return Access{}, e
	}
	_, e = s.Store.DB.Exec("UPDATE mcp_tokens SET last_used=? WHERE id=?", time.Now().Unix(), a.Owner)
	return a, e
}
func (s *Service) CreateToken(in TokenInput) (Token, string, error) {
	admin, e := s.AdminAccess()
	if e != nil {
		return Token{}, "", e
	}
	in.Name = strings.TrimSpace(in.Name)
	if len(in.Name) < 1 || len(in.Name) > 80 || len(in.Accounts) < 1 || len(in.Accounts) > 10 || len(in.Actions) < 1 || len(in.Actions) > 10 {
		return Token{}, "", ErrInput
	}
	if in.Days == 0 {
		in.Days = 30
	}
	if in.Days < 1 || in.Days > 90 {
		return Token{}, "", ErrInput
	}
	if in.Folders == nil {
		in.Folders = map[string][]string{}
	}
	for _, id := range in.Accounts {
		account, e := s.Account(id)
		if e != nil {
			return Token{}, "", ErrInput
		}
		fs := in.Folders[id]
		if len(fs) > 100 {
			return Token{}, "", ErrInput
		}
		for _, f := range fs {
			if f == "" || len(f) > 256 || strings.ContainsAny(f, "\x00\r\n") || (len(account.Folders) > 0 && !slices.Contains(account.Folders, f)) {
				return Token{}, "", ErrInput
			}
		}
		in.Folders[id] = fs
	}
	for _, action := range in.Actions {
		if !slices.Contains([]string{"read", "folder_manage", "archive", "move", "trash", "restore", "rule_manage", "task_manage", "automation"}, action) {
			return Token{}, "", ErrInput
		}
	}
	rawA, _ := json.Marshal(in.Accounts)
	rawF, _ := json.Marshal(in.Folders)
	rawAct, _ := json.Marshal(in.Actions)
	now := time.Now().Unix()
	t := Token{ID: credentials.Random(18), Name: in.Name, Access: Access{Version: admin.Version, Accounts: in.Accounts, Folders: in.Folders, Actions: in.Actions}, Expires: now + int64(in.Days)*86400}
	t.Access.Owner = t.ID
	secret := credentials.Random(32)
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return Token{}, "", e
	}
	defer tx.Rollback()
	_, e = tx.Exec("INSERT INTO mcp_tokens(id,name,hash,accounts,folders,actions,admin_version,expires,created_at) VALUES(?,?,?,?,?,?,?,?,?)", t.ID, t.Name, tokenHash(secret), string(rawA), string(rawF), string(rawAct), admin.Version, t.Expires, now)
	if e != nil {
		return Token{}, "", e
	}
	if e = store.Audit(tx, "mcp.token.create", t.ID, "ok"); e != nil {
		return Token{}, "", e
	}
	e = tx.Commit()
	return t, secret, e
}
func (s *Service) Tokens() ([]Token, error) {
	rows, e := s.Store.DB.Query("SELECT id,name,expires,revoked,last_used FROM mcp_tokens ORDER BY created_at DESC,id LIMIT 100")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		var t Token
		if e = rows.Scan(&t.ID, &t.Name, &t.Expires, &t.Revoked, &t.LastUsed); e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Service) RevokeToken(id string) error {
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	r, e := tx.Exec("UPDATE mcp_tokens SET revoked=1 WHERE id=?", id)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	if e = store.Audit(tx, "mcp.token.revoke", id, "ok"); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Service) Message(ref string) (Message, error) {
	m, e := scanMessage(s.Store.DB.QueryRow("SELECT * FROM messages WHERE ref=?", ref))
	if e != nil {
		return m, e
	}
	m.Labels, e = s.messageLabels(ref)
	return m, e
}
func (s *Service) AllowedMessage(a Access, ref, action string) (Message, error) {
	a, e := s.CheckAccess(a)
	if e != nil {
		return Message{}, e
	}
	m, e := s.Message(ref)
	if e != nil {
		return m, e
	}
	if !a.Allows(m.AccountID, m.Folder, action) {
		return Message{}, ErrDenied
	}
	return m, nil
}
