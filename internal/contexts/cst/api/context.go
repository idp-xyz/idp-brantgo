package api

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Kinds of costing context (T02 §13.1): an actual context covers the RFQ's complete line set
// exactly; a what_if context keeps its lines, SKUs and units and changes quantities, and is never
// used for that RFQ's formal quote.
const (
	ContextActual = "actual"
	ContextWhatIf = "what_if"
)

// Closed values of a costing context.
const (
	CostBasisNominalDirect = "nominal_direct_cost"

	DeliveryFixed    = "fixed"
	DeliveryLeadTime = "lead_time"

	MilestoneGoodsReady = "goods_ready"
	MilestoneHandover   = "handover"
	MilestoneArrival    = "arrival"

	DayBasisCalendar = "calendar"
	DayBasisWorking  = "working"

	// PlaceKind is the kind of a delivery or comparison place reference. No Brant context owns
	// locations yet: CST checks the form of a place and compares places as identities.
	PlaceKind = "location"
	// AnchorKind is the kind of a delivery anchor reference: the verification event of a business
	// revision, "<kind>:<object ID>" (T02 §15.3).
	AnchorKind = "delivery_anchor"
)

// POL configuration kinds each field of a costing context takes.
const (
	KindCurrency       = "currency"
	KindPricingPolicy  = "pricing_policy"
	KindUnit           = "unit"
	KindUnitConversion = "unit_conversion"
	KindFeeCategory    = "fee_category"
	KindTradeTerms     = "trade_terms"
	KindPaymentTerms   = "payment_terms"
	KindCalendar       = "calendar"
)

// RfqRef binds a costing context to an INQ RFQ at an exact row version (RfqRef v1).
type RfqRef struct {
	RfqID      string `json:"id"`
	RowVersion int64
}

// ContextLine is one demand line: the RFQ line it costs, the SKU and CAT specification revision,
// the quantity in the unit of UnitRef (a POL unit) and the unit conversions the costing may apply
// (POL unit_conversion revisions of this SKU and specification revision).
type ContextLine struct {
	LineID          string
	SkuID           string
	SkuSpecRevision int64
	Quantity        string // decimal string, never a float
	UnitRef         ConfigRef
	ConversionRefs  []ConfigRef
}

// BusinessRef names a revision of a business object outside POL: a place (kind location) or a
// delivery anchor (kind delivery_anchor).
type BusinessRef struct {
	Kind     string
	ID       string
	Revision int64
}

// Delivery is the customer's verified delivery requirement (T02 §15.3): a milestone at a place in
// an IANA time zone, on a fixed date (YYYY-MM-DD) or Days calendar or working days after the
// verification event an anchor names, counted on a POL calendar.
type Delivery struct {
	Kind        string
	Date        string       // fixed
	AnchorRef   *BusinessRef // lead_time
	Days        int          // lead_time, 0–3650
	DayBasis    string       // lead_time
	CalendarRef *ConfigRef   // lead_time: a POL calendar
	Milestone   string
	PlaceRef    BusinessRef
	TimeZone    string
}

// ComparisonBasis is what every candidate is compared on (T02 §4, §7): the specification, the
// place, the fee types the cost must account for (POL fee categories), the tax basis, the delivery
// requirement, the cost basis and the sales terms (POL trade and payment terms).
type ComparisonBasis struct {
	SpecificationBasis   string
	NamedPlaceRef        BusinessRef
	RequiredFeeTypeRefs  []ConfigRef
	TaxBasis             string
	Delivery             Delivery
	CostBasis            string
	SalesTradeTermsRef   ConfigRef
	SalesPaymentTermsRef ConfigRef
}

// Assumption annotates one typed input with the value a costing assumes (T02 §15.2): Path is a JSON
// Pointer to that input, ValueText its explicit value. It is never executed and never replaces a
// missing value; any assumption keeps a result provisional, so it cannot be frozen.
type Assumption struct {
	AssumptionID string `json:"id"`
	Path         string
	ValueText    string
	Reason       string
	Evidence     []Evidence
}

