package quote

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// goVectors are TRD's own golden commercial documents: the fixture's freeze with its server
// calculation, passing the minimum margin and below it (BC-AT06's 7.00 against 660.00 at 0.25),
// each with the canonical bytes and the hash that Python V0.50
// protocol.canonical(protocol.normalize(document)) gives for the document's JSON (protocol's
// schema check aside: the document has TRD's references). Regenerate with TRD_WRITE_VECTORS=1 and
// check the result with the Python oracle before committing it.
const goVectors = "testdata/trd-commercial-vectors.json"

type goVector struct {
	Name       string          `json:"name"`
	Document   json.RawMessage `json:"document"`
	Normalized string          `json:"normalized_canonical"`
	SHA256     string          `json:"sha256"`
}

// TRD-INV-002 and WBS S3-01 (哈希跨实现互操作向量，含中文与换行), S3-02: the freeze of the fixture
// seals a normalized canonical document, with the server calculation, whose bytes and SHA-256 are
// pinned, with Chinese text and line breaks kept, decimals trimmed, and the document JSON
// reproducing the same bytes through the normalization alone.
func TestCommercialDocumentGoldenVectors(t *testing.T) {
	below := draft()
	below.Lines[0].UnitPrice = "7.00"
	cases := []struct {
		name    string
		d       Data
		minimum string
		rule    string
	}{
		{"fixture with server calculation", draft(), "0.1", MarginPass},
		{"BC-AT06 below the minimum margin", below, "0.25", MarginRequiresApproval},
	}
	var got []goVector
	for _, c := range cases {
		s, err := sealed(c.d, c.minimum)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if s.BusinessHash != Hash(s.Commercial) || !strings.Contains(string(s.Commercial), `\nKeep this line break.`) ||
			!strings.Contains(string(s.Commercial), "合成核实依据") || !strings.Contains(string(s.Commercial), `"quantity":"120"`) {
			t.Fatalf("%s: %s", c.name, s.Commercial)
		}
		doc, err := ParseCommercial(s.Commercial, s.BusinessHash)
		if err != nil || doc.QuoteID != "quote.1" || doc.BusinessRevision != 1 || doc.IssuedAt != "2026-10-22T09:00:00.000000Z" ||
			doc.NormalizationVersion != NormalizationVersion || doc.Calculation == nil || doc.Calculation.MarginRule != c.rule ||
			doc.Calculation.CalculationVersion != CalculationVersion || doc.Data.NamedPlace != deham || doc.Data.Delivery != handover {
			t.Fatalf("%s: parsed %+v, %v", c.name, doc, err)
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, goVector{Name: c.name, Document: raw, Normalized: string(s.Commercial), SHA256: s.BusinessHash})
	}
	if os.Getenv("TRD_WRITE_VECTORS") == "1" {
		out, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(goVectors, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(goVectors)
	if err != nil {
		t.Fatal(err)
	}
	var want []goVector
	if err = json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Fatalf("%d vectors, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Normalized != want[i].Normalized || got[i].SHA256 != want[i].SHA256 {
			t.Fatalf("%s: commercial document changed:\n%s\nwant\n%s", want[i].Name, got[i].Normalized, want[i].Normalized)
		}
		tree, err := Tree(want[i].Document)
		if err != nil {
			t.Fatal(err)
		}
		canonical, hash, err := Digest(tree)
		if err != nil || string(canonical) != want[i].Normalized || hash != want[i].SHA256 {
			t.Fatalf("%s: the document does not normalize to its bytes: %v", want[i].Name, err)
		}
	}
	if want[0].SHA256 == want[1].SHA256 {
		t.Fatal("the price and the calculation are part of the business hash")
	}
}

// WBS S3-02 wired into the freeze (Python test_t03_server_calculation.py
// test_freeze_ignores_client_calculation_and_persists_server_result, PYT-0442, the domain half):
// the sealed calculation is the server's — 120 × 8.00 USD is 960.00 net, 0 tax, 960.00 payable,
// against the 660.00 USD cost a profit of 300.00 (31.25%, above 0.1), one stage of 960.00 — with
// the scope and terms bindings and the calculation version, and the margin below the minimum is
// sealed as requires_approval with its exact shortfall (BC-AT06: 840 − 660 = 180 against 0.25 ×
// 840 = 210, short by 30).
func TestFrozenRevisionCarriesTheServerCalculation(t *testing.T) {
	s, err := sealed(draft(), "0.1")
	if err != nil {
		t.Fatal(err)
	}
	c := s.Calculation
	if c.NetMinor != "96000" || c.TaxMinor != "0" || c.PayableMinor != "96000" || c.ComparisonRevenueMinor != "96000" || c.CostMinor != "66000" ||
		c.ProfitMinor != "30000" || c.MarginRule != MarginPass || c.MinimumMargin != "0.1" || c.MarginShortfall != "0" ||
		len(c.Payments) != 1 || c.Payments[0].AmountMinor != "96000" || c.Payments[0].Trigger != "before_shipment" ||
		c.ComparisonCurrency != usd || c.SalesFX != nil || c.CostScope.PricingPolicy != policy || len(c.CostScope.Lines) != 1 ||
		c.CostScope.Lines[0].NormalizedQuantity != "120" || c.CostScope.Delivery != handover || len(c.SalesTerms.Taxes) != 1 ||
		c.SalesTerms.Taxes[0].Role != "line:L1" || c.SalesTerms.Taxes[0].Rate != "0" || len(c.ExcludedCostCategories) != 3 {
		t.Fatalf("calculation %+v", c)
	}
	below := draft()
	below.Lines[0].UnitPrice = "7"
	s, err = sealed(below, "0.25")
	if err != nil {
		t.Fatal(err)
	}
	if c := s.Calculation; c.MarginRule != MarginRequiresApproval || c.MarginShortfall != "30" || c.ProfitMinor != "18000" ||
		c.MinimumMargin != "0.25" {
		t.Fatalf("below the minimum %+v", c)
	}
	if _, err := SealDraft("quote.1", 1, draft(), facts(), nil, issuedAt); !errors.Is(err, ErrNoCalculation) {
		t.Fatalf("a freeze without the calculation: %v", err)
	}
}

// TRD-INV-002: what the customer is offered changes the hash; another spelling of the same decimals
// or another order of the lines' evidence does not.
func TestBusinessHashCoversCommercialContentOnly(t *testing.T) {
	base, err := sealed(draft(), "0.1")
	if err != nil {
		t.Fatal(err)
	}
	same := draft()
	same.Lines[0].Quantity, same.Lines[0].UnitPrice, same.Lines[0].DiscountRatio = "120", "8", "0"
	same.Evidence = append([]Evidence{{SourceID: "source.1", Locator: "第二处"}}, proof...)
	twice := draft()
	twice.Evidence = append(slicesClone(proof), Evidence{SourceID: "source.1", Locator: "第二处"})
	a, err := sealed(same, "0.1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := sealed(twice, "0.1")
	if err != nil {
		t.Fatal(err)
	}
	if a.BusinessHash != b.BusinessHash {
		t.Fatal("evidence order must not change the hash")
	}
	spelled := draft()
	spelled.Lines[0].Quantity, spelled.Lines[0].UnitPrice = "120.0", "8.00000000"
	c, err := sealed(spelled, "0.1")
	if err != nil || c.BusinessHash != base.BusinessHash {
		t.Fatalf("equivalent decimals: %v", err)
	}
	for name, edit := range map[string]func(d *Data){
		"customer notes": func(d *Data) { n := *d.CustomerNotes + "\n"; d.CustomerNotes = &n },
		"unit price":     func(d *Data) { d.Lines[0].UnitPrice = "8.01" },
		"expiry":         func(d *Data) { d.ExpiresAt = "2026-10-28T08:00:00.000001Z" },
	} {
		d := draft()
		edit(&d)
		s, err := sealed(d, "0.1")
		if err != nil || s.BusinessHash == base.BusinessHash {
			t.Errorf("%s must change the hash: %v", name, err)
		}
	}
	other, err := SealDraft("quote.1", 2, draft(), facts(), calculator(draft(), "0.1"), issuedAt)
	if err != nil || other.BusinessHash == base.BusinessHash {
		t.Fatalf("the business revision is part of the hash: %v", err)
	}
	stricter, err := sealed(draft(), "0.2")
	if err != nil || stricter.BusinessHash == base.BusinessHash {
		t.Fatalf("the minimum margin the calculation applied is part of the hash: %v", err)
	}
}

func slicesClone(es []Evidence) []Evidence { return append([]Evidence(nil), es...) }

// The freeze's checks in their order, each rule once, each refused with its catalog code at its
// path (Python t03_storage freeze, _seal_quote_timing, _server_calculation, t03_trade_parties.bind):
// the domain's own checks, then the validity window, then the server calculation's (package calc,
// whose own tests cover every rule; here each family appears once, through the freeze). The cases
// port, as the freeze judges them:
//   - test_t03_a27_cost_scope.py test_public_missing_rfq_fails_closed (PYT-0293): no RFQ;
//   - test_t03_a28_sales_terms.py test_missing_issuer_cannot_skip_sales_tax_party_binding (PYT-0316)
//     and test_foreign_issuer_cannot_use_local_sales_applicability (PYT-0317): no issuer, and an
//     issuer other than the company of the scope (the sales tax applications are the company's,
//     S1-D15, so the issuer's own applicability is not asked);
//   - test_t03_a29_trade_parties.py test_reference_and_address_validation (PYT-0348): no trading
//     profile, no address and an address the customer does not have (an empty ID or a boolean
//     revision fails the contract's decoding, SCHEMA_INVALID);
//   - test_t03_a29_trade_parties.py test_profile_must_match_customer_contact_terms_and_supported_address_role
//     (PYT-0349): a profile of another party (REFERENCE_INVALID; Python REFERENCE_STALE) or of another
//     revision of the customer, a superseded profile, a contact the profile does not pin, an address
//     of no supported role and terms other than the profile's (COMPARISON_BASIS_MISMATCH; the payment
//     terms in calc's TestBindScopeRefusals); PTY refuses a party without a name;
//   - test_t03_a30_contact_profiles.py test_profile_without_exact_legacy_contact_cannot_authorize_freeze
//     (PYT-0365): the contact must be the one the trading profile pins at its row version (Brant has
//     no contact profile; PTY's trading profile pins the contact, S1-D35);
//   - test_t03_a31_address_roles.py test_all_other_foundation_roles_are_rejected (PYT-0380): an
//     address of another role alone (more in TestCustomerAddressesBindTheirRoles);
//   - test_t03_server_calculation.py test_structured_quote_validity_is_bounded_by_server_policy
//     (PYT-0443): expired at issue, and beyond the pricing policy's validity.
func TestSealDraftRefusesWhatCannotBeFrozen(t *testing.T) {
	stale := func(path string) error { return fail(CodeReferenceStale, path, "") }
	charge := Charge{ChargeID: "C1", FeeCategory: freight, ObligationKey: "freight", Amount: "60", Inclusion: InclusionSeparate,
		TaxApplication: chargeTx, Evidence: proof}
	cases := []struct {
		name string
		d    func(d *Data)
		f    func(f *Facts)
		c    calcEdit
		want string
	}{
		{"no issuer", func(d *Data) { d.Issuer = nil }, nil, nil, "DRAFT_NOT_READY at /args/data/issuer_ref"},
		{"no customer", func(d *Data) { d.Customer = nil }, nil, nil, "CUSTOMER_REFERENCE_MISSING at /args/data/customer_ref"},
		{"no contact", func(d *Data) { d.Contact = nil }, nil, nil, "CONTACT_REFERENCE_MISSING at /args/data/contact_ref"},
		{"no trading profile", func(d *Data) { d.Profile = nil }, nil, nil, "DRAFT_NOT_READY at /args/data/trading_profile_ref"},
		{"no address", func(d *Data) { d.Addresses = nil }, nil, nil, "DRAFT_NOT_READY at /args/data/customer_address_refs"},
		{"no RFQ", func(d *Data) { d.Rfq = nil }, nil, nil, "DRAFT_NOT_READY at /args/data/rfq_ref"},
		{"no snapshot", func(d *Data) { d.Snapshot = nil }, nil, nil, "COST_SNAPSHOT_MISSING at /args/data/cost_snapshot_ref"},
		{"no named place", func(d *Data) { d.NamedPlace = nil }, nil, nil, "DRAFT_NOT_READY at /args/data/named_place_ref"},
		{"no delivery", func(d *Data) { d.Delivery = nil }, nil, nil, "DRAFT_NOT_READY at /args/data/delivery"},
		{"no expiry", func(d *Data) { d.ExpiresAt = "" }, nil, nil, "DRAFT_NOT_READY at /args/data/expires_at"},
		{"no evidence", func(d *Data) { d.Evidence = nil }, nil, nil, "DRAFT_NOT_READY at /args/data/evidence"},
		{"unreadable evidence", nil, func(f *Facts) { delete(f.Sources, "source.1") }, nil, "REFERENCE_INVALID at /args/data/evidence/0/source_id"},
		{"another company", func(d *Data) { r := Ref{Kind: KindCompanyProfile, ID: "company.other", Revision: 1}; d.Issuer = &r }, nil, nil,
			"REFERENCE_INVALID at /args/data/issuer_ref/id"},
		{"customer revised", nil, func(f *Facts) { f.Party.Revision = 3 }, nil, "REFERENCE_STALE at /args/data/customer_ref"},
		{"not a customer", nil, func(f *Facts) { f.Party.Customer = false }, nil, "REFERENCE_INVALID at /args/data/customer_ref"},
		{"contact of another party", nil, func(f *Facts) { f.Contact.PartyID = "customer.2" }, nil, "REFERENCE_INVALID at /args/data/contact_ref"},
		{"contact changed", nil, func(f *Facts) { f.Contact.RowVersion = 3 }, nil, "REFERENCE_STALE at /args/data/contact_ref"},
		{"profile superseded", nil, func(f *Facts) { f.Profile.Check = stale }, nil, "REFERENCE_STALE at /args/data/trading_profile_ref"},
		{"profile of another party", func(d *Data) { d.Profile.PartyID = "customer.2" }, nil, nil, "REFERENCE_INVALID at /args/data/trading_profile_ref"},
		{"profile of another revision of the customer", nil, func(f *Facts) { f.Profile.PartyRevision = 1 }, nil,
			"REFERENCE_STALE at /args/data/trading_profile_ref"},
		{"contact not pinned", nil, func(f *Facts) { f.Profile.Contacts = nil }, nil, "REFERENCE_INVALID at /args/data/contact_ref"},
		{"unknown address", func(d *Data) {
			d.Addresses = []AddressRef{{PartyID: "customer.1", PartyRevision: 2, AddressID: "nowhere"}}
		}, nil, nil, "REFERENCE_INVALID at /args/data/customer_address_refs/0"},
		{"billing only is enough, consignee only is not", func(d *Data) {
			d.Addresses = []AddressRef{{PartyID: "customer.1", PartyRevision: 2, AddressID: "dock"}}
		}, func(f *Facts) {
			f.Addresses[AddressRef{PartyID: "customer.1", PartyRevision: 2, AddressID: "dock"}] = Address{Role: "consignee", Address: "dock"}
		}, nil, "REFERENCE_INVALID at /args/data/customer_address_refs"},
		{"rfq cancelled", nil, func(f *Facts) { f.Rfq.Ready = false }, nil, "REFERENCE_STALE at /args/data/rfq_ref"},
		{"spec revised", nil, func(f *Facts) { f.Specs[SkuRef{SkuID: "sku.1", SpecRevision: 1}] = stale }, nil,
			"REFERENCE_STALE at /args/data/lines/0/sku_ref"},
		{"config retired", nil, func(f *Facts) {
			c := f.Configs[template]
			c.Check = func(p string) error { return kernel.RejectAs("CONFIG_NOT_ACTIVE", kernel.CategoryInvalid, p, "") }
			f.Configs[template] = c
		}, nil, "CONFIG_NOT_ACTIVE at /args/data/document_template_ref"},
		{"nested tax profile unknown", nil, func(f *Facts) { delete(f.Configs, zero) }, nil, "REFERENCE_INVALID at /args/data/lines/0/tax_application_ref"},
		{"snapshot not adoptable", nil, func(f *Facts) {
			f.Snapshot.Check = func(p string) error {
				return kernel.RejectAs("COST_SNAPSHOT_INELIGIBLE", kernel.CategoryInvalid, p, "")
			}
		}, nil, "COST_SNAPSHOT_INELIGIBLE at /args/data/cost_snapshot_ref"},
		{"the costing's unit unknown", nil, func(f *Facts) { f.Adopted = append(f.Adopted, Ref{Kind: KindUnit, ID: "KG", Revision: 1}) }, nil,
			"REFERENCE_INVALID at /args/data/cost_snapshot_ref"},
		{"renderer", nil, func(f *Facts) { c := f.Configs[template]; c.Renderer = "html/1"; f.Configs[template] = c }, nil,
			"DEPENDENCY_NOT_SUPPORTED at /args/data/document_template_ref"},
		{"order sequence", nil, func(f *Facts) { c := f.Configs[sequence]; c.SequenceType = "order"; f.Configs[sequence] = c }, nil,
			"REFERENCE_INVALID at /args/data/sequence_ref"},
		{"snapshot of another line set", nil, func(f *Facts) { f.Snapshot.RfqLinesHash = hashOf("other lines") }, nil,
			"COMPARISON_BASIS_MISMATCH at /args/data/cost_snapshot_ref"},
		{"expired at issue", func(d *Data) { d.ExpiresAt = kernel.FormatTimestamp(issuedAt) }, nil, nil, "QUOTE_EXPIRED at /args/data/expires_at"},
		{"beyond the policy's validity", func(d *Data) { d.ExpiresAt = kernel.FormatTimestamp(issuedAt.Add(7*24*time.Hour + time.Microsecond)) },
			nil, nil, "QUOTE_VALIDITY_INVALID at /args/data/expires_at"},
		{"the validity before the calculation", func(d *Data) { d.ExpiresAt = kernel.FormatTimestamp(issuedAt); d.Lines[0].Quantity = "100" },
			nil, nil, "QUOTE_EXPIRED at /args/data/expires_at"},
		// The server calculation's rules (TRD-INV-006–008), each checked there once.
		{"partial line set", nil, nil, func(_ *calc.Quote, f *calc.Facts) {
			f.Rfq.Lines = append(f.Rfq.Lines, calc.RfqLine{LineID: "L2", SkuID: "sku.2", Quantity: kernel.MustDecimal("1"), UOM: "PCS"})
		}, "COMPARISON_BASIS_MISMATCH at /args/data/cost_snapshot_ref"},
		{"another quantity", func(d *Data) { d.Lines[0].Quantity = "100" }, nil, nil, "COMPARISON_BASIS_MISMATCH at /args/data/lines/0/quantity"},
		{"BC-AT04 stated base quantity", func(d *Data) { d.Lines[0].BaseQuantity = "2" }, nil, nil,
			"BASE_QUANTITY_MISMATCH at /args/data/lines/0/base_quantity"},
		{"the RFQ's customer", nil, nil, func(_ *calc.Quote, f *calc.Facts) { f.Rfq.Customer = "customer.2" },
			"REFERENCE_INVALID at /args/data/customer_ref"},
		{"terms not the profile's", nil, nil, func(_ *calc.Quote, f *calc.Facts) {
			f.Profile.TradeTerms = calc.Ref{Kind: KindTradeTerms, ID: "FOB", Revision: 1}
		},
			"COMPARISON_BASIS_MISMATCH at /args/data/trade_terms_ref"},
		{"another delivery than the costing's", func(d *Data) { v := handover; v.Date = "2027-02-01"; d.Delivery = &v }, nil, nil,
			"COMPARISON_BASIS_MISMATCH at /args/data/delivery"},
		{"purchase tax application", nil, nil, func(q *calc.Quote, _ *calc.Facts) { q.Lines[0].Tax.Side = calc.SidePurchase },
			"TAX_POLICY_UNCONFIRMED at /args/data/lines/0/tax_application_ref"},
		{"redundant fx", nil, nil, func(_ *calc.Quote, f *calc.Facts) {
			f.FX = &calc.FXRate{Ref: calc.Ref{Kind: KindFXRate, ID: "usd.usd", Revision: 1}, From: dollars, To: dollars, Rate: kernel.MustDecimal("1"),
				Purpose: calc.PurposeQuotation, ObservedAt: issuedAt}
		}, "FX_SCOPE_MISMATCH at /args/data/sales_fx_ref"},
		{"duplicate obligation", func(d *Data) {
			c2 := charge
			c2.ChargeID, c2.FeeCategory = "C2", Ref{Kind: KindFeeCategory, ID: "insurance.sale", Revision: 1}
			d.Charges = []Charge{charge, c2}
		}, func(f *Facts) {
			f.Configs[Ref{Kind: KindFeeCategory, ID: "insurance.sale", Revision: 1}] = Config{ContentHash: hashOf("insurance")}
		}, nil,
			"CHARGE_DOUBLE_COUNT at /args/data/charges/1/obligation_key"},
		{"payment stages short of 100", nil, nil, func(_ *calc.Quote, f *calc.Facts) { f.Stages[0].Percent = kernel.MustDecimal("99") },
			"PAYMENT_TOTAL_MISMATCH at /args/data/payment_terms_ref"},
	}
	for _, c := range cases {
		d, f := draft(), facts()
		if c.d != nil {
			c.d(&d)
		}
		if c.f != nil {
			c.f(&f)
		}
		var edits []calcEdit
		if c.c != nil {
			edits = append(edits, c.c)
		}
		_, err := SealDraft("quote.1", 1, d, f, calculator(d, "0.1", edits...), issuedAt)
		if got := code(err); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// The policy's last valid instant is admitted (issued < expires ≤ issued + 7 days), and a
	// separate sales charge is added once (Python PYT-0449, the domain half).
	d := draft()
	d.ExpiresAt = kernel.FormatTimestamp(issuedAt.Add(7 * 24 * time.Hour))
	d.Charges = []Charge{charge}
	f := facts()
	s, err := SealDraft("quote.1", 1, d, f, calculator(d, "0.1"), issuedAt)
	if err != nil {
		t.Fatalf("seven days: %v", err)
	}
	if c := s.Calculation; c.NetMinor != "102000" || len(c.Charges) != 1 || c.Charges[0].ObligationKey != "freight" ||
		c.Charges[0].NetMinor != "6000" || len(c.SalesTerms.Charges) != 1 || c.SalesTerms.Charges[0].Amount != "60" {
		t.Fatalf("with a sales charge %+v", c)
	}
}

// Python test_t03_a31_address_roles.py test_buyer_and_billing_are_bound_as_selected_roles
// (PYT-0379) and test_all_other_foundation_roles_are_rejected (PYT-0380), T03 A31: a quote addresses
// its customer at a buyer or a billing address of the confirmed revision it binds, and the freeze
// seals the address with its role into the party snapshots the business hash covers; an address of
// any of PTY's five other roles (seller, payer, payee, consignee, shipper; S1-D33) alone is
// REFERENCE_INVALID. (Go binds a list of addresses, one per role, of which one must be buyer or
// billing; the formal document prints the buyer's, else the billing one: TestChargesAndRecipientAddress.)
func TestCustomerAddressesBindTheirRoles(t *testing.T) {
	billing := AddressRef{PartyID: "customer.1", PartyRevision: 2, AddressID: "ap"}
	for role, ref := range map[string]AddressRef{"buyer": hq, "billing": billing} {
		d := draft()
		d.Addresses = []AddressRef{ref}
		s, err := sealed(d, "0.1")
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		c, err := ParseCommercial(s.Commercial, s.BusinessHash)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.PartySnapshots.Customer.Addresses; len(got) != 1 || got[0].AddressID != ref.AddressID || got[0].Role != role ||
			got[0].Address != facts().Addresses[ref].Address {
			t.Errorf("%s: sealed %+v", role, got)
		}
	}
	for _, role := range []string{"seller", "payer", "payee", "consignee", "shipper"} {
		other := AddressRef{PartyID: "customer.1", PartyRevision: 2, AddressID: role}
		d, f := draft(), facts()
		d.Addresses = []AddressRef{other}
		f.Addresses[other] = Address{Role: role, Address: "synthetic " + role + " address"}
		_, err := SealDraft("quote.1", 1, d, f, calculator(d, "0.1"), issuedAt)
		if got := code(err); got != "REFERENCE_INVALID at /args/data/customer_address_refs" {
			t.Errorf("%s alone: %s", role, got)
		}
	}
}

// A quote in another unit than the costing's converts its quantity exactly through the SKU
// specification's conversion (TRD-INV-006; Python A27 test_explicit_conversion_preserves_the
// _normalized_scope): 10 DOZ is 120 PCS; 11 DOZ is not the RFQ's quantity, 10.5 DOZ not a DOZ quantity; without a conversion
// the unit is a dependency not delivered.
func TestLineQuantityConvertsToTheCostingUnit(t *testing.T) {
	for q, want := range map[string]string{"10": "OK", "11": "COMPARISON_BASIS_MISMATCH at /args/data/lines/0/quantity", "10.5": "INVALID_KNOWN_VALUE at /args/data/lines/0/quantity"} {
		d := draft()
		d.Lines[0].Quantity, d.Lines[0].Unit, d.Lines[0].Conversion = q, dozen, &toPieces
		s, err := sealed(d, "0.1")
		if got := code(err); got != want {
			t.Errorf("%s DOZ: %s, want %s", q, got, want)
		}
		if err == nil && (s.Calculation.CostScope.Lines[0].NormalizedQuantity != "120" || *s.Calculation.CostScope.Lines[0].Conversion != toPieces) {
			t.Errorf("binding %+v", s.Calculation.CostScope.Lines[0])
		}
	}
	d := draft()
	d.Lines[0].Unit = dozen
	if _, err := sealed(d, "0.1"); code(err) != "DEPENDENCY_NOT_SUPPORTED at /args/data/lines/0/conversion_ref" {
		t.Errorf("a missing conversion: %s", code(err))
	}
}

// A server calculation that does not fit the revision is not sealed.
func TestCalculationMustFitTheRevision(t *testing.T) {
	for name, edit := range map[string]func(c *Calculation){
		"another line":          func(c *Calculation) { c.Lines[0].ID = "L2" },
		"tax not added":         func(c *Calculation) { c.TaxMinor = "1" },
		"payments short":        func(c *Calculation) { c.Payments[0].AmountMinor = "95999" },
		"unknown margin":        func(c *Calculation) { c.MarginRule = "maybe" },
		"pass with a shortfall": func(c *Calculation) { c.MarginShortfall = "1" },
		"float-like amount":     func(c *Calculation) { c.NetMinor = "960.00" },
		"no excluded list":      func(c *Calculation) { c.ExcludedCostCategories = nil },
		"another cost basis":    func(c *Calculation) { c.CostBasis = "full_cost" },
		"another version":       func(c *Calculation) { c.CalculationVersion = "brant.quote-calculation/0.0.1" },
	} {
		c, err := calculator(draft(), "0.1")()
		if err != nil {
			t.Fatal(err)
		}
		edit(c)
		_, err = SealDraft("quote.1", 1, draft(), facts(), func() (*Calculation, error) { return c, nil }, issuedAt)
		if !strings.HasPrefix(code(err), "INVALID_KNOWN_VALUE at /calculation") {
			t.Errorf("%s: %s", name, code(err))
		}
	}
}

func human(id string) kernel.ActorRef { return kernel.ActorRef{Kind: kernel.ActorHuman, ID: id} }

// SM-TRD-REVISION, S3-01 part (Python test_t03_quote_review test_revise_and_discard_preserve_sealed
// _history, test_hash_and_revision_are_immutable_and_cas_is_required): save replaces the working
// revision, freeze seals it, revise copies a frozen revision into a new number, discard keeps the
// revision and never reuses its number; metadata moves the root version only.
func TestRevisionLifecycle(t *testing.T) {
	q := New("quote.1", "author", "trade")
	if err := CheckSave(nil, false, 1); code(err) != "NOT_FOUND at /args/quote_id" {
		t.Fatalf("a missing root at version 1: %s", code(err))
	}
	now := issuedAt
	w := q.Save(draft(), human("author"), 1, now)
	q.Save(draft(), kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent"}, 1, now)
	if q.RootVersion != 2 || w.Number != 1 || len(q.Revisions) != 1 || len(w.Contributors) != 2 {
		t.Fatalf("after two saves %+v", q)
	}
	if err := CheckSave(q, true, 1); code(err) != "VERSION_CONFLICT at /args/expected_version" {
		t.Fatalf("stale version: %s", code(err))
	}
	if _, err := q.Revise(1, human("author"), 1, now); code(err) != "INVALID_STATE at /args/quote_id" {
		t.Fatalf("revise with a working revision: %s", code(err))
	}
	s, err := SealDraft(q.ID, w.Number, w.Data, facts(), calculator(w.Data, "0.1"), now)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.CheckFreeze(2); code(err) != "INVALID_STATE at /args/working_revision" {
		t.Fatalf("another working revision: %s", code(err))
	}
	if _, err = q.Freeze(1, s, human("checker"), 1, now); err != nil {
		t.Fatal(err)
	}
	if w.Status != Frozen || q.RootVersion != 3 || w.Seal.RootVersion != 3 || q.FrozenAt(3) != w || q.Working() != nil {
		t.Fatalf("frozen %+v", w)
	}
	if err = CheckSave(q, true, 3); code(err) != "INVALID_STATE at /args/quote_id" {
		t.Fatalf("save after freeze: %s", code(err))
	}
	hash := w.Seal.BusinessHash
	q.UpdateMetadata([]string{"VIP", "急"}, "内部备注\n第二行")
	if q.RootVersion != 4 || w.Seal.BusinessHash != hash || !q.MetadataChanged {
		t.Fatal("metadata moves the root version only")
	}
	if _, err = q.Revise(2, human("author"), 1, now); code(err) != "REFERENCE_INVALID at /args/base_revision" {
		t.Fatalf("revise an unknown revision: %s", code(err))
	}
	r2, err := q.Revise(1, human("reviser"), 1, now)
	if err != nil || r2.Number != 2 || r2.Base != 1 || r2.Hash != w.Data.Hash() || len(r2.Contributors) != 3 {
		t.Fatalf("revise %+v, %v", r2, err)
	}
	if _, err = q.Discard(1, human("author"), 1, now, "reason", proof); code(err) != "INVALID_STATE at /args/working_revision" {
		t.Fatalf("discard a frozen revision: %s", code(err))
	}
	if _, err = q.Discard(2, human("author"), 1, now, "客户改需求", proof); err != nil || r2.Status != Discarded {
		t.Fatalf("discard: %v", err)
	}
	r3, err := q.Revise(1, human("author"), 1, now)
	if err != nil || r3.Number != 3 {
		t.Fatalf("a discarded number is not reused: %+v, %v", r3, err)
	}
}

// TRD-INV-015 (Python test_t03_a24_write_control and t03_storage.guard): who may write while the
// root is unbound, bound or stopping, and with which claim.
func TestControlFence(t *testing.T) {
	q := New("quote.1", "author", "")
	now := issuedAt
	task := &Claim{TaskID: "task.1", Epoch: 1}
	if err := q.Control.Admit(nil, 7); err != nil {
		t.Fatalf("an unbound root takes manual writes: %v", err)
	}
	if got := code(q.Control.Admit(task, 7)); got != "CONTROL_EPOCH_STALE at /" {
		t.Fatalf("a claim on an unbound root: %s", got)
	}
	if got := code(q.Bind("task.1", 1, human("author"), 7, now)); got != "VERSION_CONFLICT at /args/expected_control_epoch" {
		t.Fatalf("bind at another epoch: %s", got)
	}
	if err := q.Bind("task.1", 0, human("author"), 7, now); err != nil || q.Control.Epoch != 1 || q.Control.State != Bound {
		t.Fatalf("bind: %v %+v", err, q.Control)
	}
	if got := code(q.Bind("task.2", 1, human("author"), 7, now)); got != "OBJECT_CONTROL_CONFLICT at /args/quote_id" {
		t.Fatalf("a second binding: %s", got)
	}
	for name, c := range map[string]struct {
		claim     *Claim
		authority int64
		want      string
	}{
		"no claim":          {nil, 7, "OBJECT_CONTROL_CONFLICT at /"},
		"another task":      {&Claim{TaskID: "task.2", Epoch: 1}, 7, "OBJECT_CONTROL_CONFLICT at /"},
		"an earlier epoch":  {&Claim{TaskID: "task.1", Epoch: 0}, 7, "CONTROL_EPOCH_STALE at /"},
		"access changed":    {task, 8, "OBJECT_CONTROL_CONFLICT at /"},
		"the bound task":    {task, 7, "OK"},
		"a future epoch":    {&Claim{TaskID: "task.1", Epoch: 2}, 7, "CONTROL_EPOCH_STALE at /"},
		"guessed and wrong": {&Claim{TaskID: "other", Epoch: 2}, 7, "CONTROL_EPOCH_STALE at /"},
	} {
		if got := code(q.Control.Admit(c.claim, c.authority)); got != c.want {
			t.Errorf("bound, %s: %s, want %s", name, got, c.want)
		}
	}
	if got := code(q.Release(0, human("author"), now)); got != "VERSION_CONFLICT at /args/expected_control_epoch" {
		t.Fatalf("release at an old epoch: %s", got)
	}
	if err := q.Stop(1, human("lead"), now); err != nil || q.Control.State != Stopping || q.Control.Epoch != 2 || q.Control.TaskID != "task.1" {
		t.Fatalf("stop: %v %+v", err, q.Control)
	}
	if got := code(q.Control.Admit(task, 7)); got != "CONTROL_EPOCH_STALE at /" {
		t.Fatalf("the stopped task: %s", got)
	}
	if got := code(q.Control.Admit(nil, 7)); got != "OBJECT_CONTROL_CONFLICT at /" {
		t.Fatalf("a manual write while stopping: %s", got)
	}
	if got := code(q.Control.Admit(&Claim{TaskID: "task.1", Epoch: 2}, 7)); got != "OBJECT_CONTROL_CONFLICT at /" {
		t.Fatalf("a claim of the stop's epoch: %s", got)
	}
	if got := code(q.Stop(2, human("lead"), now)); got != "INVALID_STATE at /args/quote_id" {
		t.Fatalf("stop twice: %s", got)
	}
	if err := q.Release(2, human("lead"), now); err != nil || q.Control.State != Unbound || q.Control.Epoch != 3 || q.Control.TaskID != "" {
		t.Fatalf("release: %v %+v", err, q.Control)
	}
	if err := q.Control.Admit(nil, 9); err != nil {
		t.Fatalf("released: %v", err)
	}
	if got := code(q.Release(3, human("lead"), now)); got != "INVALID_STATE at /args/quote_id" {
		t.Fatalf("release an unbound root: %s", got)
	}
	if err := q.Bind("task.2", 3, kernel.ActorRef{Kind: kernel.ActorSystem, ID: "tsk"}, 9, now); err != nil || q.Control.Epoch != 4 {
		t.Fatalf("rebind: %v", err)
	}
	if got := code(q.Control.Admit(task, 9)); got != "CONTROL_EPOCH_STALE at /" {
		t.Fatalf("the first task after a rebinding: %s", got)
	}
	if err := q.Control.Admit(&Claim{TaskID: "task.2", Epoch: 4}, 9); err != nil {
		t.Fatalf("the new task: %v", err)
	}
	if q.RootVersion != 0 {
		t.Fatal("control never moves the root version")
	}
}

func TestDraftChecks(t *testing.T) {
	readable := map[string]bool{"source.1": true}
	for name, c := range map[string]struct {
		edit func(d *Data)
		want string
	}{
		"duplicate line":     {func(d *Data) { d.Lines = append(d.Lines, d.Lines[0]) }, "DUPLICATE_LINE_ID at /args/data/lines/1/line_id"},
		"duplicate rfq line": {func(d *Data) { l := d.Lines[0]; l.LineID = "L2"; d.Lines = append(d.Lines, l) }, "DUPLICATE_LINE_ID at /args/data/lines/1/rfq_line_id"},
		"wrong kind":         {func(d *Data) { d.Currency = &pcs }, "REFERENCE_INVALID at /args/data/currency_ref/kind"},
		"delivery calendar of another kind": {func(d *Data) {
			v := Delivery{Kind: "lead_time", Anchor: &BusinessRef{Kind: "delivery_anchor", ID: "o", Revision: 1}, Days: 3, DayBasis: "working",
				Calendar: &usd, Milestone: "handover", Place: deham, TimeZone: "UTC"}
			d.Delivery = &v
		}, "REFERENCE_INVALID at /args/data/delivery/calendar_ref/kind"},
		"included in nothing": {func(d *Data) {
			d.Charges = []Charge{{ChargeID: "C1", FeeCategory: freight, ObligationKey: "o", Amount: "1", Inclusion: InclusionIncludedInLine, IncludedLineID: "L9", TaxApplication: chargeTx, Evidence: proof}}
		}, "REFERENCE_INVALID at /args/data/charges/0/included_line_id"},
		"unreadable":        {func(d *Data) { d.Lines[0].Evidence = []Evidence{{SourceID: "secret", Locator: "x"}} }, "REFERENCE_INVALID at /args/data/lines/0/evidence/0/source_id"},
		"duplicate address": {func(d *Data) { d.Addresses = append(d.Addresses, hq) }, "INVALID_KNOWN_VALUE at /args/data/customer_address_refs/1/address_id"},
		"empty draft":       {func(d *Data) { *d = Data{} }, "OK"},
	} {
		d := draft()
		c.edit(&d)
		if got := code(CheckDraft(d, readable)); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	if got := fmt.Sprint(draft().Sources()); got != "[source.1]" {
		t.Fatalf("sources %s", got)
	}
}
