package interop

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Behaviour is what Fake does with the next new submission. A repeat of an id already held does
// not consume a step.
type Behaviour string

const (
	// BehaviourAccept stores an authoritative success and returns it.
	BehaviourAccept Behaviour = "accept"
	// BehaviourReject stores an authoritative refusal and returns it.
	BehaviourReject Behaviour = "reject"
	// BehaviourTimeoutHeld stores the scripted result and returns ErrUnknown with a zero Result.
	// Lookup then returns that result. The timeout itself is not a success.
	BehaviourTimeoutHeld Behaviour = "timeout_held"
	// BehaviourTimeoutLost returns ErrUnknown and stores nothing. Lookup returns ErrNotFound.
	// A repeat of the same id returns ErrUnknown again and does not consume another step.
	BehaviourTimeoutLost Behaviour = "timeout_lost"
	// BehaviourUnavailable returns ErrUnavailable. Nothing is stored.
	BehaviourUnavailable Behaviour = "unavailable"
)

// Step is one scripted submission.
type Step struct {
	Behaviour         Behaviour
	Outcome           string
	Sequence          int64
	StatusCode        string
	OriginalText      string
	MappingVersion    string
	DeclarationNumber string
	ExportedAt        time.Time
	AmountMinor       int64
	CurrencyCode      string
}

// Fake is a deterministic in-process connector. It does not open a network connection.
// An empty script accepts. Now, when set, is the clock of every result; otherwise results are
// stamped 2026-01-15T00:00:00Z.
type Fake struct {
	mu                sync.Mutex
	Script            []Step
	LookupUnavailable bool
	Now               func() time.Time
	held              map[OperationID]Result
	lost              map[OperationID]bool
	Submits           int
}

var _ Port = (*Fake)(nil)

func (f *Fake) at() time.Time {
	if f.Now != nil {
		return f.Now().UTC()
	}
	return time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
}

func (f *Fake) resultOf(id OperationID, step Step, state State) Result {
	at := f.at()
	exported := step.ExportedAt
	if exported.IsZero() && step.Outcome == "released" {
		exported = at.Add(-24 * time.Hour)
	}
	return Result{OperationID: id, State: state, Outcome: step.Outcome, Sequence: step.Sequence, StatusCode: step.StatusCode,
		OriginalText: step.OriginalText, MappingVersion: step.MappingVersion, DeclarationNumber: step.DeclarationNumber,
		ExportedAt: exported, At: at, AmountMinor: step.AmountMinor, CurrencyCode: step.CurrencyCode}
}

// Submit applies the next step to a new id. A held id returns its first result. A lost id returns
// ErrUnknown again. ErrUnknown and ErrUnavailable are returned with the zero Result.
func (f *Fake) Submit(ctx context.Context, op Submission) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("%w: %s", ErrUnavailable, err.Error())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held == nil {
		f.held = map[OperationID]Result{}
	}
	if held, ok := f.held[op.ID]; ok {
		return held, nil
	}
	if f.lost[op.ID] {
		return Result{}, fmt.Errorf("%w: answer lost", ErrUnknown)
	}
	step := Step{Behaviour: BehaviourAccept, Outcome: "accepted", Sequence: 1}
	if len(f.Script) > 0 {
		step, f.Script = f.Script[0], f.Script[1:]
	}
	switch step.Behaviour {
	case BehaviourUnavailable:
		return Result{}, fmt.Errorf("%w: fake connector unreachable", ErrUnavailable)
	case BehaviourTimeoutLost:
		if f.lost == nil {
			f.lost = map[OperationID]bool{}
		}
		f.lost[op.ID] = true
		return Result{}, fmt.Errorf("%w: request lost", ErrUnknown)
	case BehaviourTimeoutHeld:
		f.Submits++
		f.held[op.ID] = f.resultOf(op.ID, step, Succeeded)
		if step.Outcome == "rejected" {
			f.held[op.ID] = f.resultOf(op.ID, step, Failed)
		}
		return Result{}, fmt.Errorf("%w: answer lost", ErrUnknown)
	case BehaviourReject:
		f.Submits++
		held := f.resultOf(op.ID, step, Failed)
		f.held[op.ID] = held
		return held, nil
	default:
		f.Submits++
		held := f.resultOf(op.ID, step, Succeeded)
		f.held[op.ID] = held
		return held, nil
	}
}

// Lookup returns the held result, ErrNotFound when the id was never stored, or ErrUnavailable
// when LookupUnavailable is set. ErrNotFound is not a result.
func (f *Fake) Lookup(ctx context.Context, id OperationID) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("%w: %s", ErrUnavailable, err.Error())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.LookupUnavailable {
		return Result{}, fmt.Errorf("%w: fake lookup failed", ErrUnavailable)
	}
	held, ok := f.held[id]
	if !ok {
		return Result{}, ErrNotFound
	}
	return held, nil
}

// Held reports how many authoritative results the connector stores.
func (f *Fake) Held() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.held)
}
