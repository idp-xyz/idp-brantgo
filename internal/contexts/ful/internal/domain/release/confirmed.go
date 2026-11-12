package release

import (
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The documents release of a confirmed shipment (owner decision 2026-11-12, S5 unit C1b, a
// follow-up of WBS S5-07; FUL-INV-002, SM-FUL-RELEASE, WF-07, D-5, D-11, R27, R45). The goods have
// left under the clearance the shipment's confirmation judged, so commercial_release.evaluate,
// commercial_release.grant_exception and document_release.record of such a release judge
//
//   - money on the order's current revision: STL's PaymentConditionGuard v1 for the documents scope
//     on the revision and hash TRD shows now, because what the customer owes now is that
//     revision's;
//   - goods as confirmed: trade compliance, the export licence, party screening, supplier
//     deliveries, quality releases and FUL-INV-001 are not asked again; the release reuses what the
//     consumed shipment release of the shipment bound (Confirmation) and names it;
//   - the order on that current binding with TRD's OrderGuard v1: active, or ended by order.close or
//     order.close_remainder (status closed) — the documents of what shipped are still released —,
//     but not cancelled (an order with a confirmed shipment cannot be cancelled since S5 C4a:
//     order.cancel answers ORDER_HAS_SHIPMENTS, and a draft has no shipment) and not with its
//     activating approval revoked.
//
// The shipment scope and a prepared shipment's releases keep judging every guard on the revision the
// shipment binds.

// Order states TRD reports (SM-TRD-ORDER) that the documents release of a confirmed shipment tells
// apart.
const (
	OrderActive    = "active"
	OrderClosed    = "closed"
	OrderCancelled = "cancelled"
)

// GuardConfirmation names, among the guard evidence of a documents release of a confirmed shipment,
// the shipment confirmation whose verdicts it reuses.
const GuardConfirmation = "ful.shipment_confirmation"

// ConfirmationBinding is the shipment confirmation a documents release of a confirmed shipment
// reuses: the consumed shipment release, its version, the evaluation whose bindings the goods'
// guards repeat, the SHA-256 of the confirming transaction's guard evidence, when it was consumed,
// the order revision the goods left under and the guards reused.
type ConfirmationBinding struct {
	ReleaseID      string    `json:"release_id"`
	ReleaseVersion int64     `json:"release_version"`
	EvaluationSeq  int64     `json:"evaluation_seq"`
	GuardsSHA256   string    `json:"guards_sha256"`
	ConfirmedAt    time.Time `json:"confirmed_at"`
	OrderRevision  int64     `json:"order_revision"`
	Reused         []string  `json:"reused"`
}

// Confirmation returns what the shipment release r, consumed by shipment.confirm, lets a documents
// release of the same shipment reuse: the bindings of the goods' guards of its latest evaluation
// (the one in force when the confirmation consumed it) with the binding that names the
// confirmation, and the evidence entry pinning the confirming transaction's sealed guard evidence.
// ok is false when r is not such a release (none, not consumed, never evaluated).
func (r *Release) Confirmation() (Bindings, Entry, bool) {
	if r == nil || r.Scope != ScopeShipment || r.Status != StatusConsumed || r.Consumption == nil || len(r.Evaluations) == 0 {
		return Bindings{}, Entry{}, false
	}
	e := r.Latest()
	src := e.Bindings
	out := Bindings{TradeCompliance: src.TradeCompliance, ExportLicense: src.ExportLicense, Screenings: src.Screenings,
		Deliveries: src.Deliveries, QualityReleases: src.QualityReleases}
	reused := []string{shipment.GuardTradeCompliance}
	if src.ExportLicense != nil {
		reused = append(reused, shipment.GuardExportLicense)
	}
	if out.Screenings == nil {
		out.Screenings = []ScreeningBinding{}
	} else {
		reused = append(reused, shipment.GuardPartyScreening)
	}
	if out.Deliveries == nil {
		out.Deliveries = []DeliveryBinding{}
	} else {
		reused = append(reused, shipment.GuardSupplierDelivery)
	}
	if out.QualityReleases == nil {
		out.QualityReleases = []QualityBinding{}
	} else {
		reused = append(reused, shipment.GuardQualityRelease)
	}
	reused = append(reused, GuardFulfilment)
	c := r.Consumption
	out.Confirmation = &ConfirmationBinding{ReleaseID: r.ID, ReleaseVersion: r.RowVersion, EvaluationSeq: e.Seq, GuardsSHA256: c.EntriesSHA256,
		ConfirmedAt: c.Consumed.At, OrderRevision: r.Order.OrderRevision, Reused: reused}
	entry := Entry{Name: GuardConfirmation, Value: r.ID + "/v" + strconv.FormatInt(r.RowVersion, 10) + "/" + r.Status + "/" + c.EntriesSHA256 +
		"/r" + strconv.FormatInt(r.Order.OrderRevision, 10)}
	return out, entry, true
}

// Current is the order binding the documents release of a confirmed shipment bound to bound judges:
// the revision and hash TRD's order state shows now (known, revision, hash), or bound itself when
// TRD shows the order at no later revision.
func Current(bound shipment.OrderBinding, known bool, revision int64, hash string) shipment.OrderBinding {
	if !known || hash == "" || revision < bound.OrderRevision || (revision == bound.OrderRevision && hash != bound.OrderHash) {
		return bound
	}
	return shipment.OrderBinding{OrderID: bound.OrderID, OrderRevision: revision, OrderHash: hash}
}

// CheckDocumentsOrder judges at path TRD's OrderGuard v1 verdict v on judged, the order's current
// binding, for the documents release of a confirmed shipment: an unknown order is
// REFERENCE_INVALID; REFERENCE_STALE says TRD moved on after FUL read its state (evaluate again); a
// revoked activating approval is ORDER_APPROVAL_REVOKED; an active order, or one ended by order.close
// or order.close_remainder, serves; any other state (cancelled) is ORDER_NOT_ACTIVE.
func CheckDocumentsOrder(judged shipment.OrderBinding, v shipment.OrderVerdict, path string) (Entry, error) {
	e := Entry{Name: shipment.GuardOrder, Value: v.Status + "/r" + strconv.FormatInt(v.OrderRevision, 10)}
	switch {
	case !v.Known:
		return e, Reject(kernel.CodeReferenceInvalid, path, "TRD has no such order")
	case v.Code == shipment.CodeReferenceStale:
		return e, Reject(shipment.CodeReferenceStale, path, "The order moved to revision "+strconv.FormatInt(v.OrderRevision, 10)+
			" while its revision "+strconv.FormatInt(judged.OrderRevision, 10)+" was judged: evaluate the documents release again")
	case v.ApprovalRevoked || v.Code == shipment.CodeOrderApprovalRevoked:
		return e, Reject(shipment.CodeOrderApprovalRevoked, path, "The order's activating approval was revoked")
	case v.Code == "" && v.Status == OrderActive:
		return e, nil
	case v.Status == OrderClosed && v.Code == shipment.CodeOrderNotActive:
		// Ended by order.close or order.close_remainder: what shipped is still released.
		return e, nil
	case v.Code == "" || v.Code == shipment.CodeOrderNotActive:
		return e, Reject(shipment.CodeOrderNotActive, path, "The order is "+v.Status+": the documents of its shipments are not released")
	}
	return e, shipment.Upstream(v.Code, path, "The order is "+v.Status)
}
