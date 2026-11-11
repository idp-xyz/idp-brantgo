package quote

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Address roles a quote addresses its customer by (Python t03_trade_parties
// QUOTE_CUSTOMER_ADDRESS_ROLES): at least one of the customer addresses is one of them.
var customerAddressRoles = []string{"buyer", "billing"}

// Kinds of the dependency manifest beyond the POL kinds: the RFQ (INQ RfqRef v1, hash of the
// complete line set), the cost snapshot (CST CostSnapshotRef v1, evidence hash of the snapshot
// document), the trading profile (PTY TradingProfileRef v1, content hash) and the evidence sources
// (EVD SourceRef v1, content SHA-256).
const (
	DependencyRfq            = "rfq"
	DependencyCostSnapshot   = "cost_snapshot"
	DependencyTradingProfile = "trading_profile"
	DependencySource         = "source"
)

// Values of a calculation (Python commercial.schema.json calculation).
const (
	MarginPass             = "pass"
	MarginRequiresApproval = "requires_approval"
	CostBasisNominalDirect = "nominal_direct_cost"
)

// Policy values the freeze judges.
const (
	SideSales          = "sales"
	SideSalesCharge    = "sales_charge"
	SubjectSKU         = "sku"
	SubjectFeeCategory = "fee_category"
	PurposeQuotation   = "quotation"
	DocumentTypeQuote  = "quote"
	QuoteRendererV010  = "quote-text/0.1.0"
)

// Terms is the commercial data of a frozen revision: the draft complete, with every member the
// commercial document requires present (charges and customer notes possibly empty) and the
// customer addresses in address ID order.
type Terms struct {
	Issuer           Ref          `json:"issuer_ref"`
	Customer         PartyRef     `json:"customer_ref"`
	Contact          ContactRef   `json:"contact_ref"`
	Profile          ProfileRef   `json:"trading_profile_ref"`
	Addresses        []AddressRef `json:"customer_address_refs"`
	Rfq              RfqRef       `json:"rfq_ref"`
	Snapshot         SnapshotRef  `json:"cost_snapshot_ref"`
	Lines            []Line       `json:"lines"`
	Charges          []Charge     `json:"charges"`
	Currency         Ref          `json:"currency_ref"`
	PricingPolicy    Ref          `json:"pricing_policy_ref"`
	SalesFX          *Ref         `json:"sales_fx_ref,omitempty"`
	TradeTerms       Ref          `json:"trade_terms_ref"`
	PaymentTerms     Ref          `json:"payment_terms_ref"`
	DocumentTemplate Ref          `json:"document_template_ref"`
	Sequence         Ref          `json:"sequence_ref"`
	ExpiresAt        string       `json:"expires_at"`
	CustomerNotes    string       `json:"customer_notes"`
	Evidence         []Evidence   `json:"evidence"`
}

// Commercial is the commercial document a freeze seals (Python commercial.schema.json, on TRD's
// references): what the customer is offered, the server calculation, the parties as they read
// then, and every upstream fact it rests on with its content hash. Its business hash is
// Digest(document) (canonical.go). Calculation is null until the S3-02 server calculation is wired
// into the freeze (Calculator in the application layer).
type Commercial struct {
	QuoteID              string         `json:"quote_id"`
	BusinessRevision     int64          `json:"business_revision"`
	IssuedAt             string         `json:"issued_at"`
	Data                 Terms          `json:"data"`
	Calculation          *Calculation   `json:"calculation"`
	PartySnapshots       PartySnapshots `json:"party_snapshots"`
	DependencyManifest   []Dependency   `json:"dependency_manifest"`
	NormalizationVersion string         `json:"normalization_version"`
}

// Calculation is the server-owned result of the S3-02 calculation a freeze seals (Python
// commercial.schema.json calculation): amounts in minor units of the quote currency (comparison
// amounts in the pricing policy's), as integer strings.
type Calculation struct {
	Lines                  []AmountLine `json:"lines"`
	Charges                []AmountLine `json:"charges"`
	NetMinor               string       `json:"net_minor"`
	TaxMinor               string       `json:"tax_minor"`
	PayableMinor           string       `json:"payable_minor"`
	ComparisonCurrency     Ref          `json:"comparison_currency_ref"`
	ComparisonRevenueMinor string       `json:"comparison_revenue_minor"`
	CostMinor              string       `json:"cost_minor"`
	ProfitMinor            string       `json:"profit_minor"`
	MarginNumerator        string       `json:"margin_numerator"`
	MarginDenominator      string       `json:"margin_denominator"`
	MarginRule             string       `json:"margin_rule"`
	CostBasis              string       `json:"cost_basis"`
	ExcludedCostCategories []string     `json:"excluded_cost_categories"`
	Payments               []Payment    `json:"payments"`
}

// AmountLine is the amounts of one line or charge.
type AmountLine struct {
	ID           string `json:"id"`
	NetMinor     string `json:"net_minor"`
	TaxMinor     string `json:"tax_minor"`
	PayableMinor string `json:"payable_minor"`
}

// Payment is one stage of the payment schedule with its amount.
type Payment struct {
	StageID     string `json:"stage_id"`
	Percent     string `json:"percent"`
	Trigger     string `json:"trigger"`
	Days        int    `json:"days"`
	DayBasis    string `json:"day_basis,omitempty"`
	Calendar    *Ref   `json:"calendar,omitempty"`
	AmountMinor string `json:"amount_minor"`
}

// PartySnapshots are the parties as the freeze read them: the issuer's company profile and the
// customer's addresses of the bound party revision.
type PartySnapshots struct {
	Issuer   IssuerSnapshot   `json:"issuer"`
	Customer CustomerSnapshot `json:"customer"`
}

// IssuerSnapshot is the company profile of the issuer.
type IssuerSnapshot struct {
	CompanyID      string `json:"company_id"`
	LegalName      string `json:"legal_name"`
	RegistrationNo string `json:"registration_no"`
	Country        string `json:"country"`
	Address        string `json:"address"`
}

// CustomerSnapshot is the customer party revision and the addresses the quote uses.
type CustomerSnapshot struct {
	PartyID       string            `json:"party_id"`
	PartyRevision int64             `json:"party_revision"`
	Addresses     []AddressSnapshot `json:"addresses"`
}

// AddressSnapshot is one customer address with its role.
type AddressSnapshot struct {
	AddressID string `json:"address_id"`
	Role      string `json:"role"`
	Address   string `json:"address"`
}

// Dependency is one entry of the dependency manifest (Python commercial.schema.json dependency).
type Dependency struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Version     int64  `json:"version"`
	ContentHash string `json:"content_hash"`
}

