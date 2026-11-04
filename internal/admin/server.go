package admin

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"mail-auto/internal/config"
	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
)

//go:embed web/*
var web embed.FS

const Version = "0.4.0"
const sessionCookie = "mail_auto_session"
const csrfCookie = "mail_auto_csrf"

type attempts struct {
	count int
	until time.Time
}
type Server struct {
	Auth     *Auth
	Mail     *mail.Service
	Config   config.Config
	mu       sync.Mutex
	attempts map[string]attempts
	global   attempts
	hashing  chan struct{}
	mcpMu    sync.RWMutex
	mcpState *mcpRuntime
}

func New(auth *Auth, m *mail.Service, c config.Config) *Server {
	s := &Server{Auth: auth, Mail: m, Config: c, attempts: map[string]attempts{}, hashing: make(chan struct{}, 2)}
	s.initMCP()
	return s
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		fail(w, 415, "请使用 application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		fail(w, 400, "请求格式错误或超过大小限制")
		return false
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		fail(w, 400, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}
func (s *Server) cookie(w http.ResponseWriter, r *http.Request, name, value string, expires int64) {
	maxAge := int(time.Until(time.Unix(expires, 0)).Seconds())
	if maxAge < 1 {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: s.Config.SecureCookie || r.TLS != nil || s.mcpHost(r), SameSite: http.SameSiteStrictMode, Expires: time.Unix(expires, 0), MaxAge: maxAge})
}
func (s *Server) clear(w http.ResponseWriter, r *http.Request) {
	s.cookie(w, r, sessionCookie, "", 1)
	s.cookie(w, r, csrfCookie, "", 1)
}
func (s *Server) session(r *http.Request) (Session, error) {
	c, e := r.Cookie(sessionCookie)
	if e != nil {
		return Session{}, ErrSession
	}
	return s.Auth.Session(c.Value)
}
func (s *Server) sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := s.Config.PublicURL
	if origin == "" {
		scheme := "http"
		if s.Config.SecureCookie || r.TLS != nil {
			scheme = "https"
		}
		origin = scheme + "://" + r.Host
	}
	return r.Header.Get("Origin") == origin || (s.requestMCP(r).PublicURL != "" && r.Header.Get("Origin") == s.requestMCP(r).PublicURL)
}
func tokenEqual(a, b string) bool {
	return len(a) == 43 && len(b) == 43 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func (s *Server) throttle(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.attempts {
		if now.After(v.until) {
			delete(s.attempts, k)
		}
	}
	if now.After(s.global.until) {
		s.global = attempts{until: now.Add(5 * time.Minute)}
	}
	if s.global.count >= 30 {
		return false
	}
	a := s.attempts[key]
	if now.After(a.until) {
		a = attempts{until: now.Add(5 * time.Minute)}
	}
	if a.count >= 5 || len(s.attempts) >= 1024 {
		return false
	}
	a.count++
	s.global.count++
	s.attempts[key] = a
	return true
}
func (s *Server) Handler() http.Handler {
	assets, _ := fs.Sub(web, "web")
	static := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'")
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()
		r = r.WithContext(context.WithValue(ctx, mcpRuntimeKey{}, s.currentMCP()))
		if r.URL.Path == "/healthz" {
			if r.Method != http.MethodGet {
				fail(w, 405, "method not allowed")
				return
			}
			if e := s.Auth.Store.DB.PingContext(ctx); e != nil {
				fail(w, 503, "unavailable")
				return
			}
			writeJSON(w, 200, map[string]string{"status": "ok", "version": Version})
			return
		}
		if r.URL.Path == "/readyz" {
			s.readyz(w, r)
			return
		}
		if r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/mcp/") {
			if s.requestMCP(r).handler != nil {
				if !s.mcpHost(r) {
					fail(w, 421, "请使用配置的 HTTPS MCP 地址")
					return
				}
				s.requestMCP(r).handler.ServeHTTP(w, r)
				return
			}
			fail(w, 503, "MCP 尚未启用；请在「整理与 MCP」页面保存 HTTPS 地址并启用")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/.well-known/oauth-") || strings.HasPrefix(r.URL.Path, "/oauth/") {
			s.oauthPublic(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.api(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			fail(w, 405, "method not allowed")
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/app.js" && r.URL.Path != "/organizer.js" && r.URL.Path != "/rules.js" && r.URL.Path != "/tasks.js" && r.URL.Path != "/style.css" {
			http.NotFound(w, r)
			return
		}
		static.ServeHTTP(w, r)
	})
}

