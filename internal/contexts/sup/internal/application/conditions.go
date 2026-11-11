package application

import (
	"context"
	"encoding/json"
	"maps"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/domain/revision"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// conditionCommand locks a supply condition root, with the revision named by the command, and
// applies one command.
type conditionCommand struct {
	s     Service
	kind  string
	id    string
	named int64
	root  *revision.Root
	from  int64
	apply func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *conditionCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.root, err = postgres.LoadRoot(ctx, exec, scope(ctx), h.kind, h.id, h.named, true)
	h.from = 0
	if h.root != nil {
		h.from = h.root.Version
	}
	return err
}

func (h *conditionCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *conditionCommand) visible(d authorization.Decision) bool {
	return h.root != nil && d.Sees(h.root.OwnerActor, h.root.OwnerTeam)
}

func (h *conditionCommand) outcome(rev *revision.Revision, events ...action.Event) action.Outcome {
	return action.Outcome{ObjectID: h.id, RowVersion: h.root.Version, Status: string(rev.Status), Events: events}
}

var aggregateTypes = map[string]string{
	api.KindSupplierOffer: "SupplierOffer", api.KindPurchaseTerms: "PurchaseTerms", api.KindLogisticsOffer: "LogisticsOffer",
}

// event carries identifiers, versions and the content hash of rev, never its content (contract C4).
func (h *conditionCommand) event(typ string, rev *revision.Revision, fields map[string]any) action.Event {
	f := map[string]any{"kind": h.kind, "id": h.id, "revision": rev.Number, "root_version": h.root.Version, "content_hash": rev.Content.Hash}
	maps.Copy(f, fields)
	return action.Event{Type: typ, AggregateType: aggregateTypes[h.kind], AggregateID: h.id, AggregateVersion: h.root.Version, Fields: f}
}

// own reads SUP's own facts a document is judged against, in SUP's transaction and without locks:
// the supplier RFQ an offer answers and the source offer revision of purchase terms, each only when
// the caller sees it.
func (h *conditionCommand) own(ctx context.Context, exec bentopg.Executor, d authorization.Decision, doc document) (own, error) {
	var w own
	sc := scope(ctx)
	switch {
	case doc.offer != nil && doc.offer.Origin != nil && doc.offer.Origin.SupplierRfq != nil:
		r, err := postgres.LoadSupplierRfq(ctx, exec, sc, doc.offer.Origin.SupplierRfq.SupplierRfqID, false)
		if err != nil || r == nil || !d.Sees(r.OwnerActor, r.OwnerTeam) {
			return w, err
		}
		w.supplierRfq, w.supplierRfqContent = r, &api.SupplierRfq{}
		return w, json.Unmarshal(r.Data, w.supplierRfqContent)
	case doc.terms != nil && doc.terms.SourceOfferRef != nil:
		ref := doc.terms.SourceOfferRef
		key := postgres.Key{Kind: api.KindSupplierOffer, ID: ref.ID, Revision: ref.Revision}
		found, err := postgres.RevisionsByKey(ctx, exec, sc, []postgres.Key{key})
		s, ok := found[key]
		if err != nil || !ok || !d.Sees(s.OwnerActor, s.OwnerTeam) {
			return w, err
		}
		w.source, w.sourceContent = s.Rev, &api.SupplierOffer{}
		return w, json.Unmarshal(s.Rev.Content.Data, w.sourceContent)
	}
	return w, nil
}

// document judges a document of any kind.
func (j judge) document(d document, w own) error {
	switch d.kind {
	case api.KindSupplierOffer:
		return j.offer(d.offer, w)
	case api.KindPurchaseTerms:
		return j.terms(d.terms, w)
	}
	return j.logistics(d.logistics)
}

// SaveSupplierOffer saves the whole draft revision of a supplier offer (supplier_offer.save_draft).
func (s Service) SaveSupplierOffer(ctx context.Context, cmd api.SaveSupplierOffer) (api.Receipt, error) {
	return s.save(ctx, cmd.IdempotencyKey, cmd.OfferID, cmd.ExpectedVersion, document{kind: api.KindSupplierOffer, offer: &cmd.Data},
		cmd.Validate())
}

// SavePurchaseTerms saves the whole draft revision of purchase terms (purchase_terms.save_draft).
func (s Service) SavePurchaseTerms(ctx context.Context, cmd api.SavePurchaseTerms) (api.Receipt, error) {
	return s.save(ctx, cmd.IdempotencyKey, cmd.TermsID, cmd.ExpectedVersion, document{kind: api.KindPurchaseTerms, terms: &cmd.Data},
		cmd.Validate())
}

// SaveLogisticsOffer saves the whole draft revision of a logistics offer (logistics_offer.save_draft).
func (s Service) SaveLogisticsOffer(ctx context.Context, cmd api.SaveLogisticsOffer) (api.Receipt, error) {
	return s.save(ctx, cmd.IdempotencyKey, cmd.OfferID, cmd.ExpectedVersion, document{kind: api.KindLogisticsOffer, logistics: &cmd.Data},
		cmd.Validate())
}

func (s Service) save(ctx context.Context, key, id string, expected int64, doc document, validation error) (api.Receipt, error) {
	var f facts
	h := &conditionCommand{s: s, kind: doc.kind, id: id}
	prepare := func(ctx context.Context) (err error) {
		var r refs
		r.document(doc)
		f, err = s.gather(ctx, r, s.Clock.Now())
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := revision.CheckSave(h.root, h.visible(d), expected); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		w, err := h.own(ctx, exec, d, doc)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = (judge{f: f, at: now}).document(doc, w); err != nil {
			return action.Outcome{}, err
		}
		content, err := doc.encode()
		if err != nil {
			return action.Outcome{}, err
		}
		created := h.root == nil
		if created {
			h.root = revision.New(revision.Kind(doc.kind), id, d.Actor.ID, d.Team)
		}
		rev, fresh, err := h.root.Save(content, d.Actor, d.Epoch, now)
		if err != nil {
			return action.Outcome{}, err
		}
		sc := scope(ctx)
		if created {
			err = postgres.InsertRoot(ctx, exec, sc, h.root, now)
		} else {
			err = postgres.UpdateRoot(ctx, exec, sc, h.root, h.from, now)
		}
		if err == nil && fresh {
			err = postgres.InsertRevision(ctx, exec, sc, h.root, rev)
		} else if err == nil {
			err = postgres.UpdateRevision(ctx, exec, sc, h.root, rev)
		}
		return h.outcome(rev), err
	}
	args := struct {
		Kind string
		ID   string
		Data any
	}{doc.kind, id, doc.member()}
	return s.run(ctx, action.Command{Name: api.Command(doc.kind, api.ActionSaveDraft), IdempotencyKey: key, ObjectID: id}, args, validation,
		prepared{h, prepare})
}

// Revise starts a new draft revision from a sealed one (<kind>.revise).
func (s Service) Revise(ctx context.Context, cmd api.Revise) (api.Receipt, error) {
	h := &conditionCommand{s: s, kind: cmd.Kind, id: cmd.ID, named: cmd.BaseRevision}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := revision.Check(h.root, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		rev, err := h.root.Revise(h.root.Revision(cmd.BaseRevision), d.Actor, d.Epoch, now)
		if err != nil {
			return action.Outcome{}, err
		}
		sc := scope(ctx)
		if err = postgres.UpdateRoot(ctx, exec, sc, h.root, h.from, now); err == nil {
			err = postgres.InsertRevision(ctx, exec, sc, h.root, rev)
		}
		return h.outcome(rev), err
	}
	args := struct {
		Kind, ID     string
		BaseRevision int64
		Reason       string
	}{cmd.Kind, cmd.ID, cmd.BaseRevision, cmd.Reason}
	return s.run(ctx, action.Command{Name: api.Command(cmd.Kind, api.ActionRevise), IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ID,
		Reason: cmd.Reason}, args, cmd.Validate(), h)
}

// DiscardDraft ends the draft revision (<kind>.discard_draft).
func (s Service) DiscardDraft(ctx context.Context, cmd api.DiscardDraft) (api.Receipt, error) {
	h := &conditionCommand{s: s, kind: cmd.Kind, id: cmd.ID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := revision.Check(h.root, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		rev, err := h.root.Discard(cmd.WorkingRevision, d.Actor, d.Epoch, cmd.Reason, now)
		if err != nil {
			return action.Outcome{}, err
		}
		sc := scope(ctx)
		if err = postgres.UpdateRevision(ctx, exec, sc, h.root, rev); err == nil {
			err = postgres.UpdateRoot(ctx, exec, sc, h.root, h.from, now)
		}
		return h.outcome(rev), err
	}
	args := struct {
		Kind, ID        string
		WorkingRevision int64
		Reason          string
	}{cmd.Kind, cmd.ID, cmd.WorkingRevision, cmd.Reason}
	return s.run(ctx, action.Command{Name: api.Command(cmd.Kind, api.ActionDiscardDraft), IdempotencyKey: cmd.IdempotencyKey,
		ObjectID: cmd.ID, Reason: cmd.Reason}, args, cmd.Validate(), h)
}

var sealEvents = map[string]string{
	api.KindSupplierOffer: api.EventSupplierOfferVerified, api.KindPurchaseTerms: api.EventPurchaseTermsPublished,
	api.KindLogisticsOffer: api.EventLogisticsOfferVerified,
}

var endEvents = map[string]string{
	api.KindSupplierOffer: api.EventSupplierOfferWithdrawn, api.KindPurchaseTerms: api.EventPurchaseTermsRetired,
	api.KindLogisticsOffer: api.EventLogisticsOfferWithdrawn,
}

// Seal verifies or publishes the draft revision and supersedes the active one (supplier_offer.verify,
// purchase_terms.publish, logistics_offer.verify; SUP-INV-001, SUP-INV-003).
func (s Service) Seal(ctx context.Context, cmd api.Seal) (api.Receipt, error) {
	var f facts
	var seen int64 // the root version the draft's references were read for
	h := &conditionCommand{s: s, kind: cmd.Kind, id: cmd.ID}
	prepare := func(ctx context.Context) error {
		var r refs
		r.evidence(cmd.Evidence)
		err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
			root, err := postgres.LoadRoot(ctx, exec, scope(ctx), cmd.Kind, cmd.ID, 0, false)
			if err != nil || root == nil || root.Working == nil || !d.Sees(root.OwnerActor, root.OwnerTeam) {
				return err
			}
			doc, err := decode(cmd.Kind, root.Working.Content.Data)
			if err != nil {
				return err
			}
			seen = root.Version
			r.document(doc)
			return nil
		})
		if err != nil {
			return err
		}
		f, err = s.gather(ctx, r, s.Clock.Now())
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := revision.Check(h.root, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if h.root.Working != nil && h.root.Version != seen {
			// The draft changed after its references were read; sealing would judge stale facts.
			return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
		}
		if err := h.root.CheckSeal(cmd.WorkingRevision, cmd.SupersedesRevision, d.Actor); err != nil {
			return action.Outcome{}, err
		}
		doc, err := decode(cmd.Kind, h.root.Working.Content.Data)
		if err != nil {
			return action.Outcome{}, err
		}
		w, err := h.own(ctx, exec, d, doc)
		if err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		j := judge{f: f, at: now, seal: true}
		if err = j.document(doc, w); err != nil {
			return action.Outcome{}, err
		}
		if err = j.sources(cmd.Evidence, "/args/evidence"); err != nil {
			return action.Outcome{}, err
		}
		superseded := h.root.Seal(d.Actor, d.Epoch, toEvidence(cmd.Evidence), now)
		active, sc := h.root.Active, scope(ctx)
		fields := map[string]any{"superseded_revision": int64(0)}
		if superseded != nil {
			// The superseded revision is written first: there is one active revision at a time.
			fields["superseded_revision"] = superseded.Number
			err = postgres.UpdateRevision(ctx, exec, sc, h.root, superseded)
		}
		if hd := active.Content.Header; hd.ValidityMode != "" {
			fields["validity_mode"], fields["source_offer_id"], fields["source_offer_revision"] = hd.ValidityMode, hd.SourceID, hd.SourceRevision
		}
		if err == nil {
			err = postgres.UpdateRevision(ctx, exec, sc, h.root, active)
		}
		if err == nil {
			err = postgres.UpdateRoot(ctx, exec, sc, h.root, h.from, now)
		}
		return h.outcome(active, h.event(sealEvents[cmd.Kind], active, fields)), err
	}
	args := struct {
		Kind, ID           string
		WorkingRevision    int64
		SupersedesRevision int64
		Evidence           []api.Evidence
	}{cmd.Kind, cmd.ID, cmd.WorkingRevision, cmd.SupersedesRevision, cmd.Evidence}
	return s.run(ctx, action.Command{Name: api.Command(cmd.Kind, api.SealAction(cmd.Kind)), IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ID}, args, cmd.Validate(),
		prepared{h, prepare})
}

// End withdraws or retires an exact sealed revision (supplier_offer.withdraw, purchase_terms.retire,
// logistics_offer.withdraw).
func (s Service) End(ctx context.Context, cmd api.End) (api.Receipt, error) {
	var f facts
	h := &conditionCommand{s: s, kind: cmd.Kind, id: cmd.ID, named: cmd.TargetRevision}
	prepare := func(ctx context.Context) (err error) {
		var r refs
		r.evidence(cmd.Evidence)
		f, err = s.gather(ctx, r, s.Clock.Now())
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := revision.Check(h.root, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		target := h.root.Revision(cmd.TargetRevision)
		if target == nil || (target.Status != revision.Active && target.Status != revision.Superseded) {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/target_revision", "")
		}
		if err := (judge{f: f}).sources(cmd.Evidence, "/args/evidence"); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		if err := h.root.End(target, d.Actor, d.Epoch, cmd.Reason, toEvidence(cmd.Evidence), now); err != nil {
			return action.Outcome{}, err
		}
		sc := scope(ctx)
		err := postgres.UpdateRevision(ctx, exec, sc, h.root, target)
		if err == nil {
			err = postgres.UpdateRoot(ctx, exec, sc, h.root, h.from, now)
		}
		active := int64(0)
		if h.root.Active != nil {
			active = h.root.Active.Number
		}
		return h.outcome(target, h.event(endEvents[cmd.Kind], target, map[string]any{"active_revision": active})), err
	}
	args := struct {
		Kind, ID       string
		TargetRevision int64
		Reason         string
		Evidence       []api.Evidence
	}{cmd.Kind, cmd.ID, cmd.TargetRevision, cmd.Reason, cmd.Evidence}
	return s.run(ctx, action.Command{Name: api.Command(cmd.Kind, api.EndAction(cmd.Kind)), IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ID, Reason: cmd.Reason}, args,
		cmd.Validate(), prepared{h, prepare})
}
