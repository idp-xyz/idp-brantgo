package api

import (
	"context"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Credit notes, settlement reversal and cross-currency applications (S5 unit C8, WBS S5-18; tactical model
// STL-AGG-SettlementStatement entities CreditNote and SettlementReversal, STL-AGG-Application; SM-STL-CREDIT-NOTE,
// SM-STL-SETTLEMENT; STL-INV-007, STL-INV-008, STL-INV-009; UL STL-CreditNote, UL-INV-22; Context Map R33, MD-10;
// baseline 4.2, 4.3, 4.5, 4.7 S-B; CN-AT11, STL half).
//
//	credit_note.request  human or AI (sales clerk): requests a credit note on the business receivable of a
//	                     confirmed sales statement of the order named — kind price_allowance, return or claim,
//	                     an amount in the statement's currency, its basis and EVD evidence. Above what is open
//	                     on the receivable beyond the confirmed credit notes not yet applied:
//	                     APPLICATION_EXCEEDS_OPEN. The statement's version moves by one.
//	credit.confirm       human only (credit manager), never the requester (INDEPENDENT_REVIEW_REQUIRED):
//	                     confirms or rejects a requested credit note with a reason. A confirmed credit note is
//	                     a source for application.apply ({credit_note_id} instead of fund_record_id) to its
//	                     statement's receivable, at most its rest and what is open. It changes no shipped
//	                     quantity, refunds nothing (a refund is a fund record) and adjusts no tax-refund
//	                     material (CNX records that impact: external_reconciliation.resolve; STL-INV-008).
//	settlement.reverse   human only (credit manager), never the statement's confirmer
//	                     (INDEPENDENT_REVIEW_REQUIRED): reverses a confirmed statement on which nothing stands
//	                     applied (SETTLEMENT_HAS_APPLICATIONS: unapply first, the moved prepayments' replacements
//	                     and credit note applications included) and no credit note waits for a decision
//	                     (INVALID_STATE). Its lines are reversed: their sources settle again on a new
//	                     statement; the statement, its lines and the reversal record stay (STL-INV-009).
//
// application.apply also takes {fx_rate: {kind: "fx_rate", id, revision}} when the fund's currency is not the
// target's: a POL fx_rate of purpose settlement from the fund's currency to the target's, in force (no inverse,
// no cross: FX_SCOPE_MISMATCH; without it FX_SCOPE_MISMATCH at /args/fx_rate). amount stays in the fund's
// currency; the target receives amount × rate, HALF_UP at its minor units; for an order's stage or receivable
// the application records the order's analysis rate and the business settlement FX difference against it, in
// the analysis currency (baseline 4.5: never a receipt or revenue; unit C9's order result).
//
// Request members: credit_note.request {credit_note_id, kind, order_id, statement_id, amount, basis,
// evidence_ids}; credit.confirm {credit_note_id, expected_version, decision, reason}; settlement.reverse
// {statement_id, expected_version, reason, evidence_ids}.
const (
	CommandRequestCreditNote = "credit_note.request"
	CommandConfirmCredit     = "credit.confirm"
	CommandReverseSettlement = "settlement.reverse"

	// Reads.
	CommandGetCreditNote = "credit_note.get"

	EventCreditConfirmed    = "idp-brant.stl.credit_confirmed.v1"
	EventSettlementReversed = "idp-brant.stl.settlement_reversed.v1"

	// Kinds, decisions and states of a credit note; the reversed state of a statement and its lines.
	CreditPriceAllowance = "price_allowance"
	CreditReturn         = "return"
	CreditClaim          = "claim"
	DecisionConfirm      = "confirm"
	CreditRequested      = "requested"
	CreditConfirmed      = "confirmed"
	CreditRejected       = "rejected"
	StatementReversed    = "reversed"
	LineReversed         = "reversed"

	// MaxBasis bounds a credit note's basis.
	MaxBasis = 1000
)

// RequestCreditNote requests credit note CreditNoteID of CreditKind ("kind": price_allowance, return or claim)
// on the receivable of sales statement StatementID of order OrderID: Amount in the statement's currency, on
// Basis, resting on EvidenceIDs (1 to 20 EVD sources). Human or AI.
type RequestCreditNote struct {
	IdempotencyKey string
	CreditNoteID   string
	CreditKind     string `json:"kind"`
	OrderID        string
	StatementID    string
	Amount         string
	Basis          string
	EvidenceIDs    []string
}

// ConfirmCredit decides credit note CreditNoteID at ExpectedVersion: Decision confirm or reject, for Reason.
// Human only, not the requester.
type ConfirmCredit struct {
	IdempotencyKey  string
	CreditNoteID    string
	ExpectedVersion int64
	Decision        string
	Reason          string
}

// ReverseSettlement reverses statement StatementID at ExpectedVersion for Reason, resting on EvidenceIDs (0 to
// 20 EVD sources). Human only, not the confirmer.
type ReverseSettlement struct {
	IdempotencyKey  string
	StatementID     string
	ExpectedVersion int64
	Reason          string
	EvidenceIDs     []string
}

func (RequestCreditNote) stlCommand() {}
func (ConfirmCredit) stlCommand()     {}
func (ReverseSettlement) stlCommand() {}

// Validate checks the closed contract.
func (c RequestCreditNote) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.CreditNoteID):
		return schema("/args/credit_note_id")
	case c.CreditKind != CreditPriceAllowance && c.CreditKind != CreditReturn && c.CreditKind != CreditClaim:
		return schema("/args/kind")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case !kernel.ValidIdentifier(c.StatementID):
		return schema("/args/statement_id")
	case !amount(c.Amount):
		return schema("/args/amount")
	case !Reason(c.Basis):
		return schema("/args/basis")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 1)
}

