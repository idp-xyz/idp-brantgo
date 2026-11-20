package orderrev

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Contracts of the two forms of an order revision document.
const (
	// SnapshotContract is revision 1: the order's commercial snapshot (order.prepare; Python
	// t03_order_snapshot.CONTRACT).
	SnapshotContract = "brant.order-commercial-snapshot/0.1.0"
	// RevisionContract is every later revision (order.amendment.apply).
	RevisionContract = "brant.order-revision/0.1.0"
	// SlotFullV1 is the conversion slot every order holds (TRD-INV-010).
	SlotFullV1 = "full_v1"
)

// Scope is the tenant and company an order belongs to.
type Scope struct {
	Tenant  string `json:"tenant"`
	Company string `json:"company"`
}

// Link names an order revision by its number and order hash (OrderBinding of OrderGuard v1).
type Link struct {
	Revision int64  `json:"order_revision"`
	Hash     string `json:"order_hash"`
}

// AmendmentRef names the approved amendment that produced a revision, by its ID and hash.
type AmendmentRef struct {
	ID   string `json:"amendment_id"`
	Hash string `json:"amendment_hash"`
}

// Source is where the order's contract comes from: the formal quote revision the customer accepted
// (its business hash, number and formal document) and the acceptance that formed the contract. No
// amendment changes it.
type Source struct {
	QuoteID        string `json:"quote_id"`
	Revision       int64  `json:"business_revision"`
	BusinessHash   string `json:"business_hash"`
	Number         string `json:"number"`
	DocumentSHA256 string `json:"document_sha256"`
	AcceptanceID   string `json:"acceptance_id"`
}

// Terms are the order's trade terms, named place, payment terms (POL revisions and a place) and
// delivery, and the notify party it binds (S5R-D40: a PTY AddressRoleRef v1 of the role notify;
// absent when none, so terms written before S5R keep their bytes; seal.go).
type Terms struct {
	TradeTerms   quote.Ref         `json:"trade_terms_ref"`
	NamedPlace   quote.BusinessRef `json:"named_place_ref"`
	PaymentTerms quote.Ref         `json:"payment_terms_ref"`
	Delivery     quote.Delivery    `json:"delivery"`
	NotifyParty  *quote.AddressRef `json:"notify_party_ref,omitempty"`
}

// Tax is the tax a line or charge is taxed by: the POL tax application and the profile it applied,
// with the profile's price basis and rate as the amounts used them.
type Tax struct {
	Application quote.Ref `json:"tax_application_ref"`
	Profile     quote.Ref `json:"tax_profile_ref"`
	PriceBasis  string    `json:"price_basis"`
	Rate        string    `json:"rate"`
}

// Line is one order line: the RFQ line it quotes ("" for a line an amendment added), the SKU
// specification, the quantity in its unit and, converted, in the SKU's cost unit (what PUR buys
// and FUL ships against), the price and the tax.
type Line struct {
	LineID             string       `json:"line_id"`
	RfqLineID          string       `json:"rfq_line_id,omitempty"`
	Sku                quote.SkuRef `json:"sku_ref"`
	Quantity           string       `json:"quantity"`
	Unit               quote.Ref    `json:"unit_ref"`
	Conversion         *quote.Ref   `json:"conversion_ref,omitempty"`
	NormalizedQuantity string       `json:"normalized_quantity"`
	CostUnit           string       `json:"cost_unit"`
	UnitPrice          string       `json:"unit_price"`
	PriceBasisQuantity string       `json:"price_basis_quantity"`
	DiscountRatio      string       `json:"discount_ratio"`
	Tax                Tax          `json:"tax"`
}

// Charge is one sales charge of the order: its fee category and obligation, its amount in the
// order currency, whether it is charged separately or included in a line, and its tax.
type Charge struct {
	ChargeID       string    `json:"charge_id"`
	FeeCategory    quote.Ref `json:"fee_category_ref"`
	ObligationKey  string    `json:"obligation_key"`
	Amount         string    `json:"amount"`
	Inclusion      string    `json:"inclusion"`
	IncludedLineID string    `json:"included_line_id,omitempty"`
	Tax            Tax       `json:"tax"`
}

// Amounts are the order revision's amounts in integer minor units of the order currency: per line
// and charge, the totals and the payment plan (a plan, not receivables).
type Amounts struct {
	Lines    []quote.AmountLine `json:"lines"`
	Charges  []quote.AmountLine `json:"charges"`
	Net      string             `json:"net_minor"`
	Tax      string             `json:"tax_minor"`
	Payable  string             `json:"payable_minor"`
	Payments []quote.Payment    `json:"payments"`
}

// Revision is an order revision (UL TRD-OrderRevision) as the rules see it, the same for revision 1
// (FromSnapshot) and every later one (Parse, Next). Hash is the order hash, the SHA-256 of Document,
// the bytes stored for the revision: the snapshot for revision 1, the canonical
// brant.order-revision/0.1.0 document for the others. Contract says which.
type Revision struct {
	Contract  string               `json:"revision_contract"`
	Scope     Scope                `json:"scope"`
	OrderID   string               `json:"order_id"`
	Number    int64                `json:"order_revision"`
	Previous  *Link                `json:"previous,omitempty"`
	Amendment *AmendmentRef        `json:"amendment,omitempty"`
	Source    Source               `json:"source"`
	Parties   quote.PartySnapshots `json:"parties"`
	Customer  quote.PartyRef       `json:"customer_ref"`
	Contact   quote.ContactRef     `json:"contact_ref"`
	Currency  quote.Ref            `json:"currency_ref"`
	Terms     Terms                `json:"terms"`
	Lines     []Line               `json:"lines"`
	Charges   []Charge             `json:"charges"`
	Amounts   Amounts              `json:"amounts"`
	// The legal terms the source quote revision sealed (WBS S3-17) and their POL revision, copied
	// from its commercial document and never from POL's current revision; absent when it sealed
	// none. No amendment changes them.
	LegalTermsRef *quote.Ref        `json:"legal_terms_ref,omitempty"`
	LegalTerms    *quote.LegalTerms `json:"legal_terms,omitempty"`
	// What the revision sealed of its trade terms and parties (S5R-D35, D40; seal.go): absent in a
	// revision written before S5R, whose bytes and order hash never change.
	Seal *Seal `json:"seal,omitempty"`

	Hash     string `json:"-"`
	Document []byte `json:"-"`
}

// Link returns the revision's number and order hash.
func (r *Revision) Link() Link { return Link{Revision: r.Number, Hash: r.Hash} }

// Line returns line id of the revision, or nil.
func (r *Revision) Line(id string) *Line {
	for i := range r.Lines {
		if r.Lines[i].LineID == id {
			return &r.Lines[i]
		}
	}
	return nil
}

// Charge returns charge id of the revision, or nil.
func (r *Revision) Charge(id string) *Charge {
	for i := range r.Charges {
		if r.Charges[i].ChargeID == id {
			return &r.Charges[i]
		}
	}
	return nil
}

// LineAmounts returns the amounts of line id, or false.
func (r *Revision) LineAmounts(id string) (quote.AmountLine, bool) {
	i := slices.IndexFunc(r.Amounts.Lines, func(a quote.AmountLine) bool { return a.ID == id })
	if i < 0 {
		return quote.AmountLine{}, false
	}
	return r.Amounts.Lines[i], true
}

// ChargeAmounts returns the amounts of charge id, or false.
func (r *Revision) ChargeAmounts(id string) (quote.AmountLine, bool) {
	i := slices.IndexFunc(r.Amounts.Charges, func(a quote.AmountLine) bool { return a.ID == id })
	if i < 0 {
		return quote.AmountLine{}, false
	}
	return r.Amounts.Charges[i], true
}