// Facts are what the upstream contexts said about the references of a draft, each in a
// transaction of its own before TRD's, at the freeze's instant. A reference that is absent does
// not exist or lies outside the caller's data scope. A Check is the upstream's verdict, nil when the
// reference serves new business.
type Facts struct {
	Company   string // the company of the caller's scope
	Party     *Party
	Contact   *Contact
	Profile   *Profile
	Addresses map[AddressRef]Address
	Rfq       *Rfq
	Specs     map[SkuRef]Verdict
	Configs   map[Ref]Config
	Snapshot  *Snapshot
	Sources   map[string]string // the EVD sources the caller reads, with their content SHA-256
}

// Verdict is an upstream verdict at a path of the request: nil when the reference serves new
// business.
type Verdict func(path string) error

func (c Verdict) at(path string) error {
	if c == nil {
		return nil
	}
	return c(path)
}

// Party is PTY's PartyRef v1 of the customer now.
type Party struct {
	Confirmed bool
	Revision  int64 // latest confirmed revision
	Customer  bool  // the party has the customer role
}

// Contact is a PTY contact now.
type Contact struct {
	PartyID    string
	RowVersion int64
	Confirmed  bool
}

// Profile is a published trading profile revision as PTY resolved it.
type Profile struct {
	Check         Verdict
	ContentHash   string
	PartyRevision int64
	PaymentTerms  Ref
	TradeTerms    Ref
	Contacts      []ContactRef
}

// Address is an address of a confirmed party revision as PTY resolved it.
type Address struct {
	Check   Verdict
	Role    string
	Address string
}

// Rfq is INQ's RfqRef v1 now.
type Rfq struct {
	RowVersion int64
	Ready      bool
	LinesHash  string
	Lines      []RfqLine
}

// RfqLine is one line of an RFQ.
type RfqLine struct {
	LineID       string
	SkuID        string
	SpecRevision int64
	Quantity     string
	UOM          string
}