// ContextData is the content of a costing context revision. A draft may lack any field (nil, zero
// or an omitted list); a field it states must be valid. Verification needs RfqRef, Kind, Lines,
// CalculationCurrencyRef, PricingPolicyRef and ComparisonBasis. Nil and empty lists are different
// requests for idempotency; an empty Lines is refused.
type ContextData struct {
	RfqRef                   *RfqRef
	Kind                     string `json:"context_kind"`
	Lines                    []ContextLine
	CalculationCurrencyRef   *ConfigRef
	PricingPolicyRef         *ConfigRef
	ComparisonBasis          *ComparisonBasis
	RequestedCurrencyRef     *ConfigRef
	RequestedTradeTermsRef   *ConfigRef
	RequestedPaymentTermsRef *ConfigRef
	Assumptions              []Assumption
}

// SaveContext creates a costing context at ExpectedVersion 0 with working revision 1, or replaces
// its working revision whole at the current root version. A context with a sealed revision and no
// working one is revised first (INVALID_STATE).
type SaveContext struct {
	IdempotencyKey  string
	ContextID       string
	ExpectedVersion int64
	Data            ContextData
}

// ReviseContext copies sealed revision BaseRevision into a new working revision; the active
// revision stays in effect until the new one is verified.
type ReviseContext struct {
	IdempotencyKey  string
	ContextID       string
	ExpectedVersion int64
	BaseRevision    int64
	Reason          string
}

// DiscardContextDraft ends the working revision for a recorded reason; its number is not reused
// and no older revision comes back into effect.
type DiscardContextDraft struct {
	IdempotencyKey  string
	ContextID       string
	ExpectedVersion int64
	WorkingRevision int64
	Reason          string
}

// VerifyContext seals the working revision: a human who did not contribute to it, with evidence,
// against the RFQ still ready at the bound row version with its complete line set and every POL
// configuration in force. SupersedesRevision is the active revision it replaces, 0 for none.
type VerifyContext struct {
	IdempotencyKey     string
	ContextID          string
	ExpectedVersion    int64
	WorkingRevision    int64
	SupersedesRevision int64
	Evidence           []Evidence
}

// WithdrawContext withdraws sealed revision TargetRevision for a recorded reason with evidence; it
// clears the active revision when it is the target and revives no older one.
type WithdrawContext struct {
	IdempotencyKey  string
	ContextID       string
	ExpectedVersion int64
	TargetRevision  int64
	Reason          string
	Evidence        []Evidence
}

