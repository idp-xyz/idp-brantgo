package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The review commands (WBS S3-03, S3-14; api/review.go): quote.submit_review, quote.review.decide
// and quote.approval.revoke, human only. Each locks the quote root FOR UPDATE first and then, through
// the ledger's Authorize, the company's authority guard row FOR SHARE; quote.review.decide also
// reads the decider's own grant again (Authorizer.Qualify, the same rows) for trd.read and the
// margin exception capability. The review requests, approvals and margin exceptions are rows of
// the root (brant_trd.quote_review, quote_approval, quote_margin_exception) written in the root's
// transaction, so concurrent decisions on one review have one winner: the root lock and its version
// CAS serialize them.

// SubmitReview opens a review request (frozen → pending_approval).
func (s Service) SubmitReview(ctx context.Context, cmd api.SubmitReview) (api.ReviewReceipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, false); err != nil {
			return action.Outcome{}, err
		}
		if err := quote.Check(h.q, true, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		r, err := h.q.SubmitReview(quote.ReviewRequest{ID: cmd.ReviewRequestID, Revision: cmd.BusinessRevision, Target: cmd.TargetCommand,
			ExpectedActive: cmd.ExpectedActiveRevision}, d.Actor, d.Epoch, s.Clock.Now())
		if err != nil {
			return action.Outcome{}, err
		}
		fields := map[string]any{"quote_id": h.q.ID, "business_revision": r.Revision, "business_hash": r.BusinessHash,
			"review_request_id": r.ID, "target_command": r.Target, "margin_exception_required": r.MarginExceptionRequired}
		if r.ExpectedActive > 0 {
			fields["expected_active_revision"] = r.ExpectedActive
		}
		return h.save(ctx, exec, false, api.StatusPendingApproval, h.event(api.EventQuoteReviewRequested, fields))
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandSubmitReview, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.ReviewReceipt{}, err
	}
	return s.reviewReceipt(ctx, res, cmd.ReviewRequestID)
}

// DecideReview decides a review request: approve (with or without the margin exception), return
// or reject; or adds the margin exception to its approval.
func (s Service) DecideReview(ctx context.Context, cmd api.DecideReview) (api.ReviewReceipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	decision := quote.ReviewDecision{ReviewID: cmd.ReviewRequestID, BusinessHash: cmd.BusinessHash, Decision: cmd.Decision,
		ApprovalID: cmd.ApprovalID, Reason: cmd.Reason, Evidence: evidence(cmd.Evidence)}
	if t, err := kernel.ParseTimestamp(cmd.ExpiresAt); err == nil {
		decision.ExpiresAt = t
	}
	var sources []string
	for _, e := range cmd.Evidence {
		sources = append(sources, e.SourceID)
	}
	if m := cmd.MarginException; m != nil {
		minimum, _ := kernel.ParseDecimal(m.MinimumMargin, kernel.FactorFormat) // Validate refuses what does not parse
		shortfall, _ := kernel.ParseDecimal(m.Shortfall, api.ShortfallFormat)
		decision.Exception = &quote.ExceptionClaim{PricingPolicy: ref(m.PricingPolicyRef), MinimumMargin: minimum, Shortfall: shortfall,
			Reason: m.Reason, Evidence: evidence(m.Evidence)}
		for _, e := range m.Evidence {
			sources = append(sources, e.SourceID)
		}
	}
	var readable map[string]bool
	h.prepare = func(ctx context.Context) error {
		found, err := s.sources(ctx, sources)
		readable = readableSet(found)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, false); err != nil {
			return action.Outcome{}, err
		}
		if err := quote.Check(h.q, true, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		own, err := s.Ledger.Authorizer.Qualify(ctx, exec, d.Actor.ID)
		if err != nil {
			return action.Outcome{}, err
		}
		if !own.Holds(quote.PermissionRead) {
			// A reviewer reads what it approves now, as it must later (T03 A33).
			return action.Outcome{}, kernel.Reject(kernel.CodePermissionDenied, "/", "")
		}
		review, approval, err := h.q.Decide(decision, quote.Decider{Actor: d.Actor, Epoch: d.Epoch,
			MarginException: own.Holds(quote.PermissionMarginException)}, s.Clock.Now())
		if err != nil {
			return action.Outcome{}, err
		}
		if err = checkEvidence(cmd, readable); err != nil {
			return action.Outcome{}, err
		}
		fields := map[string]any{"quote_id": h.q.ID, "business_revision": review.Revision, "business_hash": review.BusinessHash,
			"review_request_id": review.ID, "decision": cmd.Decision, "review_status": string(review.Status),
			"margin_exception_required": review.MarginExceptionRequired, "margin_exception_approved": false}
		if approval != nil {
			fields["approval_id"], fields["effectivity_version"] = approval.ID, approval.EffectivityVersion
			fields["expires_at"] = kernel.FormatTimestamp(approval.ExpiresAt)
			fields["margin_exception_approved"] = approval.Exception != nil
		}
		return h.save(ctx, exec, false, string(review.Status), h.event(api.EventQuoteReviewDecided, fields))
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandDecideReview, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), &preparedQuote{h})
	if err != nil {
		return api.ReviewReceipt{}, err
	}
	return s.reviewReceipt(ctx, res, cmd.ReviewRequestID)
}

