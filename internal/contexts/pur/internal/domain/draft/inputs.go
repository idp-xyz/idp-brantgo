package draft

import (
	"cmp"
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Kinds of supply condition a draft line names (SUP's SupplyConditionRef v1 kinds).
const (
	KindSupplierOffer = "supplier_offer"
	KindPurchaseTerms = "purchase_terms"

	ValidityDependent = "dependent_validity"

	DeliveryFixed = "fixed"

	MOQScopeSKU           = "sku"
	MOQScopeExplicitGroup = "explicit_group"

	PartyConfirmed = "confirmed"
)

// Ref is a sealed supply condition revision.
type Ref struct {
	Kind     string
	ID       string
	Revision int64
}

func compareRef(a, b Ref) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Revision, b.Revision))
}

// Source is the condition line a draft line buys under: a line of a sealed supplier offer or
// purchase terms revision.
type Source struct {
	Ref
	ConditionLineID string
}

func compareSource(a, b Source) int {
	return cmp.Or(compareRef(a.Ref, b.Ref), cmp.Compare(a.ConditionLineID, b.ConditionLineID))
}

// Overbuy is what a draft line buys beyond its allocation, explicitly marked with a reason
// (PUR-INV-005: MOQ, order multiple or loss).
type Overbuy struct {
	Quantity kernel.Decimal
	Reason   string
}

// RequestLine is one line of a purchase.draft.prepare request: Quantity of order line OrderLineID
// allocated to the draft, bought under Source, plus an optional overbuy.
type RequestLine struct {
	OrderLineID string
	Quantity    kernel.Decimal
	Source      Source
	Overbuy     *Overbuy
}

// Request is a purchase.draft.prepare request, its structure already validated.
type Request struct {
	DraftID         string
	OrderID         string
	Lines           []RequestLine
	SelectionReason string
}

// ConfigKey is a POL configuration revision.
type ConfigKey struct {
	Kind     string
	ID       string
	Revision int64
}

// OrderLine is an active order's line as ActiveOrderRef v1 shows it: the RFQ line it answers, the
// SKU specification revision, the quantity and its unit.
type OrderLine struct {
	LineID       string
	RfqLineID    string
	SkuID        string
	SpecRevision int64
	Quantity     kernel.Decimal
	Unit         ConfigKey
}

// RequiredDelivery is the customer's delivery requirement the order binds: a milestone at a place
// (the location's identity) in a time zone, on a fixed date or after a lead time.
type RequiredDelivery struct {
	Kind      string
	Date      string
	Milestone string
	Place     string
	TimeZone  string
}

// RfqRef is the order's source RFQ at the row version its quote bound.
type RfqRef struct {
	RfqID      string
	RowVersion int64
}

// SnapshotRef is the cost snapshot the order's quote adopted (CostSnapshotRef v1).
type SnapshotRef struct {
	SnapshotID string
	ResultHash string
}

// Order is what TRD says of an order now (ActiveOrderRef v1). Code is the first reason it does not
// serve new purchase work ("" when it does): ORDER_NOT_ACTIVE, ORDER_APPROVAL_REQUIRED,
// ORDER_APPROVAL_REVOKED or REFERENCE_STALE.
type Order struct {
	OrderID       string
	OrderRevision int64
	OrderHash     string
	Status        string
	Code          string
	QuoteID       string
	QuoteRevision int64
	QuoteHash     string
	ReviewID      string
	CustomerID    string
	Rfq           *RfqRef
	Snapshot      *SnapshotRef
	Delivery      *RequiredDelivery
	Lines         map[string]OrderLine
}

// Check returns nil when the order serves new purchase work and TRD's verdict at path otherwise.
func (o Order) Check(path string) error {
	switch o.Code {
	case "":
		return nil
	case CodeOrderNotActive, CodeOrderApprovalRequired, CodeOrderApprovalRevoked, CodeReferenceStale:
		return fail(o.Code, path, "")
	}
	return fail(kernel.CodeReferenceInvalid, path, "")
}

// Tier is one price tier of a condition line as SUP's content states it.
type Tier struct {
	Lower           string
	Upper           string
	UnitPrice       string
	ZeroPriceReason string
}

