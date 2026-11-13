package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Payable adjustments, the invoice_reversed flag and fund reversal (S5R unit R8; simulated sign-off E0 of 2026-11-13:
// S5R-D20, S5R-D22; tactical model STL-AGG-SettlementStatement entity PayableAdjustment, STL-AGG-FundRecord entity
// FundReversal, SM-STL-FUND, STL-INV-013, STL-INV-014; Context Map R55 CNX→STL PurchaseInvoiceRef v1; humanOnlyCommands;
// role templates treasury payable_adjustment.request, credit_manager payable_adjustment.confirm and fund.reverse).
//
//	payable_adjustment.request  human only (treasury): requests an adjustment of the payable of confirmed purchase
//	                            statement StatementID — kind price_allowance (the supplier's allowance, evidenced by a
//	                            red purchase invoice) or under_invoiced (the supplier invoiced less) —, an amount in the
//	                            statement's currency within what is open on the payable (APPLICATION_EXCEEDS_OPEN), the
//	                            CNX purchase invoice that evidences it (PurchaseInvoiceRef v1: verified, of the
//	                            statement's supplier, matched — or reversing an invoice matched — to a commitment the
//	                            statement settles; red for an allowance; REFERENCE_INVALID otherwise), basis and EVD
//	                            evidence. The statement's version moves by one.
//	payable_adjustment.confirm  human only (credit manager), never the requester (INDEPENDENT_REVIEW_REQUIRED):
//	                            confirms or rejects a requested adjustment with a reason. A confirmed adjustment
//	                            reduces what is open on the payable at once and, by its net part (S5R-D46(2)), the
//	                            purchase cost of the order result; the statement's version moves by one.
//	fund.reverse                human only (credit manager), never the fund record's recorder
//	                            (INDEPENDENT_REVIEW_REQUIRED): reverses a fund record — an opening prepayment included —
//	                            with a reason, once nothing stands applied from it (FUND_RECORD_HAS_APPLICATIONS) and
//	                            nothing drawn on it stands (INVALID_STATE: reverse its refunds, returned remittances and
//	                            conversions first). The record stays, reversed (terminal), version + 1; its bank
//	                            reference may be recorded again; an opening prepayment's item is reversed with it.
//
// payable.get, settlement_statement.get (purchase) and settlement.confirm's receipt show invoice_reversed: a purchase
// invoice CNX matched to a commitment line the statement settles was reversed by red invoices, wholly or in part. It is
// derived when read, never stored, and changes nothing (no consumer adjusts a payable: S5R-D20). InvoicesShown is false
// when CNX's PurchaseInvoiceRef v1 is not available to STL.
//
// Request members: payable_adjustment.request {adjustment_id, statement_id, kind, amount, purchase_invoice_id, basis,
// evidence_ids}; payable_adjustment.confirm {adjustment_id, expected_version, decision, reason}; fund.reverse
// {fund_record_id, expected_version, reason, evidence_ids}. Events (source idp-brant.stl): payable_adjustment_confirmed.v1
// {statement_id, statement_version, adjustment_id, kind, amount_minor, currency, purchase_invoice_id} (aggregate
// SettlementStatement; confirm only), fund_record_reversed.v1 {fund_record_id, fund_record_version, kind, direction,
// reason} (aggregate FundRecord).
const (
	CommandRequestPayableAdjustment = "payable_adjustment.request"
	CommandConfirmPayableAdjustment = "payable_adjustment.confirm"
	CommandReverseFund              = "fund.reverse"

	EventPayableAdjustmentConfirmed = "idp-brant.stl.payable_adjustment_confirmed.v1"
	EventFundRecordReversed         = "idp-brant.stl.fund_record_reversed.v1"

	// Kinds and states of a payable adjustment; the reversed state of a fund record.
	AdjustmentPriceAllowance = "price_allowance"
	AdjustmentUnderInvoiced  = "under_invoiced"
	AdjustmentRequested      = "requested"
	AdjustmentConfirmed      = "confirmed"
	AdjustmentRejected       = "rejected"
	FundReversed             = "reversed"

	// CodeFundRecordHasApplications: something stands applied from the fund record (unapply first).
	CodeFundRecordHasApplications = "FUND_RECORD_HAS_APPLICATIONS"
)

// RequestPayableAdjustment requests adjustment AdjustmentID of AdjustmentKind ("kind": price_allowance or
// under_invoiced) on the payable of purchase statement StatementID: Amount in the statement's currency, evidenced by
// CNX purchase invoice PurchaseInvoiceID, on Basis, resting on EvidenceIDs (1 to 20 EVD sources). Human only.
type RequestPayableAdjustment struct {
	IdempotencyKey    string
	AdjustmentID      string
	StatementID       string
	AdjustmentKind    string `json:"kind"`
	Amount            string
	PurchaseInvoiceID string
	Basis             string
	EvidenceIDs       []string
}

// ConfirmPayableAdjustment decides adjustment AdjustmentID at ExpectedVersion: Decision confirm or reject, for Reason.
// Human only, not the requester.
type ConfirmPayableAdjustment struct {
	IdempotencyKey  string
	AdjustmentID    string
	ExpectedVersion int64
	Decision        string
	Reason          string
}

// ReverseFund reverses fund record FundRecordID at ExpectedVersion for Reason, resting on EvidenceIDs (0 to 20 EVD
// sources). Human only, not the recorder.
type ReverseFund struct {
	IdempotencyKey  string
	FundRecordID    string
	ExpectedVersion int64
	Reason          string
	EvidenceIDs     []string
}

func (RequestPayableAdjustment) stlCommand() {}
func (ConfirmPayableAdjustment) stlCommand() {}
func (ReverseFund) stlCommand()              {}

// Validate checks the closed contract.
func (c RequestPayableAdjustment) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.AdjustmentID):
		return schema("/args/adjustment_id")
	case !kernel.ValidIdentifier(c.StatementID):
		return schema("/args/statement_id")
	case c.AdjustmentKind != AdjustmentPriceAllowance && c.AdjustmentKind != AdjustmentUnderInvoiced:
		return schema("/args/kind")
	case !amount(c.Amount):
		return schema("/args/amount")
	case !kernel.ValidIdentifier(c.PurchaseInvoiceID):
		return schema("/args/purchase_invoice_id")
	case !Reason(c.Basis):
		return schema("/args/basis")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 1)
}

