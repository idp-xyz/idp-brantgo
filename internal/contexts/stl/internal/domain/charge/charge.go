// Package charge holds STL's Charge rules (tactical model STL-AGG-Charge with its entity ChargeAllocation,
// SM-STL-CHARGE, STL-INV-010; Context Map MD-49; baseline 4.2, 4.5, 4.6 S-A, 4.13; WBS S5-17).
//
// A charge states where it comes from (a supplier bill, a logistics offer or an internal charge), which orders or
// shipments it benefits, its POL fee category, its currency and how it converts into an order's analysis
// currency, its amount and its state: estimated, pending once its bill or basis is recorded, confirmed once an
// authorised person checked the bill. Estimated, pending and confirmed charges are recorded apart: an estimate
// keeps its estimated amount when the bill replaces it (the forecast reads the estimate, the confirmed result the
// bill), so a charge counts once. A charge may be an adjustment below zero (baseline 4.2 "修订估算或新增已确认费用调整").
//
// charge.allocate splits the amount over orders or shipments by amount, quantity or weight: every weight has a
// data basis and is not negative, their sum is above zero (ALLOCATION_INVALID otherwise: nothing is split evenly by
// default), the exact shares are floored to minor units and the units left go to the largest remainders, ties by
// line ID; a negative charge is split on its absolute value and signed back, so no line absorbs rounding against
// the charge's sign (STL-INV-010, baseline 4.5). Allocations are only appended; the charge names the one that
// counts. A bill that changes the amount drops the allocation: the charge is allocated again.
package charge

