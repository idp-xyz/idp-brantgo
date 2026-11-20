package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Dispositions of delivered goods (WBS S5R-04; simulated decision S5R-D05, E0, 2026-11-13; tactical model
// SM-PUR-COMMITMENT-DISPOSITION, PUR-INV-013; UL PUR-CommitmentDisposition). Goods a supplier delivered for
// a commitment line, verified and not shipped, are disposed of before an order that holds them is
// cancelled or closed short (TRD-INV-019):
//
//	purchase.commitment.dispose              human only (purchaser): requests the disposition of a quantity of
//	                                         a line of an issued commitment (issued, confirmed or cancelled
//	                                         since) — return_to_supplier (back to the factory, citing the
//	                                         evidence), write_off (borne by us: the order's sunk cost,
//	                                         S5R-D42) or hold_for_reuse (kept for a later order; recorded
//	                                         only while STK is not enabled) — with a reason. The line's
//	                                         dispositions approved or still requested add up to no more
//	                                         than what is verified delivered of it (INVALID_KNOWN_VALUE).
//	                                         An AI agent is refused (PERMISSION_DENIED).
//	purchase.commitment.disposition.decide   human only (order reviewer), independent of the requester
//	                                         (INDEPENDENT_REVIEW_REQUIRED): approve, the line's approved
//	                                         dispositions still within what is verified delivered of it,
//	                                         or reject with a reason. An approval publishes
//	                                         PurchaseCommitmentDisposed and counts the goods as disposed of
//	                                         in DownstreamCommitments v1 (R71).
//
// A disposition never changes the commitment's status or version; its own version is 1 requested and 2
// decided. What FUL shipped of the goods is not PUR's to read: at an order's cancellation nothing has
// shipped, and for a remainder closed short the reviewer judges it (the register's "处置量不超过已核实交付减
// 已出运" is checked against the verified deliveries only).
//
// Request members in snake_case: purchase.commitment.dispose {disposition_id, commitment_id,
// order_line_id, disposition (return_to_supplier, write_off or hold_for_reuse), quantity, reason,
// evidence [{source_id, locator}] (1 to 20 for return_to_supplier, at most 20 otherwise)};
// purchase.commitment.disposition.decide {disposition_id, expected_version, decision (approve or reject),
// reason (required to reject; optional to approve)}.

// Disposition commands and events (Context Map PUR commands and fact PurchaseCommitmentDisposed).
const (
	CommandDisposeCommitment = "purchase.commitment.dispose"
	CommandDecideDisposition = "purchase.commitment.disposition.decide"

	EventPurchaseCommitmentDisposed = "idp-brant.pur.purchase_commitment_disposed.v1"

	// States of a disposition (SM-PUR-COMMITMENT-DISPOSITION).
	DispositionRequested = "requested"
	DispositionApproved  = "approved"
	DispositionRejected  = "rejected"

	// Kinds of disposition: the values of DownstreamCommitments v1's disposition column.
	DispositionReturnToSupplier = "return_to_supplier"
	DispositionWriteOff         = "write_off"
	DispositionHoldForReuse     = "hold_for_reuse"

	// DownstreamKindDisposition is DownstreamCommitments v1's kind of an approved disposition.
	DownstreamKindDisposition = "commitment_disposition"
)

// DispositionKinds are the kinds a disposition is of.
var DispositionKinds = []string{DispositionReturnToSupplier, DispositionWriteOff, DispositionHoldForReuse}

// DisposeCommitment requests disposition DispositionID of Quantity (kernel.QuantityFormat, positive, in
// the order line's unit) of line OrderLineID of commitment CommitmentID: Disposition is its kind. Human
// only.
type DisposeCommitment struct {
	IdempotencyKey string
	DispositionID  string
	CommitmentID   string
	OrderLineID    string
	Disposition    string
	Quantity       string
	Reason         string
	Evidence       []Evidence
}

// DecideDisposition approves or rejects disposition DispositionID at ExpectedVersion. Human only.
type DecideDisposition struct {
	IdempotencyKey  string
	DispositionID   string
	ExpectedVersion int64
	Decision        string
	Reason          string
}

func (DisposeCommitment) purCommand() {}
func (DecideDisposition) purCommand() {}

// Validate checks the closed contract: identifiers, a known kind, a positive quantity, a reason, and
// the evidence — 1 to 20 sources for a return to the supplier, at most 20 otherwise.
func (c DisposeCommitment) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DispositionID):
		return schema("/args/disposition_id")
	case !kernel.ValidIdentifier(c.CommitmentID):
		return schema("/args/commitment_id")
	case !kernel.ValidIdentifier(c.OrderLineID):
		return schema("/args/order_line_id")
	case !contains(DispositionKinds, c.Disposition):
		return schema("/args/disposition")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	if err := quantity(c.Quantity, "/args/quantity"); err != nil {
		return err
	}
	if c.Disposition != DispositionReturnToSupplier && len(c.Evidence) == 0 {
		return nil
	}
	return validEvidence(c.Evidence, "/args/evidence")
}

// Validate checks the closed contract: a rejection states its reason.
func (c DecideDisposition) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DispositionID):
		return schema("/args/disposition_id")
	case !version(c.ExpectedVersion):
		return schema("/args/expected_version")
	case c.Decision != DecisionApprove && c.Decision != DecisionReject:
		return schema("/args/decision")
	case c.Reason != "" && !Reason(c.Reason):
		return schema("/args/reason")
	case c.Decision == DecisionReject && c.Reason == "":
		return schema("/args/reason")
	}
	return nil
}

// DispositionReceipt is the answer of both disposition commands: the receipt (ObjectID the disposition
// ID, RowVersion 1 requested or 2 decided, Status its state) and what it disposes of. HTTP answers the
// base Receipt, as every PUR command.
type DispositionReceipt struct {
	Receipt
	CommitmentID string
	OrderID      string
	OrderLineID  string
	Disposition  string
	Quantity     string
}

// DispositionView is a disposition as the commitment read shows it: the line, its state and version,
// its kind, quantity and unit (the order line's, PUR-INV-012), the request with its reason and evidence,
// and the decision. Zero times and empty actors mean the step has not happened.
type DispositionView struct {
	DispositionID  string
	OrderLineID    string
	Status         string
	RowVersion     int64
	Disposition    string
	Quantity       string
	UnitID         string
	UnitRevision   int64
	Reason         string
	Evidence       []Evidence
	RequestedBy    kernel.ActorRef
	RequestedAt    time.Time
	RequestEpoch   int64
	Decision       string
	DecidedBy      kernel.ActorRef
	DecidedAt      time.Time
	DecideEpoch    int64
	DecisionReason string
}

// PurchaseDisposition is an approved disposition of a purchase line as PurchaseLineRef v1 shows it
// (Context Map R31, S5R-D42: a write_off is the order's sunk cost, valued by STL).
type PurchaseDisposition struct {
	DispositionID string
	Disposition   string
	Quantity      string
	DecidedAt     time.Time
}

// DispositionCommands is PUR's command entry point for dispositions. Errors are *kernel.Rejection for
// refusals.
type DispositionCommands interface {
	DisposeCommitment(ctx context.Context, cmd DisposeCommitment) (DispositionReceipt, error)
	DecideDisposition(ctx context.Context, cmd DecideDisposition) (DispositionReceipt, error)
}
