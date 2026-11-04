package mail

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/store"
)

type NotificationChannel struct {
	Version    int    `json:"version"`
	Enabled    bool   `json:"enabled"`
	ChatID     string `json:"chat_id"`
	Policy     string `json:"policy"`
	Configured bool   `json:"configured"`
	Recovery   bool   `json:"recovery"`
}
type NotificationInput struct {
	Version    int    `json:"version"`
	Enabled    bool   `json:"enabled"`
	ChatID     string `json:"chat_id"`
	Policy     string `json:"policy"`
	Recovery   bool   `json:"recovery"`
	BotToken   string `json:"bot_token"`
	ClearToken bool   `json:"clear_token"`
}
type NotificationDelivery struct {
	ID          string `json:"id"`
	RunID       string `json:"run_id"`
	State       string `json:"state"`
	Attempts    int    `json:"attempts"`
	NextAttempt int64  `json:"next_attempt"`
	ErrorCode   string `json:"error_code"`
	UpdatedAt   int64  `json:"updated_at"`
}

func validNotificationPolicy(v string, inherit bool) bool {
	return slices.Contains([]string{"off", "failures", "changes", "all"}, v) || (inherit && v == "inherit")
}

var botTokenPattern = regexp.MustCompile(`^[0-9]{5,20}:[A-Za-z0-9_-]{20,100}$`)
var chatIDPattern = regexp.MustCompile(`^-?[0-9]{1,20}$`)

func (s *Service) notificationChannel() (NotificationChannel, []byte, error) {
	var raw string
	var secret []byte
	e := s.Store.DB.QueryRow("SELECT payload,secret FROM notification_channel WHERE id=1").Scan(&raw, &secret)
	if e == sql.ErrNoRows {
		return NotificationChannel{Policy: "failures"}, nil, nil
	}
	if e != nil {
		return NotificationChannel{}, nil, e
	}
	var c NotificationChannel
	if json.Unmarshal([]byte(raw), &c) != nil {
		return c, nil, ErrConflict
	}
	c.Configured = len(secret) > 0
	return c, secret, nil
}
func (s *Service) Notifications(a Access) (NotificationChannel, error) {
	a, e := s.CheckAccess(a)
	if e != nil || a.Owner != "admin" {
		return NotificationChannel{}, ErrDenied
	}
	c, _, e := s.notificationChannel()
	return c, e
}
func (s *Service) SaveNotifications(a Access, in NotificationInput) (NotificationChannel, error) {
	s.notificationMu.Lock()
	defer s.notificationMu.Unlock()
	a, e := s.CheckAccess(a)
	if e != nil || a.Owner != "admin" {
		return NotificationChannel{}, ErrDenied
	}
	old, secret, e := s.notificationChannel()
	if e != nil {
		return old, e
	}
	if in.Version != old.Version {
		return old, ErrConflict
	}
	in.ChatID = strings.TrimSpace(in.ChatID)
	in.BotToken = strings.TrimSpace(in.BotToken)
	if !validNotificationPolicy(in.Policy, false) || (in.ChatID != "" && !chatIDPattern.MatchString(in.ChatID)) || (in.BotToken != "" && !botTokenPattern.MatchString(in.BotToken)) || (in.ClearToken && in.BotToken != "") {
		return old, ErrInput
	}
	if in.ClearToken {
		secret = []byte{}
		in.Enabled = false
	}
	if in.BotToken != "" {
		secret, e = s.Vault.Encrypt(in.BotToken, "notification:telegram:1")
		if e != nil {
			return old, ErrConflict
		}
	}
	if in.Enabled && (len(secret) == 0 || in.ChatID == "") {
		return old, ErrInput
	}
	c := NotificationChannel{Version: old.Version + 1, Enabled: in.Enabled, ChatID: in.ChatID, Policy: in.Policy, Configured: len(secret) > 0, Recovery: in.Recovery}
	raw, _ := json.Marshal(c)
	tx, e := s.Store.DB.Begin()
	if e != nil {
		return c, e
	}
	defer tx.Rollback()
	if old.Version == 0 {
		_, e = tx.Exec("INSERT INTO notification_channel VALUES(1,?,?,?)", c.Version, string(raw), secret)
	} else {
		var res sql.Result
		res, e = tx.Exec("UPDATE notification_channel SET version=?,payload=?,secret=? WHERE id=1 AND version=?", c.Version, string(raw), secret, old.Version)
		if e == nil {
			n, _ := res.RowsAffected()
			if n != 1 {
				return c, ErrConflict
			}
		}
	}
	if e != nil {
		return c, e
	}
	if _, e = tx.Exec("UPDATE notification_outbox SET state='suppressed',error_code='channel_changed',updated_at=? WHERE state IN ('pending','retry','sending')", time.Now().Unix()); e != nil {
		return c, e
	}
	if e = store.Audit(tx, "notification.configure", "telegram", "ok"); e != nil {
		return c, e
	}
	return c, tx.Commit()
}

