package application

import (
	"context"
	"encoding/json"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/domain/revision"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// GetSupplierRfq returns a supplier RFQ the caller sees, audited.
func (s Service) GetSupplierRfq(ctx context.Context, id string) (api.SupplierRfqView, error) {
	var v api.SupplierRfqView
	err := s.Ledger.Read(ctx, CommandGetSupplierRfq, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		if !kernel.ValidIdentifier(id) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		r, err := postgres.LoadSupplierRfq(ctx, exec, scope(ctx), id, false)
		if err != nil {
			return err
		}
		if r == nil || !d.Sees(r.OwnerActor, r.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.SupplierRfqView{SupplierRfqID: r.ID, Status: string(r.Status), RowVersion: r.RowVersion, ContentHash: r.Hash,
			SavedBy: r.SavedBy, SavedAt: r.SavedAt, IssuedVersion: r.IssuedVersion, Issue: fromAct(r.Issue), Close: fromAct(r.Closing)}
		return json.Unmarshal(r.Data, &v.Data)
	})
	return v, err
}

// revisionView returns the view of a revision of kind.
func revisionView(kind string, rev *revision.Revision) (api.RevisionView, error) {
	v := api.RevisionView{Revision: rev.Number, Status: string(rev.Status), BaseRevision: rev.Base, ContentHash: rev.Content.Hash,
		SavedBy: rev.SavedBy, SavedAt: rev.SavedAt, Contributors: rev.Contributors, Verification: fromAct(rev.Verification),
		SupersededAt: rev.SupersededAt, End: fromAct(rev.End)}
	doc, err := decode(kind, rev.Content.Data)
	v.Offer, v.Terms, v.Logistics = doc.offer, doc.terms, doc.logistics
	return v, err
}

// GetCondition returns a supply condition the caller sees and all its revisions, audited as
// <kind>.get. Sealed revisions are read as sealed: a later supersession, withdrawal or expiry never
// changes them.
func (s Service) GetCondition(ctx context.Context, kind, id string) (api.ConditionView, error) {
	var v api.ConditionView
	command := CommandGet(kind)
	if !api.KindKnown(kind) {
		command = CommandGet(api.KindSupplierOffer)
	}
	err := s.Ledger.Read(ctx, command, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		if !api.KindKnown(kind) || !kernel.ValidIdentifier(id) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		sc := scope(ctx)
		root, err := postgres.LoadRoot(ctx, exec, sc, kind, id, 0, false)
		if err != nil {
			return err
		}
		if root == nil || !d.Sees(root.OwnerActor, root.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.ConditionView{Kind: kind, ID: id, RootVersion: root.Version, Revisions: []api.RevisionView{}}
		if root.Working != nil {
			v.WorkingRevision = root.Working.Number
		}
		if root.Active != nil {
			v.ActiveRevision = root.Active.Number
		}
		revisions, err := postgres.Revisions(ctx, exec, sc, kind, id)
		if err != nil {
			return err
		}
		for _, rev := range revisions {
			view, err := revisionView(kind, rev)
			if err != nil {
				return err
			}
			v.Revisions = append(v.Revisions, view)
		}
		return nil
	})
	return v, err
}

// ResolveSupplyConditions answers SupplyConditionRef v1 lookups of downstream contexts at instant at
// (R09, R20): the sealed revisions the caller sees, with their content and the verdict
// SupplyConditionGuard v1 would give now. Drafts and discarded revisions are absent: they were
// never supply conditions.
func (s Service) ResolveSupplyConditions(ctx context.Context, refs []api.SupplyConditionRef, at time.Time) (map[api.SupplyConditionRef]api.ResolvedSupplyCondition, error) {
	out := map[api.SupplyConditionRef]api.ResolvedSupplyCondition{}
	if err := api.ValidateRefs(refs); err != nil {
		return out, err
	}
	if at.IsZero() {
		return out, kernel.Reject(kernel.CodeSchemaInvalid, "/at", "")
	}
	if len(refs) == 0 {
		return out, nil
	}
	at = kernel.Instant(at)
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		keys := make([]postgres.Key, len(refs))
		for i, r := range refs {
			keys[i] = postgres.Key(r)
		}
		found, err := postgres.RevisionsByKey(ctx, exec, sc, keys)
		if err != nil {
			return err
		}
		// The source offers of dependent purchase terms decide their verdict too.
		var sources []postgres.Key
		for _, st := range found {
			if h := st.Rev.Content.Header; h.Dependent() {
				sources = append(sources, postgres.Key{Kind: api.KindSupplierOffer, ID: h.SourceID, Revision: h.SourceRevision})
			}
		}
		origins, err := postgres.RevisionsByKey(ctx, exec, sc, sources)
		if err != nil {
			return err
		}
		for i, ref := range refs {
			st, ok := found[keys[i]]
			if !ok || !st.Rev.Sealed() || !d.Sees(st.OwnerActor, st.OwnerTeam) {
				continue
			}
			rev := st.Rev
			h := rev.Content.Header
			var source *revision.Revision
			if h.Dependent() {
				source = origins[postgres.Key{Kind: api.KindSupplierOffer, ID: h.SourceID, Revision: h.SourceRevision}].Rev
			}
			doc, err := decode(ref.Kind, rev.Content.Data)
			if err != nil {
				return err
			}
			out[ref] = api.ResolvedSupplyCondition{Ref: ref, Status: string(rev.Status), Code: revision.InForceWith(rev, source, at),
				ContentHash: rev.Content.Hash, ValidFrom: h.ValidFrom, ValidUntil: h.ValidUntil, Verification: *fromAct(rev.Verification),
				Offer: doc.offer, Terms: doc.terms, Logistics: doc.logistics}
		}
		return nil
	})
	return out, err
}
