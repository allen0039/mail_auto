package admin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mail-auto/internal/modelprovider"
	"mail-auto/internal/network"
	"mail-auto/internal/store"
	"mail-auto/internal/telegram"
)

func networkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, network.ErrConflict):
		fail(w, 409, "网络配置已更新，请刷新后重试")
	case errors.Is(err, network.ErrReferenced):
		fail(w, 409, "代理仍被邮箱引用，请先修改邮箱的连接策略")
	default:
		fail(w, 400, "代理配置无效：请检查协议、地址、端口、账号密码和引用关系")
	}
}

func (s *Server) networkAPI(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/network") {
		return false
	}
	if s.Mail.Network == nil {
		fail(w, 503, "网络设置未初始化")
		return true
	}
	if r.URL.Path == "/api/network/check" && r.Method == http.MethodPost {
		s.checkNetwork(w, r)
		return true
	}
	if r.URL.Path != "/api/network" {
		fail(w, 404, "接口不存在")
		return true
	}
	switch r.Method {
	case http.MethodGet:
		out, err := s.Mail.Network.Settings()
		if err != nil {
			networkError(w, err)
			return true
		}
		writeJSON(w, 200, out)
	case http.MethodPut:
		var in network.Input
		if !decode(w, r, &in) {
			return true
		}
		out, err := s.Mail.Network.Save(in)
		if err != nil {
			networkError(w, err)
			return true
		}
		s.Mail.NetworkChanged()
		if s.Telegram != nil {
			s.Telegram.NetworkChanged()
		}
		writeJSON(w, 200, out)
	default:
		fail(w, 405, "此接口不支持该方法")
	}
	return true
}

func (s *Server) checkNetwork(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope     string `json:"scope"`
		AccountID string `json:"account_id"`
		Mode      string `json:"mode"`
		ProxyID   string `json:"proxy_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Scope != "account" && in.Scope != "global" && in.Scope != "telegram" && in.Scope != "agent" {
		networkError(w, network.ErrConfig)
		return
	}
	address, hostname := "api.telegram.org:443", "api.telegram.org"
	access, err := s.Mail.AdminAccess()
	if err != nil {
		operationError(w, err)
		return
	}
	if in.Scope == "account" {
		account, err := s.Mail.Account(in.AccountID)
		if err != nil {
			operationError(w, err)
			return
		}
		address, hostname = net.JoinHostPort(account.Host, strconv.Itoa(account.Port)), account.Host
	}
	if in.Scope == "agent" {
		if s.Agent == nil {
			fail(w, 503, "Agent 未初始化")
			return
		}
		target, err := s.Agent.NetworkTarget(access)
		if err != nil {
			agentError(w, err)
			return
		}
		hostname = target
		if host, _, err := net.SplitHostPort(target); err == nil {
			hostname, address = host, target
		} else {
			address = net.JoinHostPort(hostname, "443")
		}
	}
	var route network.Route
	switch in.Mode {
	case "", "configured":
		route, err = s.Mail.Network.Resolve(in.Scope, in.AccountID)
	case "direct":
		route.Key = "direct"
	case "proxy":
		route, err = s.Mail.Network.ProfileRoute(in.ProxyID)
	default:
		err = network.ErrConfig
	}
	if err != nil {
		networkError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	probeCtx, probeCancel := context.WithTimeout(ctx, 20*time.Second)
	var checks []network.Check
	if in.Scope == "agent" {
		transport := modelprovider.ProxyTransport(route)
		checks = network.ProbeTLSWithDialer(probeCtx, transport.DialContext, address, hostname)
	} else {
		checks = network.ProbeTLS(probeCtx, route, address, hostname)
	}
	probeCancel()
	ok := len(checks) > 0 && checks[len(checks)-1].OK
	var folders []string
	if ok && in.Scope != "global" {
		start := time.Now()
		stage, code := "", "ok"
		switch in.Scope {
		case "account":
			stage = "imap_login_folders"
			folders, err = s.Mail.DiagnoseNetwork(ctx, in.AccountID, route)
			if err != nil {
				code = "imap_login_or_folders_failed"
				var coded interface{ OperationErrorCode() string }
				if errors.As(err, &coded) {
					code = coded.OperationErrorCode()
				}
			}
		case "telegram":
			stage = "telegram_get_me"
			token, _, credentialErr := s.Mail.TelegramCredential()
			err = credentialErr
			if err == nil {
				client := telegram.NewClient(token)
				client.HTTP = network.HTTPClient(s.Mail.Network.Transport(route), 15*time.Second)
				_, err = client.GetMe(ctx)
				if err != nil {
					code = err.Error()
				}
			} else {
				code = "telegram_credential_missing"
			}
		case "agent":
			stage = "agent_chat"
			err = s.Agent.DiagnoseNetwork(ctx, access, route)
			if err != nil {
				code = modelprovider.CodeOf(err)
			}
		}
		ok = err == nil
		checks = append(checks, network.Check{Stage: stage, OK: ok, Code: code, ElapsedMS: time.Since(start).Milliseconds()})
	}
	_ = store.Audit(s.Auth.Store.DB, "network.diagnose", in.Scope, map[bool]string{true: "ok", false: "failed"}[ok])
	writeJSON(w, 200, map[string]any{"ok": ok, "scope": in.Scope, "route": route.Label(), "target": address, "checks": checks, "folders": folders, "fallback": false})
}
