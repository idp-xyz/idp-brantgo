package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/release"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// judgement is what every guard answered in one FUL transaction for a shipment and a release
// scope: the evidence entries and bindings, the order binding the payment condition was judged on,
// STL's payment verdict, the first unmet guard with its refusal, the first unmet guard other than
// the payment condition, and the exception that covered the payment condition.
type judgement struct {
	entries   []shipment.Entry
	bindings  release.Bindings
	order     shipment.OrderBinding
	payment   release.PaymentVerdict
	failure   *kernel.Rejection
	guard     string
	other     *kernel.Rejection
	exception int64
}

// note records the refusal of guard, if any: the first one decides the outcome, the first one
// that is not the payment condition decides an exception. An error that is not a refusal is
// returned.
func (j *judgement) note(guard string, err error) error {
	if err == nil {
		return nil
	}
	rej, ok := kernel.AsRejection(err)
	if !ok {
		return err
	}
	if j.failure == nil {
		j.failure, j.guard = rej, guard
	}
	if guard != shipment.GuardPaymentCondition && j.other == nil {
		j.other = rej
	}
	return nil
}

// upstream turns a refusal an upstream contract's Check gave into FUL's own code at the same path
// (shipment.Upstream: a code FUL does not declare is REFERENCE_INVALID).
func upstream(err error) error {
	rej, ok := kernel.AsRejection(err)
	if !ok || len(rej.Fields) == 0 {
		return err
	}
	return shipment.Upstream(rej.Code, rej.Fields[0].Path, rej.Fields[0].Reason)
}

// licenseLines are the shipment's lines as CNX's ExportLicenseGuard v1 compares them with the
// allocation (cnxapi.LinesHash): the unit is FUL's unit ID.
func licenseLines(s *shipment.Shipment) []cnxapi.ShipmentLine {
	out := make([]cnxapi.ShipmentLine, 0, len(s.Lines))
	for _, l := range s.Lines {
		out = append(out, cnxapi.ShipmentLine{LineID: l.ID, SkuID: l.SkuID, SpecRevision: l.SpecRevision, Quantity: postgres.Text(l.Quantity),
			Unit: l.UnitID})
	}
	return out
}

// judgeRelease judges the release of scope of the shipment r locked: the documents release of a
// confirmed shipment whose confirmation consumed a shipment release by judgeDocuments (owner
// decision 2026-11-12, S5 C1b), every other release by judge.
func (s Service) judgeRelease(ctx context.Context, exec bentopg.Executor, r *releaseRootsOf, releaseScope string, now time.Time) (*judgement, error) {
	if releaseScope == release.ScopeDocuments && r.existing.Status == shipment.StatusConfirmed {
		if goods, entry, ok := r.confirmation.Confirmation(); ok {
			return s.judgeDocuments(ctx, exec, r.existing, goods, entry, r.release, now)
		}
	}
	return s.judge(ctx, exec, r.existing, r.consumed, releaseScope, r.release, now)
}

func complianceVerdict(b shipment.OrderBinding, v cnxapi.TradeComplianceVerdict) release.ComplianceVerdict {
	return release.ComplianceVerdict{Order: b, Status: v.Status, Reason: v.Reason, CheckID: v.CheckID,
		DecisionID: v.DecisionID, LicenseRequired: v.LicenseRequired, RegulationID: v.RegulationID, RegulationRevision: v.RegulationRevision,
		ValidUntil: v.ValidUntil}
}

func paymentVerdict(v stlapi.PaymentVerdict) release.PaymentVerdict {
	return release.PaymentVerdict{Scope: v.Query.Scope, Satisfied: v.Satisfied(), Status: v.Status, Reason: v.Reason, OrderRevision: v.OrderRevision,
		OrderHash: v.OrderHash, Currency: v.Currency, RequiredMinor: v.RequiredMinor, AppliedMinor: v.AppliedMinor, StageID: v.StageID,
		StageTrigger: v.StageTrigger, StageDueMinor: v.StageDueMinor, StageAppliedMinor: v.StageAppliedMinor, ShortfallMinor: v.ShortfallMinor}
}

