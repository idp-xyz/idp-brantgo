package api

import (
	"context"
)

// OrderCommercialSnapshot v1 (Context Map R32 TRD→STL: 订单商业快照与付款计划（不生成应收）; drafts and
// active orders, payment stages, amounts and the credit portion), SalesOrderRef v1 with
// SalesContractDocumentRef v1 (R46 TRD→CNX: the order revision, lines, parties and addresses; the
// export sales contract and proforma invoice) and the terms ActiveOrderRef v1 carries to FUL (R27:
// trade terms, delivery requirement, address roles and payment stages) are one lookup,
// OrderSnapshotLookups: what TRD says of an order at its current revision, whatever its state.
// STL (credit.reserve, the order account), CNX (trade compliance, declarations) and FUL ask it
// before their own transactions (S5 plan, unit A2); Status says what the order is now, and the
// caller decides whether that serves its step. Inside its transaction a caller that needs an
// active order re-checks it with OrderGuard v1; the snapshot itself is never locked.
//
// Every fact is one TRD sealed, never an upstream's current revision. The order's current revision
// — revision 1 is the commercial snapshot (OrderSnapshotContract), a later one the
// brant.order-revision/0.1.0 document an applied amendment produced, its amounts recalculated by
// the quote calculation's rules (S3-D124) — gives the lines, charges, totals, the trade terms and
// payment terms references, the named place, the payment plan and the delivery. The source quote
// revision's sealed commercial document gives the customer's addresses with the roles and text PTY's
// confirmed revision gave them (no amendment changes the parties) and the analysis rate of its
// calculation. The order's S/C and PI bindings give the document references. The Incoterm and its
// edition are what the POL trade_terms revision the order binds states (a published revision never
// changes), read whatever that revision's state is now.
//
// A payment plan is a plan, never a receivable, a receipt or a bank credit (R32's risk); the credit
// portion is TRD-INV-022's: the stages due after_shipment or after_invoice without the documents
// release control (D-5).
//
// Like ActiveOrderLookups the lookup needs no permission beyond an active grant (S3-D153); orders
// of quotes outside the caller's data scope are absent, as are unknown ones.
type OrderSnapshotLookups interface {
	// ResolveOrderSnapshots answers at most MaxOrderRefs order IDs (ValidateOrderIDs).
	ResolveOrderSnapshots(ctx context.Context, orderIDs []string) (map[string]OrderSnapshotFact, error)
}

// OrderSnapshotQueries is the audited read of the same fact.
type OrderSnapshotQueries interface {
	// GetOrderSnapshot returns what OrderSnapshotLookups says of order orderID, audited as
	// order.snapshot.get (trd.read): NOT_FOUND for an order of a quote the caller does not see; an
	// order whose sealed revision is not intact is REFERENCE_INVALID.
	GetOrderSnapshot(ctx context.Context, orderID string) (OrderSnapshotFact, error)
}

// The triggers of a payment stage, as POL's payment_terms states them and the payment plan copies
// them, and the release control a stage may carry (D-5, F-04).
const (
	TriggerOrderConfirmed = "order_confirmed"
	TriggerBeforeShipment = "before_shipment"
	TriggerAfterShipment  = "after_shipment"
	TriggerAfterInvoice   = "after_invoice"

	// ReleaseControlDocuments is a stage FUL's release gate secures: the shipping documents stay
	// with us until it is paid, so it needs no credit reservation and PaymentConditionGuard v1
	// requires it for the documents scope.
	ReleaseControlDocuments = "documents"
)

