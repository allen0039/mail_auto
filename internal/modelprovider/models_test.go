package modelprovider

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type modelsRT struct {
	reqs []*http.Request
	page func(*http.Request) (int, string)
}

func (m *modelsRT) RoundTrip(r *http.Request) (*http.Response, error) {
	m.reqs = append(m.reqs, r)
	status, body := m.page(r)
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func TestListModelsPagesWithinOriginAndSorts(t *testing.T) {
	rt := &modelsRT{page: func(r *http.Request) (int, string) {
		switch r.URL.Query().Get("after_id") {
		case "":
			// A server-supplied next URL must never be followed.
			return 200, `{"data":[{"id":"z-model"},{"id":"a-model"}],"has_more":true,"last_id":"a-model","next":"https://evil.example/steal"}`
		case "a-model":
			return 200, `{"data":[{"id":"m-model"},{"id":"a-model"},{"id":"bad\nid"}],"has_more":false}`
		}
		return 500, ""
	}}
	got, err := ListModels(context.Background(), rt, "https://api.example.com/v1/", "sk-1")
	if err != nil || strings.Join(got, ",") != "a-model,m-model,z-model" {
		t.Fatal(got, err)
	}
	if len(rt.reqs) != 2 {
		t.Fatal(len(rt.reqs))
	}
	for _, r := range rt.reqs {
		if r.URL.Host != "api.example.com" || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer sk-1" || r.Method != http.MethodGet {
			t.Fatal(r.URL, r.Header)
		}
	}
}

func TestListModelsBoundsAndErrors(t *testing.T) {
	// A server that always claims more pages is cut off.
	n := 0
	loop := &modelsRT{page: func(*http.Request) (int, string) {
		n++
		return 200, `{"data":[{"id":"m` + strings.Repeat("x", n) + `"}],"has_more":true,"last_id":"m` + strings.Repeat("x", n) + `"}`
	}}
	if got, err := ListModels(context.Background(), loop, "https://api.example.com/v1", "k"); err != nil || len(loop.reqs) != MaxModelPages || len(got) != MaxModelPages {
		t.Fatal(len(loop.reqs), got, err)
	}
	for status, code := range map[int]string{401: CodeAuth, 404: CodeModelsUnsupported, 429: CodeRateLimited, 502: CodeUnavailable, 302: CodeRedirect, 400: CodeBadRequest} {
		rt := &modelsRT{page: func(*http.Request) (int, string) { return status, `{}` }}
		if _, err := ListModels(context.Background(), rt, "https://api.example.com/v1", "k"); CodeOf(err) != code {
			t.Fatal(status, err)
		}
	}
	notList := &modelsRT{page: func(*http.Request) (int, string) { return 200, `{"object":"error"}` }}
	if _, err := ListModels(context.Background(), notList, "https://api.example.com/v1", "k"); CodeOf(err) != CodeModelsUnsupported {
		t.Fatal(err)
	}
	if _, err := ListModels(context.Background(), notList, "http://api.example.com/v1", "k"); CodeOf(err) != CodeConfig {
		t.Fatal("http accepted", err)
	}
	if _, err := ListModels(context.Background(), notList, "https://127.0.0.1/v1", "k"); CodeOf(err) != CodeDestination {
		t.Fatal("private address accepted", err)
	}
	if _, err := ListModels(context.Background(), notList, "https://api.example.com/v1", ""); CodeOf(err) != CodeConfig {
		t.Fatal("empty key accepted", err)
	}
}