type telegramResult struct {
	OK        bool `json:"ok"`
	ErrorCode int  `json:"error_code"`
	Result    struct {
		Username string `json:"username"`
	} `json:"result"`
	Parameters struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (s *Service) telegram(ctx context.Context, c NotificationChannel, secret []byte, method, text string) (string, string, int) {
	token, e := s.Vault.Decrypt(secret, "notification:telegram:1")
	if e != nil {
		return "", "credential_unavailable", 0
	}
	if !botTokenPattern.MatchString(token) {
		return "", "credential_unavailable", 0
	}
	data := map[string]any{}
	if method == "sendMessage" {
		data = map[string]any{"chat_id": c.ChatID, "text": text, "disable_web_page_preview": true}
	}
	body, _ := json.Marshal(data)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+token+"/"+method, bytes.NewReader(body))
	if e != nil {
		return "", "notification_request_invalid", 0
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.NotificationHTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, e := client.Do(req)
	if e != nil {
		return "", "telegram_network_error", 0
	}
	defer resp.Body.Close()
	var out telegramResult
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out)
	code := resp.StatusCode
	if out.ErrorCode != 0 {
		code = out.ErrorCode
	}
	if code == 429 {
		wait := out.Parameters.RetryAfter
		if wait == 0 {
			wait, _ = strconv.Atoi(resp.Header.Get("Retry-After"))
		}
		return "", "telegram_rate_limited", max(1, min(wait, 86400))
	}
	if code == 401 {
		return "", "telegram_token_invalid", 0
	}
	if code == 403 {
		return "", "telegram_access_denied", 0
	}
	if code == 400 {
		return "", "telegram_chat_or_request_invalid", 0
	}
	if code >= 500 {
		return "", "telegram_service_unavailable", 0
	}
	if code < 200 || code >= 300 {
		return "", "telegram_request_rejected", 0
	}
	if decodeErr != nil || !out.OK {
		return "", "telegram_invalid_response", 0
	}
	return out.Result.Username, "", 0
}
func (s *Service) CheckNotifications(ctx context.Context, a Access, send bool) (map[string]any, error) {
	s.notificationMu.Lock()
	defer s.notificationMu.Unlock()
	a, e := s.CheckAccess(a)
	if e != nil || a.Owner != "admin" {
		return nil, ErrDenied
	}
	c, secret, e := s.notificationChannel()
	if e != nil {
		return nil, e
	}
	if len(secret) == 0 || (send && c.ChatID == "") {
		return nil, ErrInput
	}
	method := "getMe"
	text := ""
	if send {
		method = "sendMessage"
		text = "Mail Auto · Telegram 测试通知\n通知通道可达。此消息不代表邮件任务已运行。"
	}
	username, code, _ := s.telegram(ctx, c, secret, method, text)
	_ = store.Audit(s.Store.DB, "notification.test", "telegram", map[bool]string{true: "send", false: "check"}[send]+":"+code)
	return map[string]any{"ok": code == "", "bot_username": username, "error_code": code, "sent": send && code == ""}, nil
}
func notificationFailure(r TaskRun) bool {
	return slices.Contains([]string{"failed", "partial", "needs_verification", "interrupted"}, r.State)
}
func (s *Service) enqueueNotification(tx *sql.Tx, t Task, r TaskRun) error {
	if activeTaskState(r.State) {
		return nil
	}
	var raw string
	var cv int
	e := tx.QueryRow("SELECT payload,version FROM notification_channel WHERE id=1").Scan(&raw, &cv)
	if e == sql.ErrNoRows {
		return nil
	}
	if e != nil {
		return e
	}
	var c NotificationChannel
	if json.Unmarshal([]byte(raw), &c) != nil {
		return ErrConflict
	}
	policy := t.Notify
	if policy == "inherit" {
		policy = c.Policy
	}
	if !c.Enabled || policy == "off" {
		return nil
	}
	failure := notificationFailure(r)
	if policy == "failures" && !failure {
		return nil
	}
	if policy == "changes" && !failure && r.Completed == 0 {
		return nil
	}
	if policy != "all" && !failure && r.State == "observed" {
		return nil
	}
	var existing int
	if e = tx.QueryRow("SELECT count(*) FROM notification_outbox WHERE run_id=? AND event_key<>?", r.ID, r.ID+":"+r.State).Scan(&existing); e != nil {
		return e
	}
	if existing > 0 && !failure && !c.Recovery {
		return nil
	}
	now := time.Now().Unix()
	event := r.State + ":" + r.ErrorCode
	if failure {
		var last int64
		e = tx.QueryRow("SELECT last_sent FROM notification_cooldown WHERE task_id=? AND event=?", t.ID, event).Scan(&last)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		if last > now-6*3600 {
			return nil
		}
	}
	labels := map[string]string{"completed": "任务完成", "noop": "本轮无变更", "observed": "观察完成", "partial": "任务部分失败", "failed": "任务失败", "needs_verification": "邮件结果待核验", "interrupted": "任务中断", "cancelled": "本轮已取消", "limited": "本轮达到限制", "requires_ai": "部分邮件需外部 AI"}
	title := labels[r.State]
	if title == "" {
		title = r.State
	}
	text := fmt.Sprintf("Mail Auto · %s\n任务：%s\n已核验整理 %d，失败 %d，待核验 %d\n保留 %d，保护跳过 %d，需外部 AI %d\n来源：%s\n时间：%s\n事件：%s", title, r.TaskName, r.Completed, r.Failed, r.Unknown, r.Kept, r.Protected, r.NeedsAI, map[string]string{"manual": "手动", "internal": "内置定时", "external": "外部 MCP", "monitor": "外部触发监测"}[r.Source], time.Unix(r.CreatedAt, 0).In(automationLocation).Format("2006-01-02 15:04"), r.ID[:8])
	if r.ErrorCode != "" {
		text += "\n错误：" + r.ErrorCode
	}
	if r.HasMore {
		text += "\n仍有候选，下次触发继续扫描"
	}
	if base, e := url.Parse(s.NotificationPublicURL); e == nil && base.Scheme == "https" && base.Host != "" && base.User == nil {
		base.Path = "/"
		base.RawQuery = ""
		base.Fragment = "task-run=" + r.ID
		text += "\n详情：" + base.String()
	}
	payload, _ := json.Marshal(map[string]string{"text": text})
	res, e := tx.Exec("INSERT OR IGNORE INTO notification_outbox(id,run_id,event_key,channel_version,state,payload,next_attempt,created_at,updated_at) VALUES(?,?,?,?,'pending',?,?,?,?)", credentials.Random(24), r.ID, r.ID+":"+r.State, cv, string(payload), now, now, now)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if failure && n > 0 {
		_, e = tx.Exec("INSERT INTO notification_cooldown VALUES(?,?,?) ON CONFLICT(task_id,event) DO UPDATE SET last_sent=excluded.last_sent", t.ID, event, now)
	}
	return e
}
func (s *Service) deliverNotification(ctx context.Context) {
	s.notificationMu.Lock()
	defer s.notificationMu.Unlock()
	now := time.Now().Unix()
	var id, runID, raw string
	var version, attempts int
	e := s.Store.DB.QueryRowContext(ctx, "SELECT id,run_id,payload,channel_version,attempts FROM notification_outbox WHERE ((state IN ('pending','retry') AND next_attempt<=?) OR (state='sending' AND lease_until<?)) ORDER BY next_attempt,id LIMIT 1", now, now).Scan(&id, &runID, &raw, &version, &attempts)
	if e != nil {
		return
	}
	res, e := s.Store.DB.Exec("UPDATE notification_outbox SET state='sending',lease_until=? WHERE id=? AND ((state IN ('pending','retry') AND next_attempt<=?) OR (state='sending' AND lease_until<?))", now+60, id, now, now)
	if e != nil {
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return
	}
	c, secret, e := s.notificationChannel()
	if e != nil {
		return
	}
	r, e := s.loadTaskRun(runID)
	if e != nil {
		return
	}
	t, e := s.loadTask(r.TaskID)
	if e != nil {
		return
	}
	if !c.Enabled || c.Version != version || t.Notify == "off" || (t.Notify == "inherit" && c.Policy == "off") {
		_, _ = s.Store.DB.Exec("UPDATE notification_outbox SET state='suppressed',error_code='notification_disabled',updated_at=? WHERE id=?", now, id)
		return
	}
	var payload struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return
	}
	_, code, wait := s.telegram(ctx, c, secret, "sendMessage", payload.Text)
	attempts++
	state := "sent"
	next := int64(0)
	if code != "" {
		state = "failed"
		if slices.Contains([]string{"telegram_network_error", "telegram_service_unavailable", "telegram_rate_limited", "telegram_invalid_response"}, code) && attempts < 5 {
			state = "retry"
			next = now + int64(max(wait, 30*(1<<min(attempts-1, 4))))
		}
	}
	_, _ = s.Store.DB.Exec("UPDATE notification_outbox SET state=?,attempts=?,next_attempt=?,lease_until=0,error_code=?,updated_at=? WHERE id=? AND state='sending'", state, attempts, next, code, time.Now().Unix(), id)
}
func (s *Service) NotificationDeliveries(a Access) ([]NotificationDelivery, error) {
	a, e := s.CheckAccess(a)
	if e != nil || a.Owner != "admin" {
		return nil, ErrDenied
	}
	rows, e := s.Store.DB.Query("SELECT id,run_id,state,attempts,next_attempt,error_code,updated_at FROM notification_outbox ORDER BY created_at DESC,id DESC LIMIT 50")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []NotificationDelivery{}
	for rows.Next() {
		var d NotificationDelivery
		if e = rows.Scan(&d.ID, &d.RunID, &d.State, &d.Attempts, &d.NextAttempt, &d.ErrorCode, &d.UpdatedAt); e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
