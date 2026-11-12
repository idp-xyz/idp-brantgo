package application

import (
	"context"
	"slices"
	"strings"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The histories of a quote root (WBS S3-09; Python T03 A9–A13; api/queries.go): paged, ordered,
// payload-free projections of the aggregate as stored, and of the events TRD enqueued for it in
// the Outbox. Each is an audited read under trd.read of a quote the caller sees now.

// page returns the entries of all whose position (pos) is after p.After, at most p.Size() of them,
// and the next page's After (0 when none follows).
func page[T any](all []T, p api.Page, pos func(T) int64) ([]T, int64) {
	var out []T
	for _, e := range all {
		if pos(e) > p.After {
			out = append(out, e)
		}
	}
	if len(out) <= p.Size() {
		return out, 0
	}
	out = out[:p.Size()]
	return out, pos(out[len(out)-1])
}

// QuoteHistory returns a page of the quote's revisions in revision order, audited as quote.history
// (Python T03 A9 quote_history).
func (s Service) QuoteHistory(ctx context.Context, hq api.HistoryQuery) (api.QuoteHistoryPage, error) {
	var out api.QuoteHistoryPage
	err := s.readQuote(ctx, api.QueryQuoteHistory, hq.QuoteID, hq.Validate(),
		func(ctx context.Context, exec bentopg.Executor, d authorization.Decision, q *quote.Quote) error {
			out = api.QuoteHistoryPage{QuoteID: q.ID, RootVersion: q.RootVersion, LastRevision: q.LastRevision, Revisions: []api.RevisionHistory{}}
			if w := q.Working(); w != nil {
				out.WorkingRevision = w.Number
			}
			if a := q.Active(); a != nil {
				out.ActiveRevision = a.Number
			}
			revs, next := page(q.Revisions, hq.Page, func(r *quote.Revision) int64 { return r.Number })
			for _, r := range revs {
				h, err := revisionHistory(q, r)
				if err != nil {
					return err
				}
				out.Revisions = append(out.Revisions, h)
			}
			out.Next = next
			return nil
		})
	return out, err
}

// revisionHistory is revision r of q without its content, with the steps it went through.
func revisionHistory(q *quote.Quote, r *quote.Revision) (api.RevisionHistory, error) {
	h := api.RevisionHistory{BusinessRevision: r.Number, Status: string(r.Status), BaseRevision: r.Base, Events: []api.HistoryEvent{}}
	var events []api.HistoryEvent
	step := func(event string, a quote.Act, rootVersion, effectivity int64) {
		events = append(events, api.HistoryEvent{Event: event, By: a.By, At: a.At, RootVersion: rootVersion, EffectivityVersion: effectivity})
	}
	step(api.EventSaved, quote.Act{By: r.SavedBy, At: r.SavedAt}, 0, 0)
	if e := r.End; e != nil {
		step(api.EventDiscarded, quote.Act{By: e.By, At: e.At}, 0, 0)
	}
	if sl := r.Seal; sl != nil {
		h.BusinessHash, h.IssuedAt, h.ExpiresAt = sl.BusinessHash, sl.IssuedAt, sl.ExpiresAt
		step(api.EventFrozen, quote.Act{By: sl.By, At: sl.At}, sl.RootVersion, 0)
		c, err := r.Calculation()
		if err != nil {
			return api.RevisionHistory{}, err
		}
		if c != nil {
			h.Calculation = &api.CalculationSummary{NetMinor: c.NetMinor, TaxMinor: c.TaxMinor, PayableMinor: c.PayableMinor,
				ComparisonRevenueMinor: c.ComparisonRevenueMinor, CostMinor: c.CostMinor, ProfitMinor: c.ProfitMinor, MarginRule: c.MarginRule}
		}
	}
	for _, rv := range q.Reviews {
		if rv.Revision != r.Number {
			continue
		}
		step(api.EventReviewRequested, rv.Requested, 0, 0)
		if rv.Decided != nil {
			step(reviewEvent(rv.Status), *rv.Decided, 0, 0)
		}
	}
	for _, a := range q.Approvals {
		if a.Revision != r.Number {
			continue
		}
		if e := a.Exception; e != nil {
			step(api.EventMarginExceptionApproved, e.Approved, 0, 1)
		}
		if a.Revocation != nil {
			step(api.EventApprovalRevoked, *a.Revocation, 0, a.EffectivityVersion)
		}
	}
	if f := r.Formal; f != nil {
		h.Number = f.Document.Number
		step(formalEvent(f.Commit.Command), quote.Act{By: f.Commit.By, At: f.Commit.At}, f.Commit.RootVersion, 0)
	}
	for _, o := range q.Revisions {
		if f := o.Formal; f != nil && f.Commit.Replaced == r.Number {
			step(api.EventSuperseded, quote.Act{By: f.Commit.By, At: f.Commit.At}, f.Commit.RootVersion, 0)
		}
	}
	if hold := q.Hold(r.Number); hold != nil {
		step(api.EventRiskHeld, hold.Registered, hold.RootVersion, 0)
	}
	if c := r.Closed; c != nil {
		event := api.EventWithdrawn
		if c.Status == quote.Cancelled {
			event = api.EventCancelled
		}
		step(event, c.Closed, c.RootVersion, 0)
	}
	if sl := q.Slot; sl != nil && sl.Revision == r.Number {
		step(api.EventConversionSlotTaken, sl.Taken, sl.RootVersion, 0)
	}
	slices.SortStableFunc(events, func(a, b api.HistoryEvent) int { return a.At.Compare(b.At) })
	h.Events = append(h.Events, events...)
	return h, nil
}

func reviewEvent(s quote.ReviewStatus) string {
	switch s {
	case quote.ReviewApproved:
		return api.EventReviewApproved
	case quote.ReviewReturned:
		return api.EventReviewReturned
	case quote.ReviewRejected:
		return api.EventReviewRejected
	}
	return api.EventReviewCancelled
}

func formalEvent(command string) string {
	if command == api.CommandReplace {
		return api.EventReplaced
	}
	return api.EventActivated
}

// ApprovalHistory returns a page of the quote's review requests in request order with the
// approvals they gave, audited as quote.approval.history (Python T03 A10 approval_history).
func (s Service) ApprovalHistory(ctx context.Context, hq api.HistoryQuery) (api.ApprovalHistoryPage, error) {
	var out api.ApprovalHistoryPage
	err := s.readQuote(ctx, api.QueryApprovalHistory, hq.QuoteID, hq.Validate(),
		func(ctx context.Context, exec bentopg.Executor, d authorization.Decision, q *quote.Quote) error {
			out = api.ApprovalHistoryPage{QuoteID: q.ID, Reviews: []api.ReviewHistory{}}
			all := make([]api.ReviewHistory, len(q.Reviews))
			for i, rv := range q.Reviews {
				all[i] = reviewHistory(q, rv, int64(i+1))
			}
			out.Reviews, out.Next = page(all, hq.Page, func(r api.ReviewHistory) int64 { return r.Seq })
			if out.Reviews == nil {
				out.Reviews = []api.ReviewHistory{}
			}
			return nil
		})
	return out, err
}

// reviewHistory is review rv of q at position seq, with its approval's effectivity and steps.
func reviewHistory(q *quote.Quote, rv *quote.Review, seq int64) api.ReviewHistory {
	h := api.ReviewHistory{Seq: seq, ReviewRequestID: rv.ID, BusinessRevision: rv.Revision, BusinessHash: rv.BusinessHash,
		TargetCommand: rv.Target, ExpectedActiveRevision: rv.ExpectedActive, ReviewStatus: string(rv.Status),
		MarginExceptionRequired: rv.MarginExceptionRequired, Events: []api.HistoryEvent{}}
	step := func(event string, a quote.Act, effectivity int64) {
		h.Events = append(h.Events, api.HistoryEvent{Event: event, By: a.By, At: a.At, EffectivityVersion: effectivity})
	}
	step(api.EventReviewRequested, rv.Requested, 0)
	var approval *quote.Approval
	for _, a := range q.Approvals {
		if a.ReviewID == rv.ID {
			approval = a
		}
	}
	if rv.Decided != nil {
		effectivity := int64(0)
		if approval != nil {
			effectivity = 1
		}
		step(reviewEvent(rv.Status), *rv.Decided, effectivity)
	}
	if a := approval; a != nil {
		h.ApprovalID, h.ApprovalExpiresAt, h.Effectivity, h.EffectivityVersion = a.ID, a.ExpiresAt, string(a.Effectivity), a.EffectivityVersion
		h.MarginExceptionApproved = a.Exception != nil
		// The exception comes with the approval or in a second decision later (ES-02 step 5).
		var later []api.HistoryEvent
		if e := a.Exception; e != nil {
			later = append(later, api.HistoryEvent{Event: api.EventMarginExceptionApproved, By: e.Approved.By, At: e.Approved.At,
				EffectivityVersion: 1})
		}
		if r := a.Revocation; r != nil {
			later = append(later, api.HistoryEvent{Event: api.EventApprovalRevoked, By: r.By, At: r.At, EffectivityVersion: a.EffectivityVersion})
		}
		slices.SortStableFunc(later, func(x, y api.HistoryEvent) int { return x.At.Compare(y.At) })
		h.Events = append(h.Events, later...)
	}
	return h
}

// formals are q's formalized revisions in revision order.
func formals(q *quote.Quote) []*quote.Revision {
	var out []*quote.Revision
	for _, r := range q.Revisions {
		if r.Formal != nil && r.Seal != nil {
			out = append(out, r)
		}
	}
	return out
}

// DocumentHistory returns a page of the quote's formal documents in revision order with their
// integrity now, audited as quote.document.history (Python T03 A11 document_history).
func (s Service) DocumentHistory(ctx context.Context, hq api.HistoryQuery) (api.DocumentHistoryPage, error) {
	var out api.DocumentHistoryPage
	err := s.readQuote(ctx, api.QueryDocumentHistory, hq.QuoteID, hq.Validate(),
		func(ctx context.Context, exec bentopg.Executor, d authorization.Decision, q *quote.Quote) error {
			out = api.DocumentHistoryPage{QuoteID: q.ID, Documents: []api.DocumentHistory{}}
			revs, next := page(formals(q), hq.Page, func(r *quote.Revision) int64 { return r.Number })
			for _, r := range revs {
				doc, c := r.Formal.Document, r.Formal.Commit
				h := api.DocumentHistory{BusinessRevision: r.Number, BusinessHash: doc.BusinessHash, RevisionStatus: string(r.Status),
					Number: doc.Number, SequenceRef: apiRef(doc.Sequence), BusinessRef: doc.BusinessRef, TemplateRef: apiRef(doc.Template),
					TemplateHash: doc.TemplateHash, RendererVersion: doc.RendererVersion, FileName: doc.FileName, MimeType: doc.MimeType,
					DocumentSHA256: doc.ContentSHA256, SizeBytes: doc.SizeBytes, FormalizedBy: c.By, FormalizedAt: c.At, Integrity: api.DocumentIntact}
				// The bytes are checked as every read of the document checks them (TRD-INV-009); a
				// failure is the document's integrity, not the history's.
				if _, err := s.verifiedContent(ctx, exec, q, r); err != nil {
					rej, ok := kernel.AsRejection(err)
					if !ok {
						return err
					}
					h.Integrity = rej.Code
				}
				out.Documents = append(out.Documents, h)
			}
			out.Next = next
			return nil
		})
	return out, err
}

// CommitHistory returns a page of the quote's commit evidence in revision order, audited as
// quote.commit.history (Python T03 A13 commit_history).
func (s Service) CommitHistory(ctx context.Context, hq api.HistoryQuery) (api.CommitHistoryPage, error) {
	var out api.CommitHistoryPage
	err := s.readQuote(ctx, api.QueryCommitHistory, hq.QuoteID, hq.Validate(),
		func(ctx context.Context, exec bentopg.Executor, d authorization.Decision, q *quote.Quote) error {
			out = api.CommitHistoryPage{QuoteID: q.ID, Commits: []api.CommitHistory{}}
			revs, next := page(formals(q), hq.Page, func(r *quote.Revision) int64 { return r.Number })
			for _, r := range revs {
				c, doc := r.Formal.Commit, r.Formal.Document
				entries, err := c.EntriesJSON()
				if err != nil {
					return err
				}
				h := api.CommitHistory{BusinessRevision: r.Number, BusinessHash: r.Seal.BusinessHash, Command: c.Command, ApprovalID: c.ApprovalID,
					EffectivityVersion: c.EffectivityVersion, ReplacedRevision: c.Replaced, CostSnapshotID: c.CostSnapshotID,
					HasCostQualification: slices.ContainsFunc(c.Entries, func(e quote.Entry) bool { return e.Name == "cost_snapshot_adoptability" }),
					AuthorityEpoch:       c.AuthorityEpoch, ControlEpoch: c.ControlEpoch, RootVersion: c.RootVersion, CommittedBy: c.By,
					CommittedAt: c.At, Number: doc.Number, DocumentSHA256: doc.ContentSHA256, EntriesSHA256: quote.Hash(entries)}
				act, err := postgres.LoadAction(ctx, exec, scope(ctx), c.Command, c.IdempotencyKey)
				if err != nil {
					return err
				}
				if act != nil {
					h.ActionID = act.ActionID
				}
				out.Commits = append(out.Commits, h)
			}
			out.Next = next
			return nil
		})
	return out, err
}

// OutboxHistory returns a page of the events TRD enqueued for the quote root, its customer
// responses and their acceptances, in enqueue order, audited as quote.outbox.history (Python T03
// A12 outbox_history). The events are read from the Outbox itself by partition key: TRD
// partitions its events by scope and aggregate (QuoteRoot, CustomerResponse, Acceptance).
func (s Service) OutboxHistory(ctx context.Context, hq api.HistoryQuery) (api.OutboxHistoryPage, error) {
	var out api.OutboxHistoryPage
	err := s.readQuote(ctx, api.QueryOutboxHistory, hq.QuoteID, hq.Validate(),
		func(ctx context.Context, exec bentopg.Executor, d authorization.Decision, q *quote.Quote) error {
			sc := scope(ctx).String()
			keys := []string{sc + "/QuoteRoot:" + q.ID}
			responses, err := postgres.LoadResponseIDs(ctx, exec, scope(ctx), q.ID)
			if err != nil {
				return err
			}
			for _, id := range responses {
				keys = append(keys, sc+"/CustomerResponse:"+id)
			}
			accs, err := postgres.LoadAcceptances(ctx, exec, scope(ctx), postgres.AcceptanceFilter{QuoteID: q.ID}, postgres.NoLock)
			if err != nil {
				return err
			}
			for _, a := range accs {
				keys = append(keys, sc+"/Acceptance:"+a.ID)
			}
			events, err := postgres.LoadOutbox(ctx, exec, s.Ledger.DB.Schema(), api.Source, keys, hq.After, hq.Size()+1)
			if err != nil {
				return err
			}
			out = api.OutboxHistoryPage{QuoteID: q.ID, Events: []api.OutboxEntry{}}
			if len(events) > hq.Size() {
				events = events[:hq.Size()]
				out.Next = events[len(events)-1].Seq
			}
			for _, e := range events {
				entry := api.OutboxEntry{Seq: e.Seq, EventID: e.EventID, EventType: e.EventType, AggregateType: e.AggregateType,
					AggregateID: e.AggregateID, AggregateVersion: e.AggregateVersion, BusinessRevision: e.BusinessRevision, ActionID: e.ActionID,
					OccurredAt: e.OccurredAt, PayloadSHA256: e.PayloadSHA256, RelayStatus: strings.ToLower(e.Status), Attempts: e.Attempts,
					ExternalDelivery: api.ExternalDeliveryNotStarted}
				if e.PublishedAt != nil {
					entry.PublishedAt = kernel.Instant(*e.PublishedAt)
				}
				if e.AbandonedAt != nil {
					entry.AbandonedAt = kernel.Instant(*e.AbandonedAt)
				}
				out.Events = append(out.Events, entry)
			}
			return nil
		})
	return out, err
}
