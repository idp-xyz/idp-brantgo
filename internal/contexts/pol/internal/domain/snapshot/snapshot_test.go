package snapshot

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/config"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func TestCaptureIsCanonicalAndAdoptsEachRevisionOnce(t *testing.T) {
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	by := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "editor"}
	hash := "0000000000000000000000000000000000000000000000000000000000000000"
	usd := Entry{Kind: "currency", ConfigID: "USD", Revision: 1, ContentHash: hash}
	fx := Entry{Kind: "fx_rate", ConfigID: "usd.cny", Revision: 2, ContentHash: hash}
	cny := Entry{Kind: "currency", ConfigID: "CNY", Revision: 1, ContentHash: hash}
	a, err := Capture("s", "cost_estimate", at, by, 1, []Entry{fx, usd, cny, usd})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Capture("s", "cost_estimate", at, by, 1, []Entry{cny, usd, fx})
	quote, _ := Capture("s", "quotation", at, by, 1, []Entry{cny, usd, fx})
	if len(a.Entries) != 3 || a.Entries[0] != cny || a.Entries[2] != fx || a.Hash != b.Hash || a.Hash == quote.Hash || len(a.Hash) != 64 {
		t.Fatalf("%+v %s %s", a.Entries, a.Hash, quote.Hash)
	}
	if r, ok := kernel.AsRejection(CheckNew(true)); !ok || r.Code != CodeBusinessKeyConflict || r.Category != kernel.CategoryConflict {
		t.Fatal("a taken snapshot ID is a business key conflict")
	}
}

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}

func adopted(path string, c config.Content) Adopted {
	return Adopted{Entry: Entry{Kind: string(c.Kind)}, Rev: &config.Revision{Number: 1, Content: c}, Path: path}
}

// Python test_foundation.py test_snapshot_requires_company_policy_and_unknown_tax_blocks and the
// snapshot half of test_stale_fx_is_not_silently_reused_and_no_reciprocal.
func TestFoundationNeedsCompanyAndPolicyAndRefusesUnknownTaxAndStaleFX(t *testing.T) {
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	company := adopted("/args/refs/0", config.Content{Kind: config.KindCompanyProfile, CompanyProfile: &config.CompanyProfile{}})
	policy := func(maxAge int64) Adopted {
		return adopted("/args/refs/1", config.Content{Kind: config.KindPricingPolicy, PricingPolicy: &config.PricingPolicy{MaxFXAgeSeconds: maxAge}})
	}
	tax := func(recovery string) Adopted {
		return adopted("/args/refs/1", config.Content{Kind: config.KindTaxProfile, TaxProfile: &config.TaxProfile{RecoveryTreatment: recovery}})
	}
	fx := adopted("/args/refs/2", config.Content{Kind: config.KindFXRate, FXRate: &config.FXRate{ObservedAt: "2026-10-22T00:00:00Z"}})
	for _, c := range []struct {
		adopted []Adopted
		want    string
	}{
		{[]Adopted{fx}, "DRAFT_NOT_READY /args/refs"},
		{[]Adopted{company, fx}, "DRAFT_NOT_READY /args/refs"},
		{[]Adopted{company, policy(86400), policy(60), fx}, "DRAFT_NOT_READY /args/refs"},
		{[]Adopted{company, policy(86400), tax("nonrecoverable"), fx}, "OK"},
		{[]Adopted{company, policy(86400), tax("unknown"), fx}, "TAX_POLICY_UNCONFIRMED /args/refs/1"},
		{[]Adopted{company, policy(60), fx}, "FX_STALE /args/refs/2"},
		{[]Adopted{company, policy(9 * 3600), fx}, "OK"},
	} {
		if got := code(CheckFoundation(c.adopted, at)); got != c.want {
			t.Errorf("%d adopted: %s, want %s", len(c.adopted), got, c.want)
		}
	}
	if got := code(CheckAdoptable("bank_account", "/args/refs/3")); got != "PERMISSION_DENIED /args/refs/3" {
		t.Fatal(got)
	}
	if CheckAdoptable("fx_rate", "/args/refs/0") != nil {
		t.Fatal("an exchange rate is adoptable")
	}
}
