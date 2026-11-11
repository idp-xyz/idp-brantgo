package quote

import (
	"fmt"
	"slices"
	"strconv"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// POL kinds each reference of a quote takes (Context Map R15, ConfigRef v1).
const (
	KindCompanyProfile        = "company_profile"
	KindCurrency              = "currency"
	KindUnit                  = "unit"
	KindUnitConversion        = "unit_conversion"
	KindFXRate                = "fx_rate"
	KindTradeTerms            = "trade_terms"
	KindPaymentTerms          = "payment_terms"
	KindTaxApplication        = "tax_application"
	KindTaxProfile            = "tax_profile"
	KindFeeCategory           = "fee_category"
	KindPricingPolicy         = "pricing_policy"
	KindNumberSequence        = "number_sequence"
	KindQuoteDocumentTemplate = "quote_document_template"
	KindCalendar              = "calendar"
)

// Charge inclusions.
const (
	InclusionSeparate       = "separate"
	InclusionIncludedInLine = "included_in_line"
)

// Ref is a POL ConfigRef v1: a configuration kind, its ID and the exact revision adopted.
type Ref struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

func (r Ref) String() string { return r.Kind + ":" + r.ID + "@" + strconv.FormatInt(r.Revision, 10) }

// Compare orders references by kind, ID and revision.
func (r Ref) Compare(o Ref) int {
	if c := compareString(r.Kind, o.Kind); c != 0 {
		return c
	}
	if c := compareString(r.ID, o.ID); c != 0 {
		return c
	}
	return compareInt(r.Revision, o.Revision)
}

// PartyRef is PTY PartyRef v1 as a quote binds it: the customer party at its confirmed revision,
// which never changes (Context Map R13).
type PartyRef struct {
	PartyID  string `json:"party_id"`
	Revision int64  `json:"revision"`
}

// ContactRef is a PTY contact of the customer at the row version the quote binds.
type ContactRef struct {
	ContactID  string `json:"contact_id"`
	RowVersion int64  `json:"row_version"`
}

// ProfileRef is PTY TradingProfileRef v1: one published revision of the customer's trading
// profile in the company.
type ProfileRef struct {
	PartyID  string `json:"party_id"`
	Revision int64  `json:"revision"`
}

// AddressRef is PTY AddressRoleRef v1: an address of the customer as its confirmed revision keeps
// it. Its role is the one PTY declared with the address.
type AddressRef struct {
	PartyID       string `json:"party_id"`
	PartyRevision int64  `json:"party_revision"`
	AddressID     string `json:"address_id"`
}

// RfqRef is INQ RfqRef v1 as a draft names it: the RFQ and the row version it covers (Context Map
// R12). The freeze binds the hash of its complete line set in the dependency manifest.
type RfqRef struct {
	RfqID      string `json:"rfq_id"`
	RowVersion int64  `json:"row_version"`
}

// SnapshotRef is CST CostSnapshotRef v1: a frozen cost snapshot and its result hash (Context Map
// R11).
type SnapshotRef struct {
	SnapshotID string `json:"snapshot_id"`
	ResultHash string `json:"result_hash"`
}

// SkuRef is a CAT SKU at a confirmed specification revision (Context Map R14).
type SkuRef struct {
	SkuID        string `json:"sku_id"`
	SpecRevision int64  `json:"spec_revision"`
}

// BusinessRef names a revision of a business object no Brant context owns yet: a place (kind
// location) or a delivery anchor (kind delivery_anchor), compared as an identity (CST BusinessRef).
type BusinessRef struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// Delivery is the customer's delivery requirement as a quote states it and as the adopted costing
// compared candidates on it (CST Delivery): a milestone at a place in an IANA time zone, on a
// fixed date or Days (calendar or working) days after the event Anchor names, counted on a POL
// calendar. Days of 0 are omitted from the JSON.
type Delivery struct {
	Kind      string       `json:"kind"`
	Date      string       `json:"date,omitempty"`
	Anchor    *BusinessRef `json:"anchor_ref,omitempty"`
	Days      int          `json:"days,omitempty"`
	DayBasis  string       `json:"day_basis,omitempty"`
	Calendar  *Ref         `json:"calendar_ref,omitempty"`
	Milestone string       `json:"milestone"`
	Place     BusinessRef  `json:"place_ref"`
	TimeZone  string       `json:"time_zone"`
}

// Evidence is an EVD source and the place in it a human relied on (Context Map R16).
type Evidence struct {
	SourceID string `json:"source_id"`
	Locator  string `json:"locator"`
}

// Line is one quoted line: an RFQ line at its SKU specification, the quantity in the quoted unit
// (converted to the costing's unit by Conversion when the units differ), the quantity in the
// costing's unit when the author states it (BaseQuantity, "" otherwise), the price per
// PriceBasisQuantity units, the discount and the sales tax application. Decimals are strings in
// their contract formats, never floats.
type Line struct {
	LineID             string     `json:"line_id"`
	RfqLineID          string     `json:"rfq_line_id"`
	Sku                SkuRef     `json:"sku_ref"`
	Quantity           string     `json:"quantity"`
	Unit               Ref        `json:"unit_ref"`
	Conversion         *Ref       `json:"conversion_ref,omitempty"`
	BaseQuantity       string     `json:"base_quantity,omitempty"`
	UnitPrice          string     `json:"unit_price"`
	PriceBasisQuantity string     `json:"price_basis_quantity"`
	DiscountRatio      string     `json:"discount_ratio"`
	TaxApplication     Ref        `json:"tax_application_ref"`
	Evidence           []Evidence `json:"evidence"`
}

// Charge is a sales charge: a fee category, the obligation it settles (counted once), its amount in
// the quote currency, whether it is charged separately or included in a line, and its sales tax
// application.
type Charge struct {
	ChargeID       string     `json:"charge_id"`
	FeeCategory    Ref        `json:"fee_category_ref"`
	ObligationKey  string     `json:"obligation_key"`
	Amount         string     `json:"amount"`
	Inclusion      string     `json:"inclusion"`
	IncludedLineID string     `json:"included_line_id,omitempty"`
	TaxApplication Ref        `json:"tax_application_ref"`
	Evidence       []Evidence `json:"evidence"`
}

// Data is the content of a working revision as saved, whole (quote.save_draft). Every member is
// optional in a draft; the freeze requires what Complete names. A line or charge, once present,
// is complete (the contract checks its form). ExpiresAt is stored in kernel.FormatTimestamp form.
// CustomerNotes are customer-visible and part of the commercial content; internal labels and
// notes live on the root (quote.metadata.update) and never enter the business hash.
type Data struct {
	Issuer           *Ref         `json:"issuer_ref,omitempty"`
	Customer         *PartyRef    `json:"customer_ref,omitempty"`
	Contact          *ContactRef  `json:"contact_ref,omitempty"`
	Profile          *ProfileRef  `json:"trading_profile_ref,omitempty"`
	Addresses        []AddressRef `json:"customer_address_refs,omitempty"`
	Rfq              *RfqRef      `json:"rfq_ref,omitempty"`
	Snapshot         *SnapshotRef `json:"cost_snapshot_ref,omitempty"`
	Lines            []Line       `json:"lines,omitempty"`
	Charges          []Charge     `json:"charges,omitempty"`
	Currency         *Ref         `json:"currency_ref,omitempty"`
	PricingPolicy    *Ref         `json:"pricing_policy_ref,omitempty"`
	SalesFX          *Ref         `json:"sales_fx_ref,omitempty"`
	TradeTerms       *Ref         `json:"trade_terms_ref,omitempty"`
	NamedPlace       *BusinessRef `json:"named_place_ref,omitempty"`
	PaymentTerms     *Ref         `json:"payment_terms_ref,omitempty"`
	Delivery         *Delivery    `json:"delivery,omitempty"`
	DocumentTemplate *Ref         `json:"document_template_ref,omitempty"`
	Sequence         *Ref         `json:"sequence_ref,omitempty"`
	ExpiresAt        string       `json:"expires_at,omitempty"`
	CustomerNotes    *string      `json:"customer_notes,omitempty"`
	Evidence         []Evidence   `json:"evidence,omitempty"`
}

// Hash returns the content hash of the draft as saved: the SHA-256 of its canonical JSON (the
// normalization of a commercial document does not apply to a draft).
func (d Data) Hash() string {
	tree, err := TreeOf(d)
	if err == nil {
		var doc []byte
		if doc, err = Canonical(tree); err == nil {
			return Hash(doc)
		}
	}
	panic(fmt.Sprintf("quote: draft does not encode: %v", err)) // the contract admits only valid UTF-8 strings and integers
}

// Field is a POL reference of the draft with the kind its field takes and its path in a request.
type Field struct {
	Ref  Ref
	Kind string
	Path string
}

// Fields returns every POL reference of d in document order.
func (d Data) Fields() []Field {
	const p = "/args/data"
	var out []Field
	add := func(r *Ref, kind, path string) {
		if r != nil {
			out = append(out, Field{Ref: *r, Kind: kind, Path: path})
		}
	}
	add(d.Issuer, KindCompanyProfile, p+"/issuer_ref")
	for i := range d.Lines {
		l := &d.Lines[i]
		lp := fmt.Sprintf("%s/lines/%d", p, i)
		add(&l.Unit, KindUnit, lp+"/unit_ref")
		add(l.Conversion, KindUnitConversion, lp+"/conversion_ref")
		add(&l.TaxApplication, KindTaxApplication, lp+"/tax_application_ref")
	}
	for i := range d.Charges {
		c := &d.Charges[i]
		cp := fmt.Sprintf("%s/charges/%d", p, i)
		add(&c.FeeCategory, KindFeeCategory, cp+"/fee_category_ref")
		add(&c.TaxApplication, KindTaxApplication, cp+"/tax_application_ref")
	}
	add(d.Currency, KindCurrency, p+"/currency_ref")
	add(d.PricingPolicy, KindPricingPolicy, p+"/pricing_policy_ref")
	add(d.SalesFX, KindFXRate, p+"/sales_fx_ref")
	add(d.TradeTerms, KindTradeTerms, p+"/trade_terms_ref")
	add(d.PaymentTerms, KindPaymentTerms, p+"/payment_terms_ref")
	if d.Delivery != nil {
		add(d.Delivery.Calendar, KindCalendar, p+"/delivery/calendar_ref")
	}
	add(d.DocumentTemplate, KindQuoteDocumentTemplate, p+"/document_template_ref")
	add(d.Sequence, KindNumberSequence, p+"/sequence_ref")
	return out
}

// ConfigRefs returns the distinct POL references of d in kind, ID and revision order.
func (d Data) ConfigRefs() []Ref {
	var refs []Ref
	for _, f := range d.Fields() {
		refs = append(refs, f.Ref)
	}
	return sortedRefs(refs)
}

func sortedRefs(refs []Ref) []Ref {
	out := slices.Clone(refs)
	slices.SortFunc(out, Ref.Compare)
	return slices.Compact(out)
}

// Sources returns the distinct EVD sources every evidence entry of d cites, in ID order.
func (d Data) Sources() []string {
	var ids []string
	for _, e := range d.Evidence {
		ids = append(ids, e.SourceID)
	}
	for _, l := range d.Lines {
		for _, e := range l.Evidence {
			ids = append(ids, e.SourceID)
		}
	}
	for _, c := range d.Charges {
		for _, e := range c.Evidence {
			ids = append(ids, e.SourceID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// CheckDraft judges a draft at quote.save_draft beyond its form (Python _validate_quote_evidence
// and the closed schema): line and charge IDs once each (DUPLICATE_LINE_ID), each POL reference of
// the kind its field takes, an included charge naming a line of the draft, and every evidence
// source one the caller reads (sources: the readable ones). Whether references exist and are in
// force is decided at the freeze.
func CheckDraft(d Data, sources map[string]bool) error {
	const p = "/args/data"
	lines := map[string]bool{}
	for i, l := range d.Lines {
		if lines[l.LineID] {
			return kernel.Reject(kernel.CodeDuplicateLineID, fmt.Sprintf("%s/lines/%d/line_id", p, i), "")
		}
		lines[l.LineID] = true
	}
	rfqLines := map[string]bool{}
	for i, l := range d.Lines {
		if rfqLines[l.RfqLineID] {
			return kernel.Reject(kernel.CodeDuplicateLineID, fmt.Sprintf("%s/lines/%d/rfq_line_id", p, i), "")
		}
		rfqLines[l.RfqLineID] = true
	}
	charges := map[string]bool{}
	for i, c := range d.Charges {
		if charges[c.ChargeID] {
			return kernel.Reject(kernel.CodeDuplicateLineID, fmt.Sprintf("%s/charges/%d/charge_id", p, i), "")
		}
		charges[c.ChargeID] = true
	}
	for _, f := range d.Fields() {
		if f.Ref.Kind != f.Kind {
			return kernel.Reject(kernel.CodeReferenceInvalid, f.Path+"/kind", "The reference is not of the kind its field takes")
		}
	}
	addresses := map[string]bool{}
	for i, a := range d.Addresses {
		if addresses[a.AddressID] {
			return kernel.Reject(kernel.CodeInvalidKnownValue, fmt.Sprintf("%s/customer_address_refs/%d/address_id", p, i), "")
		}
		addresses[a.AddressID] = true
	}
	for i, c := range d.Charges {
		if c.Inclusion == InclusionIncludedInLine && !lines[c.IncludedLineID] {
			return kernel.Reject(kernel.CodeReferenceInvalid, fmt.Sprintf("%s/charges/%d/included_line_id", p, i), "")
		}
	}
	return readable(d, sources)
}

// readable refuses the first evidence entry of d whose source the caller does not read; a missing
// source and one outside the caller's data scope are refused alike (Python _source_ref).
func readable(d Data, sources map[string]bool) error {
	const p = "/args/data"
	check := func(es []Evidence, path string) error {
		for i, e := range es {
			if !sources[e.SourceID] {
				return kernel.Reject(kernel.CodeReferenceInvalid, fmt.Sprintf("%s/%d/source_id", path, i), "Source is not accessible")
			}
		}
		return nil
	}
	if err := check(d.Evidence, p+"/evidence"); err != nil {
		return err
	}
	for i, l := range d.Lines {
		if err := check(l.Evidence, fmt.Sprintf("%s/lines/%d/evidence", p, i)); err != nil {
			return err
		}
	}
	for i, c := range d.Charges {
		if err := check(c.Evidence, fmt.Sprintf("%s/charges/%d/evidence", p, i)); err != nil {
			return err
		}
	}
	return nil
}

func compareString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func compareInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
