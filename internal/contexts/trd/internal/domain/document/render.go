package document

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// MaxDocumentBytes bounds a quote document (Python document.render: 1 MiB,
// CALCULATION_LIMIT_EXCEEDED beyond).
const MaxDocumentBytes = 1 << 20

const refInvalid = kernel.CodeReferenceInvalid

// customerAddressRoles are the roles that address a quote, in the order the recipient block
// prefers them (Python t03_trade_parties QUOTE_CUSTOMER_ADDRESS_ROLES).
var customerAddressRoles = []string{"buyer", "billing"}

// sheet is a quote document as quote-text/0.1.0 prints it: every value in its printed form.
type sheet struct {
	issuerName, issuerAddress                string
	customerName, customerAddress, contact   string
	number, quoteID, businessHash            string
	revision                                 int64
	lines                                    []lineRow // printed in line ID order
	charges                                  []chargeRow
	currency                                 string
	minorUnits                               int
	net, tax, payable                        string
	incoterm, edition, responsibility, scope string
	namedPlace                               string
	delivery                                 []string // the parts after "Delivery: "
	payments                                 []paymentRow
	issuedAt, expiresAt, customerNotes       string
	legal                                    []string // the legal_terms section's lines (quote-text/0.2.0)
}

type lineRow struct {
	id, item, specification, quantity, unit, unitPrice, basis, discount, net, tax, payable string
}

type chargeRow struct{ id, obligation, inclusion, net, tax, payable string }

type paymentRow struct {
	stage, percent, amount, trigger, dayBasis string
	days                                      int
	calendar                                  *quote.Ref
}

// text renders s with t (Python document.render): the title, a blank line, the sections in t's
// order separated by blank lines, and a final line break.
func text(t Template, s sheet) []byte {
	var b strings.Builder
	b.WriteString(t.Title)
	b.WriteString("\n\n")
	for i, name := range t.Sections {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(block(t, s, name))
	}
	b.WriteString("\n")
	return []byte(b.String())
}

func block(t Template, s sheet, name string) string {
	switch name {
	case "issuer":
		return "Issuer: " + s.issuerName + "\n" + s.issuerAddress
	case "recipient":
		return "Customer: " + s.customerName + "\n" + s.customerAddress + "\nContact: " + s.contact
	case "identity":
		return "Number: " + s.number + "\nQuote: " + s.quoteID + " r" + strconv.FormatInt(s.revision, 10) + "\nBusiness SHA-256: " + s.businessHash
	case "lines":
		rows := slices.Clone(s.lines)
		slices.SortStableFunc(rows, func(a, b lineRow) int { return strings.Compare(a.id, b.id) })
		out := make([]string, 0, len(rows)+len(s.charges))
		for _, l := range rows {
			out = append(out, strings.Join([]string{l.id, l.item, l.specification, l.quantity + " " + l.unit,
				"price " + l.unitPrice + " per " + l.basis, "discount " + l.discount, "net " + l.net, "tax " + l.tax, "payable " + l.payable}, " | "))
		}
		for _, c := range s.charges {
			out = append(out, strings.Join([]string{"charge " + c.id, c.obligation, c.inclusion, "net " + c.net, "tax " + c.tax,
				"payable " + c.payable}, " | "))
		}
		return "Lines (all amounts in currency minor units):\n" + strings.Join(out, "\n")
	case "totals":
		return "Currency: " + s.currency + " | minor digits: " + strconv.Itoa(s.minorUnits) + "\nNet: " + s.net + " | Tax: " + s.tax +
			" | Payable: " + s.payable
	case "terms":
		return "Terms: " + s.incoterm + " " + s.edition + " | " + s.responsibility + "\nNamed place: " + s.namedPlace + " | Scope: " + s.scope
	case "delivery":
		return "Delivery: " + strings.Join(s.delivery, " | ")
	case "payments":
		out := make([]string, 0, len(s.payments))
		for _, p := range s.payments {
			basis := p.dayBasis
			if basis == "" {
				basis = "calendar" // POL: days without a day basis are calendar days
			}
			parts := []string{p.stage, p.percent + "%", p.amount, p.trigger, strconv.Itoa(p.days) + " " + basis + " days"}
			if p.calendar != nil {
				parts = append(parts, "calendar "+refText(*p.calendar))
			}
			out = append(out, strings.Join(parts, " | "))
		}
		return "Payment plan (minor units; not receipts):\n" + strings.Join(out, "\n")
	case "validity":
		return "Commercial issue: " + s.issuedAt + "\nExpires exclusively: " + s.expiresAt
	case "notes":
		return s.customerNotes + "\n" + t.FooterNote
	case SectionLegalTerms:
		return strings.Join(s.legal, "\n")
	}
	panic("document: unchecked section " + name) // Supported admits its renderer's sections only
}

