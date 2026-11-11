package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/document"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Reads of formal revisions (WBS S3-04; TRD-INV-009: the stored bytes are checked against their
// binding on every read): the audited GetFormalDocument and GetActiveRevision, and the lookups the
// steps relying on a formal revision ask before their transaction (api.Lookups).

// GetFormalDocument returns the formal document of a revision with its bytes, audited as
// quote.document.get (trd.read): NOT_FOUND for a quote the caller does not see or an unknown
// revision; document.Verify's codes for a revision without its own binding or with bytes that are
// not the bound ones.
func (s Service) GetFormalDocument(ctx context.Context, quoteID string, revision int64) (api.FormalDocument, error) {
	var v api.FormalDocument
	err := s.Ledger.Read(ctx, CommandGetFormalDocument, quoteID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), quoteID, postgres.NoLock)
		if err != nil {
			return err
		}
		if q == nil || !d.Sees(q.OwnerActor, q.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		rev := q.Revision(revision)
		if rev == nil || rev.Seal == nil {
			return kernel.Reject(kernel.CodeNotFound, "/business_revision", "")
		}
		content, err := s.verifiedContent(ctx, exec, q, rev) // DOCUMENT_BINDING_MISSING for a revision never formalized
		if err != nil {
			return err
		}
		v = api.FormalDocument{QuoteID: q.ID, BusinessRevision: rev.Number, BusinessHash: rev.Seal.BusinessHash, Status: string(rev.Status),
			Content: content}
		if f := formalView(rev.Formal); f != nil {
			v.Formal = *f
		}
		return nil
	})
	return v, err
}

// GetActiveRevision returns the root's active revision as the lookups describe it, audited as
// quote.active.get (trd.read); NO_ACTIVE_REVISION when the root has none.
func (s Service) GetActiveRevision(ctx context.Context, quoteID string) (api.FormalRevision, error) {
	var v api.FormalRevision
	err := s.Ledger.Read(ctx, CommandGetActiveRevision, quoteID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), quoteID, postgres.NoLock)
		if err != nil {
			return err
		}
		if q == nil || !d.Sees(q.OwnerActor, q.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		active := q.Active()
		if active == nil {
			return api.Reject(api.CodeNoActiveRevision, "/id")
		}
		v, err = s.formalRevision(ctx, exec, q, active)
		return err
	})
	return v, err
}

