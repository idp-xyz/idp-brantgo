package application

import (
	"context"
	"fmt"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/domain/inspection"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/domain/release"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// grantCommand runs quality_release.grant. Before the transaction it reads the subject from PUR and
// the evidence from EVD; inside it, it locks the release its ID names, or else every inspection of
// the subject FOR UPDATE, reads the subject's inspections and releases as they stand once it holds
// them, decides (QLT-INV-001: conforming plus approved concessions less what the releases count,
// a revoked release's frozen quantity until it is inspected again; S5R-D18) and writes the release
// with its concession quantity.
type grantCommand struct {
	s           Service
	cmd         api.GrantRelease
	req         release.Request
	subject     *inspection.Subject
	code        string
	sources     map[string]string
	existing    *release.Release
	inspections []*inspection.Inspection
	releases    []*release.Release
	replay      bool
}

func (h *grantCommand) Prepare(ctx context.Context) error {
	var taken bool
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		r, err := postgres.LoadRelease(ctx, exec, scope(ctx), h.req.ID, postgres.NoLock)
		taken = r != nil
		return err
	})
	if err != nil || taken {
		return err
	}
	got, err := h.s.subjects(ctx, []api.SubjectRef{h.cmd.Subject}, "/args/subject")
	if err != nil {
		return err
	}
	if s, ok := got[h.cmd.Subject]; ok {
		if h.subject, err = subjectOf(s); err != nil {
			return err
		}
		h.code = s.Code
	}
	h.sources, err = h.s.sources(ctx, h.req.Evidence)
	return err
}

func (h *grantCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.existing, h.inspections, h.releases, h.replay = nil, nil, nil, false
	sc := scope(ctx)
	r, err := postgres.LoadRelease(ctx, exec, sc, h.req.ID, postgres.ForUpdate)
	if err != nil || r != nil {
		h.existing = r
		return err
	}
	if err = postgres.LockSubject(ctx, exec, sc, h.req.Subject); err != nil {
		return err
	}
	if h.inspections, err = postgres.LoadSubjectInspections(ctx, exec, sc, h.req.Subject); err != nil {
		return err
	}
	h.releases, err = postgres.LoadSubjectReleases(ctx, exec, sc, h.req.Subject)
	return err
}

func (h *grantCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if r := h.existing; r != nil {
		if !d.Sees(r.OwnerActor, r.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/release_id", "")
		}
		if err := r.Answer(h.req.RequestHash, d.Actor); err != nil {
			return action.Outcome{}, err
		}
		h.replay = true
		return action.Outcome{ObjectID: r.ID, RowVersion: r.RowVersion, Status: r.Status}, nil
	}
	r, err := release.Grant(h.req, release.Inputs{Subject: h.subject, SubjectCode: h.code, Inspections: h.inspections, Releases: h.releases,
		Sources: h.sources, Granter: d.Actor, Epoch: d.Epoch, Team: d.Team, Now: h.s.Clock.Now()})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertRelease(ctx, exec, scope(ctx), r); err != nil {
		return action.Outcome{}, err
	}
	s := r.Subject
	event := action.Event{Type: api.EventQualityReleased, AggregateType: "QualityRelease", AggregateID: r.ID, AggregateVersion: r.RowVersion,
		Fields: map[string]any{"release_id": r.ID, "subject_kind": s.Ref.Kind, "subject_id": s.Ref.ID, "subject_line_id": s.Ref.LineID,
			"subject_revision": s.Revision, "supplier_id": s.SupplierID, "order_id": s.OrderID, "order_line_id": s.OrderLineID, "sku_id": s.SkuID,
			"spec_revision": s.SpecRevision, "unit_id": s.UnitID, "unit_revision": s.UnitRevision, "released_quantity": postgres.Text(r.Quantity),
			"valid_from": kernel.FormatTimestamp(r.ValidFrom), "valid_until": kernel.FormatTimestamp(r.ValidUntil), "inspection_ids": r.InspectionIDs,
			"concession": r.Concession(), "concession_quantity": postgres.Text(r.ConcessionQuantity)}}
	return action.Outcome{ObjectID: r.ID, RowVersion: r.RowVersion, Status: r.Status, Events: []action.Event{event}}, nil
}

// GrantRelease grants a quality release (quality_release.grant; SM-QLT-RELEASE, QLT-INV-001, F-06).
func (s Service) GrantRelease(ctx context.Context, cmd api.GrantRelease) (api.ReleaseReceipt, error) {
	h := &grantCommand{s: s, cmd: cmd}
	err := cmd.Validate()
	if err == nil {
		h.req = release.Request{ID: cmd.ReleaseID, Subject: subjectRef(cmd.Subject), Evidence: slices.Clone(cmd.EvidenceIDs),
			RequestHash: requestHash(cmd)}
		h.req.Quantity, _ = api.Quantity(cmd.Quantity)
		h.req.ValidUntil, _ = kernel.ParseTimestamp(cmd.ValidUntil)
		if cmd.ValidFrom != "" {
			h.req.ValidFrom, _ = kernel.ParseTimestamp(cmd.ValidFrom)
		}
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandGrantRelease, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ReleaseID}, cmd, err, h)
	if err != nil {
		return api.ReleaseReceipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return s.releaseReceipt(ctx, res)
}