// snapshotDoc is what revision 1's snapshot holds that a revision reads (order.BuildSnapshot, package
// order: Python t03_order_snapshot.build on TRD's commercial document).
type snapshotDoc struct {
	Contract string           `json:"snapshot_contract"`
	Scope    Scope            `json:"scope"`
	OrderID  string           `json:"order_id"`
	Slot     string           `json:"conversion_slot"`
	Customer quote.PartyRef   `json:"customer_ref"`
	Contact  quote.ContactRef `json:"contact_ref"`
	Currency quote.Ref        `json:"currency_ref"`
	Terms    struct {
		TradeTerms   quote.Ref         `json:"trade_terms_ref"`
		NamedPlace   quote.BusinessRef `json:"named_place_ref"`
		PaymentTerms quote.Ref         `json:"payment_terms_ref"`
		Delivery     quote.Delivery    `json:"delivery"`
		NotifyParty  *quote.AddressRef `json:"notify_party_ref"`
	} `json:"terms"`
	Seal                 *Seal              `json:"seal"`
	Lines                []quote.Line       `json:"lines"`
	Charges              []quote.Charge     `json:"charges"`
	Calculation          *quote.Calculation `json:"calculation"`
	SourceQuoteID        string             `json:"source_quote_id"`
	SourceQuoteRevision  int64              `json:"source_quote_revision"`
	SourceQuoteHash      string             `json:"source_quote_hash"`
	SourceQuoteNumber    string             `json:"source_quote_number"`
	SourceDocumentSHA256 string             `json:"source_document_sha256"`
	AcceptanceID         string             `json:"acceptance_id"`
	Quote                struct {
		PartySnapshots quote.PartySnapshots `json:"party_snapshots"`
		LegalTerms     *quote.LegalTerms    `json:"legal_terms"`
		Data           struct {
			LegalTerms *quote.Ref `json:"legal_terms_ref"`
		} `json:"data"`
	} `json:"source_quote_snapshot"`
}

// FromSnapshot reads revision 1 of an order from the commercial snapshot order.prepare stored and
// its order hash. The snapshot must hash to orderHash, be an order commercial snapshot of slot
// full_v1, and carry the server calculation with a cost binding, a tax binding and amounts for every
// line and charge (REFERENCE_INVALID at /snapshot... otherwise: the snapshot copies a sealed quote
// revision whole, so a gap means the bytes are not a snapshot order.prepare wrote). The lines and
// charges come out in ID order; nothing is recalculated.
func FromSnapshot(doc []byte, orderHash string) (*Revision, error) {
	bad := func(path, why string) (*Revision, error) {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/snapshot"+path, "The order snapshot "+why)
	}
	if quote.Hash(doc) != orderHash {
		return bad("", "does not hash to its order hash")
	}
	var s snapshotDoc
	if err := json.Unmarshal(doc, &s); err != nil {
		return bad("", "is not readable")
	}
	if s.Contract != SnapshotContract || s.Slot != SlotFullV1 || !kernel.ValidIdentifier(s.OrderID) {
		return bad("/snapshot_contract", "is not an order commercial snapshot of slot full_v1")
	}
	c := s.Calculation
	if c == nil {
		return bad("/calculation", "has no server calculation")
	}
	r := &Revision{Contract: SnapshotContract, Scope: s.Scope, OrderID: s.OrderID, Number: 1,
		Source: Source{QuoteID: s.SourceQuoteID, Revision: s.SourceQuoteRevision, BusinessHash: s.SourceQuoteHash, Number: s.SourceQuoteNumber,
			DocumentSHA256: s.SourceDocumentSHA256, AcceptanceID: s.AcceptanceID},
		Parties: s.Quote.PartySnapshots, Customer: s.Customer, Contact: s.Contact, Currency: s.Currency,
		LegalTermsRef: s.Quote.Data.LegalTerms, LegalTerms: s.Quote.LegalTerms,
		Terms: Terms{TradeTerms: s.Terms.TradeTerms, NamedPlace: s.Terms.NamedPlace, PaymentTerms: s.Terms.PaymentTerms, Delivery: s.Terms.Delivery,
			NotifyParty: s.Terms.NotifyParty},
		Seal:  s.Seal,
		Lines: []Line{}, Charges: []Charge{},
		Amounts: Amounts{Lines: []quote.AmountLine{}, Charges: []quote.AmountLine{}, Net: c.NetMinor, Tax: c.TaxMinor,
			Payable: c.PayableMinor, Payments: slices.Clone(c.Payments)},
		Hash: orderHash, Document: slices.Clone(doc)}
	if r.Amounts.Payments == nil {
		r.Amounts.Payments = []quote.Payment{}
	}
	taxes := map[string]quote.TaxBinding{}
	for _, t := range c.SalesTerms.Taxes {
		taxes[t.Role] = t
	}
	for i, l := range s.Lines {
		at := "/lines/" + strconv.Itoa(i)
		b := slices.IndexFunc(c.CostScope.Lines, func(b quote.LineBinding) bool { return b.LineID == l.LineID })
		t, taxed := taxes["line:"+l.LineID]
		a := slices.IndexFunc(c.Lines, func(a quote.AmountLine) bool { return a.ID == l.LineID })
		if b < 0 || !taxed || a < 0 {
			return bad(at, "has no cost binding, tax binding or amounts for line "+l.LineID)
		}
		binding := c.CostScope.Lines[b]
		r.Lines = append(r.Lines, Line{LineID: l.LineID, RfqLineID: l.RfqLineID, Sku: l.Sku, Quantity: l.Quantity, Unit: l.Unit,
			Conversion: l.Conversion, NormalizedQuantity: binding.NormalizedQuantity, CostUnit: binding.CostUnit, UnitPrice: l.UnitPrice,
			PriceBasisQuantity: l.PriceBasisQuantity, DiscountRatio: l.DiscountRatio, Tax: taxOf(t)})
		r.Amounts.Lines = append(r.Amounts.Lines, c.Lines[a])
	}
	for i, ch := range s.Charges {
		at := "/charges/" + strconv.Itoa(i)
		t, taxed := taxes["charge:"+ch.ObligationKey]
		a := slices.IndexFunc(c.Charges, func(a quote.AmountLine) bool { return a.ID == ch.ChargeID })
		if !taxed || a < 0 {
			return bad(at, "has no tax binding or amounts for charge "+ch.ChargeID)
		}
		r.Charges = append(r.Charges, Charge{ChargeID: ch.ChargeID, FeeCategory: ch.FeeCategory, ObligationKey: ch.ObligationKey,
			Amount: ch.Amount, Inclusion: ch.Inclusion, IncludedLineID: ch.IncludedLineID, Tax: taxOf(t)})
		r.Amounts.Charges = append(r.Amounts.Charges, c.Charges[a])
	}
	if len(r.Lines) == 0 {
		return bad("/lines", "has no lines")
	}
	if (r.LegalTerms == nil) != (r.LegalTermsRef == nil) {
		return bad("/source_quote_snapshot/legal_terms", "seals legal terms without their revision, or a revision without its terms")
	}
	if why := checkSeal(r); why != "" {
		return bad("/seal", why)
	}
	sortRevision(r)
	return r, nil
}

