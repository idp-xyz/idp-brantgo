package api

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return "OK"
}

func usd() SaveDraft {
	return SaveDraft{IdempotencyKey: "k1", Kind: KindCurrency, ConfigID: "USD", ValidFrom: "2026-10-22T00:00:00Z",
		Data: ConfigData{Currency: &Currency{Code: "USD", Name: "US dollar", MinorUnits: 2, Rounding: RoundingHalfUp}}}
}

func TestSaveDraftContract(t *testing.T) {
	fx := SaveDraft{IdempotencyKey: "k1", Kind: KindFXRate, ConfigID: "usd.cny", ValidFrom: "2026-10-22T00:00:00Z",
		ValidUntil: "2026-10-23T00:00:00Z", Data: ConfigData{FXRate: &FXRate{FromCurrency: "USD", ToCurrency: "CNY", Rate: "7.123456",
			Purpose: PurposeCostEstimate, SourceName: "SYNTHETIC RATE", ObservedAt: "2026-10-22T00:00:00Z"}}}
	for _, c := range []struct {
		edit func(*SaveDraft)
		want string
	}{
		{func(*SaveDraft) {}, "OK"},
		{func(c *SaveDraft) { c.Kind = KindScreeningRegulation }, "UNSUPPORTED_CONTRACT /args/kind"},
		{func(c *SaveDraft) { c.Kind = "fx" }, "SCHEMA_INVALID /args/kind"},
		{func(c *SaveDraft) { c.ConfigID = strings.Repeat("x", MaxConfigIDLength+1) }, "SCHEMA_INVALID /args/id"},
		{func(c *SaveDraft) { c.ValidFrom = "2026-10-22T00:00:00+08:00" }, "SCHEMA_INVALID /args/valid_from"},
		{func(c *SaveDraft) { c.Data.Unit = &Unit{} }, "SCHEMA_INVALID /args/data"},
		{func(c *SaveDraft) { c.Data.Currency.Rounding = "HALF_EVEN" }, "SCHEMA_INVALID /args/data/rounding"},
		{func(c *SaveDraft) { c.Data.Currency.Name = " \t" }, "SCHEMA_INVALID /args/data/name"},
		{func(c *SaveDraft) { c.ExpectedVersion = -1 }, "SCHEMA_INVALID /args/expected_version"},
	} {
		cmd := usd()
		c.edit(&cmd)
		if got := code(cmd.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", cmd, got, c.want)
		}
	}
	if got := code(fx.Validate()); got != "OK" {
		t.Fatal(got)
	}
	fx.ValidUntil = ""
	if got := code(fx.Validate()); got != "SCHEMA_INVALID /args/valid_until" {
		t.Fatal("an exchange rate always expires:", got)
	}
	fx.ValidUntil, fx.Data.FXRate.Rate = "2026-10-23T00:00:00Z", "7.1e0"
	if got := code(fx.Validate()); got != "SCHEMA_INVALID /args/data/rate" {
		t.Fatal("a rate is a decimal string:", got)
	}
}

func TestReferencesAndQueries(t *testing.T) {
	seq := ConfigRef{Kind: KindNumberSequence, ConfigID: "quote.seq", Revision: 1}
	if got := code(IssueNumber{IdempotencyKey: "k", Sequence: seq, BusinessRef: "quote1"}.Validate()); got != "OK" {
		t.Fatal(got)
	}
	if got := code(IssueNumber{IdempotencyKey: "k", Sequence: ConfigRef{Kind: KindCurrency, ConfigID: "USD", Revision: 1}, BusinessRef: "q"}.Validate()); got != "SCHEMA_INVALID /args/sequence/kind" {
		t.Fatal(got)
	}
	if got := code(ValidateRefs([]ConfigRef{seq, seq})); got != "SCHEMA_INVALID /refs" {
		t.Fatal("references are unique:", got)
	}
	if got := code(Publish{IdempotencyKey: "k", Kind: KindCurrency, ConfigID: "USD", ExpectedVersion: 1, Revision: 1, Reason: "核实"}.Validate()); got != "SCHEMA_INVALID /args/evidence_ids" {
		t.Fatal("publication cites verification evidence:", got)
	}
	if ObjectID(KindQuoteDocumentTemplate, strings.Repeat("x", MaxConfigIDLength)) == "" ||
		!kernel.ValidIdentifier(ObjectID(KindQuoteDocumentTemplate, strings.Repeat("x", MaxConfigIDLength))) {
		t.Fatal("the longest object ID must stay an identifier")
	}
	for code := range codes {
		if r := Reject(code, "/x"); r.Code != code || r.Fields[0].Path != "/x" {
			t.Fatal(r)
		}
	}
}
