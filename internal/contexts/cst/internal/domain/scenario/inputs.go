package scenario

import (
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/costing"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/measurement"
)

// Inputs are what an evaluation reads, resolved by the application before CST's transaction:
// CST's own costing context and measurement revisions, and what INQ, CAT, POL and SUP said about
// the references, each at instant Now. A reference that is absent from its map does not exist or
// lies outside the caller's data scope (REFERENCE_INVALID); a present one carries its owner's
// verdict and content.
type Inputs struct {
	ScenarioID string
	Data       Data
	Now        time.Time

	Context      *Context                     // nil: the context revision is absent
	Rfq          *Rfq                         // INQ now; nil: absent
	Specs        map[costing.Spec]Standing    // CAT
	Configs      map[Ref]Config               // POL, at Now
	Supply       map[Ref]Supply               // SUP, at Now
	Measurements map[MeasurementKey]Measured  // CST
	Contexts     map[ContextRef]AnchorContext // CST: costing context revisions delivery anchors name
	// Sources are the EVD sources the inputs rest on that the caller reads (Context Map R64), with
	// CST's standing of each; a source absent from the map is one the caller does not read.
	Sources map[string]Source
}

// Source is an EVD source as the evaluation reads it: its content hash (SourceRef v1) and whether
// CST recorded a denial of it (costing.attest_source).
type Source struct {
	ContentHash string
	Denied      bool
}

// Context is the costing context revision a scenario names, as CST stores it.
type Context struct {
	ID       string
	Revision *costing.Revision
}

// AnchorContext is a costing context revision a delivery anchor names: whether it was verified and
// when.
type AnchorContext struct {
	Sealed     bool
	Status     string
	VerifiedAt time.Time
	Hash       string
	Sources    []string // the sources its verification and assumptions cite
}

// Rfq is RfqRef v1 as INQ answers it now.
type Rfq struct {
	RfqID      string
	RowVersion int64
	Ready      bool
	LinesHash  string
}

// Standing is CAT's standing of a specification revision: Check is nil when it is current.
type Standing struct {
	Check func(path string) error
}

// Config is what POL said about one configuration revision at Now: Check is POL's verdict (it
// returns nil when the revision is in force), the content hash and validity, and the data of its
// kind; exactly one member is set.
type Config struct {
	Check       func(path string) error
	ContentHash string
	ValidFrom   time.Time
	ValidUntil  time.Time // zero: open-ended

	Currency       *Currency
	Unit           *Unit
	Conversion     *Conversion
	FXRate         *FXRate
	Calendar       *Calendar
	TradeTerms     *TradeTerms
	PaymentTerms   *PaymentTerms
	TaxProfile     *TaxProfile
	TaxApplication *TaxApplication
	FeeCategory    *FeeCategory
	PricingPolicy  *PricingPolicy
}

// Currency is a POL currency.
type Currency struct {
	Code       string
	MinorUnits int
}

// Unit is a POL unit.
type Unit struct {
	Code      string
	Dimension string
	Precision int
}

// Conversion is a POL unit conversion of one SKU specification revision.
type Conversion struct {
	SkuID        string
	SpecRevision int64
	From, To     string
	Factor       string
}

// FXRate is a POL exchange rate of one direction and purpose.
type FXRate struct {
	From, To   string
	Rate       string
	Purpose    string
	ObservedAt string
}

// Calendar is a POL working calendar.
type Calendar struct {
	TimeZone        string
	WorkingWeekdays []int
	Holidays        []string
}

// TradeTerms are POL trade terms.
type TradeTerms struct {
	Incoterm   string
	NamedPlace string
}

// PaymentTerms are POL payment terms.
type PaymentTerms struct {
	Stages   []PaymentStage
	DayBasis string
	Calendar *Ref
}

// PaymentStage is one stage of payment terms.
type PaymentStage struct {
	StageID string
	Percent string
	Trigger string
	Days    int
}

// TaxProfile is a POL tax profile.
type TaxProfile struct {
	Rate       string
	PriceBasis string
	Recovery   string
}

// TaxApplication is a POL tax application.
type TaxApplication struct {
	Side        string
	SubjectKind string
	SubjectID   string
	PartyID     string
	TaxProfile  Ref
}