// Validate checks the closed contract.
func (c ConfirmPayableAdjustment) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.AdjustmentID):
		return schema("/args/adjustment_id")
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
func (c ReverseFund) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.FundRecordID):
		return schema("/args/fund_record_id")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 0)
}

// PayableAdjustmentReceipt is the answer of payable_adjustment.request and .confirm: the receipt (ObjectID the
// adjustment, RowVersion its version, Status requested, confirmed or rejected), its kind, statement and the statement's
// version, currency, amount, invoice, and what the payable has open now.
type PayableAdjustmentReceipt struct {
	Receipt
	AdjustmentKind     string `json:"kind"`
	StatementID        string
	StatementVersion   int64
	Currency           string
	AmountMinor        string
	PurchaseInvoiceID  string
	StatementOpenMinor string
}

// PayableAdjustmentView is a payable adjustment as the reads of its statement show it: kind, state, amount with its
// net and tax parts (S5R-D46(2)), the invoice, basis, evidence, who requested and decided it, and whether it counts
// (Effective: confirmed on a standing statement).
type PayableAdjustmentView struct {
	AdjustmentID      string
	AdjustmentKind    string `json:"kind"`
	Status            string
	Version           int64
	Currency          string
	AmountMinor       string
	NetMinor          string
	TaxMinor          string
	PurchaseInvoiceID string
	Basis             string
	EvidenceIDs       []string
	Effective         bool
	RequestedBy       kernel.ActorRef
	RequestedAt       time.Time
	DecidedBy         kernel.ActorRef
	DecidedAt         time.Time
	DecisionReason    string
}

// PurchaseInvoiceView is a CNX purchase invoice of a payable's commitments as PurchaseInvoiceRef v1 states it: its
// colour, state and number, the blue invoice a red one reverses and the red ones reversing a blue one, and Reversal
// ("", partial or full).
type PurchaseInvoiceView struct {
	PurchaseInvoiceID string
	Colour            string
	Status            string
	Number            string
	RedOf             string
	ReversedBy        []string
	Reversal          string
}

// FundReversalView is a fund record's reversal: why, on which evidence, who reversed it and when.
type FundReversalView struct {
	Reason      string
	EvidenceIDs []string
	ReversedBy  kernel.ActorRef
	ReversedAt  time.Time
}

// SettlementRateView is the POL settlement rate a receipt in the order's currency recorded when it was applied to an
// order analysed in another currency (S5R-D19): the revision, its hash, the rate and the analysis currency.
type SettlementRateView struct {
	FXRate     ConfigRef
	FXRateHash string
	Rate       string
	Currency   string
}

// PayableCommands is STL's command entry point for payable adjustments and fund reversal; errors as for Commands.
type PayableCommands interface {
	RequestPayableAdjustment(ctx context.Context, cmd RequestPayableAdjustment) (PayableAdjustmentReceipt, error)
	ConfirmPayableAdjustment(ctx context.Context, cmd ConfirmPayableAdjustment) (PayableAdjustmentReceipt, error)
	ReverseFund(ctx context.Context, cmd ReverseFund) (FundRecordReceipt, error)
}
