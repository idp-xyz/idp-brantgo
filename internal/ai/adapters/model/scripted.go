package model

import (
	"context"
	"errors"
	"sync"
)

// ErrScriptExhausted is a turn after the last scripted step.
var ErrScriptExhausted = errors.New("model: script exhausted")

// Step is one scripted turn: the reply (with Usage), or Err. Check, when set, inspects the request
// first; its error is returned as the turn's error.
type Step struct {
	Reply Message
	Usage Usage
	// FinishReason defaults to "tool_calls" for a reply with tool calls and "stop" otherwise.
	FinishReason string
	Err          error
	Check        func(Request) error
}

// Scripted is a deterministic model: it answers the turns of a fixed script in order and records
// every request. The tests and CI use it; it never reaches a network (plan S4-03, decision E-5).
type Scripted struct {
	model string
	mu    sync.Mutex
	steps []Step
	next  int
	seen  []Request
}

var _ Port = (*Scripted)(nil)

// NewScripted returns a model that reports itself as model and answers with steps.
func NewScripted(model string, steps ...Step) *Scripted {
	return &Scripted{model: model, steps: steps}
}

// Complete answers the next step. A cancelled context and an invalid request are answered before
// the script moves on; after the last step every turn is ErrScriptExhausted.
func (s *Scripted) Complete(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, req.clone())
	if s.next >= len(s.steps) {
		return Response{}, ErrScriptExhausted
	}
	step := s.steps[s.next]
	s.next++
	if step.Check != nil {
		if err := step.Check(req); err != nil {
			return Response{}, err
		}
	}
	if step.Err != nil {
		return Response{}, step.Err
	}
	reply := step.Reply.clone()
	reply.Role = RoleAssistant
	finish := step.FinishReason
	if finish == "" {
		finish = "stop"
		if len(reply.ToolCalls) > 0 {
			finish = "tool_calls"
		}
	}
	return Response{Model: s.model, Message: reply, FinishReason: finish, Usage: step.Usage}, nil
}

// Requests returns copies of the requests received so far, in order.
func (s *Scripted) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.seen))
	for i, r := range s.seen {
		out[i] = r.clone()
	}
	return out
}

// Remaining returns the number of steps not yet answered.
func (s *Scripted) Remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.steps) - s.next
}
