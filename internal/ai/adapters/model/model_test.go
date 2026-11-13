package model

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func turn(content string) Request {
	return Request{Profile: "q01.scripted", Messages: []Message{{Role: RoleSystem, Content: "rules"}, {Role: RoleUser, Content: content}},
		Tools: []Tool{{Name: "rfq.save_draft", Description: "save", Parameters: json.RawMessage(`{"type":"object"}`)}}}
}

// The scripted model answers its steps in order, reports its model ID and usage, records every
// request as sent, and refuses turns past the end of the script.
func TestScriptedAnswersInOrder(t *testing.T) {
	call := ToolCall{ID: "call.1", Name: "rfq.save_draft", Arguments: json.RawMessage(`{"RfqID":"rfq.1"}`)}
	m := NewScripted("scripted-q01-v1",
		Step{Reply: Message{Content: "plan"}, Usage: Usage{InputTokens: 10, OutputTokens: 2}},
		Step{Reply: Message{ToolCalls: []ToolCall{call}}, Check: func(r Request) error {
			if r.Messages[len(r.Messages)-1].Content != "second" {
				return errors.New("unexpected request")
			}
			return nil
		}},
		Step{Err: ErrUnavailable},
	)
	ctx := context.Background()
	r1, err := m.Complete(ctx, turn("first"))
	if err != nil || r1.Model != "scripted-q01-v1" || r1.Message.Role != RoleAssistant || r1.Message.Content != "plan" ||
		r1.FinishReason != "stop" || r1.Usage.InputTokens != 10 {
		t.Fatalf("turn 1 %+v, %v", r1, err)
	}
	r2, err := m.Complete(ctx, turn("second"))
	if err != nil || r2.FinishReason != "tool_calls" || len(r2.Message.ToolCalls) != 1 || r2.Message.ToolCalls[0].Name != "rfq.save_draft" {
		t.Fatalf("turn 2 %+v, %v", r2, err)
	}
	r2.Message.ToolCalls[0].Arguments[2] = 'X' // the caller's copy only
	if _, err = m.Complete(ctx, turn("third")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("turn 3 %v", err)
	}
	if _, err = m.Complete(ctx, turn("fourth")); !errors.Is(err, ErrScriptExhausted) {
		t.Fatalf("turn 4 %v", err)
	}
	seen := m.Requests()
	if len(seen) != 4 || seen[1].Messages[1].Content != "second" || m.Remaining() != 0 {
		t.Fatalf("recorded %d requests, %d remaining", len(seen), m.Remaining())
	}
	again, _ := NewScripted("m", Step{Reply: Message{ToolCalls: []ToolCall{call}}}).Complete(ctx, turn("x"))
	if string(again.Message.ToolCalls[0].Arguments) != `{"RfqID":"rfq.1"}` {
		t.Fatal("a reply must not share the script's bytes")
	}
}

func TestScriptedChecksAndCancellation(t *testing.T) {
	m := NewScripted("m", Step{Reply: Message{Content: "x"}}, Step{Check: func(Request) error { return errors.New("no") }})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Complete(cancelled, turn("x")); !errors.Is(err, context.Canceled) || m.Remaining() != 2 {
		t.Fatalf("cancelled %v, %d remaining", err, m.Remaining())
	}
	if _, err := m.Complete(context.Background(), Request{}); !errors.Is(err, ErrInvalidRequest) || m.Remaining() != 2 {
		t.Fatalf("empty request %v", err)
	}
	if _, err := m.Complete(context.Background(), turn("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Complete(context.Background(), turn("x")); err == nil || err.Error() != "no" {
		t.Fatalf("check %v", err)
	}
}

func TestRequestValidation(t *testing.T) {
	bad := map[string]Request{
		"no messages":      {},
		"unknown role":     {Messages: []Message{{Role: "admin"}}},
		"user tool call":   {Messages: []Message{{Role: RoleUser, ToolCalls: []ToolCall{{Name: "x"}}}}},
		"orphan tool":      {Messages: []Message{{Role: RoleTool, Content: "x"}}},
		"assistant answer": {Messages: []Message{{Role: RoleAssistant, ToolCallID: "c"}}},
		"tool name":        {Messages: []Message{{Role: RoleUser}}, Tools: []Tool{{Name: "Drop Table"}}},
		"duplicate tool":   {Messages: []Message{{Role: RoleUser}}, Tools: []Tool{{Name: "a.b"}, {Name: "a.b"}}},
		"parameters":       {Messages: []Message{{Role: RoleUser}}, Tools: []Tool{{Name: "a.b", Parameters: json.RawMessage("{")}}},
		"budget":           {Messages: []Message{{Role: RoleUser}}, MaxOutputTokens: -1},
	}
	for name, r := range bad {
		if err := r.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := Request{Messages: []Message{{Role: RoleUser}, {Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "a.b"}}},
		{Role: RoleTool, ToolCallID: "c", Content: "{}"}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
}

// Concurrent turns each take one step; none is answered twice.
func TestScriptedIsSafeForConcurrentTurns(t *testing.T) {
	steps := make([]Step, 50)
	for i := range steps {
		steps[i] = Step{Reply: Message{Content: "ok"}}
	}
	m := NewScripted("m", steps...)
	var wg sync.WaitGroup
	errs := make(chan error, 60)
	for range 60 {
		wg.Go(func() {
			_, err := m.Complete(context.Background(), turn("x"))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	exhausted := 0
	for err := range errs {
		if errors.Is(err, ErrScriptExhausted) {
			exhausted++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if exhausted != 10 || m.Remaining() != 0 || len(m.Requests()) != 60 {
		t.Fatalf("%d exhausted, %d remaining", exhausted, m.Remaining())
	}
}
