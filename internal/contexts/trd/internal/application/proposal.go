package application

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The price proposal (api/price_proposal.go; owner decision 2026-11-18, S4 unit G4a): an audited
// read under trd.read. What it needs of INQ (the RFQ), POL (the pricing policy, the currency and
// the price list) and CST (the cost snapshot with its adoptability) it reads through their lookups
// before TRD's transaction, each in a transaction of its owner (Context Map R11, R12, R15); the
// rules are calc.ProposePrices. The read's own transaction authorizes the caller and records the
// read, or the refusal, in TRD's audit trail; it reads no TRD table and writes nothing else.

// proposalInputs are what the upstreams said before the read's transaction.
type proposalInputs struct {
	rfq      inqapi.RfqRef
	policy   polapi.PricingPolicy
	prices   polapi.SalesPrices
	snapshot *cstapi.ResolvedSnapshot // nil: none named, or CST shows the caller none
}

// PriceProposal proposes a unit price for every line of a ready RFQ, audited as quote.price_proposal
// (api.PriceProposalQueries). A refusal of an upstream lookup is the read's refusal: it is decided
// before the transaction and returned, and audited, inside it, after the caller's trd.read.
func (s Service) PriceProposal(ctx context.Context, q api.PriceProposalQuery) (api.PriceProposal, error) {
	if validation := q.Validate(); validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, api.QueryPriceProposal, q.RfqID, rej.Code); err != nil {
				return api.PriceProposal{}, errors.Join(validation, err)
			}
		}
		return api.PriceProposal{}, validation
	}
	now := s.Clock.Now()
	in, refused := s.proposalInputs(ctx, q, now)
	if _, ok := kernel.AsRejection(refused); refused != nil && !ok {
		return api.PriceProposal{}, refused
	}
	var out api.PriceProposal
	err := s.Ledger.Read(ctx, api.QueryPriceProposal, q.RfqID, func(context.Context, bentopg.Executor, authorization.Decision) error {
		if refused != nil {
			return refused
		}
		var err error
		out, err = proposal(q, in, now)
		return err
	})
	if err != nil {
		return api.PriceProposal{}, err
	}
	return out, nil
}

// proposalInputs asks INQ, POL and CST about what q names at instant at. A rejection is the
// proposal's refusal, at the path of q's member.
func (s Service) proposalInputs(ctx context.Context, q api.PriceProposalQuery, at time.Time) (proposalInputs, error) {
	var in proposalInputs
	rfqs, err := s.Upstream.Rfqs.ResolveRfqs(ctx, []string{q.RfqID})
	if err != nil {
		return in, err
	}
	rfq, known := rfqs[q.RfqID]
	switch {
	case !known:
		return in, kernel.Reject(kernel.CodeNotFound, "/rfq_id", "")
	case !rfq.Ready():
		return in, kernel.Reject(kernel.CodeInvalidState, "/rfq_id", "Prices are proposed for a ready RFQ; this one is "+rfq.Status)
	case q.CustomerID != "" && q.CustomerID != rfq.CustomerID:
		return in, kernel.Reject(kernel.CodeReferenceInvalid, "/customer_id", "The customer is not the RFQ's")
	}
	in.rfq = rfq

	const policyPath = "/pricing_policy_ref"
	policy := polapi.ConfigRef{Kind: q.PricingPolicyRef.Kind, ConfigID: q.PricingPolicyRef.ID, Revision: q.PricingPolicyRef.Revision}
	if policy.Validate(policyPath) != nil {
		return in, kernel.Reject(kernel.CodeReferenceInvalid, policyPath, "") // an ID POL cannot hold is simply unknown
	}
	configs, err := s.Upstream.Configs.ResolveConfigs(ctx, []polapi.ConfigRef{policy}, at)
	if err != nil {
		return in, err
	}
	rc, known := configs[policy]
	if !known || rc.Data.PricingPolicy == nil {
		return in, kernel.Reject(kernel.CodeReferenceInvalid, policyPath, "")
	}
	if err = rc.Check(policyPath); err != nil {
		return in, err
	}
	in.policy = *rc.Data.PricingPolicy

	var skus []string
	for _, l := range rfq.Lines {
		if kernel.ValidIdentifier(l.SkuID) && !slices.Contains(skus, l.SkuID) {
			skus = append(skus, l.SkuID)
		}
	}
	if len(skus) == 0 {
		return in, kernel.Reject(kernel.CodeInvalidState, "/rfq_id", "The RFQ has no lines")
	}
	// POL refuses a currency that is not configured and in force at /currency, the query's member.
	if in.prices, err = s.Upstream.Prices.LookupSalesPrices(ctx, polapi.SalesPriceQuery{SkuIDs: skus, PartyID: rfq.CustomerID,
		Currency: q.Currency, At: at}); err != nil {
		return in, err
	}

	if r := q.CostSnapshotRef; r != nil {
		ref := cstapi.CostSnapshotRef{SnapshotID: r.SnapshotID, ResultHash: r.ResultHash}
		found, err := s.Upstream.Snapshots.ResolveSnapshots(ctx, []cstapi.CostSnapshotRef{ref}, at)
		if err != nil {
			return in, err
		}
		if rs, ok := found[ref]; ok {
			if rs.Rfq.RfqID != q.RfqID {
				return in, kernel.Reject(kernel.CodeReferenceInvalid, "/cost_snapshot_ref", "The cost snapshot costs another RFQ")
			}
			in.snapshot = &rs
		}
	}
	return in, nil
}

