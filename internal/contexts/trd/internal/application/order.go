package application

import (
	"context"
	"slices"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The order commands (WBS S3-07, the order half of S3-13; api/order.go): order.prepare and
// order.review.request (human or AI), order.review.decide, order.approval.revoke, order.activate,
// order.cancel and order.close_unfulfilled (human only).
//
// Lock order (tactical lockOrder, ADR-BR-002, ADR-BR-011): every order command locks the quote root
// the order converts FOR UPDATE first — the conflict key of quotes, acceptances and orders, which
// also holds the object control (TRD-INV-015), the conversion slot and, by the acceptance commands
// locking it first too, the acceptances' effectivity — then the order FOR UPDATE (order.prepare: the
// order ID, to refuse a taken one), then, through the ledger's Authorize, the company's authority
// guard row FOR SHARE. Inside the transaction TRD decides on the locked aggregates what
// QuoteRevisionGuard v1 and AcceptanceGuard v1 answer callers that do not own the root
// (quote.Usable, acceptance.Check), as those guards' contracts say of TRD's own order step. The
// reviewers' grants are read FOR SHARE through the authority guard (Authorizer.Qualify), the first
// upstream guard in lock order; the upstream guards of the source revision follow in context code
// order (order_upstream.go).

const (
	CommandGetOrder   = "order.get"
	CommandCheckOrder = "order.check"
)

// orderWrites are the commands of this file; a replay of one passes the quote root's object control
// as the write would (Visible).
var orderWrites = []string{api.CommandPrepareOrder, api.CommandRequestOrderReview, api.CommandDecideOrderReview, api.CommandActivateOrder,
	api.CommandRevokeOrderApproval, api.CommandCancelOrder, api.CommandCloseOrderUnfulfilled, api.CommandPrepareAmendment,
	api.CommandSubmitAmendment, api.CommandDecideAmendment, api.CommandApplyAmendment, api.CommandWithdrawAmendment,
	api.CommandCloseOrderRemainder, api.CommandCloseOrder, api.CommandGenerateOrderDocument, api.CommandRecordSignedOrderDocument}

// orderCommand locks the quote root an order converts and the order, and applies one command.
// order.prepare names the quote; the others resolve it from the order, whose quote never changes.
type orderCommand struct {
	s         Service
	orderID   string
	quoteID   string // order.prepare: the quote named
	q         *quote.Quote
	o         *order.Order
	from      int64 // the order's row version as locked
	qfrom     int64 // the quote root's version as locked
	qfromCtl  int64 // the quote root's control epoch as locked
	prepare   func(ctx context.Context) error
	apply     func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
	reviewID  string // the review the command acted on, for the receipt
	preparing bool
}

func (h *orderCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.q, h.o, h.from, h.qfrom, h.qfromCtl = nil, nil, 0, 0, 0
	quoteID := h.quoteID
	if !h.preparing {
		id, err := postgres.OrderQuote(ctx, exec, scope(ctx), h.orderID)
		if err != nil || id == "" {
			return err
		}
		quoteID = id
	}
	q, err := postgres.LoadQuote(ctx, exec, scope(ctx), quoteID, postgres.ForUpdate)
	if err != nil || q == nil {
		return err
	}
	h.q, h.qfrom, h.qfromCtl = q, q.RootVersion, q.Control.Epoch
	if h.o, err = postgres.LoadOrder(ctx, exec, scope(ctx), h.orderID, postgres.ForUpdate); err != nil {
		return err
	}
	if h.o != nil {
		h.from = h.o.RowVersion
	}
	return nil
}

func (h *orderCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *orderCommand) Prepare(ctx context.Context) error {
	if h.prepare == nil {
		return nil
	}
	return h.prepare(ctx)
}

// admit runs the checks every order command starts with: the quote exists and the caller sees it,
// and the order exists unless the command prepares it (NOT_FOUND at path: an order of a quote the
// caller does not see is not found either), then the quote root's object control.
func (h *orderCommand) admit(ctx context.Context, exec bentopg.Executor, d authorization.Decision, path string) error {
	if h.q == nil || !d.Sees(h.q.OwnerActor, h.q.OwnerTeam) || (h.o == nil && !h.preparing) {
		return kernel.Reject(kernel.CodeNotFound, path, "")
	}
	return h.q.Control.Admit(fence(ctx, exec, d))
}

// save writes the order and, when the command changed the quote root (order.prepare took its slot),
// the root first.
func (h *orderCommand) save(ctx context.Context, exec bentopg.Executor, status string, events ...action.Event) (action.Outcome, error) {
	now := h.s.Clock.Now()
	if h.q.RootVersion != h.qfrom {
		if err := postgres.SaveQuote(ctx, exec, scope(ctx), h.q, false, h.qfrom, h.qfromCtl, now); err != nil {
			return action.Outcome{}, err
		}
	}
	if err := postgres.SaveOrder(ctx, exec, scope(ctx), h.o, h.from, now); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: h.o.ID, RowVersion: h.o.RowVersion, Status: status, Events: events}, nil
}