// OrderSnapshotFact is what TRD says of an order now (OrderCommercialSnapshot v1, SalesOrderRef v1,
// SalesContractDocumentRef v1): its binding at its current revision (OrderBinding), state and row
// version, the quote revision and acceptance it came from, the customer and currency, the terms
// (OrderTerms, embedded: TradeTerms, Addresses, PaymentTerms, Stages, CreditPortionMinor), the
// delivery requirement, the lines, charges and totals in minor units of the order currency, the
// S/C and PI of every revision, the analysis rate, and the revisions its open amendments propose
// (Proposed). Ended names the command that ended the order
// (order.cancel or order.close_unfulfilled of a draft; order.cancel, order.close_remainder — a short
// close — or order.close of an active order; "" while it is a draft or active). Code is "" when the
// order's sealed revision is intact, REFERENCE_INVALID otherwise — then only Binding, Status,
// RowVersion, Ended and the source are set and nothing else may be relied on.
type OrderSnapshotFact struct {
	Binding          OrderBinding
	Status           string // OrderDraft, OrderActive, OrderCancelled, OrderClosedUnfulfilled or OrderClosed (SM-TRD-ORDER)
	Ended            string
	RowVersion       int64
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
	AcceptanceID     string
	CustomerRef      PartyRef
	CurrencyRef      ConfigRef
	OrderTerms
	Delivery     Delivery
	Lines        []OrderLine
	Charges      []OrderCharge
	NetMinor     string
	TaxMinor     string
	PayableMinor string
	DocumentRefs []OrderDocumentRef // every S/C and PI of the order, oldest revision first
	AnalysisFX   AnalysisFX
	Proposed     []ProposedRevision // what the open amendments of an active order propose; none for a draft or an ended order
	Code         string
}

// ProposedRevision is the revision an amendment of an active order proposes (S5 decision D-9: STL
// reserves credit for it before order.amendment.apply, which calls CreditGuard v1 for it when it
// asks more credit than the current revision). TRD states one for every amendment pending
// approval or approved that binds the order's current revision — a draft's content still changes,
// and an amendment of an older revision can never be applied. Binding is the order at the revision
// number and order hash order.amendment.apply would produce from the amendment (AmendmentID,
// AmendmentHash, AmendmentStatus) on the current revision with what POL says now and no shipment
// confirmed; Stages, CreditPortionMinor and the totals are that revision's, in minor units of the
// order currency (which no amendment changes). The derivation is the one apply repeats: while the
// facts it reads stay the same the binding is the one apply produces, and a reservation for it is
// what apply's guard finds. Code is "" when TRD derives the revision now and otherwise the reason
// it cannot (then only the amendment's fields are set): such an amendment cannot be applied now
// either.
type ProposedRevision struct {
	AmendmentID        string
	AmendmentHash      string
	AmendmentStatus    string // pending_approval or approved
	Binding            OrderBinding
	Stages             []PaymentStage
	CreditPortionMinor string
	NetMinor           string
	TaxMinor           string
	PayableMinor       string
	Code               string
}

// ProposedAt returns the proposed revision with binding b, if TRD states one.
func (f OrderSnapshotFact) ProposedAt(b OrderBinding) (ProposedRevision, bool) {
	for _, p := range f.Proposed {
		if p.Code == "" && p.Binding == b {
			return p, true
		}
	}
	return ProposedRevision{}, false
}

// Check returns nil when the order's sealed revision is intact and REFERENCE_INVALID at path
// otherwise. It says nothing of the order's state: the caller decides on Status.
func (f OrderSnapshotFact) Check(path string) error {
	if f.Code == "" {
		return nil
	}
	return refuse(f.Code, path)
}

// Address returns the customer's addresses of role, in the order the revision binds them.
func (t OrderTerms) Address(role string) []OrderAddress {
	var out []OrderAddress
	for _, a := range t.Addresses {
		if a.Role == role {
			out = append(out, a)
		}
	}
	return out
}

// OrderTerms are the terms of an order revision downstream contexts act on (the Terms block of
// ActiveOrderRef v1, R27, and of OrderCommercialSnapshot v1, R32): the trade terms, the customer's
// addresses with their roles, the POL payment_terms revision and the payment plan it produced, the
// credit portion of that plan (TRD-INV-022) in minor units of the order currency, and the notify
// party the revision binds (S5R-D40; nil when none): its address of the role notify, as sealed.
type OrderTerms struct {
	TradeTerms         TradeTerms
	Addresses          []OrderAddress
	PaymentTerms       ConfigRef
	Stages             []PaymentStage
	CreditPortionMinor string
	NotifyParty        *OrderAddress
}

