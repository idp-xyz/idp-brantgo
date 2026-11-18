package capability

import (
	"context"
	"reflect"
	"testing"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// completeDraft is the content of a quote draft a person can freeze as far as completeness goes:
// every member of QuoteData. Only presence matters to the gate; the values are placeholders.
func completeDraft() trdapi.QuoteData {
	cfg := func(kind string) *trdapi.ConfigRef {
		return &trdapi.ConfigRef{Kind: kind, ID: kind + ".1", Revision: 1}
	}
	place := trdapi.BusinessRef{Kind: trdapi.PlaceKind, ID: "place.1", Revision: 1}
	evidence := []trdapi.Evidence{{SourceID: "src.1", Locator: "1:1"}}
	notes := "notes"
	return trdapi.QuoteData{
		IssuerRef:           cfg("company_profile"),
		CustomerRef:         &trdapi.PartyRef{PartyID: "customer.1", Revision: 1},
		ContactRef:          &trdapi.ContactRef{ContactID: "contact.1", RowVersion: 1},
		TradingProfileRef:   &trdapi.TradingProfileRef{PartyID: "customer.1", Revision: 1},
		CustomerAddressRefs: []trdapi.AddressRoleRef{{PartyID: "customer.1", PartyRevision: 1, AddressID: "address.1"}},
		RfqRef:              &trdapi.RfqRef{RfqID: "rfq.1", RowVersion: 1},
		CostSnapshotRef:     &trdapi.CostSnapshotRef{SnapshotID: "snapshot.1", ResultHash: "0000000000000000000000000000000000000000000000000000000000000000"},
		Lines: []trdapi.Line{{LineID: "L1", RfqLineID: "L1", Sku: trdapi.SkuRef{SkuID: "sku.1", SpecRevision: 1}, Quantity: "1", UnitRef: *cfg("unit"),
			UnitPrice: "1.00", PriceBasisQuantity: "1", DiscountRatio: "0", TaxApplicationRef: *cfg("tax_application"), Evidence: evidence}},
		CurrencyRef:         cfg("currency"),
		PricingPolicyRef:    cfg("pricing_policy"),
		TradeTermsRef:       cfg("trade_terms"),
		NamedPlaceRef:       &place,
		PaymentTermsRef:     cfg("payment_terms"),
		Delivery:            &trdapi.Delivery{Kind: trdapi.DeliveryFixed, Date: "2027-01-31", Milestone: trdapi.MilestoneHandover, PlaceRef: place, TimeZone: "Asia/Shanghai"},
		DocumentTemplateRef: cfg("quote_document_template"),
		SequenceRef:         cfg("number_sequence"),
		ExpiresAt:           "2026-12-01T00:00:00Z",
		Evidence:            evidence,
		CustomerNotes:       &notes,
	}
}

// What the freeze answers for each member left out, in the freeze's order: the code and the path of
// the first missing member (S4 G6; trd's TestMissingForFreezeIsWhatTheFreezeAsks pins this table to
// the domain's Complete).
var freezeAnswers = []struct {
	member string
	edit   func(*trdapi.QuoteData)
	code   string
	path   string
}{
	{"IssuerRef", func(d *trdapi.QuoteData) { d.IssuerRef = nil }, "DRAFT_NOT_READY", "/args/data/issuer_ref"},
	{"CustomerRef", func(d *trdapi.QuoteData) { d.CustomerRef = nil }, "CUSTOMER_REFERENCE_MISSING", "/args/data/customer_ref"},
	{"ContactRef", func(d *trdapi.QuoteData) { d.ContactRef = nil }, "CONTACT_REFERENCE_MISSING", "/args/data/contact_ref"},
	{"TradingProfileRef", func(d *trdapi.QuoteData) { d.TradingProfileRef = nil }, "DRAFT_NOT_READY", "/args/data/trading_profile_ref"},
	{"CustomerAddressRefs", func(d *trdapi.QuoteData) { d.CustomerAddressRefs = nil }, "DRAFT_NOT_READY", "/args/data/customer_address_refs"},
	{"RfqRef", func(d *trdapi.QuoteData) { d.RfqRef = nil }, "DRAFT_NOT_READY", "/args/data/rfq_ref"},
	{"CostSnapshotRef", func(d *trdapi.QuoteData) { d.CostSnapshotRef = nil }, "COST_SNAPSHOT_MISSING", "/args/data/cost_snapshot_ref"},
	{"Lines", func(d *trdapi.QuoteData) { d.Lines = nil }, "DRAFT_NOT_READY", "/args/data/lines"},
	{"CurrencyRef", func(d *trdapi.QuoteData) { d.CurrencyRef = nil }, "DRAFT_NOT_READY", "/args/data/currency_ref"},
	{"PricingPolicyRef", func(d *trdapi.QuoteData) { d.PricingPolicyRef = nil }, "DRAFT_NOT_READY", "/args/data/pricing_policy_ref"},
	{"TradeTermsRef", func(d *trdapi.QuoteData) { d.TradeTermsRef = nil }, "DRAFT_NOT_READY", "/args/data/trade_terms_ref"},
	{"NamedPlaceRef", func(d *trdapi.QuoteData) { d.NamedPlaceRef = nil }, "DRAFT_NOT_READY", "/args/data/named_place_ref"},
	{"PaymentTermsRef", func(d *trdapi.QuoteData) { d.PaymentTermsRef = nil }, "DRAFT_NOT_READY", "/args/data/payment_terms_ref"},
	{"Delivery", func(d *trdapi.QuoteData) { d.Delivery = nil }, "DRAFT_NOT_READY", "/args/data/delivery"},
	{"DocumentTemplateRef", func(d *trdapi.QuoteData) { d.DocumentTemplateRef = nil }, "DRAFT_NOT_READY", "/args/data/document_template_ref"},
	{"SequenceRef", func(d *trdapi.QuoteData) { d.SequenceRef = nil }, "DRAFT_NOT_READY", "/args/data/sequence_ref"},
	{"ExpiresAt", func(d *trdapi.QuoteData) { d.ExpiresAt = "" }, "DRAFT_NOT_READY", "/args/data/expires_at"},
	{"Evidence", func(d *trdapi.QuoteData) { d.Evidence = nil }, "DRAFT_NOT_READY", "/args/data/evidence"},
}

// The agent's quote.save_draft is refused before TRD is called when its Data lacks what a freeze
// needs: each member left out is refused with the code and the path the freeze answers for it, the
// first missing one when several are, and the refusal's reason is a static text the model is
// allowed to be told. A complete draft passes through unchanged (S4 G6).
func TestQ01SaveDraftIsRefusedWhenAFreezeWouldBeRefused(t *testing.T) {
	reg, err := Q01Registry(Q01Targets{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := reg.Lookup("tenant.1", trdapi.CommandSaveDraft, "/command")
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(d trdapi.QuoteData) (any, error) {
		return c.Prepare(context.Background(), trdapi.SaveDraft{QuoteID: "quote.1", ExpectedVersion: 0, Data: d})
	}
	whole := trdapi.SaveDraft{QuoteID: "quote.1", Data: completeDraft()}
	got, err := prepare(whole.Data)
	if err != nil || !reflect.DeepEqual(got, any(whole)) {
		t.Fatalf("a complete draft: %+v %v", got, err)
	}
	for _, m := range freezeAnswers {
		d := completeDraft()
		m.edit(&d)
		_, err := prepare(d)
		rej, ok := kernel.AsRejection(err)
		if !ok || rej.Code != m.code || len(rej.Fields) != 1 || rej.Fields[0].Path != m.path {
			t.Errorf("without %s: %v, want %s at %s", m.member, err, m.code, m.path)
			continue
		}
		if StatedReason(rej.Code, rej.Fields[0].Reason) != notFreezable {
			t.Errorf("without %s: the reason %q would be withheld from the model", m.member, rej.Fields[0].Reason)
		}
		// The path names a member of the tool's arguments the model can find and resend.
		if mem, ok := c.Member(m.path); !ok || mem.Path != "Data."+m.member {
			t.Errorf("without %s: the path %s names %q (%v)", m.member, m.path, mem.Path, ok)
		}
	}
	// Several members missing: the first in the freeze's order, however many.
	d := completeDraft()
	for i := len(freezeAnswers) - 1; i >= 0; i-- {
		freezeAnswers[i].edit(&d)
		want := freezeAnswers[i]
		if _, err := prepare(d); code(err) != want.code || path(err) != want.path {
			t.Errorf("members %s and every later one missing: %v, want %s at %s", want.member, err, want.code, want.path)
		}
	}
	// The members a freeze does not need change nothing; the draft is not otherwise judged here.
	d = completeDraft()
	d.CustomerNotes, d.Charges, d.SalesFXRef, d.LegalTermsRef = nil, nil, nil, nil
	d.ExpiresAt, d.Lines[0].UnitPrice = "not an instant", "not a price"
	if got, err := prepare(d); err != nil || !reflect.DeepEqual(got, any(trdapi.SaveDraft{QuoteID: "quote.1", Data: d})) {
		t.Fatalf("a draft with every member needed and others optional left out or invalid: %+v %v", got, err)
	}
}

// The gate is the agent's alone and quote.save_draft only: the other quote commands pass through
// their preparation (none) as before.
func TestOnlyTheAgentsQuoteSaveDraftHasTheFreezeGate(t *testing.T) {
	reg, err := Q01Registry(Q01Targets{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{trdapi.CommandRevise, trdapi.CommandUpdateMetadata} {
		c, err := reg.Lookup("tenant.1", name, "/command")
		if err != nil {
			t.Fatal(err)
		}
		cmd := reflect.New(c.Input()).Elem().Interface()
		if got, err := c.Prepare(context.Background(), cmd); err != nil || !reflect.DeepEqual(got, cmd) {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	// TRD's own contract for a person's save is untouched: an empty draft is valid there.
	if err := (trdapi.SaveDraft{IdempotencyKey: "k1", QuoteID: "quote.1"}).Validate(); err != nil {
		t.Fatalf("a person's partial draft is no longer valid: %v", err)
	}
}

// path is the first path of err's rejection, "" when it is none.
func path(err error) string {
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 {
		return rej.Fields[0].Path
	}
	return ""
}