// revokeCommand runs quality_release.revoke: it locks the release row FOR UPDATE — the row
// QualityReleaseGuard v1 holds FOR SHARE in FUL's transactions — then the subject's inspections FOR
// UPDATE (the frozen quantity it records changes what the subject may release once it is inspected
// again), reads what FUL shipped on the release (ReleaseConsumption v1, R79, no lock) and revokes it,
// recording the shipped and the frozen quantity (QLT-INV-002, S5R-D18). A release revoked before is
// answered unchanged, without an event, and FUL is not asked (BT21).
type revokeCommand struct {
	s        Service
	cmd      api.RevokeRelease
	sources  map[string]string
	existing *release.Release
	shipped  kernel.Decimal
	replay   bool
}

func (h *revokeCommand) Prepare(ctx context.Context) error {
	var err error
	h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *revokeCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.replay, h.shipped = false, kernel.Decimal{}
	sc := scope(ctx)
	h.existing, err = postgres.LoadRelease(ctx, exec, sc, h.cmd.ReleaseID, postgres.ForUpdate)
	r := h.existing
	if err != nil || r == nil || r.Status == release.StatusRevoked {
		return err
	}
	if err = postgres.LockSubject(ctx, exec, sc, r.Subject.Ref); err != nil {
		return err
	}
	// Read after the release lock: a confirmation holding the guard committed before it was granted.
	got, err := h.s.Upstream.Consumption(ctx, exec, r.ID)
	if err != nil {
		return err
	}
	h.shipped, err = shippedOn(r, got)
	return err
}

// shippedOn sums FUL's answers for release r. FUL keeps every shipment line of a release in the
// release's unit and never ships more than it releases; an answer in another unit is an error.
func shippedOn(r *release.Release, got []Consumed) (kernel.Decimal, error) {
	var sum kernel.Decimal
	for _, c := range got {
		q, ok := api.Quantity(c.Quantity)
		if !ok || c.UnitID != r.Subject.UnitID || c.UnitRevision != r.Subject.UnitRevision {
			return kernel.Decimal{}, fmt.Errorf("qlt: FUL's ReleaseConsumption v1 answers %q %s r%d for release %s in %s r%d", c.Quantity, c.UnitID,
				c.UnitRevision, r.ID, r.Subject.UnitID, r.Subject.UnitRevision)
		}
		sum = sum.Add(q)
	}
	return sum, nil
}

func (h *revokeCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	r := h.existing
	if r == nil || !d.Sees(r.OwnerActor, r.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/release_id", "")
	}
	from := r.RowVersion
	changed, err := r.Revoke(h.cmd.Reason, h.cmd.EvidenceIDs, h.sources, h.shipped, inspection.Act{By: d.Actor, Epoch: d.Epoch, At: h.s.Clock.Now()})
	if err != nil {
		return action.Outcome{}, err
	}
	if !changed {
		h.replay = true
		return action.Outcome{ObjectID: r.ID, RowVersion: r.RowVersion, Status: r.Status}, nil
	}
	if err = postgres.RevokeRelease(ctx, exec, scope(ctx), r, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	s := r.Subject
	event := action.Event{Type: api.EventQualityReleaseRevoked, AggregateType: "QualityRelease", AggregateID: r.ID, AggregateVersion: r.RowVersion,
		Fields: map[string]any{"release_id": r.ID, "subject_kind": s.Ref.Kind, "subject_id": s.Ref.ID, "subject_line_id": s.Ref.LineID,
			"order_id": s.OrderID, "order_line_id": s.OrderLineID, "released_quantity": postgres.Text(r.Quantity), "unit_id": s.UnitID,
			"revoked_at": kernel.FormatTimestamp(r.Revocation.Revoked.At), "shipped_quantity": postgres.Text(*r.Revocation.Shipped),
			"frozen_quantity": postgres.Text(r.Frozen())}}
	return action.Outcome{ObjectID: r.ID, RowVersion: r.RowVersion, Status: r.Status, Events: []action.Event{event}}, nil
}

// RevokeRelease revokes a quality release (quality_release.revoke; SM-QLT-RELEASE, QLT-INV-002,
// BT21, S5R-D18).
func (s Service) RevokeRelease(ctx context.Context, cmd api.RevokeRelease) (api.ReleaseReceipt, error) {
	h := &revokeCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandRevokeRelease, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ReleaseID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.ReleaseReceipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return s.releaseReceipt(ctx, res)
}

// releaseReceipt completes res with what the stored release binds.
func (s Service) releaseReceipt(ctx context.Context, res api.Receipt) (api.ReleaseReceipt, error) {
	out := api.ReleaseReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		r, err := postgres.LoadRelease(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || r == nil {
			return err
		}
		out.Subject, out.Quantity, out.UnitID = apiRef(r.Subject.Ref), postgres.Text(r.Quantity), r.Subject.UnitID
		out.ValidFrom, out.ValidUntil = r.ValidFrom, r.ValidUntil
		out.Concession, out.ConcessionQuantity = r.Concession(), postgres.Text(r.ConcessionQuantity)
		if v := r.Revocation; v != nil && v.Shipped != nil {
			out.ShippedQuantity, out.FrozenQuantity = postgres.Text(*v.Shipped), postgres.Text(r.Frozen())
		}
		return nil
	})
	return out, err
}