// ResolveQuoteRevisions answers QuoteRevisionRef lookups (api.Lookups): only an active grant is
// needed; quotes outside the caller's data scope and revisions never formalized are absent.
func (s Service) ResolveQuoteRevisions(ctx context.Context, refs []api.QuoteRevisionRef) (map[api.QuoteRevisionRef]api.FormalRevision, error) {
	if err := api.ValidateRevisionRefs(refs); err != nil {
		return nil, err
	}
	out := map[api.QuoteRevisionRef]api.FormalRevision{}
	err := s.lookupQuotes(ctx, func(load func(id string) (*quote.Quote, error), resolve func(q *quote.Quote, r *quote.Revision) (api.FormalRevision, error)) error {
		for _, ref := range refs {
			q, err := load(ref.QuoteID)
			if err != nil {
				return err
			}
			if q == nil {
				continue
			}
			if rev := q.Revision(ref.BusinessRevision); rev != nil && rev.Formal != nil {
				if out[ref], err = resolve(q, rev); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return out, err
}

// ResolveActiveRevisions returns the active revision of each quote the caller sees that has one
// (api.Lookups).
func (s Service) ResolveActiveRevisions(ctx context.Context, quoteIDs []string) (map[string]api.FormalRevision, error) {
	if err := api.ValidateQuoteIDs(quoteIDs); err != nil {
		return nil, err
	}
	out := map[string]api.FormalRevision{}
	err := s.lookupQuotes(ctx, func(load func(id string) (*quote.Quote, error), resolve func(q *quote.Quote, r *quote.Revision) (api.FormalRevision, error)) error {
		for _, id := range quoteIDs {
			q, err := load(id)
			if err != nil {
				return err
			}
			if q == nil {
				continue
			}
			if active := q.Active(); active != nil {
				if out[id], err = resolve(q, active); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return out, err
}

// lookupQuotes runs fn in a lookup transaction with a loader of the quotes the caller sees (nil
// otherwise), each loaded once, and the resolver of a formal revision.
func (s Service) lookupQuotes(ctx context.Context, fn func(load func(id string) (*quote.Quote, error),
	resolve func(q *quote.Quote, r *quote.Revision) (api.FormalRevision, error)) error) error {
	return s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		loaded := map[string]*quote.Quote{}
		load := func(id string) (*quote.Quote, error) {
			if q, ok := loaded[id]; ok {
				return q, nil
			}
			q, err := postgres.LoadQuote(ctx, exec, scope(ctx), id, postgres.NoLock)
			if err != nil {
				return nil, err
			}
			if q != nil && !d.Sees(q.OwnerActor, q.OwnerTeam) {
				q = nil
			}
			loaded[id] = q
			return q, nil
		}
		return fn(load, func(q *quote.Quote, r *quote.Revision) (api.FormalRevision, error) {
			return s.formalRevision(ctx, exec, q, r)
		})
	})
}

// formalRevision is what TRD says of formal revision r of q now: Code is its document's integrity
// refusal, else quote.Usable's (QUOTE_NOT_CURRENT, the hold's code), else empty.
func (s Service) formalRevision(ctx context.Context, exec bentopg.Executor, q *quote.Quote, r *quote.Revision) (api.FormalRevision, error) {
	v := api.FormalRevision{Ref: api.QuoteRevisionRef{QuoteID: q.ID, BusinessRevision: r.Number}, Status: string(r.Status),
		Active: r.Status == quote.Active, ConversionTaken: q.Slot != nil}
	if r.Seal != nil {
		v.BusinessHash, v.IssuedAt, v.ExpiresAt = r.Seal.BusinessHash, r.Seal.IssuedAt, r.Seal.ExpiresAt
	}
	if f := r.Formal; f != nil {
		v.Number, v.DocumentSHA256, v.ApprovalID = f.Document.Number, f.Document.ContentSHA256, f.Commit.ApprovalID
	}
	if h := q.Hold(r.Number); h != nil {
		v.OnHold, v.HoldCode = true, h.Code
	}
	_, err := s.verifiedContent(ctx, exec, q, r)
	if err == nil {
		err = q.Usable(r.Number, "", "/")
	}
	if rej, ok := kernel.AsRejection(err); ok {
		v.Code, err = rej.Code, nil
	}
	return v, err
}

// verifiedContent reads the stored bytes of r's formal document and checks them against its binding
// (document.Verify, TRD-INV-009): a revision without a binding of its own is
// DOCUMENT_BINDING_MISSING, bytes of another size or hash FORMAL_DOCUMENT_INTEGRITY_FAILURE.
func (s Service) verifiedContent(ctx context.Context, exec bentopg.Executor, q *quote.Quote, r *quote.Revision) ([]byte, error) {
	if r.Formal == nil || r.Seal == nil {
		return nil, document.Verify(nil, nil, q.ID, r.Number, "")
	}
	content, _, err := postgres.LoadDocumentContent(ctx, exec, scope(ctx), q.ID, r.Number)
	if err != nil {
		return nil, err
	}
	return content, verifyDocument(q.ID, r, content)
}

// verifyDocument checks content against the binding of revision r of quote id.
func verifyDocument(id string, r *quote.Revision, content []byte) error {
	var b *document.Binding
	if f := r.Formal; f != nil {
		d := f.Document
		b = &document.Binding{QuoteID: id, BusinessRevision: r.Number, BusinessHash: d.BusinessHash, Number: d.Number, Sequence: d.Sequence,
			BusinessRef: d.BusinessRef, Template: d.Template, TemplateHash: d.TemplateHash, RendererVersion: d.RendererVersion,
			FileName: d.FileName, MimeType: d.MimeType, ContentSHA256: d.ContentSHA256, SizeBytes: d.SizeBytes}
	}
	hash := ""
	if r.Seal != nil {
		hash = r.Seal.BusinessHash
	}
	return document.Verify(b, content, id, r.Number, hash)
}

// formalView is a formalization as the reads show it, nil for none.
func formalView(f *quote.Formalization) *api.FormalView {
	if f == nil {
		return nil
	}
	d, c := f.Document, f.Commit
	entries, err := c.EntriesJSON()
	sum := ""
	if err == nil {
		sum = quote.Hash(entries)
	}
	return &api.FormalView{Number: d.Number, SequenceRef: apiRef(d.Sequence), BusinessRef: d.BusinessRef, TemplateRef: apiRef(d.Template),
		TemplateHash: d.TemplateHash, RendererVersion: d.RendererVersion, FileName: d.FileName, MimeType: d.MimeType,
		DocumentSHA256: d.ContentSHA256, SizeBytes: d.SizeBytes, Commit: api.CommitView{Command: c.Command, ApprovalID: c.ApprovalID,
			EffectivityVersion: c.EffectivityVersion, ReplacedRevision: c.Replaced, CostSnapshotID: c.CostSnapshotID,
			AuthorityEpoch: c.AuthorityEpoch, ControlEpoch: c.ControlEpoch, RootVersion: c.RootVersion, CommittedBy: c.By, CommittedAt: c.At,
			EntriesSHA256: sum}}
}
