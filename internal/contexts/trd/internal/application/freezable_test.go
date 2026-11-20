package application

import (
	"math/bits"
	"reflect"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What a freeze needs of a draft is the domain's rule (quote.Data.Complete); api.MissingForFreeze
// publishes the same rule over the api's QuoteData for callers that may not import the domain, such
// as the AI layer's gateway (S4 G6). Neither package can call the other, so this test is what makes
// them one rule: the application is the one place that imports both and converts between them.
//
// The members of QuoteData in the order the freeze looks at them, with the answer for each when it is
// the first one missing.
type freezeMember struct {
	field string // the api QuoteData field
	clear func(*api.QuoteData)
	code  string
	path  string
}

var freezeMembers = []freezeMember{
	{"IssuerRef", func(d *api.QuoteData) { d.IssuerRef = nil }, kernel.CodeDraftNotReady, "/args/data/issuer_ref"},
	{"CustomerRef", func(d *api.QuoteData) { d.CustomerRef = nil }, api.CodeCustomerReferenceMiss, "/args/data/customer_ref"},
	{"ContactRef", func(d *api.QuoteData) { d.ContactRef = nil }, api.CodeContactReferenceMiss, "/args/data/contact_ref"},
	{"TradingProfileRef", func(d *api.QuoteData) { d.TradingProfileRef = nil }, kernel.CodeDraftNotReady, "/args/data/trading_profile_ref"},
	{"CustomerAddressRefs", func(d *api.QuoteData) { d.CustomerAddressRefs = nil }, kernel.CodeDraftNotReady, "/args/data/customer_address_refs"},
	{"RfqRef", func(d *api.QuoteData) { d.RfqRef = nil }, kernel.CodeDraftNotReady, "/args/data/rfq_ref"},
	{"CostSnapshotRef", func(d *api.QuoteData) { d.CostSnapshotRef = nil }, api.CodeCostSnapshotMissing, "/args/data/cost_snapshot_ref"},
	{"Lines", func(d *api.QuoteData) { d.Lines = nil }, kernel.CodeDraftNotReady, "/args/data/lines"},
	{"CurrencyRef", func(d *api.QuoteData) { d.CurrencyRef = nil }, kernel.CodeDraftNotReady, "/args/data/currency_ref"},
	{"PricingPolicyRef", func(d *api.QuoteData) { d.PricingPolicyRef = nil }, kernel.CodeDraftNotReady, "/args/data/pricing_policy_ref"},
	{"TradeTermsRef", func(d *api.QuoteData) { d.TradeTermsRef = nil }, kernel.CodeDraftNotReady, "/args/data/trade_terms_ref"},
	{"NamedPlaceRef", func(d *api.QuoteData) { d.NamedPlaceRef = nil }, kernel.CodeDraftNotReady, "/args/data/named_place_ref"},
	{"PaymentTermsRef", func(d *api.QuoteData) { d.PaymentTermsRef = nil }, kernel.CodeDraftNotReady, "/args/data/payment_terms_ref"},
	{"Delivery", func(d *api.QuoteData) { d.Delivery = nil }, kernel.CodeDraftNotReady, "/args/data/delivery"},
	{"DocumentTemplateRef", func(d *api.QuoteData) { d.DocumentTemplateRef = nil }, kernel.CodeDraftNotReady, "/args/data/document_template_ref"},
	{"SequenceRef", func(d *api.QuoteData) { d.SequenceRef = nil }, kernel.CodeDraftNotReady, "/args/data/sequence_ref"},
	{"ExpiresAt", func(d *api.QuoteData) { d.ExpiresAt = "" }, kernel.CodeDraftNotReady, "/args/data/expires_at"},
	{"Evidence", func(d *api.QuoteData) { d.Evidence = nil }, kernel.CodeDraftNotReady, "/args/data/evidence"},
}

// freezeOptional are the members a freeze does not need: absent or present, the draft is complete.
var freezeOptional = []struct {
	field string
	clear func(*api.QuoteData)
}{
	{"Charges", func(d *api.QuoteData) { d.Charges = nil }},
	{"SalesFXRef", func(d *api.QuoteData) { d.SalesFXRef = nil }},
	{"CustomerNotes", func(d *api.QuoteData) { d.CustomerNotes = nil }},
	{"LegalTermsRef", func(d *api.QuoteData) { d.LegalTermsRef = nil }},
	// conversion_mode is optional: omitted, the freeze seals today's single full order (TRD-INV-024).
	{"ConversionMode", func(d *api.QuoteData) { d.ConversionMode = "" }},
}

// completeQuote has every member of QuoteData. Only presence matters to either rule: the values are
// placeholders, which the freeze judges against the upstream facts (and the contract against its
// formats) elsewhere.
func completeQuote() api.QuoteData {
	cfg := func(kind string) *api.ConfigRef { return &api.ConfigRef{Kind: kind, ID: kind + ".1", Revision: 1} }
	place := api.BusinessRef{Kind: api.PlaceKind, ID: "place.1", Revision: 1}
	evidence := []api.Evidence{{SourceID: "src.1", Locator: "1:1"}}
	notes := "notes"
	return api.QuoteData{
		IssuerRef:           cfg("company_profile"),
		CustomerRef:         &api.PartyRef{PartyID: "customer.1", Revision: 1},
		ContactRef:          &api.ContactRef{ContactID: "contact.1", RowVersion: 1},
		TradingProfileRef:   &api.TradingProfileRef{PartyID: "customer.1", Revision: 1},
		CustomerAddressRefs: []api.AddressRoleRef{{PartyID: "customer.1", PartyRevision: 1, AddressID: "address.1"}},
		RfqRef:              &api.RfqRef{RfqID: "rfq.1", RowVersion: 1},
		CostSnapshotRef:     &api.CostSnapshotRef{SnapshotID: "snapshot.1", ResultHash: "0000000000000000000000000000000000000000000000000000000000000000"},
		Lines: []api.Line{{LineID: "L1", RfqLineID: "L1", Sku: api.SkuRef{SkuID: "sku.1", SpecRevision: 1}, Quantity: "1", UnitRef: *cfg("unit"),
			UnitPrice: "1.00", PriceBasisQuantity: "1", DiscountRatio: "0", TaxApplicationRef: *cfg("tax_application"), Evidence: evidence}},
		Charges: []api.Charge{{ChargeID: "C1", FeeCategoryRef: *cfg("fee_category"), ObligationKey: "o.1", Amount: "1.00", Inclusion: api.InclusionSeparate,
			TaxApplicationRef: *cfg("tax_application"), Evidence: evidence}},
		CurrencyRef:         cfg("currency"),
		PricingPolicyRef:    cfg("pricing_policy"),
		SalesFXRef:          cfg("fx_rate"),
		TradeTermsRef:       cfg("trade_terms"),
		NamedPlaceRef:       &place,
		PaymentTermsRef:     cfg("payment_terms"),
		Delivery:            &api.Delivery{Kind: api.DeliveryFixed, Date: "2027-01-31", Milestone: api.MilestoneHandover, PlaceRef: place, TimeZone: "Asia/Shanghai"},
		DocumentTemplateRef: cfg("quote_document_template"),
		SequenceRef:         cfg("number_sequence"),
		ExpiresAt:           "2026-12-01T00:00:00Z",
		CustomerNotes:       &notes,
		Evidence:            evidence,
		LegalTermsRef:       cfg("legal_terms"),
	}
}

// asked is the answer of both rules for d, which must be one answer.
func asked(t *testing.T, name string, d api.QuoteData) error {
	t.Helper()
	published, domain := api.MissingForFreeze(d), quoteData(d).Complete()
	if !reflect.DeepEqual(published, domain) {
		t.Fatalf("%s: api.MissingForFreeze answers %#v, the freeze's Complete answers %#v", name, published, domain)
	}
	return published
}

func rejection(t *testing.T, name string, err error) *kernel.Rejection {
	t.Helper()
	rej, ok := kernel.AsRejection(err)
	if !ok || len(rej.Fields) != 1 {
		t.Fatalf("%s: %v is not one rejection", name, err)
	}
	return rej
}

// A complete draft is complete for both; each member left out, alone, is refused by both the same
// way — code, category, path and reason — and as the table says; the empty draft is refused at the
// first member.
func TestMissingForFreezeIsWhatTheFreezeAsks(t *testing.T) {
	if err := asked(t, "the complete draft", completeQuote()); err != nil {
		t.Fatalf("the complete draft: %v", err)
	}
	for _, m := range freezeMembers {
		d := completeQuote()
		m.clear(&d)
		rej := rejection(t, m.field, asked(t, "without "+m.field, d))
		if rej.Code != m.code || rej.Fields[0].Path != m.path {
			t.Errorf("without %s: %s at %s, want %s at %s", m.field, rej.Code, rej.Fields[0].Path, m.code, m.path)
		}
		if want, ok := api.Category(m.code); m.code != kernel.CodeDraftNotReady && (!ok || rej.Category != want) {
			t.Errorf("without %s: category %s, the api declares %s", m.field, rej.Category, want)
		}
	}
	first := freezeMembers[0]
	if rej := rejection(t, "the empty draft", asked(t, "the empty draft", api.QuoteData{})); rej.Code != first.code || rej.Fields[0].Path != first.path {
		t.Errorf("the empty draft: %s at %s", rej.Code, rej.Fields[0].Path)
	}
}

// Every combination of absent members: both rules name the first one missing in the freeze's order
// (2^18 drafts), which is the first of the combination in the table.
func TestMissingForFreezeNamesTheFirstOfAnyCombination(t *testing.T) {
	n := len(freezeMembers)
	for mask := 1; mask < 1<<n; mask++ {
		d := completeQuote()
		for i, m := range freezeMembers {
			if mask&(1<<i) != 0 {
				m.clear(&d)
			}
		}
		published, domain := api.MissingForFreeze(d), quoteData(d).Complete()
		if !reflect.DeepEqual(published, domain) {
			t.Fatalf("members %b missing: api.MissingForFreeze answers %v, Complete answers %v", mask, published, domain)
		}
		first := freezeMembers[bits.TrailingZeros(uint(mask))] // bit i is member i of the table: the lowest set bit is the first missing
		if rej, ok := kernel.AsRejection(published); !ok || rej.Code != first.code || rej.Fields[0].Path != first.path {
			t.Fatalf("members %b missing: %v, the first is %s", mask, published, first.field)
		}
	}
}

// The members a freeze does not need change nothing, whether present or absent, and every member of
// QuoteData is one or the other: a member added to the contract is a decision about the freeze, and
// this test fails until it is taken in both rules.
func TestEveryMemberOfQuoteDataIsRequiredOrOptionalForTheFreeze(t *testing.T) {
	for _, o := range freezeOptional {
		d := completeQuote()
		o.clear(&d)
		if err := asked(t, "without "+o.field, d); err != nil {
			t.Errorf("without %s: %v", o.field, err)
		}
	}
	bare := completeQuote()
	for _, m := range freezeMembers {
		m.clear(&bare)
	}
	for _, o := range freezeOptional {
		o.clear(&bare)
	}
	if !reflect.DeepEqual(bare, api.QuoteData{}) {
		t.Errorf("QuoteData has members neither list names: %+v", bare)
	}
	seen := map[string]bool{}
	for _, m := range freezeMembers {
		seen[m.field] = true
	}
	for _, o := range freezeOptional {
		seen[o.field] = true
	}
	typ := reflect.TypeFor[api.QuoteData]()
	for i := range typ.NumField() {
		if name := typ.Field(i).Name; !seen[name] {
			t.Errorf("QuoteData.%s is neither required nor optional for the freeze in this test", name)
		}
	}
	if len(seen) != typ.NumField() {
		t.Errorf("the lists name %d members, QuoteData has %d", len(seen), typ.NumField())
	}
}

// What counts as absent is what the domain counts: a list with no item, an empty instant. A member
// that is present is not judged — not its form, its kind nor its existence: that is the freeze's
// later checks and the contract's.
func TestMissingForFreezeDecidesPresenceOnly(t *testing.T) {
	for path, edit := range map[string]func(*api.QuoteData){
		"/args/data/customer_address_refs": func(d *api.QuoteData) { d.CustomerAddressRefs = []api.AddressRoleRef{} },
		"/args/data/lines":                 func(d *api.QuoteData) { d.Lines = []api.Line{} },
		"/args/data/evidence":              func(d *api.QuoteData) { d.Evidence = []api.Evidence{} },
	} {
		d := completeQuote()
		edit(&d)
		if rej := rejection(t, path, asked(t, "empty "+path, d)); rej.Code != kernel.CodeDraftNotReady || rej.Fields[0].Path != path {
			t.Errorf("an empty list at %s: %v", path, rej)
		}
	}
	odd := completeQuote()
	odd.ExpiresAt = "next week"
	odd.IssuerRef = &api.ConfigRef{Kind: "not_a_kind", ID: "x", Revision: 1}
	odd.Lines = []api.Line{{}}
	if err := asked(t, "members present but not valid", odd); err != nil {
		t.Errorf("a present member is not judged: %v", err)
	}
}
