package api

import (
	"strings"
	"testing"
)

func screeningRegulation() SaveDraft {
	return SaveDraft{IdempotencyKey: "k1", Kind: KindScreeningRegulation, ConfigID: "screening", ValidFrom: "2026-10-22T00:00:00Z",
		Data: ConfigData{ScreeningRegulation: &ScreeningRegulation{RescreenIntervalDays: 90, Lists: []ScreeningList{
			{ListCode: "cn.uel", Jurisdiction: JurisdictionCN},
			{ListCode: "un.consolidated", Jurisdiction: JurisdictionUN},
			{ListCode: "us.ofac.sdn", Jurisdiction: JurisdictionForeign, LegalConfirmationEvidenceID: "legal.ofac"},
		}}}}
}

// ADR-BR-017, SR-19: lists are enabled in screening_regulation; a foreign list only with the
// tenant's legal confirmation.
func TestScreeningRegulationContract(t *testing.T) {
	for _, c := range []struct {
		edit func(*SaveDraft)
		want string
	}{
		{func(*SaveDraft) {}, "OK"},
		{func(c *SaveDraft) { c.Data.ScreeningRegulation.Lists[2].LegalConfirmationEvidenceID = "" },
			"SCHEMA_INVALID /args/data/lists/2/legal_confirmation_evidence_id"},
		{func(c *SaveDraft) { c.Data.ScreeningRegulation.Lists[1].ListCode = "cn.uel" }, "SCHEMA_INVALID /args/data/lists/1/list_code"},
		{func(c *SaveDraft) { c.Data.ScreeningRegulation.Lists[0].ListCode = strings.Repeat("x", 33) }, "SCHEMA_INVALID /args/data/lists/0/list_code"},
		{func(c *SaveDraft) { c.Data.ScreeningRegulation.Lists[0].Jurisdiction = "US" }, "SCHEMA_INVALID /args/data/lists/0/jurisdiction"},
		{func(c *SaveDraft) { c.Data.ScreeningRegulation.Lists[0].LegalConfirmationEvidenceID = "bad id" },
			"SCHEMA_INVALID /args/data/lists/0/legal_confirmation_evidence_id"},
		{func(c *SaveDraft) { c.Data.ScreeningRegulation.Lists = nil }, "SCHEMA_INVALID /args/data/lists"},
		{func(c *SaveDraft) { c.Data.ScreeningRegulation.RescreenIntervalDays = 0 }, "SCHEMA_INVALID /args/data/rescreen_interval_days"},
		{func(c *SaveDraft) { c.Data.Currency = &Currency{} }, "SCHEMA_INVALID /args/data"},
	} {
		cmd := screeningRegulation()
		c.edit(&cmd)
		if got := code(cmd.Validate()); got != c.want {
			t.Errorf("%s: want %s", got, c.want)
		}
	}
	if !KindImplemented(KindScreeningRegulation) {
		t.Fatal("screening_regulation is implemented")
	}
}
