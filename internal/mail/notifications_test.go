package mail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type notificationTransport func(*http.Request) (*http.Response, error)

func (f notificationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const fakeBotToken = "123456789:abcdefghijklmnopqrstuvwxyz0123456789"

func configureNotification(t *testing.T, s *Service, a Access, policy string) NotificationChannel {
	t.Helper()
	c, e := s.SaveNotifications(a, NotificationInput{Enabled: true, ChatID: "-1001234567890", Policy: policy, BotToken: fakeBotToken})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func TestNotificationEncryptionDefaultsAndPrivilege(t *testing.T) {
	s, _, admin, client, _, _, _ := automationFixture(t, "observe")
	c, e := s.Notifications(admin)
	if e != nil || c.Enabled || c.Configured || c.Policy != "failures" {
		t.Fatal(c, e)
	}
	c = configureNotification(t, s, admin, "failures")
	if _, e = s.Notifications(client); !errors.Is(e, ErrDenied) {
		t.Fatal("MCP notification secret scope", e)
	}
	var raw string
	var cipher []byte
	if e = s.Store.DB.QueryRow("SELECT payload,secret FROM notification_channel").Scan(&raw, &cipher); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(raw, fakeBotToken) || strings.Contains(string(cipher), fakeBotToken) {
		t.Fatal("plaintext secret")
	}
	encoded, _ := json.Marshal(c)
	if strings.Contains(string(encoded), fakeBotToken) {
		t.Fatal("secret response")
	}
	c, e = s.SaveNotifications(admin, NotificationInput{Version: c.Version, Enabled: true, ChatID: c.ChatID, Policy: "all"})
	if e != nil || !c.Configured {
		t.Fatal("blank input replaced token", c, e)
	}
	c, e = s.SaveNotifications(admin, NotificationInput{Version: c.Version, Policy: "failures", ClearToken: true})
	if e != nil || c.Configured || c.Enabled {
		t.Fatal(c, e)
	}
}
func notificationRun(t *testing.T, s *Service, a Access, task Task, state, code string) TaskRun {
	t.Helper()
	r, e := s.RunTask(a, TaskRunInput{TaskID: task.ID, Version: task.Version, RunKey: "notify-" + time.Now().String()}, "manual")
	if e != nil {
		t.Fatal(e)
	}
	r.State = state
	r.ErrorCode = code
	if e = s.finishTaskRun(r); e != nil {
		t.Fatal(e)
	}
	return r
}
func TestNotificationFailuresPolicyAndCooldown(t *testing.T) {
	s, _, admin, _, task, _ := taskFixture(t, "observe", "manual", "rules")
	configureNotification(t, s, admin, "failures")
	for _, state := range []string{"observed", "noop", "completed", "cancelled", "limited", "requires_ai"} {
		notificationRun(t, s, admin, task, state, "")
	}
	var count int
	_ = s.Store.DB.QueryRow("SELECT count(*) FROM notification_outbox").Scan(&count)
	if count != 0 {
		t.Fatal("success noise", count)
	}
	r := notificationRun(t, s, admin, task, "failed", "provider_timeout")
	if e := s.finishTaskRun(r); e != nil {
		t.Fatal(e)
	}
	notificationRun(t, s, admin, task, "failed", "provider_timeout")
	notificationRun(t, s, admin, task, "failed", "credential_unavailable")
	_ = s.Store.DB.QueryRow("SELECT count(*) FROM notification_outbox").Scan(&count)
	if count != 2 {
		t.Fatal("event dedupe/cooldown", count)
	}
}

func TestNotificationRecoveryHonorsFailuresPolicy(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[recovery], func(t *testing.T) {
			s, _, admin, _, task, _ := taskFixture(t, "observe", "manual", "rules")
			channel := configureNotification(t, s, admin, "failures")
			if _, err := s.SaveNotifications(admin, NotificationInput{Version: channel.Version, Enabled: true, ChatID: channel.ChatID, Policy: "failures", Recovery: recovery}); err != nil {
				t.Fatal(err)
			}
			run := notificationRun(t, s, admin, task, "needs_verification", "remote_state_unknown")
			run.State = "completed"
			run.ErrorCode = ""
			run.Completed = 1
			if err := s.finishTaskRun(run); err != nil {
				t.Fatal(err)
			}
			if err := s.finishTaskRun(run); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := s.Store.DB.QueryRow("SELECT count(*) FROM notification_outbox WHERE run_id=?", run.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 1
			if recovery {
				want = 2
			}
			if count != want {
				t.Fatal("recovery notification count", count, want)
			}
		})
	}
}

func TestNotification429RetryNeverRerunsMail(t *testing.T) {
	s, remote, admin, _, task, _ := taskFixture(t, "observe", "manual", "rules")
	configureNotification(t, s, admin, "failures")
	r := notificationRun(t, s, admin, task, "needs_verification", "remote_state_unknown")
	calls := 0
	s.NotificationHTTP = &http.Client{Transport: notificationTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		payload, _ := io.ReadAll(req.Body)
		if strings.Contains(string(payload), "a@example.com") || strings.Contains(string(payload), "fixture") {
			t.Fatal("mail privacy", string(payload))
		}
		body := `{"ok":true,"result":{"message_id":1}}`
		status := 200
		if calls == 1 {
			body = `{"ok":false,"error_code":429,"parameters":{"retry_after":120}}`
			status = 429
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	s.deliverNotification(context.Background())
	out, e := s.NotificationDeliveries(admin)
	if e != nil || len(out) != 1 || out[0].State != "retry" || out[0].Attempts != 1 || out[0].NextAttempt < time.Now().Unix()+100 {
		t.Fatal(out, e)
	}
	_, e = s.Store.DB.Exec("UPDATE notification_outbox SET next_attempt=0 WHERE run_id=?", r.ID)
	if e != nil {
		t.Fatal(e)
	}
	s.deliverNotification(context.Background())
	out, e = s.NotificationDeliveries(admin)
	if e != nil || out[0].State != "sent" || out[0].Attempts != 2 || calls != 2 || remote.moves != 0 {
		t.Fatal(out, e, calls, remote.moves)
	}
}
func TestNotificationCredentialFailureAndChannelReplacement(t *testing.T) {
	s, _, admin, _, task, _ := taskFixture(t, "observe", "manual", "rules")
	c := configureNotification(t, s, admin, "all")
	notificationRun(t, s, admin, task, "failed", "provider_timeout")
	s.NotificationHTTP = &http.Client{Transport: notificationTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":false,"description":"` + fakeBotToken + `"}`))}, nil
	})}
	s.deliverNotification(context.Background())
	out, e := s.NotificationDeliveries(admin)
	if e != nil || out[0].State != "failed" || out[0].ErrorCode != "telegram_token_invalid" {
		t.Fatal(out, e)
	}
	notificationRun(t, s, admin, task, "noop", "")
	_, e = s.SaveNotifications(admin, NotificationInput{Version: c.Version, Enabled: true, ChatID: "987654321", Policy: "all"})
	if e != nil {
		t.Fatal(e)
	}
	var state string
	_ = s.Store.DB.QueryRow("SELECT state FROM notification_outbox WHERE state='suppressed'").Scan(&state)
	if state != "suppressed" {
		t.Fatal("old destination backlog", state)
	}
}
func TestNotificationCheckAndExplicitTestRedactNetworkError(t *testing.T) {
	s, _, admin, _, _, _, _ := automationFixture(t, "observe")
	configureNotification(t, s, admin, "failures")
	calls := []string{}
	s.NotificationHTTP = &http.Client{Transport: notificationTransport(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.URL.Path)
		if strings.HasSuffix(req.URL.Path, "getMe") {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"username":"test_bot"}}`))}, nil
		}
		return nil, errors.New("secret url " + req.URL.String())
	})}
	out, e := s.CheckNotifications(context.Background(), admin, false)
	if e != nil || out["ok"] != true || out["sent"] != false || len(calls) != 1 {
		t.Fatal(out, e)
	}
	out, e = s.CheckNotifications(context.Background(), admin, true)
	if e != nil || out["ok"] != false || out["error_code"] != "telegram_network_error" || len(calls) != 2 {
		t.Fatal(out, e)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), fakeBotToken) {
		t.Fatal("network token exposed")
	}
}
