package modelprovider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Model listing bounds. Lists are fetched only on explicit request and are
// never cached on the server.
const (
	MaxModelPages = 5
	MaxModels     = 2000
)

// ListModels reads GET {base}/models with the given key. Only the OpenAI
// style {"data":[{"id":...}]} body is accepted. Pagination is followed only
// in the has_more/last_id form by requesting the same URL with after_id; a
// next-page URL supplied by the server is never followed, so the key never
// leaves the configured origin. Redirects are refused by the client.
func ListModels(ctx context.Context, rt http.RoundTripper, baseURL, apiKey string) ([]string, error) {
	base, _, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if apiKey == "" || len(apiKey) > 4096 || strings.ContainsAny(apiKey, "\r\n\x00 ") {
		return nil, &Error{Code: CodeConfig}
	}
	client := NewHTTPClient(rt)
	seen := map[string]bool{}
	out := []string{}
	after := ""
	for page := 0; page < MaxModelPages; page++ {
		endpoint := base + "/models"
		if after != "" {
			endpoint += "?" + url.Values{"after_id": {after}}.Encode()
		}
		ids, more, last, err := listPage(ctx, client, endpoint, apiKey)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if !seen[id] && len(out) < MaxModels {
				seen[id] = true
				out = append(out, id)
			}
		}
		if !more || last == "" || last == after || len(out) >= MaxModels {
			break
		}
		after = last
	}
	sort.Strings(out)
	return out, nil
}

func listPage(ctx context.Context, client *http.Client, endpoint, key string) ([]string, bool, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, "", &Error{Code: CodeConfig}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, "", &Error{Code: CodeTimeout}
		}
		var classified *Error
		if errors.As(err, &classified) {
			return nil, false, "", classified
		}
		if strings.Contains(err.Error(), errBlockedDestination.Error()) {
			return nil, false, "", &Error{Code: CodeDestination}
		}
		return nil, false, "", &Error{Code: CodeNetwork}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, "", &Error{Code: CodeTimeout}
		}
		return nil, false, "", &Error{Code: CodeNetwork}
	}
	switch {
	case len(data) > MaxResponseBytes:
		return nil, false, "", &Error{Code: CodeTooLarge, Status: resp.StatusCode}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, false, "", &Error{Code: CodeRedirect, Status: resp.StatusCode}
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return nil, false, "", &Error{Code: CodeAuth, Status: resp.StatusCode}
	case resp.StatusCode == 404 || resp.StatusCode == 405:
		return nil, false, "", &Error{Code: CodeModelsUnsupported, Status: resp.StatusCode}
	case resp.StatusCode == 429:
		return nil, false, "", &Error{Code: CodeRateLimited, Status: 429}
	case resp.StatusCode >= 500:
		return nil, false, "", &Error{Code: CodeUnavailable, Status: resp.StatusCode}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, false, "", &Error{Code: CodeBadRequest, Status: resp.StatusCode}
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		LastID  string `json:"last_id"`
	}
	if json.Unmarshal(data, &body) != nil || body.Data == nil {
		return nil, false, "", &Error{Code: CodeModelsUnsupported}
	}
	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		id := strings.TrimSpace(m.ID)
		if id != "" && len(id) <= 200 && !strings.ContainsAny(id, "\r\n\x00") {
			ids = append(ids, id)
		}
	}
	return ids, body.HasMore, body.LastID, nil
}
