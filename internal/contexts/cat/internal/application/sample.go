package application

import (
	"context"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/sampleapproval"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Audited read of a sample approval.
const CommandGetSample = "sample_approval.get"

// SaveSample registers or replaces a sample-submission draft. The SKU specification must be current
// when the draft is saved. The apparel plugin does not have to be active.
func (s Service) SaveSample(ctx context.Context, cmd api.SaveSample) (api.Receipt, error) {
	var readable map[string]evdapi.SourceRef
	h := &sampleCommand{id: cmd.SampleApprovalID, prepare: func(ctx context.Context) (err error) {
		readable, err = s.readable(ctx, cmd.EvidenceIDs)
		return err
	}}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := sampleapproval.CheckSave(h.sample, cmd.ExpectedVersion, cmd.SkuID, cmd.SampleStage); err != nil {
			return action.Outcome{}, err
		}
		if err := knownIDs(cmd.EvidenceIDs, readable, "/args/evidence_ids/"); err != nil {
			return action.Outcome{}, err
		}
		if err := requireCurrentSKU(ctx, exec, d, cmd.SkuID, cmd.SpecRevision, "/args/sku_id"); err != nil {
			return action.Outcome{}, err
		}
		sc := scope(ctx)
		created := h.sample == nil
		if created {
			taken, err := postgres.SampleKeyTaken(ctx, exec, sc, cmd.SkuID, cmd.SampleStage, cmd.SampleApprovalID)
			if err != nil {
				return action.Outcome{}, err
			}
			if err = sampleapproval.CheckKey(taken); err != nil {
				return action.Outcome{}, err
			}
			h.sample = sampleapproval.New(cmd.SampleApprovalID, cmd.SkuID, cmd.SampleStage, d.Actor.ID, d.Team)
		}
		rev, fresh := h.sample.Save(sampleapproval.DraftContent{SpecRevision: cmd.SpecRevision, TechPackSHA256: cmd.TechPackSHA256,
			EvidenceIDs: cmd.EvidenceIDs}, d.Actor, d.Epoch, s.Clock.Now())
		var err error
		if created {
			err = postgres.InsertSample(ctx, exec, sc, h.sample, s.Clock.Now())
		}
		if err == nil && fresh {
			err = postgres.InsertSampleRevision(ctx, exec, sc, h.sample, rev)
		} else if err == nil {
			err = postgres.UpdateSampleRevision(ctx, exec, sc, h.sample.ID, rev)
		}
		if err == nil && !created {
			err = postgres.UpdateSample(ctx, exec, sc, h.sample, h.from, s.Clock.Now())
		}
		return h.outcome(string(rev.Status)), stored(err)
	}
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	return s.run(ctx, action.Command{Name: api.CommandSaveSample, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.SampleApprovalID},
		args, cmd.Validate(), &preparedSample{h})
}

// ApproveSample binds the draft's specification revision. The approver is not the last editor, the
// evidence is the draft's, and SkuGuard requires that revision to still be current.
func (s Service) ApproveSample(ctx context.Context, cmd api.ApproveSample) (api.Receipt, error) {
	var readable map[string]evdapi.SourceRef
	h := &sampleCommand{id: cmd.SampleApprovalID, prepare: func(ctx context.Context) (err error) {
		readable, err = s.readable(ctx, cmd.EvidenceIDs)
		return err
	}}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		rev, err := h.sample.CheckApprove(cmd.ExpectedVersion, cmd.Revision, d.Actor)
		if err != nil {
			return action.Outcome{}, err
		}
		if !slices.Equal(slices.Sorted(slices.Values(cmd.EvidenceIDs)), rev.EvidenceIDs) {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/evidence_ids", "")
		}
		if err = knownIDs(cmd.EvidenceIDs, readable, "/args/evidence_ids/"); err != nil {
			return action.Outcome{}, err
		}
		if err = requireCurrentSKU(ctx, exec, d, h.sample.SkuID, rev.SpecRevision, "/args/revision"); err != nil {
			return action.Outcome{}, err
		}
		superseded := h.sample.Approve(d.Actor, d.Epoch, s.Clock.Now())
		sc := scope(ctx)
		if superseded != nil {
			err = postgres.SupersedeSampleRevision(ctx, exec, sc, h.sample.ID, superseded)
		}
		if err == nil {
			err = postgres.ApproveSampleRevision(ctx, exec, sc, h.sample.ID, h.sample.Approved)
		}
		if err == nil {
			err = postgres.UpdateSample(ctx, exec, sc, h.sample, h.from, s.Clock.Now())
		}
		if err != nil {
			return action.Outcome{}, stored(err)
		}
		bound := h.sample.Approved
		return h.outcome(string(bound.Status), action.Event{Type: api.EventSampleApproved, AggregateType: "SampleApproval",
			AggregateID: h.sample.ID, AggregateVersion: h.sample.Version, Fields: map[string]any{
				"sample_approval_id": h.sample.ID, "revision": bound.Number, "sku_id": h.sample.SkuID,
				"spec_revision": bound.SpecRevision, "sample_stage": h.sample.Stage, "root_version": h.sample.Version}}), nil
	}
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	return s.run(ctx, action.Command{Name: api.CommandApproveSample, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.SampleApprovalID},
		args, cmd.Validate(), &preparedSample{h})
}