// Validate checks the closed contract (Python commands.schema.json, costing_context.save_draft).
func (c SaveContext) Validate() error {
	if err := head(c.IdempotencyKey, c.ContextID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	return c.Data.validate("/args/data")
}

func (c ReviseContext) Validate() error {
	if err := head(c.IdempotencyKey, c.ContextID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if err := revision(c.BaseRevision, "/args/base_revision"); err != nil {
		return err
	}
	return reason(c.Reason)
}

func (c DiscardContextDraft) Validate() error {
	if err := head(c.IdempotencyKey, c.ContextID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if err := revision(c.WorkingRevision, "/args/working_revision"); err != nil {
		return err
	}
	return reason(c.Reason)
}

func (c VerifyContext) Validate() error {
	if err := head(c.IdempotencyKey, c.ContextID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if err := revision(c.WorkingRevision, "/args/working_revision"); err != nil {
		return err
	}
	if c.SupersedesRevision != 0 {
		if err := revision(c.SupersedesRevision, "/args/supersedes_revision"); err != nil {
			return err
		}
	}
	return evidence(c.Evidence, 1, MaxEvidence, "/args/evidence")
}

func (c WithdrawContext) Validate() error {
	if err := head(c.IdempotencyKey, c.ContextID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if err := revision(c.TargetRevision, "/args/target_revision"); err != nil {
		return err
	}
	if err := reason(c.Reason); err != nil {
		return err
	}
	return evidence(c.Evidence, 1, MaxEvidence, "/args/evidence")
}

func (d ContextData) validate(path string) error {
	if r := d.RfqRef; r != nil {
		switch {
		case !kernel.ValidIdentifier(r.RfqID):
			return schema(path + "/rfq_ref/id")
		case r.RowVersion < 1 || r.RowVersion > maxVersion:
			return schema(path + "/rfq_ref/row_version")
		}
	}
	if d.Kind != "" && d.Kind != ContextActual && d.Kind != ContextWhatIf {
		return schema(path + "/context_kind")
	}
	if d.Lines != nil && (len(d.Lines) == 0 || len(d.Lines) > MaxLines) {
		return schema(path + "/lines")
	}
	for i, l := range d.Lines {
		if err := l.validate(index(path+"/lines", i)); err != nil {
			return err
		}
	}
	for _, r := range []struct {
		ref   *ConfigRef
		field string
	}{
		{d.CalculationCurrencyRef, "/calculation_currency_ref"}, {d.PricingPolicyRef, "/pricing_policy_ref"},
		{d.RequestedCurrencyRef, "/requested_currency_ref"}, {d.RequestedTradeTermsRef, "/requested_trade_terms_ref"},
		{d.RequestedPaymentTermsRef, "/requested_payment_terms_ref"},
	} {
		if r.ref != nil {
			if err := r.ref.Validate(path + r.field); err != nil {
				return err
			}
		}
	}
	if b := d.ComparisonBasis; b != nil {
		if err := b.validate(path + "/comparison_basis"); err != nil {
			return err
		}
	}
	if len(d.Assumptions) > MaxAssumptions {
		return schema(path + "/assumptions")
	}
	for i, a := range d.Assumptions {
		p := index(path+"/assumptions", i)
		switch {
		case !kernel.ValidIdentifier(a.AssumptionID):
			return schema(p + "/id")
		case !text(a.Path, maxText):
			return schema(p + "/path")
		case !text(a.ValueText, maxText):
			return schema(p + "/value_text")
		case !text(a.Reason, maxText):
			return schema(p + "/reason")
		}
		if err := evidence(a.Evidence, 1, MaxAssumptionProofs, p+"/evidence"); err != nil {
			return err
		}
	}
	return nil
}

func (l ContextLine) validate(p string) error {
	switch {
	case !kernel.ValidIdentifier(l.LineID):
		return schema(p + "/line_id")
	case !kernel.ValidIdentifier(l.SkuID):
		return schema(p + "/sku_id")
	case l.SkuSpecRevision < 1 || l.SkuSpecRevision > maxVersion:
		return schema(p + "/sku_spec_revision")
	case !positive(l.Quantity):
		return schema(p + "/quantity")
	}
	if err := l.UnitRef.Validate(p + "/unit_ref"); err != nil {
		return err
	}
	if len(l.ConversionRefs) > MaxConversions {
		return schema(p + "/conversion_refs")
	}
	for j, c := range l.ConversionRefs {
		if err := c.Validate(index(p+"/conversion_refs", j)); err != nil {
			return err
		}
	}
	return nil
}

func (b ComparisonBasis) validate(p string) error {
	switch {
	case !text(b.SpecificationBasis, maxText):
		return schema(p + "/specification_basis")
	case !text(b.TaxBasis, maxText):
		return schema(p + "/tax_basis")
	case b.CostBasis != CostBasisNominalDirect:
		return schema(p + "/cost_basis")
	case len(b.RequiredFeeTypeRefs) > MaxRequiredFees:
		return schema(p + "/required_fee_type_refs")
	}
	if err := place(b.NamedPlaceRef, p+"/named_place_ref"); err != nil {
		return err
	}
	for i, r := range b.RequiredFeeTypeRefs {
		if err := r.Validate(index(p+"/required_fee_type_refs", i)); err != nil {
			return err
		}
	}
	if err := b.Delivery.validate(p + "/delivery"); err != nil {
		return err
	}
	if err := b.SalesTradeTermsRef.Validate(p + "/sales_trade_terms_ref"); err != nil {
		return err
	}
	return b.SalesPaymentTermsRef.Validate(p + "/sales_payment_terms_ref")
}

var milestones = []string{MilestoneGoodsReady, MilestoneHandover, MilestoneArrival}

func (d Delivery) validate(p string) error {
	if !slices.Contains(milestones, d.Milestone) {
		return schema(p + "/milestone")
	}
	if !text(d.TimeZone, maxTimeZone) {
		return schema(p + "/time_zone")
	}
	if err := place(d.PlaceRef, p+"/place_ref"); err != nil {
		return err
	}
	switch d.Kind {
	case DeliveryFixed:
		if _, err := time.Parse(time.DateOnly, d.Date); err != nil {
			return schema(p + "/date")
		}
		if d.AnchorRef != nil || d.Days != 0 || d.DayBasis != "" || d.CalendarRef != nil {
			return schema(p)
		}
		return nil
	case DeliveryLeadTime:
	default:
		return schema(p + "/kind")
	}
	switch {
	case d.Date != "":
		return schema(p + "/date")
	case d.AnchorRef == nil:
		return schema(p + "/anchor_ref")
	case d.AnchorRef.Kind != AnchorKind:
		return schema(p + "/anchor_ref/kind")
	case d.Days < 0 || d.Days > maxLeadDays:
		return schema(p + "/days")
	case d.DayBasis != DayBasisCalendar && d.DayBasis != DayBasisWorking:
		return schema(p + "/day_basis")
	case d.CalendarRef == nil:
		return schema(p + "/calendar_ref")
	}
	if err := ref(d.AnchorRef.Kind, d.AnchorRef.ID, d.AnchorRef.Revision, p+"/anchor_ref"); err != nil {
		return err
	}
	return d.CalendarRef.Validate(p + "/calendar_ref")
}

func place(r BusinessRef, p string) error {
	if r.Kind != PlaceKind {
		return schema(p + "/kind")
	}
	return ref(r.Kind, r.ID, r.Revision, p)
}

// ContextRevisionView is one revision of a costing context. A sealed revision never changes.
type ContextRevisionView struct {
	Revision     int64
	Status       string
	BaseRevision int64 // the sealed revision it was revised from, 0 for none
	ContentHash  string
	Data         ContextData
	Contributors []string // the actors who saved it, in order of their first save
	SavedBy      kernel.ActorRef
	SavedAt      time.Time
	Proposals    []AssumptionProposal

	VerifiedBy           kernel.ActorRef // zero before verification
	VerifiedAt           time.Time
	VerificationEvidence []Evidence
	AuthorityEpoch       int64
	Rfq                  RfqBinding  // what the verification bound
	RfqLines             []BoundLine // the RFQ's complete line set as INQ gave it to the verification

	SupersededAt time.Time
	EndedBy      kernel.ActorRef // who withdrew or discarded it
	EndedAt      time.Time
	EndReason    string
	EndEvidence  []Evidence // withdrawal evidence
}

// AssumptionProposal is who proposed an assumption and when: the server records it when the
// assumption first appears in a working revision, or changes (T02 §15.2).
type AssumptionProposal struct {
	AssumptionID string
	ProposedBy   kernel.ActorRef
	ProposedAt   time.Time
}

// RfqBinding is the RfqRef v1 a verification bound: the RFQ, its row version and the hash of its
// complete line set as INQ computes it (inq/api RfqLinesHash).
type RfqBinding struct {
	RfqID      string
	RowVersion int64
	LinesHash  string
}

// ContextView is a costing context and all its revisions, oldest first.
type ContextView struct {
	ContextID       string
	RootVersion     int64
	WorkingRevision int64 // 0: none
	ActiveRevision  int64 // 0: none
	LastRevision    int64
	Revisions       []ContextRevisionView
}
