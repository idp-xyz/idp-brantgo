package api

import (
	"context"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Charges, charge allocation and the order operating result (S5 unit C9, WBS S5-17; tactical model STL-AGG-Charge,
// STL-AGG-OrderResult; SM-STL-CHARGE, SM-STL-ORDER-RESULT; STL-INV-010, STL-INV-011; Context Map MD-49; UL
// STL-OrderResult; baseline 4.2, 4.5, 4.6 S-A, 4.13; BT18, BT24).
//
//	charge.record        human or AI (credit officer): a new charge (expected_version 0) — its source (supplier_bill
//	                     or logistics_offer of a party with its reference, or internal), the orders or shipments it
//	                     benefits, its POL fee_category and currency, its conversion basis into each order's
//	                     analysis currency (same_currency, order_analysis, or fx_rate with a POL fx_rate of purpose
//	                     settlement), its amount (a negative adjustment allowed, never zero), occurred_on and
//	                     analysis_on (calendar dates), basis and evidence; status estimated, or pending with its
//	                     bill or basis (evidence required). With the version of an estimated charge it records the
//	                     bill: estimated → pending (source, amount, dates, basis and evidence; the estimate is kept,
//	                     a bill of another amount drops the allocation). A bill or offer of a party is recorded once
//	                     (BUSINESS_KEY_CONFLICT).
//	charge.confirm       human only (treasury): estimated or pending → confirmed with the bill as EVD evidence.
//	charge.allocate      human or AI (credit officer): splits the amount over orders or shipments of the charge's
//	                     orders by amount, quantity or weight — weights with a data basis, none negative, their sum
//	                     above zero (ALLOCATION_INVALID), by the largest remainder in minor units, ties by line ID, a
//	                     negative charge on its absolute value then signed (STL-INV-010). The newest allocation
//	                     counts; allocations are kept.
//	order_result.review  human or AI (treasury): open → reviewed, recording the figures and close conditions as they
//	                     stand (expected_version 0 for an order without a result yet).
//	order_result.close   human only (treasury): reviewed → settled when every close condition holds (STL-INV-011):
//	                     fulfilment complete or short-closed (TRD), receivables and payables settled, purchase costs
//	                     and direct charges confirmed, and no open claim (met when the order has none). Otherwise
//	                     ORDER_RESULT_NOT_SETTLED with one field per unmet dimension (/conditions/<dimension>). It
//	                     appends an immutable snapshot.
//	order_result.reopen  human only (treasury): settled → open for a late charge or adjustment, with a reason; the
//	                     snapshot stays, the next close appends the next one (BT24).
//
// While an order's result is settled, charge.confirm and charge.allocate of a charge that benefits that order are
// INVALID_STATE (reopen first; charge.record of a late charge is allowed).
//
// S5R unit R8: a new charge states its cost_component and whether it is recharged (S5R-D24: the main carriage of an
// EXW, FCA, FAS or FOB order is the buyer's — INVALID_KNOWN_VALUE unless recharged; a recharged charge is not in the
// result); a confirmed charge is paid by application.apply {target {kind: "charge"}} (S5R-D19: charge.get derives its
// payment state; net cash counts the payments, unpaid charges are listed); the FX difference of a receipt in the
// order's currency is realised by its fund's conversions and unrealised at the settlement rate its application
// recorded (S5R-D19); write-offs are sunk cost and a cancelled order with sunk costs closes settled, marked cancelled;
// one without has no result (S5R-D42). Request members: charge.record
// {charge_id, expected_version, status, source {kind, party_id, reference}, beneficiaries [{kind, order_id,
// shipment_id}], fee_category {kind, id, revision}, currency {kind, id, revision}, conversion {kind, fx_rate},
// amount, occurred_on, analysis_on, basis, evidence_ids}; charge.confirm {charge_id, expected_version,
// evidence_ids, reason}; charge.allocate {charge_id, expected_version, basis, data_basis, evidence_ids, lines
// [{line_id, target {kind, order_id, shipment_id}, weight}]}; order_result.review {order_id, expected_version,
// note}; order_result.close {order_id, expected_version, reason}; order_result.reopen {order_id, expected_version,
// reason, evidence_ids}.
const (
	CommandRecordCharge      = "charge.record"
	CommandConfirmCharge     = "charge.confirm"
	CommandAllocateCharge    = "charge.allocate"
	CommandReviewOrderResult = "order_result.review"
	CommandCloseOrderResult  = "order_result.close"
	CommandReopenOrderResult = "order_result.reopen"
	CommandGetCharge         = "charge.get"
	CommandGetOrderResult    = "order_result.get"
	EventChargeAllocated     = "idp-brant.stl.charge_allocated.v1"
	EventOrderResultClosed   = "idp-brant.stl.order_result_closed.v1"
	EventOrderResultReopened = "idp-brant.stl.order_result_reopened.v1"
	KindFeeCategory          = "fee_category"
	ChargeEstimated          = "estimated"
	ChargePending            = "pending"
	ChargeConfirmed          = "confirmed"
	ChargeSourceSupplierBill = "supplier_bill"
	ChargeSourceLogistics    = "logistics_offer"
	ChargeSourceInternal     = "internal"
	ChargeTargetOrder        = "order"
	ChargeTargetShipment     = "shipment"
	AllocationByAmount       = "amount"
	AllocationByQuantity     = "quantity"
	AllocationByWeight       = "weight"
	ConversionSameCurrency   = "same_currency"
	ConversionOrderAnalysis  = "order_analysis"
	ConversionFXRate         = "fx_rate"
	ResultOpen               = "open"
	ResultReviewed           = "reviewed"
	ResultSettled            = "settled"
	ConditionMet             = "met"
	ConditionUnmet           = "unmet"
	ConditionNotEnabled      = "not_enabled"
	DimensionFulfilment      = "fulfilment"
	DimensionReceivables     = "receivables"
	DimensionPayables        = "payables"
	DimensionPurchaseCosts   = "purchase_costs"
	DimensionDirectCharges   = "direct_charges"
	DimensionClaims          = "claims"
	// Cost components of a charge (S5R-D24): whom the cost is for depends on the order's Incoterm.
	ComponentPreCarriage     = "pre_carriage"
	ComponentExportClearance = "export_clearance"
	ComponentMainCarriage    = "main_carriage"
	ComponentInsurance       = "insurance"
	ComponentOther           = "other"
	// Payment states of a charge, derived from the payments applied to it (S5R-D19).
	ChargeUnpaid       = "unpaid"
	ChargePartlyPaid   = "partly_paid"
	ChargePaid         = "paid"
	MaxBeneficiaries   = 50
	MaxAllocationLines = 200
	maxReference       = 128
	dateLayout         = "2006-01-02"
)

// ChargeSource is where a charge comes from: Kind supplier_bill or logistics_offer of PartyID (PTY) with its
// Reference (a bill or offer number, 1–128 characters), or internal (no party; a reference optional).
type ChargeSource struct {
	Kind      string
	PartyID   string
	Reference string
}

// ChargeTarget is an order {kind: "order", order_id} or a shipment of an order {kind: "shipment", order_id,
// shipment_id} that a charge benefits or an allocation line goes to.
type ChargeTarget struct {
	Kind       string
	OrderID    string
	ShipmentID string
}

// ChargeConversion is how a charge converts into its orders' analysis currencies: same_currency, order_analysis
// (the order's analysis rate; the charge in the order's currency) or fx_rate (FXRate: a POL fx_rate of purpose
// settlement from the charge's currency into the analysis currency).
type ChargeConversion struct {
	Kind   string
	FXRate *ConfigRef
}

// RecordCharge records a new charge (ExpectedVersion 0) or the bill of an estimated one (its version). Human or AI.
// A new charge states its CostComponent (S5R-D24: pre_carriage, export_clearance, main_carriage, insurance or
// other; the main carriage of an order whose Incoterm is EXW, FCA, FAS or FOB is the buyer's: INVALID_KNOWN_VALUE
// unless Recharged) and whether it is Recharged (advanced for the buyer and recovered from it: not in the order
// result); a bill names neither. Both are left out of the request hash while empty.
type RecordCharge struct {
	IdempotencyKey  string
	ChargeID        string
	ExpectedVersion int64
	Status          string
	Source          ChargeSource
	Beneficiaries   []ChargeTarget
	FeeCategory     *ConfigRef
	Currency        *ConfigRef
	Conversion      *ChargeConversion
	CostComponent   string `json:",omitempty"`
	Recharged       bool   `json:",omitempty"`
	Amount          string
	OccurredOn      string
	AnalysisOn      string
	Basis           string
	EvidenceIDs     []string
}

// ConfirmCharge confirms charge ChargeID at ExpectedVersion with its bill (EvidenceIDs, 1 to 20) and an optional
// Reason. Human only.
type ConfirmCharge struct {
	IdempotencyKey  string
	ChargeID        string
	ExpectedVersion int64
	EvidenceIDs     []string
	Reason          string
}

// AllocationLine is one line of charge.allocate: its ID (the stable tie-breaker), target and weight (a decimal
// with at most 6 fractional digits).
type AllocationLine struct {
	LineID string
	Target ChargeTarget
	Weight string
}

// AllocateCharge allocates charge ChargeID at ExpectedVersion by Basis (amount, quantity or weight) over Lines (1
// to 200), the weights resting on DataBasis (1–1000 characters) and EvidenceIDs (0 to 20). Human or AI.
type AllocateCharge struct {
	IdempotencyKey  string
	ChargeID        string
	ExpectedVersion int64
	Basis           string
	DataBasis       string
	EvidenceIDs     []string
	Lines           []AllocationLine
}

// ReviewOrderResult reviews the result of order OrderID at ExpectedVersion (0 when it has none yet) with an
// optional Note. Human or AI.
type ReviewOrderResult struct {
	IdempotencyKey  string
	OrderID         string
	ExpectedVersion int64
	Note            string
}

// CloseOrderResult closes the result of order OrderID at ExpectedVersion with an optional Reason. Human only.
type CloseOrderResult struct {
	IdempotencyKey  string
	OrderID         string
	ExpectedVersion int64
	Reason          string
}

// ReopenOrderResult reopens the settled result of order OrderID at ExpectedVersion for Reason, resting on
// EvidenceIDs (0 to 20). Human only.
type ReopenOrderResult struct {
	IdempotencyKey  string
	OrderID         string
	ExpectedVersion int64
	Reason          string
	EvidenceIDs     []string
}

func (RecordCharge) stlCommand()      {}
func (ConfirmCharge) stlCommand()     {}
func (AllocateCharge) stlCommand()    {}
func (ReviewOrderResult) stlCommand() {}
func (CloseOrderResult) stlCommand()  {}
func (ReopenOrderResult) stlCommand() {}

func versionOrNew(v int64) bool { return v >= 0 && v <= maxVersion }

// Date parses a calendar date of a charge (YYYY-MM-DD, the company's business calendar), at UTC midnight.
func Date(s string) (time.Time, bool) {
	t, err := time.Parse(dateLayout, s)
	return t, err == nil && t.Format(dateLayout) == s
}

// FormatDate renders a calendar date.
func FormatDate(t time.Time) string { return t.Format(dateLayout) }

// signedAmount reports whether s is an amount, a leading "-" allowed (a negative adjustment).
func signedAmount(s string) bool {
	if len(s) > 1 && s[0] == '-' {
		s = s[1:]
	}
	return amount(s)
}

func reference(s string) bool { return Reason(s) && len([]rune(s)) <= maxReference }

func (t ChargeTarget) validate(path string) error {
	switch {
	case t.Kind != ChargeTargetOrder && t.Kind != ChargeTargetShipment:
		return schema(path + "/kind")
	case !kernel.ValidIdentifier(t.OrderID):
		return schema(path + "/order_id")
	case t.Kind == ChargeTargetShipment && !kernel.ValidIdentifier(t.ShipmentID):
		return schema(path + "/shipment_id")
	case t.Kind == ChargeTargetOrder && t.ShipmentID != "":
		return schema(path + "/shipment_id")
	}
	return nil
}

func (s ChargeSource) validate() error {
	switch {
	case s.Kind != ChargeSourceSupplierBill && s.Kind != ChargeSourceLogistics && s.Kind != ChargeSourceInternal:
		return schema("/args/source/kind")
	case s.Kind != ChargeSourceInternal && !kernel.ValidIdentifier(s.PartyID):
		return schema("/args/source/party_id")
	case s.Kind == ChargeSourceInternal && s.PartyID != "":
		return schema("/args/source/party_id")
	case s.Kind != ChargeSourceInternal && !reference(s.Reference):
		return schema("/args/source/reference")
	case s.Reference != "" && !reference(s.Reference):
		return schema("/args/source/reference")
	}
	return nil
}

// Validate checks the closed contract: a new charge (expected_version 0) names its beneficiaries (1 to 50),
// fee category, currency and conversion; a bill (expected_version ≥ 1) is pending and names none of them; a
// pending charge rests on 1 to 20 evidence IDs, an estimate on 0 to 20.
func (c RecordCharge) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ChargeID):
		return schema("/args/charge_id")
	case !versionOrNew(c.ExpectedVersion):
		return schema("/args/expected_version")
	case c.Status != ChargeEstimated && c.Status != ChargePending:
		return schema("/args/status")
	case c.ExpectedVersion > 0 && c.Status != ChargePending:
		return schema("/args/status")
	}
	if err := c.Source.validate(); err != nil {
		return err
	}
	if c.ExpectedVersion > 0 {
		switch {
		case len(c.Beneficiaries) > 0:
			return schema("/args/beneficiaries")
		case c.CostComponent != "" || c.Recharged:
			return schema("/args/cost_component")
		case c.FeeCategory != nil:
			return schema("/args/fee_category")
		case c.Currency != nil:
			return schema("/args/currency")
		case c.Conversion != nil:
			return schema("/args/conversion")
		}
	} else {
		if len(c.Beneficiaries) == 0 || len(c.Beneficiaries) > MaxBeneficiaries {
			return schema("/args/beneficiaries")
		}
		for i, b := range c.Beneficiaries {
			if err := b.validate("/args/beneficiaries/" + strconv.Itoa(i)); err != nil {
				return err
			}
		}
		switch {
		case c.CostComponent != ComponentPreCarriage && c.CostComponent != ComponentExportClearance && c.CostComponent != ComponentMainCarriage &&
			c.CostComponent != ComponentInsurance && c.CostComponent != ComponentOther:
			return schema("/args/cost_component")
		case c.FeeCategory == nil:
			return schema("/args/fee_category")
		case c.Currency == nil:
			return schema("/args/currency")
		case c.Conversion == nil:
			return schema("/args/conversion")
		}
		if err := c.FeeCategory.Validate(KindFeeCategory, "/args/fee_category"); err != nil {
			return err
		}
		if err := c.Currency.Validate(KindCurrency, "/args/currency"); err != nil {
			return err
		}
		switch k := c.Conversion.Kind; {
		case k != ConversionSameCurrency && k != ConversionOrderAnalysis && k != ConversionFXRate:
			return schema("/args/conversion/kind")
		case (k == ConversionFXRate) != (c.Conversion.FXRate != nil):
			return schema("/args/conversion/fx_rate")
		case c.Conversion.FXRate != nil:
			if err := c.Conversion.FXRate.Validate(KindFXRate, "/args/conversion/fx_rate"); err != nil {
				return err
			}
		}
	}
	if !signedAmount(c.Amount) {
		return schema("/args/amount")
	}
	if _, ok := Date(c.OccurredOn); !ok {
		return schema("/args/occurred_on")
	}
	if _, ok := Date(c.AnalysisOn); !ok {
		return schema("/args/analysis_on")
	}
	if !Reason(c.Basis) {
		return schema("/args/basis")
	}
	min := 0
	if c.Status == ChargePending {
		min = 1
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", min)
}

