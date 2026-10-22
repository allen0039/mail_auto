package admin

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
)

func hashOAuth(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func (s *Server) mcpHost(r *http.Request) bool {
	u, e := url.Parse(s.requestMCP(r).PublicURL)
	return e == nil && u.Scheme == "https" && strings.EqualFold(r.Host, u.Host)
}
func oauthError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
func safeRedirect(raw string) bool {
	if len(raw) > 2048 {
		return false
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.Fragment != "" || u.User != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && (u.Hostname() == "localhost" || (net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback()))
}
func (s *Server) oauthPublic(w http.ResponseWriter, r *http.Request) {
	if s.requestMCP(r).handler == nil {
		http.NotFound(w, r)
		return
	}
	if !s.mcpHost(r) {
		oauthError(w, 421, "invalid_host")
		return
	}
	base := s.requestMCP(r).PublicURL
	resource := base + "/mcp"
	switch {
	case (r.URL.Path == "/.well-known/oauth-protected-resource" || r.URL.Path == "/.well-known/oauth-protected-resource/mcp") && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"resource": resource, "authorization_servers": []string{base}, "scopes_supported": []string{"mail.read", "mail.write", "offline_access"}, "bearer_methods_supported": []string{"header"}})
	case (r.URL.Path == "/.well-known/oauth-authorization-server" || r.URL.Path == "/.well-known/oauth-authorization-server/mcp") && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"issuer": base, "authorization_endpoint": base + "/oauth/authorize", "token_endpoint": base + "/oauth/token", "registration_endpoint": base + "/oauth/register", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{"mail.read", "mail.write", "offline_access"}})
	case r.URL.Path == "/oauth/register" && r.Method == http.MethodPost:
		var in struct {
			ClientName string   `json:"client_name"`
			Redirects  []string `json:"redirect_uris"`
			Auth       string   `json:"token_endpoint_auth_method"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		dec := json.NewDecoder(r.Body)
		if dec.Decode(&in) != nil || len(in.Redirects) < 1 || len(in.Redirects) > 5 || len(in.ClientName) > 80 || (in.Auth != "" && in.Auth != "none") {
			oauthError(w, 400, "invalid_client_metadata")
			return
		}
		for _, uri := range in.Redirects {
			if !safeRedirect(uri) {
				oauthError(w, 400, "invalid_redirect_uri")
				return
			}
		}
		var count int
		if s.Auth.Store.DB.QueryRow("SELECT count(*) FROM oauth_clients").Scan(&count) != nil || count >= 1000 {
			oauthError(w, 429, "registration_limit")
			return
		}
		id := credentials.Random(24)
		raw, _ := json.Marshal(in.Redirects)
		if _, e := s.Auth.Store.DB.Exec("INSERT INTO oauth_clients VALUES(?,?,?,?)", id, in.ClientName, string(raw), time.Now().Unix()); e != nil {
			oauthError(w, 500, "server_error")
			return
		}
		writeJSON(w, 201, map[string]any{"client_id": id, "client_name": in.ClientName, "redirect_uris": in.Redirects, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
	case r.URL.Path == "/oauth/authorize" && r.Method == http.MethodGet:
		q := r.URL.Query()
		challenge := q.Get("code_challenge")
		decoded, e := base64.RawURLEncoding.DecodeString(challenge)
		if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || e != nil || len(decoded) != 32 || len(challenge) != 43 || len(q.Get("state")) > 1024 || (q.Get("resource") != "" && q.Get("resource") != resource) {
			oauthError(w, 400, "invalid_request")
			return
		}
		var redirects string
		if s.Auth.Store.DB.QueryRow("SELECT redirects FROM oauth_clients WHERE id=?", q.Get("client_id")).Scan(&redirects) != nil {
			oauthError(w, 400, "invalid_client")
			return
		}
		var registered []string
		if json.Unmarshal([]byte(redirects), &registered) != nil || !slices.Contains(registered, q.Get("redirect_uri")) {
			oauthError(w, 400, "invalid_redirect_uri")
			return
		}
		scope := q.Get("scope")
		if scope == "" {
			scope = "mail.read mail.write offline_access"
		}
		for _, v := range strings.Fields(scope) {
			if !slices.Contains([]string{"mail.read", "mail.write", "offline_access"}, v) {
				oauthError(w, 400, "invalid_scope")
				return
			}
		}
		_, _ = s.Auth.Store.DB.Exec("DELETE FROM oauth_requests WHERE expires<?", time.Now().Unix())
		var n int
		if s.Auth.Store.DB.QueryRow("SELECT count(*) FROM oauth_requests").Scan(&n) != nil || n >= 1000 {
			oauthError(w, 429, "request_limit")
			return
		}
		id := credentials.Random(24)
		_, e = s.Auth.Store.DB.Exec("INSERT INTO oauth_requests VALUES(?,?,?,?,?,?,?)", id, q.Get("client_id"), q.Get("redirect_uri"), q.Get("state"), challenge, scope, time.Now().Add(10*time.Minute).Unix())
		if e != nil {
			oauthError(w, 500, "server_error")
			return
		}
		http.Redirect(w, r, "/?oauth_request="+url.QueryEscape(id), http.StatusFound)
	case r.URL.Path == "/oauth/token" && r.Method == http.MethodPost:
		s.oauthToken(w, r)
	default:
		http.NotFound(w, r)
	}
}
func (s *Server) oauthRequest(w http.ResponseWriter, r *http.Request, session Session) {
	id := strings.TrimPrefix(r.URL.Path, "/api/oauth/requests/")
	var client, redirect, state, challenge, scope string
	var expires int64
	e := s.Auth.Store.DB.QueryRow("SELECT client_id,redirect_uri,state,challenge,scope,expires FROM oauth_requests WHERE id=?", id).Scan(&client, &redirect, &state, &challenge, &scope, &expires)
	if e != nil || expires <= time.Now().Unix() {
		oauthError(w, 400, "expired_request")
		return
	}
	if r.Method == http.MethodGet {
		var name string
		_ = s.Auth.Store.DB.QueryRow("SELECT name FROM oauth_clients WHERE id=?", client).Scan(&name)
		writeJSON(w, 200, map[string]any{"id": id, "client_name": name, "client_id": client, "redirect_uri": redirect, "scope": scope, "expires": expires})
		return
	}
	if r.Method != http.MethodPost {
		oauthError(w, 405, "method_not_allowed")
		return
	}
	var in struct {
		Accounts []string `json:"accounts"`
		Actions  []string `json:"actions"`
		Deny     bool     `json:"deny"`
	}
	if !decode(w, r, &in) {
		return
	}
	u, _ := url.Parse(redirect)
	q := u.Query()
	q.Set("state", state)
	tx, e := s.Auth.Store.DB.Begin()
	if e != nil {
		oauthError(w, 500, "server_error")
		return
	}
	defer tx.Rollback()
	res, e := tx.Exec("DELETE FROM oauth_requests WHERE id=? AND expires>?", id, time.Now().Unix())
	if e != nil {
		oauthError(w, 500, "server_error")
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		oauthError(w, 400, "invalid_request")
		return
	}
	if in.Deny {
		q.Set("error", "access_denied")
	} else {
		if len(in.Accounts) < 1 || len(in.Accounts) > 10 {
			oauthError(w, 400, "invalid_scope")
			return
		}
		actions := []string{"read"}
		writeAllowed := slices.Contains(strings.Fields(scope), "mail.write")
		for _, action := range in.Actions {
			if action == "read" {
				continue
			}
			if !writeAllowed || !slices.Contains([]string{"folder_manage", "archive", "move", "trash", "restore"}, action) {
				oauthError(w, 400, "invalid_scope")
				return
			}
			actions = append(actions, action)
		}
		folders := map[string][]string{}
		for _, account := range in.Accounts {
			var raw string
			if tx.QueryRow("SELECT folders FROM accounts WHERE id=?", account).Scan(&raw) != nil {
				oauthError(w, 400, "invalid_account")
				return
			}
			var fs []string
			if json.Unmarshal([]byte(raw), &fs) != nil {
				oauthError(w, 500, "server_error")
				return
			}
			folders[account] = fs
		}
		payload, _ := json.Marshal(struct {
			mail.TokenInput
			Issuer string `json:"issuer"`
		}{mail.TokenInput{Name: "OAuth MCP client", Accounts: in.Accounts, Folders: folders, Actions: actions, Days: 1}, s.requestMCP(r).PublicURL})
		code := credentials.Random(32)
		granted := "mail.read"
		if len(actions) > 1 {
			granted += " mail.write"
		}
		if slices.Contains(strings.Fields(scope), "offline_access") {
			granted += " offline_access"
		}
		_, e = tx.Exec("INSERT INTO oauth_codes VALUES(?,?,?,?,?,?,?,?,0)", hashOAuth(code), client, redirect, challenge, string(payload), session.Version, granted, time.Now().Add(2*time.Minute).Unix())
		if e != nil {
			oauthError(w, 500, "server_error")
			return
		}
		q.Set("code", code)
	}
	if e = tx.Commit(); e != nil {
		oauthError(w, 500, "server_error")
		return
	}
	u.RawQuery = q.Encode()
	writeJSON(w, 200, map[string]string{"redirect": u.String()})
}
func (s *Server) oauthToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil {
		oauthError(w, 400, "invalid_request")
		return
	}
	f := r.PostForm
	if f.Get("resource") != "" && f.Get("resource") != s.requestMCP(r).PublicURL+"/mcp" {
		oauthError(w, 400, "invalid_target")
		return
	}
	var payload, scope, oldToken string
	var version int
	tx, e := s.Auth.Store.DB.Begin()
	if e != nil {
		oauthError(w, 500, "server_error")
		return
	}
	defer tx.Rollback()
	switch f.Get("grant_type") {
	case "authorization_code":
		verifier := f.Get("code_verifier")
		if len(verifier) < 43 || len(verifier) > 128 {
			oauthError(w, 400, "invalid_grant")
			return
		}
		var challenge string
		e = tx.QueryRow("SELECT payload,admin_version,scope,challenge FROM oauth_codes WHERE hash=? AND client_id=? AND redirect_uri=? AND expires>? AND consumed=0", hashOAuth(f.Get("code")), f.Get("client_id"), f.Get("redirect_uri"), time.Now().Unix()).Scan(&payload, &version, &scope, &challenge)
		hash := sha256.Sum256([]byte(verifier))
		expected := base64.RawURLEncoding.EncodeToString(hash[:])
		if e != nil || subtle.ConstantTimeCompare([]byte(expected), []byte(challenge)) != 1 {
			oauthError(w, 400, "invalid_grant")
			return
		}
		_, e = tx.Exec("UPDATE oauth_codes SET consumed=1 WHERE hash=?", hashOAuth(f.Get("code")))
	case "refresh_token":
		e = tx.QueryRow("SELECT payload,admin_version,scope,token_id FROM oauth_refresh WHERE hash=? AND client_id=? AND expires>? AND consumed=0", hashOAuth(f.Get("refresh_token")), f.Get("client_id"), time.Now().Unix()).Scan(&payload, &version, &scope, &oldToken)
		if e != nil {
			oauthError(w, 400, "invalid_grant")
			return
		}
		var revoked int
		if tx.QueryRow("SELECT revoked FROM mcp_tokens WHERE id=?", oldToken).Scan(&revoked) != nil || revoked != 0 {
			oauthError(w, 400, "invalid_grant")
			return
		}
		_, e = tx.Exec("UPDATE oauth_refresh SET consumed=1 WHERE hash=?", hashOAuth(f.Get("refresh_token")))
		if e == nil {
			_, e = tx.Exec("UPDATE mcp_tokens SET revoked=1 WHERE id=?", oldToken)
		}
	default:
		oauthError(w, 400, "unsupported_grant_type")
		return
	}
	var current, mustChange int
	if e != nil || tx.QueryRow("SELECT version,must_change FROM admin WHERE id=1").Scan(&current, &mustChange) != nil || current != version || mustChange != 0 {
		oauthError(w, 400, "invalid_grant")
		return
	}
	if e = tx.Commit(); e != nil {
		oauthError(w, 500, "server_error")
		return
	}
	var input struct {
		mail.TokenInput
		Issuer string `json:"issuer"`
	}
	if json.Unmarshal([]byte(payload), &input) != nil || input.Issuer != s.requestMCP(r).PublicURL {
		oauthError(w, 500, "server_error")
		return
	}
	token, secret, e := s.Mail.CreateToken(input.TokenInput)
	if e != nil {
		oauthError(w, 400, "invalid_grant")
		return
	}
	if token.Access.Version != version || !s.currentMCP().Enabled || s.currentMCP().PublicURL != input.Issuer {
		_ = s.Mail.RevokeToken(token.ID)
		oauthError(w, 400, "invalid_grant")
		return
	}
	if _, e = s.Auth.Store.DB.Exec("UPDATE mcp_tokens SET expires=? WHERE id=?", time.Now().Add(30*time.Minute).Unix(), token.ID); e != nil {
		_ = s.Mail.RevokeToken(token.ID)
		oauthError(w, 500, "server_error")
		return
	}
	out := map[string]any{"access_token": secret, "token_type": "Bearer", "expires_in": 1800, "scope": scope, "resource": s.requestMCP(r).PublicURL + "/mcp"}
	if slices.Contains(strings.Fields(scope), "offline_access") {
		refresh := credentials.Random(32)
		_, e = s.Auth.Store.DB.Exec("INSERT INTO oauth_refresh VALUES(?,?,?,?,?,?,?,0)", hashOAuth(refresh), f.Get("client_id"), token.ID, payload, version, scope, time.Now().Add(30*24*time.Hour).Unix())
		if e != nil {
			_ = s.Mail.RevokeToken(token.ID)
			oauthError(w, 500, "server_error")
			return
		}
		out["refresh_token"] = refresh
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, out)
}
