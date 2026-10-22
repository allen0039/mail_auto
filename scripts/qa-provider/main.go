// Disposable provider fixture. Never included in the production image.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"mail-auto/internal/admin"
	"mail-auto/internal/config"
	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
	"mail-auto/internal/store"
)

type provider struct {
	mu    sync.Mutex
	boxes map[string]map[uint32]mail.Message
	next  uint32
}

func (p *provider) Folders(context.Context, mail.Account, string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []string{}
	for name := range p.boxes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
func (p *provider) Scan(_ context.Context, _ mail.Account, _ string, folder string, emit func(mail.Snapshot) error) error {
	p.mu.Lock()
	out := mail.Snapshot{UIDValidity: 7, Messages: []mail.Message{}}
	for _, m := range p.boxes[folder] {
		out.Messages = append(out.Messages, m)
	}
	p.mu.Unlock()
	return emit(out)
}
func (p *provider) Read(_ context.Context, _ mail.Account, _ string, m mail.Message) (mail.Content, error) {
	return mail.Content{Message: m, Text: "Fictional provider message"}, nil
}
func (p *provider) Capabilities(ctx context.Context, a mail.Account, pw string) (mail.Capabilities, error) {
	names, _ := p.Folders(ctx, a, pw)
	out := mail.Capabilities{UIDPlus: true}
	for _, name := range names {
		role := ""
		if name == "已删除" {
			role = "trash"
		}
		out.Folders = append(out.Folders, mail.FolderInfo{Name: name, Label: mail.FolderLabel(name), Role: role})
	}
	return out, nil
}
func (p *provider) CreateFolder(_ context.Context, _ mail.Account, _ string, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.boxes[name]; ok {
		return errors.New("exists")
	}
	p.boxes[name] = map[uint32]mail.Message{}
	return nil
}
func (p *provider) Move(_ context.Context, _ mail.Account, _ string, m mail.Message, dest string, record func(mail.MoveResult) error) (mail.MoveResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	source, ok := p.boxes[m.Folder][m.UID]
	if !ok {
		return mail.MoveResult{}, errors.New("source missing")
	}
	if _, ok = p.boxes[dest]; !ok {
		return mail.MoveResult{}, errors.New("target missing")
	}
	p.next++
	r := mail.MoveResult{Folder: dest, UIDValidity: 7, UID: p.next}
	if e := record(r); e != nil {
		return r, e
	}
	delete(p.boxes[m.Folder], m.UID)
	source.Folder = dest
	source.UID = r.UID
	p.boxes[dest][r.UID] = source
	return r, nil
}
func (p *provider) VerifyMove(_ context.Context, _ mail.Account, _ string, m mail.Message, r mail.MoveResult) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, old := p.boxes[m.Folder][m.UID]
	_, target := p.boxes[r.Folder][r.UID]
	if old || !target {
		return mail.ErrUncertain
	}
	return nil
}
func main() {
	if len(os.Args) != 2 {
		log.Fatal("requires disposable state directory")
	}
	root := os.Args[1]
	st, e := store.Open(filepath.Join(root, "data"))
	if e != nil {
		log.Fatal(e)
	}
	defer st.Close()
	vault, e := credentials.Open(filepath.Join(root, "keys"), true)
	if e != nil {
		log.Fatal(e)
	}
	auth := &admin.Auth{Store: st, Vault: vault, TTL: time.Hour}
	if e = auth.Initialize(); e != nil {
		log.Fatal(e)
	}
	if b, e := auth.Show(); e == nil {
		raw, _ := json.Marshal(b)
		if e = os.WriteFile(filepath.Join(root, "bootstrap.json"), raw, 0600); e != nil {
			log.Fatal(e)
		}
	}
	p := &provider{boxes: map[string]map[uint32]mail.Message{"INBOX": {}, "已删除": {}}, next: 100}
	for i := uint32(1); i <= 3; i++ {
		p.boxes["INBOX"][i] = mail.Message{UID: i, Subject: fmt.Sprint("测试账单 <script> ", i), Sender: "billing@example.test", SentAt: time.Now().Unix() - int64(i), Unread: true, Size: 120}
	}
	svc := &mail.Service{Store: st, Vault: vault, Remote: p}
	defer svc.Stop()
	accounts, e := svc.Accounts()
	if e != nil {
		log.Fatal(e)
	}
	if len(accounts) == 0 {
		a, e := svc.Create(mail.AccountInput{Name: "虚构原邮箱", Provider: "qq", Username: "fixture@qq.com", Password: "fixture"})
		if e != nil {
			log.Fatal(e)
		}
		if _, e = svc.Sync(context.Background(), a.ID); e != nil {
			log.Fatal(e)
		}
	}
	if e = svc.StartOperations(); e != nil {
		log.Fatal(e)
	}
	handler := admin.New(auth, svc, config.Config{PublicURL: "http://127.0.0.1:18082"}).Handler()
	log.Fatal(http.ListenAndServe("127.0.0.1:18082", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fixture/provider" {
			p.mu.Lock()
			defer p.mu.Unlock()
			json.NewEncoder(w).Encode(p.boxes)
			return
		}
		handler.ServeHTTP(w, r)
	})))
}
