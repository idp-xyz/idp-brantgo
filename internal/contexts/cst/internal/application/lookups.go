package application

import (
	"context"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
)

// ResolveContexts answers costing context references of a cost scenario (api.Lookups) in a
// transaction of CST's own: each sealed revision d's actor sees.
func (s Service) ResolveContexts(ctx context.Context, refs []api.ContextRef) (map[api.ContextRef]api.VerifiedContext, error) {
	var out map[api.ContextRef]api.VerifiedContext
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		var err error
		out, err = VerifiedContexts(ctx, exec, d, refs, postgres.NoLock)
		return err
	})
	return out, err
}

// VerifiedContexts reads the sealed costing context revisions among refs that d's actor sees, with
// exec inside a CST transaction. A freeze passes postgres.ForShare after locking its scenario root
// FOR UPDATE, so no verification or withdrawal of those contexts commits before it does.
func VerifiedContexts(ctx context.Context, exec bentopg.Executor, d authorization.Decision, refs []api.ContextRef, lock postgres.Lock) (map[api.ContextRef]api.VerifiedContext, error) {
	keys := make([]postgres.ContextRevisionKey, len(refs))
	for i, r := range refs {
		keys[i] = postgres.ContextRevisionKey{ContextID: r.ContextID, Revision: r.Revision}
	}
	found, err := postgres.ContextRevisions(ctx, exec, scope(ctx), keys, lock)
	out := map[api.ContextRef]api.VerifiedContext{}
	for k, o := range found {
		if d.Sees(o.OwnerActor, o.OwnerTeam) {
			out[api.ContextRef{ContextID: k.ContextID, Revision: k.Revision}] = verifiedContext(k.ContextID, o.Revision)
		}
	}
	return out, err
}

// ResolveMeasurements answers measurement references of a cost scenario (api.Lookups) in a
// transaction of CST's own.
func (s Service) ResolveMeasurements(ctx context.Context, refs []api.MeasurementRef) (map[api.MeasurementRef]api.ResolvedMeasurement, error) {
	var out map[api.MeasurementRef]api.ResolvedMeasurement
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		var err error
		out, err = Measurements(ctx, exec, d, refs, postgres.NoLock)
		return err
	})
	return out, err
}

// MeasurementsForLines answers the active measurement revisions holding any of lineIDs.
func (s Service) MeasurementsForLines(ctx context.Context, lineIDs []string) (map[api.MeasurementRef]api.ResolvedMeasurement, error) {
	out := map[api.MeasurementRef]api.ResolvedMeasurement{}
	if len(lineIDs) == 0 {
		return out, nil
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		keys, err := postgres.ActiveMeasurementsForLines(ctx, exec, scope(ctx), slices.Compact(slices.Sorted(slices.Values(lineIDs))))
		if err != nil {
			return err
		}
		refs := make([]api.MeasurementRef, len(keys))
		for i, k := range keys {
			refs[i] = api.MeasurementRef{MeasurementID: k.MeasurementID, Revision: k.Revision}
		}
		out, err = Measurements(ctx, exec, d, refs, postgres.NoLock)
		return err
	})
	return out, err
}

// Measurements reads the measurement revisions among refs that d's actor sees, with exec inside a
// CST transaction; a freeze passes postgres.ForShare.
func Measurements(ctx context.Context, exec bentopg.Executor, d authorization.Decision, refs []api.MeasurementRef, lock postgres.Lock) (map[api.MeasurementRef]api.ResolvedMeasurement, error) {
	keys := make([]postgres.MeasurementRevisionKey, len(refs))
	for i, r := range refs {
		keys[i] = postgres.MeasurementRevisionKey{MeasurementID: r.MeasurementID, Revision: r.Revision}
	}
	found, err := postgres.MeasurementRevisions(ctx, exec, scope(ctx), keys, lock)
	out := map[api.MeasurementRef]api.ResolvedMeasurement{}
	for k, o := range found {
		if d.Sees(o.OwnerActor, o.OwnerTeam) {
			out[api.MeasurementRef{MeasurementID: k.MeasurementID, Revision: k.Revision}] = resolvedMeasurement(k.MeasurementID, o.Revision)
		}
	}
	return out, err
}
