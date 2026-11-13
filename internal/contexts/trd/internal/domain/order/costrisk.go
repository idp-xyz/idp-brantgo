package order

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// A risk record on an active order (WBS S4-06b; S4 decision D-9; WF-03, PL-07, SC-04 activation
// first; ADR-BR-013): CST denied a source the cost snapshot of the order's activation rests on after
// the order became active. The contract formed and stands, so the record blocks nothing — the order's
// status and row version, OrderGuard v1, order_state_v1 and every later step stay as they are — and
// tells the order's readers what changed upstream. A draft order has none: its activation re-checks
// the snapshot and its approval acknowledges the denial as an upstream risk (TRD-INV-014).

// RiskCostSourceDenied is the cause of a risk record on an active order, COST_SOURCE_DENIED its code.
const (
	RiskCostSourceDenied = "cost_source_denied"
	CodeCostSourceDenied = quote.CodeCostSourceDenied
)

// RiskRecord is a risk recorded on an active order: its cause and code, the order revision and row
// version it found, the denial (CST's attestation, the source, the event that carried it and the
// snapshot the order's activation names), and who registered it.
type RiskRecord struct {
	ID              string // cause ":" attestation ID
	Cause           string
	Code            string
	OrderRevision   int64
	OrderRowVersion int64
	Denial          quote.CostDenial
	Registered      quote.Act
	Fresh           bool
}

// CostSnapshot returns the cost snapshot the order's activation found, "" before the activation.
func (o *Order) CostSnapshot() string {
	for _, c := range o.Commits {
		if c.CostSnapshotID != "" {
			return c.CostSnapshotID
		}
	}
	return ""
}

// RecordCostRisk records the risk of denial d on the order, by the system actor by under authority
// epoch epoch at now, when the order is active and its activation's commit evidence names one of
// snapshots. It returns nil, changing nothing, when there is nothing to record: the order is not
// active, its activation names another snapshot, or the same attestation was recorded before. The
// order's row version does not move (D-9: a record that blocks nothing).
func (o *Order) RecordCostRisk(d quote.CostDenial, snapshots []string, by kernel.ActorRef, epoch int64, now time.Time) (*RiskRecord, error) {
	if by.Kind != kernel.ActorSystem {
		return nil, kernel.Reject(kernel.CodePermissionDenied, "/", "Only TRD's consumer of CST's denials records a cost source risk")
	}
	snapshot := o.CostSnapshot()
	if o.Status != Active || snapshot == "" || !slices.Contains(snapshots, snapshot) {
		return nil, nil
	}
	id := RiskCostSourceDenied + ":" + d.AttestationID
	for _, r := range o.Risks {
		if r.ID == id {
			return nil, nil
		}
	}
	denial := d
	denial.SnapshotID = snapshot
	r := &RiskRecord{ID: id, Cause: RiskCostSourceDenied, Code: CodeCostSourceDenied, OrderRevision: o.OrderRevision,
		OrderRowVersion: o.RowVersion, Denial: denial, Registered: quote.Act{By: by, Epoch: epoch, At: kernel.Instant(now)}, Fresh: true}
	o.Risks = append(o.Risks, r)
	return r, nil
}