// refText prints a reference as Python does: "<id> r<revision>".
func refText(r quote.Ref) string { return r.ID + " r" + strconv.FormatInt(r.Revision, 10) }

// compose reads the commercial document c (business hash hash) and the facts into a sheet, without
// the number. The template has been checked against c, and c.Calculation with Calculation.Check,
// which gives every line and charge exactly one amount line.
func compose(c quote.Commercial, hash string, f Facts) (sheet, error) {
	d, calc := c.Data, c.Calculation
	s := sheet{issuerName: c.PartySnapshots.Issuer.LegalName, issuerAddress: c.PartySnapshots.Issuer.Address,
		quoteID: c.QuoteID, revision: c.BusinessRevision, businessHash: hash, net: calc.NetMinor, tax: calc.TaxMinor,
		payable: calc.PayableMinor, issuedAt: c.IssuedAt, expiresAt: d.ExpiresAt, customerNotes: d.CustomerNotes}

	if f.Customer.Ref != d.Customer || blank(f.Customer.Name) {
		return sheet{}, fail(refInvalid, "/data/customer_ref", "The customer's name at the bound party revision is needed")
	}
	s.customerName = f.Customer.Name
	address, ok := recipientAddress(c.PartySnapshots.Customer.Addresses)
	if !ok {
		return sheet{}, fail(refInvalid, "/party_snapshots/customer/addresses", "A buyer or billing address addresses the quote")
	}
	s.customerAddress = address
	if f.Contact.Ref != d.Contact || blank(f.Contact.Name) {
		return sheet{}, fail(refInvalid, "/data/contact_ref", "The contact's name at the bound row version is needed")
	}
	s.contact = f.Contact.Name

	lineAmounts, chargeAmounts := amounts(calc.Lines), amounts(calc.Charges)
	for i, l := range d.Lines {
		item, ok := f.Items[l.Sku]
		if !ok || blank(item.Name) || blank(item.Specification) {
			return sheet{}, fail(refInvalid, fmt.Sprintf("/data/lines/%d/sku_ref", i), "The item and specification names are needed")
		}
		a := lineAmounts[l.LineID]
		s.lines = append(s.lines, lineRow{id: l.LineID, item: item.Name, specification: item.Specification, quantity: l.Quantity,
			unit: l.Unit.ID, unitPrice: l.UnitPrice, basis: l.PriceBasisQuantity, discount: l.DiscountRatio, net: a.NetMinor,
			tax: a.TaxMinor, payable: a.PayableMinor})
	}
	for _, ch := range d.Charges {
		a := chargeAmounts[ch.ChargeID]
		s.charges = append(s.charges, chargeRow{id: ch.ChargeID, obligation: ch.ObligationKey, inclusion: ch.Inclusion,
			net: a.NetMinor, tax: a.TaxMinor, payable: a.PayableMinor})
	}

	if f.Currency.Ref != d.Currency {
		return sheet{}, fail(refInvalid, "/data/currency_ref", "The currency fact is for another revision")
	}
	if err := pinned(c, f.Currency.Ref, f.Currency.ContentHash, "/data/currency_ref"); err != nil {
		return sheet{}, err
	}
	if f.Currency.MinorUnits < 0 || f.Currency.MinorUnits > 4 {
		return sheet{}, fail(refInvalid, "/data/currency_ref", "A currency has 0 to 4 minor units")
	}
	s.currency, s.minorUnits = d.Currency.ID, f.Currency.MinorUnits

	tt := f.TradeTerms
	if tt.Ref != d.TradeTerms {
		return sheet{}, fail(refInvalid, "/data/trade_terms_ref", "The trade terms fact is for another revision")
	}
	if err := pinned(c, tt.Ref, tt.ContentHash, "/data/trade_terms_ref"); err != nil {
		return sheet{}, err
	}
	if blank(tt.Incoterm) || blank(tt.Edition) {
		return sheet{}, fail(refInvalid, "/data/trade_terms_ref", "The trade terms' Incoterm and edition are needed")
	}
	s.incoterm, s.edition, s.responsibility, s.namedPlace, s.scope = tt.Incoterm, tt.Edition, tt.ResponsibilityNote, tt.NamedPlace,
		refText(d.TradeTerms)
	parts, err := deliveryParts(d.Delivery)
	if err != nil {
		return sheet{}, err
	}
	s.delivery = parts

	for _, p := range calc.Payments {
		s.payments = append(s.payments, paymentRow{stage: p.StageID, percent: p.Percent, amount: p.AmountMinor, trigger: p.Trigger,
			dayBasis: p.DayBasis, days: p.Days, calendar: p.Calendar})
	}
	if c.LegalTerms != nil {
		if s.legal, err = legalLines(c); err != nil {
			return sheet{}, err
		}
	}
	return s, nil
}