// checkEvidence refuses the first evidence entry of a decision whose source the decider does not
// read (REFERENCE_INVALID).
func checkEvidence(cmd api.DecideReview, readable map[string]bool) error {
	if err := quote.CheckDraft(quote.Data{Evidence: evidence(cmd.Evidence)}, readable); err != nil {
		return moved(err, "/args/data/evidence", "/args/evidence")
	}
	if m := cmd.MarginException; m != nil {
		if err := quote.CheckDraft(quote.Data{Evidence: evidence(m.Evidence)}, readable); err != nil {
			return moved(err, "/args/data/evidence", "/args/margin_exception/evidence")
		}
	}
	return nil
}

// RevokeApproval revokes an approval (valid → revoked) at its effectivity version.
func (s Service) RevokeApproval(ctx context.Context, cmd api.RevokeApproval) (api.ReviewReceipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	proofs := evidence(cmd.Evidence)
	var readable map[string]bool
	h.prepare = func(ctx context.Context) error {
		ids := make([]string, len(proofs))
		for i, e := range proofs {
			ids[i] = e.SourceID
		}
		found, err := s.sources(ctx, ids)
		readable = readableSet(found)
		return err
	}
	var reviewID string
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, false); err != nil {
			return action.Outcome{}, err
		}
		a, hold, err := h.q.Revoke(quote.Revocation{ApprovalID: cmd.ApprovalID, ExpectedEffectivity: cmd.ExpectedEffectivityVersion,
			Reason: cmd.Reason, Evidence: proofs}, d.Actor, d.Epoch, s.Clock.Now())
		if err != nil {
			return action.Outcome{}, err
		}
		if err = quote.CheckDraft(quote.Data{Evidence: proofs}, readable); err != nil {
			return action.Outcome{}, moved(err, "/args/data/evidence", "/args/evidence")
		}
		reviewID = a.ReviewID
		fields := map[string]any{"quote_id": h.q.ID, "business_revision": a.Revision, "business_hash": a.BusinessHash, "approval_id": a.ID,
			"review_request_id": a.ReviewID, "effectivity_version": a.EffectivityVersion, "risk_hold": hold != nil}
		if hold != nil {
			// The approval formalized the active revision: its forward use is paused (RiskHold).
			fields["hold_id"], fields["held_revision"], fields["hold_code"] = hold.ID, hold.Revision, hold.Code
		}
		return h.save(ctx, exec, false, api.ApprovalRevoked, h.event(api.EventQuoteApprovalRevoked, fields))
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandRevokeApproval, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), &preparedQuote{h})
	if err != nil {
		return api.ReviewReceipt{}, err
	}
	if reviewID == "" { // a replay: the approval names its review
		reviewID = s.reviewOf(ctx, res.ObjectID, cmd.ApprovalID)
	}
	return s.reviewReceipt(ctx, res, reviewID)
}

// event is a QuoteRoot event at the root version the command moved the root to.
func (h *quoteCommand) event(eventType string, fields map[string]any) action.Event {
	return action.Event{Type: eventType, AggregateType: "QuoteRoot", AggregateID: h.q.ID, AggregateVersion: h.q.RootVersion, Fields: fields}
}