// readyz distinguishes a live HTTP process from a process whose database or
// encryption key is not ready to serve mailbox operations. It intentionally
// returns only coarse state so it can be used by a container health probe.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, 405, "method not allowed")
		return
	}
	if s.Auth == nil || s.Auth.Store == nil || s.Auth.Vault == nil {
		fail(w, 503, "not ready")
		return
	}
	if e := s.Auth.Store.DB.PingContext(r.Context()); e != nil {
		fail(w, 503, "not ready")
		return
	}
	fingerprint, e := s.Auth.Store.Fingerprint()
	if e != nil || fingerprint == "" || fingerprint != s.Auth.Vault.Fingerprint {
		fail(w, 503, "not ready")
		return
	}
	var schema int
	if e = s.Auth.Store.DB.QueryRowContext(r.Context(), "PRAGMA user_version").Scan(&schema); e != nil || schema < 7 {
		fail(w, 503, "not ready")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ready", "version": Version, "schema": schema})
}
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/session" && r.Method == http.MethodGet {
		session, e := s.session(r)
		if e == nil {
			writeJSON(w, 200, map[string]any{"authenticated": true, "session": session, "version": Version})
			return
		}
		csrf := ""
		if c, e := r.Cookie(csrfCookie); e == nil && len(c.Value) == 43 {
			csrf = c.Value
		}
		if csrf == "" {
			csrf = credentials.Random(32)
			s.cookie(w, r, csrfCookie, csrf, time.Now().Add(time.Hour).Unix())
		}
		writeJSON(w, 200, map[string]any{"authenticated": false, "csrf": csrf, "version": Version})
		return
	}
	if r.URL.Path == "/api/login" && r.Method == http.MethodPost {
		c, e := r.Cookie(csrfCookie)
		if e != nil || !s.sameOrigin(r) || !tokenEqual(c.Value, r.Header.Get("X-CSRF-Token")) {
			fail(w, 403, "页面验证已过期，请刷新后重试")
			return
		}
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if !s.throttle(host) {
			w.Header().Set("Retry-After", "300")
			fail(w, 429, "登录尝试过多，请 5 分钟后重试")
			return
		}
		var input struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if !decode(w, r, &input) {
			return
		}
		if len(input.Username) > 64 || len(input.Password) > 256 {
			fail(w, 401, ErrAuth.Error())
			return
		}
		select {
		case s.hashing <- struct{}{}:
			defer func() { <-s.hashing }()
		default:
			fail(w, 429, "登录繁忙，请稍后重试")
			return
		}
		session, e := s.Auth.Login(input.Username, input.Password)
		if e != nil {
			fail(w, 401, ErrAuth.Error())
			return
		}
		s.cookie(w, r, sessionCookie, session.Token, session.Expires)
		s.cookie(w, r, csrfCookie, "", 1)
		writeJSON(w, 200, session)
		return
	}
	session, e := s.session(r)
	if e != nil {
		fail(w, 401, "请先登录")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if !s.sameOrigin(r) || !tokenEqual(session.CSRF, r.Header.Get("X-CSRF-Token")) {
			fail(w, 403, "页面验证失败，请刷新后重试")
			return
		}
	}
	if r.URL.Path == "/api/logout" && r.Method == http.MethodPost {
		if e = s.Auth.Logout(session); e != nil {
			fail(w, 500, "退出失败，请重试")
			return
		}
		s.clear(w, r)
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if r.URL.Path == "/api/setup" && r.Method == http.MethodPost {
		var in struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if !decode(w, r, &in) {
			return
		}
		select {
		case s.hashing <- struct{}{}:
			defer func() { <-s.hashing }()
		default:
			fail(w, 429, "设置繁忙，请稍后重试")
			return
		}
		if e = s.Auth.Setup(session, in.Username, in.Password); e != nil {
			fail(w, 400, "无法完成设置：请同时更换用户名和至少 12 个字符的密码，或重新登录")
			return
		}
		s.clear(w, r)
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	// This gate applies to every business API, including unknown and future routes.
	if session.Restricted {
		fail(w, 403, "请先修改初始用户名和密码")
		return
	}
	if s.organizerAPI(w, r, session) {
		return
	}
	switch {
	case r.URL.Path == "/api/settings/credentials" && r.Method == http.MethodPost:
		var in struct {
			Username        string `json:"username"`
			CurrentPassword string `json:"current_password"`
			Password        string `json:"password"`
		}
		if !decode(w, r, &in) {
			return
		}
		if !s.throttle("credentials") {
			w.Header().Set("Retry-After", "300")
			fail(w, 429, "账号修改尝试过多，请 5 分钟后重试")
			return
		}
		select {
		case s.hashing <- struct{}{}:
			defer func() { <-s.hashing }()
		default:
			fail(w, 429, "设置繁忙，请稍后重试")
			return
		}
		if e = s.Auth.ChangeCredentials(session, in.Username, in.CurrentPassword, in.Password); e != nil {
			switch {
			case errors.Is(e, ErrCurrentPassword):
				fail(w, 403, e.Error())
			case errors.Is(e, ErrCredentialsInput):
				fail(w, 400, e.Error())
			case errors.Is(e, ErrSession):
				fail(w, 401, e.Error())
			default:
				fail(w, 500, "保存账号设置失败，请稍后重试")
			}
			return
		}
		s.clear(w, r)
		writeJSON(w, 200, map[string]bool{"ok": true})
	case r.URL.Path == "/api/status" && r.Method == http.MethodGet:
		s.status(w, r)
	case r.URL.Path == "/api/accounts" && r.Method == http.MethodGet:
		out, e := s.Mail.Accounts()
		if e != nil {
			fail(w, 500, "读取邮箱配置失败")
			return
		}
		writeJSON(w, 200, out)
	case r.URL.Path == "/api/accounts" && r.Method == http.MethodPost:
		var in mail.AccountInput
		if !decode(w, r, &in) {
			return
		}
		if e = mail.ValidateAccount(&in); e != nil {
			fail(w, 400, e.Error())
			return
		}
		a, e := s.Mail.Create(in)
		if e != nil {
			fail(w, 422, "保存失败：检查外部 secret 文件是否可读，以及邮箱数量是否超过 10 个")
			return
		}
		writeJSON(w, 201, a)
	case strings.HasPrefix(r.URL.Path, "/api/accounts/"):
		s.account(w, r)
	case r.URL.Path == "/api/messages" && r.Method == http.MethodGet:
		s.search(w, r)
	case r.URL.Path == "/api/labels" || strings.HasPrefix(r.URL.Path, "/api/labels/"):
		fail(w, 410, "本地标签分类已停用；请使用原邮箱文件夹分类")
	case strings.HasPrefix(r.URL.Path, "/api/messages/") && r.Method == http.MethodGet:
		out, e := s.Mail.Read(r.Context(), strings.TrimPrefix(r.URL.Path, "/api/messages/"))
		if e != nil {
			operationError(w, e)
			return
		}
		writeJSON(w, 200, out)
	case r.URL.Path == "/api/audits" && r.Method == http.MethodGet:
		s.audits(w, r)
	default:
		fail(w, 404, "此接口未提供")
	}
}
func operationError(w http.ResponseWriter, e error) {
	if errors.Is(e, mail.ErrDailyLimit) {
		fail(w, 429, "已达到今天的自动整理上限，请明天继续或由管理员调整额度")
	} else if errors.Is(e, mail.ErrInput) || errors.Is(e, mail.ErrLabelInput) {
		fail(w, 400, "参数无效，请检查邮件和原邮箱目的文件夹")
	} else if errors.Is(e, mail.ErrDenied) {
		fail(w, 403, "当前操作未授权，或写入已停用")
	} else if errors.Is(e, mail.ErrConflict) {
		fail(w, 409, "邮件状态或计划已变化，请重新预览")
	} else if errors.Is(e, mail.ErrCapability) {
		fail(w, 422, "邮箱能力或归档/回收站映射不满足受控移动要求")
	} else if errors.Is(e, mail.ErrUncertain) {
		fail(w, 409, "原邮箱操作结果尚未确认，请先刷新原邮箱文件夹或核验任务；不要重复执行")
	} else if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "记录不存在，请重新同步")
	} else if errors.Is(e, mail.ErrBusy) {
		fail(w, 409, e.Error())
	} else {
		fail(w, 422, "操作未完成。请检查邮箱授权码、IMAP 服务、允许的文件夹、网络和凭据密钥，然后重试；部分文件夹可能已同步")
	}
}
func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/accounts/"), "/")
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodPut {
		var in mail.AccountInput
		if !decode(w, r, &in) {
			return
		}
		a, e := s.Mail.Update(id, in)
		if e != nil {
			if errors.Is(e, mail.ErrAccountInput) {
				fail(w, 400, e.Error())
			} else {
				operationError(w, e)
			}
			return
		}
		writeJSON(w, 200, a)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if e := s.Mail.Delete(id); e != nil {
			operationError(w, e)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if len(parts) != 2 {
		fail(w, 404, "接口不存在")
		return
	}
	switch {
	case parts[1] == "diagnose" && r.Method == http.MethodPost:
		folders, e := s.Mail.Diagnose(r.Context(), id)
		if e != nil {
			operationError(w, e)
			return
		}
		writeJSON(w, 200, map[string]any{"folders": folders})
	case parts[1] == "sync" && r.Method == http.MethodPost:
		result, e := s.Mail.StartSync(id)
		if e != nil {
			operationError(w, e)
			return
		}
		writeJSON(w, 202, result)
	case parts[1] == "sync" && r.Method == http.MethodGet:
		jobs, e := s.Mail.SyncJobs()
		if e != nil {
			fail(w, 500, "读取同步进度失败")
			return
		}
		for _, job := range jobs {
			if job.AccountID == id {
				writeJSON(w, 200, job)
				return
			}
		}
		fail(w, 404, "还没有同步记录")
	case parts[1] == "folders" && r.Method == http.MethodGet:
		out, e := s.Mail.Mailboxes(id)
		if e != nil {
			operationError(w, e)
			return
		}
		writeJSON(w, 200, out)
	default:
		fail(w, 404, "接口不存在")
	}
}
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	q := mail.Search{AccountID: v.Get("account_id"), Folder: v.Get("folder"), Query: v.Get("q"), Sender: v.Get("sender"), Unread: v.Get("unread") == "true", Label: v.Get("label"), WithoutLabel: v.Get("without_label"), Limit: 50}
	if len(q.Query) > 256 || len(q.Sender) > 256 {
		fail(w, 400, "搜索条件过长")
		return
	}
	var e error
	if v.Get("offset") != "" {
		q.Offset, e = strconv.Atoi(v.Get("offset"))
		if e != nil || q.Offset < 0 {
			fail(w, 400, "无效页码")
			return
		}
	}
	if v.Get("limit") != "" {
		q.Limit, e = strconv.Atoi(v.Get("limit"))
		if e != nil || q.Limit < 1 || q.Limit > 100 {
			fail(w, 400, "limit 须为 1–100")
			return
		}
	}
	for _, pair := range []struct {
		key string
		t   *time.Time
	}{{"since", &q.Since}, {"before", &q.Before}} {
		if v.Get(pair.key) != "" {
			loc, _ := time.LoadLocation("Asia/Shanghai")
			*pair.t, e = time.ParseInLocation("2006-01-02", v.Get(pair.key), loc)
			if e != nil {
				fail(w, 400, "日期格式须为 YYYY-MM-DD（北京时间）")
				return
			}
		}
	}
	out, total, e := s.Mail.Search(q)
	if e != nil {
		fail(w, 500, "搜索失败")
		return
	}
	writeJSON(w, 200, map[string]any{"messages": out, "total": total, "offset": q.Offset, "limit": q.Limit})
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	var accounts, messages, unread int
	var last sql.NullInt64
	e := s.Auth.Store.DB.QueryRowContext(r.Context(), "SELECT (SELECT count(*) FROM accounts),(SELECT count(*) FROM messages),(SELECT count(*) FROM messages WHERE unread=1),(SELECT max(synced_at) FROM mailboxes)").Scan(&accounts, &messages, &unread, &last)
	if e != nil {
		fail(w, 500, "状态读取失败")
		return
	}
	jobs, e := s.Mail.SyncJobs()
	if e != nil {
		fail(w, 500, "同步状态读取失败")
		return
	}
	automatic := s.Mail.AutomaticStatus()
	metrics, e := s.Mail.AutomationMetrics(r.Context())
	if e != nil {
		fail(w, 500, "自动化状态读取失败")
		return
	}
	writeJSON(w, 200, map[string]any{"sync_jobs": jobs, "version": Version, "accounts": accounts, "messages": messages, "unread": unread, "last_sync": last.Int64, "retention_days": 0, "all_history": true, "read_only": false, "remote_writes_default": false, "mcp_enabled": s.currentMCP().Enabled, "ai_enabled": false, "automatic_sync": automatic.Enabled, "automatic": automatic, "automation": metrics, "index_revision": s.Mail.IndexRevision()})
}
func (s *Server) audits(w http.ResponseWriter, r *http.Request) {
	rows, e := s.Auth.Store.DB.QueryContext(r.Context(), "SELECT at,action,target,outcome FROM audits ORDER BY id DESC LIMIT 100")
	if e != nil {
		fail(w, 500, "审计读取失败")
		return
	}
	defer rows.Close()
	type entry struct {
		At      int64  `json:"at"`
		Action  string `json:"action"`
		Target  string `json:"target"`
		Outcome string `json:"outcome"`
	}
	out := []entry{}
	for rows.Next() {
		var a entry
		if e = rows.Scan(&a.At, &a.Action, &a.Target, &a.Outcome); e != nil {
			fail(w, 500, "审计读取失败")
			return
		}
		out = append(out, a)
	}
	if rows.Err() != nil {
		fail(w, 500, "审计读取失败")
		return
	}
	writeJSON(w, 200, out)
}
