// Package interop is the port to a professional system (customs, tax) for one stable operation
// (WBS S6-05, S6-D05, baseline 7.3 and 23.3). A connector moves an operation through planned,
// submitted, succeeded, failed, unknown and not_received. A timeout is unknown: the call returns
// no result that could be read as success. Lookup returns the other side's result, ErrNotFound
// when that side has no such operation, or ErrUnavailable when it cannot answer.
//
// Nothing here dials a Single Window, a tax bureau, or any other network endpoint. Unconnected is
// the process default. Fake, in fake.go, is the in-process double tests use.
package interop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// State is a connector's view of one operation (baseline 7.3). not_received is the verified
// absence of the operation on the other side, distinct from unknown (the answer was lost) and
// from ErrNotFound (Lookup's report of that absence, which the caller then records).
type State string

const (
	Planned     State = "planned"
	Submitted   State = "submitted"
	Succeeded   State = "succeeded"
	Failed      State = "failed"
	Unknown     State = "unknown"
	NotReceived State = "not_received"
)

// Channels a CNX adapter names. They are labels for the operation id, not endpoints.
const (
	ChannelCustomsDeclaration = "customs_declaration"
	ChannelTaxRefund          = "tax_refund"
)

// OperationID is the stable identifier of one submission attempt. The same inputs always produce
// the same id, so a retry does not file under a second id.
type OperationID string

// OperationIDFor derives the operation id of attempt (1-based) of subject on channel in scope.
// The form matches the identifier rule (op- and 40 hex characters).
func OperationIDFor(scope kernel.Scope, channel, subject string, attempt int) OperationID {
	sum := sha256.Sum256(fmt.Appendf(nil, "brant.interop\x1f%s\x1f%s\x1f%s\x1f%s\x1f%d",
		scope.Tenant, scope.Company, channel, subject, attempt))
	return OperationID("op-" + hex.EncodeToString(sum[:20]))
}

// Submission is one operation handed to a connector.
type Submission struct {
	ID      OperationID
	Channel string
	Subject string
}

// Result is an authoritative answer. State is succeeded or failed. Outcome is the professional
// result (customs accepted, released or rejected; tax approved or rejected). A timeout is not a
// Result: Submit returns ErrUnknown and the zero Result.
type Result struct {
	OperationID       OperationID
	State             State
	Outcome           string
	Sequence          int64
	StatusCode        string
	OriginalText      string
	MappingVersion    string
	DeclarationNumber string
	ExportedAt        time.Time
	At                time.Time
	// AmountMinor and CurrencyCode are a tax approval's amount, in integer minor units.
	AmountMinor  int64
	CurrencyCode string
}

var (
	// ErrNotFound is Lookup's report that the other side has no operation under the id. It is not
	// unknown, and it is not success.
	ErrNotFound = errors.New("interop: operation not found")
	// ErrUnavailable means the connector could not be reached. The request did not leave, or a
	// lookup could not be answered. Retryable. Not success.
	ErrUnavailable = errors.New("interop: unavailable")
	// ErrUnknown means the answer was lost after the request may have reached the other side
	// (a timeout). The result is not success and must not be treated as one.
	ErrUnknown = errors.New("interop: outcome unknown")
	// ErrNotConfigured means no connector is configured. Nothing was sent and nothing succeeded.
	ErrNotConfigured = errors.New("interop: no connector is configured")
)

// Port submits one operation and looks it up later. A repeat of an id the connector already holds
// returns the first authoritative result and does not file a second time.
type Port interface {
	Submit(ctx context.Context, op Submission) (Result, error)
	Lookup(ctx context.Context, id OperationID) (Result, error)
}

// Unconnected is a connector that is not configured. Every call returns ErrNotConfigured and a
// zero Result, never a success.
type Unconnected struct{ Name string }

var _ Port = Unconnected{}

func (u Unconnected) refuse() error {
	name := u.Name
	if name == "" {
		name = "professional"
	}
	return fmt.Errorf("%w: %s", ErrNotConfigured, name)
}

// Submit refuses. Nothing is sent.
func (u Unconnected) Submit(context.Context, Submission) (Result, error) {
	return Result{}, u.refuse()
}

// Lookup refuses. An unconnected connector knows nothing.
func (u Unconnected) Lookup(context.Context, OperationID) (Result, error) {
	return Result{}, u.refuse()
}
