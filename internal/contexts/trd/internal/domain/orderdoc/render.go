package orderdoc

import (
	"strconv"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/document"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Facts are what a document prints that the order revision names by reference only, each resolved
// by the command from the exact revision the order revision binds, before its transaction. Prepare
// refuses a fact of another reference (REFERENCE_INVALID at /facts/<member>). Unit and currency
// codes need no fact (a POL unit's and currency's config_id is its code), nor do the parties'
// snapshots and the delivery, which the revision holds.
type Facts struct {
	// Customer is the customer's name at the bound party revision (PTY PartyRevision.Name).
	Customer Party
	// Contact is the contact's name at the bound row version (PTY ContactView.Name).
	Contact Contact
	// Items name each line's SKU specification revision (CAT).
	Items map[quote.SkuRef]Item
	// Currency is the order currency's revision (POL currency) and its minor digits.
	Currency Currency
	// TradeTerms is the revision's trade terms (POL trade_terms) as a document prints them.
	TradeTerms TradeTerms
	// Sequence is the POL number_sequence revision the number is issued from and its document type.
	Sequence Sequence
}

// Party is a party at a confirmed revision and its name.
type Party struct {
	Ref  quote.PartyRef
	Name string
}

// Contact is a contact at a row version and its name.
type Contact struct {
	Ref  quote.ContactRef
	Name string
}

// Item is how a document names a SKU specification revision.
type Item struct {
	Name          string
	Specification string
}

// Currency is a POL currency revision and its minor units (0–4).
type Currency struct {
	Ref        quote.Ref
	MinorUnits int
}

// TradeTerms is a POL trade_terms revision and what a document prints of it.
type TradeTerms struct {
	Ref                quote.Ref
	Incoterm           string
	Edition            string
	ResponsibilityNote string
	NamedPlace         string
}

// Sequence is a POL number_sequence revision and its document type.
type Sequence struct {
	Ref          quote.Ref
	DocumentType string
}

// sheet is an order document as order-text/0.1.0 prints it: every value in its printed form.
type sheet struct {
	kind                                 string
	sellerName, sellerAddress, sellerReg string
	sellerCountry                        string
	buyerName, buyerAddress, contact     string
	number, orderID, orderHash           string
	orderRevision                        int64
	quote, amendment, date               string
	lines, charges                       []string
	currency, net, tax, total            string
	incoterm, edition, responsibility    string
	namedPlace, tradeTerms               string
	delivery                             []string
	payments                             []string
	clauses, legal                       []string
	note                                 string
}

// text renders s with t: the title, a blank line, the sections in t's order separated by blank
// lines, and a final line break.
func text(t Template, s sheet) []byte {
	var b strings.Builder
	b.WriteString(t.Title)
	b.WriteString("\n\n")
	for i, name := range t.Sections {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(block(s, name))
	}
	b.WriteString("\n")
	return []byte(b.String())
}

func block(s sheet, name string) string {
	switch name {
	case SectionSeller:
		return "Seller: " + s.sellerName + "\n" + s.sellerAddress + "\nRegistration: " + s.sellerReg + " | Country: " + s.sellerCountry
	case SectionBuyer:
		return "Buyer: " + s.buyerName + "\n" + s.buyerAddress + "\nContact: " + s.contact
	case SectionIdentity:
		out := []string{"Number: " + s.number, "Order: " + s.orderID + " r" + strconv.FormatInt(s.orderRevision, 10), "Order SHA-256: " + s.orderHash,
			"Quote: " + s.quote}
		if s.amendment != "" {
			out = append(out, "Amendment: "+s.amendment)
		}
		out = append(out, "Date: "+s.date)
		if s.kind == KindProformaInvoice {
			out = append(out, "Not a commercial invoice or a tax invoice.")
		}
		return strings.Join(out, "\n")
	case SectionLines:
		return "Lines (amounts in " + s.currency + "):\n" + strings.Join(append(append([]string{}, s.lines...), s.charges...), "\n")
	case SectionTotals:
		return "Currency: " + s.currency + "\nNet: " + s.net + " | Tax: " + s.tax + " | Total: " + s.total
	case SectionTerms:
		return "Terms: " + s.incoterm + " " + s.edition + " | " + s.responsibility + "\nNamed place: " + s.namedPlace + " | Trade terms: " + s.tradeTerms
	case SectionDelivery:
		return "Delivery: " + strings.Join(s.delivery, " | ")
	case SectionPayments:
		return "Payment plan (planned amounts, not receipts):\n" + strings.Join(s.payments, "\n")
	case SectionClauses:
		out := make([]string, len(s.clauses))
		for i, c := range s.clauses {
			out[i] = strconv.Itoa(i+1) + ". " + c
		}
		return "Clauses:\n" + strings.Join(out, "\n")
	case SectionLaw:
		return strings.Join(s.legal, "\n")
	case SectionSignatures:
		return "Signatures (this generated copy is unsigned; signed copies are recorded as evidence):\nSeller: " + s.sellerName + "\nBuyer: " + s.buyerName
	case SectionNotes:
		return s.note
	}
	panic("orderdoc: unchecked section " + name) // Supported admits the known sections only
}

// compose reads revision r, template t and the facts into a sheet, without the number.
func compose(r *orderrev.Revision, t Template, f Facts, at time.Time) (sheet, error) {
	issuer := r.Parties.Issuer
	s := sheet{kind: t.DocumentKind, sellerName: issuer.LegalName, sellerAddress: issuer.Address, sellerReg: issuer.RegistrationNo,
		sellerCountry: issuer.Country, orderID: r.OrderID, orderRevision: r.Number, orderHash: r.Hash,
		quote: r.Source.Number + " (" + r.Source.QuoteID + " r" + strconv.FormatInt(r.Source.Revision, 10) + ") | Acceptance: " + r.Source.AcceptanceID,
		date:  kernel.FormatTimestamp(at), clauses: t.Clauses, note: t.FooterNote}
	if blank(issuer.LegalName) || blank(issuer.Address) {
		return sheet{}, fail(kernel.CodeReferenceInvalid, "/revision/parties/issuer", "The seller's legal name and address are needed")
	}
	if r.Amendment != nil && r.Previous != nil {
		s.amendment = r.Amendment.ID + " of r" + strconv.FormatInt(r.Previous.Revision, 10)
	}

	if f.Customer.Ref != r.Customer || blank(f.Customer.Name) {
		return sheet{}, fail(kernel.CodeReferenceInvalid, "/facts/customer", "The customer's name at the bound party revision is needed")
	}
	s.buyerName = f.Customer.Name
	address, ok := buyerAddress(r.Parties.Customer.Addresses)
	if !ok {
		return sheet{}, fail(kernel.CodeReferenceInvalid, "/revision/parties/customer/addresses", "A buyer or billing address addresses the document")
	}
	s.buyerAddress = address
	if f.Contact.Ref != r.Contact || blank(f.Contact.Name) {
		return sheet{}, fail(kernel.CodeReferenceInvalid, "/facts/contact", "The contact's name at the bound row version is needed")
	}
	s.contact = f.Contact.Name

	if f.Currency.Ref != r.Currency || f.Currency.MinorUnits < 0 || f.Currency.MinorUnits > 4 {
		return sheet{}, fail(kernel.CodeReferenceInvalid, "/facts/currency", "The order currency's revision and minor units (0 to 4) are needed")
	}
	c, err := kernel.NewCurrency(r.Currency.ID, f.Currency.MinorUnits)
	if err != nil {
		return sheet{}, fail(kernel.CodeReferenceInvalid, "/facts/currency", err.Error())
	}
	s.currency = c.Code()
	amount := func(minor, path string) (string, error) {
		m, err := kernel.ParseMinor(c, minor)
		if err != nil {
			return "", fail(kernel.CodeReferenceInvalid, path, "An amount is an integer of minor units")
		}
		return m.Amount(), nil
	}

	for i, l := range r.Lines {
		at := "/revision/lines/" + strconv.Itoa(i)
		item, ok := f.Items[l.Sku]
		if !ok || blank(item.Name) || blank(item.Specification) {
			return sheet{}, fail(kernel.CodeReferenceInvalid, "/facts/items/"+l.Sku.SkuID, "The item and specification names are needed")
		}
		a, ok := r.LineAmounts(l.LineID)
		if !ok {
			return sheet{}, fail(kernel.CodeReferenceInvalid, at, "The line has no amounts")
		}
		parts := []string{l.LineID, item.Name, item.Specification, l.Quantity + " " + l.Unit.ID,
			"price " + l.UnitPrice + " per " + l.PriceBasisQuantity + " " + l.Unit.ID, "discount " + l.DiscountRatio}
		for _, x := range []struct{ label, minor string }{{"net", a.NetMinor}, {"tax", a.TaxMinor}, {"total", a.PayableMinor}} {
			v, err := amount(x.minor, at)
			if err != nil {
				return sheet{}, err
			}
			parts = append(parts, x.label+" "+v)
		}
		s.lines = append(s.lines, strings.Join(parts, " | "))
	}
	for i, ch := range r.Charges {
		at := "/revision/charges/" + strconv.Itoa(i)
		a, ok := r.ChargeAmounts(ch.ChargeID)
		if !ok {
			return sheet{}, fail(kernel.CodeReferenceInvalid, at, "The charge has no amounts")
		}
		parts := []string{"charge " + ch.ChargeID, ch.ObligationKey, ch.Inclusion}
		for _, x := range []struct{ label, minor string }{{"net", a.NetMinor}, {"tax", a.TaxMinor}, {"total", a.PayableMinor}} {
			v, err := amount(x.minor, at)
			if err != nil {
				return sheet{}, err
			}
			parts = append(parts, x.label+" "+v)
		}
		s.charges = append(s.charges, strings.Join(parts, " | "))
	}
	for _, x := range []struct {
		to    *string
		minor string
	}{{&s.net, r.Amounts.Net}, {&s.tax, r.Amounts.Tax}, {&s.total, r.Amounts.Payable}} {
		if *x.to, err = amount(x.minor, "/revision/amounts"); err != nil {
			return sheet{}, err
		}
	}

	tt := f.TradeTerms
	if tt.Ref != r.Terms.TradeTerms || blank(tt.Incoterm) || blank(tt.Edition) {
		return sheet{}, fail(kernel.CodeReferenceInvalid, "/facts/trade_terms", "The trade terms' Incoterm and edition at the bound revision are needed")
	}
	s.incoterm, s.edition, s.responsibility, s.namedPlace = tt.Incoterm, tt.Edition, tt.ResponsibilityNote, tt.NamedPlace
	s.tradeTerms = r.Terms.TradeTerms.ID + " r" + strconv.FormatInt(r.Terms.TradeTerms.Revision, 10)
	if s.delivery, err = deliveryParts(r.Terms.Delivery); err != nil {
		return sheet{}, err
	}
	for i, p := range r.Amounts.Payments {
		v, err := amount(p.AmountMinor, "/revision/amounts/payments/"+strconv.Itoa(i))
		if err != nil {
			return sheet{}, err
		}
		basis := p.DayBasis
		if basis == "" {
			basis = "calendar" // POL: days without a day basis are calendar days
		}
		parts := []string{p.StageID, p.Percent + "%", v, p.Trigger, strconv.Itoa(p.Days) + " " + basis + " days"}
		if p.Calendar != nil {
			parts = append(parts, "calendar "+p.Calendar.ID+" r"+strconv.FormatInt(p.Calendar.Revision, 10))
		}
		s.payments = append(s.payments, strings.Join(parts, " | "))
	}
	if r.LegalTerms != nil && r.LegalTermsRef != nil {
		if s.legal, err = document.LegalLines(*r.LegalTermsRef, *r.LegalTerms, "/revision/legal_terms"); err != nil {
			return sheet{}, err
		}
	}
	return s, nil
}

// buyerAddress returns the address that addresses the buyer: the buyer's, else the billing one.
func buyerAddress(addresses []quote.AddressSnapshot) (string, bool) {
	for _, role := range []string{"buyer", "billing"} {
		for _, a := range addresses {
			if a.Role == role && !blank(a.Address) {
				return a.Address, true
			}
		}
	}
	return "", false
}

// deliveryParts prints the order's delivery as quote-text/0.1.0 prints a quote's.
func deliveryParts(d quote.Delivery) ([]string, error) {
	bad := func() ([]string, error) {
		return nil, fail(kernel.CodeReferenceInvalid, "/revision/terms/delivery", "The delivery is a fixed date or a lead time, with a milestone and a time zone")
	}
	var parts []string
	switch d.Kind {
	case "fixed":
		if day, err := time.Parse(time.DateOnly, d.Date); err != nil || day.Format(time.DateOnly) != d.Date {
			return bad()
		}
		parts = append(parts, d.Date)
	case "lead_time":
		if d.Anchor == nil || d.Calendar == nil || blank(d.DayBasis) {
			return bad()
		}
		parts = append(parts, strconv.Itoa(d.Days)+" "+d.DayBasis+" days after "+d.Anchor.ID+" r"+strconv.FormatInt(d.Anchor.Revision, 10),
			"calendar "+d.Calendar.ID+" r"+strconv.FormatInt(d.Calendar.Revision, 10))
	default:
		return bad()
	}
	if blank(d.Milestone) || blank(d.TimeZone) {
		return bad()
	}
	return append(parts, d.Milestone, d.TimeZone), nil
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }
