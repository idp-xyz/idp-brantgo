package api

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// The S5R contract additions of POL (simulated sign-off E0 of 2026-11-13): the contract insurance
// percentage of trade terms (S5R-D06), the four refund methods (S5R-D14; the domain refuses
// not_applicable on a new revision), the programme lists of a screening regulation (S5R-D38).
func TestS5RContracts(t *testing.T) {
	for _, c := range []struct {
		kind string
		edit func(*ConfigData)
		want string
	}{
		{KindTradeTerms, func(d *ConfigData) { d.TradeTerms.InsurancePercent = "" }, "OK"},
		{KindTradeTerms, func(d *ConfigData) { d.TradeTerms.InsurancePercent = "110" }, "OK"},
		{KindTradeTerms, func(d *ConfigData) { d.TradeTerms.InsurancePercent = "125.5" }, "OK"},
		// The rule (at least 110) is POL's, INVALID_KNOWN_VALUE; the contract checks the form.
		{KindTradeTerms, func(d *ConfigData) { d.TradeTerms.InsurancePercent = "100" }, "OK"},
		{KindTradeTerms, func(d *ConfigData) { d.TradeTerms.InsurancePercent = "1100" }, "SCHEMA_INVALID /args/data/insurance_percent"},
		{KindTradeTerms, func(d *ConfigData) { d.TradeTerms.InsurancePercent = "110%" }, "SCHEMA_INVALID /args/data/insurance_percent"},
		{KindTradeTerms, func(d *ConfigData) { d.TradeTerms.InsurancePercent = "-110" }, "SCHEMA_INVALID /args/data/insurance_percent"},
		{KindExportTaxPolicy, func(d *ConfigData) { d.ExportTaxPolicy.RefundMethod = RefundExempt }, "OK"},
		{KindExportTaxPolicy, func(d *ConfigData) { d.ExportTaxPolicy.RefundMethod = RefundTaxable }, "OK"},
		{KindScreeningRegulation, func(d *ConfigData) {
			d.ScreeningRegulation.Programmes = []ScreeningList{{ListCode: "cn.country_programmes", Jurisdiction: JurisdictionCN},
				{ListCode: "eu.programmes", Jurisdiction: JurisdictionForeign, LegalConfirmationEvidenceID: "legal.eu"}}
		}, "OK"},
		{KindScreeningRegulation, func(d *ConfigData) {
			d.ScreeningRegulation.Programmes = []ScreeningList{{ListCode: "eu.programmes", Jurisdiction: JurisdictionForeign}}
		}, "SCHEMA_INVALID /args/data/programmes/0/legal_confirmation_evidence_id"},
		{KindScreeningRegulation, func(d *ConfigData) {
			d.ScreeningRegulation.Programmes = []ScreeningList{{ListCode: "un.p", Jurisdiction: JurisdictionUN}, {ListCode: "un.p", Jurisdiction: JurisdictionUN}}
		}, "SCHEMA_INVALID /args/data/programmes/1/list_code"},
		{KindScreeningRegulation, func(d *ConfigData) {
			d.ScreeningRegulation.Programmes = []ScreeningList{{ListCode: "un.p", Jurisdiction: "EU"}}
		}, "SCHEMA_INVALID /args/data/programmes/0/jurisdiction"},
		{KindScreeningRegulation, func(d *ConfigData) {
			d.ScreeningRegulation.Programmes = make([]ScreeningList, MaxScreeningLists+1)
		}, "SCHEMA_INVALID /args/data/programmes"},
	} {
		d := valid()[c.kind]
		if c.kind == KindScreeningRegulation {
			d = screeningRegulation().Data
		}
		c.edit(&d)
		if got := code(save(c.kind, d).Validate()); got != c.want {
			t.Errorf("%s: %s, want %s", c.kind, got, c.want)
		}
	}
	terms := TradeTerms{}
	if p, err := terms.ContractInsurancePercent(); err != nil || p.String() != "110" {
		t.Fatalf("terms without a percentage: %v %v", p, err)
	}
	terms.InsurancePercent = "120.5"
	if p, err := terms.ContractInsurancePercent(); err != nil || p.String() != "120.5" {
		t.Fatalf("120.5: %v %v", p, err)
	}
}

// S5R-D30 (POL-INV-001): every kind names the capability its publisher holds besides config.publish.
func TestS5RPublishCapabilities(t *testing.T) {
	for _, k := range kinds {
		want := PermissionAdministerConfigs
		switch k {
		case KindCreditPolicy:
			want = "config.publish.credit_policy"
		case KindExportTaxPolicy:
			want = "config.publish.export_tax_policy"
		case KindScreeningRegulation:
			want = "config.publish.screening_regulation"
		}
		if got := PublishCapability(k); got != want {
			t.Errorf("%s: %s, want %s", k, got, want)
		}
	}
	if PermissionAdministerConfigs != "config.retire" || PublishCapability("party_profile") != "" {
		t.Fatal("the general kinds are the configuration administrator's; an unknown kind names none")
	}
}

// ConfigAt v1 (S5R-D23, POL-INV-007): the query's form, the SQL function's row and the verdicts.
func TestS5RConfigAtContract(t *testing.T) {
	at := time.Date(2026, 11, 13, 1, 2, 3, 456789123, time.UTC)
	q := ConfigAtQuery{Kind: KindExportTaxPolicy, ConfigID: "export.tax", At: at}
	for _, c := range []struct {
		edit func(*ConfigAtQuery)
		want string
	}{
		{func(*ConfigAtQuery) {}, "OK"},
		{func(q *ConfigAtQuery) { q.Kind = "party_profile" }, "SCHEMA_INVALID /kind"},
		{func(q *ConfigAtQuery) { q.ConfigID = strings.Repeat("x", MaxConfigIDLength+1) }, "SCHEMA_INVALID /id"},
		{func(q *ConfigAtQuery) { q.At = time.Time{} }, "SCHEMA_INVALID /at"},
	} {
		v := q
		c.edit(&v)
		if got := code(v.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", v, got, c.want)
		}
	}
	if args := ConfigAtArgs(q); len(args) != 3 || args[0] != KindExportTaxPolicy || args[1] != "export.tax" ||
		!args[2].(time.Time).Equal(at.Truncate(time.Microsecond)) {
		t.Fatalf("arguments %v", args)
	}
	if !strings.Contains(ConfigAtSQL, ConfigAtFunction+"($1, $2, $3)") {
		t.Fatal(ConfigAtSQL)
	}
	row := func(known bool, revision *int64, status, code, hash *string) func(...any) error {
		return func(dest ...any) error {
			*dest[0].(*bool), *dest[1].(**int64), *dest[2].(**string), *dest[3].(**string), *dest[4].(**string) = known, revision, status, code, hash
			return nil
		}
	}
	s := func(v string) *string { return &v }
	two := int64(2)
	v, err := ScanConfigAt(q, row(true, &two, s(StatusSuperseded), nil, s("hash")))
	if err != nil || !v.Applies() || v.Ref != (ConfigRef{Kind: KindExportTaxPolicy, ConfigID: "export.tax", Revision: 2}) ||
		v.Status != StatusSuperseded || v.ContentHash != "hash" || v.Check("/args/policy_id") != nil {
		t.Fatalf("a revision applies: %+v %v", v, err)
	}
	v, _ = ScanConfigAt(q, row(true, nil, nil, s(CodeConfigNotActive), nil))
	if v.Applies() || v.Ref.Revision != 0 || code(v.Check("/args/policy_id")) != "CONFIG_NOT_ACTIVE /args/policy_id" {
		t.Fatalf("none applies: %+v", v)
	}
	v, _ = ScanConfigAt(q, row(false, nil, nil, s("REFERENCE_INVALID"), nil))
	if v.Applies() || code(v.Check("/args/policy_id")) != "REFERENCE_INVALID /args/policy_id" {
		t.Fatalf("unknown: %+v", v)
	}
	if _, err = ScanConfigAt(q, func(...any) error { return errors.New("scan") }); err == nil {
		t.Fatal("a failed scan")
	}
	r := RevisionAt{Code: CodeConfigNotActive}
	if code(r.Check("/x")) != "CONFIG_NOT_ACTIVE /x" || (RevisionAt{}).Check("/x") != nil {
		t.Fatal("RevisionAt.Check")
	}
	if !slices.Contains(refundMethods, RefundNotApplicable) {
		t.Fatal("not_applicable stays readable through the contract")
	}
}
