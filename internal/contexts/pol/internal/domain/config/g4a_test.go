package config

import (
	"encoding/json"
	"testing"
	"time"
)

// The sales price list and the target margin (S4 unit G4a; owner decision 2026-11-18: a quotation's
// unit price comes from a price list where one is in force, otherwise from cost ÷ (1 − target
// margin)). The design sources say nothing of either yet and Python V0.50 has neither, so the tests
// cite the decision.

var g4aFrom = time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)

// g4aGolden are pricing policies as the code of 3185097 (the base of G4a, before the target margin
// existed) stored them, with their stored data and content hash, computed on that tree and pinned
// here (scratchpad/s4g4a/golden_base.txt): BRC-001's policy and one with a bounded validity.
var g4aGolden = []struct {
	content    func() Content
	hash, data string
}{
	{func() Content {
		return Content{Kind: KindPricingPolicy, ValidFrom: g4aFrom, PricingPolicy: &PricingPolicy{Name: "synthetic BRC-001 policy",
			CalculationCurrency: "USD", MinimumMargin: "0.25", QuoteValidDays: 7, MaxFXAgeSeconds: 3600,
			TaxProfile: Ref{Kind: "tax_profile", ConfigID: "tax.zero", Revision: 1}}}
	}, "fcd89eff609505943a06c60b71d8be036664ee0545fe177c7d2a43cc99f2d070",
		`{"name":"synthetic BRC-001 policy","calculation_currency":"USD","minimum_margin":"0.25","quote_valid_days":7,` +
			`"max_fx_age_seconds":3600,"tax_profile":{"kind":"tax_profile","id":"tax.zero","revision":1}}`},
	{func() Content {
		return Content{Kind: KindPricingPolicy, ValidFrom: g4aFrom, ValidUntil: g4aFrom.Add(24 * time.Hour), PricingPolicy: &PricingPolicy{
			Name: "policy", CalculationCurrency: "CNY", MinimumMargin: "0", QuoteValidDays: 30, MaxFXAgeSeconds: 86400,
			TaxProfile: Ref{Kind: "tax_profile", ConfigID: "tax", Revision: 3}}}
	}, "146edc57ed4df8ff77b0033d989c5ed9d87b91d9bf51cabc96be7cd476f3e39f",
		`{"name":"policy","calculation_currency":"CNY","minimum_margin":"0","quote_valid_days":30,"max_fx_age_seconds":86400,` +
			`"tax_profile":{"kind":"tax_profile","id":"tax","revision":3}}`},
}

