package kernel

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidValidity  = errors.New("kernel: invalid validity interval")
	ErrInvalidTimestamp = errors.New("kernel: invalid timestamp")
)

// Instant returns t in UTC truncated to microseconds, the precision of PostgreSQL timestamptz.
// Values that are hashed, compared or stored go through Instant first, so a reload never changes
// them.
func Instant(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

const timestampLayout = "2006-01-02T15:04:05.000000Z"

// ParseTimestamp parses the contracts' UTC timestamp: "YYYY-MM-DDThh:mm:ss" with up to 6
// fractional digits and a literal "Z".
func ParseTimestamp(s string) (time.Time, error) {
	n := len(s)
	ok := n >= 20 && s[n-1] == 'Z' && s[4] == '-' && s[7] == '-' && s[10] == 'T' && s[13] == ':' && s[16] == ':'
	if ok && n > 20 {
		frac := s[19 : n-1]
		ok = frac[0] == '.' && len(frac) >= 2 && len(frac) <= 7 && digits(frac[1:])
	}
	if !ok {
		return time.Time{}, fmt.Errorf("%w: %q", ErrInvalidTimestamp, s)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %q", ErrInvalidTimestamp, s)
	}
	return t.UTC(), nil
}

// FormatTimestamp renders Instant(t) with six fractional digits and "Z".
func FormatTimestamp(t time.Time) string { return Instant(t).Format(timestampLayout) }

// ValidityInterval is the half-open interval [From, Until). A zero Until means open-ended.
type ValidityInterval struct {
	From  time.Time
	Until time.Time
}

// NewValidityInterval returns [from, until) at microsecond precision; until is zero or after from.
func NewValidityInterval(from, until time.Time) (ValidityInterval, error) {
	v := ValidityInterval{From: Instant(from)}
	if !until.IsZero() {
		v.Until = Instant(until)
	}
	if !v.Valid() {
		return ValidityInterval{}, fmt.Errorf("%w: [%s, %s)", ErrInvalidValidity, from, until)
	}
	return v, nil
}

// Valid reports whether the interval has a start and, if bounded, ends after it starts.
func (v ValidityInterval) Valid() bool {
	return !v.From.IsZero() && (v.Until.IsZero() || v.From.Before(v.Until))
}

// Contains reports whether t lies in [From, Until).
func (v ValidityInterval) Contains(t time.Time) bool {
	return !t.Before(v.From) && (v.Until.IsZero() || t.Before(v.Until))
}

// Overlaps reports whether the intervals share an instant; [a, b) and [b, c) do not.
func (v ValidityInterval) Overlaps(o ValidityInterval) bool {
	return (v.Until.IsZero() || o.From.Before(v.Until)) && (o.Until.IsZero() || v.From.Before(o.Until))
}
