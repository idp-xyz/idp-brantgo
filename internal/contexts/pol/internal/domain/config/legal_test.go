package config

import (
	"slices"
	"testing"
)

func legal(law, cisg, rule string) Content {
	return Content{Kind: KindLegalTerms, ValidFrom: start, LegalTerms: &LegalTerms{Name: "legal terms", GoverningLaw: law, CISG: cisg,
		FormationRule: rule, DisputeResolution: DisputeResolution{Method: "arbitration", Institution: "CIETAC", Seat: "Shanghai", Language: "en"}}}
}

// WBS S3-17 (G0 F-02; CISG Art. 18(2), 23; PRC Civil Code Art. 137, 483, 484): under the CISG and
// under PRC law an acceptance takes effect, and the contract is concluded, when it reaches the
// offeror, so legal terms of either state the arrival rule; for another law without the CISG the
// rule is the publisher's statement, dispatch included (TRD decides whether it can form contracts
// under it).
func TestFormationRuleFollowsTheGoverningLaw(t *testing.T) {
	for _, c := range []struct{ law, cisg, rule, want string }{
		{"CN", "applies", "arrival", "OK"},
		{"CN", "excluded", "arrival", "OK"},
		{"DE", "applies", "arrival", "OK"},
		{"DE", "excluded", "arrival", "OK"},
		{"GB-ENG", "excluded", "dispatch", "OK"},
		{"CN", "applies", "dispatch", "INVALID_KNOWN_VALUE /args/data/formation_rule"},
		{"CN", "excluded", "dispatch", "INVALID_KNOWN_VALUE /args/data/formation_rule"},
		{"DE", "applies", "dispatch", "INVALID_KNOWN_VALUE /args/data/formation_rule"},
		{"GB-ENG", "applies", "dispatch", "INVALID_KNOWN_VALUE /args/data/formation_rule"},
	} {
		if got := code(Validate("legal", legal(c.law, c.cisg, c.rule))); got != c.want {
			t.Errorf("%s, CISG %s, %s: %s, want %s", c.law, c.cisg, c.rule, got, c.want)
		}
	}
	l := legal("DE", "applies", "arrival")
	if refs := l.References(); len(refs) != 0 {
		t.Fatalf("legal terms refer to nothing: %v", refs)
	}
	raw, err := l.DataJSON()
	if err != nil || string(raw) != `{"name":"legal terms","governing_law":"DE","cisg":"applies","formation_rule":"arrival",`+
		`"dispute_resolution":{"method":"arbitration","institution":"CIETAC","seat":"Shanghai","language":"en"}}` {
		t.Fatalf("stored data %s %v", raw, err)
	}
}

// The tenant's default (G0 F-02: 默认 CISG 与中国法，由租户配置): a company profile names its default
// legal terms by exact revision, configured and in force at its save and publication; a profile
// without one stores the data, and so has the content hash, it had before the member existed.
func TestCompanyProfileNamesItsDefaultLegalTerms(t *testing.T) {
	c := company("company.A")
	raw, err := c.DataJSON()
	if err != nil || string(raw) != `{"legal_name":"合成企业","registration_no":"SYNTHETIC","country":"CN","address":"合成地址",`+
		`"base_currency":"CNY","time_zone":"Asia/Shanghai","policy_scope":"synthetic"}` {
		t.Fatalf("a profile without a default %s %v", raw, err)
	}
	ref := Ref{Kind: string(KindLegalTerms), ConfigID: "legal.default", Revision: 2}
	c.CompanyProfile.DefaultLegalTerms = &ref
	want := Reference{Path: "/args/data/default_legal_terms", Kind: KindLegalTerms, ID: "legal.default", Revision: 2}
	if refs := c.References(); !slices.Contains(refs, want) {
		t.Fatalf("references %v", refs)
	}
	cny := currencyFact("CNY", 2)
	for _, f := range []struct {
		facts Facts
		want  string
	}{
		{facts(cny), "REFERENCE_INVALID /args/data/default_legal_terms"},
		{facts(cny, exactFact(KindLegalTerms, "legal.default", 2, nil)), "OK"},
		{facts(cny, exactFact(KindLegalTerms, "legal.default", 2, reject(CodeConfigNotActive, "/"))),
			"CONFIG_NOT_ACTIVE /args/data/default_legal_terms"},
	} {
		if got := code(CheckReferences("company.A", c, f.facts)); got != f.want {
			t.Errorf("%s, want %s", got, f.want)
		}
	}
	back := Content{Kind: KindCompanyProfile, ValidFrom: start}
	raw, _ = c.DataJSON()
	if err = back.DecodeData(raw); err != nil || back.CompanyProfile.DefaultLegalTerms == nil || *back.CompanyProfile.DefaultLegalTerms != ref {
		t.Fatalf("round trip %s %v", raw, err)
	}
}