import (
	"slices"
	"sort"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// States, sources, targets, bases and conversion kinds of a charge.
const (
	StatusEstimated = "estimated"
	StatusPending   = "pending"
	StatusConfirmed = "confirmed"

	SourceSupplierBill   = "supplier_bill"
	SourceLogisticsOffer = "logistics_offer"
	SourceInternal       = "internal"

	TargetOrder    = "order"
	TargetShipment = "shipment"

	BasisAmount   = "amount"
	BasisQuantity = "quantity"
	BasisWeight   = "weight"

	ConversionSameCurrency  = "same_currency"
	ConversionOrderAnalysis = "order_analysis"
	ConversionFXRate        = "fx_rate"
)

// CodeAllocationInvalid: the allocation basis does not hold — a weight missing or negative, or the weights sum
// to zero (STL-INV-010; error catalog ALLOCATION_INVALID, 422).
const CodeAllocationInvalid = "ALLOCATION_INVALID"

// Codes are the codes the charge rules give besides the kernel's, with their categories.
var Codes = map[string]kernel.Category{CodeAllocationInvalid: kernel.CategoryInvalid}

// Reject returns a rejection with one of Codes at path.
func Reject(code, path, reason string) *kernel.Rejection {
	return kernel.RejectAs(code, Codes[code], path, reason)
}

// Target is an order, or a shipment of an order, that a charge benefits or an allocation line goes to.
type Target struct {
	Kind       string
	OrderID    string
	ShipmentID string
}

// Key names the target in a set.
func (t Target) Key() string { return t.Kind + "/" + t.OrderID + "/" + t.ShipmentID }

// Source is where a charge comes from: a supplier bill or a logistics offer of party PartyID with its reference,
// or an internal charge.
type Source struct {
	Kind      string
	PartyID   string
	Reference string
}

// ConfigUse is a POL configuration revision a charge names.
type ConfigUse struct {
	ID       string
	Revision int64
}

// RateUse is the POL fx_rate revision of purpose settlement a charge converts at: one unit of the charge's
// currency is Value units of To.
type RateUse struct {
	ID       string
	Revision int64
	Hash     string
	Value    kernel.Decimal
	To       string
}

// Conversion is how a charge converts into an order's analysis currency: it is in that currency
// (same_currency, exactly 1), it is in the order's currency and converts at the order's analysis rate
// (order_analysis), or at a POL settlement rate (fx_rate).
type Conversion struct {
	Kind string
	Rate *RateUse
}

// Charge is the root of STL-AGG-Charge.
type Charge struct {
	ID                string
	Status            string
	Version           int64
	Source            Source
	EstimateReference string // an estimate's own reference once its bill replaced it
	FeeCategory       ConfigUse
	CurrencyRevision  int64
	Estimate          *kernel.Money // the estimated amount, when the charge began as an estimate
	Amount            kernel.Money  // the amount now: the estimate, or the bill's
	Conversion        Conversion
	OccurredOn        time.Time // business dates (calendar dates at UTC midnight): when it was incurred,
	AnalysisOn        time.Time // and the date it is analysed at (baseline 4.13)
	Basis             string
	EvidenceIDs       []string
	Beneficiaries     []Target
	OwnerActor        string
	OwnerTeam         string
	Recorded          fundrecord.Act
	Billed            *fundrecord.Act
	Confirmed         *fundrecord.Act // with the confirmation's reason
	ConfirmEvidence   []string
	AllocationSeq     int // the allocation that counts; 0: none
	Allocations       []Allocation
}

// Allocation is one allocation of a charge (entity ChargeAllocation): its amount at the charge version it moved
// the charge to, the basis and data basis of its weights, and its lines.
type Allocation struct {
	Seq           int
	ChargeVersion int64
	Basis         string
	DataBasis     string
	EvidenceIDs   []string
	Amount        kernel.Money
	Lines         []Line
	By            fundrecord.Act
}

// Line is one line of an allocation: its target, weight and share.
type Line struct {
	LineID string
	Seq    int
	Target Target
	Weight kernel.Decimal
	Amount kernel.Money
}

// Currency is the charge's currency.
func (c *Charge) Currency() kernel.Currency { return c.Amount.Currency() }

// Current returns the allocation that counts, or nil.
func (c *Charge) Current() *Allocation {
	for i := range c.Allocations {
		if c.Allocations[i].Seq == c.AllocationSeq && c.AllocationSeq > 0 {
			return &c.Allocations[i]
		}
	}
	return nil
}

// Orders returns the orders the charge benefits, sorted.
func (c *Charge) Orders() []string {
	var out []string
	for _, b := range c.Beneficiaries {
		if !slices.Contains(out, b.OrderID) {
			out = append(out, b.OrderID)
		}
	}
	sort.Strings(out)
	return out
}

// ForecastAmount is what the forecast counts: the estimate when the charge began as one, its amount otherwise.
func (c *Charge) ForecastAmount() kernel.Money {
	if c.Estimate != nil {
		return *c.Estimate
	}
	return c.Amount
}

func state(p, reason string) error   { return kernel.Reject(kernel.CodeInvalidState, p, reason) }
func invalid(p, reason string) error { return kernel.Reject(kernel.CodeInvalidKnownValue, p, reason) }

// RecordInput is a new charge as charge.record states it, the references already resolved.
type RecordInput struct {
	ID               string
	Status           string // estimated or pending
	Source           Source
	FeeCategory      ConfigUse
	CurrencyRevision int64
	Amount           kernel.Money
	Conversion       Conversion
	OccurredOn       time.Time
	AnalysisOn       time.Time
	Basis            string
	EvidenceIDs      []string
	Beneficiaries    []Target
	By               fundrecord.Act
	Team             string
}

// Record is charge.record's rule for a new charge (SM-STL-CHARGE: an estimate starts estimated; a bill or basis
// received makes it pending). It needs an amount other than zero and distinct beneficiaries.
func Record(in RecordInput) (*Charge, error) {
	switch {
	case in.Status != StatusEstimated && in.Status != StatusPending:
		return nil, invalid("/args/status", "A charge is recorded estimated or pending")
	case in.Amount.IsZero():
		return nil, invalid("/args/amount", "A charge amounts to something other than zero")
	case len(in.Beneficiaries) == 0:
		return nil, invalid("/args/beneficiaries", "A charge benefits an order or a shipment")
	}
	seen := map[string]bool{}
	for i, b := range in.Beneficiaries {
		if seen[b.Key()] {
			return nil, invalid("/args/beneficiaries/"+strconv.Itoa(i), "The beneficiary is named twice")
		}
		seen[b.Key()] = true
	}
	c := &Charge{ID: in.ID, Status: in.Status, Version: 1, Source: in.Source, FeeCategory: in.FeeCategory, CurrencyRevision: in.CurrencyRevision,
		Amount: in.Amount, Conversion: in.Conversion, OccurredOn: in.OccurredOn, AnalysisOn: in.AnalysisOn, Basis: in.Basis,
		EvidenceIDs: in.EvidenceIDs, Beneficiaries: in.Beneficiaries, OwnerActor: in.By.By.ID, OwnerTeam: in.Team, Recorded: in.By}
	if in.Status == StatusEstimated {
		e := in.Amount
		c.Estimate = &e
	}
	return c, nil
}

// BillInput is charge.record of the bill or basis of an estimated charge.
type BillInput struct {
	Source      Source
	Amount      kernel.Money
	OccurredOn  time.Time
	AnalysisOn  time.Time
	Basis       string
	EvidenceIDs []string
	By          fundrecord.Act
}

// Bill moves an estimated charge to pending with its bill or basis (SM-STL-CHARGE estimated → pending, "收到供应商
// 账单或费用依据"): the amount, source, dates, basis and evidence become the bill's, the estimate stays. A bill of
// another amount drops the allocation (it split the estimate).
func (c *Charge) Bill(in BillInput) error {
	switch {
	case c.Status != StatusEstimated:
		return state("/args/charge_id", "The charge is "+c.Status+": only an estimated charge takes its bill")
	case in.Amount.IsZero():
		return invalid("/args/amount", "A charge amounts to something other than zero")
	case in.Amount.Currency() != c.Currency():
		return invalid("/args/amount", "The bill is in the charge's currency "+c.Currency().Code())
	}
	if !in.Amount.Equal(c.Amount) {
		c.AllocationSeq = 0
	}
	if c.Source.Reference != "" && c.Source.Reference != in.Source.Reference {
		c.EstimateReference = c.Source.Reference
	}
	by := in.By
	c.Status, c.Version, c.Billed = StatusPending, c.Version+1, &by
	c.Source, c.Amount, c.OccurredOn, c.AnalysisOn, c.Basis, c.EvidenceIDs = in.Source, in.Amount, in.OccurredOn, in.AnalysisOn, in.Basis, in.EvidenceIDs
	return nil
}

// Confirm confirms an estimated or pending charge with its bill as evidence (SM-STL-CHARGE; human only).
func (c *Charge) Confirm(by fundrecord.Act, evidence []string) error {
	if c.Status == StatusConfirmed {
		return state("/args/charge_id", "The charge is already confirmed")
	}
	c.Status, c.Version, c.Confirmed, c.ConfirmEvidence = StatusConfirmed, c.Version+1, &by, evidence
	return nil
}

// LineInput is one line of charge.allocate: its target and weight as stated (a decimal, "-" for negative).
type LineInput struct {
	LineID string
	Target Target
	Weight string
}

// AllocateInput is charge.allocate as the rules see it: Allowed are the targets the charge may go to — its
// beneficiaries' orders and the shipments of them FUL shows.
type AllocateInput struct {
	Basis       string
	DataBasis   string
	EvidenceIDs []string
	Lines       []LineInput
	Allowed     map[string]bool
	By          fundrecord.Act
}

func linePath(i int) string { return "/args/lines/" + strconv.Itoa(i) }

// Allocate is charge.allocate's rule (STL-INV-010): every target once and among Allowed, every weight not
// negative, their sum above zero; the shares by Split. The new allocation becomes the one that counts.
func (c *Charge) Allocate(in AllocateInput) (*Allocation, error) {
	if len(in.Lines) == 0 {
		return nil, Reject(CodeAllocationInvalid, "/args/lines", "An allocation has at least one line")
	}
	lines := make([]Line, 0, len(in.Lines))
	ids, targets := map[string]bool{}, map[string]bool{}
	for i, l := range in.Lines {
		p := linePath(i)
		switch {
		case ids[l.LineID]:
			return nil, kernel.Reject(kernel.CodeDuplicateLineID, p+"/line_id", "The line ID is used twice")
		case targets[l.Target.Key()]:
			return nil, invalid(p+"/target", "The target is allocated twice: a charge counts once")
		case !in.Allowed[l.Target.Key()]:
			return nil, invalid(p+"/target", "The charge benefits no such order or shipment of its orders")
		}
		ids[l.LineID], targets[l.Target.Key()] = true, true
		w, err := Weight(l.Weight)
		if err != nil {
			return nil, Reject(CodeAllocationInvalid, p+"/weight", "The weight is not a number with a data basis")
		}
		if w.Sign() < 0 {
			return nil, Reject(CodeAllocationInvalid, p+"/weight", "A weight is never negative")
		}
		lines = append(lines, Line{LineID: l.LineID, Seq: i + 1, Target: l.Target, Weight: w})
	}
	shares, err := Split(c.Amount, lines)
	if err != nil {
		return nil, err
	}
	for i := range lines {
		lines[i].Amount = shares[i]
	}
	c.Version++
	a := Allocation{Seq: len(c.Allocations) + 1, ChargeVersion: c.Version, Basis: in.Basis, DataBasis: in.DataBasis, EvidenceIDs: in.EvidenceIDs,
		Amount: c.Amount, Lines: lines, By: in.By}
	c.Allocations = append(c.Allocations, a)
	c.AllocationSeq = a.Seq
	return &c.Allocations[len(c.Allocations)-1], nil
}

// Weight parses a weight as charge.allocate states it: a decimal in QuantityFormat ("quantities, weights,
// dimensions"), with a leading "-" when negative (refused by the rules, not by the contract).
func Weight(s string) (kernel.Decimal, error) {
	neg := len(s) > 0 && s[0] == '-'
	if neg {
		s = s[1:]
	}
	d, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil {
		return kernel.Decimal{}, err
	}
	if neg {
		d = d.Neg()
	}
	return d, nil
}

// Split splits amount over the lines' weights by the largest remainder in minor units, ties by line ID; a
// negative amount on its absolute value, then signed (baseline 4.5, STL-INV-010). Weights summing to zero are
// ALLOCATION_INVALID.
func Split(amount kernel.Money, lines []Line) ([]kernel.Money, error) {
	shares := make([]kernel.Share, 0, len(lines))
	sum := kernel.DecimalFromInt(0)
	for _, l := range lines {
		shares = append(shares, kernel.Share{ID: l.LineID, Weight: l.Weight})
		sum = sum.Add(l.Weight)
	}
	if sum.Sign() <= 0 {
		return nil, Reject(CodeAllocationInvalid, "/args/lines", "The weights sum to zero: there is no basis to split the charge on")
	}
	abs := amount
	if amount.Sign() < 0 {
		abs = amount.Neg()
	}
	parts, err := abs.Allocate(shares)
	if err != nil {
		return nil, Reject(CodeAllocationInvalid, "/args/lines", err.Error())
	}
	if amount.Sign() < 0 {
		for i := range parts {
			parts[i] = parts[i].Neg()
		}
	}
	return parts, nil
}

// Share is what of a charge goes to one target of an order: its order and shipment ("" for the order itself).
type Share struct {
	Target Target
	Amount kernel.Money
}

// SharesOf splits amount — the charge's amount or its estimate — as the charge stands: by its allocation's
// weights when it has one (its stored shares when amount is the allocated amount), wholly to its only
// beneficiary, or wholly to the order level of its only order when every beneficiary is of one order. ok is false
// when the charge is not attributable: several orders and no allocation.
func (c *Charge) SharesOf(amount kernel.Money) (shares []Share, ok bool) {
	if a := c.Current(); a != nil {
		if amount.Equal(a.Amount) {
			for _, l := range a.Lines {
				shares = append(shares, Share{Target: l.Target, Amount: l.Amount})
			}
			return shares, true
		}
		parts, err := Split(amount, a.Lines)
		if err != nil {
			return nil, false
		}
		for i, l := range a.Lines {
			shares = append(shares, Share{Target: l.Target, Amount: parts[i]})
		}
		return shares, true
	}
	if len(c.Beneficiaries) == 1 {
		return []Share{{Target: c.Beneficiaries[0], Amount: amount}}, true
	}
	if orders := c.Orders(); len(orders) == 1 {
		return []Share{{Target: Target{Kind: TargetOrder, OrderID: orders[0]}, Amount: amount}}, true
	}
	return nil, false
}