// Validate checks the closed contract.
func (c ConfirmCharge) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ChargeID):
		return schema("/args/charge_id")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case c.Reason != "" && !Reason(c.Reason):
		return schema("/args/reason")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 1)
}

// Validate checks the closed contract. A weight is a decimal with an optional leading "-" (a negative weight is
// the rules' ALLOCATION_INVALID, not a contract error).
func (c AllocateCharge) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ChargeID):
		return schema("/args/charge_id")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case c.Basis != AllocationByAmount && c.Basis != AllocationByQuantity && c.Basis != AllocationByWeight:
		return schema("/args/basis")
	case !Reason(c.DataBasis):
		return schema("/args/data_basis")
	case len(c.Lines) > MaxAllocationLines:
		return schema("/args/lines")
	}
	if err := evidence(c.EvidenceIDs, "/args/evidence_ids", 0); err != nil {
		return err
	}
	for i, l := range c.Lines {
		p := "/args/lines/" + strconv.Itoa(i)
		if !kernel.ValidIdentifier(l.LineID) {
			return schema(p + "/line_id")
		}
		if err := l.Target.validate(p + "/target"); err != nil {
			return err
		}
		if l.Weight == "" || len(l.Weight) > 40 {
			return schema(p + "/weight")
		}
	}
	return nil
}

