package config

import "testing"

func regulation(lists ...ScreeningList) Content {
	return Content{Kind: KindScreeningRegulation, ValidFrom: start,
		ScreeningRegulation: &ScreeningRegulation{Lists: lists, RescreenIntervalDays: 90}}
}

// ADR-BR-017, SR-19: a foreign list is enabled only with the tenant's legal confirmation, and the
// publication cites it among its evidence, so the publisher must be able to read it.
func TestForeignListNeedsCitedLegalConfirmation(t *testing.T) {
	un := ScreeningList{ListCode: "un.consolidated", Jurisdiction: "UN"}
	ofac := ScreeningList{ListCode: "us.ofac.sdn", Jurisdiction: JurisdictionForeign, LegalConfirmationEvidenceID: "legal.ofac"}
	if got := code(Validate("screening", regulation(un, ScreeningList{ListCode: "us.ofac.sdn", Jurisdiction: JurisdictionForeign}))); got !=
		"INVALID_KNOWN_VALUE /args/data/lists/1/legal_confirmation_evidence_id" {
		t.Fatalf("a foreign list without its legal confirmation: %s", got)
	}
	r := New(KindScreeningRegulation, "screening")
	mustSave(t, r, regulation(un, ofac), editor, 1, start)
	if _, err := r.Publish(1, Facts{}, reviewer, 1, []string{"evidence"}, clock); code(err) != "REFERENCE_INVALID /args/evidence_ids" {
		t.Fatalf("the legal confirmation must be cited: %s", code(err))
	}
	if _, err := r.Publish(1, Facts{}, reviewer, 1, []string{"evidence", "legal.ofac"}, clock); err != nil {
		t.Fatal(err)
	}
	if r.Active == nil || r.Active.Content.ScreeningRegulation.Lists[1].LegalConfirmationEvidenceID != "legal.ofac" {
		t.Fatalf("active %+v", r.Active)
	}
	only := New(KindScreeningRegulation, "cn.un")
	mustSave(t, only, regulation(un), editor, 1, start)
	if _, err := only.Publish(1, Facts{}, reviewer, 1, []string{"evidence"}, clock); err != nil {
		t.Fatal("China and UN lists need no legal confirmation:", err)
	}
}

func TestScreeningRegulationRoundTrips(t *testing.T) {
	c := regulation(ScreeningList{ListCode: "cn.uel", Jurisdiction: "CN"},
		ScreeningList{ListCode: "eu.fsf", Jurisdiction: JurisdictionForeign, LegalConfirmationEvidenceID: "legal.eu"})
	raw, err := c.DataJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"lists":[{"list_code":"cn.uel","jurisdiction":"CN"},{"list_code":"eu.fsf","jurisdiction":"foreign",`+
		`"legal_confirmation_evidence_id":"legal.eu"}],"rescreen_interval_days":90}` {
		t.Fatalf("stored data %s", raw)
	}
	back := Content{Kind: KindScreeningRegulation, ValidFrom: c.ValidFrom}
	if err = back.DecodeData(raw); err != nil || back.ScreeningRegulation.Lists[1].LegalConfirmationEvidenceID != "legal.eu" {
		t.Fatalf("decoded %+v %v", back.ScreeningRegulation, err)
	}
	h1, _ := c.Hash()
	h2, _ := back.Hash()
	if len(h1) != 64 || h1 != h2 {
		t.Fatal("a decoded revision hashes as saved")
	}
}
