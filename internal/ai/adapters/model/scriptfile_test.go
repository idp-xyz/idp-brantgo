package model

import (
	"context"
	"errors"
	"testing"
)

// A script file drives the scripted model of brant-worker -agent (the scripted evaluation suite):
// replies with tool calls and usage, and unavailable turns; anything else is refused.
func TestParseScript(t *testing.T) {
	steps, err := ParseScript([]byte(`[
		{"tool_calls":[{"id":"c1","name":"source.register","arguments":{"SourceID":"s.1"}}],"usage":{"InputTokens":10,"OutputTokens":2}},
		{"unavailable":true},
		{"content":"done","finish_reason":"stop"}]`))
	if err != nil || len(steps) != 3 {
		t.Fatalf("%v %v", steps, err)
	}
	m := NewScripted("scripted", steps...)
	req := Request{Messages: []Message{{Role: RoleUser, Content: "x"}}}
	r, err := m.Complete(context.Background(), req)
	if err != nil || len(r.Message.ToolCalls) != 1 || string(r.Message.ToolCalls[0].Arguments) != `{"SourceID":"s.1"}` || r.Usage.InputTokens != 10 ||
		r.FinishReason != "tool_calls" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err = m.Complete(context.Background(), req); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if r, err = m.Complete(context.Background(), req); err != nil || r.Message.Content != "done" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{`{}`, `[{"unknown":1}]`, `[{"unavailable":true,"content":"x"}]`, `[{"tool_calls":[{"name":"x","arguments":{}}]}]`,
		`[{"tool_calls":[{"id":"c","name":"x"}]}]`, `[] []`} {
		if _, err := ParseScript([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
