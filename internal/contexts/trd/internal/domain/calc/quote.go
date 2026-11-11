package calc

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Ref names one exact revision of a configuration or business object, its kind, identifier and
// revision, as the application layer resolved it (a POL ConfigRef, a place or a delivery anchor).
// The rules compare references, never names or labels: two payment terms with the same schedule
// are different terms.
type Ref struct {
	Kind     string
	ID       string
	Revision int64
}

// IsZero reports whether r names nothing.
func (r Ref) IsZero() bool { return r == Ref{} }

// RfqRef is a customer RFQ at the row version a quote or a costing binds.
type RfqRef struct {
	ID         string
	RowVersion int64
}

// Delivery is the customer's delivery requirement as a costing context compared candidates on it
// (CST Delivery) and as a quote states it: a milestone at a place in an IANA time zone, on a fixed
// date or Days after the event Anchor names, counted on DayBasis in a POL calendar. Two deliveries
// are the same only when every field is.
type Delivery struct {
	Kind      string // fixed or lead_time
	Date      string // fixed: YYYY-MM-DD
	Anchor    Ref    // lead_time
	Days      int    // lead_time
	DayBasis  string // lead_time
	Calendar  Ref    // lead_time
	Milestone string
	Place     Ref
	TimeZone  string
}

// Unit is a POL unit of measure: its code and the number of fractional digits a quantity of it
// may have (0–6).
type Unit struct {
	Code      string
	Precision int
}

// Conversion is a POL unit_conversion revision: one From is Factor To, for one SKU at one CAT
// specification revision. A packaging relation is never guessed and never applies to another SKU.
type Conversion struct {
	Ref          Ref
	SkuID        string
	SpecRevision int64
	From, To     string // unit codes
	Factor       kernel.Decimal
}

// Line is one quote line (T03 §3): the RFQ line it quotes, the SKU and specification revision, the
// quantity in the sales unit with the conversion into the cost unit when they differ, the price and
// the tax application. BaseQuantity is the quantity in the cost unit when the author states it
// (BC-AT04); the calculation derives it either way and refuses a stated one that differs. Tax is nil
// when the line names no tax application.
type Line struct {
	Path               string
	LineID             string
	RfqLineID          string
	SkuID              string
	SpecRevision       int64
	Quantity           kernel.Decimal
	Unit               Unit
	BaseQuantity       *kernel.Decimal
	Conversion         *Conversion
	UnitPrice          kernel.Decimal
	PriceBasisQuantity kernel.Decimal
	Discount           kernel.Decimal // discount_ratio on the 0–1 scale, below 1
	Tax                *TaxApplication
}

// Charge is one additional sales charge (UL TRD-SalesCharge): a fixed amount the exporter earns
// from the customer, as one obligation of a sales fee category, charged separately or included in
// one line's price. Fee and Tax are nil when the charge names none. Evidence is what the amount
// rests on; the application layer checks that each source is current and verified.
type Charge struct {
	Path           string
	ChargeID       string
	ObligationKey  string
	Fee            *FeeCategory
	Amount         kernel.Decimal
	Inclusion      string // separate or included_in_line
	IncludedLineID string // included_in_line only
	Evidence       []Evidence
	Tax            *TaxApplication
}

// Evidence names a source and where in it a fact is.
type Evidence struct {
	SourceID string
	Locator  string
}

// Quote is what a quote revision states that the calculation reads, every reference resolved by the
// application layer to the revision it names. Customer and Contact are party IDs; Currency is the
// quote currency, in which the customer pays.
type Quote struct {
	Path         string // the quote data, such as "/args/data"
	Rfq          RfqRef
	Customer     string
	Contact      string
	Currency     kernel.Currency
	TradeTerms   Ref
	PaymentTerms Ref
	NamedPlace   Ref
	Delivery     Delivery
	Lines        []Line
	Charges      []Charge
}

// Facts are what the upstream contexts answered for a quote at the calculation instant At, as the
// application layer resolved them before its transaction and re-checks inside it with the guards:
// the RFQ (INQ), the customer's trading profile terms (PTY), the adopted cost snapshot (CST), the
// pricing policy, the quotation exchange rate the quote names and its payment stages (POL).
type Facts struct {
	At      time.Time
	Rfq     Rfq
	Profile Profile
	Cost    Cost
	Policy  Policy
	FX      *FXRate // nil when the quote names no exchange rate
	Stages  []Stage // the stages of Quote.PaymentTerms
}

// Rfq is the RFQ at Quote.Rfq as INQ answers it: its customer, contact and complete line set.
// Whether that row version is current and ready is INQ's verdict, which the caller checks.
type Rfq struct {
	Customer string
	Contact  string
	Lines    []RfqLine
}

// RfqLine is one RFQ line: the SKU and the quantity the customer asks for, in unit UOM.
type RfqLine struct {
	LineID   string
	SkuID    string
	Quantity kernel.Decimal
	UOM      string // unit code
}

// Profile is what the customer's trading profile revision (PTY) fixes for new business with it.
type Profile struct {
	TradeTerms   Ref
	PaymentTerms Ref
}

// Cost is the adopted cost snapshot (CostSnapshotRef v1) with the costing context it froze: the
// context kind, the RFQ binding, the pricing policy, the comparison basis (cost basis, sales trade
// and payment terms, named place, delivery), the context's lines and the result's total in the
// calculation currency. ExcludedCostCategories are what its cost basis does not estimate.
type Cost struct {
	ContextKind            string
	CostBasis              string
	Rfq                    RfqRef
	PricingPolicy          Ref
	TradeTerms             Ref
	PaymentTerms           Ref
	NamedPlace             Ref
	Delivery               Delivery
	Lines                  []CostLine
	Total                  kernel.Money
	ExcludedCostCategories []string
}

// CostLine is one line of the costing context: SKU, specification revision and quantity in the cost
// unit.
type CostLine struct {
	LineID       string
	SkuID        string
	SpecRevision int64
	Quantity     kernel.Decimal
	Unit         Unit
}

// Policy is the pricing policy revision the quote names: the calculation currency costs and net
// sales are compared in, the minimum margin (0–1) and the maximum age of an exchange rate's
// observation.
type Policy struct {
	Ref                 Ref
	CalculationCurrency kernel.Currency
	MinimumMargin       kernel.Decimal
	MaxFXAge            time.Duration
}
