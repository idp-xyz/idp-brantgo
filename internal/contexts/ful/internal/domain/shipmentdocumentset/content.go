package shipmentdocumentset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Content is what a rendered version states, in no language (INT-05: the explicit snapshot of one
// shipment's particulars): every rendition of the version prints it, so the figures, currency,
// names and terms of its languages correspond (CN-AT17). Its canonical JSON is hashed and kept with
// the version. Quantities, prices and weights are exact decimals as text; amounts integer minor
// units of the currency as text.
type Content struct {
	DocumentKind    string           `json:"document_kind"`
	Number          string           `json:"number"`
	Version         int64            `json:"version"`
	DocumentSetID   string           `json:"document_set_id"`
	ShipmentID      string           `json:"shipment_id"`
	Order           OrderContent     `json:"order"`
	Date            string           `json:"date"`
	Seller          SellerContent    `json:"seller"`
	Buyer           PartyContent     `json:"buyer"`
	Consignee       PartyContent     `json:"consignee"`
	ConsigneeSource string           `json:"consignee_source"`          // stated, order or buyer
	OrderConsignee  *PartyContent    `json:"order_consignee,omitempty"` // the order's consignee address when it binds one
	Notify          *PartyContent    `json:"notify,omitempty"`
	Marks           string           `json:"marks"`
	PortOfLoading   string           `json:"port_of_loading"`
	PortOfDischarge string           `json:"port_of_discharge"`
	PortsSource     string           `json:"ports_source"` // stated, booking or none
	Booking         *BookingContent  `json:"booking,omitempty"`
	Terms           *TermsContent    `json:"terms,omitempty"`    // CI
	Currency        *CurrencyContent `json:"currency,omitempty"` // CI
	Lines           []LineContent    `json:"lines"`
	Packages        []PackageContent `json:"packages,omitempty"`       // PL
	PackingSource   string           `json:"packing_source,omitempty"` // PL: actual:<seq> or stated
	Totals          TotalsContent    `json:"totals"`
}

// OrderContent is the order the shipment binds.
type OrderContent struct {
	OrderID       string `json:"order_id"`
	OrderRevision int64  `json:"order_revision"`
	OrderHash     string `json:"order_hash"`
}

// SellerContent is the seller as its POL company profile revision states it.
type SellerContent struct {
	Name           string `json:"name"`
	Address        string `json:"address"`
	RegistrationNo string `json:"registration_no"`
	Country        string `json:"country"`
}

// PartyContent is a party as a document prints it; PartyID and PartyRevision name the PTY party it
// is of, when it is one.
type PartyContent struct {
	PartyID       string `json:"party_id,omitempty"`
	PartyRevision int64  `json:"party_revision,omitempty"`
	Name          string `json:"name"`
	Address       string `json:"address"`
}

// BookingContent is the booking version a version printed.
type BookingContent struct {
	Version int64  `json:"version"`
	Number  string `json:"number"`
	Vessel  string `json:"vessel"`
	Voyage  string `json:"voyage"`
	ETD     string `json:"etd"`
}

// TermsContent are the order's trade and payment terms as TRD showed the order's current revision
// (OrderRevision, OrderHash) when the version was prepared.
type TermsContent struct {
	OrderRevision int64          `json:"order_revision"`
	OrderHash     string         `json:"order_hash"`
	Incoterm      string         `json:"incoterm"`
	Edition       string         `json:"edition"`
	NamedPlace    string         `json:"named_place"`
	Payments      []StageContent `json:"payments"`
}

// StageContent is one payment stage.
type StageContent struct {
	StageID  string `json:"stage_id"`
	Percent  string `json:"percent"`
	Trigger  string `json:"trigger"`
	Days     int    `json:"days"`
	DayBasis string `json:"day_basis"`
}

// CurrencyContent is the order currency: its code (the POL currency's ID), revision and minor units.
type CurrencyContent struct {
	Code       string `json:"code"`
	Revision   int64  `json:"revision"`
	MinorUnits int    `json:"minor_units"`
}

// LineContent is a shipment line as a version prints it: the goods' names by language, the
// quantity and unit, and for a CI the unit price and amounts.
type LineContent struct {
	LineID       string            `json:"line_id"`
	OrderLineID  string            `json:"order_line_id"`
	SkuID        string            `json:"sku_id"`
	SpecRevision int64             `json:"spec_revision"`
	Names        map[string]string `json:"names"`
	Quantity     string            `json:"quantity"`
	UnitID       string            `json:"unit_id"`
	UnitPrice    string            `json:"unit_price,omitempty"`
	NetMinor     string            `json:"net_minor,omitempty"`
	TaxMinor     string            `json:"tax_minor,omitempty"`
	PayableMinor string            `json:"payable_minor,omitempty"`
}

// PackageContent is a group of equal cartons numbered CartonFrom on, with what each holds and
// weighs.
type PackageContent struct {
	CartonFrom    int64           `json:"carton_from"`
	Cartons       int64           `json:"cartons"`
	Contents      []PackedContent `json:"contents"`
	NetWeightKg   string          `json:"net_weight_kg"`
	GrossWeightKg string          `json:"gross_weight_kg"`
	VolumeCbm     string          `json:"volume_cbm"`
}

// PackedContent is what each carton of a package holds of a shipment line.
type PackedContent struct {
	LineID            string `json:"line_id"`
	SkuID             string `json:"sku_id"`
	QuantityPerCarton string `json:"quantity_per_carton"`
	UnitID            string `json:"unit_id"`
}

// UnitQuantity is a quantity total in one unit.
type UnitQuantity struct {
	UnitID   string `json:"unit_id"`
	Quantity string `json:"quantity"`
}

// TotalsContent are a version's totals: the quantities per unit, for a CI the amounts, for a PL the
// cartons, weights and volume.
type TotalsContent struct {
	Quantities    []UnitQuantity `json:"quantities"`
	NetMinor      string         `json:"net_minor,omitempty"`
	TaxMinor      string         `json:"tax_minor,omitempty"`
	PayableMinor  string         `json:"payable_minor,omitempty"`
	Cartons       int64          `json:"cartons,omitempty"`
	NetWeightKg   string         `json:"net_weight_kg,omitempty"`
	GrossWeightKg string         `json:"gross_weight_kg,omitempty"`
	VolumeCbm     string         `json:"volume_cbm,omitempty"`
}

// Record is a record of an external document: a bill of lading or a certificate of origin.
type Record struct {
	BillOfLading *BillOfLading `json:"bill_of_lading,omitempty"`
	Certificate  *Certificate  `json:"certificate,omitempty"`
}

// BillOfLading is what an external bill of lading states (BillOfLadingRecord).
type BillOfLading struct {
	Number           string        `json:"number"`
	Vessel           string        `json:"vessel"`
	Voyage           string        `json:"voyage"`
	PortOfLoading    string        `json:"port_of_loading"`
	PortOfDischarge  string        `json:"port_of_discharge"`
	ShippedOnBoard   string        `json:"shipped_on_board"`
	Consignee        PartyContent  `json:"consignee"`
	Notify           *PartyContent `json:"notify,omitempty"`
	Marks            string        `json:"marks"`
	Packages         int64         `json:"packages"`
	GrossWeightKg    string        `json:"gross_weight_kg"`
	VolumeCbm        string        `json:"volume_cbm,omitempty"`
	ContainerNumbers []string      `json:"container_numbers"`
}

// Certificate is what an external certificate of origin states (OriginCertificateRef). CertificateID
// and CertificateVersion name CNX's CertificateResultRef v1 when stated; S5 unit C5 checks them.
type Certificate struct {
	Number             string `json:"number"`
	Issuer             string `json:"issuer"`
	IssuedOn           string `json:"issued_on"`
	CertificateID      string `json:"certificate_id,omitempty"`
	CertificateVersion int64  `json:"certificate_version,omitempty"`
}

// Canonical returns v as canonical JSON (keys in declaration order, maps sorted, no HTML escaping)
// and its SHA-256.
func Canonical(v any) (string, string) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // plain data: never fails
	doc := bytes.TrimSpace(b.Bytes())
	sum := sha256.Sum256(doc)
	return string(doc), hex.EncodeToString(sum[:])
}

