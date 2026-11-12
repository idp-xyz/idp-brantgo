package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// changeView is a stored change as the reads show it.
func changeView(x *commitment.Change) api.ChangeView {
	v := api.ChangeView{ChangeID: x.ID, CommitmentID: x.CommitmentID, Status: x.Status, RowVersion: x.RowVersion, BaseRevision: x.BaseRevision,
		DraftID: x.DraftID, OrderID: x.OrderID, OrderRevision: x.OrderRevision, OrderHash: x.OrderHash, SupplierID: x.SupplierID,
		Currency: x.Currency, SupplyValidUntil: x.SupplyValidUntil, ChangeHash: x.Hash, Document: string(x.Document),
		Lines: revisionLines(x.Lines), Deviations: []api.DeviationView{}, Reason: x.Reason, Evidence: evidenceView(x.Evidence),
		PreparedBy: x.Prepared.By, PreparedAt: x.Prepared.At, PrepareEpoch: x.Prepared.Epoch, Acknowledgements: []api.AcknowledgementView{},
		AppliedRevision: x.AppliedRevision}
	if doc, err := x.Decode(); err == nil {
		for _, dv := range commitment.ChangeDeviations(doc) {
			v.Deviations = append(v.Deviations, api.DeviationView{OrderLineID: dv.OrderLineID, Kind: dv.Kind})
		}
	}
	if d := x.Decision; d != nil {
		v.Decision, v.DecidedBy, v.DecidedAt, v.DecideEpoch, v.DecisionReason = api.DecisionReject, d.By, d.At, d.Epoch, d.Reason
		if d.Approve {
			v.Decision = api.DecisionApprove
		}
		for _, a := range d.Acknowledgements {
			v.Acknowledgements = append(v.Acknowledgements, api.AcknowledgementView{OrderLineID: a.OrderLineID, Kind: a.Kind, Reason: a.Reason})
		}
	}
	if a := x.Applied; a != nil {
		v.AppliedBy, v.AppliedAt, v.ApplyEpoch = a.By, a.At, a.Epoch
	}
	return v
}

// GetCommitmentChange returns a change of a commitment the caller sees, audited as
// purchase.commitment.change.get (pur.read): NOT_FOUND otherwise; REFERENCE_INVALID when the caller no
// longer reads the commitment's order (TRD's lookup, before PUR's read transaction).
func (s Service) GetCommitmentChange(ctx context.Context, id string) (api.ChangeView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.ChangeView{}, s.refuseRead(ctx, api.CommandGetCommitmentChange, "/id")
	}
	var orderID string
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		x, err := postgres.LoadChange(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err == nil && x != nil {
			orderID = x.OrderID
		}
		return err
	})
	if err != nil {
		return api.ChangeView{}, err
	}
	readable := false
	if orderID != "" {
		if readable, err = s.orderReadable(ctx, orderID); err != nil {
			return api.ChangeView{}, err
		}
	}
	var v api.ChangeView
	err = s.Ledger.Read(ctx, api.CommandGetCommitmentChange, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		x, err := postgres.LoadChange(ctx, exec, sc, id, postgres.NoLock)
		if err != nil {
			return err
		}
		var c *commitment.Commitment
		if x != nil {
			if c, err = postgres.LoadCommitment(ctx, exec, sc, x.CommitmentID, postgres.NoLock); err != nil {
				return err
			}
		}
		if x == nil || c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		if !readable || x.OrderID != orderID {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/id", "The caller no longer reads the commitment's order")
		}
		v = changeView(x)
		return nil
	})
	return v, err
}

// GetCommitmentChanges returns the changes of a commitment the caller sees, by change ID, audited as
// purchase.commitment.change.list (pur.read): NOT_FOUND otherwise; REFERENCE_INVALID when the caller
// no longer reads the commitment's order.
func (s Service) GetCommitmentChanges(ctx context.Context, commitmentID string) ([]api.ChangeView, error) {
	if !kernel.ValidIdentifier(commitmentID) {
		return nil, s.refuseRead(ctx, api.CommandGetCommitmentChanges, "/commitment_id")
	}
	before, err := s.loadCommitment(ctx, commitmentID)
	if err != nil {
		return nil, err
	}
	readable := false
	if before.c != nil {
		if readable, err = s.orderReadable(ctx, before.c.OrderID); err != nil {
			return nil, err
		}
	}
	var out []api.ChangeView
	err = s.Ledger.Read(ctx, api.CommandGetCommitmentChanges, commitmentID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		c, err := postgres.LoadCommitment(ctx, exec, sc, commitmentID, postgres.NoLock)
		if err != nil {
			return err
		}
		if c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/commitment_id", "")
		}
		if !readable || before.c == nil || before.c.OrderID != c.OrderID {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/commitment_id", "The caller no longer reads the commitment's order")
		}
		xs, err := postgres.ChangesOfCommitment(ctx, exec, sc, commitmentID)
		if err != nil {
			return err
		}
		out = []api.ChangeView{}
		for _, x := range xs {
			out = append(out, changeView(x))
		}
		return nil
	})
	return out, err
}