// Printed forms of a formation (quote-text/0.2.0).
var (
	formationRules  = map[string]string{quote.FormationArrival: "acceptance effective on arrival"}
	formationBases  = map[string]string{quote.FormationBasisCISG: "CISG Art. 18(2), 23", quote.FormationBasisPRC: "PRC Civil Code Art. 137, 483, 484"}
	disputeMethods  = []string{quote.DisputeArbitration, quote.DisputeLitigation}
	legalCISGValues = []string{quote.CISGApplies, quote.CISGExcluded}
)

// legalLines prints the legal terms document c seals (quote-text/0.2.0, WBS S3-17):
//
//	Legal terms: <id> r<revision>
//	Governing law: <governing_law> | CISG: <applies or excluded>
//	Contract formation: acceptance effective on arrival | <basis>
//	Disputes: arbitration | <institution> | seat <seat> | language <language>
//	Disputes: litigation | <court>
//
// The revision is the document's legal_terms_ref, pinned by its manifest; the basis is "CISG Art.
// 18(2), 23" or "PRC Civil Code Art. 137, 483, 484". Terms a freeze could not have sealed are
// REFERENCE_INVALID at /legal_terms.
func legalLines(c quote.Commercial) ([]string, error) {
	ref := c.Data.LegalTerms
	if ref == nil {
		return nil, fail(refInvalid, "/data/legal_terms_ref", "The legal terms' revision is needed")
	}
	if !manifested(c, *ref) {
		return nil, fail(refInvalid, "/data/legal_terms_ref", "The dependency manifest does not pin the legal terms")
	}
	return LegalLines(*ref, *c.LegalTerms, "/legal_terms")
}

// LegalLines prints sealed legal terms l of revision ref as every formal document of TRD prints
// them (quote-text/0.2.0's legal_terms section; the order documents' law section): the revision, the
// governing law and the CISG choice, the formation and the dispute resolution. Terms a freeze could
// not have sealed are REFERENCE_INVALID at p.
func LegalLines(ref quote.Ref, l quote.LegalTerms, p string) ([]string, error) {
	rule, basis := formationRules[l.Formation.Rule], formationBases[l.Formation.Basis]
	d := l.DisputeResolution
	switch {
	case blank(l.GoverningLaw) || !slices.Contains(legalCISGValues, l.CISG):
		return nil, fail(refInvalid, p, "Legal terms name the governing law and whether the CISG applies")
	case rule == "" || basis == "":
		return nil, fail(refInvalid, p+"/formation", "The formation is arrival under the CISG or PRC law")
	case !slices.Contains(disputeMethods, d.Method) || (d.Method == quote.DisputeArbitration && (blank(d.Institution) || blank(d.Seat) ||
		blank(d.Language))) || (d.Method == quote.DisputeLitigation && blank(d.Court)):
		return nil, fail(refInvalid, p+"/dispute_resolution", "Disputes go to arbitration with institution, seat and language, or to a court")
	}
	disputes := "Disputes: arbitration | " + d.Institution + " | seat " + d.Seat + " | language " + d.Language
	if d.Method == quote.DisputeLitigation {
		disputes = "Disputes: litigation | " + d.Court
	}
	return []string{"Legal terms: " + refText(ref), "Governing law: " + l.GoverningLaw + " | CISG: " + l.CISG,
		"Contract formation: " + rule + " | " + basis, disputes}, nil
}