// Snapshot is CST's ResolvedSnapshot of the bound CostSnapshotRef v1.
type Snapshot struct {
	Check        Verdict // CST's adoptability now
	RfqID        string
	RfqVersion   int64
	RfqLinesHash string
	Currency     string
	EvidenceHash string
	LineIDs      []string
}

// Config is what POL said about one configuration revision at the freeze's instant: its verdict,
// content hash and the data the freeze judges. Nested are the revisions it names itself (a pricing
// policy's or tax application's tax profile, a payment or trade terms calendar), which the freeze
// adopts and guards too.
type Config struct {
	Check          Verdict
	ContentHash    string
	Nested         []Ref
	CurrencyCode   string          // currency
	Policy         *Policy         // pricing_policy
	UnitCode       string          // unit
	Conversion     *Conversion     // unit_conversion
	TaxApplication *TaxApplication // tax_application
	FeeSide        string          // fee_category
	FX             *FX             // fx_rate
	SequenceType   string          // number_sequence: its document type
	Renderer       string          // quote_document_template: its renderer version
	Company        *IssuerSnapshot // company_profile
}

// Policy is a POL pricing policy.
type Policy struct {
	CalculationCurrency string
	QuoteValidDays      int
}

// Conversion is a POL unit conversion: one From is Factor To, for one SKU specification revision.
type Conversion struct {
	SkuID        string
	SpecRevision int64
	From, To     string
	Factor       string
}

// TaxApplication is a POL tax application.
type TaxApplication struct {
	Side        string
	SubjectKind string
	SubjectID   string
	PartyID     string
}

// FX is a POL exchange rate's direction and purpose.
type FX struct {
	From, To, Purpose string
}

// Seal is what a freeze fixes: the normalized canonical commercial document and its business
// hash, issued_at and expires_at, and what the transaction guards (the POL revisions adopted, the
// RFQ binding and the cost snapshot).
type Seal struct {
	Commercial   []byte
	BusinessHash string
	IssuedAt     time.Time
	ExpiresAt    time.Time
	Configs      []Dependency // POL revisions, direct and nested, in kind, ID and revision order
	Rfq          Dependency
	Snapshot     SnapshotRef
	By           kernel.ActorRef
	Epoch        int64
	At           time.Time
	RootVersion  int64 // the root version the freeze committed at
}

// Complete refuses a draft that lacks what a freeze needs, at the first missing member in document
// order: the customer and contact references have their own codes (CUSTOMER_REFERENCE_MISSING,
// CONTACT_REFERENCE_MISSING), the cost snapshot COST_SNAPSHOT_MISSING, the rest DRAFT_NOT_READY.
func (d Data) Complete() error {
	const p = "/args/data"
	missing := func(path string) error { return kernel.Reject(kernel.CodeDraftNotReady, p+path, "") }
	switch {
	case d.Issuer == nil:
		return missing("/issuer_ref")
	case d.Customer == nil:
		return fail(CodeCustomerReference, p+"/customer_ref", "")
	case d.Contact == nil:
		return fail(CodeContactReference, p+"/contact_ref", "")
	case d.Profile == nil:
		return missing("/trading_profile_ref")
	case len(d.Addresses) == 0:
		return missing("/customer_address_refs")
	case d.Rfq == nil:
		return missing("/rfq_ref")
	case d.Snapshot == nil:
		return fail(CodeCostSnapshotMissing, p+"/cost_snapshot_ref", "")
	case len(d.Lines) == 0:
		return missing("/lines")
	case d.Currency == nil:
		return missing("/currency_ref")
	case d.PricingPolicy == nil:
		return missing("/pricing_policy_ref")
	case d.TradeTerms == nil:
		return missing("/trade_terms_ref")
	case d.PaymentTerms == nil:
		return missing("/payment_terms_ref")
	case d.DocumentTemplate == nil:
		return missing("/document_template_ref")
	case d.Sequence == nil:
		return missing("/sequence_ref")
	case d.ExpiresAt == "":
		return missing("/expires_at")
	case len(d.Evidence) == 0:
		return missing("/evidence")
	}
	return nil
}

