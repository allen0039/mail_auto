package modelprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Settings configures a Chat Completions endpoint. BaseURL is the API root
// (for example https://api.example.com/v1); the client appends
// /chat/completions. MaxTokensParam selects the output-limit parameter name
// ("" sends none). Strict and ParallelToolCalls are sent only when enabled.
type Settings struct {
	BaseURL           string
	Model             string
	APIKey            string
	Timeout           time.Duration
	MaxTokensParam    string
	Strict            bool
	ParallelToolCalls bool
}

// ChatCompletions implements Provider over the OpenAI-compatible
// /chat/completions endpoint with stream=false.
type ChatCompletions struct {
	settings Settings
	endpoint string
	client   *http.Client
	// MaxRetries bounds retries of retryable failures within ctx.
	MaxRetries int
	sleep      func(context.Context, time.Duration) error
}

// NewChatCompletions validates settings. rt nil selects the guarded
// production transport; tests inject a transport for a fake server.
func NewChatCompletions(s Settings, rt http.RoundTripper) (*ChatCompletions, error) {
	base, endpoint, err := NormalizeBaseURL(s.BaseURL)
	if err != nil {
		return nil, err
	}
	s.BaseURL = base
	s.Model = strings.TrimSpace(s.Model)
	if s.Model == "" || len(s.Model) > 200 || strings.ContainsAny(s.Model, "\r\n\x00") || s.APIKey == "" || len(s.APIKey) > 4096 || strings.ContainsAny(s.APIKey, "\r\n\x00 ") {
		return nil, &Error{Code: CodeConfig}
	}
	if s.MaxTokensParam != "" && s.MaxTokensParam != "max_tokens" && s.MaxTokensParam != "max_completion_tokens" {
		return nil, &Error{Code: CodeConfig}
	}
	if s.Timeout <= 0 || s.Timeout > 5*time.Minute {
		s.Timeout = 60 * time.Second
	}
	return &ChatCompletions{settings: s, endpoint: endpoint, client: NewHTTPClient(rt), MaxRetries: 2, sleep: sleepCtx}, nil
}

// Endpoint is the final request URL shown to administrators.
func (c *ChatCompletions) Endpoint() string { return c.endpoint }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
	Arguments   *string         `json:"arguments,omitempty"`
}
type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}
type wireCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type wireMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

func encodeMessages(in []Message) []wireMessage {
	out := make([]wireMessage, 0, len(in))
	for _, m := range in {
		content := m.Content
		w := wireMessage{Role: m.Role, Content: &content, ToolCallID: m.ToolCallID}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			if content == "" {
				w.Content = nil
			}
			for _, c := range m.ToolCalls {
				wc := wireCall{ID: c.ID, Type: "function"}
				wc.Function.Name = c.Name
				wc.Function.Arguments = c.Arguments
				w.ToolCalls = append(w.ToolCalls, wc)
			}
		}
		out = append(out, w)
	}
	return out
}

// Complete sends one request with bounded retries of network errors, 429
// and transient 5xx, honoring Retry-After within ctx. Authentication,
// request and schema failures are never retried.
func (c *ChatCompletions) Complete(ctx context.Context, r Request) (Response, error) {
	var last error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		resp, err := c.once(ctx, r)
		if err == nil {
			return resp, nil
		}
		last = err
		var pe *Error
		if !errors.As(err, &pe) || !pe.Retryable() || attempt == c.MaxRetries || ctx.Err() != nil {
			return Response{}, err
		}
		wait := pe.RetryAfter
		backoff := time.Duration(500*(1<<attempt))*time.Millisecond + time.Duration(rand.IntN(250))*time.Millisecond
		if wait < backoff {
			wait = backoff
		}
		if wait > 20*time.Second {
			wait = 20 * time.Second
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
			return Response{}, err
		}
		if c.sleep(ctx, wait) != nil {
			return Response{}, &Error{Code: CodeTimeout}
		}
	}
	return Response{}, last
}

