package application

import (
	"context"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/release"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// releaseRootsOf is the Lock of every release command: the shipment FOR UPDATE, then — for a
// shipment that is not cancelled — the order's fulfilment root FOR UPDATE, what the order's
// confirmed shipments consumed, and the shipment's release of scope FOR UPDATE.
type releaseRootsOf struct {
	existing *shipment.Shipment
	consumed shipment.Consumed
	release  *release.Release
}

func (r *releaseRootsOf) lock(ctx context.Context, exec bentopg.Executor, shipmentID, releaseScope string) error {
	r.existing, r.consumed, r.release = nil, shipment.Consumed{}, nil
	s, err := postgres.LoadShipment(ctx, exec, scope(ctx), shipmentID, postgres.ForUpdate)
	r.existing = s
	if err != nil || s == nil || s.Status == shipment.StatusCancelled {
		return err
	}
	r.consumed, r.release, err = releaseRoots(ctx, exec, s, releaseScope)
	return err
}

// visible refuses a shipment the actor does not see (NOT_FOUND).
func (r *releaseRootsOf) visible(d authorization.Decision) error {
	if s := r.existing; s == nil || !d.Sees(s.OwnerActor, s.OwnerTeam) {
		return kernel.Reject(kernel.CodeNotFound, "/args/shipment_id", "")
	}
	return nil
}

func grantedEvent(r *release.Release, evaluation, exception int64, sha string, at string) action.Event {
	return action.Event{Type: api.EventCommercialReleaseGranted, AggregateType: "CommercialRelease", AggregateID: r.ID, AggregateVersion: r.RowVersion,
		Fields: map[string]any{"release_id": r.ID, "shipment_id": r.ShipmentID, "scope": r.Scope, "order_id": r.Order.OrderID,
			"order_revision": r.Order.OrderRevision, "order_hash": r.Order.OrderHash, "evaluation_seq": evaluation, "exception_seq": exception,
			"guards_sha256": sha, "granted_at": at}}
}

// evaluateCommand runs commercial_release.evaluate (human, AI or system): it locks the shipment, the
// order's fulfilment root and the release, runs every guard (judge) and records the evaluation —
// granted, or blocked with the first unmet guard — creating the release with its first evaluation.
type evaluateCommand struct {
	s   Service
	cmd api.EvaluateRelease
	releaseRootsOf
}

func (h *evaluateCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	return h.lock(ctx, exec, h.cmd.ShipmentID, h.cmd.Scope)
}

func (h *evaluateCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := h.visible(d); err != nil {
		return action.Outcome{}, err
	}
	s, rel := h.existing, h.release
	switch {
	case s.Status == shipment.StatusCancelled:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/shipment_id", "The shipment is cancelled")
	case h.cmd.Scope == release.ScopeShipment && s.Status == shipment.StatusConfirmed:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/scope", "The shipment is confirmed: its shipment release is consumed")
	}
	if rel != nil {
		if err := rel.Evaluable(); err != nil {
			return action.Outcome{}, err
		}
	}
	now := kernel.Instant(h.s.Clock.Now())
	j, err := h.s.judge(ctx, exec, s, h.consumed, h.cmd.Scope, rel, now)
	if err != nil {
		return action.Outcome{}, err
	}
	act := shipment.Act{By: d.Actor, Epoch: d.Epoch, At: now}
	fresh := rel == nil
	if fresh {
		rel = release.New(action.NewID("rls", now), s.ID, h.cmd.Scope, s.Order, act)
	}
	from := rel.RowVersion
	granted := rel.Evaluate(release.Verdict{Entries: j.entries, Bindings: j.bindings, Failure: j.failure, Guard: j.guard, ExceptionSeq: j.exception}, act)
	if fresh {
		err = postgres.InsertRelease(ctx, exec, scope(ctx), rel)
	} else {
		err = postgres.SaveEvaluation(ctx, exec, scope(ctx), rel, from)
	}
	if err != nil {
		return action.Outcome{}, conflict(err)
	}
	out := action.Outcome{ObjectID: rel.ID, RowVersion: rel.RowVersion, Status: rel.Status}
	if granted {
		e := rel.Latest()
		out.Events = []action.Event{grantedEvent(rel, e.Seq, e.ExceptionSeq, e.EntriesSHA256, kernel.FormatTimestamp(now))}
	}
	return out, nil
}

// EvaluateRelease evaluates the release of a shipment for a scope (commercial_release.evaluate;
// SM-FUL-RELEASE, FUL-INV-002, D-11, WF-07).
func (s Service) EvaluateRelease(ctx context.Context, cmd api.EvaluateRelease) (api.ReleaseReceipt, error) {
	h := &evaluateCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandEvaluateRelease, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ShipmentID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.ReleaseReceipt{}, err
	}
	return s.releaseReceipt(ctx, res)
}

// exceptionCommand runs commercial_release.grant_exception (human only): with the same locks as an
// evaluation it runs every guard and grants the blocked release by an exception when only the
// payment condition is unmet, for one unpaid stage (release.GrantException).
type exceptionCommand struct {
	s       Service
	cmd     api.GrantReleaseException
	sources map[string]bool
	releaseRootsOf
}

func (h *exceptionCommand) Prepare(ctx context.Context) error {
	var err error
	h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *exceptionCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	return h.lock(ctx, exec, h.cmd.ShipmentID, h.cmd.Scope)
}