// Validate checks the closed contract.
func (c ReviewOrderResult) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case !versionOrNew(c.ExpectedVersion):
		return schema("/args/expected_version")
	case c.Note != "" && !Reason(c.Note):
		return schema("/args/note")
	}
	return nil
}

// Validate checks the closed contract.
func (c CloseOrderResult) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case c.Reason != "" && !Reason(c.Reason):
		return schema("/args/reason")
	}
	return nil
}

// Validate checks the closed contract.
func (c ReopenOrderResult) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 0)
}

// AllocationLineView is a line of an allocation: target, weight and share in minor units of the charge's currency.
type AllocationLineView struct {
	LineID      string
	Target      ChargeTarget
	Weight      string
	AmountMinor string
}

// ChargeReceipt is the answer of charge.record, charge.confirm and charge.allocate: the receipt (ObjectID the
// charge, RowVersion its version, Status estimated, pending or confirmed), its currency, amount and estimate, and
// the allocation that counts (Seq 0: none) with its lines.
type ChargeReceipt struct {
	Receipt
	Currency      string
	AmountMinor   string
	EstimateMinor string
	AllocationSeq int
	Lines         []AllocationLineView
}

// CloseConditionView is one close condition as it stands: its dimension, status (met, unmet or not_enabled) and items.
type CloseConditionView struct {
	Dimension string
	Status    string
	Items     []string
}