func (c *ChatCompletions) once(ctx context.Context, r Request) (Response, error) {
	body := map[string]any{"model": c.settings.Model, "messages": encodeMessages(r.Messages), "stream": false}
	if len(r.Tools) > 0 {
		tools := make([]wireTool, 0, len(r.Tools))
		for _, t := range r.Tools {
			tools = append(tools, wireTool{Type: "function", Function: wireFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters, Strict: c.settings.Strict}})
		}
		body["tools"] = tools
		if c.settings.ParallelToolCalls {
			body["parallel_tool_calls"] = true
		}
	}
	if r.MaxOutputTokens > 0 && c.settings.MaxTokensParam != "" {
		body[c.settings.MaxTokensParam] = r.MaxOutputTokens
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, &Error{Code: CodeConfig}
	}
	ctx, cancel := context.WithTimeout(ctx, c.settings.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(raw))
	if err != nil {
		return Response{}, &Error{Code: CodeConfig}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.settings.APIKey)
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, &Error{Code: CodeTimeout}
		}
		if strings.Contains(err.Error(), errBlockedDestination.Error()) {
			return Response{}, &Error{Code: CodeDestination}
		}
		return Response{}, &Error{Code: CodeNetwork}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, &Error{Code: CodeTimeout}
		}
		return Response{}, &Error{Code: CodeNetwork}
	}
	if len(data) > MaxResponseBytes {
		return Response{}, &Error{Code: CodeTooLarge, Status: resp.StatusCode}
	}
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return Response{}, &Error{Code: CodeRedirect, Status: resp.StatusCode}
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return Response{}, &Error{Code: CodeAuth, Status: resp.StatusCode}
	case resp.StatusCode == 429:
		return Response{}, &Error{Code: CodeRateLimited, Status: 429, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	case resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504 || resp.StatusCode == 529:
		return Response{}, &Error{Code: CodeUnavailable, Status: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return Response{}, &Error{Code: CodeBadRequest, Status: resp.StatusCode}
	}
	return decodeResponse(data)
}

func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
		return min(time.Duration(n)*time.Second, 60*time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0), 60*time.Second)
	}
	return 0
}

// decodeResponse accepts only a complete, well-formed first choice. Truncated
// output, malformed or oversized tool arguments and empty responses are
// errors, so nothing partial ever reaches the tool layer.
func decodeResponse(data []byte) (Response, error) {
	var w struct {
		Choices []struct {
			Message struct {
				Role      string     `json:"role"`
				Content   *string    `json:"content"`
				ToolCalls []wireCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &w); err != nil || len(w.Choices) == 0 {
		return Response{}, &Error{Code: CodeInvalidResponse}
	}
	ch := w.Choices[0]
	out := Response{FinishReason: ch.FinishReason}
	if w.Usage != nil {
		out.Usage = Usage{Known: true, PromptTokens: w.Usage.PromptTokens, CompletionTokens: w.Usage.CompletionTokens, TotalTokens: w.Usage.TotalTokens}
	}
	if ch.FinishReason == "length" {
		return Response{}, &Error{Code: CodeTruncated}
	}
	if ch.Message.Content != nil {
		out.Content = *ch.Message.Content
	}
	seen := map[string]bool{}
	for _, c := range ch.Message.ToolCalls {
		if c.ID == "" || len(c.ID) > 200 || seen[c.ID] || c.Function.Name == "" || len(c.Function.Name) > 64 || (c.Type != "" && c.Type != "function") {
			return Response{}, &Error{Code: CodeInvalidResponse}
		}
		seen[c.ID] = true
		args := strings.TrimSpace(c.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		if len(args) > MaxToolArgBytes || !json.Valid([]byte(args)) || args[0] != '{' {
			return Response{}, &Error{Code: CodeInvalidResponse}
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: args})
	}
	if out.Content == "" && len(out.ToolCalls) == 0 {
		return Response{}, &Error{Code: CodeEmpty}
	}
	if len(out.ToolCalls) > 0 && ch.FinishReason != "" && ch.FinishReason != "tool_calls" && ch.FinishReason != "stop" && ch.FinishReason != "function_call" {
		return Response{}, &Error{Code: CodeInvalidResponse}
	}
	return out, nil
}