// TradeTerms are the order's trade terms: the POL trade_terms revision it binds (Ref) with the
// Incoterm (EXW, FCA, CPT, CIP, DAP, DPU, DDP, FAS, FOB, CFR or CIF) and edition that revision
// states — sealed with the order revision since S5R-D35, read from POL's revision for one written
// before — and the named place the order binds (a BusinessRef of kind location, such as the port
// of loading under FOB or FCA).
type TradeTerms struct {
	Ref        ConfigRef
	Incoterm   string
	Edition    string
	NamedPlace BusinessRef
}

// OrderAddress is an address the order binds: a customer address with its PTY address role (buyer,
// seller, payer, payee, consignee, shipper or billing) at the confirmed revision the quote bound,
// or the notify party (role notify, any party; S5R-D40) — AddressRoleRef v1: PartyID,
// PartyRevision, AddressID — with the address text as sealed and its country (ISO 3166-1 alpha-2;
// S5R-D35): what the revision sealed, "" when PTY's revision states none or, in ActiveOrderRef v1
// and OrderCommercialSnapshot v1, when the revision was written before S5R (OrderRevisionRef v1
// derives it then from the bound party revision, which never changes).
type OrderAddress struct {
	Role          string
	PartyID       string
	PartyRevision int64
	AddressID     string
	Address       string
	Country       string
}

// PaymentStage is one stage of the order revision's payment plan: its ID, trigger (order_confirmed,
// before_shipment, after_shipment or after_invoice) and days, the day basis and POL calendar when
// the days are working days, its percentage of the payable (0–100) and amount in minor units of the
// order currency, and its release control ("" or ReleaseControlDocuments). OnCredit says whether it
// is part of the credit portion.
type PaymentStage struct {
	StageID        string
	Trigger        string
	Days           int
	DayBasis       string
	CalendarRef    *ConfigRef
	Percent        string
	AmountMinor    string
	ReleaseControl string
	OnCredit       bool
}

// OrderCharge is a sales charge of the order revision: its ID, POL fee category and obligation,
// its amount as stated (PriceFormat, order currency), whether it is charged separately or included
// in a line, and its amounts in minor units (an included charge's are zero: its line's price holds
// it).
type OrderCharge struct {
	ChargeID       string
	FeeCategoryRef ConfigRef
	ObligationKey  string
	Amount         string
	Inclusion      string
	IncludedLineID string
	NetMinor       string
	TaxMinor       string
	PayableMinor   string
}

// OrderDocumentRef is an S/C or PI of the order (SalesContractDocumentRef v1): its kind
// (DocumentSalesContract or DocumentProformaInvoice), number, the order revision and hash it binds,
// the SHA-256 of its generated bytes, whether it binds the order's current revision, and its signed
// copies.
type OrderDocumentRef struct {
	DocumentKind   string
	Number         string
	OrderRevision  int64
	OrderHash      string
	DocumentSHA256 string
	Current        bool
	Signatures     []SignedCopyRef
}

// SignedCopyRef is a signed copy of an order document: SignatureSigned (ours) or
// SignatureCountersigned (the customer's), the EVD business file and its SHA-256.
type SignedCopyRef struct {
	SignatureKind string
	FileID        string
	FileSHA256    string
}

// AnalysisFX is the order's analysis rate: what the calculation the source quote revision sealed
// compared the order's sales in — the order currency's code (OrderCurrency), the pricing policy's
// calculation currency at its POL revision (ComparisonCurrencyRef; its ID is the code), and, when
// the two differ, the POL fx_rate revision it used and the rate (one unit of the order currency in
// the comparison currency, an exact decimal string). The same currency names no rate: FXRateRef is
// nil and Rate "" (a factor of exactly 1). No amendment changes it.
type AnalysisFX struct {
	OrderCurrency         string
	ComparisonCurrencyRef ConfigRef
	FXRateRef             *ConfigRef
	Rate                  string
}