// Hash is the SHA-256 of b in lower-case hex.
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Template is a POL shipment_document_template revision as the command read it, with its content
// hash.
type Template struct {
	Ref             Ref
	Hash            string
	DocumentKind    string
	Language        string
	RendererVersion string
	Title           string
	Sections        []string
	Clauses         []string
	FooterNote      string
}

// Seller is the seller's POL company_profile revision with its content hash.
type Seller struct {
	Ref            Ref
	Hash           string
	LegalName      string
	RegistrationNo string
	Country        string
	Address        string
}

// Sequence is a POL number_sequence revision with its content hash and document type.
type Sequence struct {
	Ref          Ref
	Hash         string
	DocumentType string
}

// Currency is the order currency's POL revision and its minor units.
type Currency struct {
	Code       string
	Revision   int64
	MinorUnits int
}

// Address is an address role the order binds: the PTY party at a confirmed revision and the text
// TRD sealed.
type Address struct {
	Role          string
	PartyID       string
	PartyRevision int64
	Text          string
}

// Stage is a payment stage of the order's current revision.
type Stage struct {
	StageID  string
	Percent  string
	Trigger  string
	Days     int
	DayBasis string
}

// OrderFacts is what TRD shows of the shipment's order now (OrderCommercialSnapshot v1): its
// current revision, trade terms, payment plan and the customer's address roles.
type OrderFacts struct {
	Binding    OrderBinding
	Incoterm   string
	Edition    string
	NamedPlace string
	Stages     []Stage
	Addresses  []Address
}

