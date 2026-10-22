package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mail-auto/internal/mail"
	"mail-auto/internal/mcpserver"
	"mail-auto/internal/store"
)

type MCPSettings struct {
	Enabled   bool   `json:"enabled"`
	PublicURL string `json:"public_url"`
	Version   int    `json:"version"`
}
type mcpRuntime struct {
	MCPSettings
	handler http.Handler
}
type mcpRuntimeKey struct{}

func normalizeMCPSettings(in MCPSettings) (MCPSettings, error) {
	in.PublicURL = strings.TrimSpace(in.PublicURL)
	if in.PublicURL == "" {
		if in.Enabled {
			return in, mail.ErrInput
		}
		return in, nil
	}
	u, e := url.Parse(in.PublicURL)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return in, mail.ErrInput
	}
	host := u.Hostname()
	if net.ParseIP(host) == nil && !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`).MatchString(host) {
		return in, mail.ErrInput
	}
	if u.Port() != "" {
		port, e := strconv.Atoi(u.Port())
		if e != nil || port < 1 || port > 65535 {
			return in, mail.ErrInput
		}
	}
	in.PublicURL = "https://" + strings.ToLower(u.Host)
	return in, nil
}
func (s *Server) initMCP() {
	in := MCPSettings{Enabled: s.Config.MCPEnabled}
	if s.Config.MCPEnabled {
		in.PublicURL = s.Config.PublicURL
	}
	e := s.Auth.Store.DB.QueryRow("SELECT enabled,public_url,version FROM mcp_settings WHERE id=1").Scan(&in.Enabled, &in.PublicURL, &in.Version)
	if e != nil && e != sql.ErrNoRows {
		in = MCPSettings{}
	}
	normalized, e := normalizeMCPSettings(in)
	if e != nil {
		normalized = MCPSettings{Version: in.Version}
	}
	s.mcpState = s.buildMCP(normalized)
}
func (s *Server) buildMCP(in MCPSettings) *mcpRuntime {
	out := &mcpRuntime{MCPSettings: in}
	if in.Enabled {
		out.handler = mcpserver.Handler(s.Mail, in.PublicURL)
	}
	return out
}
func (s *Server) currentMCP() *mcpRuntime {
	s.mcpMu.RLock()
	defer s.mcpMu.RUnlock()
	return s.mcpState
}
func (s *Server) requestMCP(r *http.Request) *mcpRuntime {
	if out, ok := r.Context().Value(mcpRuntimeKey{}).(*mcpRuntime); ok {
		return out
	}
	return s.currentMCP()
}
func (s *Server) saveMCP(in MCPSettings) (MCPSettings, error) {
	in, e := normalizeMCPSettings(in)
	if e != nil {
		return in, e
	}
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	old := s.mcpState
	if in.Version != old.Version {
		return in, mail.ErrConflict
	}
	tx, e := s.Auth.Store.DB.Begin()
	if e != nil {
		return in, e
	}
	defer tx.Rollback()
	in.Version++
	_, e = tx.Exec("INSERT INTO mcp_settings(id,enabled,public_url,version) VALUES(1,?,?,?) ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled,public_url=excluded.public_url,version=excluded.version", in.Enabled, in.PublicURL, in.Version)
	if e != nil {
		return in, e
	}
	if old.PublicURL != in.PublicURL {
		if _, e = tx.Exec("UPDATE mcp_tokens SET revoked=1; DELETE FROM oauth_requests; DELETE FROM oauth_codes; DELETE FROM oauth_refresh;"); e != nil {
			return in, e
		}
	}
	if e = store.Audit(tx, "mcp.settings", "server", "ok"); e != nil {
		return in, e
	}
	if e = tx.Commit(); e != nil {
		return in, e
	}
	s.mcpState = s.buildMCP(in)
	return in, nil
}
func (s *Server) mcpSettingsAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/mcp/settings/check" {
		if r.Method != "POST" {
			fail(w, 405, "此接口不支持该方法")
			return
		}
		s.checkMCP(w, r)
		return
	}
	switch r.Method {
	case "GET":
		writeJSON(w, 200, s.mcpSettingsOutput(s.currentMCP().MCPSettings))
	case "PUT":
		var in MCPSettings
		if !decode(w, r, &in) {
			return
		}
		out, e := s.saveMCP(in)
		if errors.Is(e, mail.ErrConflict) {
			fail(w, 409, "MCP 配置已被更新，请刷新页面后重试")
			return
		}
		if errors.Is(e, mail.ErrInput) {
			fail(w, 400, "请填写有效的 HTTPS 域名地址，或留空并关闭 MCP")
			return
		}
		if e != nil {
			operationError(w, e)
			return
		}
		writeJSON(w, 200, s.mcpSettingsOutput(out))
	default:
		fail(w, 405, "此接口不支持该方法")
	}
}
func (s *Server) mcpSettingsOutput(in MCPSettings) map[string]any {
	endpoint := ""
	command := ""
	toml := ""
	metadata := ""
	if in.PublicURL != "" {
		endpoint = in.PublicURL + "/mcp"
		metadata = in.PublicURL + "/.well-known/oauth-authorization-server"
		command = "codex mcp add mail-auto --url '" + endpoint + "'\ncodex mcp login mail-auto"
		toml = "[mcp_servers.mail-auto]\nurl = " + strconv.Quote(endpoint)
	}
	return map[string]any{"enabled": in.Enabled, "public_url": in.PublicURL, "version": in.Version, "endpoint": endpoint, "metadata_url": metadata, "codex_command": command, "codex_toml": toml, "authentication": "OAuth / S256 PKCE", "transport": "Streamable HTTP"}
}

type mcpCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func (s *Server) checkMCP(w http.ResponseWriter, r *http.Request) {
	state := s.currentMCP()
	checks := []mcpCheck{}
	if !state.Enabled || state.PublicURL == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "checks": []mcpCheck{{Name: "服务开关", Detail: "请先填写 HTTPS 地址并保存启用；未配置时可以保持关闭。"}}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	checks = mcpChecks(ctx, state, client)
	ok := true
	for _, c := range checks {
		ok = ok && c.OK
	}
	writeJSON(w, 200, map[string]any{"ok": ok, "checks": checks})
}

func mcpChecks(ctx context.Context, state *mcpRuntime, client *http.Client) []mcpCheck {
	checks := []mcpCheck{}
	for _, test := range []struct{ name, path, key, expected string }{{"OAuth 元数据", "/.well-known/oauth-authorization-server", "issuer", state.PublicURL}, {"资源元数据", "/.well-known/oauth-protected-resource", "resource", state.PublicURL + "/mcp"}, {"MCP 认证入口", "/mcp", "", ""}} {
		check := mcpCheck{Name: test.name}
		req, e := http.NewRequestWithContext(ctx, "GET", state.PublicURL+test.path, nil)
		if e != nil {
			check.Detail = "地址无效"
			checks = append(checks, check)
			continue
		}
		response, e := client.Do(req)
		if e != nil {
			check.Detail = "无法通过 HTTPS 访问；检查域名解析、证书、反代和端口。"
			checks = append(checks, check)
			continue
		}
		if test.key == "" {
			check.OK = response.StatusCode == 401 && strings.Contains(response.Header.Get("WWW-Authenticate"), state.PublicURL+"/.well-known/oauth-protected-resource")
		} else {
			var body map[string]any
			check.OK = response.StatusCode == 200 && json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body) == nil && body[test.key] == test.expected
		}
		response.Body.Close()
		check.Detail = "检查通过"
		if !check.OK {
			check.Detail = "响应不符合本服务配置；请检查反代路径、Host 和公开地址。"
		}
		checks = append(checks, check)
	}
	return checks
}
