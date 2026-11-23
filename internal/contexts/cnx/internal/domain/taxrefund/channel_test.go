package taxrefund

import "testing"

// S6-D05 adds the channel to SM-CNX-TAXCASE without removing a manual pair. not_received releases
// evidence the way a rejection does; unknown still holds it and is not a filed stage (S5R-D20).
func TestChannelStagesS6D05(t *testing.T) {
	c := &Case{ID: "c1", Status: Submitted}
	status, err := Record(c, nil, nil, &Result{Stage: Unknown, Reason: "timed out"})
	if err != nil || status != Unknown {
		t.Fatalf("submitted to unknown: %s %s", status, code(err))
	}
	c.Status = status
	for _, stage := range []string{Approved, Rejected, NotReceived} {
		got, err := Record(c, nil, nil, &Result{Stage: stage})
		if err != nil || got != stage {
			t.Fatalf("unknown to %s: %s %s", stage, got, code(err))
		}
	}
	if _, err = Record(c, nil, nil, &Result{Stage: Received}); code(err) != "INVALID_STATE /args/stage" {
		t.Fatalf("unknown to received: %s", code(err))
	}
	if got, err := Record(&Case{ID: "c2", Status: Planned}, nil, nil, &Result{Stage: Submitted}); err != nil || got != Submitted {
		t.Fatalf("planned to submitted: %s %s", got, code(err))
	}
	if got, err := Record(&Case{ID: "c3", Status: Submitted}, nil, nil, &Result{Stage: Approved}); err != nil || got != Approved {
		t.Fatalf("submitted to approved: %s %s", got, code(err))
	}
	released := &Allocation{CaseStatus: NotReceived, InvoiceStatus: "verified"}
	if released.Status() != Released || released.Counts() {
		t.Fatalf("not_received allocation %s counts %v", released.Status(), released.Counts())
	}
	held := &Allocation{CaseStatus: Unknown, InvoiceStatus: "verified"}
	if held.Status() != Standing || !held.Counts() {
		t.Fatalf("unknown allocation %s counts %v", held.Status(), held.Counts())
	}
	if Filed(Unknown) || Filed(NotReceived) || !Filed(Submitted) || !Filed(Approved) {
		t.Fatal("filed")
	}
}
