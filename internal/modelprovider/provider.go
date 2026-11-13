// Package modelprovider talks to model APIs. It has no access to mailboxes or
// the business database; callers pass messages and tool definitions and get
// back assistant text and complete, syntactically valid tool calls.
package modelprovider

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Message is a provider-neutral chat message. Assistant messages may carry
// tool calls; tool messages carry the matching ToolCallID.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall holds the provider's call ID, the tool name and the raw JSON
// arguments exactly as returned (validated to be one JSON object).
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Request struct {
	Messages        []Message
	Tools           []ToolDef
	MaxOutputTokens int
}

// Usage is reported only when the provider returns it; Known=false means the
// cost is unknown and must not be displayed as a real figure.
type Usage struct {
	Known            bool `json:"known"`
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	TotalTokens      int  `json:"total_tokens"`
}

type Response struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

// Provider is the model interface used by the agent runtime.
type Provider interface {
	Complete(ctx context.Context, r Request) (Response, error)
}

// Fixed error codes. Raw provider bodies never leave this package.
const (
	CodeAuth            = "provider_auth_failed"
	CodeRateLimited     = "provider_rate_limited"
	CodeUnavailable     = "provider_unavailable"
	CodeTimeout         = "provider_timeout"
	CodeNetwork         = "provider_network_error"
	CodeBadRequest      = "provider_request_rejected"
	CodeInvalidResponse = "provider_invalid_response"
	CodeTruncated       = "provider_output_truncated"
	CodeTooLarge        = "provider_response_too_large"
	CodeRedirect        = "provider_redirect_blocked"
	CodeDestination     = "provider_destination_blocked"
	CodeToolUnsupported = "tool_call_unsupported"
	CodeEmpty           = "provider_empty_response"
	CodeConfig          = "provider_config_invalid"
)

// Error is a classified provider failure.
type Error struct {
	Code       string
	Status     int
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Code }

// Retryable reports whether a bounded retry may help.
func (e *Error) Retryable() bool {
	switch e.Code {
	case CodeRateLimited, CodeUnavailable, CodeNetwork, CodeTimeout:
		return true
	}
	return false
}

// CodeOf returns the fixed code of err, or provider_network_error.
func CodeOf(err error) string {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return CodeTimeout
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return CodeNetwork
}

// Limits bound one request. They are per-call work limits, unrelated to any
// mail processing quota.
const (
	MaxResponseBytes = 2 << 20
	MaxToolArgBytes  = 32 << 10
)
