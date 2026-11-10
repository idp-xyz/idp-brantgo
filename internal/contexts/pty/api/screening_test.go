package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return "OK"
}

func TestScreeningContracts(t *testing.T) {
	record := RecordScreening{IdempotencyKey: "k", PartyID: "acme", RegulationID: "screening", RegulationRevision: 1}
	decide := DecideScreening{IdempotencyKey: "k", PartyID: "acme", ExpectedVersion: 1, ScreeningID: "scr-1", Decision: DecisionFalsePositive,
		Reason: "different address", RegisterPositive: true}
	block := BlockParty{IdempotencyKey: "k", PartyID: "acme", Reason: "sanctioned owner"}
	for _, c := range []struct {
		err  error
		want string
	}{
		{record.Validate(), "OK"},
		{RecordScreening{IdempotencyKey: "k", PartyID: "acme", RegulationID: "screening"}.Validate(), "SCHEMA_INVALID /args/regulation_revision"},
		{RecordScreening{IdempotencyKey: "k", PartyID: "acme", RegulationRevision: 1}.Validate(), "SCHEMA_INVALID /args/regulation_id"},
		{decide.Validate(), "OK"},
		{DecideScreening{IdempotencyKey: "k", PartyID: "acme", ExpectedVersion: 1, ScreeningID: "scr-1", Decision: DecisionTrueMatch,
			Reason: "same entity", RegisterPositive: true}.Validate(), "SCHEMA_INVALID /args/register_positive"},
		{DecideScreening{IdempotencyKey: "k", PartyID: "acme", ExpectedVersion: 1, ScreeningID: "scr-1", Decision: DecisionFalsePositive}.Validate(),
			"SCHEMA_INVALID /args/reason"},
		{DecideScreening{IdempotencyKey: "k", PartyID: "acme", ExpectedVersion: 1, ScreeningID: "scr-1", Decision: "clear", Reason: "x"}.Validate(),
			"SCHEMA_INVALID /args/decision"},
		{DecideScreening{IdempotencyKey: "k", PartyID: "acme", ScreeningID: "scr-1", Decision: DecisionTrueMatch, Reason: "x"}.Validate(),
			"SCHEMA_INVALID /args/expected_version"},
		{block.Validate(), "OK"},
		{BlockParty{IdempotencyKey: "k", PartyID: "acme"}.Validate(), "SCHEMA_INVALID /args/reason"},
	} {
		if got := code(c.err); got != c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
}

// PartyScreeningGuard v1: only clear passes; everything else is PARTY_SCREENING_NOT_CLEAR (409).
func TestGuardResultCheck(t *testing.T) {
	if (ScreeningGuardResult{Clear: true, Status: ScreeningClear, Reason: ReasonScreened}).Check("/args/buyer") != nil {
		t.Fatal("clear passes")
	}
	err := ScreeningGuardResult{Status: ScreeningExpired, Reason: ReasonExpired}.Check("/args/buyer")
	r, ok := kernel.AsRejection(err)
	if !ok || r.Code != CodePartyScreeningNotClear || r.Category != kernel.CategoryConflict || r.Fields[0].Path != "/args/buyer" {
		t.Fatalf("not clear: %v", err)
	}
	var g ScreeningGuardResult
	if len(g.Targets()) != 5 {
		t.Fatal("one target per column of ScreeningGuardSQL")
	}
	for c := range codes {
		if Reject(c, "/x").Code != c {
			t.Fatal(c)
		}
	}
}
