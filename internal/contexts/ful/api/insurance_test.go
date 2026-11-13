package api

import (
	"strings"
	"testing"
)

func policy() RecordInsurance {
	return RecordInsurance{IdempotencyKey: "k", DocumentSetID: "docs.1", ShipmentID: "shipment.1", Insurer: ServiceRoleRef{PartyID: "insurer.1",
		ServiceRoleID: "cargo", Revision: 1}, PolicyNumber: "PIC-2026-0001", Clauses: []string{"ICC (A)", "Institute War Clauses (Cargo)"},
		IssuedOn: "2026-12-18", Currency: ConfigRef{Kind: KindCurrency, ID: "USD", Revision: 1}, InsuredAmount: "8602.00", Premium: "12.00",
		FileID: "file.policy", EvidenceIDs: []string{"evidence"}}
}

// The closed contract of insurance.record (WBS S5-16; FUL-INV-005; tactical model
// InsurancePolicyRecord: 保险服务方、保单号、险别、币种、投保金额; F-03).
func TestRecordInsuranceContract(t *testing.T) {
	for _, c := range []struct {
		edit func(*RecordInsurance)
		want string
	}{
		{func(*RecordInsurance) {}, "OK"},
		{func(c *RecordInsurance) { c.IdempotencyKey = "" }, "SCHEMA_INVALID /idempotency_key"},
		{func(c *RecordInsurance) { c.ShipmentID = "a b" }, "SCHEMA_INVALID /args/shipment_id"},
		{func(c *RecordInsurance) { c.DocumentSetID = "" }, "SCHEMA_INVALID /args/document_set_id"},
		{func(c *RecordInsurance) { c.ExpectedVersion = -1 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *RecordInsurance) { c.ExpectedVersion = 3 }, "OK"},
		{func(c *RecordInsurance) { c.Insurer.PartyID = "" }, "SCHEMA_INVALID /args/insurer/party_id"},
		{func(c *RecordInsurance) { c.Insurer.ServiceRoleID = "a b" }, "SCHEMA_INVALID /args/insurer/service_role_id"},
		{func(c *RecordInsurance) { c.Insurer.Revision = 0 }, "SCHEMA_INVALID /args/insurer/revision"},
		{func(c *RecordInsurance) { c.PolicyNumber = " " }, "SCHEMA_INVALID /args/policy_number"},
		{func(c *RecordInsurance) { c.PolicyNumber = strings.Repeat("9", MaxDocumentNumber+1) }, "SCHEMA_INVALID /args/policy_number"},
		{func(c *RecordInsurance) { c.Clauses = nil }, "SCHEMA_INVALID /args/clauses"},
		{func(c *RecordInsurance) { c.Clauses = make([]string, MaxClauses+1) }, "SCHEMA_INVALID /args/clauses"},
		{func(c *RecordInsurance) { c.Clauses = []string{"ICC (A)", "ICC (A)"} }, "SCHEMA_INVALID /args/clauses/1"},
		{func(c *RecordInsurance) { c.Clauses = []string{"a\nb"} }, "SCHEMA_INVALID /args/clauses/0"},
		{func(c *RecordInsurance) { c.FileID = "" }, "SCHEMA_INVALID /args/file_id"},
		{func(c *RecordInsurance) { c.IssuedOn = "2026-02-30" }, "SCHEMA_INVALID /args/issued_on"},
		{func(c *RecordInsurance) { c.Currency.Kind = "fx_rate" }, "SCHEMA_INVALID /args/currency/kind"},
		{func(c *RecordInsurance) { c.Currency.Revision = 0 }, "SCHEMA_INVALID /args/currency/revision"},
		{func(c *RecordInsurance) { c.InsuredAmount = "0" }, "SCHEMA_INVALID /args/insured_amount"},
		{func(c *RecordInsurance) { c.InsuredAmount = "-1" }, "SCHEMA_INVALID /args/insured_amount"},
		{func(c *RecordInsurance) { c.InsuredAmount = "8,602.00" }, "SCHEMA_INVALID /args/insured_amount"},
		{func(c *RecordInsurance) { c.Premium = "" }, "SCHEMA_INVALID /args/premium"},
		{func(c *RecordInsurance) { c.Premium = "0" }, "OK"},
		{func(c *RecordInsurance) { c.CoveragePercent = "110%" }, "SCHEMA_INVALID /args/coverage_percent"},
		{func(c *RecordInsurance) { c.CoveragePercent = "120.5" }, "OK"},
		{func(c *RecordInsurance) { c.EvidenceIDs = []string{"e", "e"} }, "SCHEMA_INVALID /args/evidence_ids/1"},
		{func(c *RecordInsurance) { c.EvidenceIDs = nil }, "OK"},
	} {
		cmd := policy()
		c.edit(&cmd)
		if got := outcome(cmd.Validate()); got != c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
}
