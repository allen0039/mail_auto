package modelprovider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// EchoTool is the only tool used by connection tests. It runs locally and
// has no side effects.
var EchoTool = ToolDef{Name: "echo", Description: "Return the given text unchanged. Connectivity test only.", Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to echo"}},"required":["text"],"additionalProperties":false}`)}

// ProbeChat verifies that a real request returns a non-empty reply.
func ProbeChat(ctx context.Context, p Provider) error {
	resp, err := p.Complete(ctx, Request{Messages: []Message{{Role: "system", Content: "You are a connectivity test. Reply briefly."}, {Role: "user", Content: "Reply with the word OK."}}, MaxOutputTokens: 64})
	if err != nil {
		return err
	}
	if strings.TrimSpace(resp.Content) == "" {
		return &Error{Code: CodeEmpty}
	}
	return nil
}

// ProbeToolLoop verifies the full tool-calling loop: the model must call the
// local echo tool with valid arguments, receive the tool result keyed by its
// tool_call_id, and then finish with a non-empty reply.
func ProbeToolLoop(ctx context.Context, p Provider) error {
	nonceBytes := make([]byte, 6)
	_, _ = rand.Read(nonceBytes)
	nonce := "probe-" + hex.EncodeToString(nonceBytes)
	msgs := []Message{
		{Role: "system", Content: "You are a connectivity test for tool calling. Use tools exactly as asked."},
		{Role: "user", Content: "Call the echo tool exactly once with text \"" + nonce + "\". After you receive the result, reply with the echoed text."},
	}
	resp, err := p.Complete(ctx, Request{Messages: msgs, Tools: []ToolDef{EchoTool}, MaxOutputTokens: 256})
	if err != nil {
		return err
	}
	if len(resp.ToolCalls) == 0 {
		return &Error{Code: CodeToolUnsupported}
	}
	call := resp.ToolCalls[0]
	if call.Name != "echo" {
		return &Error{Code: CodeToolUnsupported}
	}
	var args struct {
		Text string `json:"text"`
	}
	d := json.NewDecoder(bytes.NewReader([]byte(call.Arguments)))
	d.DisallowUnknownFields()
	if d.Decode(&args) != nil || d.More() || args.Text != nonce {
		return &Error{Code: CodeToolUnsupported}
	}
	msgs = append(msgs, Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})
	for _, c := range resp.ToolCalls {
		result := `{"error":"only one echo call was requested"}`
		if c.ID == call.ID {
			raw, _ := json.Marshal(map[string]string{"text": args.Text})
			result = string(raw)
		}
		msgs = append(msgs, Message{Role: "tool", ToolCallID: c.ID, Content: result})
	}
	final, err := p.Complete(ctx, Request{Messages: msgs, Tools: []ToolDef{EchoTool}, MaxOutputTokens: 256})
	if err != nil {
		return err
	}
	if strings.TrimSpace(final.Content) == "" {
		return &Error{Code: CodeToolUnsupported}
	}
	return nil
}
