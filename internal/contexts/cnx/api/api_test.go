package api

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func verdict(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}

func validEvaluate() Evaluate {
	return Evaluate{IdempotencyKey: "k1", OrderID: "o1", OrderRevision: 1, RegulationID: "screening", RegulationRevision: 1,
		ProgrammeLists:   []string{"cn.embargo", "un.sc"},
		Destination:      &Destination{Country: "DE", Reason: "B/L 显示汉堡卸货", EvidenceIDs: []string{"src.1"}},
		AddressCountries: []AddressCountry{{PartyID: "p1", AddressID: "hq", Country: "DE", EvidenceIDs: []string{"src.2"}}},
		EndUse:           &EndUse{Statement: "零售", EvidenceIDs: []string{"src.3"}}}
}

// The closed contract of trade_compliance.evaluate: identifiers, an order revision, a CAS value
// (0: no check yet), a screening_regulation revision, ISO 3166-1 alpha-2 countries, and evidence
// for every entered country and the end use (S5 D-8).
func TestEvaluateContract(t *testing.T) {
	for _, c := range []struct {
		edit func(*Evaluate)
		want string
	}{
		{func(*Evaluate) {}, "OK"},
		{func(c *Evaluate) { c.Destination, c.AddressCountries, c.EndUse = nil, nil, nil }, "OK"},
		{func(c *Evaluate) { c.IdempotencyKey = "" }, "SCHEMA_INVALID /idempotency_key"},
		{func(c *Evaluate) { c.OrderID = "bad id" }, "SCHEMA_INVALID /args/order_id"},
		{func(c *Evaluate) { c.OrderRevision = 0 }, "SCHEMA_INVALID /args/order_revision"},
		{func(c *Evaluate) { c.ExpectedVersion = -1 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *Evaluate) { c.RegulationID = strings.Repeat("r", 41) }, "SCHEMA_INVALID /args/regulation_id"},
		{func(c *Evaluate) { c.RegulationRevision = 0 }, "SCHEMA_INVALID /args/regulation_revision"},
		{func(c *Evaluate) { c.ProgrammeLists = nil }, "SCHEMA_INVALID /args/programme_lists"},
		{func(c *Evaluate) { c.ProgrammeLists = []string{"a", "a"} }, "SCHEMA_INVALID /args/programme_lists/1"},
		{func(c *Evaluate) { c.ProgrammeLists = []string{strings.Repeat("l", 33)} }, "SCHEMA_INVALID /args/programme_lists/0"},
		{func(c *Evaluate) { c.Destination.Country = "UK" }, "SCHEMA_INVALID /args/destination/country"},
		{func(c *Evaluate) { c.Destination.Country = "de" }, "SCHEMA_INVALID /args/destination/country"},
		{func(c *Evaluate) { c.Destination.Country = "DEU" }, "SCHEMA_INVALID /args/destination/country"},
		{func(c *Evaluate) { c.Destination.Reason = " " }, "SCHEMA_INVALID /args/destination/reason"},
		{func(c *Evaluate) { c.Destination.EvidenceIDs = nil }, "SCHEMA_INVALID /args/destination/evidence_ids"},
		{func(c *Evaluate) { c.Destination.EvidenceIDs = []string{"a", "a"} }, "SCHEMA_INVALID /args/destination/evidence_ids/1"},
		{func(c *Evaluate) { c.AddressCountries[0].PartyID = "" }, "SCHEMA_INVALID /args/address_countries/0/party_id"},
		{func(c *Evaluate) { c.AddressCountries = append(c.AddressCountries, c.AddressCountries[0]) }, "SCHEMA_INVALID /args/address_countries/1/address_id"},
		{func(c *Evaluate) { c.AddressCountries[0].Country = "" }, "SCHEMA_INVALID /args/address_countries/0/country"},
		{func(c *Evaluate) { c.AddressCountries[0].EvidenceIDs = nil }, "SCHEMA_INVALID /args/address_countries/0/evidence_ids"},
		{func(c *Evaluate) { c.AddressCountries = make([]AddressCountry, MaxAddresses+1) }, "SCHEMA_INVALID /args/address_countries"},
		{func(c *Evaluate) { c.EndUse.Statement = strings.Repeat("x", MaxStatement+1) }, "SCHEMA_INVALID /args/end_use/statement"},
		{func(c *Evaluate) { c.EndUse.EvidenceIDs = nil }, "SCHEMA_INVALID /args/end_use/evidence_ids"},
	} {
		cmd := validEvaluate()
		c.edit(&cmd)
		if got := verdict(cmd.Validate()); got != c.want {
			t.Errorf("want %s, got %s", c.want, got)
		}
	}
}