// Address returns the order's first address of role, or nil.
func (o *OrderFacts) Address(role string) *Address {
	for i := range o.Addresses {
		if o.Addresses[i].Role == role {
			return &o.Addresses[i]
		}
	}
	return nil
}

// PartyKey names a PTY party at a confirmed revision.
type PartyKey struct {
	PartyID  string
	Revision int64
}

// Party is a consignee or notify party as stated.
type Party struct {
	Name    string
	Address string
}

// Particulars are what a CI or PL version states besides FUL's facts: consignee and notify (nil:
// default), marks, ports ("": the booking's) and the goods' names by SKU and language.
type Particulars struct {
	Consignee       *Party
	Notify          *Party
	Marks           string
	PortOfLoading   string
	PortOfDischarge string
	Names           map[string]map[string]string // SKU → language → name
	NameOrder       []NameRef                    // the names as stated, for the paths of refusals
}

// NameRef is one stated name: its SKU and language.
type NameRef struct {
	SkuID    string
	Language string
}

// Request is a shipment_document.prepare as the contract checked it, with the POL revisions it
// names as POL showed them before the transaction.
type Request struct {
	Kind           string
	Templates      []Template
	Sequence       *Sequence
	Seller         *Seller
	Particulars    Particulars
	Packages       []shipment.Package // PL: stated; nil: the actual packing
	BillOfLading   *BillOfLading
	Certificate    *Certificate
	FileID         string
	Evidence       []string
	IdempotencyKey string
}

// Facts are what FUL holds of the shipment in the transaction, its share lock held: the shipment
// with its lines and current actual packing, the prices it keeps and its current booking.
type Facts struct {
	Shipment *shipment.Shipment
	Prices   map[string]shipment.Price
	Booking  *shipment.Booking
}