// judge runs, with FUL's executor in the caller's transaction (which holds the shipment, the
// order's fulfilment root and the release FOR UPDATE), every guard of a shipment for a release
// scope in context code order (ADR-BR-011, FUL-INV-002, D-11), each keeping its rows FOR SHARE until
// the transaction ends: CNX TradeComplianceGuard v1 on the order revision the shipment binds and,
// when its clear conclusion says a licence is required, ExportLicenseGuard v1 on the shipment's
// lines (R56); PTY PartyScreeningGuard v1 on every party (R44); PUR SupplierDeliveryGuard v1 on
// every delivery line (R59, FUL-INV-003); QLT QualityReleaseGuard v1 on every release (R26); STL
// PaymentConditionGuard v1 for the scope (R45), which a live exception of rel may cover; TRD
// OrderGuard v1 (R27); then FUL-INV-001. The quantities are judged on what FUL's confirmed shipments
// consumed besides this one. Every guard runs; the first refusal in this order decides. rel may be
// nil. The documents release of a confirmed shipment is judged by judgeDocuments instead
// (judgeRelease).
func (s Service) judge(ctx context.Context, exec bentopg.Executor, sh *shipment.Shipment, consumed shipment.Consumed, scope string,
	rel *release.Release, now time.Time) (*judgement, error) {
	const at = "/args/shipment_id"
	j := &judgement{entries: []shipment.Entry{{Name: "guards_checked_at", Value: kernel.FormatTimestamp(now)}},
		bindings: release.Bindings{Screenings: []release.ScreeningBinding{}, Deliveries: []release.DeliveryBinding{},
			QualityReleases: []release.QualityBinding{}}, order: sh.Order}
	b := cnxapi.OrderBinding{OrderID: sh.Order.OrderID, OrderRevision: sh.Order.OrderRevision, OrderHash: sh.Order.OrderHash}
	cv, err := s.Upstream.Compliance.GuardTradeCompliance(ctx, exec, b, now)
	if err != nil {
		return nil, err
	}
	j.entries = append(j.entries, shipment.Entry{Name: shipment.GuardTradeCompliance, Value: cv.Evidence()})
	j.bindings.TradeCompliance = &release.ComplianceBinding{Status: cv.Status, CheckID: cv.CheckID, DecisionID: cv.DecisionID,
		LicenseRequired: cv.LicenseRequired, RegulationID: cv.RegulationID, RegulationRevision: cv.RegulationRevision, ValidUntil: cv.ValidUntil}
	clear := cv.Check(at)
	if err = j.note(shipment.GuardTradeCompliance, upstream(clear)); err != nil {
		return nil, err
	}
	if clear == nil && cv.LicenseRequired {
		lv, err := s.Upstream.Compliance.GuardExportLicense(ctx, exec, sh.ID, now)
		if err != nil {
			return nil, err
		}
		j.entries = append(j.entries, shipment.Entry{Name: shipment.GuardExportLicense, Value: lv.Evidence()})
		j.bindings.ExportLicense = &release.LicenseBinding{Status: lv.Status, LicenseID: lv.LicenseID, AllocationID: lv.AllocationID,
			LinesHash: lv.LinesHash, Quantity: lv.Quantity, Unit: lv.Unit}
		if err = j.note(shipment.GuardExportLicense, upstream(lv.Check(at, licenseLines(sh)))); err != nil {
			return nil, err
		}
	}
	screened, err := s.screen(ctx, exec, j, sh, now)
	if err != nil {
		return nil, err
	}
	for _, p := range screened {
		j.bindings.Screenings = append(j.bindings.Screenings, release.ScreeningBinding{PartyID: p.PartyID, Status: p.Status, ScreeningID: p.ScreeningID,
			PartyRevision: p.PartyRevision})
	}
	before := sh.Before(consumed)
	for _, u := range sh.Deliveries() {
		l, err := s.Upstream.Deliveries.GuardDeliveryLine(ctx, exec, api.DeliveryLineRef{DeliveryID: u.Ref.DeliveryID, LineID: u.Ref.LineID,
			Version: u.Version})
		if err != nil {
			return nil, err
		}
		e, refused := sh.CheckDelivery(u, deliveryVerdict(l), before.Deliveries[u.Ref])
		j.entries = append(j.entries, e)
		j.bindings.Deliveries = append(j.bindings.Deliveries, release.DeliveryBinding{DeliveryID: u.Ref.DeliveryID, LineID: u.Ref.LineID,
			Version: u.Version, Status: l.Status, VerifiedVersion: l.VerifiedVersion, Verified: l.VerifiedQuantity})
		if err = j.note(shipment.GuardSupplierDelivery, refused); err != nil {
			return nil, err
		}
	}
	for _, u := range sh.Releases() {
		v, err := s.Upstream.Releases.GuardRelease(ctx, exec, releaseRef(u.ID), now)
		if err != nil {
			return nil, err
		}
		e, refused := sh.CheckRelease(u, releaseVerdict(v), before.Releases[u.ID])
		j.entries = append(j.entries, e)
		j.bindings.QualityReleases = append(j.bindings.QualityReleases, release.QualityBinding{ReleaseID: u.ID, Status: v.Status,
			Released: v.Quantity, ValidUntil: v.ValidUntil})
		if err = j.note(shipment.GuardQualityRelease, refused); err != nil {
			return nil, err
		}
	}
	if err = s.judgePayment(ctx, exec, j, scope, rel, now); err != nil {
		return nil, err
	}
	ov, err := s.Upstream.Orders.GuardOrder(ctx, exec, binding(sh.Order))
	if err != nil {
		return nil, err
	}
	e, refused := shipment.CheckOrder(sh.Order, orderVerdict(ov), at)
	j.entries = append(j.entries, e)
	j.bindings.Order = &release.OrderBindingSeen{OrderID: sh.Order.OrderID, OrderRevision: ov.OrderRevision, OrderHash: sh.Order.OrderHash,
		Status: ov.Status}
	if err = j.note(shipment.GuardOrder, refused); err != nil {
		return nil, err
	}
	if err = j.note(release.GuardFulfilment, sh.CheckOrderLines(before.OrderLines)); err != nil {
		return nil, err
	}
	return j, nil
}