// trade_compliance.decide: clear states license_required, blocked omits it (CNX-INV-001: the
// licence flag is part of a clear conclusion); a reason always.
func TestDecideContract(t *testing.T) {
	yes := true
	valid := func() Decide {
		return Decide{IdempotencyKey: "k1", CheckID: "tcc.1", ExpectedVersion: 1, EvaluationID: "tce.1", Conclusion: ConclusionClear,
			Reason: "核查 clear", LicenseRequired: &yes}
	}
	for _, c := range []struct {
		edit func(*Decide)
		want string
	}{
		{func(*Decide) {}, "OK"},
		{func(c *Decide) { c.Conclusion, c.LicenseRequired = ConclusionBlocked, nil }, "OK"},
		{func(c *Decide) { c.Conclusion = ConclusionBlocked }, "SCHEMA_INVALID /args/license_required"},
		{func(c *Decide) { c.LicenseRequired = nil }, "SCHEMA_INVALID /args/license_required"},
		{func(c *Decide) { c.Conclusion = "pending" }, "SCHEMA_INVALID /args/conclusion"},
		{func(c *Decide) { c.ExpectedVersion = 0 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *Decide) { c.EvaluationID = "" }, "SCHEMA_INVALID /args/evaluation_id"},
		{func(c *Decide) { c.Reason = "" }, "SCHEMA_INVALID /args/reason"},
		{func(c *Decide) { c.EvidenceIDs = []string{"bad id"} }, "SCHEMA_INVALID /args/evidence_ids/0"},
	} {
		cmd := valid()
		c.edit(&cmd)
		if got := verdict(cmd.Validate()); got != c.want {
			t.Errorf("want %s, got %s", c.want, got)
		}
	}
}

// trade_compliance.reopen: each cause takes its own members and refuses the others'.
func TestReopenContract(t *testing.T) {
	for _, c := range []struct {
		cmd  Reopen
		want string
	}{
		{Reopen{IdempotencyKey: "k", Cause: CauseRegulationRevised, CheckID: "c", ExpectedVersion: 2, RegulationID: "screening", RegulationRevision: 2, Reason: "r"}, "OK"},
		{Reopen{IdempotencyKey: "k", Cause: CauseExpired, CheckID: "c", ExpectedVersion: 2, Reason: "r"}, "OK"},
		{Reopen{IdempotencyKey: "k", Cause: CauseProgrammeUpdated, CheckID: "c", ExpectedVersion: 2, Reason: "r"}, "OK"},
		{Reopen{IdempotencyKey: "k", Cause: CauseProgrammeUpdated, CheckID: "c", ExpectedVersion: 2, RegulationRevision: 2, Reason: "r"}, "SCHEMA_INVALID /args/regulation_id"},
		{Reopen{IdempotencyKey: "k", Cause: CauseOrderAmended, OrderID: "o", OrderRevision: 2, Reason: "r"}, "OK"},
		{Reopen{IdempotencyKey: "k", Cause: "amended", CheckID: "c", ExpectedVersion: 2, Reason: "r"}, "SCHEMA_INVALID /args/cause"},
		{Reopen{IdempotencyKey: "k", Cause: CauseRegulationRevised, CheckID: "c", ExpectedVersion: 2, Reason: "r"}, "SCHEMA_INVALID /args/regulation_id"},
		{Reopen{IdempotencyKey: "k", Cause: CauseExpired, CheckID: "c", ExpectedVersion: 2, RegulationID: "s", Reason: "r"}, "SCHEMA_INVALID /args/regulation_id"},
		{Reopen{IdempotencyKey: "k", Cause: CauseExpired, CheckID: "c", ExpectedVersion: 2, OrderID: "o", Reason: "r"}, "SCHEMA_INVALID /args/order_id"},
		{Reopen{IdempotencyKey: "k", Cause: CauseExpired, ExpectedVersion: 2, Reason: "r"}, "SCHEMA_INVALID /args/check_id"},
		{Reopen{IdempotencyKey: "k", Cause: CauseExpired, CheckID: "c", Reason: "r"}, "SCHEMA_INVALID /args/expected_version"},
		{Reopen{IdempotencyKey: "k", Cause: CauseOrderAmended, CheckID: "c", OrderID: "o", OrderRevision: 2, Reason: "r"}, "SCHEMA_INVALID /args/check_id"},
		{Reopen{IdempotencyKey: "k", Cause: CauseOrderAmended, OrderID: "o", OrderRevision: 1, Reason: "r"}, "SCHEMA_INVALID /args/order_revision"},
		{Reopen{IdempotencyKey: "k", Cause: CauseOrderAmended, OrderID: "o", OrderRevision: 2, ExpectedVersion: 1, Reason: "r"}, "SCHEMA_INVALID /args/expected_version"},
		{Reopen{IdempotencyKey: "k", Cause: CauseOrderAmended, OrderID: "o", OrderRevision: 2}, "SCHEMA_INVALID /args/reason"},
	} {
		if got := verdict(c.cmd.Validate()); got != c.want {
			t.Errorf("%+v: want %s, got %s", c.cmd, c.want, got)
		}
	}
}