// Seal judges a complete draft against facts for a freeze of revision rev of quote id at instant
// now (the freeze's issued_at), with calc the server calculation (nil until S3-02 is wired), and
// returns its commercial document and business hash. Checks run in this order: completeness, the
// draft's own rules and evidence, then each reference known and in effect (customer, contact,
// trading profile, addresses, RFQ, specifications, configurations, cost snapshot), then the
// content (RFQ coverage and base quantities, tax applications, charges, exchange rate, terms,
// template and sequence, the snapshot's binding), then the validity window (TRD-INV-005).
func SealDraft(id string, rev int64, d Data, f Facts, calc *Calculation, now time.Time) (*Seal, error) {
	if err := d.Complete(); err != nil {
		return nil, err
	}
	readableSources := map[string]bool{}
	for s := range f.Sources {
		readableSources[s] = true
	}
	if err := CheckDraft(d, readableSources); err != nil {
		return nil, err
	}
	if err := references(d, f); err != nil {
		return nil, err
	}
	if err := content(d, f); err != nil {
		return nil, err
	}
	issued := kernel.Instant(now)
	expires, err := validity(d, f, issued)
	if err != nil {
		return nil, err
	}
	terms := sealedTerms(d)
	if calc != nil {
		if err = calc.Check(terms); err != nil {
			return nil, err
		}
	}
	configs := closure(d, f)
	doc := Commercial{QuoteID: id, BusinessRevision: rev, IssuedAt: kernel.FormatTimestamp(issued), Data: terms, Calculation: calc,
		PartySnapshots: parties(terms, f), NormalizationVersion: NormalizationVersion}
	doc.DependencyManifest = append(doc.DependencyManifest, configs...)
	rfq := Dependency{Kind: DependencyRfq, ID: terms.Rfq.RfqID, Version: terms.Rfq.RowVersion, ContentHash: f.Rfq.LinesHash}
	doc.DependencyManifest = append(doc.DependencyManifest, rfq,
		Dependency{Kind: DependencyCostSnapshot, ID: terms.Snapshot.SnapshotID, Version: 1, ContentHash: f.Snapshot.EvidenceHash},
		Dependency{Kind: DependencyTradingProfile, ID: terms.Profile.PartyID, Version: terms.Profile.Revision, ContentHash: f.Profile.ContentHash})
	for _, s := range d.Sources() {
		doc.DependencyManifest = append(doc.DependencyManifest, Dependency{Kind: DependencySource, ID: s, Version: 1, ContentHash: f.Sources[s]})
	}
	for i, dep := range doc.DependencyManifest {
		if !hexHash.MatchString(dep.ContentHash) {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, fmt.Sprintf("/dependency_manifest/%d/content_hash", i),
				"An upstream fact has no content hash")
		}
	}
	tree, err := TreeOf(doc)
	if err != nil {
		return nil, err
	}
	canonical, hash, err := Digest(tree)
	if err != nil {
		return nil, err
	}
	return &Seal{Commercial: canonical, BusinessHash: hash, IssuedAt: issued, ExpiresAt: expires, Configs: configs, Rfq: rfq,
		Snapshot: terms.Snapshot}, nil
}

var hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// references judges every reference of a complete draft as its owner answered.
func references(d Data, f Facts) error {
	const p = "/args/data"
	if d.Issuer.ID != f.Company {
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/issuer_ref/id", "The issuer is the company of the scope")
	}
	switch party := f.Party; {
	case party == nil || !party.Customer:
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/customer_ref", "")
	case !party.Confirmed || party.Revision != d.Customer.Revision:
		return fail(CodeReferenceStale, p+"/customer_ref", "The customer is not confirmed in the bound revision")
	}
	switch c := f.Contact; {
	case c == nil || c.PartyID != d.Customer.PartyID:
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/contact_ref", "")
	case !c.Confirmed || c.RowVersion != d.Contact.RowVersion:
		return fail(CodeReferenceStale, p+"/contact_ref", "The contact is not confirmed at the bound row version")
	}
	pr := f.Profile
	if pr == nil || d.Profile.PartyID != d.Customer.PartyID {
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/trading_profile_ref", "")
	}
	if err := pr.Check.at(p + "/trading_profile_ref"); err != nil {
		return err
	}
	if pr.PartyRevision != d.Customer.Revision {
		return fail(CodeReferenceStale, p+"/trading_profile_ref", "The trading profile binds another party revision")
	}
	if !slices.Contains(pr.Contacts, *d.Contact) {
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/contact_ref", "The trading profile does not pin the contact")
	}
	roles := map[string]bool{}
	for i, a := range d.Addresses {
		path := fmt.Sprintf("%s/customer_address_refs/%d", p, i)
		r, ok := f.Addresses[a]
		if !ok || a.PartyID != d.Customer.PartyID || a.PartyRevision != d.Customer.Revision {
			return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
		}
		if err := r.Check.at(path); err != nil {
			return err
		}
		if roles[r.Role] {
			return kernel.Reject(kernel.CodeInvalidKnownValue, path, "One address per role")
		}
		roles[r.Role] = true
	}
	if !slices.ContainsFunc(customerAddressRoles, func(r string) bool { return roles[r] }) {
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/customer_address_refs", "A buyer or billing address addresses the quote")
	}
	switch r := f.Rfq; {
	case r == nil:
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/rfq_ref", "")
	case !r.Ready || r.RowVersion != d.Rfq.RowVersion:
		return fail(CodeReferenceStale, p+"/rfq_ref", "The RFQ is not ready at the bound row version")
	}
	for i, l := range d.Lines {
		path := fmt.Sprintf("%s/lines/%d/sku_ref", p, i)
		check, ok := f.Specs[l.Sku]
		if !ok {
			return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
		}
		if err := check.at(path); err != nil {
			return err
		}
	}
	for _, field := range d.Fields() {
		c, ok := f.Configs[field.Ref]
		if !ok {
			return kernel.Reject(kernel.CodeReferenceInvalid, field.Path, "")
		}
		if err := c.Check.at(field.Path); err != nil {
			return err
		}
		for _, n := range c.Nested {
			nc, ok := f.Configs[n]
			if !ok {
				return kernel.Reject(kernel.CodeReferenceInvalid, field.Path, "A configuration it names is unknown")
			}
			if err := nc.Check.at(field.Path); err != nil {
				return err
			}
		}
	}
	if f.Snapshot == nil {
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/cost_snapshot_ref", "")
	}
	return f.Snapshot.Check.at(p + "/cost_snapshot_ref")
}

// content judges what the references say together.
func content(d Data, f Facts) error {
	const p = "/args/data"
	if err := cover(d, f); err != nil {
		return err
	}
	for i, l := range d.Lines {
		path := fmt.Sprintf("%s/lines/%d/tax_application_ref", p, i)
		if err := taxApplication(f.Configs[l.TaxApplication].TaxApplication, SubjectSKU, l.Sku.SkuID, d.Customer.PartyID, path); err != nil {
			return err
		}
	}
	obligations := map[string]bool{}
	for i, c := range d.Charges {
		cp := fmt.Sprintf("%s/charges/%d", p, i)
		if f.Configs[c.FeeCategory].FeeSide != SideSalesCharge {
			return kernel.Reject(kernel.CodeReferenceInvalid, cp+"/fee_category_ref", "The fee category is not a sales charge")
		}
		if obligations[c.ObligationKey] {
			return fail(CodeChargeScopeUnresolved, cp+"/obligation_key", "The obligation is charged once")
		}
		obligations[c.ObligationKey] = true
		if err := taxApplication(f.Configs[c.TaxApplication].TaxApplication, SubjectFeeCategory, c.FeeCategory.ID, d.Customer.PartyID,
			cp+"/tax_application_ref"); err != nil {
			return err
		}
	}
	policy := f.Configs[*d.PricingPolicy].Policy
	currency := f.Configs[*d.Currency].CurrencyCode
	if policy == nil || currency == "" {
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/pricing_policy_ref", "")
	}
	switch {
	case currency == policy.CalculationCurrency && d.SalesFX != nil:
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/sales_fx_ref", "A quote in the calculation currency takes no exchange rate")
	case currency != policy.CalculationCurrency && d.SalesFX == nil:
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/sales_fx_ref", "A quote in another currency names its quotation rate")
	case d.SalesFX != nil:
		fx := f.Configs[*d.SalesFX].FX
		if fx == nil || fx.From != currency || fx.To != policy.CalculationCurrency || fx.Purpose != PurposeQuotation {
			return fail(CodeFXScopeMismatch, p+"/sales_fx_ref", "The rate converts the quote currency into the calculation currency for quotation")
		}
	}
	pr := f.Profile
	if *d.TradeTerms != pr.TradeTerms {
		return fail(CodeComparisonBasisMismatch, p+"/trade_terms_ref", "The trade terms are the trading profile's")
	}
	if *d.PaymentTerms != pr.PaymentTerms {
		return fail(CodeComparisonBasisMismatch, p+"/payment_terms_ref", "The payment terms are the trading profile's")
	}
	if f.Configs[*d.DocumentTemplate].Renderer != QuoteRendererV010 {
		return fail(CodeDependencyNotSupported, p+"/document_template_ref", "The template's renderer is not supported")
	}
	if f.Configs[*d.Sequence].SequenceType != DocumentTypeQuote {
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/sequence_ref", "The number sequence does not number quotes")
	}
	if f.Configs[*d.Issuer].Company == nil {
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/issuer_ref", "")
	}
	s := f.Snapshot
	switch {
	case s.RfqID != d.Rfq.RfqID || s.RfqVersion != f.Rfq.RowVersion || s.RfqLinesHash != f.Rfq.LinesHash:
		return fail(CodeComparisonBasisMismatch, p+"/cost_snapshot_ref", "The cost snapshot costs another RFQ binding")
	case s.Currency != policy.CalculationCurrency:
		return fail(CodeComparisonBasisMismatch, p+"/cost_snapshot_ref", "The cost snapshot is in another currency than the pricing policy's")
	}
	var rfqLines []string
	for _, l := range f.Rfq.Lines {
		rfqLines = append(rfqLines, l.LineID)
	}
	if !slices.Equal(slices.Sorted(slices.Values(s.LineIDs)), slices.Sorted(slices.Values(rfqLines))) {
		return fail(CodeComparisonBasisMismatch, p+"/cost_snapshot_ref", "The cost snapshot covers another line set")
	}
	return nil
}

