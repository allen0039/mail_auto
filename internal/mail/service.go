package mail

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

type Remote interface {
	Folders(context.Context, Account, string) ([]string, error)
	Scan(context.Context, Account, string, string, func(Snapshot) error) error
	Read(context.Context, Account, string, Message) (Content, error)
}
type Service struct {
	Store                 *store.Store
	Vault                 *credentials.Vault
	Remote                Remote
	SecretDir             string
	Context               context.Context
	cancel                context.CancelFunc
	wg                    sync.WaitGroup
	stopping              bool
	operationsStarted     bool
	pending               map[string]bool
	mu                    sync.Mutex
	automationMu          sync.Mutex
	taskMu                sync.Mutex
	tasksStarted          bool
	taskWorkers           map[string]bool
	workerID              string
	NotificationHTTP      *http.Client
	NotificationPublicURL string
	notificationMu        sync.Mutex
	active                map[string]bool
	automaticHolds        map[string]int
	running               int
	epoch                 map[string]uint64
	automatic             *automaticRunner
	revision              atomic.Uint64
}
type AccountInput struct {
	Name       string   `json:"name"`
	Provider   string   `json:"provider"`
	Username   string   `json:"username"`
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	Folders    []string `json:"folders"`
	Password   string   `json:"password"`
	SecretFile string   `json:"secret_file"`
}

var ErrLabelInput = errors.New("标签配置无效")

var Providers = map[string][]string{"qq": {"imap.qq.com"}, "netease": {"imap.163.com", "imap.126.com", "imap.yeah.net"}, "icloud": {"imap.mail.me.com"}}
var ErrBusy = errors.New("邮箱正在处理其他请求，或已达到两个并行读取请求上限，请稍后重试")
var ErrAccountInput = errors.New("邮箱配置无效")

func (s *Service) acquire(id string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acquireLocked(id)
}
func (s *Service) acquireConfig(id string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || s.pending[id] || s.active[id] {
		return nil, ErrBusy
	}
	// Configuration uses no remote read slot. Its epoch invalidates any active
	// background read before the configuration transaction starts.
	if s.active == nil {
		s.active = map[string]bool{}
	}
	if s.epoch == nil {
		s.epoch = map[string]uint64{}
	}
	s.active[id] = true
	s.epoch[id]++
	return func() { s.mu.Lock(); delete(s.active, id); s.mu.Unlock() }, nil
}
func (s *Service) acquireLocked(id string) (func(), error) {
	if s.stopping {
		return nil, ErrBusy
	}
	if s.active == nil {
		s.active = map[string]bool{}
	}
	if s.active[id] || s.running >= 2 {
		return nil, ErrBusy
	}
	s.active[id] = true
	s.running++
	if s.epoch == nil {
		s.epoch = map[string]uint64{}
	}
	s.epoch[id]++
	return func() { s.mu.Lock(); delete(s.active, id); s.running--; s.mu.Unlock() }, nil
}

