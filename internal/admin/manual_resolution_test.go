package admin

import (
	"net/http"
	"testing"
)

func TestManualKeepHTTPRequiresSessionCSRFAndExplicitEvidence(t *testing.T) {
	b, server := loggedIn(t)
	anon := &browser{t: t, h: server.Handler(), cookies: map[string]*http.Cookie{}}
	for _, action := range []string{"manual-keep-preview", "manual-keep"} {
		path := "/api/operations/missing/" + action
		if w := anon.request("POST", path, "{}", false); w.Code != 401 {
			t.Fatal("anonymous request", action, w.Code)
		}
		if w := b.request("POST", path, "{}", false); w.Code != 403 {
			t.Fatal("CSRF bypass", action, w.Code)
		}
	}
	if w := b.request("POST", "/api/operations/missing/manual-keep", `{"confirm":true,"unexpected":1}`, true); w.Code != 400 {
		t.Fatal("unexpected input accepted", w.Code)
	}
	if w := b.request("POST", "/api/operations/missing/manual-keep", "{}", true); w.Code != 400 {
		t.Fatal("missing confirmation/evidence accepted", w.Code, w.Body)
	}
	var n int
	if err := server.Mail.Store.DB.QueryRow("SELECT count(*) FROM manual_resolutions").Scan(&n); err != nil || n != 0 {
		t.Fatal("boundary request changed resolution state", n, err)
	}
}
