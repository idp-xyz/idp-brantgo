package model

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ScriptStep is one turn of a scripted model in a script file (brant-worker -agent -model scripted
// -model-script <file>; the scripted evaluation suite, S4-07): the assistant's text and tool calls
// with the usage to report, or an unavailable model.
type ScriptStep struct {
	Content      string           `json:"content"`
	ToolCalls    []ScriptToolCall `json:"tool_calls"`
	Usage        Usage            `json:"usage"`
	FinishReason string           `json:"finish_reason"`
	Unavailable  bool             `json:"unavailable"`
}

// ScriptToolCall is a tool call of a script step; Arguments is any JSON value (normally an object).
type ScriptToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ParseScript reads a script file: a JSON array of ScriptStep, unknown members refused.
func ParseScript(raw []byte) ([]Step, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in []ScriptStep
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("model: script: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("model: script: trailing data")
	}
	out := make([]Step, 0, len(in))
	for i, s := range in {
		if s.Unavailable {
			if s.Content != "" || len(s.ToolCalls) > 0 {
				return nil, fmt.Errorf("model: script step %d: an unavailable step has no reply", i)
			}
			out = append(out, Step{Err: ErrUnavailable})
			continue
		}
		step := Step{Reply: Message{Role: RoleAssistant, Content: s.Content}, Usage: s.Usage, FinishReason: s.FinishReason}
		for j, tc := range s.ToolCalls {
			if tc.ID == "" || tc.Name == "" || !json.Valid(tc.Arguments) {
				return nil, fmt.Errorf("model: script step %d: tool call %d needs an id, a name and JSON arguments", i, j)
			}
			step.Reply.ToolCalls = append(step.Reply.ToolCalls, ToolCall(tc))
		}
		out = append(out, step)
	}
	return out, nil
}