// OrderResultReceipt is the answer of order_result.review, close and reopen: the receipt (ObjectID the order,
// RowVersion the result's version, Status open, reviewed or settled), the result currency, the confirmed result
// now, the newest snapshot's number and the close conditions as the command judged them.
type OrderResultReceipt struct {
	Receipt
	Currency             string
	ConfirmedResultMinor string
	Margin               string
	SnapshotSeq          int
	Conditions           []CloseConditionView
}

// ChargeConversionView is a charge's conversion basis: kind, and for fx_rate the POL revision, its hash, the rate
// and the currency it converts into.
type ChargeConversionView struct {
	Kind       string
	FXRate     *ConfigRef
	FXRateHash string
	Rate       string
	To         string
}

// ChargeAllocationView is an allocation of a charge: the charge version it moved the charge to, basis, data basis,
// evidence, amount, whether it is the one that counts, its lines and who allocated it.
type ChargeAllocationView struct {
	Seq           int
	ChargeVersion int64
	Basis         string
	DataBasis     string
	EvidenceIDs   []string
	AmountMinor   string
	Current       bool
	Lines         []AllocationLineView
	AllocatedBy   kernel.ActorRef
	AllocatedAt   time.Time
}

// ChargeView is a charge as charge.get shows it, as of CheckedAt.
type ChargeView struct {
	ChargeID          string
	Status            string
	Version           int64
	Source            ChargeSource
	EstimateReference string
	FeeCategory       ConfigRef
	Currency          string
	CurrencyRevision  int64
	EstimateMinor     string
	AmountMinor       string
	Conversion        ChargeConversionView
	CostComponent     string // S5R-D24
	Recharged         bool   // S5R-D24
	OccurredOn        string
	AnalysisOn        string
	Basis             string
	EvidenceIDs       []string
	Beneficiaries     []ChargeTarget
	// S5R-D19: what the payments applied to the charge paid, what is open, its payment state (unpaid, partly_paid,
	// paid) and the payments.
	PaidMinor          string
	OpenMinor          string
	PaymentStatus      string
	Payments           []AllocationView
	AllocationSeq      int
	Allocations        []ChargeAllocationView
	RecordedBy         kernel.ActorRef
	RecordedAt         time.Time
	BilledBy           kernel.ActorRef
	BilledAt           time.Time
	ConfirmedBy        kernel.ActorRef
	ConfirmedAt        time.Time
	ConfirmEvidenceIDs []string
	ConfirmReason      string
	CheckedAt          time.Time
}