// screen asks PTY's PartyScreeningGuard v1 (R44) with FUL's executor on every party of sh, in party
// ID order, each keeping the party's screening FOR SHARE until the transaction ends; it records
// every answer in j's evidence, notes the first party not screened clear (PARTY_SCREENING_NOT_CLEAR,
// shipment.CheckScreening) and returns the answers.
func (s Service) screen(ctx context.Context, exec bentopg.Executor, j *judgement, sh *shipment.Shipment, now time.Time) ([]release.RecheckedScreening,
	error) {
	out := []release.RecheckedScreening{}
	for _, id := range sh.ScreenedParties() {
		g, err := s.Upstream.Screening.GuardScreening(ctx, exec, id, now)
		if err != nil {
			return nil, err
		}
		e, refused := sh.CheckScreening(id, shipment.ScreeningVerdict{Clear: g.Clear, Status: g.Status, Reason: g.Reason, ScreeningID: g.ScreeningID,
			PartyRevision: g.PartyRevision})
		j.entries = append(j.entries, e)
		out = append(out, release.RecheckedScreening{PartyID: id, Status: g.Status, Reason: g.Reason, ScreeningID: g.ScreeningID,
			PartyRevision: g.PartyRevision})
		if err = j.note(shipment.GuardPartyScreening, refused); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// judgePayment asks STL's PaymentConditionGuard v1 (R45) for scope on the order binding j judges
// (j.order), with FUL's executor, and records its answer: satisfied, covered by a live exception of
// rel, or COMMERCIAL_RELEASE_BLOCKED.
func (s Service) judgePayment(ctx context.Context, exec bentopg.Executor, j *judgement, scope string, rel *release.Release, now time.Time) error {
	q := stlapi.PaymentQuery{OrderID: j.order.OrderID, OrderRevision: j.order.OrderRevision, Scope: scope, At: now}
	pv, err := s.Upstream.Payments.GuardPaymentCondition(ctx, exec, q)
	if err != nil {
		return err
	}
	j.payment = paymentVerdict(pv)
	e, exception, refused := rel.CheckPayment(j.payment, now, "/args/shipment_id")
	j.entries, j.exception = append(j.entries, e), exception
	p := j.payment
	j.bindings.Payment = &release.PaymentBinding{Scope: scope, Status: p.Status, Reason: p.Reason, OrderRevision: p.OrderRevision,
		OrderHash: p.OrderHash, Currency: p.Currency, RequiredMinor: p.RequiredMinor, AppliedMinor: p.AppliedMinor, StageID: p.StageID,
		ShortfallMinor: p.ShortfallMinor, ExceptionSeq: exception}
	return j.note(shipment.GuardPaymentCondition, refused)
}

// judgeDocuments judges, with FUL's executor in the caller's transaction (which holds the shipment,
// the order's fulfilment root, the documents release and — for document_release.record — the
// document set), the documents release of confirmed shipment sh by the owner's rules of 2026-11-12
// (S5 C1b, release/confirmed.go; S5 C1c, release/blocks.go): goods and entry are what the shipment's
// confirmation bound (release.Confirmation) — the export licence, supplier deliveries, quality
// releases and FUL-INV-001 are not asked again —; TRD's order state read, which takes no lock, gives
// the order's current revision and hash (R27); then, each keeping its rows FOR SHARE until the
// transaction ends, in context code order: CNX's TradeComplianceGuard v1 on the revision the goods
// left under and, when later, on the current revision, refusing only a blocked conclusion (R56,
// CNX-INV-001); PTY's PartyScreeningGuard v1 on every party, refusing anything but clear (R44); STL's
// PaymentConditionGuard v1 for the documents scope on the current revision, which a live exception
// of rel may cover (R45, D-5); TRD's OrderGuard v1 on the current binding (R27). rel may be nil.
func (s Service) judgeDocuments(ctx context.Context, exec bentopg.Executor, sh *shipment.Shipment, goods release.Bindings, entry shipment.Entry,
	rel *release.Release, now time.Time) (*judgement, error) {
	const at = "/args/shipment_id"
	j := &judgement{entries: []shipment.Entry{{Name: "guards_checked_at", Value: kernel.FormatTimestamp(now)}, entry}, bindings: goods}
	st, err := s.Upstream.Orders.OrderState(ctx, exec, sh.Order.OrderID)
	if err != nil {
		return nil, err
	}
	j.order = release.Current(sh.Order, st.Known, st.OrderRevision, st.OrderHash)
	again := &release.RecheckBinding{TradeCompliance: []release.RecheckedCompliance{}}
	revisions := []shipment.OrderBinding{sh.Order}
	if j.order.OrderRevision > sh.Order.OrderRevision {
		revisions = append(revisions, j.order)
	}
	for i, b := range revisions {
		cv, err := s.Upstream.Compliance.GuardTradeCompliance(ctx, exec, cnxapi.OrderBinding{OrderID: b.OrderID, OrderRevision: b.OrderRevision,
			OrderHash: b.OrderHash}, now)
		if err != nil {
			return nil, err
		}
		e, seen, refused := release.CheckComplianceBlock(complianceVerdict(b, cv), i > 0, at)
		j.entries, again.TradeCompliance = append(j.entries, e), append(again.TradeCompliance, seen)
		if err = j.note(shipment.GuardTradeCompliance, refused); err != nil {
			return nil, err
		}
	}
	if again.Screenings, err = s.screen(ctx, exec, j, sh, now); err != nil {
		return nil, err
	}
	j.bindings.Rechecked = again
	if err = s.judgePayment(ctx, exec, j, release.ScopeDocuments, rel, now); err != nil {
		return nil, err
	}
	ov, err := s.Upstream.Orders.GuardOrder(ctx, exec, binding(j.order))
	if err != nil {
		return nil, err
	}
	e, refused := release.CheckDocumentsOrder(j.order, orderVerdict(ov), at)
	j.entries = append(j.entries, e)
	j.bindings.Order = &release.OrderBindingSeen{OrderID: j.order.OrderID, OrderRevision: j.order.OrderRevision, OrderHash: j.order.OrderHash,
		Status: ov.Status}
	if err = j.note(shipment.GuardOrder, refused); err != nil {
		return nil, err
	}
	return j, nil
}

// releaseRoots locks, after shipment sh (held FOR UPDATE by the caller), the order's fulfilment root
// FOR UPDATE, reads what the order's confirmed shipments consumed, and locks the shipment's release
// of releaseScope FOR UPDATE (nil when it has none): FUL's own roots before the authority guard.
func releaseRoots(ctx context.Context, exec bentopg.Executor, sh *shipment.Shipment, releaseScope string) (shipment.Consumed, *release.Release, error) {
	sc := scope(ctx)
	if err := postgres.LockFulfilment(ctx, exec, sc, sh.Order.OrderID); err != nil {
		return shipment.Consumed{}, nil, err
	}
	var refs []shipment.DeliveryRef
	var ids []string
	for _, u := range sh.Deliveries() {
		refs = append(refs, u.Ref)
	}
	for _, u := range sh.Releases() {
		ids = append(ids, u.ID)
	}
	consumed, err := postgres.LoadConsumed(ctx, exec, sc, sh.Order.OrderID, refs, ids)
	if err != nil {
		return consumed, nil, err
	}
	rel, err := postgres.LoadRelease(ctx, exec, sc, sh.ID, releaseScope, postgres.ForUpdate)
	return consumed, rel, err
}