func taxOf(t quote.TaxBinding) Tax {
	return Tax{Application: t.Application, Profile: t.Profile, PriceBasis: t.PriceBasis, Rate: t.Rate}
}

// sortRevision puts lines, charges and their amounts in ID order (code point order).
func sortRevision(r *Revision) {
	slices.SortFunc(r.Lines, func(a, b Line) int { return strings.Compare(a.LineID, b.LineID) })
	slices.SortFunc(r.Charges, func(a, b Charge) int { return strings.Compare(a.ChargeID, b.ChargeID) })
	byID := func(a, b quote.AmountLine) int { return strings.Compare(a.ID, b.ID) }
	slices.SortFunc(r.Amounts.Lines, byID)
	slices.SortFunc(r.Amounts.Charges, byID)
}

// Parse reads an order revision after the first from its stored document and order hash: the
// document must hash to hash and be the canonical brant.order-revision/0.1.0 document of a revision
// that amends an earlier one by an amendment (REFERENCE_INVALID at /revision otherwise).
func Parse(doc []byte, hash string) (*Revision, error) {
	bad := func(why string) (*Revision, error) {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/revision", "The order revision "+why)
	}
	if quote.Hash(doc) != hash {
		return bad("does not hash to its order hash")
	}
	var r Revision
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return bad("is not readable")
	}
	if r.Contract != RevisionContract || r.Number < 2 || r.Previous == nil || r.Previous.Revision != r.Number-1 || r.Amendment == nil {
		return bad("is not a later revision of an order")
	}
	if why := checkSeal(&r); why != "" {
		return bad(why)
	}
	canonical, _, err := encode(&r)
	if err != nil || !bytes.Equal(canonical, doc) {
		return bad("is not in its canonical form")
	}
	r.Hash, r.Document = hash, slices.Clone(doc)
	return &r, nil
}

// encode returns the canonical document of revision r (a later revision) and its SHA-256.
func encode(r *Revision) ([]byte, string, error) {
	tree, err := quote.TreeOf(r)
	if err != nil {
		return nil, "", err
	}
	doc, err := quote.Canonical(tree)
	if err != nil {
		return nil, "", err
	}
	return doc, quote.Hash(doc), nil
}
