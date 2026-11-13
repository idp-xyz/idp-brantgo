package api

import "testing"

// The contracts S5R unit R7 adds (simulated sign-off E0, 2026-11-13): credit.reserve exposure_fx_rates
// (S5R-D22: one fx_rate per currency, at most MaxExposureRates, never the order's own fx_rate again),
// credit.release cause and credit.consume carried_from (S5R-D08), fund.verify_payer (S5R-D21).
func TestS5RContracts(t *testing.T) {
	rate := func(id string) ConfigRef { return ConfigRef{Kind: KindFXRate, ConfigID: id, Revision: 1} }
	reserve := ReserveCredit{IdempotencyKey: "k1", ReservationID: "res.1", OrderID: "order.1", OrderRevision: 1, OrderHash: orderHash,
		CreditPolicy: policy, FXRate: &ConfigRef{Kind: KindFXRate, ConfigID: "fx.cny.usd", Revision: 1}}
	release := ReleaseCredit{IdempotencyKey: "k1", ReservationID: "res.1", ExpectedVersion: 2, Reason: "superseded"}
	consume := ConsumeCredit{IdempotencyKey: "k1", ReservationID: "carried.1", OrderID: "order.1", OrderRevision: 2, OrderHash: orderHash,
		ActivatedAt: "2026-11-13T00:00:00Z", CarriedFrom: "res.1"}
	verify := VerifyPayer{IdempotencyKey: "k1", FundRecordID: "fund.1", ExpectedVersion: 1, CustomerID: "customer.1",
		EvidenceIDs: []string{"mandate.1"}, Reason: "客户付款委托书（合成）"}
	many := make([]ConfigRef, MaxExposureRates+1)
	for i := range many {
		many[i] = rate("fx." + string(rune('a'+i)))
	}
	for _, c := range []struct {
		name string
		cmd  interface{ Validate() error }
		want string
	}{
		{"reserve without exposure rates", reserve, "OK"},
		{"reserve with two exposure rates", func() ReserveCredit {
			c := reserve
			c.ExposureFXRates = []ConfigRef{rate("fx.eur"), rate("fx.hkd")}
			return c
		}(), "OK"},
		{"an exposure rate of another kind", func() ReserveCredit {
			c := reserve
			c.ExposureFXRates = []ConfigRef{{Kind: KindCurrency, ConfigID: "EUR", Revision: 1}}
			return c
		}(), "SCHEMA_INVALID /args/exposure_fx_rates/0/kind"},
		{"the same rate twice", func() ReserveCredit {
			c := reserve
			c.ExposureFXRates = []ConfigRef{rate("fx.eur"), rate("fx.eur")}
			return c
		}(),
			"SCHEMA_INVALID /args/exposure_fx_rates/1/id"},
		{"the order's own rate again", func() ReserveCredit { c := reserve; c.ExposureFXRates = []ConfigRef{rate("fx.cny.usd")}; return c }(),
			"SCHEMA_INVALID /args/exposure_fx_rates/0/id"},
		{"too many exposure rates", func() ReserveCredit { c := reserve; c.ExposureFXRates = many; return c }(), "SCHEMA_INVALID /args/exposure_fx_rates"},
		{"release without a cause", release, "OK"},
		{"release superseded", func() ReleaseCredit { c := release; c.Cause = CauseSuperseded; return c }(), "OK"},
		{"release for another cause", func() ReleaseCredit { c := release; c.Cause = "forgotten"; return c }(), "SCHEMA_INVALID /args/cause"},
		{"carry over", consume, "OK"},
		{"carry over from itself", func() ConsumeCredit { c := consume; c.CarriedFrom = c.ReservationID; return c }(), "SCHEMA_INVALID /args/carried_from"},
		{"carry over from no identifier", func() ConsumeCredit { c := consume; c.CarriedFrom = "res 1"; return c }(), "SCHEMA_INVALID /args/carried_from"},
		{"verify a payer", verify, "OK"},
		{"verify without a customer", func() VerifyPayer { c := verify; c.CustomerID = ""; return c }(), "SCHEMA_INVALID /args/customer_id"},
		{"verify without evidence", func() VerifyPayer { c := verify; c.EvidenceIDs = nil; return c }(), "SCHEMA_INVALID /args/evidence_ids"},
		{"verify without a reason", func() VerifyPayer { c := verify; c.Reason = ""; return c }(), "SCHEMA_INVALID /args/reason"},
		{"verify at version 0", func() VerifyPayer { c := verify; c.ExpectedVersion = 0; return c }(), "SCHEMA_INVALID /args/expected_version"},
	} {
		if got := code(c.cmd.Validate()); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
