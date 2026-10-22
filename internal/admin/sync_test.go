package admin

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"mail-auto/internal/config"
	"mail-auto/internal/mail"
)

type syncRemote struct{ release <-chan struct{} }

func (r syncRemote) Folders(context.Context, mail.Account, string) ([]string, error) {
	return []string{"INBOX", "Archive"}, nil
}
func (r syncRemote) Read(context.Context, mail.Account, string, mail.Message) (mail.Content, error) {
	return mail.Content{}, nil
}
func (r syncRemote) Scan(ctx context.Context, _ mail.Account, _, _ string, emit func(mail.Snapshot) error) error {
	select {
	case <-r.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	for start := 0; start < 2501; start += 500 {
		batch := mail.Snapshot{UIDValidity: 1}
		for i := start; i < min(start+500, 2501); i++ {
			batch.Messages = append(batch.Messages, mail.Message{UID: uint32(i + 1), Subject: fmt.Sprint(i), SentAt: 946684800})
		}
		if e := emit(batch); e != nil {
			return e
		}
	}
	return nil
}
func TestHTTPAsyncHistorySync(t *testing.T) {
	auth, _, _ := newAuth(t)
	initial, _ := auth.Show()
	restricted, _ := auth.Login(initial.Username, initial.Password)
	if e := auth.Setup(restricted, "owner", "Abcdef123456"); e != nil {
		t.Fatal(e)
	}
	session, e := auth.Login("owner", "Abcdef123456")
	if e != nil {
		t.Fatal(e)
	}
	release := make(chan struct{})
	svc := &mail.Service{Store: auth.Store, Vault: auth.Vault, Remote: syncRemote{release: release}}
	defer svc.Stop()
	a, e := svc.Create(mail.AccountInput{Name: "all", Provider: "qq", Username: "a@qq.com", Password: "fake"})
	if e != nil {
		t.Fatal(e)
	}
	b := &browser{t: t, h: New(auth, svc, config.Config{}).Handler(), csrf: session.CSRF, cookies: map[string]*http.Cookie{sessionCookie: {Name: sessionCookie, Value: session.Token}}}
	if w := b.request("POST", "/api/accounts/"+a.ID+"/sync", "{}", false); w.Code != 403 {
		t.Fatal("sync CSRF bypass", w.Code)
	}
	if w := b.request("POST", "/api/accounts/"+a.ID+"/sync", "{}", true); w.Code != 202 {
		t.Fatal("job should be accepted before network completes", w.Code)
	}
	if w := b.request("GET", "/api/accounts/"+a.ID+"/sync", "", false); w.Code != 200 {
		t.Fatal("progress unavailable", w.Code)
	}
	close(release)
	// Race instrumentation and slow CI storage can delay committing 5,002 rows.
	// The HTTP 202 assertion above checks asynchronous acceptance, not throughput.
	deadline := time.Now().Add(60 * time.Second)
	for {
		jobs, e := svc.SyncJobs()
		if e != nil {
			t.Fatal(e)
		}
		if len(jobs) == 1 && jobs[0].State == "completed" {
			if jobs[0].Messages != 5002 {
				t.Fatal("full history truncated", jobs)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job not completed", jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w := b.request("GET", "/api/messages?offset=100001", "", false); w.Code != 200 {
		t.Fatal("deep pages rejected", w.Code)
	}
}