func (h *exceptionCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := h.visible(d); err != nil {
		return action.Outcome{}, err
	}
	s, rel := h.existing, h.release
	if rel == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/scope", "The shipment has no "+h.cmd.Scope+" release: evaluate it")
	}
	if err := rel.Exceptable(h.cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	if err := shipment.Readable(h.cmd.EvidenceIDs, h.sources, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	j, err := h.s.judge(ctx, exec, s, h.consumed, h.cmd.Scope, rel, now)
	if err != nil {
		return action.Outcome{}, err
	}
	until, _ := kernel.ParseTimestamp(h.cmd.ValidUntil)
	from := rel.RowVersion
	x, err := rel.GrantException(release.ExceptionRequest{ExpectedVersion: h.cmd.ExpectedVersion, StageID: h.cmd.StageID, Currency: h.cmd.Currency,
		ShortfallMinor: h.cmd.ShortfallMinor, ValidUntil: until, Reason: h.cmd.Reason, Evidence: slices.Clone(h.cmd.EvidenceIDs)}, j.other, j.payment,
		j.entries, shipment.Act{By: d.Actor, Epoch: d.Epoch, At: now})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveException(ctx, exec, scope(ctx), rel, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	return action.Outcome{ObjectID: rel.ID, RowVersion: rel.RowVersion, Status: rel.Status,
		Events: []action.Event{grantedEvent(rel, 0, x.Seq, x.EntriesSHA256, kernel.FormatTimestamp(now))}}, nil
}

// GrantReleaseException grants a blocked release by a payment-condition exception
// (commercial_release.grant_exception, human only; SM-FUL-RELEASE blocked → granted, FUL-INV-002,
// UL-INV-28, BT13).
func (s Service) GrantReleaseException(ctx context.Context, cmd api.GrantReleaseException) (api.ReleaseReceipt, error) {
	h := &exceptionCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandGrantReleaseException, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ShipmentID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.ReleaseReceipt{}, err
	}
	return s.releaseReceipt(ctx, res)
}

// documentReleaseCommand runs document_release.record (human only): with the same locks as an
// evaluation (scope documents) it needs a confirmed shipment, re-runs every guard (D-11), needs the
// documents release granted and consumes it with how the documents were released.
type documentReleaseCommand struct {
	s       Service
	cmd     api.RecordDocumentRelease
	sources map[string]bool
	releaseRootsOf
}

func (h *documentReleaseCommand) Prepare(ctx context.Context) error {
	var err error
	h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *documentReleaseCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	return h.lock(ctx, exec, h.cmd.ShipmentID, release.ScopeDocuments)
}

func (h *documentReleaseCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := h.visible(d); err != nil {
		return action.Outcome{}, err
	}
	s, rel := h.existing, h.release
	if s.Status != shipment.StatusConfirmed {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/shipment_id",
			"The shipment is "+s.Status+": its documents are released after its confirmation")
	}
	if rel != nil && rel.RowVersion != h.cmd.ExpectedVersion {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if err := shipment.Readable(h.cmd.EvidenceIDs, h.sources, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	j, err := h.s.judge(ctx, exec, s, h.consumed, release.ScopeDocuments, rel, now)
	if err != nil {
		return action.Outcome{}, err
	}
	if j.failure != nil {
		return action.Outcome{}, j.failure
	}
	if err = rel.Consumable(release.ScopeDocuments, "/args/shipment_id"); err != nil {
		return action.Outcome{}, err
	}
	entries := append(j.entries, rel.Entry(j.exception))
	from := rel.RowVersion
	if err = rel.Consume(h.cmd.ReleaseKind, slices.Clone(h.cmd.EvidenceIDs), entries, shipment.Act{By: d.Actor, Epoch: d.Epoch, At: now}); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.ConsumeRelease(ctx, exec, scope(ctx), rel, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	c := rel.Consumption
	event := action.Event{Type: api.EventDocumentsReleased, AggregateType: "CommercialRelease", AggregateID: rel.ID, AggregateVersion: rel.RowVersion,
		Fields: map[string]any{"release_id": rel.ID, "shipment_id": s.ID, "order_id": s.Order.OrderID, "order_revision": s.Order.OrderRevision,
			"order_hash": s.Order.OrderHash, "release_kind": c.ReleaseKind, "evidence_ids": c.Evidence, "guards_sha256": c.EntriesSHA256,
			"released_at": kernel.FormatTimestamp(c.Consumed.At)}}
	return action.Outcome{ObjectID: rel.ID, RowVersion: rel.RowVersion, Status: rel.Status, Events: []action.Event{event}}, nil
}

// RecordDocumentRelease records the release of a confirmed shipment's documents
// (document_release.record, human only; SM-FUL-RELEASE granted → consumed, FUL-INV-002, D-11).
func (s Service) RecordDocumentRelease(ctx context.Context, cmd api.RecordDocumentRelease) (api.ReleaseReceipt, error) {
	h := &documentReleaseCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandRecordDocumentRelease, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ShipmentID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.ReleaseReceipt{}, err
	}
	return s.releaseReceipt(ctx, res)
}

// releaseReceipt completes res with the stored release: its shipment and scope and, when the
// evaluation that gave it res's version blocked it, the first unmet guard, its code and why.
func (s Service) releaseReceipt(ctx context.Context, res api.Receipt) (api.ReleaseReceipt, error) {
	out := api.ReleaseReceipt{Receipt: res, ReleaseID: res.ObjectID}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		r, err := postgres.LoadReleaseByID(ctx, exec, scope(ctx), res.ObjectID)
		if err != nil || r == nil {
			return err
		}
		out.ShipmentID, out.Scope = r.ShipmentID, r.Scope
		for _, e := range r.Evaluations {
			if e.ReleaseVersion == res.RowVersion && e.Outcome == release.OutcomeBlocked {
				out.Guard, out.Code, out.Reason = e.Guard, e.Code, e.Reason
			}
		}
		return nil
	})
	return out, err
}