// RevokeSample records the reason and leaves the guard with no current approval.
func (s Service) RevokeSample(ctx context.Context, cmd api.RevokeSample) (api.Receipt, error) {
	h := &sampleCommand{id: cmd.SampleApprovalID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		rev, err := h.sample.CheckRevoke(cmd.ExpectedVersion)
		if err != nil {
			return action.Outcome{}, err
		}
		h.sample.Revoke(d.Actor, d.Epoch, cmd.Reason, s.Clock.Now())
		sc := scope(ctx)
		if err = postgres.RevokeSampleRevision(ctx, exec, sc, h.sample.ID, rev); err != nil {
			return action.Outcome{}, stored(err)
		}
		if err = postgres.UpdateSample(ctx, exec, sc, h.sample, h.from, s.Clock.Now()); err != nil {
			return action.Outcome{}, stored(err)
		}
		return h.outcome(string(sampleapproval.Revoked), action.Event{Type: api.EventSampleRevoked, AggregateType: "SampleApproval",
			AggregateID: h.sample.ID, AggregateVersion: h.sample.Version, Fields: map[string]any{
				"sample_approval_id": h.sample.ID, "revision": rev.Number, "sku_id": h.sample.SkuID,
				"sample_stage": h.sample.Stage, "root_version": h.sample.Version}}), nil
	}
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	return s.run(ctx, action.Command{Name: api.CommandRevokeSample, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.SampleApprovalID,
		Reason: cmd.Reason}, args, cmd.Validate(), h)
}

// GetSampleApproval returns an approval and its revisions, audited.
func (s Service) GetSampleApproval(ctx context.Context, id string) (api.SampleApprovalView, error) {
	var v api.SampleApprovalView
	err := s.Ledger.Read(ctx, CommandGetSample, id, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		a, revs, err := postgres.SampleRecord(ctx, exec, scope(ctx), id)
		if err != nil {
			return err
		}
		if a == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.SampleApprovalView{SampleApprovalID: a.ID, SkuID: a.SkuID, SampleStage: a.Stage, Status: string(a.Status),
			RootVersion: a.Version, CurrentRevision: a.Current}
		for _, rev := range revs {
			v.Revisions = append(v.Revisions, api.SampleRevisionView{Revision: rev.Number, SpecRevision: rev.SpecRevision,
				TechPackSHA256: rev.TechPackSHA256, EvidenceIDs: rev.EvidenceIDs, Status: string(rev.Status), SavedBy: rev.SavedBy,
				ApprovedBy: rev.ApprovedBy, RevokeReason: rev.RevokeReason, RevokedBy: rev.RevokedBy})
		}
		return nil
	})
	return v, err
}

type sampleCommand struct {
	id      string
	sample  *sampleapproval.Approval
	from    int64
	prepare func(context.Context) error
	apply   func(context.Context, bentopg.Executor, authorization.Decision) (action.Outcome, error)
}

func (h *sampleCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.sample, h.from = nil, 0
	var err error
	h.sample, err = postgres.LoadSample(ctx, exec, scope(ctx), h.id, true)
	if h.sample != nil {
		h.from = h.sample.Version
	}
	return err
}

func (h *sampleCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *sampleCommand) outcome(status string, events ...action.Event) action.Outcome {
	return action.Outcome{ObjectID: h.sample.ID, RowVersion: h.sample.Version, Status: status, Events: events}
}

type preparedSample struct{ *sampleCommand }

func (h *preparedSample) Prepare(ctx context.Context) error { return h.prepare(ctx) }
