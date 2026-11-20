package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The price proposal (S4 unit G4a; owner decision 2026-11-18: a quotation's unit price comes from a
// sales price list where one is in force, otherwise from the formula cost ÷ (1 − target margin);
// people may change the price, and a price under the minimum margin goes through the margin
// exception). It is a read: before a quote is drafted it says, for every line of a ready RFQ, what
// unit price the company's own data proposes and where it comes from. It writes nothing, decides
// nothing a person decides and binds nobody: quote.save_draft takes the price its author sends,
// equal to the proposal or not, a quote does not record the proposal, and the freeze judges the
// quote's margin as before (TRD-INV-007).
//
// For each RFQ line, in this order:
//
//  1. price_list: the sales price (POL kind sales_price) in force now for the line's SKU in the
//     quote currency and in the line's unit — the customer's own before the general one — as
//     published, with its own basis quantity;
//  2. formula: the line's unit cost in the cost snapshot named, when it is adoptable now and costs
//     this RFQ in the quote currency, divided by one minus the pricing policy's target margin; the
//     arithmetic is exact and the price is rounded once, HALF_UP, to the quote currency's minor
//     units (PricePrecision), for a basis quantity of 1;
//  3. none, with the reason the formula has none (ProposalNo*, ProposalAnotherCurrency,
//     ProposalPriceOutOfRange).
//
// Every proposed price is judged against the snapshot's cost of the line where there is one in the
// quote currency: Margin is (net − cost) ÷ net on the line's net amount at that price, and
// UnderFloor says that it is below the policy's minimum margin, compared exactly as the freeze
// compares the whole quote.
//
// The read is audited as quote.price_proposal under trd.read, for any actor kind, with the RFQ as
// its object. It reads INQ (the RFQ and its lines), POL (the currency, the pricing policy and the
// price list) and CST (the snapshot and its adoptability) through their lookups before TRD's
// transaction. An RFQ the caller does not see is NOT_FOUND; a snapshot it does not see is no
// adoptable snapshot.

// QueryPriceProposal is the audited read name of the price proposal (trd.read).
const QueryPriceProposal = "quote.price_proposal"

// Sources of a proposed price, whose a list price is, and why a line has none.
const (
	PriceSourceList    = "price_list"
	PriceSourceFormula = "formula"
	PriceSourceNone    = "none"

	PriceListCustomer = "customer" // the customer's own sales price
	PriceListGeneral  = "general"  // the sales price for every customer

	// ProposalNoAdoptableSnapshot: no snapshot was named, CST shows the caller none under that
	// reference, it is not adoptable now, or it does not cost the line in its unit and quantity.
	ProposalNoAdoptableSnapshot = "no_adoptable_snapshot"
	// ProposalAnotherCurrency: the snapshot's cost is in another currency than the quote's.
	ProposalAnotherCurrency = "another_currency"
	// ProposalNoTargetMargin: the pricing policy states no target margin.
	ProposalNoTargetMargin = "no_target_margin"
	// ProposalPriceOutOfRange: the formula's price rounds to nothing in the currency's minor units,
	// or has more than 12 integer digits.
	ProposalPriceOutOfRange = "price_out_of_range"
)

// PriceProposalQuery asks for the price proposal of RFQ RfqID {rfq_id, customer_id, currency,
// cost_snapshot_ref, pricing_policy_ref}: for the customer CustomerID (optional: the RFQ's own
// customer, and it must be that one), in the quote currency Currency (its code), on the cost
// snapshot CostSnapshotRef (optional; {snapshot_id, result_hash} as a draft names it) and under
// the pricing policy revision PricingPolicyRef (kind pricing_policy), which are the references the
// quote will bind. The proposal is judged at the instant of the read.
type PriceProposalQuery struct {
	RfqID            string
	CustomerID       string
	Currency         string
	CostSnapshotRef  *CostSnapshotRef
	PricingPolicyRef ConfigRef
}

// Validate checks the closed contract.
func (q PriceProposalQuery) Validate() error {
	switch {
	case !kernel.ValidIdentifier(q.RfqID):
		return schema("/rfq_id")
	case q.CustomerID != "" && !kernel.ValidIdentifier(q.CustomerID):
		return schema("/customer_id")
	case !currencyCode(q.Currency):
		return schema("/currency")
	}
	if r := q.CostSnapshotRef; r != nil {
		switch {
		case !kernel.ValidIdentifier(r.SnapshotID):
			return schema("/cost_snapshot_ref/snapshot_id")
		case !hash64.MatchString(r.ResultHash):
			return schema("/cost_snapshot_ref/result_hash")
		}
	}
	if q.PricingPolicyRef.Kind != PricingPolicyKind {
		return schema("/pricing_policy_ref/kind")
	}
	return ref(q.PricingPolicyRef, "/pricing_policy_ref")
}

// PricingPolicyKind is the POL kind a PriceProposalQuery's pricing policy reference names.
const PricingPolicyKind = "pricing_policy"

// currencyCode reports whether s has the form of an ISO 4217 code.
func currencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

// ProposedLine is the proposal for one RFQ line: its SKU, quantity and unit as the RFQ states
// them, and
//
//   - Source price_list: UnitPrice and PriceBasisQuantity as the sales price PriceListRef (a POL
//     ConfigRef v1 of kind sales_price) states them, PriceListScope customer or general;
//   - Source formula: UnitPrice for PriceBasisQuantity 1, from UnitCost (the snapshot's cost of the
//     line ÷ its quantity; shown exactly when it has at most 8 fractional digits, otherwise rounded
//     HALF_UP to 8 for reading — the price was computed from the exact quotient) and TargetMargin;
//   - Source none: Reason, and no price.
//
// Decimals are strings: UnitPrice with at least the currency's minor units ("8.00"), quantities
// as stated. NetMinor is the line's net amount at the proposed price (quantity × unit price ÷ basis
// quantity, HALF_UP to minor units, no discount, before tax) and CostMinor the snapshot's cost of
// the line, both in minor units of the quote currency and both empty unless the line has a price
// and an adoptable cost in that currency; Margin is then (net − cost) ÷ net, exact or rounded
// HALF_UP to 12 fractional digits, and UnderFloor whether it is below the policy's minimum margin
// (decided exactly, never on the rounded figure). UnderFloor is false where Margin is empty: such a
// price was not judged.
type ProposedLine struct {
	RfqLineID          string
	SkuID              string
	Quantity           string
	Unit               string
	Source             string
	Reason             string
	UnitPrice          string
	PriceBasisQuantity string
	PriceListRef       *ConfigRef
	PriceListScope     string
	UnitCost           string
	TargetMargin       string
	NetMinor           string
	CostMinor          string
	Margin             string
	UnderFloor         bool
}

// PriceProposal is the answer of a PriceProposalQuery: the RFQ at the row version read, the
// customer and currency the proposal is for, PricePrecision (the fractional digits a formula price
// is rounded to: the currency's minor units), the pricing policy with its minimum margin and its
// target margin ("" when it states none), what CST says now of the snapshot named (nil when none
// was named or CST shows the caller none), one ProposedLine per RFQ line in line-ID order, and the
// instant it was judged at.
//
// QuoteValidDays is the policy's bound on a quote's validity and ExpiresAtLatest is CheckedAt plus
// that many days (owner decision 2026-11-18, after the live trials whose drafts lapsed weeks
// later: the drafter is told the bound). The freeze requires issued < expires_at ≤ issued +
// quote_valid_days × 86400 s with issued the instant of the freeze (TRD-INV-005), so a draft
// whose ExpiresAt is not after ExpiresAtLatest is within the bound whenever it is frozen after
// this read, and is QUOTE_EXPIRED only when it is frozen at or after its ExpiresAt. Like the
// price it is advice: quote.save_draft takes the ExpiresAt its author sends, and the freeze
// judges it as before.
type PriceProposal struct {
	RfqID            string
	RfqRowVersion    int64
	CustomerID       string
	Currency         string
	PricePrecision   int
	PricingPolicyRef ConfigRef
	MinimumMargin    string
	TargetMargin     string
	QuoteValidDays   int
	ExpiresAtLatest  time.Time
	CostSnapshot     *CostQualification
	Lines            []ProposedLine
	CheckedAt        time.Time
}

// PriceProposalQueries is the read of the price proposal.
type PriceProposalQueries interface {
	// PriceProposal proposes a unit price for every line of a ready RFQ, audited as
	// quote.price_proposal (trd.read). Refusals: SCHEMA_INVALID for a malformed query; NOT_FOUND at
	// /rfq_id for an RFQ the caller does not see; INVALID_STATE at /rfq_id for one that is not ready
	// (its lines may still change, or it ended); REFERENCE_INVALID at /customer_id for a customer
	// that is not the RFQ's, at /cost_snapshot_ref for a snapshot that costs another RFQ and at
	// /pricing_policy_ref for a policy POL does not know; POL's verdict (CONFIG_NOT_ACTIVE,
	// CONFIG_EXPIRED, REFERENCE_INVALID) at /pricing_policy_ref and /currency for a policy or a
	// currency not in force. A snapshot that is unknown or not adoptable is not a refusal: the lines
	// say no_adoptable_snapshot.
	PriceProposal(ctx context.Context, q PriceProposalQuery) (PriceProposal, error)
}
