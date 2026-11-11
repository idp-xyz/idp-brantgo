package application

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/document"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// quote.activate and quote.replace (human; WBS S3-04, S3-05; TRD-INV-001, 009, 010, 016; api/formal.go).
//
// Before TRD's transaction — after the ledger authorized the command and found no stored result for
// its key — Prepare:
//
//  1. reads the root in a TRD transaction of its own and runs every check the command transaction
//     will run (admitFormal: visibility, object control, root version, conversion slot, Releasable,
//     the upstream guards), so a refusal costs no number;
//  2. reads what the formal document prints (documentFacts: PTY's customer and contact names, CAT's
//     item names, POL's template, currency and trade terms, and the sales exchange rate's age
//     inputs), each upstream in its own transaction, and checks the exchange rate's age;
//  3. prepares the document (document.Prepare: everything but the number, the size included);
//  4. has POL issue the number for the revision's business reference (number.issue as the acting
//     human, NumberIssue v1, Context Map R15; the tactical model's QuoteRoot and
//     DocumentNumberSequence notes: 编号由 POL 在事务前分配). POL returns the number a business
//     reference already has, so a retry after a rollback formalizes with the same number; a
//     revision never formalized leaves a gap in the sequence and nothing else.
//
// In the transaction (lock order ADR-BR-011, tactical lockOrder):
//
//  1. the quote root FOR UPDATE (Lock), which also holds its object control and conversion slot;
//  2. the company's authority guard row FOR SHARE (the ledger's Authorize);
//  3. visibility, the object control, the root version (VERSION_CONFLICT: two first activations of
//     one root have one winner, TRD-INV-001), the conversion slot for a replacement
//     (ROOT_CONVERSION_LOCKED, TRD-INV-010);
//  4. Releasable (review.go): the approval releases the revision now, on the reviewers' grants read
//     FOR SHARE through the authority guard (the first upstream guard in lock order);
//  5. the sales exchange rate's age at the transaction's instant (FX_STALE);
//  6. the freeze's upstream guards, in the same context code order (guardUpstream: CAT SkuGuard, CST
//     AdoptabilityGuard, INQ RfqGuard, POL ConfigGuard with the content hashes the manifest pins, PTY
//     TradingProfileGuard and AddressRoleGuard): what the revision binds is still in force;
//  7. the document rendered with the number (Plan.Formalize; pure, no wait);
//  8. the formalization (quote.Formalize): document and commit evidence bound, the revision active,
//     a replaced revision superseded;
//  9. the writes: root (version CAS), document bytes and binding, commit evidence, revision
//     statuses, then the ledger's action result, audit entry and QuoteActivated or QuoteReplaced.
//
// Any failure rolls all of it back (TRD-INV-016).
//
// Not wired, by the owner's decision of 2026-11-10: STL CreditGuard (R57) and CNX
// TradeComplianceGuard (R58) belong to order.activate (TRD-INV-022, 023) and TSK's object control
// commands to TSK (R17); they are recorded gaps of S4–S5, not passes.

// formalCmd is an activation or a replacement as the command path handles both.
type formalCmd struct {
	name     string
	key      string
	quoteID  string
	expected int64
	release  quote.Release
}

// Activate formalizes an approved revision as the root's first active revision (quote.activate).
func (s Service) Activate(ctx context.Context, cmd api.Activate) (api.FormalizationReceipt, error) {
	return s.formalize(ctx, formalCmd{name: api.CommandActivate, key: cmd.IdempotencyKey, quoteID: cmd.QuoteID, expected: cmd.ExpectedVersion,
		release: quote.Release{Revision: cmd.BusinessRevision, Target: quote.TargetActivate, ApprovalID: cmd.ApprovalID,
			ExpectedEffectivity: cmd.ExpectedApprovalEffectivityVersion}}, cmd, cmd.Validate())
}

// Replace formalizes an approved revision in place of the active one, which becomes superseded
// (quote.replace).
func (s Service) Replace(ctx context.Context, cmd api.Replace) (api.FormalizationReceipt, error) {
	return s.formalize(ctx, formalCmd{name: api.CommandReplace, key: cmd.IdempotencyKey, quoteID: cmd.QuoteID, expected: cmd.ExpectedVersion,
		release: quote.Release{Revision: cmd.BusinessRevision, Target: quote.TargetReplace, ExpectedActive: cmd.ExpectedActiveRevision,
			ApprovalID: cmd.ApprovalID, ExpectedEffectivity: cmd.ExpectedApprovalEffectivityVersion}}, cmd, cmd.Validate())
}

// numberKey is the idempotency key of POL's number.issue for formalization command name sent with
// key: a retry of the same command replays POL's action, another command for the revision gets the
// number of its business reference back.
func numberKey(name, key string) string { return "trd." + quote.Hash([]byte(name + "\n" + key))[:56] }

