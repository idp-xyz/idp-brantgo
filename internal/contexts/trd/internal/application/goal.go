package application

import (
	"context"
	"errors"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// GoalReadiness v1 (api/goal.go; Context Map R18; WBS S4-01c; baseline 15.4). The query reads like
// QuoteReady: the inputs of the revision a quote_ready goal judges are read before TRD's transaction
// (the upstream lookups, R11–R16), and inside it the read authorizes the caller (trd.read, any actor
// kind), loads the root unlocked (NOT_FOUND outside the caller's data scope, X-INV-003) and calls the
// upstream guard functions with TRD's executor, in context code order (CAT's SkuGuard for
// Q01.industry, then CST, INQ, POL and PTY for Q01.lines; each locks its rows FOR SHARE until the
// read ends, ADR-BR-004 option B). The approval's reviewer is qualified through the authority guard
// (Authorizer.Qualify) before the upstream guards, as readiness does. Nothing is written but the
// audit entry; the object control is not consulted.

// GoalReadiness judges goal gq.Goal on quote root gq.QuoteID now, audited as goal.readiness
// (api.GoalQueries).
func (s Service) GoalReadiness(ctx context.Context, gq api.GoalQuery) (api.GoalAssessment, error) {
	var out api.GoalAssessment
	const attempts = 3
	for attempt := 1; ; attempt++ {
		now := s.Clock.Now()
		var read int64 // the sealed revision whose inputs were read; 0: none
		var f facts
		var inputs error
		if gq.Validate() == nil && gq.Goal == api.GoalQuoteReady {
			q, err := s.preRead(ctx, gq.QuoteID)
			if err != nil {
				return api.GoalAssessment{}, err
			}
			if q != nil {
				if rev := judged(q, gq.BusinessRevision); rev != nil && rev.Seal != nil {
					read = rev.Number
					if f, inputs = s.facts(ctx, withoutSources(rev.Data), now); inputs != nil {
						if _, refused := kernel.AsRejection(inputs); !refused {
							return api.GoalAssessment{}, inputs
						}
					}
				}
			}
		}
		err := s.readQuote(ctx, api.QueryGoalReadiness, gq.QuoteID, gq.Validate(),
			func(ctx context.Context, exec bentopg.Executor, d authorization.Decision, q *quote.Quote) error {
				g := goalJudge{s: s, ctx: ctx, exec: exec, q: q, gq: gq, at: kernel.Instant(now)}
				var err error
				if gq.Goal == api.GoalOrderActive {
					out, err = g.orderActive()
					return err
				}
				if rev := judged(q, gq.BusinessRevision); rev != nil && rev.Seal != nil && rev.Number != read {
					if attempt < attempts {
						return errMoved
					}
					return kernel.Reject(kernel.CodeVersionConflict, "/quote_id", "The quote changed while it was judged")
				}
				out, err = g.quoteReady(f, inputs)
				return err
			})
		if errors.Is(err, errMoved) {
			continue
		}
		return out, err
	}
}

// goalJudge judges a goal on root q inside the read's transaction at instant at.
type goalJudge struct {
	s    Service
	ctx  context.Context
	exec bentopg.Executor
	q    *quote.Quote
	gq   api.GoalQuery
	at   time.Time
}

// verdict is one assertion being judged.
type verdict struct{ a api.Assertion }

func newVerdict(assertion string) *verdict {
	return &verdict{a: api.Assertion{Assertion: assertion, Checks: []api.GoalCheck{}, Evidence: []api.EvidenceRef{}}}
}

// check records check with err's answer: satisfied for nil, otherwise the refusal's code and path
// with answer refused gives (unsatisfied, unknown or error) — DEPENDENCY_NOT_SUPPORTED, an upstream
// that cannot answer, is always unknown. An error that is not a refusal is returned.
func (v *verdict) check(check string, err error, refused string) error {
	if err == nil {
		v.a.Checks = append(v.a.Checks, api.GoalCheck{Check: check, Answer: api.AnswerSatisfied})
		return nil
	}
	rej, ok := kernel.AsRejection(err)
	if !ok {
		return err
	}
	path := ""
	if len(rej.Fields) > 0 {
		path = rej.Fields[0].Path
	}
	if rej.Code == api.CodeDependencyNotSupported {
		refused = api.AnswerUnknown
	}
	v.a.Checks = append(v.a.Checks, api.GoalCheck{Check: check, Answer: refused, Code: rej.Code, Path: path})
	return nil
}

// evidence records the records the assertion rests on.
func (v *verdict) evidence(refs ...api.EvidenceRef) { v.a.Evidence = append(v.a.Evidence, refs...) }

// done combines the checks' answers (api.Outcome) and returns the assertion.
func (v *verdict) done() api.Assertion {
	answers := make([]string, len(v.a.Checks))
	for i, c := range v.a.Checks {
		answers[i] = c.Answer
	}
	v.a.Answer, _ = api.Outcome(answers...)
	v.a.Code = ""
	for _, c := range v.a.Checks {
		if c.Answer == v.a.Answer && c.Answer != api.AnswerSatisfied {
			v.a.Code = c.Code
			break
		}
	}
	return v.a
}

// assessment completes the answer for the assertions vs, in order.
func (g goalJudge) assessment(out api.GoalAssessment, vs ...*verdict) api.GoalAssessment {
	out.Goal, out.GoalVersion, out.QuoteID, out.RootVersion, out.CheckedAt = g.gq.Goal, g.gq.GoalVersion, g.q.ID, g.q.RootVersion, g.at
	answers := make([]string, len(vs))
	out.Assertions = make([]api.Assertion, len(vs))
	for i, v := range vs {
		out.Assertions[i] = v.done()
		answers[i] = out.Assertions[i].Answer
	}
	out.Outcome, out.Code = api.Outcome(answers...)
	return out
}

// target checks the target customer and RFQ the goal names against the data d of the revision judged
// (Q01.scope): REFERENCE_INVALID at /customer_id or /rfq_id when the revision binds another.
func (g goalJudge) target(scope *verdict, d quote.Data) {
	customer := ""
	if d.Customer != nil {
		customer = d.Customer.PartyID
		scope.evidence(api.EvidenceRef{Kind: api.EvidenceCustomer, ID: d.Customer.PartyID, Version: d.Customer.Revision})
	}
	var err error
	if g.gq.CustomerID != "" && g.gq.CustomerID != customer {
		err = kernel.Reject(kernel.CodeReferenceInvalid, "/customer_id", "The revision binds another customer")
	}
	_ = scope.check(api.CheckCustomer, err, api.AnswerUnsatisfied)
	rfq := ""
	if d.Rfq != nil {
		rfq = d.Rfq.RfqID
		scope.evidence(api.EvidenceRef{Kind: api.EvidenceRfq, ID: d.Rfq.RfqID, Version: d.Rfq.RowVersion})
	}
	err = nil
	if g.gq.RfqID != "" && g.gq.RfqID != rfq {
		err = kernel.Reject(kernel.CodeReferenceInvalid, "/rfq_id", "The revision answers another RFQ")
	}
	_ = scope.check(api.CheckRfq, err, api.AnswerUnsatisfied)
}

// unmet marks every verdict of vs unsatisfied on check with err's code: what they judge does not
// exist (no revision, no order).
func unmet(check string, err error, vs ...*verdict) {
	unmetAs(api.AnswerUnsatisfied, check, err, vs...)
}

// unmetAs marks every verdict of vs with answer on check with err's code.
func unmetAs(answer, check string, err error, vs ...*verdict) {
	for _, v := range vs {
		_ = v.check(check, err, answer)
	}
}

// revisionEvidence is the evidence of a sealed, formalized revision r of q.
func revisionEvidence(q *quote.Quote, r *quote.Revision) api.EvidenceRef {
	return api.EvidenceRef{Kind: api.EvidenceQuoteRevision, ID: q.ID, Version: r.Number, Hash: r.Seal.BusinessHash}
}

// commitEvidence is the evidence of r's commit evidence.
func commitEvidence(r *quote.Revision) api.EvidenceRef {
	doc, _ := r.Formal.Commit.EntriesJSON()
	return api.EvidenceRef{Kind: api.EvidenceCommit, ID: r.Formal.Commit.IdempotencyKey, Version: r.Number, Hash: quote.Hash(doc)}
}

// documentEvidence is the evidence of r's formal document.
func documentEvidence(r *quote.Revision) api.EvidenceRef {
	d := r.Formal.Document
	return api.EvidenceRef{Kind: api.EvidenceDocument, ID: d.Number, Version: r.Number, Hash: d.ContentSHA256}
}

// specEvidence is the evidence of the distinct SKU specification revisions of skus, in (SKU,
// revision) order.
func specEvidence(skus []quote.SkuRef) []api.EvidenceRef {
	seen := map[quote.SkuRef]bool{}
	var out []api.EvidenceRef
	for _, s := range skus {
		if !seen[s] {
			seen[s] = true
			out = append(out, api.EvidenceRef{Kind: api.EvidenceSkuSpec, ID: s.SkuID, Version: s.SpecRevision})
		}
	}
	slices.SortFunc(out, func(a, b api.EvidenceRef) int {
		if a.ID != b.ID {
			if a.ID < b.ID {
				return -1
			}
			return 1
		}
		return int(a.Version - b.Version)
	})
	return out
}

// industry judges Q01.industry: CAT's SkuGuard (R14) on every SKU specification revision of skus
// (lines under prefix), each refusal an unsatisfied check.
func (g goalJudge) industry(v *verdict, skus []quote.SkuRef, prefix string) error {
	v.evidence(specEvidence(skus)...)
	refused := false
	err := guardSpecs(g.ctx, g.exec, skus, prefix, func(refusal error, _ string) error {
		refused = true
		return v.check(api.CheckIndustry, refusal, api.AnswerUnsatisfied)
	})
	if err != nil || refused {
		return err
	}
	return v.check(api.CheckIndustry, nil, "")
}

// quoteReady judges quote_ready on the revision named (or the active one) of the root, on the
// inputs f read before the transaction (inputs: their refusal).
func (g goalJudge) quoteReady(f facts, inputs error) (api.GoalAssessment, error) {
	q := g.q
	scoped, quoted, lines, approval, document, industry := newVerdict(api.AssertionScope), newVerdict(api.AssertionQuote),
		newVerdict(api.AssertionLines), newVerdict(api.AssertionApproval), newVerdict(api.AssertionDocument), newVerdict(api.AssertionIndustry)
	scoped.evidence(api.EvidenceRef{Kind: api.EvidenceQuoteRoot, ID: q.ID, Version: q.RootVersion})
	var out api.GoalAssessment
	finish := func() (api.GoalAssessment, error) {
		return g.assessment(out, scoped, quoted, lines, approval, document, industry), nil
	}
	active, rev := q.Active(), judged(q, g.gq.BusinessRevision)
	switch {
	case g.gq.BusinessRevision != 0 && rev == nil:
		return api.GoalAssessment{}, kernel.Reject(kernel.CodeNotFound, "/business_revision", "")
	case rev == nil:
		unmet(api.CheckRevision, refusal(api.CodeNoActiveRevision, "/quote_id"), scoped, quoted, lines, approval, document, industry)
		return finish()
	}
	g.target(scoped, rev.Data)
	if rev.Seal == nil || rev.Formal == nil {
		unmet(api.CheckCommitEvidence, refusal(api.CodeCommitEvidenceMissing, "/business_revision"), quoted, lines, approval, document, industry)
		return finish()
	}
	out.BusinessRevision, out.BusinessHash = rev.Number, rev.Seal.BusinessHash
	quoted.evidence(revisionEvidence(q, rev), commitEvidence(rev))
	var err error
	if rev != active {
		err = refusal(api.CodeQuoteNotCurrent, "/business_revision")
	}
	_ = quoted.check(api.CheckRevision, err, api.AnswerUnsatisfied)
	_ = quoted.check(api.CheckCommitEvidence, nil, "")
	err = nil
	if h := q.Hold(rev.Number); h != nil {
		err = refusal(h.Code, "/holds/"+h.ID)
	}
	_ = quoted.check(api.CheckRiskHold, err, api.AnswerUnsatisfied)
	err = nil
	if !g.at.Before(rev.Seal.ExpiresAt) {
		err = refusal(api.CodeQuoteExpired, "/expires_at")
	}
	_ = quoted.check(api.CheckValidity, err, api.AnswerUnsatisfied)
	// The root's one full_v1 conversion: once an order holds it, the revision serves no conversion
	// any more and the answer names that order (baseline 15.4: 已占用时返回已有订单或明确冲突).
	err = nil
	if sl := q.Slot; sl != nil {
		quoted.evidence(api.EvidenceRef{Kind: api.EvidenceConversionSlot, ID: sl.OrderID, Version: sl.Revision, Hash: sl.BusinessHash})
		err = api.Reject(api.CodeConversionSlotConflict, "/quote_id")
	}
	_ = quoted.check(api.CheckConversion, err, api.AnswerUnsatisfied)
	// The formal document: its stored bytes are the bound ones (an integrity failure is an error).
	document.evidence(documentEvidence(rev))
	if _, err = g.s.verifiedContent(g.ctx, g.exec, q, rev); document.check(api.CheckDocument, err, api.AnswerError) != nil {
		return api.GoalAssessment{}, err
	}
	// The approval the commit evidence names, with its approvers' standings now.
	approval.evidence(api.EvidenceRef{Kind: api.EvidenceApproval, ID: rev.Formal.Commit.ApprovalID, Version: rev.Formal.Commit.EffectivityVersion})
	if _, err = q.ReadyApproval(rev, g.at, g.s.standings(g.ctx, g.exec, q)); approval.check(api.CheckApproval, err, api.AnswerUnsatisfied) != nil {
		return api.GoalAssessment{}, err
	}
	c, err := quote.ParseCommercial(rev.Seal.Commercial, rev.Seal.BusinessHash)
	if err != nil {
		if document.check(api.CheckDocument, err, api.AnswerError) != nil {
			return api.GoalAssessment{}, err
		}
		unmetAs(api.AnswerError, api.CheckDocument, err, lines, industry)
		return finish()
	}
	// CAT first (Q01.industry), then CST, INQ, POL and PTY (Q01.lines): context code order.
	skus := make([]quote.SkuRef, len(rev.Data.Lines))
	for i, l := range rev.Data.Lines {
		skus[i] = l.Sku
	}
	if err = g.industry(industry, skus, "/data"); err != nil {
		return api.GoalAssessment{}, err
	}
	if s := rev.Data.Snapshot; s != nil {
		lines.evidence(api.EvidenceRef{Kind: api.EvidenceCostSnapshot, ID: s.SnapshotID, Hash: s.ResultHash})
	}
	if r := rev.Data.Rfq; r != nil {
		lines.evidence(api.EvidenceRef{Kind: api.EvidenceRfq, ID: r.RfqID, Version: r.RowVersion})
	}
	// Every guard answers (a lapse is recorded and the next guard called); a refusal the guards give
	// before any guard (a bound fact missing) ends them.
	guards := sealedGuards(rev, c)
	guards.skipSpecs = true
	refused := false
	_, err = g.s.checkUpstream(g.ctx, g.exec, guards, g.at, func(r error, _ string) error {
		refused = true
		return lines.check(api.CheckUpstream, r, api.AnswerUnsatisfied)
	})
	if err != nil || !refused {
		if err = lines.check(api.CheckUpstream, err, api.AnswerUnsatisfied); err != nil {
			return api.GoalAssessment{}, err
		}
	}
	if inputs != nil {
		_ = lines.check(api.CheckInputs, inputs, api.AnswerUnknown)
		return finish()
	}
	_ = lines.check(api.CheckInputs, nil, "")
	check, err := bindAgain(c, rev.Data, f, g.at)
	if lines.check(check, err, api.AnswerUnsatisfied) != nil {
		return api.GoalAssessment{}, err
	}
	if err = lines.check(api.CheckSalesFX, salesFXAge(rev.Data, f, g.at), api.AnswerUnsatisfied); err != nil {
		return api.GoalAssessment{}, err
	}
	return finish()
}

// orderActive judges order_active: the order holding the root's full_v1 slot, the acceptance that
// formed the contract it converts, and the revision it converts (named, which the order must convert,
// or the order's; without an order, the one named or the active one).
func (g goalJudge) orderActive() (api.GoalAssessment, error) {
	q := g.q
	scoped, quoted, lines, approval, document, industry, accepted, ordered := newVerdict(api.AssertionScope), newVerdict(api.AssertionQuote),
		newVerdict(api.AssertionLines), newVerdict(api.AssertionApproval), newVerdict(api.AssertionDocument), newVerdict(api.AssertionIndustry),
		newVerdict(api.AssertionAcceptance), newVerdict(api.AssertionOrder)
	scoped.evidence(api.EvidenceRef{Kind: api.EvidenceQuoteRoot, ID: q.ID, Version: q.RootVersion})
	var out api.GoalAssessment
	finish := func() (api.GoalAssessment, error) {
		return g.assessment(out, scoped, quoted, lines, approval, document, industry, accepted, ordered), nil
	}
	if g.gq.BusinessRevision != 0 && q.Revision(g.gq.BusinessRevision) == nil {
		return api.GoalAssessment{}, kernel.Reject(kernel.CodeNotFound, "/business_revision", "")
	}
	// The order: the one holding the root's full_v1 conversion slot (ORDER_MISSING without one).
	var o *order.Order
	if sl := q.Slot; sl != nil {
		var err error
		if o, err = postgres.LoadOrder(g.ctx, g.exec, scope(g.ctx), sl.OrderID, postgres.NoLock); err != nil {
			return api.GoalAssessment{}, err
		}
		ordered.evidence(api.EvidenceRef{Kind: api.EvidenceConversionSlot, ID: sl.OrderID, Version: sl.Revision, Hash: sl.BusinessHash})
		if o == nil || o.Source.QuoteID != q.ID || o.Source.Revision != sl.Revision || o.Source.BusinessHash != sl.BusinessHash {
			_ = ordered.check(api.CheckConversion, kernel.Reject(kernel.CodeReferenceInvalid, "/quote_id", "The conversion slot names no order of it"),
				api.AnswerError)
			unmet(api.CheckOrder, kernel.Reject(kernel.CodeReferenceInvalid, "/quote_id", ""), lines, approval, accepted)
			return g.orderlessRevision(&out, finish, scoped, quoted, document, industry)
		}
		_ = ordered.check(api.CheckConversion, nil, "")
	}
	if o == nil {
		missing := api.Reject(api.CodeOrderMissing, "/quote_id")
		_ = ordered.check(api.CheckConversion, missing, api.AnswerUnsatisfied)
		unmet(api.CheckOrder, missing, lines, approval, accepted)
		return g.orderlessRevision(&out, finish, scoped, quoted, document, industry)
	}
	src := o.Source
	out.OrderID, out.OrderRevision, out.OrderHash = o.ID, o.OrderRevision, o.OrderHash
	ordered.evidence(api.EvidenceRef{Kind: api.EvidenceOrder, ID: o.ID, Version: o.OrderRevision, Hash: o.OrderHash})
	var err error
	if o.Status != order.Active {
		err = api.Reject(api.CodeOrderNotActive, "/order/status")
	}
	_ = ordered.check(api.CheckOrder, err, api.AnswerUnsatisfied)
	// The revision the order converts, sealed and formalized at the hash the order binds.
	rev := q.Revision(src.Revision)
	if rev == nil || rev.Seal == nil || rev.Formal == nil || rev.Seal.BusinessHash != src.BusinessHash {
		broken := kernel.Reject(kernel.CodeReferenceInvalid, "/order/source", "The order converts no formal revision of the root at its hash")
		unmetAs(api.AnswerError, api.CheckRevision, broken, scoped, quoted, lines, approval, document, industry, accepted)
		return finish()
	}
	out.BusinessRevision, out.BusinessHash = rev.Number, rev.Seal.BusinessHash
	quoted.evidence(revisionEvidence(q, rev), commitEvidence(rev))
	err = nil
	if g.gq.BusinessRevision != 0 && g.gq.BusinessRevision != rev.Number {
		err = refusal(api.CodeQuoteNotCurrent, "/business_revision")
	}
	_ = quoted.check(api.CheckRevision, err, api.AnswerUnsatisfied)
	_ = quoted.check(api.CheckCommitEvidence, nil, "")
	g.target(scoped, rev.Data)
	// The acceptance that formed the contract: valid, formed, of this revision at this hash; the
	// contract formed when the acceptance arrived before the revision expired, so a later expiry does
	// not matter (TRD-INV-017).
	a, err := postgres.LoadAcceptance(g.ctx, g.exec, scope(g.ctx), src.AcceptanceID, postgres.NoLock)
	if err != nil {
		return api.GoalAssessment{}, err
	}
	if a == nil {
		missing := kernel.Reject(kernel.CodeReferenceInvalid, "/order/acceptance_id", "The order's acceptance is missing")
		_ = accepted.check(api.CheckAcceptance, missing, api.AnswerError)
		_ = quoted.check(api.CheckValidity, missing, api.AnswerError)
	} else {
		accepted.evidence(api.EvidenceRef{Kind: api.EvidenceAcceptance, ID: a.ID, Version: a.Version, Hash: a.BusinessHash})
		_ = accepted.check(api.CheckAcceptance, a.Check(src.QuoteID, src.Revision, src.BusinessHash, "/order/acceptance_id"), api.AnswerUnsatisfied)
		err = nil
		if !a.ArrivalAt.Before(rev.Seal.ExpiresAt) {
			err = api.Reject(api.CodeAcceptanceLate, "/expires_at")
		}
		_ = quoted.check(api.CheckValidity, err, api.AnswerUnsatisfied)
		err = nil
		if rev.Data.Customer == nil || a.Customer.PartyID != rev.Data.Customer.PartyID {
			err = api.Reject(api.CodeAcceptanceScopeMismatch, "/order/acceptance_id")
		}
		_ = scoped.check(api.CheckAcceptance, err, api.AnswerError)
	}
	// The order's lines: its snapshot is the sealed commercial document copied whole, and its current
	// revision a sound successor (an integrity failure is an error).
	_, err = o.Verify(scope(g.ctx), "/order/snapshot")
	if lines.check(api.CheckOrderSnapshot, err, api.AnswerError) != nil {
		return api.GoalAssessment{}, err
	}
	lines.evidence(api.EvidenceRef{Kind: api.EvidenceOrder, ID: o.ID, Version: o.OrderRevision, Hash: o.OrderHash})
	current, revErr := o.Revision()
	if err == nil {
		if revErr != nil {
			revErr = kernel.Reject(kernel.CodeReferenceInvalid, "/order/revision", "The order's current revision is not sound")
		}
		_ = lines.check(api.CheckOrderSnapshot, revErr, api.AnswerError)
	}
	// The approval that activated the order: approved and not revoked (ORDER_APPROVAL_REVOKED, as
	// OrderGuard v1 reports it), with the activation's commit evidence.
	err = nil
	r := o.Review(o.ActivatedReview)
	switch {
	case r == nil || len(o.Commits) == 0:
		err = api.Reject(api.CodeOrderApprovalRequired, "/order/activated_review_id")
	case r.Status == order.ReviewRevoked:
		err = api.Reject(api.CodeOrderApprovalRevoked, "/order/activated_review_id")
	case r.Status != order.ReviewApproved:
		err = api.Reject(api.CodeOrderApprovalRequired, "/order/activated_review_id")
	}
	if r != nil {
		approval.evidence(api.EvidenceRef{Kind: api.EvidenceOrderReview, ID: r.ID, Version: r.OrderRevision, Hash: r.OrderHash})
	}
	_ = approval.check(api.CheckOrderApproval, err, api.AnswerUnsatisfied)
	// The formal document the order binds: the source revision's, its stored bytes intact.
	document.evidence(documentEvidence(rev))
	if _, err = g.s.verifiedContent(g.ctx, g.exec, q, rev); document.check(api.CheckDocument, err, api.AnswerError) != nil {
		return api.GoalAssessment{}, err
	}
	err = nil
	if d := rev.Formal.Document; d.ContentSHA256 != src.DocumentSHA256 || d.Number != src.Number {
		err = api.RejectDocument(api.CodeFormalDocumentIntegrityFailure, "/order/source")
	}
	_ = document.check(api.CheckOrderSnapshot, err, api.AnswerError)
	// Q01.industry on the order's current revision's lines (the source revision's if it is not sound).
	var skus []quote.SkuRef
	prefix := "/order"
	if revErr == nil && current != nil {
		for _, l := range current.Lines {
			skus = append(skus, l.Sku)
		}
	} else {
		prefix = "/data"
		for _, l := range rev.Data.Lines {
			skus = append(skus, l.Sku)
		}
	}
	if err = g.industry(industry, skus, prefix); err != nil {
		return api.GoalAssessment{}, err
	}
	return finish()
}

// orderlessRevision judges what order_active can judge without an order: the target, the revision
// (named, or the active one), its formal document and its SKU specifications; then finishes.
func (g goalJudge) orderlessRevision(out *api.GoalAssessment, finish func() (api.GoalAssessment, error), scoped, quoted, document,
	industry *verdict) (api.GoalAssessment, error) {
	q := g.q
	rev := judged(q, g.gq.BusinessRevision)
	if rev == nil {
		unmet(api.CheckRevision, refusal(api.CodeNoActiveRevision, "/quote_id"), scoped, quoted, document, industry)
		return finish()
	}
	g.target(scoped, rev.Data)
	if rev.Seal == nil || rev.Formal == nil {
		unmet(api.CheckCommitEvidence, refusal(api.CodeCommitEvidenceMissing, "/business_revision"), quoted, document, industry)
		return finish()
	}
	out.BusinessRevision, out.BusinessHash = rev.Number, rev.Seal.BusinessHash
	quoted.evidence(revisionEvidence(q, rev), commitEvidence(rev))
	var err error
	if rev != q.Active() {
		err = refusal(api.CodeQuoteNotCurrent, "/business_revision")
	}
	_ = quoted.check(api.CheckRevision, err, api.AnswerUnsatisfied)
	_ = quoted.check(api.CheckCommitEvidence, nil, "")
	document.evidence(documentEvidence(rev))
	if _, err = g.s.verifiedContent(g.ctx, g.exec, q, rev); document.check(api.CheckDocument, err, api.AnswerError) != nil {
		return api.GoalAssessment{}, err
	}
	skus := make([]quote.SkuRef, len(rev.Data.Lines))
	for i, l := range rev.Data.Lines {
		skus[i] = l.Sku
	}
	if err = g.industry(industry, skus, "/data"); err != nil {
		return api.GoalAssessment{}, err
	}
	return finish()
}