// event is a SalesOrder event at the row version the command moved the order to: every event of one
// order shares its partition and is delivered in order.
func (h *orderCommand) event(eventType string, fields map[string]any) action.Event {
	o := h.o
	base := map[string]any{"order_id": o.ID, "quote_id": o.Source.QuoteID, "business_revision": o.Source.Revision,
		"business_hash": o.Source.BusinessHash, "order_revision": o.OrderRevision, "order_hash": o.OrderHash, "order_status": string(o.Status)}
	for k, v := range fields {
		base[k] = v
	}
	return action.Event{Type: eventType, AggregateType: "SalesOrder", AggregateID: o.ID, AggregateVersion: o.RowVersion, Fields: base}
}

// standings reads an actor's standing now through the authority guard rows (Authorizer.Qualify, FOR
// SHARE), with whether it sees quote q.
func (s Service) standings(ctx context.Context, exec bentopg.Executor, q *quote.Quote) quote.Standings {
	return func(actorID string) (quote.Standing, error) {
		h, err := s.Ledger.Authorizer.Qualify(ctx, exec, actorID)
		if err != nil {
			return quote.Standing{}, err
		}
		return quote.Standing{Human: h.Active && h.Actor.Kind == kernel.ActorHuman, Permissions: h.Permissions,
			SeesRoot: h.Sees(q.OwnerActor, q.OwnerTeam)}, nil
	}
}

// PrepareOrder converts an accepted formal revision into a sales order draft and takes the quote's
// conversion slot (order.prepare).
func (s Service) PrepareOrder(ctx context.Context, cmd api.PrepareOrder) (api.OrderReceipt, error) {
	h := &orderCommand{s: s, orderID: cmd.OrderID, quoteID: cmd.QuoteID, preparing: true}
	var notify *quote.AddressRef
	if n := cmd.NotifyPartyRef; n != nil {
		notify = &quote.AddressRef{PartyID: n.PartyID, PartyRevision: n.PartyRevision, AddressID: n.AddressID}
	}
	var seal *orderrev.SealFacts
	h.prepare = func(ctx context.Context) error {
		var err error
		seal, err = s.prepareSeal(ctx, cmd.QuoteID, cmd.BusinessRevision, cmd.BusinessHash, notify)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, exec, d, "/args/quote_id"); err != nil {
			return action.Outcome{}, err
		}
		if h.o != nil && h.o.Source.QuoteID != h.q.ID {
			return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/order_id")
		}
		a, err := postgres.LoadAcceptance(ctx, exec, scope(ctx), cmd.AcceptanceID, postgres.NoLock)
		if err != nil {
			return action.Outcome{}, err
		}
		o, err := order.Prepare(order.Preparation{OrderID: cmd.OrderID, QuoteID: cmd.QuoteID, Revision: cmd.BusinessRevision,
			BusinessHash: cmd.BusinessHash, AcceptanceID: cmd.AcceptanceID, NotifyParty: notify, Seal: seal}, h.q, a, scope(ctx), d.Actor, d.Epoch,
			s.Clock.Now())
		if err != nil {
			return action.Outcome{}, err
		}
		// The notify party (S5R-D40): its address current now, its party row held FOR SHARE until
		// the order commits (PTY AddressRoleGuard v1, R13).
		if notify != nil {
			if err = guardAddress(ctx, exec, ptyRef(*notify), "/args/notify_party_ref"); err != nil {
				return action.Outcome{}, err
			}
		}
		h.o = o
		src := o.Source
		return h.save(ctx, exec, string(o.Status), h.event(api.EventOrderPrepared, map[string]any{"number": src.Number,
			"acceptance_id": src.AcceptanceID, "acceptance_effectivity_version": src.AcceptanceVersion,
			"arrival_at": kernel.FormatTimestamp(src.ArrivalAt), "arrival_basis": src.ArrivalBasis,
			"quote_expires_at": kernel.FormatTimestamp(src.ExpiresAt), "conversion_slot": o.Slot,
			"customer_party_id": a.Customer.PartyID, "customer_revision": a.Customer.Revision}))
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandPrepareOrder, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.OrderReceipt{}, err
	}
	return s.orderReceipt(ctx, res, "")
}

// RequestOrderReview opens a review of a draft order (order.review.request).
func (s Service) RequestOrderReview(ctx context.Context, cmd api.RequestOrderReview) (api.OrderReceipt, error) {
	h := &orderCommand{s: s, orderID: cmd.OrderID, reviewID: cmd.ReviewID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, exec, d, "/args/order_id"); err != nil {
			return action.Outcome{}, err
		}
		if err := h.o.Check(cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		r, err := h.o.RequestReview(cmd.ReviewID, d.Actor, d.Epoch, s.Clock.Now())
		if err != nil {
			return action.Outcome{}, err
		}
		return h.save(ctx, exec, string(r.Status), h.event(api.EventOrderReviewRequested, map[string]any{"review_id": r.ID,
			"review_status": string(r.Status)}))
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandRequestOrderReview, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.OrderReceipt{}, err
	}
	return s.orderReceipt(ctx, res, cmd.ReviewID)
}

// DecideOrderReview approves (acknowledging the upstream risks found now) or rejects a review of a
// draft order (order.review.decide).
func (s Service) DecideOrderReview(ctx context.Context, cmd api.DecideOrderReview) (api.OrderReceipt, error) {
	h := &orderCommand{s: s, orderID: cmd.OrderID, reviewID: cmd.ReviewID}
	decision := order.Decision{ReviewID: cmd.ReviewID, OrderHash: cmd.OrderHash, Approve: cmd.Decision == api.DecisionApprove,
		Reason: cmd.Reason, Evidence: evidence(cmd.Evidence)}
	if t, err := kernel.ParseTimestamp(cmd.ExpiresAt); err == nil {
		decision.ExpiresAt = t
	}
	for _, r := range cmd.RiskAcknowledgements {
		decision.Acknowledged = append(decision.Acknowledged, order.Risk{Path: r.Path, Code: r.Code, Detail: r.Detail})
	}
	var readable map[string]bool
	var fx salesFX
	h.prepare = func(ctx context.Context) error {
		var err error
		if readable, err = s.readEvidence(ctx, evidenceIDs(decision.Evidence)); err != nil {
			return err
		}
		if !decision.Approve {
			return nil
		}
		fx, err = s.orderFX(ctx, cmd.OrderID)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, exec, d, "/args/order_id"); err != nil {
			return action.Outcome{}, err
		}
		if err := h.o.Check(cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		own, err := s.Ledger.Authorizer.Qualify(ctx, exec, d.Actor.ID)
		if err != nil {
			return action.Outcome{}, err
		}
		if !own.Holds(order.PermissionRead) {
			// A reviewer reads what it approves now (T04 A41), as a quote reviewer must (T03 A33).
			return action.Outcome{}, kernel.Reject(kernel.CodePermissionDenied, "/", "")
		}
		now := s.Clock.Now()
		r, err := h.o.Decide(decision, d.Actor, d.Epoch, func() ([]order.Risk, error) {
			// The contract still formed and the source revision still in force (TRD-INV-017), then
			// what changed upstream since.
			src, err := s.orderSource(ctx, exec, h, "/args/order_id")
			if err != nil {
				return nil, err
			}
			found, err := s.orderUpstream(ctx, exec, h, src, fx, false, now)
			return found.risks, err
		}, now)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = quote.CheckDraft(quote.Data{Evidence: decision.Evidence}, readable); err != nil {
			return action.Outcome{}, moved(err, "/args/data/evidence", "/args/evidence")
		}
		fields := map[string]any{"review_id": r.ID, "decision": cmd.Decision, "review_status": string(r.Status)}
		if r.Status == order.ReviewApproved {
			fields["expires_at"], fields["risks_acknowledged"] = kernel.FormatTimestamp(r.ExpiresAt), len(r.Acknowledged)
		}
		return h.save(ctx, exec, string(r.Status), h.event(api.EventOrderReviewDecided, fields))
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandDecideOrderReview, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.OrderReceipt{}, err
	}
	return s.orderReceipt(ctx, res, cmd.ReviewID)
}

// RevokeOrderApproval revokes an approved review (order.approval.revoke).
func (s Service) RevokeOrderApproval(ctx context.Context, cmd api.RevokeOrderApproval) (api.OrderReceipt, error) {
	h := &orderCommand{s: s, orderID: cmd.OrderID, reviewID: cmd.ReviewID}
	proofs := evidence(cmd.Evidence)
	var readable map[string]bool
	h.prepare = func(ctx context.Context) error {
		var err error
		readable, err = s.readEvidence(ctx, evidenceIDs(proofs))
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, exec, d, "/args/order_id"); err != nil {
			return action.Outcome{}, err
		}
		r, err := h.o.Revoke(cmd.ReviewID, quote.Act{By: d.Actor, Epoch: d.Epoch, At: s.Clock.Now(), Reason: cmd.Reason, Evidence: proofs})
		if err != nil {
			return action.Outcome{}, err
		}
		if err = quote.CheckDraft(quote.Data{Evidence: proofs}, readable); err != nil {
			return action.Outcome{}, moved(err, "/args/data/evidence", "/args/evidence")
		}
		return h.save(ctx, exec, string(r.Status), h.event(api.EventOrderApprovalRevoked, map[string]any{"review_id": r.ID,
			"review_status": string(r.Status), "activated_order": h.o.ActivatedReview == r.ID}))
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandRevokeOrderApproval, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.OrderReceipt{}, err
	}
	return s.orderReceipt(ctx, res, cmd.ReviewID)
}

// CloseOrderUnfulfilled closes a draft order unfulfilled (order.close_unfulfilled).
func (s Service) CloseOrderUnfulfilled(ctx context.Context, cmd api.CloseOrderUnfulfilled) (api.OrderReceipt, error) {
	return s.closeOrder(ctx, api.CommandCloseOrderUnfulfilled, api.EventOrderClosedUnfulfilled, cmd.IdempotencyKey, cmd.OrderID,
		cmd.ExpectedVersion, cmd.Reason, cmd.Evidence, cmd, cmd.Validate(), (*order.Order).CloseUnfulfilled)
}

func (s Service) closeOrder(ctx context.Context, name, eventType, key, id string, expected int64, reason string, proofs []api.Evidence,
	req api.Command, validation error, end func(*order.Order, quote.Act) (*order.Closure, error)) (api.OrderReceipt, error) {
	h := &orderCommand{s: s, orderID: id}
	evidenced := evidence(proofs)
	var readable map[string]bool
	h.prepare = func(ctx context.Context) error {
		var err error
		readable, err = s.readEvidence(ctx, evidenceIDs(evidenced))
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, exec, d, "/args/order_id"); err != nil {
			return action.Outcome{}, err
		}
		if err := h.o.Check(expected); err != nil {
			return action.Outcome{}, err
		}
		c, err := end(h.o, quote.Act{By: d.Actor, Epoch: d.Epoch, At: s.Clock.Now(), Reason: reason, Evidence: evidenced})
		if err != nil {
			return action.Outcome{}, err
		}
		if err = quote.CheckDraft(quote.Data{Evidence: evidenced}, readable); err != nil {
			return action.Outcome{}, moved(err, "/args/data/evidence", "/args/evidence")
		}
		return h.save(ctx, exec, string(c.Status), h.event(eventType, map[string]any{"conversion_slot": h.o.Slot}))
	}
	res, err := s.run(ctx, action.Command{Name: name, IdempotencyKey: key, ObjectID: id, Reason: reason}, req, validation, h)
	if err != nil {
		return api.OrderReceipt{}, err
	}
	return s.orderReceipt(ctx, res, "")
}

// orderReceipt completes the receipt of an order command, executed or replayed, from the stored
// order: what it binds and, for a review command or an activation, the review.
func (s Service) orderReceipt(ctx context.Context, r api.Receipt, reviewID string) (api.OrderReceipt, error) {
	out := api.OrderReceipt{Receipt: r}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		o, err := postgres.LoadOrder(ctx, exec, scope(ctx), r.ObjectID, postgres.NoLock)
		if err != nil || o == nil {
			return err
		}
		out.QuoteID, out.BusinessRevision, out.BusinessHash, out.AcceptanceID = o.Source.QuoteID, o.Source.Revision, o.Source.BusinessHash,
			o.Source.AcceptanceID
		out.OrderRevision, out.OrderHash, out.OrderStatus = o.OrderRevision, o.OrderHash, string(o.Status)
		if reviewID == "" {
			if c := o.ActivatedAt(r.RowVersion); c != nil {
				reviewID = c.ReviewID
			}
		}
		if rv := o.Review(reviewID); rv != nil {
			out.ReviewID, out.ReviewStatus = rv.ID, string(rv.Status)
		}
		return nil
	})
	return out, err
}

// orderOf resolves, in a TRD transaction of its own before a command's, order id and its quote, for
// what a command reads upstream before its transaction; nil when either is unknown.
func (s Service) orderOf(ctx context.Context, id string) (*order.Order, *quote.Quote, error) {
	var o *order.Order
	var q *quote.Quote
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		if o, err = postgres.LoadOrder(ctx, exec, scope(ctx), id, postgres.NoLock); err != nil || o == nil {
			return err
		}
		q, err = postgres.LoadQuote(ctx, exec, scope(ctx), o.Source.QuoteID, postgres.NoLock)
		return err
	})
	if q == nil {
		o = nil
	}
	return o, q, err
}

// orderView is an order as GetOrder shows it at now.
func orderView(o *order.Order, now time.Time) api.OrderView {
	src := o.Source
	v := api.OrderView{OrderID: o.ID, QuoteID: src.QuoteID, BusinessRevision: src.Revision, BusinessHash: src.BusinessHash, Number: src.Number,
		DocumentSHA256: src.DocumentSHA256, AcceptanceID: src.AcceptanceID, AcceptanceVersion: src.AcceptanceVersion, ArrivalAt: src.ArrivalAt,
		ArrivalBasis: src.ArrivalBasis, QuoteExpiresAt: src.ExpiresAt, Slot: o.Slot, OrderRevision: o.OrderRevision, OrderHash: o.OrderHash,
		Snapshot: string(o.Snapshot), Status: string(o.Status), RowVersion: o.RowVersion, PreparedBy: o.Prepared.By, PreparedAt: o.Prepared.At,
		ActivatedReviewID: o.ActivatedReview}
	for _, r := range o.Reviews {
		rv := api.OrderReviewView{ReviewID: r.ID, OrderRevision: r.OrderRevision, OrderHash: r.OrderHash, Status: string(r.Status),
			RequestedBy: r.Requested.By, RequestedAt: r.Requested.At, ExpiresAt: r.ExpiresAt, Acknowledged: apiRisks(r.Acknowledged),
			Activated: o.ActivatedReview == r.ID}
		switch {
		case r.Status == order.ReviewOpen:
			rv.DecisionState = "pending"
		case r.Status == order.ReviewApproved && !now.Before(r.ExpiresAt):
			rv.DecisionState = "expired"
		default:
			rv.DecisionState = string(r.Status)
		}
		if d := r.Decided; d != nil {
			rv.DecidedBy, rv.DecidedAt, rv.Reason, rv.Evidence = d.By, d.At, d.Reason, apiEvidence(d.Evidence)
		}
		if x := r.Revoked; x != nil {
			rv.RevokedBy, rv.RevokedAt, rv.RevokeReason = x.By, x.At, x.Reason
		}
		v.Reviews = append(v.Reviews, rv)
	}
	for _, c := range o.Commits {
		entries := map[string]string{}
		for _, e := range c.Entries {
			entries[e.Name] = e.Value
		}
		doc, _ := c.EntriesJSON()
		v.Commits = append(v.Commits, api.OrderCommitView{IdempotencyKey: c.IdempotencyKey, OrderRevision: c.OrderRevision, OrderHash: c.OrderHash,
			ReviewID: c.ReviewID, ApprovalID: c.ApprovalID, CostSnapshotID: c.CostSnapshotID, AuthorityEpoch: c.AuthorityEpoch,
			ControlEpoch: c.ControlEpoch, RowVersion: c.RowVersion, CommittedBy: c.By, CommittedAt: c.At, Entries: entries,
			EntriesSHA256: quote.Hash(doc)})
	}
	if c := o.Closure; c != nil {
		v.Closure = &api.OrderClosureView{Status: string(c.Status), ClosedBy: c.Closed.By, ClosedAt: c.Closed.At, Reason: c.Closed.Reason,
			Evidence: apiEvidence(c.Closed.Evidence), RowVersion: c.RowVersion}
	}
	amendmentViews(o, &v)
	v.Documents = documentViews(o)
	for _, r := range o.Risks {
		d := r.Denial
		v.RiskRecords = append(v.RiskRecords, api.OrderRiskRecordView{RiskID: r.ID, Cause: r.Cause, Code: r.Code, OrderRevision: r.OrderRevision,
			OrderRowVersion: r.OrderRowVersion, CostSnapshotID: d.SnapshotID, CostSourceID: d.SourceID, AttestationID: d.AttestationID,
			EventID: d.EventID, RegisteredBy: r.Registered.By, RegisteredAt: r.Registered.At})
	}
	return v
}

func apiRisks(rs []order.Risk) []api.Risk {
	if rs == nil {
		return nil
	}
	out := make([]api.Risk, len(rs))
	for i, r := range rs {
		out[i] = api.Risk{Path: r.Path, Code: r.Code, Detail: r.Detail}
	}
	return out
}

// GetOrder returns an order of a quote the caller sees, audited as order.get (trd.read).
func (s Service) GetOrder(ctx context.Context, id string) (api.OrderView, error) {
	var v api.OrderView
	err := s.Ledger.Read(ctx, CommandGetOrder, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		o, err := postgres.LoadOrder(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if o == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		root, err := postgres.LoadRoot(ctx, exec, scope(ctx), o.Source.QuoteID)
		if err != nil {
			return err
		}
		if root == nil || !d.Sees(root.OwnerActor, root.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		if _, err = o.Verify(scope(ctx), "/id"); err != nil {
			return err
		}
		v = orderView(o, kernel.Instant(s.Clock.Now()))
		return nil
	})
	return v, err
}

// ResolveActiveOrders answers ActiveOrderRef v1 lookups (api.ActiveOrderLookups): only an active
// grant is needed; orders of quotes outside the caller's data scope are absent.
func (s Service) ResolveActiveOrders(ctx context.Context, ids []string) (map[string]api.ActiveOrderFact, error) {
	if err := api.ValidateOrderIDs(ids); err != nil {
		return nil, err
	}
	out := map[string]api.ActiveOrderFact{}
	err := s.lookupAcceptances(ctx, func(ctx context.Context, exec bentopg.Executor, sees func(quoteID string) (bool, error)) error {
		for _, id := range ids {
			o, err := postgres.LoadOrder(ctx, exec, scope(ctx), id, postgres.NoLock)
			if err != nil {
				return err
			}
			if o == nil {
				continue
			}
			ok, err := sees(o.Source.QuoteID)
			if err != nil {
				return err
			}
			if ok {
				out[id] = activeOrderFact(o, scope(ctx))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The Terms block's Incoterm and edition, after TRD's read transaction (order_snapshot.go).
	return out, s.stateActiveTrade(ctx, out)
}

// activeOrderFact is what ActiveOrderRef v1 says of an order now.
func activeOrderFact(o *order.Order, sc kernel.Scope) api.ActiveOrderFact {
	f := api.ActiveOrderFact{Binding: api.OrderBinding{OrderID: o.ID, OrderRevision: o.OrderRevision, OrderHash: o.OrderHash},
		Status: string(o.Status), RowVersion: o.RowVersion, QuoteID: o.Source.QuoteID, BusinessRevision: o.Source.Revision,
		BusinessHash: o.Source.BusinessHash, AcceptanceID: o.Source.AcceptanceID, ReviewID: o.ActivatedReview}
	if len(o.Commits) > 0 {
		f.ActivatedAt = o.Commits[len(o.Commits)-1].At
	}
	if r := o.Review(o.ActivatedReview); r != nil && r.Status == order.ReviewRevoked {
		f.ApprovalRevoked = true
	}
	snap, err := o.Verify(sc, "/order_ids")
	switch {
	case err != nil:
		f.Code = kernel.CodeReferenceInvalid
		return f
	case o.Status != order.Active:
		f.Code = api.CodeOrderNotActive
	case f.ApprovalRevoked:
		f.Code = api.CodeOrderApprovalRevoked
	}
	d := snap.Commercial.Data
	f.CustomerRef = api.PartyRef{PartyID: d.Customer.PartyID, Revision: d.Customer.Revision}
	f.CurrencyRef = apiRef(d.Currency)
	f.RfqRef = api.RfqRef{RfqID: d.Rfq.RfqID, RowVersion: d.Rfq.RowVersion}
	f.CostSnapshotRef = api.CostSnapshotRef{SnapshotID: d.Snapshot.SnapshotID, ResultHash: d.Snapshot.ResultHash}
	// The lines, delivery and terms are the current revision's: an applied amendment changes them
	// (S3-15).
	r, err := o.Revision()
	if err != nil {
		f.Code = kernel.CodeReferenceInvalid
		return f
	}
	f.Delivery = apiDelivery(r.Terms.Delivery)
	f.Lines = orderLines(r)
	if f.Terms, err = orderTerms(snap, r); err != nil {
		f.Code = kernel.CodeReferenceInvalid
	}
	return f
}

// joinGuards names extension points in one event field.
func joinGuards(names []string) string { return strings.Join(slices.Clone(names), ",") }
