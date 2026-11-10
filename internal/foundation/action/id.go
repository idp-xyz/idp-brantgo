package action

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// NewID returns prefix, a hyphen and a UUIDv7 (RFC 9562): 48 bits of Unix milliseconds, then
// random bits. IDs sort roughly by creation time and fit the 64-character identifier rule.
func NewID(prefix string, now time.Time) string {
	var b [16]byte
	if _, err := rand.Read(b[6:]); err != nil {
		panic("action: crypto/rand failed: " + err.Error()) // the runtime never returns an error here
	}
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(now.UnixMilli()))
	copy(b[:6], ms[2:])
	b[6] = 0x70 | (b[6] & 0x0f) // version 7
	b[8] = 0x80 | (b[8] & 0x3f) // variant 10
	h := hex.EncodeToString(b[:])
	return prefix + "-" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// RequestHash returns the SHA-256 of the JSON encoding of a command's business arguments. Callers
// pass a struct without the expected version, so a replay with another CAS value still matches
// (Python test_replay_ignores_changed_cas_but_not_changed_business_fields); nil and empty slices
// encode differently, so omitting a field and sending it empty are different requests.
func RequestHash(args any) (string, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("action: hash request: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

type correlationKey struct{}

// WithCorrelation returns ctx carrying the correlation ID of the inbound request. The ID is
// recorded with every audit entry and event the request causes and returned with rejections.
func WithCorrelation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}

// Correlation returns the correlation ID of ctx. A request without one gets a new ID, so a caller
// that forgets to set one still produces a traceable audit trail.
func Correlation(ctx context.Context) string {
	if id, ok := ctx.Value(correlationKey{}).(string); ok && kernel.ValidIdentifier(id) {
		return id
	}
	return NewID("corr", time.Now())
}