// MOQ is a condition line's minimum order quantity.
type MOQ struct {
	Quantity string
	Unit     *ConfigKey
	Scope    string
	GroupID  string
	Members  []string // RFQ line IDs of an explicit group
}

// SupplyDelivery is a supplier's delivery commitment: a milestone at a place (UN/LOCODE) in a time
// zone, on a fixed date or after a lead time.
type SupplyDelivery struct {
	Kind      string
	Date      string
	Milestone string
	Place     string
	TimeZone  string
}

// Condition is one line of a sealed supply condition revision.
type Condition struct {
	ID                 string
	SkuID              string
	SpecRevision       int64
	SupplierItemCode   string
	PurchaseUnit       *ConfigKey
	PriceUnit          *ConfigKey
	TierUnit           *ConfigKey
	PriceBasisQuantity string
	PriceTierScope     string
	PricingMode        string
	Tiers              []Tier
	MOQ                *MOQ
	OrderMultiple      string
	Currency           *ConfigKey
	TaxProfile         *ConfigKey
	TaxApplication     *ConfigKey
	TradeTerms         *ConfigKey
	PaymentTerms       *ConfigKey
	Delivery           *SupplyDelivery
}

// Origin is the source offer revision of purchase terms, with their validity mode and the offer's
// validity.
type Origin struct {
	Ref          Ref
	ValidityMode string
	ValidFrom    time.Time
	ValidUntil   time.Time
}

// Supply is what SUP says of a sealed supply condition revision now (SupplyConditionRef v1): its
// verdict (Check, nil when in force), content hash, supplier as bound, RFQ and lines answered (an
// offer's; nil for purchase terms), validity, verification and lines.
type Supply struct {
	Ref                Ref
	Check              func(path string) error
	ContentHash        string
	SupplierID         string
	SupplierRowVersion int64
	Rfq                *RfqRef
	RfqLineIDs         []string
	ValidFrom          time.Time
	ValidUntil         time.Time
	VerifiedBy         kernel.ActorRef
	VerifiedAt         time.Time
	Origin             *Origin
	Lines              map[string]Condition
}

// Party is what PTY says of a party now (PartyRef v1); absent when the caller does not see it.
type Party struct {
	Status     string
	RowVersion int64
	Supplier   bool // the party has the supplier role
}

// Spec is a CAT SKU specification revision.
type Spec struct {
	SkuID    string
	Revision int64
}

// Currency, Unit and TaxProfile are the meanings of the POL configurations the rules use.
type (
	Currency struct {
		Code       string
		MinorUnits int
	}
	Unit struct {
		Code      string
		Precision int
	}
	TaxProfile struct {
		Rate       string
		PriceBasis string
		Recovery   string
	}
)

// Config is what POL says of a configuration revision now: its verdict (Check, nil when in force),
// content hash and, for the kinds the rules read, its meaning.
type Config struct {
	Check       func(path string) error
	ContentHash string
	Currency    *Currency
	Unit        *Unit
	TaxProfile  *TaxProfile
}

// CostedLine is what a cost snapshot says of one RFQ line: the condition lines it was bought under
// and its purchase cost in the snapshot's currency (the purchase components' allocations to it).
type CostedLine struct {
	Sources       []Source
	PurchaseMinor *kernel.Decimal // integer minor units; nil when the snapshot allocates none to the line
}

// Costing is the cost snapshot the order's quote adopted, as CST resolves it (CostSnapshotRef v1).
type Costing struct {
	Ref      SnapshotRef
	Currency string
	Lines    map[string]CostedLine // by RFQ line ID
}

// Inputs are the facts a preparation is judged on, each read from its owner before the transaction
// (lookups) and re-checked inside it by the owners' guards; Allocations are the order lines'
// DemandAllocation roots, locked FOR UPDATE inside it (new roots open there).
type Inputs struct {
	Scope       kernel.Scope
	Now         time.Time
	Preparer    kernel.ActorRef
	Epoch       int64
	Order       *Order
	Supply      map[Ref]Supply
	Parties     map[string]Party
	Specs       map[Spec]func(path string) error
	Configs     map[ConfigKey]Config
	Costing     *Costing
	Allocations map[allocation.Key]*allocation.Allocation
}

func index(path string, i int) string { return fmt.Sprintf("%s/%d", path, i) }