// proposal applies calc.ProposePrices to what the upstreams said and returns it in the contract's
// form, judged at instant at.
func proposal(q api.PriceProposalQuery, in proposalInputs, at time.Time) (api.PriceProposal, error) {
	currency := in.prices.Currency
	minimum, err := kernel.ParseDecimal(in.policy.MinimumMargin, kernel.FactorFormat)
	if err != nil {
		return api.PriceProposal{}, kernel.Reject(kernel.CodeReferenceInvalid, "/pricing_policy_ref", "The pricing policy has no minimum margin")
	}
	basis := calc.ProposalBasis{Currency: currency, Customer: in.rfq.CustomerID, Minimum: minimum}
	if in.policy.TargetMargin != "" {
		target, err := kernel.ParseDecimal(in.policy.TargetMargin, kernel.FactorFormat)
		if err != nil {
			return api.PriceProposal{}, kernel.Reject(kernel.CodeReferenceInvalid, "/pricing_policy_ref", "The pricing policy's target margin is not a factor")
		}
		basis.Target = &target
	}
	for _, p := range in.prices.Prices {
		basis.Prices = append(basis.Prices, calc.ListPrice{Ref: calc.Ref{Kind: p.Ref.Kind, ID: p.Ref.ConfigID, Revision: p.Ref.Revision},
			SkuID: p.SkuID, Customer: p.PartyID, Unit: p.Unit, UnitPrice: p.UnitPrice, BasisQuantity: p.PriceBasisQuantity})
	}
	out := api.PriceProposal{RfqID: in.rfq.RfqID, RfqRowVersion: in.rfq.RowVersion, CustomerID: in.rfq.CustomerID, Currency: currency.Code(),
		PricePrecision: currency.MinorUnits(), PricingPolicyRef: q.PricingPolicyRef, MinimumMargin: in.policy.MinimumMargin,
		TargetMargin: in.policy.TargetMargin, Lines: []api.ProposedLine{}, CheckedAt: kernel.Instant(at)}
	if snap := in.snapshot; snap != nil {
		out.CostSnapshot = &api.CostQualification{SnapshotID: snap.Ref.SnapshotID, ResultHash: snap.Ref.ResultHash, EvidenceHash: snap.EvidenceHash,
			Status: snap.Adoptability.Status, Adoptable: snap.Adoptability.Adoptable()}
		if snap.Adoptability.Adoptable() {
			basis.Cost = snapshotCost(*snap, currency)
		}
	}
	var lines []calc.PricedLine
	for _, l := range in.rfq.Lines {
		quantity, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if err != nil {
			return api.PriceProposal{}, kernel.Reject(kernel.CodeReferenceInvalid, "/rfq_id", "An RFQ line has no quantity")
		}
		lines = append(lines, calc.PricedLine{LineID: l.LineID, SkuID: l.SkuID, Quantity: quantity, Unit: l.UOM})
	}
	for i, p := range calc.ProposePrices(lines, basis) {
		rl := in.rfq.Lines[i]
		line := api.ProposedLine{RfqLineID: rl.LineID, SkuID: rl.SkuID, Quantity: rl.Quantity, Unit: rl.UOM, Source: p.Source, Reason: p.Reason,
			UnderFloor: p.UnderFloor()}
		if p.Source != calc.PriceNone {
			line.UnitPrice, line.PriceBasisQuantity = priceText(p.UnitPrice, currency.MinorUnits()), p.BasisQuantity.String()
		}
		if p.List != nil {
			line.PriceListRef = &api.ConfigRef{Kind: p.List.Ref.Kind, ID: p.List.Ref.ID, Revision: p.List.Ref.Revision}
			line.PriceListScope = p.ListScope
		}
		if p.UnitCost != nil {
			line.UnitCost, line.TargetMargin = priceText(p.UnitCost.RoundHalfUp(calc.PriceScale), currency.MinorUnits()), in.policy.TargetMargin
		}
		if p.Margin != nil {
			line.NetMinor, line.CostMinor = p.Net.MinorString(), p.Cost.MinorString()
			line.Margin = p.Margin.Rate.RoundHalfUp(calc.FactorScale).String()
		}
		out.Lines = append(out.Lines, line)
	}
	return out, nil
}

// snapshotCost is what adoptable snapshot s costs each RFQ line, in its own currency: the quote
// currency when the codes are equal, otherwise a currency that only says it is another one (its
// amounts are then never used). A line whose amount or quantity CST's result does not state as a
// number is left out: the formula has no cost for it.
func snapshotCost(s cstapi.ResolvedSnapshot, quote kernel.Currency) *calc.SnapshotCost {
	currency := quote
	if s.Currency != quote.Code() {
		other, err := kernel.NewCurrency(s.Currency, 0)
		if err != nil {
			return nil
		}
		currency = other
	}
	out := &calc.SnapshotCost{Currency: currency, Lines: map[string]calc.LineCost{}}
	for _, l := range s.Lines {
		amount, err := kernel.ParseMinor(currency, l.AmountMinor)
		if err != nil {
			continue
		}
		quantity, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if err != nil {
			continue
		}
		out.Lines[l.LineID] = calc.LineCost{Amount: amount, Quantity: quantity, Unit: l.Unit}
	}
	return out
}

// priceText is unit price d as a decimal string with at least minor fractional digits and at most
// the 8 of a unit price: 8 is "8.00" in USD, 812.5 "812.50", 7.33333333 as it is.
func priceText(d kernel.Decimal, minor int) string {
	s, exact := d.Exact()
	if !exact {
		return d.StringFixed(calc.PriceScale)
	}
	if _, frac, _ := strings.Cut(s, "."); len(frac) < minor {
		return d.StringFixed(minor)
	}
	return s
}
