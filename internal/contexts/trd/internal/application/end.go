package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
)

// quote.withdraw and quote.cancel (human; WBS S3-05; SM-TRD-REVISION; Python t03_storage withdraw
// and cancel). Each locks the quote root FOR UPDATE — the row activation, replacement, approval
// revocation and the order step taking the conversion slot lock first — so a cancellation and a
// conversion, or a withdrawal and an activation, have one winner. Both need a reason and new
// evidence the actor reads (REFERENCE_INVALID otherwise), and keep everything the revision had: its
// commercial document, approvals, formal document and commit evidence.

// Withdraw ends a frozen or pending revision before it takes effect, cancelling an open review of
// it (quote.withdraw).
func (s Service) Withdraw(ctx context.Context, cmd api.Withdraw) (api.RevisionReceipt, error) {
	return s.endRevision(ctx, api.CommandWithdraw, cmd.IdempotencyKey, cmd.QuoteID, cmd.Reason, cmd.Evidence, cmd, cmd.Validate(),
		func(h *quoteCommand, d authorization.Decision, proofs []quote.Evidence) (*quote.Revision, action.Event, error) {
			if err := quote.Check(h.q, true, cmd.ExpectedVersion); err != nil {
				return nil, action.Event{}, err
			}
			rev, review, err := h.q.Withdraw(cmd.BusinessRevision, d.Actor, d.Epoch, s.Clock.Now(), cmd.Reason, proofs)
			if err != nil {
				return nil, action.Event{}, err
			}
			fields := map[string]any{"quote_id": h.q.ID, "business_revision": rev.Number, "business_hash": rev.Seal.BusinessHash}
			if review != nil {
				fields["review_request_id"] = review.ID
			}
			return rev, h.event(api.EventQuoteWithdrawn, fields), nil
		})
}

// Cancel ends the active revision (quote.cancel).
func (s Service) Cancel(ctx context.Context, cmd api.Cancel) (api.RevisionReceipt, error) {
	return s.endRevision(ctx, api.CommandCancel, cmd.IdempotencyKey, cmd.QuoteID, cmd.Reason, cmd.Evidence, cmd, cmd.Validate(),
		func(h *quoteCommand, d authorization.Decision, proofs []quote.Evidence) (*quote.Revision, action.Event, error) {
			if err := quote.Check(h.q, true, cmd.ExpectedVersion); err != nil {
				return nil, action.Event{}, err
			}
			rev, err := h.q.Cancel(cmd.BusinessRevision, d.Actor, d.Epoch, s.Clock.Now(), cmd.Reason, proofs)
			if err != nil {
				return nil, action.Event{}, err
			}
			return rev, h.event(api.EventQuoteCancelled, map[string]any{"quote_id": h.q.ID, "business_revision": rev.Number,
				"business_hash": rev.Seal.BusinessHash, "number": rev.Formal.Document.Number}), nil
		})
}

// endRevision runs a withdrawal or cancellation: the evidence's sources are read before the transaction;
// in it, visibility and the object control, then end (the root version and the domain's rules),
// then the evidence.
func (s Service) endRevision(ctx context.Context, name, key, quoteID, reason string, proofs []api.Evidence, req api.Command, validation error,
	end func(h *quoteCommand, d authorization.Decision, proofs []quote.Evidence) (*quote.Revision, action.Event, error)) (api.RevisionReceipt, error) {
	h := &quoteCommand{s: s, id: quoteID}
	evidenced := evidence(proofs)
	var readable map[string]bool
	h.prepare = func(ctx context.Context) error {
		ids := make([]string, len(evidenced))
		for i, e := range evidenced {
			ids[i] = e.SourceID
		}
		found, err := s.sources(ctx, ids)
		readable = readableSet(found)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, false); err != nil {
			return action.Outcome{}, err
		}
		rev, event, err := end(h, d, evidenced)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = quote.CheckDraft(quote.Data{Evidence: evidenced}, readable); err != nil {
			return action.Outcome{}, moved(err, "/args/data/evidence", "/args/evidence")
		}
		return h.save(ctx, exec, false, string(rev.Status), event)
	}
	r, err := s.run(ctx, action.Command{Name: name, IdempotencyKey: key, ObjectID: quoteID, Reason: reason}, req, validation, &preparedQuote{h})
	if err != nil {
		return api.RevisionReceipt{}, err
	}
	out := api.RevisionReceipt{Receipt: r}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), r.ObjectID, postgres.NoLock)
		if err == nil && q != nil {
			if rev := q.ClosedAt(r.RowVersion); rev != nil {
				out.BusinessRevision, out.BusinessHash = rev.Number, rev.Seal.BusinessHash
			}
		}
		return err
	})
	return out, err
}
