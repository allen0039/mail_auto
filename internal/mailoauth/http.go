package mailoauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

func readJSON(response *http.Response, output any) error {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return Failure("oauth_provider_unavailable")
	}
	if err = json.Unmarshal(body, output); err != nil {
		return Failure("oauth_provider_unavailable")
	}
	return nil
}

func (manager *Manager) tokenRequest(ctx context.Context, client *http.Client, app App, form url.Values) (Token, error) {
	secret, err := manager.Vault.Decrypt(app.Secret, "mail-oauth-app:"+app.Provider)
	if err != nil {
		return Token{}, err
	}
	form.Set("client_id", app.ClientID)
	form.Set("client_secret", secret)
	_, endpoint, _ := endpoints(app)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, Failure("oauth_network_failed")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return Token{}, Failure("oauth_network_failed")
	}
	var payload struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Type    string `json:"token_type"`
		Scope   string `json:"scope"`
		IDToken string `json:"id_token"`
		Expires int64  `json:"expires_in"`
		Error   string `json:"error"`
	}
	if err = readJSON(response, &payload); err != nil {
		return Token{}, err
	}
	if response.StatusCode == 429 {
		return Token{}, Failure("oauth_rate_limited")
	}
	if response.StatusCode >= 500 {
		return Token{}, Failure("oauth_provider_unavailable")
	}
	if response.StatusCode != 200 {
		switch payload.Error {
		case "invalid_grant", "interaction_required", "consent_required":
			return Token{}, Failure("oauth_reauthorization_required")
		case "invalid_scope", "insufficient_scope", "access_denied":
			return Token{}, Failure("oauth_scope_missing")
		case "invalid_client", "unauthorized_client":
			return Token{}, Failure("oauth_config_invalid")
		default:
			return Token{}, Failure("oauth_provider_unavailable")
		}
	}
	if payload.Type != "Bearer" || payload.Access == "" || len(payload.Access) > 32768 || strings.ContainsAny(payload.Access, "\r\n\x00\x01") || payload.Expires < 1 || payload.Expires > 86400 {
		return Token{}, Failure("oauth_provider_unavailable")
	}
	if payload.Scope == "" && form.Get("grant_type") == "refresh_token" {
		payload.Scope = Scope(app.Provider)
	}
	if !slices.Contains(strings.Fields(payload.Scope), Scope(app.Provider)) {
		return Token{}, Failure("oauth_scope_missing")
	}
	return Token{Access: payload.Access, Refresh: payload.Refresh, Scope: payload.Scope, IDToken: payload.IDToken, Expires: time.Now().Add(time.Duration(payload.Expires) * time.Second)}, nil
}
