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
	if err == nil {
		return "OK"
	}
	return err.Error()
}

func TestCaptureLeadContract(t *testing.T) {
	ok := CaptureLead{IdempotencyKey: "k1", LeadID: "lead.1", SourceChannel: "fair", CompanyName: "Northwind",
		ContactName: "Ada", Summary: "asked about shirts"}
	if code(ok.Validate()) != "OK" {
		t.Fatal(code(ok.Validate()))
	}
	bad := ok
	bad.ExpectedVersion = 1
	if code(bad.Validate()) != "SCHEMA_INVALID /args/expected_version" {
		t.Fatal(code(bad.Validate()))
	}
	bad = ok
	bad.Summary = ""
	if code(bad.Validate()) != "SCHEMA_INVALID /args/summary" {
		t.Fatal(code(bad.Validate()))
	}
	bad = ok
	bad.CompanyName = strings.Repeat("名", 201)
	if code(bad.Validate()) != "SCHEMA_INVALID /args/company_name" {
		t.Fatal(code(bad.Validate()))
	}
}

func TestAdvanceOpportunityContract(t *testing.T) {
	create := AdvanceOpportunity{IdempotencyKey: "k1", OpportunityID: "opp.1", Title: "Spring", Currency: "USD",
		ExpectedAmountMinor: 96000, CustomerID: "customer.1"}
	if code(create.Validate()) != "OK" {
		t.Fatal(code(create.Validate()))
	}
	create.Stage = StageProposal
	if code(create.Validate()) != "SCHEMA_INVALID /args/stage" {
		t.Fatal(code(create.Validate()))
	}
	create.Stage = ""
	create.Currency = "usd"
	if code(create.Validate()) != "SCHEMA_INVALID /args/expected_amount_minor" {
		t.Fatal(code(create.Validate()))
	}
	advance := AdvanceOpportunity{IdempotencyKey: "k1", OpportunityID: "opp.1", ExpectedVersion: 1, Stage: StageProposal}
	if code(advance.Validate()) != "OK" {
		t.Fatal(code(advance.Validate()))
	}
	advance.Title = "renamed"
	if code(advance.Validate()) != "SCHEMA_INVALID /args/title" {
		t.Fatal(code(advance.Validate()))
	}
	advance.Title = ""
	advance.Stage = "nurture"
	if code(advance.Validate()) != "SCHEMA_INVALID /args/stage" {
		t.Fatal(code(advance.Validate()))
	}
}

func TestRecordFollowUpContract(t *testing.T) {
	ok := RecordFollowUp{IdempotencyKey: "k1", OpportunityID: "opp.1", ExpectedVersion: 1, FollowUpID: "fu.1",
		Summary: "sent the line sheet"}
	if code(ok.Validate()) != "OK" {
		t.Fatal(code(ok.Validate()))
	}
	ok.ExpectedVersion = 0
	if code(ok.Validate()) != "SCHEMA_INVALID /args/expected_version" {
		t.Fatal(code(ok.Validate()))
	}
}

func TestActiveStagesMatchTheMachine(t *testing.T) {
	for _, stage := range []string{StageQualified, StageProposal, StageNegotiation, StageWon} {
		if !ActiveStage(stage) {
			t.Errorf("%s should be active", stage)
		}
	}
	for _, stage := range []string{StageLost, StageAbandoned, ""} {
		if ActiveStage(stage) {
			t.Errorf("%s should be inactive", stage)
		}
	}
}