// The error catalog's rows for the codes CNX declares: TRADE_COMPLIANCE_NOT_CLEAR, REFERENCE_STALE
// 409; ORDER_NOT_ACTIVE, DECLARATION_PROFILE_UNVERIFIED, DEPENDENCY_NOT_SUPPORTED 422.
func TestCodesFollowTheCatalog(t *testing.T) {
	want := map[string]kernel.Category{
		"TRADE_COMPLIANCE_NOT_CLEAR": kernel.CategoryConflict, "REFERENCE_STALE": kernel.CategoryConflict,
		"ORDER_NOT_ACTIVE": kernel.CategoryInvalid, "DECLARATION_PROFILE_UNVERIFIED": kernel.CategoryInvalid,
		"DEPENDENCY_NOT_SUPPORTED": kernel.CategoryInvalid,
	}
	got := Codes()
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for code, cat := range want {
		if c, ok := Category(code); !ok || c != cat || got[code] != cat {
			t.Errorf("%s: %v %v", code, c, ok)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an undeclared code panics")
		}
	}()
	_ = Reject("SURPRISE", "/", "")
}

func guardRow(clear bool, status, reason, check, decision string, license bool, reg *string, rev *int64, until *time.Time, code *string) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*bool) = clear
		*dest[1].(**string) = &status
		*dest[2].(**string) = &reason
		*dest[3].(**string) = &check
		*dest[4].(**string) = &decision
		*dest[5].(*bool) = license
		*dest[6].(**string) = reg
		*dest[7].(**int64) = rev
		*dest[8].(**time.Time) = until
		*dest[9].(**string) = code
		return nil
	}
}

// TradeComplianceGuard v1's rows as the caller reads them: only a clear row without a code passes;
// every other row is TRADE_COMPLIANCE_NOT_CLEAR (409) at the caller's path with status and reason.
func TestTradeComplianceGuardVerdicts(t *testing.T) {
	b := OrderBinding{OrderID: "o1", OrderRevision: 2, OrderHash: strings.Repeat("a", 64)}
	reg, rev, until := "screening", int64(3), time.Date(2026, 12, 1, 0, 0, 0, 1500, time.UTC)
	notClear := CodeTradeComplianceNotClear
	for _, c := range []struct {
		scan   func(dest ...any) error
		want   string
		reason string
	}{
		{guardRow(true, StatusClear, ReasonCleared, "tcc.1", "tcd.1", true, &reg, &rev, &until, nil), "OK", ReasonCleared},
		{guardRow(false, StatusPending, ReasonOrderRevised, "", "", false, nil, nil, nil, &notClear), "TRADE_COMPLIANCE_NOT_CLEAR /args/order_id", ReasonOrderRevised},
		{guardRow(false, StatusBlocked, ReasonBlocked, "tcc.1", "tcd.2", false, &reg, &rev, nil, &notClear), "TRADE_COMPLIANCE_NOT_CLEAR /args/order_id", ReasonBlocked},
		{guardRow(false, StatusExpired, ReasonExpired, "tcc.1", "tcd.1", true, &reg, &rev, &until, &notClear), "TRADE_COMPLIANCE_NOT_CLEAR /args/order_id", ReasonExpired},
		{guardRow(true, StatusClear, ReasonCleared, "tcc.1", "tcd.1", false, &reg, &rev, &until, &notClear), "TRADE_COMPLIANCE_NOT_CLEAR /args/order_id", ReasonCleared},
	} {
		v, err := ScanTradeComplianceGuard(b, c.scan)
		if err != nil {
			t.Fatal(err)
		}
		if got := verdict(v.Check("/args/order_id")); got != c.want || v.Reason != c.reason || v.Ref != b {
			t.Errorf("%+v: want %s, got %s", v, c.want, got)
		}
	}
	v, _ := ScanTradeComplianceGuard(b, guardRow(true, StatusClear, ReasonCleared, "tcc.1", "tcd.1", true, &reg, &rev, &until, nil))
	if v.RegulationID != reg || v.RegulationRevision != rev || !v.ValidUntil.Equal(kernel.Instant(until)) || !v.LicenseRequired ||
		v.Evidence() != "clear/cleared/tcc.1/tcd.1/true" {
		t.Fatalf("%+v %s", v, v.Evidence())
	}
	if args := TradeComplianceGuardArgs(b, until); len(args) != 4 || args[3].(time.Time) != kernel.Instant(until) {
		t.Fatalf("%v", args)
	}
}