// cover checks the lines against the RFQ's complete line set (TRD-INV-006): each RFQ line exactly
// once, at its SKU and specification, and its quantity in the RFQ's unit equal to the RFQ's, through
// a conversion of the line's SKU specification when the quoted unit differs.
func cover(d Data, f Facts) error {
	const p = "/args/data/lines"
	rfq := map[string]RfqLine{}
	for _, l := range f.Rfq.Lines {
		rfq[l.LineID] = l
	}
	if len(d.Lines) != len(f.Rfq.Lines) {
		return fail(CodeComparisonBasisMismatch, p, "The quote covers the RFQ's complete line set")
	}
	for i, l := range d.Lines {
		path := fmt.Sprintf("%s/%d", p, i)
		r, ok := rfq[l.RfqLineID]
		if !ok {
			return fail(CodeComparisonBasisMismatch, path+"/rfq_line_id", "The quote covers the RFQ's complete line set")
		}
		if l.Sku.SkuID != r.SkuID || l.Sku.SpecRevision != r.SpecRevision {
			return fail(CodeComparisonBasisMismatch, path+"/sku_ref", "SKU and specification are the RFQ line's")
		}
		unit := f.Configs[l.Unit].UnitCode
		if unit == "" {
			return kernel.Reject(kernel.CodeReferenceInvalid, path+"/unit_ref", "")
		}
		q, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if err != nil {
			return kernel.Reject(kernel.CodeSchemaInvalid, path+"/quantity", "")
		}
		want, err := kernel.ParseDecimal(r.Quantity, kernel.QuantityFormat)
		if err != nil {
			return kernel.Reject(kernel.CodeReferenceInvalid, path+"/rfq_line_id", "The RFQ line has no quantity")
		}
		base := q
		switch {
		case unit == r.UOM && l.Conversion != nil:
			return kernel.Reject(kernel.CodeReferenceInvalid, path+"/conversion_ref", "A line in the RFQ's unit takes no conversion")
		case unit != r.UOM:
			if l.Conversion == nil {
				return kernel.Reject(kernel.CodeReferenceInvalid, path+"/conversion_ref", "A line in another unit than the RFQ's names its conversion")
			}
			c := f.Configs[*l.Conversion].Conversion
			if c == nil || c.SkuID != l.Sku.SkuID || c.SpecRevision != l.Sku.SpecRevision {
				return kernel.Reject(kernel.CodeReferenceInvalid, path+"/conversion_ref", "The conversion is not of this SKU specification")
			}
			factor, err := kernel.ParseDecimal(c.Factor, kernel.FactorFormat)
			if err != nil || factor.Sign() <= 0 {
				return kernel.Reject(kernel.CodeReferenceInvalid, path+"/conversion_ref", "")
			}
			switch {
			case c.From == unit && c.To == r.UOM:
				base = q.Mul(factor)
			case c.From == r.UOM && c.To == unit:
				if base, err = q.Quo(factor); err != nil {
					return kernel.Reject(kernel.CodeReferenceInvalid, path+"/conversion_ref", "")
				}
			default:
				return kernel.Reject(kernel.CodeReferenceInvalid, path+"/conversion_ref", "The conversion is not between the quoted and the RFQ's unit")
			}
		}
		if !base.Equal(want) {
			return fail(CodeBaseQuantityMismatch, path+"/quantity", "The quantity in the RFQ's unit is the RFQ line's")
		}
	}
	return nil
}