// Validate checks the closed contract.
func (c ConfirmCredit) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.CreditNoteID):
		return schema("/args/credit_note_id")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case c.Decision != DecisionConfirm && c.Decision != DecisionReject:
		return schema("/args/decision")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return nil
}

// Validate checks the closed contract.
func (c ReverseSettlement) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.StatementID):
		return schema("/args/statement_id")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 0)
}

// validateSource checks application.apply's source: exactly one of a fund record and a credit note; a credit
// note only to a receivable and without a rate; the rate a POL fx_rate reference.
func (c Apply) validateSource() error {
	switch {
	case (c.FundRecordID == "") == (c.CreditNoteID == ""):
		return schema("/args/fund_record_id")
	case c.FundRecordID != "" && !kernel.ValidIdentifier(c.FundRecordID):
		return schema("/args/fund_record_id")
	case c.CreditNoteID != "" && !kernel.ValidIdentifier(c.CreditNoteID):
		return schema("/args/credit_note_id")
	case c.CreditNoteID != "" && (c.Target.Kind != TargetReceivable || c.FXRate != nil):
		return schema("/args/credit_note_id")
	case c.FXRate != nil:
		return c.FXRate.Validate(KindFXRate, "/args/fx_rate")
	}
	return nil
}

// CreditNoteReceipt is the answer of credit_note.request and credit.confirm: the receipt (ObjectID the credit
// note, RowVersion its version, Status requested, confirmed or rejected), its kind, statement and the
// statement's version, order, currency and amount, and what the receivable has open now.
type CreditNoteReceipt struct {
	Receipt
	CreditKind         string `json:"kind"`
	StatementID        string
	StatementVersion   int64
	OrderID            string
	Currency           string
	AmountMinor        string
	StatementOpenMinor string
}

// ReversalReceipt is the answer of settlement.reverse: the receipt (ObjectID the statement, RowVersion its
// version, Status reversed), its kind, order (sales) and the source lines it no longer settles.
type ReversalReceipt struct {
	Receipt
	StatementKind string `json:"kind"`
	OrderID       string
	Sources       []SettlementSourceRef
}