// holdAutomatic keeps the background sync session from connecting between the
// one-message operations that make up a task batch. Individual operations still
// use acquire/release for their normal foreground protection.
func (s *Service) holdAutomatic(id string) func() {
	s.mu.Lock()
	if s.automaticHolds == nil {
		s.automaticHolds = map[string]int{}
	}
	if s.epoch == nil {
		s.epoch = map[string]uint64{}
	}
	s.automaticHolds[id]++
	s.epoch[id]++
	var active *automaticSessionHandle
	if s.automatic != nil {
		if worker := s.automatic.workers[id]; worker != nil {
			active = worker.activeSession
			worker.activeSession = nil
		}
	}
	s.mu.Unlock()
	if active != nil {
		active.Close()
	}
	return func() {
		s.mu.Lock()
		if s.automaticHolds[id] <= 1 {
			delete(s.automaticHolds, id)
		} else {
			s.automaticHolds[id]--
		}
		s.mu.Unlock()
	}
}
func ValidateAccount(a *AccountInput) error {
	return validateAccount(a, false)
}
func validateAccount(a *AccountInput, keepCredential bool) error {
	a.Name = strings.TrimSpace(a.Name)
	a.Username = strings.TrimSpace(a.Username)
	a.Host = strings.ToLower(strings.TrimSpace(a.Host))
	if len(a.Name) < 1 || len(a.Name) > 80 || len(a.Username) < 3 || len(a.Username) > 254 || strings.ContainsAny(a.Username, "\r\n\x00") {
		return errors.New("请填写有效名称和邮箱地址")
	}
	hosts, ok := Providers[a.Provider]
	if !ok {
		return errors.New("首版支持 QQ、网易和 iCloud")
	}
	if a.Host == "" {
		a.Host = hosts[0]
	}
	found := false
	for _, h := range hosts {
		if h == a.Host {
			found = true
		}
	}
	if !found {
		return errors.New("IMAP 主机不在该提供商允许列表内")
	}
	if a.Port == 0 {
		a.Port = 993
	}
	if a.Port < 1 || a.Port > 65535 {
		return errors.New("TLS 端口须为 1–65535 的整数")
	}
	if a.Folders == nil {
		a.Folders = []string{}
	}
	seen := map[string]bool{}
	for _, f := range a.Folders {
		if f == "" || len(f) > 256 || strings.ContainsAny(f, "\x00\r\n") || seen[f] {
			return errors.New("文件夹不能为空、重复或包含控制字符")
		}
		seen[f] = true
	}
	if (a.Password != "" && a.SecretFile != "") || (!keepCredential && a.Password == "" && a.SecretFile == "") {
		return errors.New("授权码和外部 secret 文件须且仅填写一项")
	}
	if len(a.Password) > 4096 {
		return errors.New("授权码过长")
	}
	return nil
}
func (s *Service) Create(a AccountInput) (Account, error) {
	if e := ValidateAccount(&a); e != nil {
		return Account{}, e
	}
	id := credentials.Random(18)
	cipher := []byte(nil)
	var e error
	if a.SecretFile != "" {
		if _, e = credentials.ExternalSecret(s.SecretDir, a.SecretFile); e != nil {
			return Account{}, e
		}
	} else {
		cipher, e = s.Vault.Encrypt(a.Password, "account:"+id)
		if e != nil {
			return Account{}, e
		}
	}
	folders, _ := json.Marshal(a.Folders)
	now := time.Now().Unix()
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return Account{}, e
	}
	defer tx.Rollback()
	var n int
	if e = tx.QueryRow("SELECT count(*) FROM accounts").Scan(&n); e != nil {
		return Account{}, e
	}
	if n >= 10 {
		return Account{}, errors.New("首版最多支持 10 个邮箱")
	}
	if _, e = tx.Exec("INSERT INTO accounts VALUES(?,?,?,?,?,?,?,?,?,?)", id, a.Name, a.Provider, a.Username, a.Host, a.Port, string(folders), cipher, a.SecretFile, now); e != nil {
		return Account{}, e
	}
	if e = store.Audit(tx, "account.create", id, "ok"); e != nil {
		return Account{}, e
	}
	if e = tx.Commit(); e != nil {
		return Account{}, e
	}
	s.wakeAutomatic()
	return s.Account(id)
}

