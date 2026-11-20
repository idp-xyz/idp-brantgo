// Package model is the AI layer's only way to a language model (Context Map externalBoundaries:
// 模型服务 → internal/ai, Adapter (ModelPort); decision E-3). A model's answer is text and tool
// call requests: candidates, never facts or permissions. Tool calls reach the business only through
// internal/ai/gateway, which decodes them strictly into registered api commands and takes scope,
// actor, task, epoch and grant from the platform (AI-INV-004).
//
// Scripted is the deterministic model of the tests and CI. The OpenAI-compatible adapter over
// net/http for the local model service (unit C2, adapters/model/openaicompat) implements the same
// Port.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
)

// Port completes one model turn.
type Port interface {
	// Complete sends the conversation and the tools on offer and returns the model's answer. An
	// unreachable, overloaded or timed-out model is ErrUnavailable (TOOL_UNAVAILABLE): the caller
	// retries within the task's budget and then pauses the task (AI-INV-003, BC-AT26).
	Complete(ctx context.Context, req Request) (Response, error)
}

// ErrUnavailable is a model or tool that cannot answer now (error catalog TOOL_UNAVAILABLE, 503,
// retryable).
var ErrUnavailable = errors.New("model: unavailable")

// ErrInvalidRequest is a request the port refuses to send.
var ErrInvalidRequest = errors.New("model: invalid request")

// Role is the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one message of the conversation.
type Message struct {
	Role       Role
	Content    string
	ToolCalls  []ToolCall // the assistant's tool call requests
	ToolCallID string     // a tool message answers this call
}

// Tool is a tool offered to the model: a registered capability under its command name.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema of the arguments (capability.Schema)
}

// ToolCall is a model's request to call a tool. Its arguments are untrusted model output.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Request is one model turn.
type Request struct {
	Profile         string // the ModelProfile the call runs under (baseline 19.3)
	Messages        []Message
	Tools           []Tool
	MaxOutputTokens int
}

// Usage is what a turn consumed as the provider reports it.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// Response is the model's answer to one turn.
type Response struct {
	Model        string  // the model ID as the provider returned it (baseline 17.3: record what was used)
	Message      Message // RoleAssistant: text and tool call requests
	FinishReason string  // "stop", "tool_calls", "length", ...
	Usage        Usage
	Latency      time.Duration
}

var toolName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// Validate checks a request before it is sent: at least one message, known roles, tool answers
// naming their call, and unique, well-formed tool names.
func (r Request) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, a...))
	}
	if len(r.Messages) == 0 {
		return bad("no messages")
	}
	if r.MaxOutputTokens < 0 {
		return bad("negative output budget")
	}
	for i, m := range r.Messages {
		switch m.Role {
		case RoleSystem, RoleUser:
			if len(m.ToolCalls) > 0 || m.ToolCallID != "" {
				return bad("message %d: a %s message has no tool calls", i, m.Role)
			}
		case RoleAssistant:
			if m.ToolCallID != "" {
				return bad("message %d: an assistant message answers no call", i)
			}
		case RoleTool:
			if m.ToolCallID == "" || len(m.ToolCalls) > 0 {
				return bad("message %d: a tool message answers exactly one call", i)
			}
		default:
			return bad("message %d: role %q", i, m.Role)
		}
	}
	seen := map[string]bool{}
	for _, t := range r.Tools {
		if !toolName.MatchString(t.Name) || len(t.Name) > 64 || seen[t.Name] {
			return bad("tool %q", t.Name)
		}
		seen[t.Name] = true
		if len(t.Parameters) > 0 && !json.Valid(t.Parameters) {
			return bad("tool %s: parameters are not JSON", t.Name)
		}
	}
	return nil
}

// clone returns a deep copy of r, so a recorded request cannot change under its recorder.
func (r Request) clone() Request {
	out := r
	out.Messages = make([]Message, len(r.Messages))
	for i, m := range r.Messages {
		out.Messages[i] = m.clone()
	}
	out.Tools = make([]Tool, len(r.Tools))
	for i, t := range r.Tools {
		t.Parameters = slices.Clone(t.Parameters)
		out.Tools[i] = t
	}
	return out
}

func (m Message) clone() Message {
	if m.ToolCalls == nil {
		return m
	}
	calls := make([]ToolCall, len(m.ToolCalls))
	for i, c := range m.ToolCalls {
		c.Arguments = slices.Clone(c.Arguments)
		calls[i] = c
	}
	m.ToolCalls = calls
	return m
}