// ResultSourceView is a record a figure counts.
type ResultSourceView struct {
	Kind    string
	ID      string
	Version int64
	Detail  string
}

// FigureView is one view of an order's result in minor units of the result currency (the order's analysis
// currency): net sales, credits (confirmed result only), purchase cost (forecast: expected; executing: confirmed
// plus the estimated rest; confirmed: the cost of the goods sold), direct charges (confirmed and estimated), the
// result, the margin in percent (result ÷ net sales less credits, HALF_UP to 2 places; "" when nothing is sold),
// its sources and what it could not count.
type FigureView struct {
	NetSalesMinor          string
	CreditsMinor           string
	PurchaseCostMinor      string
	PurchaseConfirmedMinor string
	PurchaseEstimatedMinor string
	DirectChargesMinor     string
	ChargesConfirmedMinor  string
	ChargesEstimatedMinor  string
	ResultMinor            string
	Margin                 string
	Sources                []ResultSourceView
	Gaps                   []string
}

// BatchView is the confirmed result of one settled shipment.
type BatchView struct {
	ShipmentID         string
	StatementIDs       []string
	NetSalesMinor      string
	CreditsMinor       string
	CostOfSalesMinor   string
	DirectChargesMinor string
	ResultMinor        string
}

// ConfirmedResultView is the confirmed result with its batches, the order's own direct charges and the confirmed
// purchase cost of goods not sold yet.
type ConfirmedResultView struct {
	FigureView
	Batches                 []BatchView
	OrderLevelChargesMinor  string
	UnsoldPurchaseCostMinor string
	WriteOffCostMinor       string // S5R-D42: the cost of written-off goods, part of the purchase cost (sunk cost)
}

