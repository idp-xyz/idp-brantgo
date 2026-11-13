package quote

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// A denied cost source (WBS S4-06b; tactical model WF-03 step 4, event storming PL-07, Context Map
// SC-04 "激活先提交则保留当时合法事实，并登记风险暂停限制后续前向动作"; S4 decision D-9; UL TRD-RiskHold).
//
// CST's CostSourceDenied names the snapshots whose evaluation rests on the denied source. TRD's
// Inbox consumer meets each quote root whose active revision's commit evidence names one of them:
//
//   - no contract formed on the revision — no live (valid or disputed) acceptance accepts it and the
//     root's conversion slot is free: a risk hold, cause cost_source_denied, code COST_SOURCE_DENIED,
//     like the hold of a revoked approval (S3-D67) and never lifted. The revision stays active and
//     its history unchanged; its forward use stops (Usable, QuoteRevisionGuard v1, quote_ready);
//     a replacement or a cancellation ends it. The root version moves.
//   - a contract formed: no hold. ADR-BR-004 narrows the re-check by the contract's formation and
//     ADR-BR-013 keeps the contract: the order's approval acknowledges the denial as an upstream
//     risk (COST_SNAPSHOT_INELIGIBLE, TRD-INV-014; baseline 15.3 "准备后出现的上游风险由订单审批逐项确认"),
//     and an active order gets a risk record that blocks nothing (package order, D-9).
//
// The hold is keyed by the cause and CST's attestation, so one denial holds a revision once,
// whichever delivery of its event arrives. Registering it is not a forward write: the root's object
// control does not fence it (a bound task cannot keep a revision's forward use by holding the root).

// CostDenial is a denial of a cost source as TRD's consumer received it: CST's attestation, the
// denied source, the event that carried it and the snapshot of the revision it names.
type CostDenial struct {
	AttestationID string
	SourceID      string
	EventID       string
	SnapshotID    string
}

// CostHoldID is the identifier of the hold attestation registers.
func CostHoldID(attestation string) string { return HoldCostSourceDenied + ":" + attestation }

// Contracted reports whether a contract formed on revision n: a live acceptance accepts it, or the
// root's conversion slot is taken (an order converts it).
func (q *Quote) Contracted(n int64) bool {
	return len(q.Contracts[n]) > 0 || q.Slot != nil
}

// CostSnapshotOf returns the cost snapshot the commit evidence of the root's active revision names
// and that revision, or "" and nil when the root has no active formal revision.
func (q *Quote) CostSnapshotOf() (string, *Revision) {
	active := q.Active()
	if active == nil || active.Formal == nil {
		return "", nil
	}
	return active.Formal.Commit.CostSnapshotID, active
}

// HoldCostSource registers the risk hold of denial d on the root's active revision, by the system
// actor by under authority epoch epoch at now, when that revision's commit evidence names one of
// snapshots and no contract formed on it (Contracted). It returns nil, changing nothing, when there
// is nothing to hold: no active formal revision, another snapshot, a contract formed, or a hold of
// the same attestation already registered. The root version moves with the hold.
func (q *Quote) HoldCostSource(d CostDenial, snapshots []string, by kernel.ActorRef, epoch int64, now time.Time) (*Hold, error) {
	if by.Kind != kernel.ActorSystem {
		return nil, kernel.Reject(kernel.CodePermissionDenied, "/", "Only TRD's consumer of CST's denials registers a cost source hold")
	}
	snapshot, rev := q.CostSnapshotOf()
	if rev == nil || !slices.Contains(snapshots, snapshot) || q.Contracted(rev.Number) {
		return nil, nil
	}
	id := CostHoldID(d.AttestationID)
	for _, h := range q.Holds {
		if h.ID == id {
			return nil, nil
		}
	}
	denial := d
	denial.SnapshotID = snapshot
	q.RootVersion++
	h := &Hold{ID: id, Revision: rev.Number, Cause: HoldCostSourceDenied, Denial: &denial, Code: CodeCostSourceDenied,
		Registered: Act{By: by, Epoch: epoch, At: kernel.Instant(now)}, RootVersion: q.RootVersion, Fresh: true}
	q.Holds = append(q.Holds, h)
	return h, nil
}
