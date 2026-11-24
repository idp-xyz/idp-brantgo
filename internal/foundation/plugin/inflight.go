package plugin

import "context"

// Checker reports open documents or tasks of a profile. Nil means none are open.
type Checker interface {
	InFlight(ctx context.Context, profile DocumentProfile) (bool, error)
}

// InFlightFunc is a Checker.
type InFlightFunc func(ctx context.Context, profile DocumentProfile) (bool, error)

// InFlight implements Checker. A nil function reports nothing in flight.
func (f InFlightFunc) InFlight(ctx context.Context, profile DocumentProfile) (bool, error) {
	if f == nil {
		return false, nil
	}
	return f(ctx, profile)
}