// FXItemView is one cross-currency receipt of the order with its business settlement FX difference.
//
// S5R-D19: Basis says how it is valued — settlement_rate (a receipt in another currency converted at a POL settlement
// rate when applied, C8), conversion (the part of a receipt in the order's currency its fx_conversion records
// realised, at their bank rates weighted by the amounts sold: Rate, ConversionIDs) or unrealized (the part not
// converted yet, at the settlement rate recorded when it was applied). Source is that part in the receipt's currency,
// Target its value.
type FXItemView struct {
	ApplicationID     string
	TargetKind        string
	StatementID       string
	StageID           string
	SourceCurrency    string
	SourceAmountMinor string
	TargetCurrency    string
	TargetAmountMinor string
	DifferenceMinor   string
	Basis             string
	FundRecordID      string
	Rate              string
	ConversionIDs     []string
	Unrealized        bool
}

// FXResultView is the business settlement FX difference (never a receipt or revenue).
type FXResultView struct {
	DifferenceMinor string
	RealizedMinor   string // S5R-D19
	UnrealizedMinor string // S5R-D19
	Items           []FXItemView
	Gaps            []string
}

// CashView is the net cash result: receipts less payments, its sources and scope.
//
// S5R-D19: payments include what was paid on the order's charges (ChargePaymentsMinor, the order's share);
// UnpaidChargesMinor and Unpaid are the order's part of its charges not paid through STL.
type CashView struct {
	ReceiptsMinor       string
	PaymentsMinor       string
	ChargePaymentsMinor string
	NetMinor            string
	UnpaidChargesMinor  string
	Sources             []ResultSourceView
	Unpaid              []ResultSourceView
	Gaps                []string
}

