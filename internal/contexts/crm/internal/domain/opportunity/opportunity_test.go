package opportunity

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// SM-CRM-OPPORTUNITY: the registered advances, and no others. Follow-up stays on the stage.
func TestOpportunityStageMachine(t *testing.T) {
	o := New("opp.1", "Spring shirts", "lead.1", "customer.1", "USD", 96000, "sales.1", "trade")
	if o.Stage != StageQualified || o.RowVersion != 1 || !o.Active() {
		t.Fatalf("created %+v", o)
	}
	for _, step := range []struct {
		to       string
		previous string
		active   bool
	}{
		{StageProposal, StageQualified, true},
		{StageNegotiation, StageProposal, true},
		{StageWon, StageNegotiation, true},
	} {
		prev, err := o.Advance(step.to)
		if err != nil || prev != step.previous || o.Stage != step.to || o.Active() != step.active {
			t.Fatalf("advance to %s: prev %s err %v opp %+v", step.to, prev, err, o)
		}
	}
	if _, err := o.Advance(StageLost); code(err) != kernel.CodeInvalidState {
		t.Fatalf("won → lost: %v", err)
	}
	won := o.RowVersion
	if err := o.Record(FollowUp{ID: "fu.1", Summary: "asked for the size run", RecordedAt: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if o.Stage != StageWon || o.RowVersion != won+1 || len(o.FollowUps) != 1 {
		t.Fatalf("follow-up changed the opportunity: %+v", o)
	}
	if code(o.Record(FollowUp{ID: "fu.1", Summary: "again"})) != "BUSINESS_KEY_CONFLICT" {
		t.Fatal("duplicate follow-up id")
	}
}

// The side exits: abandoned from each open stage, lost from proposal and negotiation.
func TestOpportunityExits(t *testing.T) {
	for _, from := range []string{StageQualified, StageProposal, StageNegotiation} {
		o := New("opp.1", "t", "", "", "", 0, "sales.1", "")
		o.Stage = from
		if _, err := o.Advance(StageAbandoned); err != nil || o.Active() {
			t.Fatalf("%s → abandoned: %v %+v", from, err, o)
		}
	}
	for _, from := range []string{StageProposal, StageNegotiation} {
		o := New("opp.1", "t", "", "", "", 0, "sales.1", "")
		o.Stage = from
		if _, err := o.Advance(StageLost); err != nil || o.Active() {
			t.Fatalf("%s → lost: %v %+v", from, err, o)
		}
	}
	o := New("opp.1", "t", "", "", "", 0, "sales.1", "")
	if _, err := o.Advance(StageLost); code(err) != kernel.CodeInvalidState {
		t.Fatalf("qualified → lost: %v", err)
	}
	if _, err := o.Advance(StageWon); code(err) != kernel.CodeInvalidState {
		t.Fatalf("qualified → won: %v", err)
	}
}

// Follow-up is legal on every stage, including the terminal ones, and never moves one.
func TestFollowUpOnEveryStage(t *testing.T) {
	for _, stage := range []string{StageQualified, StageProposal, StageNegotiation, StageWon, StageLost, StageAbandoned} {
		o := New("opp.1", "t", "", "", "", 0, "sales.1", "")
		o.Stage = stage
		if err := o.Record(FollowUp{ID: "fu.1", Summary: "note", NextStep: "call"}); err != nil || o.Stage != stage {
			t.Fatalf("follow-up on %s: %v %+v", stage, err, o)
		}
	}
}

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code
	}
	if err == nil {
		return ""
	}
	return err.Error()
}