// Upstream are what TRD, PTY, POL and EVD said before the transaction: the order (nil: TRD shows
// none), the PTY parties' names at their revisions, the order currency, the sources and the file the
// actor reads, and the number POL issued.
type Upstream struct {
	Order    *OrderFacts
	Names    map[PartyKey]string
	Currency *Currency
	Found    map[string]bool
	File     *File
	Number   string
}

func text(d kernel.Decimal) string {
	s, _ := d.Exact()
	return s
}

func minorText(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

// Prepare decides a new version of req.Kind in set s (shipment_document.prepare) on facts f and what
// the upstreams said. It refuses, in this order: a cancelled shipment (INVALID_STATE), a source of
// evidence the actor does not read (REFERENCE_INVALID); for a CI or PL a template of another kind or
// two of one language (REFERENCE_INVALID), of another renderer version or language
// (DEPENDENCY_NOT_SUPPORTED), a number sequence of another document type (REFERENCE_INVALID), an
// order TRD does not show, a buyer without a name or an address, a SKU of the shipment without a
// name in a language rendered or a name of another SKU or language, for a CI a line without a price
// or a currency other than the prices' (REFERENCE_INVALID), for a PL stated contents of no shipment
// line or no actual packing to print (REFERENCE_INVALID), a document beyond MaxDocumentBytes
// (INVALID_KNOWN_VALUE); for a B/L or CO a file the actor does not read (REFERENCE_INVALID). The
// version is the kind's next; Add appends it.
func Prepare(s *Set, req Request, f Facts, up Upstream, act Act) (*Document, error) {
	sh := f.Shipment
	if sh == nil {
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/shipment_id", "No shipment the actor reads")
	}
	if sh.Status == shipment.StatusCancelled {
		return nil, Reject(kernel.CodeInvalidState, "/args/shipment_id", "The shipment is cancelled")
	}
	if err := Readable(req.Evidence, up.Found, "/args/evidence_ids"); err != nil {
		return nil, err
	}
	act.At = kernel.Instant(act.At)
	d := &Document{Kind: req.Kind, Version: s.NextVersion(req.Kind), Evidence: slices.Clone(req.Evidence), IdempotencyKey: req.IdempotencyKey,
		Prepared: act}
	d.Sources = []Entry{{Name: "shipment", Value: sh.ID + " v" + strconv.FormatInt(sh.RowVersion, 10) + " " + sh.Status},
		{Name: "order_binding", Value: sh.Order.OrderID + " r" + strconv.FormatInt(sh.Order.OrderRevision, 10) + " " + sh.Order.OrderHash},
		{Name: "booking", Value: bookingSource(f.Booking)}}
	if !Rendered(req.Kind) {
		return record(d, req, up)
	}
	if err := CheckLayout(req); err != nil {
		return nil, err
	}
	c, err := compose(s, d, req, f, up)
	if err != nil {
		return nil, err
	}
	d.Number, d.RendererVersion = up.Number, RendererVersion
	seq, seller := req.Sequence.Ref, req.Seller.Ref
	d.Sequence, d.SequenceHash, d.Seller, d.SellerHash = &seq, req.Sequence.Hash, &seller, req.Seller.Hash
	d.Content = c
	d.ContentJSON, d.ContentSHA256 = Canonical(c)
	if c.Terms != nil {
		d.Sources = append(d.Sources, Entry{Name: "terms_order_revision", Value: "r" + strconv.FormatInt(c.Terms.OrderRevision, 10) + " " +
			c.Terms.OrderHash})
	}
	if c.PackingSource != "" {
		d.Sources = append(d.Sources, Entry{Name: "packing", Value: c.PackingSource})
	}
	for _, t := range req.Templates {
		body := Render(t, c)
		if len(body) > MaxDocumentBytes {
			return nil, Reject(kernel.CodeInvalidKnownValue, "/args/templates", "A shipment document has at most 1 MiB")
		}
		d.Renditions = append(d.Renditions, Rendition{Language: t.Language, Template: t.Ref, TemplateHash: t.Hash,
			FileName: FileName(d.Number, d.Version, t.Language), MimeType: MimeText, SHA256: Hash(body), Size: len(body), Content: body})
		d.Sources = append(d.Sources, Entry{Name: "template:" + t.Language, Value: t.Ref.String() + " " + t.Hash})
	}
	return d, nil
}

// FileName is the file of a rendition: <number>-v<version>-<language>.txt.
func FileName(number string, version int64, language string) string {
	return number + "-v" + strconv.FormatInt(version, 10) + "-" + language + ".txt"
}

func bookingSource(b *shipment.Booking) string {
	if b == nil {
		return "none"
	}
	return "v" + strconv.FormatInt(b.Version, 10)
}

// record decides the version of an external document: its file must be one the actor reads.
func record(d *Document, req Request, up Upstream) (*Document, error) {
	at := "/args/bill_of_lading/file_id"
	r := &Record{BillOfLading: req.BillOfLading}
	if req.Kind == KindCertificateOfOrigin {
		at, r = "/args/certificate/file_id", &Record{Certificate: req.Certificate}
	}
	if up.File == nil || up.File.ID != req.FileID {
		return nil, Reject(kernel.CodeReferenceInvalid, at, "No file the actor reads")
	}
	f := *up.File
	d.File, d.Record = &f, r
	if r.BillOfLading != nil {
		d.Number = r.BillOfLading.Number
	} else {
		d.Number = r.Certificate.Number
	}
	d.ContentJSON, d.ContentSHA256 = Canonical(r)
	d.Sources = append(d.Sources, Entry{Name: "file", Value: f.ID + " " + f.SHA256})
	return d, nil
}

// Numbered reports whether req's layout passes CheckLayout: only then does the command have POL
// issue a number before its transaction.
func Numbered(req Request) bool { return CheckLayout(req) == nil }

// CheckLayout refuses templates that are not of the kind, of another renderer version or language,
// or two of one language, a number sequence of another document type and a seller without a legal
// name and address. The command checks it before it has POL issue a number.
func CheckLayout(req Request) error {
	seen := map[string]bool{}
	for i, t := range req.Templates {
		at := "/args/templates/" + strconv.Itoa(i)
		switch {
		case t.Ref.Kind != KindShipmentDocumentTemplate || t.DocumentKind != req.Kind:
			return Reject(kernel.CodeReferenceInvalid, at, "The template lays out a "+t.DocumentKind+", not a "+req.Kind)
		case t.RendererVersion != RendererVersion:
			return Reject(CodeDependencyNotSupported, at, "Only the shipment-text/0.1.0 renderer is supported")
		case t.Language != LanguageEn && t.Language != LanguageZhCN:
			return Reject(CodeDependencyNotSupported, at, "shipment-text/0.1.0 has layouts for en and zh-CN")
		case seen[t.Language]:
			return Reject(kernel.CodeReferenceInvalid, at, "Two templates of one language")
		case strings.TrimSpace(t.Title) == "":
			return Reject(kernel.CodeReferenceInvalid, at, "A template has a title")
		}
		if bad, ok := checkSections(req.Kind, t.Sections); !ok {
			return Reject(kernel.CodeReferenceInvalid, at, "The template lists an unknown or repeated section "+bad)
		}
		seen[t.Language] = true
	}
	if req.Sequence == nil || req.Sequence.Ref.Kind != KindNumberSequence || req.Sequence.DocumentType != DocumentType(req.Kind) {
		return Reject(kernel.CodeReferenceInvalid, "/args/sequence_ref", "A "+req.Kind+" is numbered from a sequence of document type "+
			DocumentType(req.Kind))
	}
	if req.Seller == nil || req.Seller.Ref.Kind != KindCompanyProfile || strings.TrimSpace(req.Seller.LegalName) == "" ||
		strings.TrimSpace(req.Seller.Address) == "" {
		return Reject(kernel.CodeReferenceInvalid, "/args/seller_ref", "The seller's legal name and address are needed")
	}
	return nil
}

// compose reads the facts into the language-neutral content of rendered version d.
func compose(s *Set, d *Document, req Request, f Facts, up Upstream) (*Content, error) {
	sh, o := f.Shipment, up.Order
	if o == nil || o.Binding.OrderID != sh.Order.OrderID {
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/shipment_id", "TRD shows no order of the shipment to the actor")
	}
	c := &Content{DocumentKind: req.Kind, Number: up.Number, Version: d.Version, DocumentSetID: s.ID, ShipmentID: sh.ID,
		Order: OrderContent{OrderID: sh.Order.OrderID, OrderRevision: sh.Order.OrderRevision, OrderHash: sh.Order.OrderHash},
		Date:  kernel.FormatTimestamp(d.Prepared.At), Lines: []LineContent{},
		Seller: SellerContent{Name: req.Seller.LegalName, Address: req.Seller.Address, RegistrationNo: req.Seller.RegistrationNo,
			Country: req.Seller.Country}}
	if !kernel.ValidIdentifier(up.Number) || strings.Contains(up.Number, ":") {
		return nil, Reject(kernel.CodeReferenceInvalid, "/number", "A document number is 1-64 letters, digits, '.', '_' or '-'")
	}
	if err := parties(c, sh, o, req.Particulars, up.Names); err != nil {
		return nil, err
	}
	c.Marks = req.Particulars.Marks
	if b := f.Booking; b != nil {
		c.Booking = &BookingContent{Version: b.Version, Number: b.Number, Vessel: b.Vessel, Voyage: b.Voyage, ETD: b.ETD}
	}
	switch p := req.Particulars; {
	case p.PortOfLoading != "" || p.PortOfDischarge != "":
		c.PortOfLoading, c.PortOfDischarge, c.PortsSource = p.PortOfLoading, p.PortOfDischarge, "stated"
	case f.Booking != nil:
		c.PortOfLoading, c.PortOfDischarge, c.PortsSource = f.Booking.PortOfLoading, f.Booking.PortOfDischarge, "booking"
	default:
		c.PortsSource = "none"
	}
	names, err := goodsNames(sh, req)
	if err != nil {
		return nil, err
	}
	if req.Kind == KindCommercialInvoice {
		return c, invoice(c, sh, o, f.Prices, up.Currency, names)
	}
	return c, packingList(c, sh, req.Packages, names)
}

// parties fills the buyer (the customer at the revision the shipment binds, at the order's buyer
// address, else its billing address), the consignee (stated, else the order's consignee address,
// else the buyer) and the notify party (stated, else none).
func parties(c *Content, sh *shipment.Shipment, o *OrderFacts, p Particulars, names map[PartyKey]string) error {
	var customer shipment.Party
	for _, x := range sh.Parties {
		if x.Role == shipment.RoleCustomer {
			customer = x
		}
	}
	name := names[PartyKey{PartyID: customer.PartyID, Revision: customer.PartyRevision}]
	address := o.Address("buyer")
	if address == nil {
		address = o.Address("billing")
	}
	if customer.PartyID == "" || strings.TrimSpace(name) == "" || address == nil || strings.TrimSpace(address.Text) == "" {
		return Reject(kernel.CodeReferenceInvalid, "/args/shipment_id", "The buyer's name and buyer or billing address are needed")
	}
	c.Buyer = PartyContent{PartyID: customer.PartyID, PartyRevision: customer.PartyRevision, Name: name, Address: address.Text}
	if a := o.Address("consignee"); a != nil {
		n := names[PartyKey{PartyID: a.PartyID, Revision: a.PartyRevision}]
		if strings.TrimSpace(n) == "" {
			return Reject(kernel.CodeReferenceInvalid, "/args/shipment_id", "The order's consignee has no name the actor reads")
		}
		c.OrderConsignee = &PartyContent{PartyID: a.PartyID, PartyRevision: a.PartyRevision, Name: n, Address: a.Text}
	}
	switch {
	case p.Consignee != nil:
		c.Consignee, c.ConsigneeSource = PartyContent{Name: p.Consignee.Name, Address: p.Consignee.Address}, "stated"
	case c.OrderConsignee != nil:
		c.Consignee, c.ConsigneeSource = *c.OrderConsignee, "order"
	default:
		c.Consignee, c.ConsigneeSource = c.Buyer, "buyer"
	}
	if p.Notify != nil {
		c.Notify = &PartyContent{Name: p.Notify.Name, Address: p.Notify.Address}
	}
	return nil
}

// goodsNames returns the names of every SKU of the shipment by language, refusing a SKU without a
// name in a language rendered and a name of another SKU or language.
func goodsNames(sh *shipment.Shipment, req Request) (map[string]map[string]string, error) {
	languages := map[string]bool{}
	for _, t := range req.Templates {
		languages[t.Language] = true
	}
	skus := map[string]bool{}
	for _, l := range sh.Lines {
		skus[l.SkuID] = true
	}
	for i, n := range req.Particulars.NameOrder {
		at := "/args/particulars/descriptions/" + strconv.Itoa(i)
		if !skus[n.SkuID] {
			return nil, Reject(kernel.CodeReferenceInvalid, at+"/sku_id", "The shipment has no line of SKU "+n.SkuID)
		}
		if !languages[n.Language] {
			return nil, Reject(kernel.CodeReferenceInvalid, at+"/language", "No template of language "+n.Language)
		}
	}
	out := map[string]map[string]string{}
	for _, l := range sh.Lines {
		for _, t := range req.Templates {
			name := req.Particulars.Names[l.SkuID][t.Language]
			if strings.TrimSpace(name) == "" {
				return nil, Reject(kernel.CodeReferenceInvalid, "/args/particulars/descriptions", "SKU "+l.SkuID+" has no name in "+t.Language)
			}
			if out[l.SkuID] == nil {
				out[l.SkuID] = map[string]string{}
			}
			out[l.SkuID][t.Language] = name
		}
	}
	return out, nil
}

// invoice fills a CI's terms, currency, lines at the prices the shipment keeps (the order line's
// price at the bound revision; the amounts of the shipped quantity as shipment.Price.Of values them)
// and totals.
func invoice(c *Content, sh *shipment.Shipment, o *OrderFacts, prices map[string]shipment.Price, cur *Currency,
	names map[string]map[string]string) error {
	c.Terms = &TermsContent{OrderRevision: o.Binding.OrderRevision, OrderHash: o.Binding.OrderHash, Incoterm: o.Incoterm, Edition: o.Edition,
		NamedPlace: o.NamedPlace, Payments: []StageContent{}}
	if strings.TrimSpace(o.Incoterm) == "" {
		return Reject(kernel.CodeReferenceInvalid, "/args/shipment_id", "The order states no Incoterm")
	}
	for _, st := range o.Stages {
		c.Terms.Payments = append(c.Terms.Payments, StageContent(st))
	}
	total := shipment.Amounts{}
	quantities := map[string]kernel.Decimal{}
	for i, l := range sh.Lines {
		p, ok := prices[l.ID]
		if !ok {
			return Reject(kernel.CodeReferenceInvalid, "/args/shipment_id", "Shipment line "+l.ID+" keeps no price")
		}
		if cur == nil || p.CurrencyID != cur.Code || p.CurrencyRevision != cur.Revision || cur.MinorUnits < 0 || cur.MinorUnits > 4 {
			return Reject(kernel.CodeReferenceInvalid, "/args/shipment_id", "The order currency's revision and minor units are needed")
		}
		if i == 0 {
			c.Currency = &CurrencyContent{Code: cur.Code, Revision: cur.Revision, MinorUnits: cur.MinorUnits}
		}
		a := p.Of(l.Quantity, l.OrderQuantity)
		total = total.Add(a)
		quantities[l.UnitID] = quantities[l.UnitID].Add(l.Quantity)
		c.Lines = append(c.Lines, LineContent{LineID: l.ID, OrderLineID: l.OrderLineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision,
			Names: names[l.SkuID], Quantity: text(l.Quantity), UnitID: l.UnitID, UnitPrice: text(p.UnitPrice), NetMinor: minorText(a.Net),
			TaxMinor: minorText(a.Tax), PayableMinor: minorText(a.Payable)})
	}
	c.Totals = TotalsContent{Quantities: unitTotals(quantities), NetMinor: minorText(total.Net), TaxMinor: minorText(total.Tax),
		PayableMinor: minorText(total.Payable)}
	return nil
}

// packingList fills a PL's packages — the stated ones, else the shipment's current actual packing
// — with the lines' packed quantities and the totals.
func packingList(c *Content, sh *shipment.Shipment, stated []shipment.Package, names map[string]map[string]string) error {
	packages := stated
	c.PackingSource = "stated"
	if packages == nil {
		if sh.Actual == nil {
			return Reject(kernel.CodeReferenceInvalid, "/args/packages", "The shipment has no actual packing; state the packages")
		}
		packages, c.PackingSource = sh.Actual.Packages, "actual:"+strconv.FormatInt(sh.Actual.Seq, 10)
	}
	packed := map[string]kernel.Decimal{}
	quantities := map[string]kernel.Decimal{}
	var cartons int64
	var net, gross, volume kernel.Decimal
	for i, p := range packages {
		k := PackageContent{CartonFrom: p.CartonFrom, Cartons: p.Cartons, Contents: []PackedContent{}}
		for j, x := range p.Contents {
			l, _, ok := sh.Line(x.LineID)
			if !ok {
				return Reject(kernel.CodeReferenceInvalid, "/args/packages/"+strconv.Itoa(i)+"/contents/"+strconv.Itoa(j)+"/line_id",
					"The shipment has no such line")
			}
			q := kernel.DecimalFromInt(p.Cartons).Mul(x.PerCarton)
			packed[l.ID], quantities[l.UnitID] = packed[l.ID].Add(q), quantities[l.UnitID].Add(q)
			k.Contents = append(k.Contents, PackedContent{LineID: l.ID, SkuID: l.SkuID, QuantityPerCarton: text(x.PerCarton), UnitID: l.UnitID})
		}
		n := kernel.DecimalFromInt(p.Cartons)
		if p.Net == nil || p.Gross == nil || p.Volume == nil {
			return Reject(kernel.CodeReferenceInvalid, "/args/packages/"+strconv.Itoa(i), "A packing list states every package's weights and volume")
		}
		k.NetWeightKg, k.GrossWeightKg, k.VolumeCbm = text(*p.Net), text(*p.Gross), text(*p.Volume)
		cartons, net, gross, volume = cartons+p.Cartons, net.Add(n.Mul(*p.Net)), gross.Add(n.Mul(*p.Gross)), volume.Add(n.Mul(*p.Volume))
		c.Packages = append(c.Packages, k)
	}
	for _, l := range sh.Lines {
		c.Lines = append(c.Lines, LineContent{LineID: l.ID, OrderLineID: l.OrderLineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision,
			Names: names[l.SkuID], Quantity: text(packed[l.ID]), UnitID: l.UnitID})
	}
	c.Totals = TotalsContent{Quantities: unitTotals(quantities), Cartons: cartons, NetWeightKg: text(net), GrossWeightKg: text(gross),
		VolumeCbm: text(volume)}
	return nil
}

func unitTotals(m map[string]kernel.Decimal) []UnitQuantity {
	units := make([]string, 0, len(m))
	for u := range m {
		units = append(units, u)
	}
	sort.Strings(units)
	out := make([]UnitQuantity, 0, len(units))
	for _, u := range units {
		out = append(out, UnitQuantity{UnitID: u, Quantity: text(m[u])})
	}
	return out
}