// ResultFiguresView are an order's result figures as of AsOf in Currency (MinorUnits), converted from the order's
// currency at AnalysisRate.
type ResultFiguresView struct {
	AsOf          time.Time
	Currency      string
	MinorUnits    int
	OrderCurrency string
	AnalysisRate  string
	Forecast      FigureView
	Executing     FigureView
	Confirmed     ConfirmedResultView
	FXDifference  FXResultView
	NetCash       CashView
	// S5R-D42: the order is cancelled (no revenue: the result is its sunk cost) and has sunk costs at all; S5R-D24:
	// the recharged charges the result leaves out.
	Cancelled bool
	SunkCost  bool
	Recharged []ResultSourceView
}

// SnapshotContent is what a closing snapshot holds: the order binding, the figures and the close conditions.
type SnapshotContent struct {
	OrderID       string
	OrderRevision int64
	OrderHash     string
	Figures       ResultFiguresView
	Conditions    []CloseConditionView
}

// SnapshotView is a closing snapshot: its number, content and the SHA-256 of the content's canonical JSON, the
// close's reason, who closed it and when, and whether a reopening followed.
type SnapshotView struct {
	Seq int
	SnapshotContent
	ContentSHA256 string
	Reason        string
	ClosedBy      kernel.ActorRef
	ClosedAt      time.Time
	Reopened      bool
}

// ReviewView is a review of the result: the order revision it saw, the SHA-256 of its summary, its note.
type ReviewView struct {
	Seq           int
	OrderRevision int64
	SummarySHA256 string
	Note          string
	ReviewedBy    kernel.ActorRef
	ReviewedAt    time.Time
}

// ReopeningView is a reopening of snapshot Seq: its reason and evidence.
type ReopeningView struct {
	Seq         int
	Reason      string
	EvidenceIDs []string
	ReopenedBy  kernel.ActorRef
	ReopenedAt  time.Time
}

// OrderResultView is an order's result as order_result.get shows it: the result's state and version (version 0,
// open, when no command touched it yet), the order as TRD states it, the figures and close conditions derived
// now (Live, Conditions; Settleable when none is unmet), every closing snapshot as it was settled, the reviews and
// the reopenings.
type OrderResultView struct {
	OrderID       string
	Status        string
	Version       int64
	OrderRevision int64
	OrderStatus   string
	OrderEnded    string
	Cancelled     bool // S5R-D42: the result of a cancelled order (closes settled, marked cancelled)
	Live          ResultFiguresView
	Conditions    []CloseConditionView
	Settleable    bool
	Snapshots     []SnapshotView
	Reviews       []ReviewView
	Reopenings    []ReopeningView
	CheckedAt     time.Time
}

// ResultCommands is STL's command entry point for charges and order results; errors as for Commands.
type ResultCommands interface {
	RecordCharge(ctx context.Context, cmd RecordCharge) (ChargeReceipt, error)
	ConfirmCharge(ctx context.Context, cmd ConfirmCharge) (ChargeReceipt, error)
	AllocateCharge(ctx context.Context, cmd AllocateCharge) (ChargeReceipt, error)
	ReviewOrderResult(ctx context.Context, cmd ReviewOrderResult) (OrderResultReceipt, error)
	CloseOrderResult(ctx context.Context, cmd CloseOrderResult) (OrderResultReceipt, error)
	ReopenOrderResult(ctx context.Context, cmd ReopenOrderResult) (OrderResultReceipt, error)
}

// ResultQueries is the audited read side of charges and order results (stl.read). A charge outside the caller's
// data scope, or the result of an order TRD does not show the caller, is NOT_FOUND.
type ResultQueries interface {
	GetCharge(ctx context.Context, id string) (ChargeView, error)
	GetOrderResult(ctx context.Context, orderID string) (OrderResultView, error)
}