// reviewOf returns the review an approval of quote id was given in, "" when it cannot be read.
func (s Service) reviewOf(ctx context.Context, id, approvalID string) string {
	var review string
	_ = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err == nil && q != nil {
			if a := q.Approval(approvalID); a != nil {
				review = a.ReviewID
			}
		}
		return err
	})
	return review
}

// reviewReceipt completes the receipt of a review command, executed or replayed, from the stored
// root: the review, its revision and hash, and its approval when it has one.
func (s Service) reviewReceipt(ctx context.Context, r api.Receipt, reviewID string) (api.ReviewReceipt, error) {
	out := api.ReviewReceipt{Receipt: r, ReviewRequestID: reviewID}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), r.ObjectID, postgres.NoLock)
		if err != nil || q == nil {
			return err
		}
		review := q.Review(reviewID)
		if review == nil {
			return nil
		}
		out.BusinessRevision, out.BusinessHash, out.MarginExceptionRequired = review.Revision, review.BusinessHash, review.MarginExceptionRequired
		for _, a := range q.Approvals {
			if a.ReviewID == reviewID {
				out.ApprovalID, out.EffectivityVersion, out.MarginExceptionApproved = a.ID, a.EffectivityVersion, a.Exception != nil
			}
		}
		return nil
	})
	return out, err
}

// Releasable decides, inside the caller's TRD transaction after the quote root is locked FOR UPDATE,
// whether approval r.ApprovalID releases revision r.Revision of q at instant at (quote.Releasable):
// quote.activate and quote.replace call it before they formalize, and CheckApproval in a read. Each
// reviewer's grant is read through the authority guard rows FOR SHARE (Authorizer.Qualify), after
// the root as the lock order requires, so a revocation of their access either committed before and
// is seen (APPROVAL_STALE), or waits until the caller's transaction ends.
func (s Service) Releasable(ctx context.Context, exec bentopg.Executor, q *quote.Quote, r quote.Release, at time.Time) (*quote.Approval, *quote.Calculation, error) {
	return q.Releasable(r, at, func(actorID string) (quote.Standing, error) {
		h, err := s.Ledger.Authorizer.Qualify(ctx, exec, actorID)
		if err != nil {
			return quote.Standing{}, err
		}
		return quote.Standing{Human: h.Active && h.Actor.Kind == kernel.ActorHuman, Permissions: h.Permissions,
			SeesRoot: h.Sees(q.OwnerActor, q.OwnerTeam)}, nil
	})
}

// CheckApproval says whether an approval releases its revision now, audited as
// quote.approval.check (trd.read). A refusal of the check is its answer, not an error.
func (s Service) CheckApproval(ctx context.Context, quoteID, approvalID string) (api.ApprovalCheck, error) {
	var v api.ApprovalCheck
	err := s.Ledger.Read(ctx, CommandCheckApproval, quoteID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), quoteID, postgres.NoLock)
		if err != nil {
			return err
		}
		if q == nil || !d.Sees(q.OwnerActor, q.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		a := q.Approval(approvalID)
		if a == nil {
			return kernel.Reject(kernel.CodeNotFound, "/approval_id", "")
		}
		now := s.Clock.Now()
		v = api.ApprovalCheck{QuoteID: q.ID, ApprovalID: a.ID, BusinessRevision: a.Revision, BusinessHash: a.BusinessHash,
			TargetCommand: a.Target, EffectivityVersion: a.EffectivityVersion, MarginExceptionApproved: a.Exception != nil,
			CheckedAt: kernel.Instant(now)}
		if review := q.Review(a.ReviewID); review != nil {
			v.MarginExceptionRequired = review.MarginExceptionRequired
		}
		if rev := q.Revision(a.Revision); rev != nil {
			c, err := rev.Calculation()
			if err != nil {
				return err
			}
			if c != nil {
				v.MarginRule = c.MarginRule
			}
		}
		_, _, err = s.Releasable(ctx, exec, q, quote.Release{Revision: a.Revision, Target: a.Target, ExpectedActive: a.ExpectedActive,
			ApprovalID: a.ID}, now)
		if rej, ok := kernel.AsRejection(err); ok {
			v.Code, v.Path = rej.Code, rej.Fields[0].Path
			return nil
		}
		v.Releasable = err == nil
		return err
	})
	return v, err
}
