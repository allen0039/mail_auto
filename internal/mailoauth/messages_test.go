package mailoauth

import (
	"os"
	"strings"
	"testing"
)

func TestOAuthActionableMessages(t *testing.T) {
	scope := (&Error{Code: "oauth_scope_missing"}).PublicMessage()
	for _, required := range []string{"重新进入官方授权", "明确同意 IMAP 邮件访问", "个人 Gmail", "Google 登录或个人资料不等于", "Workspace / Microsoft 365"} {
		if !strings.Contains(scope, required) {
			t.Fatalf("missing actionable scope guidance: %s", required)
		}
	}
	frontend, err := os.ReadFile("../admin/web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"oauth_scope_missing", "oauth_imap_denied", "oauth_reauthorization_required"} {
		if !strings.Contains(string(frontend), Message(code)) {
			t.Fatalf("frontend and backend guidance differ: %s", code)
		}
	}
	for _, code := range []string{"oauth_scope_missing", "oauth_imap_denied", "oauth_reauthorization_required", "oauth_network_failed"} {
		message := (&Error{Code: code}).PublicMessage()
		for _, forbidden := range []string{"提供token", "client secret", "不安全应用", "绕过验证"} {
			if strings.Contains(message, forbidden) {
				t.Fatalf("unsafe guidance: %s", code)
			}
		}
	}
	if !strings.Contains(Message("oauth_imap_denied"), "IMAP 登录被拒绝") || !strings.Contains(Message("oauth_reauthorization_required"), "撤销或过期") || !strings.Contains(Message("oauth_network_failed"), "网络") {
		t.Fatal("authentication, scope and network failures must remain distinguishable")
	}
}
