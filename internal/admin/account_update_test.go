package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"mail-auto/internal/config"
	"mail-auto/internal/mail"
)

func TestHTTPAccountUpdate(t *testing.T) {
	auth, session := formalAuth(t)
	svc := &mail.Service{Store: auth.Store, Vault: auth.Vault}
	b := &browser{t: t, h: New(auth, svc, config.Config{}).Handler(), csrf: session.CSRF, cookies: map[string]*http.Cookie{sessionCookie: {Name: sessionCookie, Value: session.Token}}}
	a, e := svc.Create(mail.AccountInput{Name: "before", Provider: "qq", Username: "a@qq.com", Password: "private-original"})
	if e != nil {
		t.Fatal(e)
	}
	path := "/api/accounts/" + a.ID
	body := `{"name":"after","provider":"qq","username":"a@qq.com","port":1993}`
	anon := &browser{t: t, h: b.h, cookies: map[string]*http.Cookie{}}
	if w := anon.request("PUT", path, body, true); w.Code != 401 {
		t.Fatal("unauthenticated edit", w.Code)
	}
	if w := b.request("PUT", path, body, false); w.Code != 403 {
		t.Fatal("CSRF bypass", w.Code)
	}
	if w := b.request("PUT", path, `{"name":"bad","provider":"qq","username":"a@qq.com","host":"127.0.0.1"}`, true); w.Code != 400 {
		t.Fatal("invalid host accepted", w.Code)
	}
	if w := b.request("PUT", path, `{"name":"bad","provider":"qq","username":"a@qq.com","unexpected":true}`, true); w.Code != 400 {
		t.Fatal("unknown field accepted", w.Code)
	}
	for _, port := range []string{"-1", "65536", "1.5"} {
		bad := `{"name":"after","provider":"qq","username":"a@qq.com","port":` + port + `}`
		if w := b.request("PUT", path, bad, true); w.Code != 400 {
			t.Fatal("invalid port accepted", port, w.Code)
		}
	}
	w := b.request("PUT", path, body, true)
	if w.Code != 200 {
		t.Fatal("edit failed", w.Code, w.Body)
	}
	var updated mail.Account
	if e = json.Unmarshal(w.Body.Bytes(), &updated); e != nil || updated.ID != a.ID || updated.Name != "after" || updated.Port != 1993 {
		t.Fatal("wrong updated account", e)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("private-original")) || bytes.Contains(w.Body.Bytes(), []byte(`"secret"`)) {
		t.Fatal("secret exposed")
	}
	stored, _ := svc.Account(a.ID)
	if !bytes.Equal(stored.Secret, a.Secret) {
		t.Fatal("omitted password erased secret")
	}
	if w = b.request("PUT", path, `{"name":"after","provider":"qq","username":"a@qq.com","password":"private-new"}`, true); w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte("private-new")) {
		t.Fatal("credential replacement failed or exposed", w.Code)
	}
	if w = b.request("PUT", "/api/accounts/missing", body, true); w.Code != 404 {
		t.Fatal("missing account update", w.Code)
	}
	if _, e = auth.Store.DB.Exec("UPDATE sessions SET restricted=1"); e != nil {
		t.Fatal(e)
	}
	if _, e = auth.Store.DB.Exec("UPDATE admin SET must_change=1"); e != nil {
		t.Fatal(e)
	}
	if w = b.request("PUT", path, body, true); w.Code != 403 {
		t.Fatal("restricted session edited account", w.Code)
	}
}
