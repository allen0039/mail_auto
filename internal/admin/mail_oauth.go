package admin

import (
	"context"
	"errors"
	"mail-auto/internal/credentials"
	"mail-auto/internal/mail"
	"mail-auto/internal/mailoauth"
	"net/http"
	"net/url"
	"strings"
)

const mailOAuthCookie = "mail_auto_oauth_session"

type probeCredential struct{ credential mailoauth.Credential }

func mailOAuthDiagnostic(provider string, diagnostic error) error {
	if provider == "google" && errors.Is(diagnostic, mail.ErrCapability) {
		return mailoauth.Failure("oauth_gmail_capability_missing")
	}
	return diagnostic
}

func (source *probeCredential) Credential(context.Context, string, string) (mailoauth.Credential, error) {
	return source.credential, nil
}

func (server *Server) mailOAuthAPI(writer http.ResponseWriter, request *http.Request, session Session) bool {
	manager := server.Mail.OAuthManager()
	if request.URL.Path == "/api/settings/mail-oauth" && request.Method == http.MethodGet {
		apps := []mailoauth.App{}
		for _, provider := range []string{"google", "microsoft"} {
			app, err := manager.App(provider)
			if err != nil {
				operationError(writer, err)
				return true
			}
			apps = append(apps, app)
		}
		writeJSON(writer, 200, apps)
		return true
	}
	if strings.HasPrefix(request.URL.Path, "/api/settings/mail-oauth/") && request.Method == http.MethodPut {
		provider := strings.TrimPrefix(request.URL.Path, "/api/settings/mail-oauth/")
		var input mailoauth.AppInput
		if !decode(writer, request, &input) {
			return true
		}
		app, err := manager.SaveApp(provider, input)
		if err != nil {
			operationError(writer, err)
			return true
		}
		writeJSON(writer, 200, app)
		return true
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/accounts/"), "/")
	if !strings.HasPrefix(request.URL.Path, "/api/accounts/") || len(parts) != 2 || request.Method != http.MethodPost {
		return false
	}
	if parts[1] == "oauth-disconnect" {
		if err := manager.Disconnect(parts[0]); err != nil {
			operationError(writer, err)
		} else {
			writeJSON(writer, 200, map[string]string{"status": "本地授权已断开；如需撤销服务商授权，请在服务商安全页面操作"})
		}
		return true
	}
	if parts[1] != "oauth-start" {
		return false
	}
	account, err := server.Mail.Account(parts[0])
	if err != nil {
		operationError(writer, err)
		return true
	}
	location, err := manager.Start(request.Context(), account.ID, account.Provider, credentials.Digest(session.Token))
	if err != nil {
		operationError(writer, err)
		return true
	}
	http.SetCookie(writer, &http.Cookie{Name: mailOAuthCookie, Value: session.Token, Path: "/api/mail-oauth/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	writeJSON(writer, 200, map[string]string{"url": location})
	return true
}

func (server *Server) mailOAuthCallback(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Cache-Control", "no-store")
	result, accountID := "oauth_state_invalid", ""
	defer func() {
		http.SetCookie(writer, &http.Cookie{Name: mailOAuthCookie, Value: "", Path: "/api/mail-oauth/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
		query := url.Values{"mail_oauth": {result}}
		if accountID != "" {
			query.Set("account", accountID)
		}
		http.Redirect(writer, request, "/?"+query.Encode(), http.StatusSeeOther)
	}()
	if request.Method != http.MethodGet || len(request.URL.RawQuery) > 32768 {
		return
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/mail-oauth/"), "/")
	if len(parts) != 2 || parts[1] != "callback" || !mailoauth.IsProvider(parts[0]) {
		return
	}
	cookie, err := request.Cookie(mailOAuthCookie)
	if err != nil {
		cookie, err = request.Cookie(sessionCookie)
	}
	if err != nil {
		return
	}
	session, err := server.Auth.Session(cookie.Value)
	if err != nil || session.Restricted {
		return
	}
	manager := server.Mail.OAuthManager()
	state, err := manager.Consume(request.Context(), parts[0], request.URL.Query().Get("state"), credentials.Digest(session.Token))
	if err != nil {
		result = mailoauth.Code(err)
		return
	}
	accountID = state.Account.ID
	if request.URL.Query().Get("error") != "" {
		result = "oauth_denied"
		return
	}
	code := request.URL.Query().Get("code")
	if code == "" || len(code) > 16384 {
		return
	}
	token, identity, err := manager.Exchange(request.Context(), state, code)
	if err != nil {
		result = mailoauth.Code(err)
		return
	}
	account, err := server.Mail.Account(accountID)
	if err != nil {
		return
	}
	account.AuthSource = &probeCredential{credential: mailoauth.Credential{Mechanism: "XOAUTH2", Username: account.Username, Token: token.Access}}
	_, diagnostic := server.Mail.Remote.Folders(request.Context(), account, "")
	diagnostic = mailOAuthDiagnostic(account.Provider, diagnostic)
	if diagnostic != nil && mailoauth.Code(diagnostic) == "oauth_gmail_capability_missing" {
		result = mailoauth.Code(diagnostic)
		return
	}
	if diagnostic != nil && account.Provider == "microsoft" {
		result = mailoauth.Code(diagnostic)
		return
	}
	if err = manager.Bind(request.Context(), state, token, identity, diagnostic); err != nil {
		result = mailoauth.Code(err)
		return
	}
	result = "ok"
	if diagnostic != nil {
		result = "oauth_network_failed"
	}
}