// FeeCategory is a POL fee category.
type FeeCategory struct {
	Category string
	Side     string
}

// PricingPolicy is a POL pricing policy.
type PricingPolicy struct {
	CalculationCurrency string
	MaxFXAgeSeconds     int64
	TaxProfile          Ref
}

// Supply is what SUP said about one supply condition revision at Now: Status, its verdict (Check
// is nil-safe and returns nil when the revision is in force), the content hash, when it was
// verified, and its content; exactly one of Offer, Terms and Logistics is set.
type Supply struct {
	Status      string
	Check       func(path string) error
	ContentHash string
	VerifiedAt  time.Time
	Sources     []string // the sources its content and its verification cite
	Offer       *Offer
	Terms       *Terms
	Logistics   *Logistics
}

// Offer is a supplier offer revision.
type Offer struct {
	SupplierID    string
	RfqID         string
	RfqRowVersion int64
	RfqLineIDs    []string
	Lines         []ConditionLine
}

// Terms are a purchase terms revision.
type Terms struct {
	SupplierID   string
	SourceOffer  *Ref
	ValidityMode string
	Lines        []ConditionLine
}

// ConditionLine is one priced line of an offer or of purchase terms.
type ConditionLine struct {
	ID                 string
	SkuID              string
	SpecRevision       int64
	PurchaseUnit       *Ref
	PriceUnit          *Ref
	PriceBasisQuantity string
	Conversions        []Ref
	TierUnit           *Ref
	PriceTierScope     string
	PricingMode        string
	Tiers              []Tier
	MOQ                *MOQ
	OrderMultiple      string
	Currency           *Ref
	TaxProfile         *Ref
	TaxApplication     *Ref
	TradeTerms         *Ref
	PaymentTerms       *Ref
	Delivery           *Delivery
	IncludedObligation []string
}

// Tier is one price band.
type Tier struct {
	Lower, Upper    string // Upper empty: open-ended
	UnitPrice       string
	ZeroPriceReason string
}

// MOQ is a minimum order quantity.
type MOQ struct {
	Quantity string
	Unit     *Ref
	Scope    string
	GroupID  string
	Members  []string
}

// Delivery is a supplier's delivery commitment: a fixed date or a lead time after a POL anchor
// event (an order event that has not occurred while costing).
type Delivery struct {
	Kind        string
	Date        string
	AnchorEvent string
	Days        int
	DayBasis    string
	Calendar    *Ref
	Milestone   string
	Place       string
	TimeZone    string
}

// Logistics is a logistics offer revision; ProviderID is the party of its service role.
type Logistics struct {
	ProviderID  string
	ServiceCode string
	Mode        string
	Origin      string
	Destination string
	Fees        []Fee
}

// Fee is one charge of a logistics offer.
type Fee struct {
	ID              string
	ObligationID    string
	FeeType         *Ref
	Party           string
	Currency        *Ref
	TaxProfile      *Ref
	TaxApplication  *Ref
	Billing         *Billing
	Chargeable      *Chargeable
	IncludedIn      string
	Allocation      *FeeAllocation
	ZeroPriceReason string
}

// Billing is a charge's tariff.
type Billing struct {
	Kind          string
	Amount        string
	Rate          string
	BasisQuantity string
	Unit          *Ref
	Minimum       string
	PricingMode   string
	Tiers         []Tier
}

// Chargeable is what a charge is billed on.
type Chargeable struct {
	Kind             string
	Measurement      *MeasurementKey
	BillingIncrement string
	RoundingMode     string
	RoundingScope    string
	MaxScope         string
	Divisor          string
}

// FeeAllocation is how a charge is allocated.
type FeeAllocation struct {
	Basis       string
	Targets     []string
	Measurement *MeasurementKey
	Weights     []Weight
}

// Weight is an explicit relative weight, an integer string.
type Weight struct {
	LineID string
	Weight string
}

// MeasurementKey names a measurement revision.
type MeasurementKey struct {
	MeasurementID string
	Revision      int64
}

// Measured is a measurement revision as CST stores it.
type Measured struct {
	Revision *measurement.Revision
}
