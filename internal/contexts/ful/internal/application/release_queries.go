package application

import (
	"context"
	"encoding/json"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/release"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func guardViews(entries []shipment.Entry) []api.GuardEvidence {
	out := make([]api.GuardEvidence, 0, len(entries))
	for _, e := range entries {
		out = append(out, api.GuardEvidence(e))
	}
	return out
}

// bindingsView is the published form of b: the same JSON members (api.ReleaseBindings).
func bindingsView(b release.Bindings) api.ReleaseBindings {
	var out api.ReleaseBindings
	doc, _ := json.Marshal(b)
	_ = json.Unmarshal(doc, &out)
	return out
}

func releaseView(r *release.Release) api.ReleaseView {
	v := api.ReleaseView{ReleaseID: r.ID, ShipmentID: r.ShipmentID, Scope: r.Scope, Status: r.Status, RowVersion: r.RowVersion,
		Order:       api.OrderBinding{OrderID: r.Order.OrderID, OrderRevision: r.Order.OrderRevision, OrderHash: r.Order.OrderHash},
		Evaluations: []api.EvaluationView{}, Exceptions: []api.ExceptionView{}}
	for _, e := range r.Evaluations {
		v.Evaluations = append(v.Evaluations, api.EvaluationView{Seq: e.Seq, ReleaseVersion: e.ReleaseVersion, Outcome: e.Outcome, Guard: e.Guard,
			Code: e.Code, Reason: e.Reason, ExceptionSeq: e.ExceptionSeq, Guards: guardViews(e.Entries), GuardsSHA256: e.EntriesSHA256,
			Bindings: bindingsView(e.Bindings), EvaluatedBy: e.Evaluated.By, EvaluatedAt: e.Evaluated.At, AuthorityEpoch: e.Evaluated.Epoch})
	}
	for _, x := range r.Exceptions {
		v.Exceptions = append(v.Exceptions, api.ExceptionView{Seq: x.Seq, ReleaseVersion: x.ReleaseVersion, Order: v.Order, StageID: x.StageID,
			Currency: x.Currency, ShortfallMinor: x.ShortfallMinor, ValidUntil: x.ValidUntil, Reason: x.Reason, EvidenceIDs: slices.Clone(x.Evidence),
			Guards: guardViews(x.Entries), GuardsSHA256: x.EntriesSHA256, GrantedBy: x.Granted.By, GrantedAt: x.Granted.At,
			AuthorityEpoch: x.Granted.Epoch})
	}
	if c := r.Consumption; c != nil {
		v.Consumption = &api.ConsumptionView{Command: c.Command, ReleaseKind: c.ReleaseKind, EvidenceIDs: slices.Clone(c.Evidence),
			Guards: guardViews(c.Entries), GuardsSHA256: c.EntriesSHA256, ConsumedBy: c.Consumed.By, ConsumedAt: c.Consumed.At,
			AuthorityEpoch: c.Consumed.Epoch}
		if b := c.BillOfLading; b != nil {
			v.Consumption.BillOfLading = &api.BillOfLadingRef{DocumentSetID: b.DocumentSetID, Version: b.Version, Number: b.Number,
				ContentSHA256: b.ContentSHA256}
		}
	}
	return v
}

// GetRelease returns a release of a shipment the caller sees, audited as commercial_release.get
// (ful.read): NOT_FOUND otherwise.
func (s Service) GetRelease(ctx context.Context, id string) (api.ReleaseView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.ReleaseView{}, invalidID(s, ctx, api.CommandGetRelease)
	}
	var v api.ReleaseView
	err := s.Ledger.Read(ctx, api.CommandGetRelease, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		r, err := postgres.LoadReleaseByID(ctx, exec, scope(ctx), id)
		if err != nil {
			return err
		}
		if r == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		sh, err := postgres.LoadShipment(ctx, exec, scope(ctx), r.ShipmentID, postgres.NoLock)
		if err != nil {
			return err
		}
		if sh == nil || !d.Sees(sh.OwnerActor, sh.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = releaseView(r)
		return nil
	})
	return v, err
}

// ShipmentReleases returns the releases of a shipment the caller sees, audited as
// commercial_release.list (ful.read): NOT_FOUND otherwise.
func (s Service) ShipmentReleases(ctx context.Context, shipmentID string) (api.ShipmentReleases, error) {
	if !kernel.ValidIdentifier(shipmentID) {
		return api.ShipmentReleases{}, invalidID(s, ctx, api.CommandListShipmentReleases)
	}
	out := api.ShipmentReleases{ShipmentID: shipmentID, Releases: []api.ReleaseView{}}
	err := s.Ledger.Read(ctx, api.CommandListShipmentReleases, shipmentID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sh, err := postgres.LoadShipment(ctx, exec, scope(ctx), shipmentID, postgres.NoLock)
		if err != nil {
			return err
		}
		if sh == nil || !d.Sees(sh.OwnerActor, sh.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		list, err := postgres.LoadShipmentReleases(ctx, exec, scope(ctx), shipmentID)
		for _, r := range list {
			out.Releases = append(out.Releases, releaseView(r))
		}
		return err
	})
	return out, err
}