// The target margin is left out of the stored data while a policy states none, so a pricing policy
// stored before it existed keeps its stored data and its content hash, decodes without one and
// hashes as stored after a jsonb round trip; a stated target margin is content.
func TestG4APricingPoliciesStoredBeforeTheTargetMarginKeepTheirHash(t *testing.T) {
	for _, g := range g4aGolden {
		c := g.content()
		raw, err := c.DataJSON()
		if err != nil {
			t.Fatal(err)
		}
		if hash, _ := c.Hash(); string(raw) != g.data || hash != g.hash {
			t.Errorf("%s now stores %s with hash %s", c.PricingPolicy.Name, raw, hash)
		}
		var generic map[string]any
		if err = json.Unmarshal([]byte(g.data), &generic); err != nil {
			t.Fatal(err)
		}
		reordered, _ := json.Marshal(generic)
		back := Content{Kind: c.Kind, ValidFrom: c.ValidFrom, ValidUntil: c.ValidUntil}
		if err = back.DecodeData(reordered); err != nil {
			t.Fatal(err)
		}
		if hash, _ := back.Hash(); hash != g.hash || back.PricingPolicy.TargetMargin != "" {
			t.Errorf("%s read back from jsonb hashes as %s with target margin %q", c.PricingPolicy.Name, hash, back.PricingPolicy.TargetMargin)
		}
	}
	// The S3 vector of compat_test.go (main 481fa24) is a pricing policy too and still holds
	// (TestRevisionsStoredBeforeS5KeepTheirDataAndHash).
	with := g4aGolden[0].content()
	with.PricingPolicy.TargetMargin = "0.3125"
	raw, err := with.DataJSON()
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"name":"synthetic BRC-001 policy","calculation_currency":"USD","minimum_margin":"0.25","quote_valid_days":7,` +
		`"max_fx_age_seconds":3600,"tax_profile":{"kind":"tax_profile","id":"tax.zero","revision":1},"target_margin":"0.3125"}`
	if hash, _ := with.Hash(); string(raw) != want || hash == g4aGolden[0].hash ||
		hash != "028cbf02f6e93bb9847aeb14a37729c0a0b059c928f735ec9d0cb8997bbc1d45" {
		t.Fatalf("a policy with a target margin stores %s with hash %s", raw, hash)
	}
}

// A target margin is at least the minimum margin and below 1: at 1 the formula divides by zero, and
// a target under the floor would propose prices that need the margin exception.
func TestG4ATargetMarginRules(t *testing.T) {
	for _, c := range []struct{ minimum, target, want string }{
		{"0.25", "", "OK"},
		{"0.25", "0.3125", "OK"},
		{"0.25", "0.25", "OK"},
		{"0", "0", "OK"},
		{"0.25", "0.999999999999", "OK"},
		{"0.25", "0.249999999999", "INVALID_KNOWN_VALUE /args/data/target_margin"},
		{"0.25", "0", "INVALID_KNOWN_VALUE /args/data/target_margin"},
		{"0.25", "1", "INVALID_KNOWN_VALUE /args/data/target_margin"},
		{"1", "1", "INVALID_KNOWN_VALUE /args/data/target_margin"},
		{"0.25", "1.5", "INVALID_KNOWN_VALUE /args/data/target_margin"},
		{"0.25", "x", "INVALID_KNOWN_VALUE /args/data/target_margin"},
		{"1.01", "0.5", "INVALID_KNOWN_VALUE /args/data/minimum_margin"},
	} {
		p := policy(c.minimum)
		p.PricingPolicy.TargetMargin = c.target
		if got := code(Validate("policy", p)); got != c.want {
			t.Errorf("minimum %s target %q: %s, want %s", c.minimum, c.target, got, c.want)
		}
	}
	// The target margin adds no reference: currency by code and tax profile by revision, as before.
	p := policy("0.25")
	p.PricingPolicy.TargetMargin = "0.3125"
	if refs := p.References(); len(refs) != 2 {
		t.Fatalf("%+v", refs)
	}
}

func salesPrice(sku, party, currency, unit, price, basis string) Content {
	return Content{Kind: KindSalesPrice, ValidFrom: g4aFrom, SalesPrice: &SalesPrice{SkuID: sku, PartyID: party, Currency: currency, Unit: unit,
		UnitPrice: price, PriceBasisQuantity: basis}}
}

// A sales price is one price: positive, for a positive quantity of a unit in force within that
// unit's precision, in a currency in force. The stored data names the customer only when the price
// is one customer's; the hashes pin the stored form of the new kind.
func TestG4ASalesPriceContent(t *testing.T) {
	inForce := facts(currencyFact("USD", 2), unitFact("PCS", "count", 0), unitFact("KG", "mass", 3))
	for _, c := range []struct {
		content Content
		want    string
	}{
		{salesPrice("ST01.red.S", "", "USD", "PCS", "8.00", "1"), "OK"},
		{salesPrice("ST01.red.S", "customer.1", "USD", "PCS", "7.5", "1"), "OK"},
		{salesPrice("ST01.red.S", "", "USD", "PCS", "4.93", "1000"), "OK"},
		{salesPrice("yarn", "", "USD", "KG", "12.34567891", "0.5"), "OK"},
		{salesPrice("ST01.red.S", "", "USD", "PCS", "0", "1"), "INVALID_KNOWN_VALUE /args/data/unit_price"},
		{salesPrice("ST01.red.S", "", "USD", "PCS", "0.00", "1"), "INVALID_KNOWN_VALUE /args/data/unit_price"},
		{salesPrice("ST01.red.S", "", "USD", "PCS", "8.00", "0"), "INVALID_KNOWN_VALUE /args/data/price_basis_quantity"},
		{salesPrice("ST01.red.S", "", "USD", "PCS", "8.00", "0.5"), "INVALID_KNOWN_VALUE /args/data/price_basis_quantity"},
		{salesPrice("yarn", "", "USD", "KG", "8.00", "0.0005"), "INVALID_KNOWN_VALUE /args/data/price_basis_quantity"},
		{salesPrice("ST01.red.S", "", "EUR", "PCS", "8.00", "1"), "REFERENCE_INVALID /args/data/currency"},
		{salesPrice("ST01.red.S", "", "USD", "CTN", "8.00", "1"), "REFERENCE_INVALID /args/data/unit"},
	} {
		err := Validate("price.1", c.content)
		if err == nil {
			err = CheckReferences("price.1", c.content, inForce)
		}
		if got := code(err); got != c.want {
			t.Errorf("%+v: %s, want %s", *c.content.SalesPrice, got, c.want)
		}
	}
	retired := facts(currencyFact("USD", 2), unitFact("PCS", "count", 0))
	retired.Configs[Target{Kind: KindUnit, ID: "PCS"}] = Fact{Rev: &Revision{Number: 1}, Refused: Reject(CodeConfigNotActive, "/")}
	if got := code(CheckReferences("price.1", salesPrice("sku", "", "USD", "PCS", "8.00", "1"), retired)); got != "CONFIG_NOT_ACTIVE /args/data/unit" {
		t.Fatal(got)
	}
	refs := salesPrice("sku", "customer.1", "USD", "PCS", "8.00", "1").References()
	if len(refs) != 2 || refs[0].Target() != (Target{Kind: KindCurrency, ID: "USD"}) || refs[1].Target() != (Target{Kind: KindUnit, ID: "PCS"}) {
		t.Fatalf("%+v", refs)
	}

	for _, c := range []struct {
		content    Content
		data, hash string
	}{
		{salesPrice("ST01.red.S", "", "USD", "PCS", "8.00", "1"),
			`{"sku_id":"ST01.red.S","currency":"USD","unit":"PCS","unit_price":"8.00","price_basis_quantity":"1"}`,
			"91c86b7f16e0bb91318cbc1313b239e80d95982fdf81ea3ea569949a9851c5a7"},
		{salesPrice("ST01.red.S", "customer.1", "USD", "PCS", "7.50", "1"),
			`{"sku_id":"ST01.red.S","party_id":"customer.1","currency":"USD","unit":"PCS","unit_price":"7.50","price_basis_quantity":"1"}`,
			"8300208a5e663a6e871713749710fa56b35c258e16c981eb31941e42480025db"},
	} {
		raw, err := c.content.DataJSON()
		if err != nil {
			t.Fatal(err)
		}
		if hash, _ := c.content.Hash(); string(raw) != c.data || hash != c.hash {
			t.Errorf("stores %s with hash %s", raw, hash)
		}
		back := Content{Kind: KindSalesPrice, ValidFrom: g4aFrom}
		if err = back.DecodeData(raw); err != nil || *back.SalesPrice != *c.content.SalesPrice {
			t.Errorf("%s reads back as %+v (%v)", raw, back.SalesPrice, err)
		}
	}
}

// The subject of a sales price — SKU, customer and currency — is fixed by its first publication, so
// a configuration of the price list is one price with its history: the next revision changes the
// price, the unit, the basis quantity and the validity, never what is priced; and the subject stays
// after a retirement.
func TestG4ASalesPriceKeepsItsSubject(t *testing.T) {
	inForce := facts(currencyFact("USD", 2), currencyFact("EUR", 2), unitFact("PCS", "count", 0), unitFact("CTN", "count", 0))
	first := salesPrice("ST01.red.S", "", "USD", "PCS", "8.00", "1")
	next := func(c Content) error {
		r := New(KindSalesPrice, "price.red.S")
		mustSave(t, r, first, editor, 1, g4aFrom)
		if _, err := r.Publish(1, inForce, reviewer, 1, []string{"evidence"}, g4aFrom.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		mustSave(t, r, c, editor, 1, g4aFrom.Add(2*time.Hour))
		superseded, err := r.Publish(2, inForce, reviewer, 1, []string{"evidence"}, g4aFrom.Add(3*time.Hour))
		if err == nil && (superseded == nil || superseded.Status != Superseded || r.Active.Number != 2) {
			t.Fatalf("%+v %+v", superseded, r.Active)
		}
		return err
	}
	for _, c := range []struct {
		content Content
		want    string
	}{
		{salesPrice("ST01.red.S", "", "USD", "PCS", "8.50", "1"), "OK"},
		{salesPrice("ST01.red.S", "", "USD", "CTN", "96.00", "1"), "OK"},
		{salesPrice("ST01.red.M", "", "USD", "PCS", "8.00", "1"), "IMMUTABLE_DEFINITION /args/revision"},
		{salesPrice("ST01.red.S", "customer.1", "USD", "PCS", "8.00", "1"), "IMMUTABLE_DEFINITION /args/revision"},
		{salesPrice("ST01.red.S", "", "EUR", "PCS", "8.00", "1"), "IMMUTABLE_DEFINITION /args/revision"},
	} {
		if got := code(next(c.content)); got != c.want {
			t.Errorf("%+v: %s, want %s", *c.content.SalesPrice, got, c.want)
		}
	}
	// A retired price keeps its subject: what is published next is still that SKU's price.
	r := New(KindSalesPrice, "price.red.S")
	mustSave(t, r, first, editor, 1, g4aFrom)
	if _, err := r.Publish(1, inForce, reviewer, 1, []string{"evidence"}, g4aFrom.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Retire(g4aFrom.Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	mustSave(t, r, salesPrice("ST01.blue.S", "", "USD", "PCS", "8.00", "1"), editor, 1, g4aFrom.Add(3*time.Hour))
	if _, err := r.Publish(2, inForce, reviewer, 1, []string{"evidence"}, g4aFrom.Add(4*time.Hour)); code(err) != "IMMUTABLE_DEFINITION /args/revision" {
		t.Fatal(code(err))
	}
	// ConfigAt v1 serves the kind as every other: the revision valid at the instant.
	both := []*Revision{
		{Number: 1, Status: Superseded, Content: first, Publication: &Publication{At: g4aFrom.Add(time.Hour)}, EndedAt: g4aFrom.Add(3 * time.Hour)},
		{Number: 2, Status: Active, Content: salesPrice("ST01.red.S", "", "USD", "PCS", "8.50", "1"), Publication: &Publication{At: g4aFrom.Add(3 * time.Hour)}},
	}
	both[1].Content.ValidFrom = g4aFrom.Add(24 * time.Hour)
	if at := At(both, g4aFrom.Add(12*time.Hour)); at == nil || at.Number != 1 {
		t.Fatalf("%+v", at)
	}
	if at := At(both, g4aFrom.Add(36*time.Hour)); at == nil || at.Number != 2 {
		t.Fatalf("%+v", at)
	}
}