func (s Service) formalize(ctx context.Context, f formalCmd, req api.Command, validation error) (api.FormalizationReceipt, error) {
	h := &quoteCommand{s: s, id: f.quoteID}
	var plan *document.Plan
	var number string
	var fx salesFX
	h.prepare = func(ctx context.Context) error {
		var rev *quote.Revision
		var doc quote.Commercial
		err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
			q, err := postgres.LoadQuote(ctx, exec, scope(ctx), f.quoteID, postgres.NoLock)
			if err != nil {
				return err
			}
			in, err := s.admitFormal(ctx, exec, &quoteCommand{s: s, id: f.quoteID, q: q}, d, f, nil, s.Clock.Now())
			rev, doc = in.revision, in.commercial
			return err
		})
		if err != nil {
			return err
		}
		template, facts, err := s.documentFacts(ctx, doc, &fx, s.Clock.Now())
		if err != nil {
			return err
		}
		if err = fx.check(s.Clock.Now()); err != nil {
			return err
		}
		if plan, err = document.Prepare(document.Revision{Commercial: rev.Seal.Commercial, BusinessHash: rev.Seal.BusinessHash}, template,
			facts); err != nil {
			return err
		}
		seq := plan.Number.Sequence
		issued, err := s.Upstream.Numbers.IssueNumber(ctx, polapi.IssueNumber{IdempotencyKey: numberKey(f.name, f.key),
			Sequence: polapi.ConfigRef{Kind: seq.Kind, ConfigID: seq.ID, Revision: seq.Revision}, BusinessRef: plan.Number.BusinessRef})
		if err != nil {
			return moved(err, "/args/sequence", "/data/sequence_ref")
		}
		number = issued.ObjectID
		return nil
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		now := s.Clock.Now()
		in, err := s.admitFormal(ctx, exec, h, d, f, &fx, now)
		if err != nil {
			return action.Outcome{}, err
		}
		if plan == nil || plan.BusinessRevision != in.revision.Number || plan.BusinessHash != in.revision.Seal.BusinessHash {
			return action.Outcome{}, errors.New("trd: the formalization was not prepared for this revision")
		}
		formal, err := plan.Formalize(number)
		if err != nil {
			return action.Outcome{}, err
		}
		file, b := formal.File, formal.Binding
		doc := quote.Document{Number: b.Number, Sequence: b.Sequence, BusinessRef: b.BusinessRef, BusinessHash: b.BusinessHash,
			Template: b.Template, TemplateHash: b.TemplateHash, RendererVersion: b.RendererVersion, FileName: file.Name, MimeType: file.MimeType,
			ContentSHA256: file.SHA256, SizeBytes: file.Size, Content: file.Content}
		var binding []quote.Entry
		for _, e := range b.Entries() {
			binding = append(binding, quote.Entry{Name: e.Name, Value: e.Value})
		}
		a := in.adoptable
		upstream := []quote.Entry{{Name: "cost_snapshot_id", Value: a.Ref.SnapshotID}, {Name: "cost_snapshot_result_hash", Value: a.ResultHash},
			{Name: "cost_snapshot_evidence_hash", Value: a.EvidenceHash}, {Name: "cost_snapshot_adoptability", Value: a.Status},
			{Name: "upstream_checked_at", Value: kernel.FormatTimestamp(now)}}
		rev, replaced, err := h.q.Formalize(quote.Formalize{Release: f.release, IdempotencyKey: f.key, Document: doc, Binding: binding,
			Upstream: upstream, CostSnapshotID: a.Ref.SnapshotID, ControlEpoch: h.q.Control.Epoch}, in.approval, d.Actor, d.Epoch, now)
		if err != nil {
			return action.Outcome{}, err
		}
		fields := map[string]any{"quote_id": h.q.ID, "business_revision": rev.Number, "business_hash": rev.Seal.BusinessHash,
			"approval_id": in.approval.ID, "number": doc.Number, "file_name": doc.FileName, "mime_type": doc.MimeType,
			"file_sha256": doc.ContentSHA256, "file_size": doc.SizeBytes, "template_ref": doc.Template.String(),
			"renderer_version": doc.RendererVersion, "cost_snapshot_id": a.Ref.SnapshotID, "expires_at": kernel.FormatTimestamp(rev.Seal.ExpiresAt)}
		eventType := api.EventQuoteActivated
		if replaced != nil {
			eventType, fields["replaced_revision"] = api.EventQuoteReplaced, replaced.Number
		}
		return h.save(ctx, exec, false, api.StatusActive, h.event(eventType, fields))
	}
	r, err := s.run(ctx, action.Command{Name: f.name, IdempotencyKey: f.key, ObjectID: f.quoteID}, req, validation, &preparedQuote{h})
	if err != nil {
		return api.FormalizationReceipt{}, err
	}
	return s.formalizationReceipt(ctx, r)
}

