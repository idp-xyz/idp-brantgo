package api

import (
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The content of SUP's documents. Decimals are strings in their contract format, never floats:
// quantities (tier bounds, price basis quantities, MOQs, order multiples, billing increments)
// kernel.QuantityFormat, prices and amounts kernel.PriceFormat, divisors kernel.FactorFormat.
// Times are the contract's UTC timestamps ("2026-10-22T00:00:00Z"); SUP stores them in canonical
// form. A draft may omit anything; an omitted value is unknown, never zero. The JSON names below
// are the snake_case of the Go field names, as transports derive them; SUP stores the same JSON.

// Closed values of the content.
const (
	OriginInResponseToRfq   = "in_response_to_rfq"  // the offer answers an issued supplier RFQ
	OriginUnsolicited       = "unsolicited"         // the supplier offered without being asked
	OriginImportedPriceList = "imported_price_list" // a price list the supplier published

	ValidityIndependent = "independent_validity" // purchase terms in force on their own validity
	ValidityDependent   = "dependent_validity"   // purchase terms in force only while the source offer is

	TierScopeSKU           = "sku"            // tiers count the condition line's SKU (also when omitted)
	TierScopeExplicitGroup = "explicit_group" // refused at verification: UNSUPPORTED_PRICING_MODE
	PricingFlat            = "flat"
	PricingAllUnits        = "all_units"
	PricingGraduated       = "graduated"
	MOQScopeSKU            = "sku"
	MOQScopeExplicitGroup  = "explicit_group"

	DeliveryFixed       = "fixed"
	DeliveryLeadTime    = "lead_time"
	MilestoneGoodsReady = "goods_ready"
	MilestoneHandover   = "handover"
	MilestoneArrival    = "arrival"
	DayBasisCalendar    = "calendar"
	DayBasisWorking     = "working"
	// Anchor events of a lead time (POL's Delivery anchors).
	AnchorOrderConfirmed    = "order_confirmed"
	AnchorDepositReceived   = "deposit_received"
	AnchorTechnicalApproved = "technical_approved"

	ModeSea     = "sea"
	ModeAir     = "air"
	ModeRoad    = "road"
	ModeRail    = "rail"
	ModeCourier = "courier"

	PartySeller = "seller" // the exporter bears the charge under the export sale
	PartyBuyer  = "buyer"  // the customer bears it

	BillingFixed    = "fixed"
	BillingPerBasis = "per_basis"
	BillingTiered   = "tiered"

	ChargeableFixed               = "fixed"
	ChargeableActualWeight        = "actual_weight"
	ChargeableVolume              = "volume"
	ChargeableMaxActualVolumetric = "max_actual_volumetric"
	RoundingCeil                  = "ceil"
	RoundingHalfUp                = "half_up"
	ScopePerPackage               = "per_package"
	ScopeShipment                 = "shipment"

	AllocationQuantity = "quantity"
	AllocationWeight   = "weight"
	AllocationVolume   = "volume"
	AllocationAmount   = "amount"
	AllocationExplicit = "explicit"

	// MaxExplicitWeight bounds an explicit allocation weight (T02 §13.4).
	MaxExplicitWeight = 1_000_000_000
	// MaxLeadDays bounds a lead time.
	MaxLeadDays = 3650
)

// POL configuration kinds the content refers to (Context Map R07).
const (
	ConfigUnit           = "unit"
	ConfigUnitConversion = "unit_conversion"
	ConfigCurrency       = "currency"
	ConfigTaxProfile     = "tax_profile"
	ConfigTaxApplication = "tax_application"
	ConfigTradeTerms     = "trade_terms"
	ConfigPaymentTerms   = "payment_terms"
	ConfigFeeCategory    = "fee_category"
	ConfigCalendar       = "calendar"
)

var (
	origins       = []string{OriginInResponseToRfq, OriginUnsolicited, OriginImportedPriceList}
	validityModes = []string{ValidityIndependent, ValidityDependent}
	tierScopes    = []string{TierScopeSKU, TierScopeExplicitGroup}
	moqScopes     = []string{MOQScopeSKU, MOQScopeExplicitGroup}
	milestones    = []string{MilestoneGoodsReady, MilestoneHandover, MilestoneArrival}
	dayBases      = []string{DayBasisCalendar, DayBasisWorking}
	anchors       = []string{AnchorOrderConfirmed, AnchorDepositReceived, AnchorTechnicalApproved}
	modes         = []string{ModeSea, ModeAir, ModeRoad, ModeRail, ModeCourier}
	parties       = []string{PartySeller, PartyBuyer}
	roundings     = []string{RoundingCeil, RoundingHalfUp}
	scopes        = []string{ScopePerPackage, ScopeShipment}
	allocations   = []string{AllocationQuantity, AllocationWeight, AllocationVolume, AllocationAmount, AllocationExplicit}
)

// ConfigRef is POL's ConfigRef v1 (kind, id, revision) as SUP's content carries it.
type ConfigRef struct {
	Kind     string `json:"kind"`
	ConfigID string `json:"id"`
	Revision int64  `json:"revision"`
}

// PartyRef is PTY's PartyRef v1 as a document binds it: a party at the row version its author saw.
type PartyRef struct {
	PartyID    string `json:"party_id"`
	RowVersion int64  `json:"row_version"`
}

// RfqRef is a customer RFQ of INQ at the row version its author saw (RfqLineRef v1 binds its lines
// through it).
type RfqRef struct {
	RfqID      string `json:"rfq_id"`
	RowVersion int64  `json:"row_version"`
}

// SupplierRfqRef is a supplier RFQ at the row version its issue sealed.
type SupplierRfqRef struct {
	SupplierRfqID string `json:"supplier_rfq_id"`
	RowVersion    int64  `json:"row_version"`
}

// SkuRef is CAT's SkuRef v1 as SUP stores it: a SKU at one specification revision.
type SkuRef struct {
	SkuID        string `json:"sku_id"`
	SpecRevision int64  `json:"spec_revision"`
}

// ItemCodeMappingRef is CAT's ItemCodeMappingRef v1: the published mapping revision that maps the
// supplier's own item code to the line's SKU.
type ItemCodeMappingRef struct {
	MappingID string `json:"mapping_id"`
	Revision  int64  `json:"revision"`
}

// ServiceRoleRef is PTY's ServiceRoleRef v1: one verified revision of a service party role.
type ServiceRoleRef struct {
	PartyID       string `json:"party_id"`
	ServiceRoleID string `json:"service_role_id"`
	Revision      int64  `json:"revision"`
}

// MeasurementRef names a MeasurementSnapshot revision a charge is measured on. Measurements belong
// to CST, which is downstream of SUP: SUP stores the reference as given and CST checks it when it
// evaluates the charge.
type MeasurementRef struct {
	MeasurementID string `json:"measurement_id"`
	Revision      int64  `json:"revision"`
}

// SupplierRfq is the content of a supplier RFQ: the supplier asked, the ready customer RFQ and the
// lines of it the request covers, and the sources of the request.
type SupplierRfq struct {
	Supplier   *PartyRef  `json:"supplier,omitempty"`
	Rfq        *RfqRef    `json:"rfq,omitempty"`
	RfqLineIDs []string   `json:"rfq_line_ids,omitempty"`
	Evidence   []Evidence `json:"evidence,omitempty"`
}

// Origin is how an offer came: in response to an issued supplier RFQ, which it names, or
// unsolicited or as an imported price list, which name none and never invent one (T02 §13.8).
type Origin struct {
	Kind        string          `json:"kind"`
	SupplierRfq *SupplierRfqRef `json:"supplier_rfq,omitempty"`
}

// SupplierOffer is the content of a supplier offer revision (T02 §5): the supplier, its origin,
// the customer RFQ lines it answers, its validity [ValidFrom, ValidUntil), its condition lines and
// the sources it was read from.
type SupplierOffer struct {
	Supplier   *PartyRef       `json:"supplier,omitempty"`
	Origin     *Origin         `json:"origin,omitempty"`
	Rfq        *RfqRef         `json:"rfq,omitempty"`
	RfqLineIDs []string        `json:"rfq_line_ids,omitempty"`
	ValidFrom  string          `json:"valid_from,omitempty"`
	ValidUntil string          `json:"valid_until,omitempty"`
	Lines      []ConditionLine `json:"lines,omitempty"`
	Evidence   []Evidence      `json:"evidence,omitempty"`
}

// PurchaseTerms is the content of a purchase terms revision (T02 §5 可复用条件, §13.8; A46):
// reusable conditions of a supplier formed from a verified offer revision, SourceOfferRef. With
// independent validity they are in force on their own validity, the source offer being provenance
// only; with dependent validity they are in force only while the source offer revision is.
type PurchaseTerms struct {
	Supplier       *PartyRef           `json:"supplier,omitempty"`
	SourceOfferRef *SupplyConditionRef `json:"source_offer_ref,omitempty"`
	ValidityMode   string              `json:"validity_mode,omitempty"`
	ValidFrom      string              `json:"valid_from,omitempty"`
	ValidUntil     string              `json:"valid_until,omitempty"`
	Lines          []ConditionLine     `json:"lines,omitempty"`
	Evidence       []Evidence          `json:"evidence,omitempty"`
}

// ConditionLine is one priced line of an offer or of purchase terms (T02 §5, §13.1): a SKU at a
// specification revision, optionally with the supplier's item code and the CAT mapping that maps
// it; units, conversions, price tiers, MOQ and order multiple; currency, tax and terms references;
// the supplier's delivery commitment; and the charge obligations the price includes.
type ConditionLine struct {
	ID                    string              `json:"id"`
	Sku                   *SkuRef             `json:"sku,omitempty"`
	SupplierItemCode      string              `json:"supplier_item_code,omitempty"`
	ItemCodeMapping       *ItemCodeMappingRef `json:"item_code_mapping,omitempty"`
	PurchaseUnitRef       *ConfigRef          `json:"purchase_unit_ref,omitempty"`
	PriceUnitRef          *ConfigRef          `json:"price_unit_ref,omitempty"`
	PriceBasisQuantity    string              `json:"price_basis_quantity,omitempty"`
	ConversionRefs        []ConfigRef         `json:"conversion_refs,omitempty"`
	TierUnitRef           *ConfigRef          `json:"tier_unit_ref,omitempty"`
	PriceTierScope        string              `json:"price_tier_scope,omitempty"`
	PricingMode           string              `json:"pricing_mode,omitempty"` // empty: the offer does not say; stays pending (SUP-INV-002)
	Tiers                 []Tier              `json:"tiers,omitempty"`
	MOQ                   *MOQ                `json:"moq,omitempty"`
	OrderMultiple         string              `json:"order_multiple,omitempty"`
	CurrencyRef           *ConfigRef          `json:"currency_ref,omitempty"`
	TaxProfileRef         *ConfigRef          `json:"tax_profile_ref,omitempty"`
	TaxApplicationRef     *ConfigRef          `json:"tax_application_ref,omitempty"`
	TradeTermsRef         *ConfigRef          `json:"trade_terms_ref,omitempty"`
	PaymentTermsRef       *ConfigRef          `json:"payment_terms_ref,omitempty"`
	Delivery              *Delivery           `json:"delivery,omitempty"`
	IncludedObligationIDs []string            `json:"included_obligation_ids,omitempty"`
}

// Tier is the half-open band [LowerInclusive, UpperExclusive) priced at UnitPrice per price basis
// quantity; an empty UpperExclusive is open-ended. A zero price needs a ZeroPriceReason.
type Tier struct {
	LowerInclusive  string `json:"lower_inclusive"`
	UpperExclusive  string `json:"upper_exclusive,omitempty"`
	UnitPrice       string `json:"unit_price"`
	ZeroPriceReason string `json:"zero_price_reason,omitempty"`
}

// MOQ is a minimum order quantity in a unit, counted per SKU or over an explicit group of the
// request's lines the supplier lets be ordered together.
type MOQ struct {
	Quantity      string     `json:"quantity"`
	UnitRef       *ConfigRef `json:"unit_ref,omitempty"`
	Scope         string     `json:"scope"`
	GroupID       string     `json:"group_id,omitempty"`
	MemberLineIDs []string   `json:"member_line_ids,omitempty"`
}

// Delivery is a supplier's delivery commitment (T02 §15.3): a milestone at an exact place
// (UN/LOCODE) in an IANA time zone, on a fixed Date (YYYY-MM-DD) or Days after AnchorEvent counted
// on DayBasis in a POL calendar revision. A commitment is not reserved stock.
type Delivery struct {
	Kind        string     `json:"kind"`
	Date        string     `json:"date,omitempty"`
	AnchorEvent string     `json:"anchor_event,omitempty"`
	Days        int        `json:"days"`
	DayBasis    string     `json:"day_basis,omitempty"`
	CalendarRef *ConfigRef `json:"calendar_ref,omitempty"`
	Milestone   string     `json:"milestone"`
	Place       string     `json:"place"`
	TimeZone    string     `json:"time_zone"`
}

// LogisticsOffer is the content of a logistics offer revision (T02 §6): the verified service role
// of the provider, exactly its service, mode, origin and destination (UN/LOCODE; zones and routes
// are refused), the validity, the charges and the sources.
type LogisticsOffer struct {
	ServiceRoleRef *ServiceRoleRef `json:"service_role_ref,omitempty"`
	ServiceCode    string          `json:"service_code,omitempty"`
	Mode           string          `json:"mode,omitempty"`
	Origin         string          `json:"origin,omitempty"`
	Destination    string          `json:"destination,omitempty"`
	ValidFrom      string          `json:"valid_from,omitempty"`
	ValidUntil     string          `json:"valid_until,omitempty"`
	Fees           []Fee           `json:"fees,omitempty"`
	Evidence       []Evidence      `json:"evidence,omitempty"`
}

// Fee is one charge of a logistics offer (T02 §6): the commercial obligation it pays, its fee type
// (a POL fee_category), who bears it under the export sale, currency and tax references, its
// tariff, chargeable basis, inclusion in another component and allocation.
type Fee struct {
	ID                     string      `json:"id"`
	ObligationID           string      `json:"obligation_id,omitempty"`
	FeeTypeRef             *ConfigRef  `json:"fee_type_ref,omitempty"`
	ResponsibleParty       string      `json:"responsible_party,omitempty"`
	CurrencyRef            *ConfigRef  `json:"currency_ref,omitempty"`
	TaxProfileRef          *ConfigRef  `json:"tax_profile_ref,omitempty"`
	TaxApplicationRef      *ConfigRef  `json:"tax_application_ref,omitempty"`
	Billing                *Billing    `json:"billing,omitempty"`
	Chargeable             *Chargeable `json:"chargeable,omitempty"`
	IncludedInComponentRef string      `json:"included_in_component_ref,omitempty"`
	Allocation             *Allocation `json:"allocation,omitempty"`
	ZeroPriceReason        string      `json:"zero_price_reason,omitempty"`
}

// Billing is a charge's tariff: fixed (Amount), per basis (Rate per BasisQuantity of UnitRef, at
// least MinimumAmount) or tiered (PricingMode and Tiers over the quantity in UnitRef, per
// BasisQuantity, at least MinimumAmount). A measured charge is in KG, or M3 for a volume basis.
type Billing struct {
	Kind          string     `json:"kind"`
	Amount        string     `json:"amount,omitempty"`
	Rate          string     `json:"rate,omitempty"`
	BasisQuantity string     `json:"basis_quantity,omitempty"`
	UnitRef       *ConfigRef `json:"unit_ref,omitempty"`
	MinimumAmount string     `json:"minimum_amount,omitempty"`
	PricingMode   string     `json:"pricing_mode,omitempty"`
	Tiers         []Tier     `json:"tiers,omitempty"`
}

// Chargeable is what a charge is billed on: a fixed charge, or the actual weight, the volume or
// the larger of actual and volumetric weight of a measurement, rounded to BillingIncrement per
// package or per shipment. The volumetric divisor is explicit (m³ per kg); no default is assumed.
type Chargeable struct {
	Kind             string          `json:"kind"`
	MeasurementRef   *MeasurementRef `json:"measurement_ref,omitempty"`
	BillingIncrement string          `json:"billing_increment,omitempty"`
	RoundingMode     string          `json:"rounding_mode,omitempty"`
	RoundingScope    string          `json:"rounding_scope,omitempty"`
	MaxScope         string          `json:"max_scope,omitempty"`
	DivisorM3PerKG   string          `json:"divisor_m3_per_kg,omitempty"`
}

// Allocation is how a charge is allocated to the lines a scenario buys: by quantity, weight,
// volume or amount over TargetLineIDs, or by explicit integer Weights. Weight and volume need the
// measurement they are taken from.
type Allocation struct {
	Basis          string           `json:"basis"`
	TargetLineIDs  []string         `json:"target_line_ids,omitempty"`
	MeasurementRef *MeasurementRef  `json:"measurement_ref,omitempty"`
	Weights        []ExplicitWeight `json:"weights,omitempty"`
}

// ExplicitWeight is a relative allocation weight: an integer string, not a ratio.
type ExplicitWeight struct {
	LineID string `json:"line_id"`
	Weight string `json:"weight"`
}

// Validate checks what a draft states: every value given is well formed. Completeness, current
// references and the pricing rules are judged when the draft is sealed.
func (d SupplierRfq) Validate(path string) error {
	if err := partyRef(d.Supplier, path+"/supplier"); err != nil {
		return err
	}
	if err := rfqRef(d.Rfq, path+"/rfq"); err != nil {
		return err
	}
	if err := ids(d.RfqLineIDs, MaxLines, path+"/rfq_line_ids"); err != nil {
		return err
	}
	return evidence(d.Evidence, 0, path+"/evidence")
}

func (d SupplierOffer) Validate(path string) error {
	if err := partyRef(d.Supplier, path+"/supplier"); err != nil {
		return err
	}
	if o := d.Origin; o != nil {
		switch {
		case !slices.Contains(origins, o.Kind):
			return schema(path + "/origin/kind")
		case (o.Kind == OriginInResponseToRfq) != (o.SupplierRfq != nil):
			return schema(path + "/origin/supplier_rfq")
		}
		if r := o.SupplierRfq; r != nil && (!kernel.ValidIdentifier(r.SupplierRfqID) || r.RowVersion < 1 || r.RowVersion > maxVersion) {
			return schema(path + "/origin/supplier_rfq")
		}
	}
	if err := rfqRef(d.Rfq, path+"/rfq"); err != nil {
		return err
	}
	if err := ids(d.RfqLineIDs, MaxLines, path+"/rfq_line_ids"); err != nil {
		return err
	}
	if err := validity(d.ValidFrom, d.ValidUntil, path); err != nil {
		return err
	}
	if err := lines(d.Lines, path+"/lines"); err != nil {
		return err
	}
	return evidence(d.Evidence, 0, path+"/evidence")
}

func (d PurchaseTerms) Validate(path string) error {
	if err := partyRef(d.Supplier, path+"/supplier"); err != nil {
		return err
	}
	if r := d.SourceOfferRef; r != nil {
		if err := r.Validate(path + "/source_offer_ref"); err != nil {
			return err
		}
		if r.Kind != KindSupplierOffer {
			return kernel.Reject(kernel.CodeReferenceInvalid, path+"/source_offer_ref/kind", "Purchase terms are formed from a supplier offer")
		}
	}
	if d.ValidityMode != "" && !slices.Contains(validityModes, d.ValidityMode) {
		return schema(path + "/validity_mode")
	}
	if err := validity(d.ValidFrom, d.ValidUntil, path); err != nil {
		return err
	}
	if err := lines(d.Lines, path+"/lines"); err != nil {
		return err
	}
	return evidence(d.Evidence, 0, path+"/evidence")
}

func (d LogisticsOffer) Validate(path string) error {
	if r := d.ServiceRoleRef; r != nil && (!kernel.ValidIdentifier(r.PartyID) || !kernel.ValidIdentifier(r.ServiceRoleID) ||
		r.Revision < 1 || r.Revision > maxVersion) {
		return schema(path + "/service_role_ref")
	}
	switch {
	case d.ServiceCode != "" && !kernel.ValidIdentifier(d.ServiceCode):
		return schema(path + "/service_code")
	case d.Mode != "" && !slices.Contains(modes, d.Mode):
		return schema(path + "/mode")
	case d.Origin != "" && !Location(d.Origin):
		return schema(path + "/origin")
	case d.Destination != "" && !Location(d.Destination):
		return schema(path + "/destination")
	case len(d.Fees) > MaxFees:
		return schema(path + "/fees")
	}
	if err := validity(d.ValidFrom, d.ValidUntil, path); err != nil {
		return err
	}
	for i, f := range d.Fees {
		if err := f.validate(path + "/fees/" + strconv.Itoa(i)); err != nil {
			return err
		}
	}
	return evidence(d.Evidence, 0, path+"/evidence")
}

func (l ConditionLine) validate(path string) error {
	if !kernel.ValidIdentifier(l.ID) {
		return schema(path + "/id")
	}
	if s := l.Sku; s != nil && (!kernel.ValidIdentifier(s.SkuID) || s.SpecRevision < 1 || s.SpecRevision > maxVersion) {
		return schema(path + "/sku")
	}
	if l.SupplierItemCode != "" && !itemCode(l.SupplierItemCode) {
		return schema(path + "/supplier_item_code")
	}
	if m := l.ItemCodeMapping; m != nil {
		if !kernel.ValidIdentifier(m.MappingID) || m.Revision < 1 || m.Revision > maxVersion {
			return schema(path + "/item_code_mapping")
		}
		if l.SupplierItemCode == "" {
			return schema(path + "/supplier_item_code")
		}
	}
	for _, r := range []struct {
		ref  *ConfigRef
		kind string
		name string
	}{
		{l.PurchaseUnitRef, ConfigUnit, "purchase_unit_ref"}, {l.PriceUnitRef, ConfigUnit, "price_unit_ref"},
		{l.TierUnitRef, ConfigUnit, "tier_unit_ref"}, {l.CurrencyRef, ConfigCurrency, "currency_ref"},
		{l.TaxProfileRef, ConfigTaxProfile, "tax_profile_ref"}, {l.TaxApplicationRef, ConfigTaxApplication, "tax_application_ref"},
		{l.TradeTermsRef, ConfigTradeTerms, "trade_terms_ref"}, {l.PaymentTermsRef, ConfigPaymentTerms, "payment_terms_ref"},
	} {
		if err := configRef(r.ref, r.kind, path+"/"+r.name); err != nil {
			return err
		}
	}
	if len(l.ConversionRefs) > MaxConversions {
		return schema(path + "/conversion_refs")
	}
	for i := range l.ConversionRefs {
		if err := configRef(&l.ConversionRefs[i], ConfigUnitConversion, path+"/conversion_refs/"+strconv.Itoa(i)); err != nil {
			return err
		}
		if slices.Index(l.ConversionRefs, l.ConversionRefs[i]) != i {
			return schema(path + "/conversion_refs")
		}
	}
	if err := decimal(l.PriceBasisQuantity, kernel.QuantityFormat, path+"/price_basis_quantity"); err != nil {
		return err
	}
	if err := decimal(l.OrderMultiple, kernel.QuantityFormat, path+"/order_multiple"); err != nil {
		return err
	}
	if l.PriceTierScope != "" && !slices.Contains(tierScopes, l.PriceTierScope) {
		return schema(path + "/price_tier_scope")
	}
	if l.PricingMode != "" && !token(l.PricingMode) {
		return schema(path + "/pricing_mode")
	}
	if err := tiers(l.Tiers, path+"/tiers"); err != nil {
		return err
	}
	if m := l.MOQ; m != nil {
		if err := m.validate(path + "/moq"); err != nil {
			return err
		}
	}
	if d := l.Delivery; d != nil {
		if err := d.validate(path + "/delivery"); err != nil {
			return err
		}
	}
	return ids(l.IncludedObligationIDs, MaxObligations, path+"/included_obligation_ids")
}

func (m MOQ) validate(path string) error {
	switch {
	case m.Quantity == "":
		return schema(path + "/quantity")
	case m.UnitRef == nil:
		return schema(path + "/unit_ref")
	case !slices.Contains(moqScopes, m.Scope):
		return schema(path + "/scope")
	case m.Scope == MOQScopeSKU && (m.GroupID != "" || m.MemberLineIDs != nil):
		return schema(path + "/scope")
	case m.GroupID != "" && !kernel.ValidIdentifier(m.GroupID):
		return schema(path + "/group_id")
	}
	if err := decimal(m.Quantity, kernel.QuantityFormat, path+"/quantity"); err != nil {
		return err
	}
	if err := configRef(m.UnitRef, ConfigUnit, path+"/unit_ref"); err != nil {
		return err
	}
	return ids(m.MemberLineIDs, MaxLines, path+"/member_line_ids")
}

func (d Delivery) validate(path string) error {
	switch {
	case !slices.Contains(milestones, d.Milestone):
		return schema(path + "/milestone")
	case !Location(d.Place):
		return schema(path + "/place")
	case !text(d.TimeZone, maxTimeZone) || strings.ContainsFunc(d.TimeZone, unicode.IsSpace):
		return schema(path + "/time_zone")
	}
	switch d.Kind {
	case DeliveryFixed:
		if _, err := time.Parse(time.DateOnly, d.Date); err != nil || len(d.Date) != len(time.DateOnly) {
			return schema(path + "/date")
		}
		if d.AnchorEvent != "" || d.Days != 0 || d.DayBasis != "" || d.CalendarRef != nil {
			return schema(path + "/kind")
		}
	case DeliveryLeadTime:
		switch {
		case d.Date != "":
			return schema(path + "/date")
		case !slices.Contains(anchors, d.AnchorEvent):
			return schema(path + "/anchor_event")
		case d.Days < 0 || d.Days > MaxLeadDays:
			return schema(path + "/days")
		case !slices.Contains(dayBases, d.DayBasis):
			return schema(path + "/day_basis")
		case d.CalendarRef == nil:
			return schema(path + "/calendar_ref")
		}
		return configRef(d.CalendarRef, ConfigCalendar, path+"/calendar_ref")
	default:
		return schema(path + "/kind")
	}
	return nil
}

func (f Fee) validate(path string) error {
	switch {
	case !kernel.ValidIdentifier(f.ID):
		return schema(path + "/id")
	case f.ObligationID != "" && !kernel.ValidIdentifier(f.ObligationID):
		return schema(path + "/obligation_id")
	case f.ResponsibleParty != "" && !slices.Contains(parties, f.ResponsibleParty):
		return schema(path + "/responsible_party")
	case f.IncludedInComponentRef != "" && !kernel.ValidIdentifier(f.IncludedInComponentRef):
		return schema(path + "/included_in_component_ref")
	case f.ZeroPriceReason != "" && !text(f.ZeroPriceReason, maxText):
		return schema(path + "/zero_price_reason")
	}
	for _, r := range []struct {
		ref  *ConfigRef
		kind string
		name string
	}{
		{f.FeeTypeRef, ConfigFeeCategory, "fee_type_ref"}, {f.CurrencyRef, ConfigCurrency, "currency_ref"},
		{f.TaxProfileRef, ConfigTaxProfile, "tax_profile_ref"}, {f.TaxApplicationRef, ConfigTaxApplication, "tax_application_ref"},
	} {
		if err := configRef(r.ref, r.kind, path+"/"+r.name); err != nil {
			return err
		}
	}
	if b := f.Billing; b != nil {
		p := path + "/billing"
		switch {
		case !token(b.Kind):
			return schema(p + "/kind")
		case b.PricingMode != "" && !token(b.PricingMode):
			return schema(p + "/pricing_mode")
		}
		for _, d := range []struct{ value, format, name string }{
			{b.Amount, "price", "amount"}, {b.Rate, "price", "rate"}, {b.MinimumAmount, "price", "minimum_amount"},
			{b.BasisQuantity, "quantity", "basis_quantity"},
		} {
			format := kernel.PriceFormat
			if d.format == "quantity" {
				format = kernel.QuantityFormat
			}
			if err := decimal(d.value, format, p+"/"+d.name); err != nil {
				return err
			}
		}
		if err := configRef(b.UnitRef, ConfigUnit, p+"/unit_ref"); err != nil {
			return err
		}
		if err := tiers(b.Tiers, p+"/tiers"); err != nil {
			return err
		}
	}
	if c := f.Chargeable; c != nil {
		p := path + "/chargeable"
		switch {
		case !token(c.Kind):
			return schema(p + "/kind")
		case c.RoundingMode != "" && !slices.Contains(roundings, c.RoundingMode):
			return schema(p + "/rounding_mode")
		case c.RoundingScope != "" && !slices.Contains(scopes, c.RoundingScope):
			return schema(p + "/rounding_scope")
		case c.MaxScope != "" && !slices.Contains(scopes, c.MaxScope):
			return schema(p + "/max_scope")
		}
		if err := measurementRef(c.MeasurementRef, p+"/measurement_ref"); err != nil {
			return err
		}
		if err := decimal(c.BillingIncrement, kernel.QuantityFormat, p+"/billing_increment"); err != nil {
			return err
		}
		if err := decimal(c.DivisorM3PerKG, kernel.FactorFormat, p+"/divisor_m3_per_kg"); err != nil {
			return err
		}
	}
	if a := f.Allocation; a != nil {
		p := path + "/allocation"
		if !slices.Contains(allocations, a.Basis) {
			return schema(p + "/basis")
		}
		if (a.Basis == AllocationExplicit) != (a.Weights != nil) || (a.Basis == AllocationExplicit && a.TargetLineIDs != nil) {
			return schema(p + "/basis")
		}
		if err := ids(a.TargetLineIDs, MaxLines, p+"/target_line_ids"); err != nil {
			return err
		}
		if err := measurementRef(a.MeasurementRef, p+"/measurement_ref"); err != nil {
			return err
		}
		if len(a.Weights) > MaxLines {
			return schema(p + "/weights")
		}
		for i, w := range a.Weights {
			wp := p + "/weights/" + strconv.Itoa(i)
			if !kernel.ValidIdentifier(w.LineID) {
				return schema(wp + "/line_id")
			}
			if !integer(w.Weight) {
				return schema(wp + "/weight")
			}
		}
	}
	return nil
}

func lines(ls []ConditionLine, path string) error {
	if len(ls) > MaxLines {
		return schema(path)
	}
	for i, l := range ls {
		if err := l.validate(path + "/" + strconv.Itoa(i)); err != nil {
			return err
		}
	}
	return nil
}

func tiers(ts []Tier, path string) error {
	if len(ts) > MaxTiers {
		return schema(path)
	}
	for i, t := range ts {
		p := path + "/" + strconv.Itoa(i)
		switch {
		case t.LowerInclusive == "":
			return schema(p + "/lower_inclusive")
		case t.UnitPrice == "":
			return schema(p + "/unit_price")
		case t.ZeroPriceReason != "" && !text(t.ZeroPriceReason, maxText):
			return schema(p + "/zero_price_reason")
		}
		if err := decimal(t.LowerInclusive, kernel.QuantityFormat, p+"/lower_inclusive"); err != nil {
			return err
		}
		if err := decimal(t.UpperExclusive, kernel.QuantityFormat, p+"/upper_exclusive"); err != nil {
			return err
		}
		if err := decimal(t.UnitPrice, kernel.PriceFormat, p+"/unit_price"); err != nil {
			return err
		}
	}
	return nil
}

func validity(from, until, path string) error {
	var f, u time.Time
	var err error
	if from != "" {
		if f, err = kernel.ParseTimestamp(from); err != nil {
			return schema(path + "/valid_from")
		}
	}
	if until != "" {
		if u, err = kernel.ParseTimestamp(until); err != nil {
			return schema(path + "/valid_until")
		}
	}
	if from != "" && until != "" && !kernel.Instant(u).After(kernel.Instant(f)) {
		return kernel.Reject(kernel.CodeInvalidKnownValue, path+"/valid_until", "A validity ends after it starts")
	}
	return nil
}

func partyRef(r *PartyRef, path string) error {
	if r != nil && (!kernel.ValidIdentifier(r.PartyID) || r.RowVersion < 1 || r.RowVersion > maxVersion) {
		return schema(path)
	}
	return nil
}

func rfqRef(r *RfqRef, path string) error {
	if r != nil && (!kernel.ValidIdentifier(r.RfqID) || r.RowVersion < 1 || r.RowVersion > maxVersion) {
		return schema(path)
	}
	return nil
}

func measurementRef(r *MeasurementRef, path string) error {
	if r != nil && (!kernel.ValidIdentifier(r.MeasurementID) || r.Revision < 1 || r.Revision > maxVersion) {
		return schema(path)
	}
	return nil
}

// configRef checks a POL reference of a field: a well-formed ConfigRef v1 of the field's kind (a
// reference of another kind is REFERENCE_INVALID, Python FIELD_KINDS).
func configRef(r *ConfigRef, kind, path string) error {
	if r == nil {
		return nil
	}
	switch {
	case !kernel.ValidIdentifier(r.Kind):
		return schema(path + "/kind")
	case len(r.ConfigID) > 40 || !kernel.ValidIdentifier(r.ConfigID):
		return schema(path + "/id")
	case r.Revision < 1 || r.Revision > maxVersion:
		return schema(path + "/revision")
	case r.Kind != kind:
		return kernel.Reject(kernel.CodeReferenceInvalid, path+"/kind", "The field refers to a "+kind)
	}
	return nil
}

// ids checks a list of identifiers: at most max, each well formed. Repeated IDs are a rule of the
// content (DUPLICATE_LINE_ID when sealed), not of the form.
func ids(list []string, max int, path string) error {
	if len(list) > max {
		return schema(path)
	}
	for i, id := range list {
		if !kernel.ValidIdentifier(id) {
			return schema(path + "/" + strconv.Itoa(i))
		}
	}
	return nil
}

func decimal(s string, f kernel.DecimalFormat, path string) error {
	if s == "" {
		return nil
	}
	if _, err := kernel.ParseDecimal(s, f); err != nil {
		return schema(path)
	}
	return nil
}

// integer reports whether s is a non-negative integer of at most 18 digits without leading zeros.
func integer(s string) bool {
	if s == "" || len(s) > maxExplicitDigit || (len(s) > 1 && s[0] == '0') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// token is a closed-value word such as a pricing mode: 1–32 of a–z and '_', starting with a letter.
// Whether SUP supports it is a rule of the content (UNSUPPORTED_PRICING_MODE when sealed).
func token(s string) bool {
	if len(s) < 1 || len(s) > 32 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < 'a' || s[i] > 'z') && s[i] != '_' {
			return false
		}
	}
	return true
}

// itemCode is a supplier's own item code as CAT matches it: 1–500 characters, no control
// characters, no white space at either end.
func itemCode(s string) bool {
	if !text(s, maxItemCode) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return false
	}
	return strings.TrimSpace(s) == s
}

// Location reports whether s is an exact location code: a UN/LOCODE of a two-letter country and a
// three-character place ("CNSHA"), as PTY's service roles carry it. Zones and routes are not
// locations (T02 §13.1).
func Location(s string) bool {
	if len(s) != 5 {
		return false
	}
	for i := 0; i < 5; i++ {
		c := s[i]
		upper := c >= 'A' && c <= 'Z'
		if !upper && (i < 2 || c < '2' || c > '9') {
			return false
		}
	}
	return true
}