// manifested reports whether c's dependency manifest pins r.
func manifested(c quote.Commercial, r quote.Ref) bool {
	return slices.ContainsFunc(c.DependencyManifest, func(d quote.Dependency) bool {
		return d.Kind == r.Kind && d.ID == r.ID && d.Version == r.Revision
	})
}

// recipientAddress returns the address that addresses the quote: the buyer's, else the billing one.
func recipientAddress(addresses []quote.AddressSnapshot) (string, bool) {
	for _, role := range customerAddressRoles {
		for _, a := range addresses {
			if a.Role == role {
				return a.Address, true
			}
		}
	}
	return "", false
}

// Delivery kinds of a frozen document (TRD api.Delivery, CST's form).
const (
	deliveryFixed    = "fixed"
	deliveryLeadTime = "lead_time"
)

// deliveryParts prints the delivery the document binds (data.delivery): Python's "<date> |
// <milestone> | <time zone>" for a fixed date; a lead time prints "<days> <day basis> days after
// <anchor id> r<revision> | calendar <id> r<revision>" in place of the date. Every freeze seals one
// of these forms whole; any other stored delivery is REFERENCE_INVALID.
func deliveryParts(d quote.Delivery) ([]string, error) {
	const p = "/data/delivery"
	var parts []string
	switch d.Kind {
	case deliveryFixed:
		if day, err := time.Parse(time.DateOnly, d.Date); err != nil || day.Format(time.DateOnly) != d.Date {
			return nil, fail(refInvalid, p, "A fixed delivery has a date")
		}
		parts = append(parts, d.Date)
	case deliveryLeadTime:
		if d.Anchor == nil || d.Calendar == nil || blank(d.DayBasis) || d.Days < 0 || d.Days > 3650 {
			return nil, fail(refInvalid, p, "A lead time has an anchor, 0 to 3650 days, a day basis and a calendar")
		}
		parts = append(parts, strconv.Itoa(d.Days)+" "+d.DayBasis+" days after "+d.Anchor.ID+" r"+strconv.FormatInt(d.Anchor.Revision, 10),
			"calendar "+refText(*d.Calendar))
	default:
		return nil, fail(refInvalid, p, "The delivery is a fixed date or a lead time")
	}
	if blank(d.Milestone) || blank(d.TimeZone) {
		return nil, fail(refInvalid, p, "A delivery has a milestone and a time zone")
	}
	return append(parts, d.Milestone, d.TimeZone), nil
}

// pinned checks that r is pinned by c's dependency manifest with content hash hash: a revision the
// manifest does not list is REFERENCE_INVALID, one read with another content REFERENCE_STALE.
func pinned(c quote.Commercial, r quote.Ref, hash, path string) error {
	for _, d := range c.DependencyManifest {
		if d.Kind == r.Kind && d.ID == r.ID && d.Version == r.Revision {
			if d.ContentHash != hash {
				return fail(CodeReferenceStale, path, "The revision read is not the content the freeze pinned")
			}
			return nil
		}
	}
	return fail(refInvalid, path, "The dependency manifest does not pin the revision")
}

func amounts(rows []quote.AmountLine) map[string]quote.AmountLine {
	m := make(map[string]quote.AmountLine, len(rows))
	for _, r := range rows {
		m[r.ID] = r
	}
	return m
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }
