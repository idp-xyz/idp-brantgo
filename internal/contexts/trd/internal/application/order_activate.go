package application

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// order.activate (human; WBS S3-07, S3-13; TRD-INV-013, 014, 016, 017, 021; ADR-BR-004, ADR-BR-013;
// SC-04, SC-07, BC-AT13).
//
// Before TRD's transaction, Prepare reads from POL the sales exchange rate the source revision was
// calculated with and the pricing policy's maximum age (ConfigRef v1, R15), as a formalization does.
//
// In the transaction (tactical lockOrder, ADR-BR-011):
//
//  1. the quote root FOR UPDATE, then the order FOR UPDATE (the handler's Lock);
//  2. the company's authority guard row FOR SHARE (the ledger's Authorize);
//  3. visibility and the quote root's object control (TRD-INV-015); the order is a draft
//     (ORDER_NOT_DRAFT) at expected_version (VERSION_CONFLICT) and its snapshot intact
//     (REFERENCE_INVALID);
//  4. the source (orderSource): the acceptance still valid and formed for the revision
//     (ACCEPTANCE_NOT_VALID — never the clock against the quote's expires_at, TRD-INV-017); the
//     revision still the root's active, unheld revision at its hash (QUOTE_NOT_CURRENT,
//     APPROVAL_REVOKED for a risk hold); the slot held by this order; the formal document's bytes
//     (FORMAL_DOCUMENT_INTEGRITY_FAILURE);
//  5. the formalization approval and its reviewer, whose grant is read FOR SHARE through the
//     authority guard (quote.FormalApproval: APPROVAL_REVOKED, INDEPENDENT_REVIEW_REQUIRED,
//     APPROVAL_STALE);
//  6. the order gate (order.Admit), the order reviewer's grant read the same way;
//  7. the upstream guards of the source revision in context code order, each locking its rows FOR
//     SHARE until the order commits (orderUpstream): CAT SkuGuard; the CNX TradeComplianceGuard
//     extension point; CST AdoptabilityGuard; INQ RfqGuard; POL ConfigGuard; PTY TradingProfileGuard,
//     AddressRoleGuard and PartyScreeningGuard; the STL CreditGuard extension point. A lapse of a
//     bound fact becomes a risk (with the sales exchange rate's age), a party not screened clear a
//     refusal (PARTY_SCREENING_NOT_CLEAR);
//  8. every risk acknowledged by the approval (order.Activate: ORDER_RISK_UNACKNOWLEDGED);
//  9. the writes: the commit evidence, the order active at its next row version, then the ledger's
//     action result, audit entry and OrderActivated (TRD-INV-016: all or nothing).

// ActivationGuard is an upstream guard order.activate calls inside its transaction with TRD's
// executor, at its place in context code order: it returns the evidence entries the commit evidence
// records and nil when the order may activate, or the owner's refusal.
type ActivationGuard func(ctx context.Context, exec bentopg.Executor, f ActivationFacts) ([]quote.Entry, error)

// ActivationFacts are what an activation guard judges: the order at its revision and hash, the
// source revision's commercial document (parties, destination, lines, payment terms and the
// calculation's payment schedule) and the activation's instant.
type ActivationFacts struct {
	OrderID       string
	OrderRevision int64
	OrderHash     string
	Commercial    quote.Commercial
	At            time.Time
}

// OrderGuards are the named extension points of order.activate for the guards of contexts that do
// not exist yet. By the owner's decision of 2026-11-10 neither is wired in S3: a nil guard is a
// recorded gap — the commit evidence says "not_wired" and OrderActivated lists it in
// unchecked_guards — never a pass.
type OrderGuards struct {
	// Credit is STL's CreditGuard v1 (Context Map R57, TRD-INV-022: a credit reservation covering the
	// part of the payment terms due after shipment; CREDIT_RESERVATION_REQUIRED), called after PTY.
	Credit ActivationGuard
	// TradeCompliance is CNX's TradeComplianceGuard v1 (R58, TRD-INV-023: a clear trade compliance
	// conclusion covering the order revision; TRADE_COMPLIANCE_NOT_CLEAR), called after CAT.
	TradeCompliance ActivationGuard
}

// notWired is the evidence value of an extension point no guard is wired to.
const notWired = "not_wired"

// orderFX reads, before a transaction of order id, the sales exchange rate its source revision was
// calculated with and the pricing policy's maximum age (the zero value: no sales rate, or a rate POL
// no longer resolves, which ConfigGuard reports as a risk in the transaction).
func (s Service) orderFX(ctx context.Context, id string) (salesFX, error) {
	o, q, err := s.orderOf(ctx, id)
	if err != nil || o == nil {
		return salesFX{}, err
	}
	rev := q.Revision(o.Source.Revision)
	if rev == nil || rev.Data.SalesFX == nil || rev.Data.PricingPolicy == nil {
		return salesFX{}, nil
	}
	fx, policy := *rev.Data.SalesFX, *rev.Data.PricingPolicy
	refs := []polapi.ConfigRef{{Kind: fx.Kind, ConfigID: fx.ID, Revision: fx.Revision}, {Kind: policy.Kind, ConfigID: policy.ID,
		Revision: policy.Revision}}
	got, err := s.Upstream.Configs.ResolveConfigs(ctx, refs, s.Clock.Now())
	if err != nil {
		return salesFX{}, err
	}
	rate, p := got[refs[0]].Data.FXRate, got[refs[1]].Data.PricingPolicy
	if rate == nil || p == nil {
		return salesFX{}, nil
	}
	// A rate without a readable observation instant is as stale as one too old (FX_STALE).
	observed, _ := kernel.ParseTimestamp(rate.ObservedAt)
	return salesFX{present: true, observed: observed, maxAge: time.Duration(p.MaxFXAgeSeconds) * time.Second}, nil
}

// source is what orderSource found: the source revision and its commercial document.
type source struct {
	revision   *quote.Revision
	commercial quote.Commercial
}

// orderSource checks, on the locked quote root and order, that the order's source still stands:
// the order's snapshot is intact (REFERENCE_INVALID); the acceptance is still valid and formed for
// the revision (ACCEPTANCE_NOT_VALID, ACCEPTANCE_SCOPE_MISMATCH; TRD-INV-017: no clock is compared
// with the quote's expires_at); the revision is still the root's active, unheld revision at its hash
// (quote.Usable); the conversion slot is held by this order (CONVERSION_SLOT_CONFLICT); and the
// formal document's stored bytes are still the bound ones (FORMAL_DOCUMENT_INTEGRITY_FAILURE).
// Acceptance changes lock the quote root first, so reading the acceptance under it is current.
func (s Service) orderSource(ctx context.Context, exec bentopg.Executor, h *orderCommand, path string) (source, error) {
	o, q := h.o, h.q
	if _, err := o.Verify(scope(ctx), path); err != nil {
		return source{}, err
	}
	src := o.Source
	a, err := postgres.LoadAcceptance(ctx, exec, scope(ctx), src.AcceptanceID, postgres.NoLock)
	if err != nil {
		return source{}, err
	}
	if a == nil {
		return source{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "The order's acceptance is missing")
	}
	if err = a.Check(src.QuoteID, src.Revision, src.BusinessHash, path); err != nil {
		return source{}, err
	}
	if err = q.Usable(src.Revision, src.BusinessHash, path); err != nil {
		return source{}, err
	}
	if q.Slot == nil || q.Slot.OrderID != o.ID || q.Slot.Revision != src.Revision {
		return source{}, api.Reject(api.CodeConversionSlotConflict, path)
	}
	rev := q.Revision(src.Revision)
	content, ok, err := postgres.LoadDocumentContent(ctx, exec, scope(ctx), src.QuoteID, src.Revision)
	if err != nil {
		return source{}, err
	}
	if d := rev.Formal.Document; !ok || len(content) != d.SizeBytes || quote.Hash(content) != d.ContentSHA256 || d.ContentSHA256 != src.DocumentSHA256 {
		return source{}, api.RejectDocument(api.CodeFormalDocumentIntegrityFailure, path)
	}
	c, err := quote.ParseCommercial(rev.Seal.Commercial, rev.Seal.BusinessHash)
	if err != nil {
		return source{}, err
	}
	return source{revision: rev, commercial: c}, nil
}

// upstreamFound is what orderUpstream found: the risks, CST's verdict on the cost snapshot, the
// first party not screened clear (activation only), the evidence entries of the guards and the
// extension points not wired.
type upstreamFound struct {
	risks     []order.Risk
	adoptable cstapi.AdoptabilityVerdict
	screening error
	entries   []quote.Entry
	unchecked []string
}

// orderUpstream re-checks what the order's source revision binds upstream, in context code order,
// recording each lapse as a risk (ADR-BR-004: a change of cost, configuration or exchange rate after
// the contract formed is acknowledged by the order approval, not refused), and the sales exchange
// rate's age at now as one. Activating, it also runs PTY's PartyScreeningGuard on every party the
// revision binds and the extension points of CNX and STL at their places.
func (s Service) orderUpstream(ctx context.Context, exec bentopg.Executor, h *orderCommand, src source, fx salesFX, activating bool,
	now time.Time) (upstreamFound, error) {
	var found upstreamFound
	var err error
	g := sealedGuards(src.revision, src.commercial)
	facts := ActivationFacts{OrderID: h.o.ID, OrderRevision: h.o.OrderRevision, OrderHash: h.o.OrderHash, Commercial: src.commercial, At: now}
	extension := func(name string, guard ActivationGuard) error {
		if guard == nil {
			found.unchecked = append(found.unchecked, name)
			found.entries = append(found.entries, quote.Entry{Name: name, Value: notWired})
			return nil
		}
		entries, err := guard(ctx, exec, facts)
		found.entries = append(found.entries, entries...)
		return err
	}
	if activating {
		g.afterCAT = func() error { return extension(api.GuardCNXTradeCompliance, s.Orders.TradeCompliance) }
		g.afterPTY = func() error {
			if err := s.screen(ctx, exec, g.data, now, &found); err != nil {
				return err
			}
			return extension(api.GuardSTLCredit, s.Orders.Credit)
		}
	}
	found.adoptable, err = s.checkUpstream(ctx, exec, g, now, func(refusal error, detail string) error {
		rej, ok := kernel.AsRejection(refusal)
		if !ok {
			return refusal
		}
		path := ""
		if len(rej.Fields) > 0 {
			path = rej.Fields[0].Path
		}
		found.risks = append(found.risks, order.Risk{Path: path, Code: rej.Code, Detail: detail})
		return nil
	})
	if err != nil {
		return found, err
	}
	if err = fx.check(now); err != nil {
		rej, ok := kernel.AsRejection(err)
		if !ok {
			return found, err
		}
		found.risks = append(found.risks, order.Risk{Path: rej.Fields[0].Path, Code: rej.Code})
	}
	a := found.adoptable
	found.entries = append([]quote.Entry{{Name: "cost_snapshot_id", Value: a.Ref.SnapshotID}, {Name: "cost_snapshot_result_hash", Value: a.Ref.ResultHash},
		{Name: "cost_snapshot_adoptability", Value: a.Status}, {Name: "cost_snapshot_input", Value: a.Input},
		{Name: "upstream_checked_at", Value: kernel.FormatTimestamp(now)}}, found.entries...)
	return found, nil
}

// screen runs PTY's PartyScreeningGuard v1 at now on every party revision data binds — the customer
// and the parties of its addresses (buyer, payer, consignee; TRD-INV-021) — in party ID order,
// keeping the first party not screened clear in found.screening (PARTY_SCREENING_NOT_CLEAR) and the
// conclusions in its entries. A refusal is never a risk: no order approval overrides it.
func (s Service) screen(ctx context.Context, exec bentopg.Executor, d quote.Data, now time.Time, found *upstreamFound) error {
	parties := map[string]string{}
	if d.Customer != nil {
		parties[d.Customer.PartyID] = "/data/customer_ref"
	}
	for i, a := range d.Addresses {
		if _, ok := parties[a.PartyID]; !ok {
			parties[a.PartyID] = index("/data/customer_address_refs", i)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(parties)) {
		var g ptyapi.ScreeningGuardResult
		if err := exec.QueryRow(ctx, ptyapi.ScreeningGuardSQL, id, now).Scan(g.Targets()...); err != nil {
			return err
		}
		found.entries = append(found.entries, quote.Entry{Name: "screening:" + id, Value: g.Status + "/" + g.Reason + "/" + g.ScreeningID})
		if err := g.Check(parties[id]); err != nil && found.screening == nil {
			found.screening = err
		}
	}
	return nil
}

func index(path string, i int) string { return path + "/" + itoa(int64(i)) }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// activation is what admitActivation found.
type activation struct {
	approval *quote.Approval
	review   *order.Review
	upstream upstreamFound
}

// admitActivation runs every check of order.activate on the locked quote root and order at now, in
// the order activation refuses in (steps 3–7 above); expected is the order's row version (0: not
// checked, CheckOrder) and reviewID the review (""; no order gate, CheckOrder). The risks found are
// in the result whatever the gate says.
func (s Service) admitActivation(ctx context.Context, exec bentopg.Executor, h *orderCommand, expected int64, reviewID string, fx salesFX,
	now time.Time) (activation, error) {
	var in activation
	if err := h.o.CheckDraft(); err != nil {
		return in, err
	}
	if expected > 0 {
		if err := h.o.Check(expected); err != nil {
			return in, err
		}
	}
	src, err := s.orderSource(ctx, exec, h, "/args/order_id")
	if err != nil {
		return in, err
	}
	standings := s.standings(ctx, exec, h.q)
	if in.approval, err = h.q.FormalApproval(h.o.Source.Revision, standings, "/args/order_id"); err != nil {
		return in, err
	}
	if reviewID != "" {
		if in.review, err = h.o.Admit(reviewID, standings, now); err != nil {
			return in, err
		}
	}
	if in.upstream, err = s.orderUpstream(ctx, exec, h, src, fx, true, now); err != nil {
		return in, err
	}
	return in, in.upstream.screening
}

// ActivateOrder activates a draft order on its approved review (order.activate).
func (s Service) ActivateOrder(ctx context.Context, cmd api.ActivateOrder) (api.OrderReceipt, error) {
	h := &orderCommand{s: s, orderID: cmd.OrderID, reviewID: cmd.ReviewID}
	var fx salesFX
	h.prepare = func(ctx context.Context) error {
		var err error
		fx, err = s.orderFX(ctx, cmd.OrderID)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, "/args/order_id"); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		in, err := s.admitActivation(ctx, exec, h, cmd.ExpectedVersion, cmd.ReviewID, fx, now)
		if err != nil {
			return action.Outcome{}, err
		}
		up := in.upstream
		c, err := h.o.Activate(order.Activation{Review: in.review, IdempotencyKey: cmd.IdempotencyKey, Risks: up.risks, ApprovalID: in.approval.ID,
			CostSnapshotID: up.adoptable.Ref.SnapshotID, Upstream: up.entries, ControlEpoch: h.q.Control.Epoch}, d.Actor, d.Epoch, now)
		if err != nil {
			return action.Outcome{}, err
		}
		src, r := h.o.Source, in.review
		return h.save(ctx, exec, string(h.o.Status), h.event(api.EventOrderActivated, map[string]any{"number": src.Number,
			"acceptance_id": src.AcceptanceID, "arrival_at": kernel.FormatTimestamp(src.ArrivalAt),
			"quote_expires_at": kernel.FormatTimestamp(src.ExpiresAt), "review_id": r.ID, "reviewer_id": r.Decided.By.ID,
			"review_expires_at": kernel.FormatTimestamp(r.ExpiresAt), "formalization_approval_id": in.approval.ID,
			"risks_acknowledged": len(up.risks), "unchecked_guards": joinGuards(up.unchecked), "activated_at": kernel.FormatTimestamp(c.At)}))
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandActivateOrder, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.OrderReceipt{}, err
	}
	return s.orderReceipt(ctx, res, cmd.ReviewID)
}

// CheckOrder says what order.activate would decide now on review reviewID, audited as order.check
// (trd.read), without changing anything. A refusal is its answer.
func (s Service) CheckOrder(ctx context.Context, orderID, reviewID string) (api.OrderCheck, error) {
	fx, err := s.orderFX(ctx, orderID)
	if _, refused := kernel.AsRejection(err); err != nil && !refused {
		return api.OrderCheck{}, err
	}
	var v api.OrderCheck
	err = s.Ledger.Read(ctx, CommandCheckOrder, orderID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		h := &orderCommand{s: s, orderID: orderID}
		quoteID, err := postgres.OrderQuote(ctx, exec, scope(ctx), orderID)
		if err != nil {
			return err
		}
		if h.q, err = postgres.LoadQuote(ctx, exec, scope(ctx), quoteID, postgres.NoLock); err != nil {
			return err
		}
		if h.o, err = postgres.LoadOrder(ctx, exec, scope(ctx), orderID, postgres.NoLock); err != nil {
			return err
		}
		if h.q == nil || h.o == nil || !d.Sees(h.q.OwnerActor, h.q.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		now := kernel.Instant(s.Clock.Now())
		v = api.OrderCheck{OrderID: h.o.ID, Status: string(h.o.Status), RowVersion: h.o.RowVersion, OrderHash: h.o.OrderHash, ReviewID: reviewID,
			CheckedAt: now}
		in, err := s.admitActivation(ctx, exec, h, 0, reviewID, fx, now)
		v.Risks, v.Unchecked = apiRisks(in.upstream.risks), in.upstream.unchecked
		if err == nil && in.review != nil {
			v.Unacknowledged = apiRisks(order.Unacknowledged(in.upstream.risks, in.review.Acknowledged))
			if len(v.Unacknowledged) > 0 {
				err = api.Reject(api.CodeOrderRiskUnacknowledged, "/args/review_id")
			}
		}
		if rej, ok := kernel.AsRejection(err); ok {
			v.Code, v.Path = rej.Code, rej.Fields[0].Path
			return nil
		}
		v.Activatable = err == nil && reviewID != ""
		return err
	})
	return v, err
}