// Update keeps the account identity and existing credential unless a replacement
// is supplied. Index changes and the audit entry commit with the configuration.
func (s *Service) Update(id string, in AccountInput) (Account, error) {
	if e := validateAccount(&in, true); e != nil {
		return Account{}, fmt.Errorf("%w: %v", ErrAccountInput, e)
	}
	release, e := s.acquireConfig(id)
	if e != nil {
		return Account{}, e
	}
	defer release()
	old, e := s.Account(id)
	if e != nil {
		return Account{}, e
	}
	cipher, secretFile := old.Secret, old.SecretFile
	if in.Password != "" {
		cipher, e = s.Vault.Encrypt(in.Password, "account:"+id)
		secretFile = ""
	} else if in.SecretFile != "" {
		_, e = credentials.ExternalSecret(s.SecretDir, in.SecretFile)
		cipher, secretFile = nil, in.SecretFile
	}
	if e != nil {
		return Account{}, e
	}
	folders, _ := json.Marshal(in.Folders)
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return Account{}, e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("UPDATE accounts SET name=?,provider=?,username=?,host=?,port=?,folders=?,secret=?,secret_file=? WHERE id=?", in.Name, in.Provider, in.Username, in.Host, in.Port, string(folders), cipher, secretFile, id); e != nil {
		return Account{}, e
	}
	identityChanged := old.Provider != in.Provider || old.Username != in.Username || old.Host != in.Host || old.Port != in.Port
	if identityChanged || len(in.Folders) > 0 {
		condition := "account_id=?"
		args := []any{id}
		if !identityChanged {
			marks := make([]string, len(in.Folders))
			for i, folder := range in.Folders {
				marks[i] = "?"
				args = append(args, folder)
			}
			condition += " AND %s NOT IN (" + strings.Join(marks, ",") + ")"
		}
		for _, table := range []struct{ name, folder string }{{"messages", "folder"}, {"mailboxes", "name"}} {
			where := condition
			if !identityChanged {
				where = fmt.Sprintf(condition, table.folder)
			}
			if _, e = tx.Exec("DELETE FROM "+table.name+" WHERE "+where, args...); e != nil {
				return Account{}, e
			}
		}
	}
	for _, table := range []string{"sync_stage", "sync_jobs"} {
		if _, e = tx.Exec("DELETE FROM "+table+" WHERE account_id=?", id); e != nil {
			return Account{}, e
		}
	}
	if e = store.Audit(tx, "account.update", id, "ok"); e != nil {
		return Account{}, e
	}
	if e = tx.Commit(); e != nil {
		return Account{}, e
	}
	s.revision.Add(1)
	s.Store.RequestReclaim()
	s.wakeAutomatic()
	return s.Account(id)
}
func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var folders string
	e := row.Scan(&a.ID, &a.Name, &a.Provider, &a.Username, &a.Host, &a.Port, &folders, &a.Secret, &a.SecretFile, &a.CreatedAt)
	if e != nil {
		return a, e
	}
	if e = json.Unmarshal([]byte(folders), &a.Folders); e != nil {
		return a, e
	}
	a.CredentialSource = "encrypted"
	if a.SecretFile != "" {
		a.CredentialSource = "external"
	}
	return a, nil
}
func (s *Service) Accounts() ([]Account, error) {
	rows, e := s.Store.DB.Query("SELECT * FROM accounts ORDER BY created_at,id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, e := scanAccount(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	rows.Close()
	for i := range out {
		boxes, e := s.Mailboxes(out[i].ID)
		if e != nil {
			return nil, e
		}
		out[i].SyncedFolders = []string{}
		for _, box := range boxes {
			out[i].SyncedFolders = append(out[i].SyncedFolders, box.Name)
		}
	}
	return out, nil
}
func (s *Service) Account(id string) (Account, error) {
	return scanAccount(s.Store.DB.QueryRow("SELECT * FROM accounts WHERE id=?", id))
}
func (s *Service) password(a Account) (string, error) {
	if a.SecretFile != "" {
		return credentials.ExternalSecret(s.SecretDir, a.SecretFile)
	}
	return s.Vault.Decrypt(a.Secret, "account:"+a.ID)
}
func (s *Service) Delete(id string) error {
	release, e := s.acquireConfig(id)
	if e != nil {
		return e
	}
	defer release()
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	res, e := tx.Exec("DELETE FROM accounts WHERE id=?", id)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	if e = store.Audit(tx, "account.delete", id, "ok"); e != nil {
		return e
	}
	if e = tx.Commit(); e == nil {
		s.revision.Add(1)
		s.Store.RequestReclaim()
		s.wakeAutomatic()
	}
	return e
}
func (s *Service) Diagnose(ctx context.Context, id string) ([]string, error) {
	release, e := s.acquire(id)
	if e != nil {
		return nil, e
	}
	defer release()
	a, e := s.Account(id)
	if e != nil {
		return nil, e
	}
	pw, e := s.password(a)
	if e != nil {
		return nil, e
	}
	folders, e := s.Remote.Folders(ctx, a, pw)
	outcome := "ok"
	if e != nil {
		outcome = "failed"
	}
	_ = store.Audit(s.Store.DB, "account.diagnose", id, outcome)
	return folders, e
}

func (s *Service) Mailboxes(id string) ([]Mailbox, error) {
	rows, e := s.Store.DB.Query("SELECT name,uidvalidity,synced_at FROM mailboxes WHERE account_id=? ORDER BY name", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Mailbox{}
	for rows.Next() {
		var m Mailbox
		if e = rows.Scan(&m.Name, &m.UIDValidity, &m.SyncedAt); e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Service) Search(q Search) ([]Message, int, error) {
	where := []string{"1=1"}
	args := []any{}
	for _, f := range []struct{ col, value string }{{"account_id", q.AccountID}, {"folder", q.Folder}} {
		if f.value != "" {
			where = append(where, f.col+"=?")
			args = append(args, f.value)
		}
	}
	if q.Query != "" {
		where = append(where, "(instr(lower(subject),lower(?))>0 OR instr(lower(sender),lower(?))>0)")
		args = append(args, q.Query, q.Query)
	}
	if q.Sender != "" {
		where = append(where, "instr(lower(sender),lower(?))>0")
		args = append(args, q.Sender)
	}
	if q.Unread {
		where = append(where, "unread=1")
	}
	if q.ReadState != "" && q.ReadState != "all" {
		if q.ReadState == "read" {
			where = append(where, "unread=0")
		} else if q.ReadState == "unread" {
			where = append(where, "unread=1")
		} else {
			return nil, 0, ErrInput
		}
	}
	if len(q.Folders) > 0 {
		marks := make([]string, len(q.Folders))
		for i, f := range q.Folders {
			marks[i] = "?"
			args = append(args, f)
		}
		where = append(where, "folder IN ("+strings.Join(marks, ",")+")")
	}
	if q.SenderExact != "" {
		where = append(where, "instr(','||lower(replace(sender,' ',''))||',', ','||lower(?)||',')>0")
		args = append(args, q.SenderExact)
	}
	if q.Domain != "" {
		where = append(where, "instr(lower(replace(sender,' ',''))||',', '@'||lower(?)||',')>0")
		args = append(args, q.Domain)
	}
	if q.MinSize > 0 {
		where = append(where, "size>=?")
		args = append(args, q.MinSize)
	}
	if q.MaxSize > 0 {
		where = append(where, "size<=?")
		args = append(args, q.MaxSize)
	}
	if q.Label != "" {
		where = append(where, "EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_ref=messages.ref AND l.name=?)")
		args = append(args, q.Label)
	}
	if q.WithoutLabel != "" {
		where = append(where, "NOT EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_ref=messages.ref AND l.name=?)")
		args = append(args, q.WithoutLabel)
	}
	if !q.Since.IsZero() {
		where = append(where, "sent_at>=?")
		args = append(args, q.Since.Unix())
	}
	if !q.Before.IsZero() {
		where = append(where, "sent_at<?")
		args = append(args, q.Before.Unix())
	}
	if q.Limit < 1 || q.Limit > 100 {
		q.Limit = 50
	}
	if q.Offset < 0 {
		return nil, 0, errors.New("invalid offset")
	}
	if q.Cursor != "" {
		var c searchCursor
		if json.Unmarshal([]byte(q.Cursor), &c) != nil || c.Ref == "" {
			return nil, 0, ErrInput
		}
		where = append(where, "(sent_at<? OR (sent_at=? AND ref>?))")
		args = append(args, c.Time, c.Time, c.Ref)
	}
	clause := strings.Join(where, " AND ")
	var total int
	if e := s.Store.DB.QueryRow("SELECT count(*) FROM messages WHERE "+clause, args...).Scan(&total); e != nil {
		return nil, 0, e
	}
	args = append(args, q.Limit, q.Offset)
	rows, e := s.Store.DB.Query("SELECT "+messageColumns+" FROM messages WHERE "+clause+" ORDER BY sent_at DESC,ref LIMIT ? OFFSET ?", args...)
	if e != nil {
		return nil, 0, e
	}
	out := []Message{}
	for rows.Next() {
		m, e := scanMessage(rows)
		if e != nil {
			rows.Close()
			return nil, 0, e
		}
		out = append(out, m)
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return nil, 0, rowErr
	}
	refs := make([]string, len(out))
	for i := range out {
		refs[i] = out[i].Ref
	}
	labels, e := s.messageLabelsForRefs(refs)
	if e != nil {
		return nil, 0, e
	}
	for i := range out {
		out[i].Labels = labels[out[i].Ref]
	}
	return out, total, nil
}

func (s *Service) Read(ctx context.Context, ref string) (Content, error) {
	return s.read(ctx, ref, nil)
}
func (s *Service) read(ctx context.Context, ref string, guard func(Message) error) (Content, error) {
	m, e := scanMessage(s.Store.DB.QueryRow("SELECT "+messageColumns+" FROM messages WHERE ref=?", ref))
	if e != nil {
		return Content{}, e
	}
	release, e := s.acquire(m.AccountID)
	if e != nil {
		return Content{}, e
	}
	defer release()
	// The configuration may have changed after the first lookup, before acquire.
	m, e = scanMessage(s.Store.DB.QueryRow("SELECT "+messageColumns+" FROM messages WHERE ref=?", ref))
	if e != nil {
		return Content{}, e
	}
	if guard != nil {
		if e = guard(m); e != nil {
			return Content{}, e
		}
	}
	a, e := s.Account(m.AccountID)
	if e != nil {
		return Content{}, e
	}
	allowed := len(a.Folders) == 0
	for _, f := range a.Folders {
		if m.Folder == f {
			allowed = true
		}
	}
	if !allowed {
		return Content{}, errors.New("文件夹未授权")
	}
	pw, e := s.password(a)
	if e != nil {
		return Content{}, e
	}
	m.Labels, e = s.messageLabels(m.Ref)
	if e != nil {
		return Content{}, e
	}
	out, e := s.Remote.Read(ctx, a, pw, m)
	status := "ok"
	if e != nil {
		status = "failed"
	}
	_ = store.Audit(s.Store.DB, "mail.read", m.Ref, status)
	return out, e
}
func Endpoint(a Account) string { return net.JoinHostPort(a.Host, strconv.Itoa(a.Port)) }
func (s SyncResult) String() string {
	return fmt.Sprintf("%d folders, %d messages", s.Folders, s.Messages)
}