// ConversionView is a cross-currency application's conversion (STL-INV-007): what the fund gave in its
// currency, the POL fx_rate revision of purpose settlement and its rate (units of the target's currency per
// unit of the fund's), and — for an order's stage or receivable — the order's analysis currency and rate and
// the business settlement FX difference in minor units of the analysis currency ("" when no rate known to STL
// relates the fund's currency to it).
type ConversionView struct {
	SourceCurrency    string
	SourceAmountMinor string
	FXRate            ConfigRef
	FXRateHash        string
	Rate              string
	AnalysisCurrency  string
	AnalysisRate      string
	FXDifferenceMinor string
}

// CreditNoteRef is CreditNoteRef v1: a credit note as STL states it — kind, state, the statement (with the
// shipment lines it settles, which tie it to FUL's export lots) and order it credits, currency, amount, what stands applied from it and its rest, whether it counts against the
// statement's sales (Effective: confirmed on a standing statement), basis, evidence, who requested and
// decided it, and its applications. CNX cites it in its own records of the impact (CN-AT11); it adjusts no
// tax-refund material by itself.
type CreditNoteRef struct {
	CreditNoteID    string
	CreditKind      string `json:"kind"`
	Status          string
	Version         int64
	StatementID     string
	StatementStatus string
	Sources         []SettlementSourceRef // the shipment lines the statement settles (FUL ShipmentLineRef v1)
	OrderID         string
	CounterpartyID  string
	Currency        string
	AmountMinor     string
	AppliedMinor    string
	RestMinor       string
	Effective       bool
	Basis           string
	EvidenceIDs     []string
	Applications    []AllocationView
	RequestedBy     kernel.ActorRef
	RequestedAt     time.Time
	DecidedBy       kernel.ActorRef
	DecidedAt       time.Time
	DecisionReason  string
}

// CreditNoteView is a credit note as credit_note.get shows it, as of CheckedAt.
type CreditNoteView struct {
	CreditNoteRef
	CheckedAt time.Time
}

// ReversalView is a statement's reversal record.
type ReversalView struct {
	Reason      string
	EvidenceIDs []string
	ReversedBy  kernel.ActorRef
	ReversedAt  time.Time
}

// FXDifferenceView is a cross-currency application of an order as its settlement position shows it.
type FXDifferenceView struct {
	ApplicationID string
	TargetKind    string
	StatementID   string
	StageID       string
	Currency      string
	AmountMinor   string
	Status        string
	ConversionView
}

// CreditNoteCommands is STL's command entry point for credit notes and reversals; errors as for Commands.
type CreditNoteCommands interface {
	RequestCreditNote(ctx context.Context, cmd RequestCreditNote) (CreditNoteReceipt, error)
	ConfirmCredit(ctx context.Context, cmd ConfirmCredit) (CreditNoteReceipt, error)
	ReverseSettlement(ctx context.Context, cmd ReverseSettlement) (ReversalReceipt, error)
}

// CreditNoteQueries is the audited read side of credit notes (stl.read); outside the caller's data scope a
// credit note is NOT_FOUND.
type CreditNoteQueries interface {
	GetCreditNote(ctx context.Context, id string) (CreditNoteView, error)
}

// CreditNoteLookups publishes CreditNoteRef v1 (for CNX, whose external_reconciliation.resolve records a credit
// note's tax impact; CN-AT11): ResolveCreditNotes returns, among ids, the credit notes that exist and the
// caller sees (an active grant and its data scope), never locking anything. Not audited.
type CreditNoteLookups interface {
	ResolveCreditNotes(ctx context.Context, ids []string) (map[string]CreditNoteRef, error)
}

// ValidateCreditNoteIDs checks the IDs of a lookup.
func ValidateCreditNoteIDs(ids []string) error {
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) {
			return schema("/credit_note_ids/" + strconv.Itoa(i))
		}
	}
	return nil
}