func taxApplication(t *TaxApplication, subjectKind, subjectID, partyID, path string) error {
	switch {
	case t == nil || t.Side != SideSales:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "Only a sales tax application answers for a sale")
	case t.SubjectKind != subjectKind || t.SubjectID != subjectID:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "The tax application is for another subject")
	case t.PartyID != "" && t.PartyID != partyID:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "The tax application is for another party")
	}
	return nil
}

// validity checks issued < expires ≤ issued + quote_valid_days × 86400 s (TRD-INV-005, Python
// _seal_quote_timing): QUOTE_EXPIRED when the quote would expire at or before it is issued,
// QUOTE_VALIDITY_INVALID beyond the pricing policy's validity.
func validity(d Data, f Facts, issued time.Time) (time.Time, error) {
	const p = "/args/data/expires_at"
	expires, err := kernel.ParseTimestamp(d.ExpiresAt)
	if err != nil {
		return time.Time{}, kernel.Reject(kernel.CodeSchemaInvalid, p, "")
	}
	expires = kernel.Instant(expires)
	days := f.Configs[*d.PricingPolicy].Policy.QuoteValidDays
	if days <= 0 {
		return time.Time{}, fail(CodeQuoteValidityInvalid, "/args/data/pricing_policy_ref", "The pricing policy bounds no validity")
	}
	if !expires.After(issued) {
		return time.Time{}, fail(CodeQuoteExpired, p, "The quote expires at or before it is issued")
	}
	if expires.After(issued.Add(time.Duration(days) * 24 * time.Hour)) {
		return time.Time{}, fail(CodeQuoteValidityInvalid, p, fmt.Sprintf("The quote is valid at most %d days", days))
	}
	return expires, nil
}

// sealedTerms returns the commercial data of a complete draft: required members present, addresses
// in address ID order (lines, charges and evidence are sorted by the normalization).
func sealedTerms(d Data) Terms {
	t := Terms{Issuer: *d.Issuer, Customer: *d.Customer, Contact: *d.Contact, Profile: *d.Profile,
		Addresses: slices.Clone(d.Addresses), Rfq: *d.Rfq, Snapshot: *d.Snapshot, Lines: slices.Clone(d.Lines),
		Charges: slices.Clone(d.Charges), Currency: *d.Currency, PricingPolicy: *d.PricingPolicy, SalesFX: d.SalesFX,
		TradeTerms: *d.TradeTerms, PaymentTerms: *d.PaymentTerms, DocumentTemplate: *d.DocumentTemplate, Sequence: *d.Sequence,
		ExpiresAt: d.ExpiresAt, Evidence: slices.Clone(d.Evidence)}
	if t.Charges == nil {
		t.Charges = []Charge{}
	}
	if d.CustomerNotes != nil {
		t.CustomerNotes = *d.CustomerNotes
	}
	slices.SortFunc(t.Addresses, func(a, b AddressRef) int { return cmp.Compare(a.AddressID, b.AddressID) })
	return t
}

// closure returns the POL revisions the draft adopts, directly or through the revisions it names,
// with their content hashes, in kind, ID and revision order.
func closure(d Data, f Facts) []Dependency {
	refs := d.ConfigRefs()
	for _, r := range d.ConfigRefs() {
		refs = append(refs, f.Configs[r].Nested...)
	}
	var out []Dependency
	for _, r := range sortedRefs(refs) {
		out = append(out, Dependency{Kind: r.Kind, ID: r.ID, Version: r.Revision, ContentHash: f.Configs[r].ContentHash})
	}
	return out
}