// admitted is what the checks of a formalization found: the approval that releases the revision,
// the revision and its commercial document, and CST's adoptability verdict on its cost snapshot.
type admitted struct {
	approval   *quote.Approval
	revision   *quote.Revision
	commercial quote.Commercial
	adoptable  cstapi.AdoptabilityVerdict
}

// admitFormal runs every check of an activation or a replacement on h.q at instant at, in the order
// the command refuses in: visibility and the object control, the root version, the conversion slot
// (replacement), Releasable, the sales exchange rate's age (fx; nil before the facts are read), the
// upstream guards. Prepare runs it on the root as read before the transaction, Apply on the root as
// locked.
func (s Service) admitFormal(ctx context.Context, exec bentopg.Executor, h *quoteCommand, d authorization.Decision, f formalCmd, fx *salesFX,
	at time.Time) (admitted, error) {
	if err := h.admit(ctx, d, false); err != nil {
		return admitted{}, err
	}
	if err := quote.Check(h.q, true, f.expected); err != nil {
		return admitted{}, err
	}
	if err := h.q.CheckConversion(f.release.Target); err != nil {
		return admitted{}, err
	}
	approval, _, err := s.Releasable(ctx, exec, h.q, f.release, at)
	if err != nil {
		return admitted{}, err
	}
	rev := h.q.Revision(f.release.Revision)
	c, err := quote.ParseCommercial(rev.Seal.Commercial, rev.Seal.BusinessHash)
	if err != nil {
		return admitted{}, err
	}
	if fx != nil {
		if err = fx.check(at); err != nil {
			return admitted{}, err
		}
	}
	adoptable, err := s.guardUpstream(ctx, exec, sealedGuards(rev, c), at)
	if err != nil {
		return admitted{}, err
	}
	return admitted{approval: approval, revision: rev, commercial: c, adoptable: adoptable}, nil
}

// salesFX is what a formalization re-checks of the sales exchange rate a revision was calculated
// with (Python _current_sales_fx): its observation instant and the pricing policy's maximum age,
// read from their pinned POL revisions before the transaction (ConfigGuard keeps them in force and
// at their content hashes in it). The zero value is a revision in the comparison currency.
type salesFX struct {
	present  bool
	observed time.Time
	maxAge   time.Duration
}

func (fx salesFX) check(at time.Time) error {
	if !fx.present {
		return nil
	}
	return quote.FreshFX(fx.observed, fx.maxAge, at, "/data/sales_fx_ref")
}

