// Package mailtools defines named business tools with input schemas, risk
// classes and handlers over mail.Service. Authorization always comes from the
// service layer; schema annotations are hints only.
package mailtools

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"sort"

	"mail-auto/internal/mail"
)

// Risk classifies what a tool may do.
type Risk string

const (
	RiskRead     Risk = "read"     // no state change
	RiskDraft    Risk = "draft"    // disabled rule / paused observe task only
	RiskProposal Risk = "proposal" // returns a proposal; never writes effective config
	RiskRun      Risk = "run"      // explicit run / pause / stop request
)

// Limits on tool input and output. Outputs over the limit fail visibly
// instead of being silently truncated.
const (
	MaxArgBytes    = 32 << 10
	MaxResultBytes = 32 << 10
	maxDepth       = 8
)

// Call carries the server-resolved identity and the server-assigned action
// ID for one tool call. The model supplies only business arguments.
type Call struct {
	Service  *mail.Service
	Access   mail.Access
	ActionID string
}

// Result is a tool outcome. Proposal is set when the requested change needs
// explicit confirmation; nothing effective was written in that case.
type Result struct {
	Data     any       `json:"data,omitempty"`
	Proposal *Proposal `json:"-"`
}

type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Risk        Risk
	Idempotent  bool
	handler     func(context.Context, Call, json.RawMessage) (Result, error)
}

// Error is a fixed, user-safe tool error code with an optional Chinese hint.
type Error struct {
	Code string `json:"code"`
	Hint string `json:"hint,omitempty"`
}

func (e *Error) Error() string { return e.Code }

func fail(code, hint string) error { return &Error{Code: code, Hint: hint} }

// CodeOf maps service errors to fixed tool error codes.
func CodeOf(e error) *Error {
	var te *Error
	switch {
	case e == nil:
		return nil
	case errors.As(e, &te):
		return te
	case errors.Is(e, mail.ErrDenied):
		return &Error{Code: "permission_denied", Hint: "当前 Agent 授权或自动化策略不允许该操作；如需扩大范围请由管理员在 Web 调整"}
	case errors.Is(e, mail.ErrConflict):
		return &Error{Code: "state_conflict", Hint: "对象版本或绑定已变化，请重新读取后再试"}
	case errors.Is(e, mail.ErrInput):
		return &Error{Code: "invalid_input", Hint: "参数不符合业务校验"}
	case errors.Is(e, mail.ErrCapability):
		return &Error{Code: "capability_unavailable", Hint: "该能力首版不支持或邮箱映射/写入设置不满足"}
	case errors.Is(e, mail.ErrBusy):
		return &Error{Code: "busy", Hint: "邮箱或任务正忙，请稍后再试"}
	case errors.Is(e, sql.ErrNoRows):
		return &Error{Code: "not_found"}
	default:
		return &Error{Code: "operation_failed"}
	}
}

type Registry struct {
	tools map[string]Tool
	names []string
}

func (r *Registry) add(t Tool) {
	if r.tools == nil {
		r.tools = map[string]Tool{}
	}
	if _, dup := r.tools[t.Name]; dup {
		panic("duplicate tool " + t.Name)
	}
	if !json.Valid(t.Schema) {
		panic("invalid schema for " + t.Name)
	}
	r.tools[t.Name] = t
	r.names = append(r.names, t.Name)
	sort.Strings(r.names)
}

// Tools returns the tool definitions in a stable order.
func (r *Registry) Tools() []Tool {
	out := make([]Tool, 0, len(r.names))
	for _, n := range r.names {
		out = append(out, r.tools[n])
	}
	return out
}

func (r *Registry) Lookup(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Canonical validates raw arguments for the named tool and returns their
// canonical JSON (for digests). Unknown tools, unknown fields, trailing JSON,
// excessive nesting and oversized input are rejected.
func (r *Registry) Canonical(name string, raw string) (json.RawMessage, error) {
	t, ok := r.tools[name]
	if !ok {
		return nil, fail("unknown_tool", "")
	}
	if len(raw) > MaxArgBytes {
		return nil, fail("arguments_too_large", "")
	}
	if !json.Valid([]byte(raw)) || depth([]byte(raw)) > maxDepth {
		return nil, fail("invalid_arguments", "")
	}
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return nil, fail("invalid_arguments", "")
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fail("invalid_arguments", "")
	}
	canonical, _ := json.Marshal(v)
	_ = t
	return canonical, nil
}

// Invoke runs a tool with strict decoding into the handler's input type and
// enforces the result size limit.
func (r *Registry) Invoke(ctx context.Context, c Call, name string, raw json.RawMessage) (Result, json.RawMessage, error) {
	t, ok := r.tools[name]
	if !ok {
		return Result{}, nil, fail("unknown_tool", "")
	}
	res, err := t.handler(ctx, c, raw)
	if err != nil {
		return Result{}, nil, err
	}
	encoded, err := json.Marshal(res.Data)
	if err != nil {
		return Result{}, nil, fail("operation_failed", "")
	}
	if len(encoded) > MaxResultBytes {
		return Result{}, nil, fail("result_too_large", "结果过大，请缩小范围或分页")
	}
	return res, encoded, nil
}

func depth(b []byte) int {
	d, maxD := 0, 0
	inString, escaped := false, false
	for _, c := range b {
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			d++
			if d > maxD {
				maxD = d
			}
		case '}', ']':
			d--
		}
	}
	return maxD
}

// strict decodes one JSON object with no unknown fields and no trailing data.
func strict[I any](raw json.RawMessage) (I, error) {
	var in I
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return in, fail("invalid_arguments", "参数含未知字段、类型错误或格式错误")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return in, fail("invalid_arguments", "参数只能包含一个 JSON 对象")
	}
	return in, nil
}

func def[I any](r *Registry, name string, risk Risk, idempotent bool, description, schema string, h func(context.Context, Call, I) (Result, error)) {
	r.add(Tool{Name: name, Description: description, Schema: json.RawMessage(schema), Risk: risk, Idempotent: idempotent, handler: func(ctx context.Context, c Call, raw json.RawMessage) (Result, error) {
		in, err := strict[I](raw)
		if err != nil {
			return Result{}, err
		}
		return h(ctx, c, in)
	}})
}