func parties(t Terms, f Facts) PartySnapshots {
	ps := PartySnapshots{Issuer: *f.Configs[t.Issuer].Company,
		Customer: CustomerSnapshot{PartyID: t.Customer.PartyID, PartyRevision: t.Customer.Revision, Addresses: []AddressSnapshot{}}}
	ps.Issuer.CompanyID = t.Issuer.ID
	for _, a := range t.Addresses {
		r := f.Addresses[a]
		ps.Customer.Addresses = append(ps.Customer.Addresses, AddressSnapshot{AddressID: a.AddressID, Role: r.Role, Address: r.Address})
	}
	return ps
}

var minor = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,23})$`)
var signedMinor = regexp.MustCompile(`^(?:0|-?[1-9][0-9]{0,23})$`)

// Check verifies the form of a server calculation against the terms it computed before a freeze
// seals it: one amount line per quote line and per charge, amounts in minor units, net + tax =
// payable, the payments adding up to the payable amount, and a known margin rule and cost basis.
// The arithmetic is S3-02's (TRD-INV-007, TRD-INV-008); this only refuses a calculation that does
// not fit the revision.
func (c Calculation) Check(t Terms) error {
	const p = "/calculation"
	bad := func(path string) error {
		return kernel.Reject(kernel.CodeInvalidKnownValue, p+path, "The server calculation does not fit the revision")
	}
	ids := func(lines []AmountLine) []string {
		var out []string
		for _, l := range lines {
			out = append(out, l.ID)
		}
		slices.Sort(out)
		return out
	}
	var lineIDs, chargeIDs []string
	for _, l := range t.Lines {
		lineIDs = append(lineIDs, l.LineID)
	}
	for _, ch := range t.Charges {
		chargeIDs = append(chargeIDs, ch.ChargeID)
	}
	slices.Sort(lineIDs)
	slices.Sort(chargeIDs)
	if !slices.Equal(ids(c.Lines), lineIDs) {
		return bad("/lines")
	}
	if !slices.Equal(ids(c.Charges), chargeIDs) {
		return bad("/charges")
	}
	for _, l := range append(slices.Clone(c.Lines), c.Charges...) {
		if !sums(l.NetMinor, l.TaxMinor, l.PayableMinor) {
			return bad("/lines")
		}
	}
	switch {
	case !sums(c.NetMinor, c.TaxMinor, c.PayableMinor):
		return bad("/payable_minor")
	case !minor.MatchString(c.ComparisonRevenueMinor) || !minor.MatchString(c.CostMinor) || !minor.MatchString(c.MarginDenominator):
		return bad("/comparison_revenue_minor")
	case !signedMinor.MatchString(c.ProfitMinor) || !signedMinor.MatchString(c.MarginNumerator):
		return bad("/profit_minor")
	case c.MarginRule != MarginPass && c.MarginRule != MarginRequiresApproval:
		return bad("/margin_rule")
	case c.CostBasis != CostBasisNominalDirect:
		return bad("/cost_basis")
	case c.ExcludedCostCategories == nil || len(c.Payments) == 0:
		return bad("/payments")
	}
	total := new(big.Int)
	for _, pay := range c.Payments {
		if !minor.MatchString(pay.AmountMinor) {
			return bad("/payments")
		}
		total.Add(total, integerOf(pay.AmountMinor))
	}
	if total.Cmp(integerOf(c.PayableMinor)) != 0 {
		return bad("/payments")
	}
	return nil
}

// sums reports whether net and tax are minor amounts adding up to payable.
func sums(net, tax, payable string) bool {
	if !minor.MatchString(net) || !minor.MatchString(tax) || !minor.MatchString(payable) {
		return false
	}
	return new(big.Int).Add(integerOf(net), integerOf(tax)).Cmp(integerOf(payable)) == 0
}

// integerOf parses a minor amount the caller matched against minor.
func integerOf(s string) *big.Int {
	n, _ := new(big.Int).SetString(s, 10)
	return n
}

// ParseCommercial decodes a sealed commercial document after checking it hashes to hash
// (BUSINESS_HASH_MISMATCH otherwise): a stored document that does not hash to its business hash is
// corrupt and never used.
func ParseCommercial(doc []byte, hash string) (Commercial, error) {
	if Hash(doc) != hash {
		return Commercial{}, fail(CodeBusinessHashMismatch, "/business_hash", "The commercial document does not hash to its business hash")
	}
	var c Commercial
	if err := json.Unmarshal(doc, &c); err != nil {
		return Commercial{}, err
	}
	return c, nil
}
