package order

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S4 decision D-9, WF-03, ADR-BR-013: a denial of a source the cost snapshot of an active order's
// activation rests on records a risk on the order once — cause cost_source_denied, code
// COST_SOURCE_DENIED, at the order revision and row version it found — and changes nothing else of
// the order: status, row version and activation stay (the record blocks nothing). A draft order
// gets none (its activation re-checks the snapshot; TRD-INV-014), nor does another snapshot, nor a
// second delivery of the same attestation; only the consumer's system actor records one.
func TestCostSourceDenialRecordsARiskOnTheActiveOrder(t *testing.T) {
	consumer := kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.trd.cost_source_holds"}
	denial := quote.CostDenial{AttestationID: "denial.1", SourceID: "src.1", EventID: "evt.1"}
	decided := prepareOn.Add(time.Hour)
	o, r := approvedOrder(t, decided, decided.Add(48*time.Hour), denied)
	now := decided.Add(2 * time.Hour)
	if rr, err := o.RecordCostRisk(denial, []string{"snapshot.1"}, consumer, 5, now); err != nil || rr != nil || o.CostSnapshot() != "" {
		t.Fatalf("a draft order: %+v %v", rr, err)
	}
	if _, err := o.Activate(Activation{Review: r, IdempotencyKey: "k.1", Risks: []Risk{denied}, ApprovalID: "approval.1",
		CostSnapshotID: "snapshot.1"}, clerk, 3, decided.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	status, version := o.Status, o.RowVersion
	if rr, err := o.RecordCostRisk(denial, []string{"snapshot.1"}, reviewer, 5, now); code(err) != "PERMISSION_DENIED /" || rr != nil {
		t.Fatalf("a human recording a cost risk: %+v %v", rr, err)
	}
	if rr, err := o.RecordCostRisk(denial, []string{"snapshot.other"}, consumer, 5, now); err != nil || rr != nil || len(o.Risks) != 0 {
		t.Fatalf("another snapshot: %+v %v", rr, err)
	}
	rr, err := o.RecordCostRisk(denial, []string{"snapshot.1"}, consumer, 5, now)
	if err != nil || rr == nil || rr.ID != "cost_source_denied:denial.1" || rr.Cause != RiskCostSourceDenied || rr.Code != "COST_SOURCE_DENIED" ||
		rr.OrderRevision != o.OrderRevision || rr.OrderRowVersion != version || rr.Denial.SnapshotID != "snapshot.1" ||
		rr.Denial.AttestationID != "denial.1" || rr.Registered.By != consumer || !rr.Fresh || len(o.Risks) != 1 {
		t.Fatalf("risk %+v %v", rr, err)
	}
	if o.Status != status || o.RowVersion != version || o.Status != Active {
		t.Fatalf("the record changed the order: %s v%d", o.Status, o.RowVersion)
	}
	if again, err := o.RecordCostRisk(denial, []string{"snapshot.1"}, consumer, 5, now); err != nil || again != nil || len(o.Risks) != 1 {
		t.Fatalf("the same attestation again: %+v %v", again, err)
	}
	o.Status = Cancelled
	if rr, err := o.RecordCostRisk(quote.CostDenial{AttestationID: "denial.2"}, []string{"snapshot.1"}, consumer, 5, now); err != nil || rr != nil {
		t.Fatalf("an ended order: %+v %v", rr, err)
	}
}
