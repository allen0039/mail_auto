package modelprovider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rewrite sends requests for the configured public test host to a local fake
// server. Only tests inject it; production always uses GuardedTransport.
type rewrite struct{ target *url.URL }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = r.target.Scheme
	clone.URL.Host = r.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

type fakeModel struct {
	mu       sync.Mutex
	requests []map[string]any
	auth     []string
	handler  func(n int, body map[string]any, w http.ResponseWriter)
}

func (f *fakeModel) server(t *testing.T) (*httptest.Server, *ChatCompletions) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.requests = append(f.requests, body)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		n := len(f.requests)
		f.mu.Unlock()
		f.handler(n, body, w)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	c, err := NewChatCompletions(Settings{BaseURL: "https://api.model.example/v1/", Model: "test-model", APIKey: "sk-test-secret-value", Timeout: 2 * time.Second}, rewrite{u})
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	return srv, c
}

func reply(w http.ResponseWriter, msg map[string]any, finish string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg, "finish_reason": finish}}})
}

func TestProbeDistinguishesChatFromToolLoop(t *testing.T) {
	chatOnly := &fakeModel{handler: func(n int, body map[string]any, w http.ResponseWriter) {
		reply(w, map[string]any{"role": "assistant", "content": "OK"}, "stop")
	}}
	_, c := chatOnly.server(t)
	if err := ProbeChat(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := ProbeToolLoop(context.Background(), c); CodeOf(err) != CodeToolUnsupported {
		t.Fatal("chat-only model passed tool loop", err)
	}
	var nonce string
	loop := &fakeModel{handler: func(n int, body map[string]any, w http.ResponseWriter) {
		msgs := body["messages"].([]any)
		if n == 1 {
			text := msgs[1].(map[string]any)["content"].(string)
			nonce = text[strings.Index(text, "probe-") : strings.Index(text, "probe-")+18]
			args, _ := json.Marshal(map[string]string{"text": nonce})
			reply(w, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_a", "type": "function", "function": map[string]any{"name": "echo", "arguments": string(args)}}}}, "tool_calls")
			return
		}
		last := msgs[len(msgs)-1].(map[string]any)
		assistant := msgs[len(msgs)-2].(map[string]any)
		if last["role"] != "tool" || last["tool_call_id"] != "call_a" || assistant["tool_calls"] == nil || !strings.Contains(last["content"].(string), nonce) {
			w.WriteHeader(400)
			return
		}
		reply(w, map[string]any{"role": "assistant", "content": nonce}, "stop")
	}}
	_, c = loop.server(t)
	if err := ProbeToolLoop(context.Background(), c); err != nil {
		t.Fatal("tool loop failed", err)
	}
	if len(loop.requests) != 2 || loop.auth[0] != "Bearer sk-test-secret-value" {
		t.Fatal(loop.requests, loop.auth)
	}
	if loop.requests[0]["stream"] != false || loop.requests[0]["temperature"] != nil {
		t.Fatal("unexpected optional parameters", loop.requests[0])
	}
}

func TestDecodeRejectsPartialOrInvalidToolCalls(t *testing.T) {
	good := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"a","type":"function","function":{"name":"list","arguments":"{\"q\":\"中文 \\\"引号\\\" 😀\"}"}},{"id":"b","type":"function","function":{"name":"get","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	resp, err := decodeResponse([]byte(good))
	if err != nil || len(resp.ToolCalls) != 2 || resp.ToolCalls[0].ID != "a" || resp.ToolCalls[1].Name != "get" || resp.Usage.Known {
		t.Fatal(resp, err)
	}
	var args map[string]string
	if json.Unmarshal([]byte(resp.ToolCalls[0].Arguments), &args) != nil || args["q"] != "中文 \"引号\" 😀" {
		t.Fatal(resp.ToolCalls[0].Arguments)
	}
	for name, body := range map[string]string{
		"truncated":  `{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`,
		"cut args":   `{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"x","arguments":"{\"a\":"}}]},"finish_reason":"tool_calls"}]}`,
		"array args": `{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"x","arguments":"[1]"}}]},"finish_reason":"tool_calls"}]}`,
		"dup ids":    `{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"x","arguments":"{}"}},{"id":"a","function":{"name":"y","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		"no id":      `{"choices":[{"message":{"tool_calls":[{"id":"","function":{"name":"x","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		"empty":      `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`,
		"no choices": `{"choices":[]}`,
		"not json":   `<html>`,
		"trailing":   `{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"x","arguments":"{} {}"}}]},"finish_reason":"tool_calls"}]}`,
	} {
		if _, err := decodeResponse([]byte(body)); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	big := `{"choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":"x","arguments":"{\"a\":\"` + strings.Repeat("x", MaxToolArgBytes) + `\"}"}}]},"finish_reason":"tool_calls"}]}`
	if _, err := decodeResponse([]byte(big)); err == nil {
		t.Fatal("oversized arguments accepted")
	}
	withUsage := `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	if resp, err := decodeResponse([]byte(withUsage)); err != nil || !resp.Usage.Known || resp.Usage.TotalTokens != 4 {
		t.Fatal(resp, err)
	}
}

func TestProviderErrorsRetriesAndNoLeak(t *testing.T) {
	cases := []struct {
		name     string
		status   []int
		code     string
		attempts int
	}{
		{"auth", []int{401}, CodeAuth, 1},
		{"forbidden", []int{403}, CodeAuth, 1},
		{"bad request", []int{400}, CodeBadRequest, 1},
		{"rate limit then ok", []int{429, 200}, "", 2},
		{"5xx exhausted", []int{503, 502, 500}, CodeUnavailable, 3},
	}
	for _, tc := range cases {
		f := &fakeModel{handler: func(n int, body map[string]any, w http.ResponseWriter) {
			status := tc.status[min(n-1, len(tc.status)-1)]
			if status == 200 {
				reply(w, map[string]any{"content": "ok"}, "stop")
				return
			}
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key sk-test-secret-value"}}`)
		}}
		_, c := f.server(t)
		_, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
		if CodeOf(err) != tc.code && !(tc.code == "" && err == nil) {
			t.Fatal(tc.name, err)
		}
		if err != nil && strings.Contains(err.Error(), "sk-") {
			t.Fatal("provider body leaked", err)
		}
		if len(f.requests) != tc.attempts {
			t.Fatal(tc.name, "attempts", len(f.requests))
		}
	}
	slow := &fakeModel{handler: func(n int, body map[string]any, w http.ResponseWriter) {
		time.Sleep(300 * time.Millisecond)
		reply(w, map[string]any{"content": "late"}, "stop")
	}}
	_, c := slow.server(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Complete(ctx, Request{Messages: []Message{{Role: "user", Content: "hi"}}}); CodeOf(err) != CodeTimeout {
		t.Fatal("timeout not classified", err)
	}
	huge := &fakeModel{handler: func(n int, body map[string]any, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + strings.Repeat("x", MaxResponseBytes) + `"}}]}`))
	}}
	_, c = huge.server(t)
	if _, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}}); CodeOf(err) != CodeTooLarge {
		t.Fatal("oversized body accepted", err)
	}
}

func TestDestinationGuard(t *testing.T) {
	for _, raw := range []string{"http://api.example.com/v1", "https://user:pw@api.example.com/v1", "https://api.example.com/v1?x=1", "https://api.example.com/v1#f", "https://localhost/v1", "https://127.0.0.1/v1", "https://[::1]/v1", "https://169.254.169.254/v1", "https://10.0.0.8/v1", "https://[fd00::1]/v1", "https://api.example.com/v1/chat/completions", "ftp://api.example.com"} {
		if _, _, err := NormalizeBaseURL(raw); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	base, endpoint, err := NormalizeBaseURL("https://api.example.com/v1/")
	if err != nil || base != "https://api.example.com/v1" || endpoint != "https://api.example.com/v1/chat/completions" {
		t.Fatal(base, endpoint, err)
	}
	for addr, public := range map[string]bool{"8.8.8.8": true, "2606:4700:4700::1111": true, "127.0.0.1": false, "::1": false, "169.254.169.254": false, "100.64.1.1": false, "::ffff:10.1.2.3": false, "fe80::1": false, "fd12::1": false, "0.0.0.0": false, "224.0.0.1": false, "64:ff9b::a00:1": false, "2002:a00:1::": false} {
		if PublicAddress(netip.MustParseAddr(addr)) != public {
			t.Fatal(addr)
		}
	}
	if GuardedTransport().Proxy != nil {
		t.Fatal("environment proxy enabled")
	}
	// The dial-time check blocks a loopback destination even when the URL
	// passed earlier validation (DNS rebinding to 127.0.0.1).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("guarded transport reached loopback") }))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL, nil)
	if _, err := GuardedTransport().RoundTrip(req); err == nil || !strings.Contains(err.Error(), errBlockedDestination.Error()) {
		t.Fatal("loopback dial allowed", err)
	}
	// Redirects are never followed, so the key is not forwarded.
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	f := &fakeModel{handler: func(n int, body map[string]any, w http.ResponseWriter) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(302)
	}}
	_, c := f.server(t)
	if _, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}}); CodeOf(err) != CodeRedirect || redirected.Load() != 0 {
		t.Fatal("redirect followed", err, redirected.Load())
	}
	if _, err := NewChatCompletions(Settings{BaseURL: "https://api.example.com/v1", Model: "m", APIKey: "k", MaxTokensParam: "max_output"}, nil); err == nil {
		t.Fatal("unknown output parameter accepted")
	}
	var pe *Error
	if !errors.As(&Error{Code: CodeRateLimited}, &pe) || !pe.Retryable() {
		t.Fatal("rate limit should be retryable")
	}
}