// documentFacts reads, before the transaction, what the formal document of commercial document c
// prints: from PTY the customer's name at the bound party revision and the contact's at the bound
// row version (audited reads, pty.read), from CAT each SKU's name at the bound specification
// revision (audited, cat.read), from POL the template, the currency and the trade terms at instant
// at; and, for a revision with a sales exchange rate, the rate's observation instant and the pricing
// policy's maximum age into fx. Unknown or hidden records are REFERENCE_INVALID, a contact moved on
// REFERENCE_STALE, at the commercial document's path.
func (s Service) documentFacts(ctx context.Context, c quote.Commercial, fx *salesFX, at time.Time) (document.Template, document.Facts, error) {
	d := c.Data
	var t document.Template
	var f document.Facts
	party, err := s.Upstream.Parties.GetParty(ctx, d.Customer.PartyID)
	if err != nil {
		return t, f, factErr(err, "/data/customer_ref")
	}
	for _, r := range party.Revisions {
		if r.Revision == d.Customer.Revision {
			f.Customer = document.Party{Ref: d.Customer, Name: r.Name}
		}
	}
	if f.Customer.Name == "" {
		return t, f, kernel.Reject(kernel.CodeReferenceInvalid, "/data/customer_ref", "The customer revision is unknown")
	}
	contact, err := s.Upstream.Parties.GetContact(ctx, d.Contact.ContactID)
	if err != nil {
		return t, f, factErr(err, "/data/contact_ref")
	}
	if contact.RowVersion != d.Contact.RowVersion || contact.PartyID != d.Customer.PartyID {
		return t, f, api.Reject(api.CodeReferenceStale, "/data/contact_ref")
	}
	f.Contact = document.Contact{Ref: d.Contact, Name: contact.Name}
	f.Items = map[quote.SkuRef]document.Item{}
	for i, l := range d.Lines {
		if _, done := f.Items[l.Sku]; done {
			continue
		}
		path := fmt.Sprintf("/data/lines/%d/sku_ref", i)
		sku, err := s.Upstream.Skus.GetSku(ctx, l.Sku.SkuID)
		if err != nil {
			return t, f, factErr(err, path)
		}
		i := slices.IndexFunc(sku.Revisions, func(r catapi.SpecRevision) bool { return r.Revision == l.Sku.SpecRevision })
		if i < 0 {
			return t, f, kernel.Reject(kernel.CodeReferenceInvalid, path, "The specification revision is unknown")
		}
		f.Items[l.Sku] = document.Item{Name: sku.Revisions[i].Name, Specification: fmt.Sprintf("%s r%d", l.Sku.SkuID, l.Sku.SpecRevision)}
	}
	refs := []quote.Ref{d.DocumentTemplate, d.Currency, d.TradeTerms}
	if d.SalesFX != nil {
		refs = append(refs, *d.SalesFX, d.PricingPolicy)
	}
	asked := make([]polapi.ConfigRef, len(refs))
	for i, r := range refs {
		asked[i] = polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ID, Revision: r.Revision}
	}
	got, err := s.Upstream.Configs.ResolveConfigs(ctx, asked, at)
	if err != nil {
		return t, f, err
	}
	data := func(r quote.Ref) polapi.ResolvedConfig {
		return got[polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ID, Revision: r.Revision}]
	}
	unknown := func(path string) error { return kernel.Reject(kernel.CodeReferenceInvalid, path, "") }
	rc := data(d.DocumentTemplate)
	qt := rc.Data.QuoteDocumentTemplate
	if qt == nil {
		return t, f, unknown("/data/document_template_ref")
	}
	t = document.Template{Ref: d.DocumentTemplate, ContentHash: rc.ContentHash, Language: qt.Language, RendererVersion: qt.RendererVersion,
		Title: qt.Title, Sections: slices.Clone(qt.Sections), FooterNote: qt.FooterNote}
	if rc = data(d.Currency); rc.Data.Currency == nil {
		return t, f, unknown("/data/currency_ref")
	}
	f.Currency = document.Currency{Ref: d.Currency, ContentHash: rc.ContentHash, MinorUnits: rc.Data.Currency.MinorUnits}
	if rc = data(d.TradeTerms); rc.Data.TradeTerms == nil {
		return t, f, unknown("/data/trade_terms_ref")
	}
	tt := rc.Data.TradeTerms
	f.TradeTerms = document.TradeTerms{Ref: d.TradeTerms, ContentHash: rc.ContentHash, Incoterm: tt.Incoterm, Edition: tt.Edition,
		ResponsibilityNote: tt.ResponsibilityNote, NamedPlace: tt.NamedPlace}
	if d.SalesFX != nil {
		rate, policy := data(*d.SalesFX).Data.FXRate, data(d.PricingPolicy).Data.PricingPolicy
		switch {
		case rate == nil:
			return t, f, unknown("/data/sales_fx_ref")
		case policy == nil:
			return t, f, unknown("/data/pricing_policy_ref")
		}
		observed, err := kernel.ParseTimestamp(rate.ObservedAt)
		if err != nil {
			return t, f, kernel.Reject(kernel.CodeReferenceInvalid, "/data/sales_fx_ref", "The rate has no observation instant")
		}
		*fx = salesFX{present: true, observed: observed, maxAge: time.Duration(policy.MaxFXAgeSeconds) * time.Second}
	}
	return t, f, nil
}

// factErr is an upstream's refusal of a read a formal document needs, at path of the commercial
// document: a record the actor does not see is REFERENCE_INVALID there; a missing permission stays
// PERMISSION_DENIED; anything else keeps its code.
func factErr(err error, path string) error {
	rej, ok := kernel.AsRejection(err)
	switch {
	case !ok:
		return err
	case rej.Code == kernel.CodeNotFound:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	case rej.Code == kernel.CodePermissionDenied:
		return err
	}
	return kernel.RejectAs(rej.Code, rej.Category, path, "")
}

// formalizationReceipt completes the receipt of an activation or replacement, executed or
// replayed, from the stored root.
func (s Service) formalizationReceipt(ctx context.Context, r api.Receipt) (api.FormalizationReceipt, error) {
	out := api.FormalizationReceipt{Receipt: r}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), r.ObjectID, postgres.NoLock)
		if err != nil || q == nil {
			return err
		}
		if rev := q.FormalizedAt(r.RowVersion); rev != nil {
			d, c := rev.Formal.Document, rev.Formal.Commit
			out.BusinessRevision, out.BusinessHash, out.ApprovalID, out.ReplacedRevision = rev.Number, rev.Seal.BusinessHash, c.ApprovalID, c.Replaced
			out.Number, out.FileName, out.MimeType, out.DocumentSHA256, out.SizeBytes = d.Number, d.FileName, d.MimeType, d.ContentSHA256, d.SizeBytes
		}
		return nil
	})
	return out, err
}
