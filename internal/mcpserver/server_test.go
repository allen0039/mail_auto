package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
	"mail-auto/internal/store"
)

type bearerTransport struct{ secret string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.secret)
	return http.DefaultTransport.RoundTrip(r)
}
func TestStreamableMCPPermissionsAndRevocation(t *testing.T) {
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	v, e := credentials.Open(t.TempDir(), true)
	if e != nil {
		t.Fatal(e)
	}
	st.DB.Exec("INSERT INTO admin VALUES(1,'test','unused',0,1,0,1)")
	svc := &mail.Service{Store: st, Vault: v}
	a, e := svc.Create(mail.AccountInput{Name: "fixture", Provider: "qq", Username: "fixture@qq.com", Password: "fake"})
	if e != nil {
		t.Fatal(e)
	}
	token, secret, e := svc.CreateToken(mail.TokenInput{Name: "MCP fixture", Accounts: []string{a.ID}, Actions: []string{"read"}})
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(Handler(svc, "https://mail.test"))
	defer srv.Close()
	resp, e := http.Get(srv.URL + "/mcp")
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatal(resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{secret}}, DisableStandaloneSSE: true}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	listed, e := session.ListTools(ctx, nil)
	if e != nil || len(listed.Tools) < 10 {
		t.Fatal(listed, e)
	}
	for _, tool := range listed.Tools {
		if strings.Contains(tool.Name, "label") {
			t.Fatal("local classification tool exposed", tool.Name)
		}
		if tool.Name == "mail_execute" && (tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint) {
			t.Fatal("unsafe hint")
		}
	}
	result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "mail_accounts", Arguments: map[string]any{}})
	if e != nil || result.IsError {
		t.Fatal(result, e)
	}
	result, e = session.CallTool(ctx, &mcp.CallToolParams{Name: "mail_search", Arguments: map[string]any{"account_id": "other"}})
	if e != nil || !result.IsError {
		t.Fatal("cross account accepted", result, e)
	}
	result, e = session.CallTool(ctx, &mcp.CallToolParams{Name: "mail_create_folder", Arguments: map[string]any{"account_id": a.ID, "name": "denied"}})
	if e != nil || !result.IsError {
		t.Fatal("write accepted", result, e)
	}
	if e = svc.RevokeToken(token.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = session.ListTools(ctx, nil); e == nil {
		t.Fatal("revoked HTTP bearer accepted")
	}
}
