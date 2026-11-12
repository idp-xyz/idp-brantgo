package application

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Purchase commitment changes and cancellation (WBS S5-19; domain/commitment change.go; api/change.go;
// PUR-INV-011, SM-PUR-COMMITMENT, SM-PUR-COMMITMENT-CHANGE).
//
// Lock order (ADR-BR-011, tactical lockOrder "承诺根 FOR UPDATE，再锁需求分配根，再锁上游守卫 FOR SHARE"): the
// commitment root FOR UPDATE; the DemandAllocation roots of its draft's lines FOR UPDATE in
// (order_id, order_line_id) order, after which the draft is read again (lockCommitment); for a
// decision and an application then the change row FOR UPDATE, which only ever happens under its
// commitment's lock; the verified deliveries of the commitment are read without a lock — every
// verification locks this commitment root FOR UPDATE before it writes, so they are as the last one
// committed and no other can commit until this command does (B3; commitment commands never lock a
// delivery); then the company's authority guard FOR SHARE (the ledger's Authorize); then, for a
// preparation, a decision and an application, the upstream guards in context code order (CAT, CST,
// POL, PTY, SUP, TRD OrderGuard on the order revision the proposed revision binds), each FOR SHARE
// until PUR commits; then the rows, the ledger's result, audit and Outbox. A cancellation calls no
// upstream guard: it only removes from what DownstreamCommitments v1 shows (S5 decision D-1).
//
// A change's preparation, decision and application each prepare the proposed revision again: before
// the transaction they read the facts (facts) for the draft's request with the change's lines; inside
// it the draft's preparation (draft.Prepare with draft.Change) runs against the allocation roots
// without the commitment's own entries — an increase beyond the line's room is
// PURCHASE_ALLOCATION_EXCEEDED —, and the decision and the application require the result equal to
// the document sealed at the preparation (PURCHASE_DRAFT_STALE otherwise, as PUR-INV-009). Refusals of
// a preparation point at the change's request (/args/lines/<i>/...), those of a decision and an
// application into the change (/change/lines/<i>/...).

var changeCmds = []string{api.CommandPrepareCommitmentChange, api.CommandDecideCommitmentChange, api.CommandApplyCommitmentChange}

// changeRequestOf is the domain's request for cmd, whose structure Validate checked.
func changeRequestOf(cmd api.PrepareCommitmentChange) commitment.ChangeRequest {
	r := commitment.ChangeRequest{ChangeID: cmd.ChangeID, CommitmentID: cmd.CommitmentID, Revision: cmd.Revision, Reason: cmd.Reason,
		Evidence: evidenceOf(cmd.Evidence)}
	for _, l := range cmd.Lines {
		q, _ := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		x := commitment.ChangeLine{OrderLineID: l.OrderLineID, Quantity: q, SupplierDate: l.SupplierDate}
		if o := l.Overbuy; o != nil {
			oq, _ := kernel.ParseDecimal(o.Quantity, kernel.QuantityFormat)
			x.Overbuy = &draft.Overbuy{Quantity: oq, Reason: o.Reason}
		}
		if s := l.Source; s != nil {
			x.Source = &draft.Source{Ref: draft.Ref{Kind: s.Kind, ID: s.ID, Revision: s.Revision}, ConditionLineID: s.ConditionLineID}
		}
		r.Lines = append(r.Lines, x)
	}
	return r
}

// proposed returns err with a path of the draft's preparation pointed at the change: a request line
// k at line index[k] of the change, the order at the commitment (prefix "/args/") or at the change's
// order (prefix "/change/"); every other path keeps its place under prefix.
func proposed(err error, index []int, prefix string) error {
	rej, ok := kernel.AsRejection(err)
	if !ok || len(rej.Fields) == 0 {
		return err
	}
	out := *rej
	out.Fields = slices.Clone(rej.Fields)
	p := out.Fields[0].Path
	switch {
	case strings.HasPrefix(p, "/args/lines/"):
		rest := strings.TrimPrefix(p, "/args/lines/")
		k, tail, _ := strings.Cut(rest, "/")
		if n, err := strconv.Atoi(k); err == nil && n >= 0 && n < len(index) {
			p = prefix + "lines/" + strconv.Itoa(index[n])
			if tail != "" {
				p += "/" + tail
			}
		}
	case p == "/args/order_id" && prefix == "/args/":
		p = "/args/commitment_id"
	case strings.HasPrefix(p, "/args/"):
		p = prefix + strings.TrimPrefix(p, "/args/")
	}
	out.Fields[0].Path = p
	return &out
}

// revisionFacts is what a change command read before its transaction to prepare the proposed
// revision again: the proposal of the change's request and the facts it is judged on.
type revisionFacts struct {
	read     bool
	proposal commitment.Proposal
	in       draft.Inputs
	order    *Order
}

func (s Service) proposalFacts(ctx context.Context, r commitment.ChangeRequest, dr *draft.Draft, at time.Time) (revisionFacts, error) {
	if dr == nil {
		return revisionFacts{}, nil
	}
	p := r.Propose(dr)
	if len(p.Request.Lines) == 0 {
		return revisionFacts{}, nil
	}
	in, order, err := s.facts(ctx, p.Request, at)
	return revisionFacts{read: true, proposal: p, in: in, order: order}, err
}

// revise prepares the proposal of f again at instant now as commitment c's next revision, against
// copies of roots without the draft's own entries, and returns the preparation, its refusals
// pointed at the change under prefix: the order (REFERENCE_INVALID when the actor does not read it,
// TRD's verdict), the draft's preparation (SUP's, CAT's, POL's, PTY's verdicts, PURCHASE_ALLOCATION_EXCEEDED
// for an increase beyond the line's room ...), another supplier (SUPPLIER_MISMATCH) or currency
// (PURCHASE_TERMS_INCONSISTENT) than the commitment's, and a line whose specification revision or unit
// is not the draft's (SPECIFICATION_MISMATCH).
func (s Service) revise(ctx context.Context, f revisionFacts, c *commitment.Commitment, dr *draft.Draft, roots map[allocation.Key]*allocation.Allocation,
	preparer kernel.ActorRef, epoch int64, now time.Time, prefix string) (*draft.Prepared, error) {
	if !f.read {
		return nil, kernel.Reject(kernel.CodeVersionConflict, prefix+"lines", "The commitment changed while the command was prepared")
	}
	if f.order == nil || f.in.Order == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, prefix+"order_id", "The actor does not read the commitment's order")
	}
	alloc := make(map[allocation.Key]*allocation.Allocation, len(roots))
	for k, a := range roots {
		alloc[k] = a.Without(dr.ID)
	}
	in := f.in
	in.Scope, in.Now, in.Preparer, in.Epoch, in.Allocations = scope(ctx), kernel.Instant(now), preparer, epoch, alloc
	in.Change = &draft.Change{Revision: c.Revision + 1, Dates: f.proposal.Dates}
	p, err := draft.Prepare(f.proposal.Request, in)
	if err != nil {
		return nil, proposed(err, f.proposal.Index, prefix)
	}
	first := prefix + "lines/" + strconv.Itoa(f.proposal.Index[0]) + "/source"
	switch {
	case p.Doc.Supplier.PartyID != c.SupplierID:
		return nil, rejectAs(api.CodeSupplierMismatch, first, "A commitment buys from its supplier")
	case p.Doc.Currency != c.Currency:
		return nil, rejectAs(api.CodePurchaseTermsInconsistent, first, "A commitment keeps its currency")
	}
	doc, err := dr.Decode()
	if err != nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/commitment_id", "The stored draft is not intact")
	}
	for _, l := range p.Doc.Lines {
		i := slices.IndexFunc(doc.Lines, func(x draft.DocLine) bool { return x.OrderLineID == l.OrderLineID })
		if i < 0 || doc.Lines[i].Sku != l.Sku || doc.Lines[i].UnitRef != l.UnitRef {
			n := slices.IndexFunc(f.proposal.Request.Lines, func(x draft.RequestLine) bool { return x.OrderLineID == l.OrderLineID })
			at := prefix + "lines"
			if n >= 0 {
				at = prefix + "lines/" + strconv.Itoa(f.proposal.Index[n])
			}
			return nil, rejectAs(api.CodeSpecificationMismatch, at, "A revision buys the specification revision and unit its draft line buys")
		}
	}
	relocateTo(p, f.proposal.Index, prefix)
	return p, nil
}

// relocateTo points the references p adopted at the change under prefix.
func relocateTo(p *draft.Prepared, index []int, prefix string) {
	to := func(path string) string {
		err := proposed(kernel.Reject(kernel.CodeReferenceInvalid, path, ""), index, prefix)
		rej, _ := kernel.AsRejection(err)
		return rej.Fields[0].Path
	}
	for k, path := range p.Specs {
		p.Specs[k] = to(path)
	}
	for k, u := range p.Configs {
		u.Path = to(u.Path)
		p.Configs[k] = u
	}
	for k, u := range p.Sources {
		u.Path = to(u.Path)
		p.Sources[k] = u
	}
	for i := range p.Parties {
		p.Parties[i].Path = to(p.Parties[i].Path)
	}
}

// guardRevision runs the upstream guards on what p adopted and on the order the revision binds: a
// refusal at the order is at the change's order (prefix), one at another revision or hash
// PURCHASE_DRAFT_STALE.
func (s Service) guardRevision(ctx context.Context, exec bentopg.Executor, p *draft.Prepared, prefix string, at time.Time) (adoption, error) {
	o := p.Doc.Order
	a, err := s.guard(ctx, exec, p, OrderBinding{OrderID: o.OrderID, OrderRevision: o.OrderRevision, OrderHash: o.OrderHash}, at)
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 && rej.Fields[0].Path == "/args/order_id" {
		target := prefix + "order_id"
		if prefix == "/args/" {
			target = "/args/commitment_id"
		}
		if rej.Code == api.CodeReferenceStale {
			return a, rejectAs(api.CodePurchaseDraftStale, target, "The order moved since the change was prepared")
		}
		return a, moved(err, "/args/order_id", target)
	}
	return a, err
}

// keeps refuses lines buying less than verified (SUPPLIER_DELIVERY_EXCEEDED), at the change's line
// under prefix.
func keeps(lines []commitment.LineQuantity, verified map[string]kernel.Decimal, order []string, prefix string) error {
	return commitment.Keeps(lines, verified, func(id string) string {
		if i := slices.Index(order, id); i >= 0 {
			return prefix + "lines/" + strconv.Itoa(i) + "/quantity"
		}
		return prefix + "lines"
	})
}

func changeOutcome(x *commitment.Change, events ...action.Event) action.Outcome {
	return action.Outcome{ObjectID: x.ID, RowVersion: x.RowVersion, Status: x.Status, Events: events}
}

// heldChange is what a decision or an application holds: the commitment root with its draft and
// allocation roots, and the change.
type heldChange struct {
	l locked
	x *commitment.Change
}

// lockChange locks the commitment of change id FOR UPDATE, then its draft's allocation roots, then
// the change row.
func lockChange(ctx context.Context, exec bentopg.Executor, id string) (heldChange, error) {
	sc := scope(ctx)
	cid, err := postgres.ChangeCommitment(ctx, exec, sc, id)
	if err != nil || cid == "" {
		return heldChange{}, err
	}
	l, err := lockCommitment(ctx, exec, cid, true)
	if err != nil || l.c == nil {
		return heldChange{l: l}, err
	}
	x, err := postgres.LoadChange(ctx, exec, sc, id, postgres.ForUpdate)
	return heldChange{l: l, x: x}, err
}

// changeFacts reads change id, its commitment and draft without locks and, for a change still to be
// judged, the facts its proposed revision is prepared again on, as the actor sees them now.
func (s Service) changeFacts(ctx context.Context, id string) (revisionFacts, error) {
	var l locked
	var x *commitment.Change
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		if x, err = postgres.LoadChange(ctx, exec, scope(ctx), id, postgres.NoLock); err != nil || x == nil {
			return err
		}
		if l.c, err = postgres.LoadCommitment(ctx, exec, scope(ctx), x.CommitmentID, postgres.NoLock); err != nil || l.c == nil {
			return err
		}
		l.dr, err = postgres.LoadDraft(ctx, exec, scope(ctx), l.c.DraftID, postgres.NoLock)
		return err
	})
	if err != nil || x == nil || l.dr == nil {
		return revisionFacts{}, err
	}
	r, err := commitment.DecodeChangeRequest(x.Request)
	if err != nil {
		return revisionFacts{}, nil //nolint:nilerr // a request that does not decode is refused in the transaction
	}
	return s.proposalFacts(ctx, r, l.dr, s.Clock.Now())
}

// changeDraft refuses a change whose commitment d's actor does not see (NOT_FOUND at /args/change_id)
// and a draft that is not intact (REFERENCE_INVALID).
func changeDraft(ctx context.Context, d authorization.Decision, h heldChange) error {
	if h.x == nil || h.l.c == nil || !d.Sees(h.l.c.OwnerActor, h.l.c.OwnerTeam) {
		return kernel.Reject(kernel.CodeNotFound, "/args/change_id", "")
	}
	if h.l.dr == nil {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/change_id", "The commitment's draft is missing")
	}
	return h.l.dr.Verify(scope(ctx), "/args/change_id")
}

// lineOrder returns the change's order line IDs in the order its request states them.
func lineOrder(x *commitment.Change) []string {
	r, err := commitment.DecodeChangeRequest(x.Request)
	if err != nil {
		return nil
	}
	out := make([]string, len(r.Lines))
	for i, l := range r.Lines {
		out[i] = l.OrderLineID
	}
	return out
}

// prepareChange runs purchase.commitment.change.prepare.
type prepareChange struct {
	s        Service
	cmd      api.PrepareCommitmentChange
	req      commitment.ChangeRequest
	f        revisionFacts
	readable map[string]bool
	l        locked
	existing *commitment.Change
	replay   bool
}

// Prepare reads, unless the change ID is taken, the facts the proposed revision is prepared on and
// which of the evidence sources the preparer reads.
func (h *prepareChange) Prepare(ctx context.Context) error {
	h.f, h.readable = revisionFacts{}, nil
	var l locked
	taken := false
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		x, err := postgres.LoadChange(ctx, exec, scope(ctx), h.cmd.ChangeID, postgres.NoLock)
		if err != nil || x != nil {
			taken = x != nil
			return err
		}
		if l.c, err = postgres.LoadCommitment(ctx, exec, scope(ctx), h.cmd.CommitmentID, postgres.NoLock); err != nil || l.c == nil {
			return err
		}
		l.dr, err = postgres.LoadDraft(ctx, exec, scope(ctx), l.c.DraftID, postgres.NoLock)
		return err
	})
	if err != nil || taken {
		return err
	}
	if h.readable, err = h.s.readable(ctx, h.cmd.Evidence); err != nil {
		return err
	}
	h.f, err = h.s.proposalFacts(ctx, h.req, l.dr, h.s.Clock.Now())
	return err
}

func (h *prepareChange) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.replay, h.existing = false, nil
	if h.l, err = lockCommitment(ctx, exec, h.cmd.CommitmentID, true); err != nil {
		return err
	}
	h.existing, err = postgres.LoadChange(ctx, exec, scope(ctx), h.cmd.ChangeID, postgres.ForUpdate)
	return err
}

// Apply refuses, in this order: a change ID taken by another request or preparer (VERSION_CONFLICT;
// the same request by the same preparer is answered from the stored change), a commitment the
// preparer does not see (NOT_FOUND), a draft not intact (REFERENCE_INVALID), Proposable's refusals
// (INVALID_STATE, PURCHASE_COMMITMENT_CHANGE_STALE, REFERENCE_INVALID, INVALID_KNOWN_VALUE), evidence
// the preparer does not read (REFERENCE_INVALID), a line below its verified deliveries
// (SUPPLIER_DELIVERY_EXCEEDED), revise's refusals and the upstream guards'.
func (h *prepareChange) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	if x := h.existing; x != nil {
		c, err := postgres.LoadCommitment(ctx, exec, sc, x.CommitmentID, postgres.NoLock)
		if err != nil {
			return action.Outcome{}, err
		}
		if c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/change_id", "")
		}
		if err := x.Answer(h.req, d.Actor); err != nil {
			return action.Outcome{}, err
		}
		h.replay = true
		return changeOutcome(x), nil
	}
	if _, err := commitmentDraft(ctx, d, h.l); err != nil {
		return action.Outcome{}, err
	}
	c, dr := h.l.c, h.l.dr
	current, err := c.Document(dr)
	if err != nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/commitment_id", "The commitment's revision is not intact")
	}
	lines, err := h.req.Proposable(c, dr, current)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = cited(h.cmd.Evidence, h.readable); err != nil {
		return action.Outcome{}, err
	}
	verified, err := postgres.VerifiedOfCommitment(ctx, exec, sc, c.ID, "")
	if err != nil {
		return action.Outcome{}, err
	}
	order := make([]string, len(h.req.Lines))
	for i, l := range h.req.Lines {
		order[i] = l.OrderLineID
	}
	if err = keeps(lines, verified, order, "/args/"); err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	p, err := h.s.revise(ctx, h.f, c, dr, h.l.roots, d.Actor, d.Epoch, now, "/args/")
	if err != nil {
		return action.Outcome{}, err
	}
	for i := range lines {
		if j := slices.IndexFunc(p.Doc.Lines, func(x draft.DocLine) bool { return x.OrderLineID == lines[i].OrderLineID }); j >= 0 {
			lines[i].SupplierDate = p.Doc.Lines[j].Delivery.SupplierDate
		}
	}
	adopt, err := h.s.guardRevision(ctx, exec, p, "/args/", now)
	if err != nil {
		return action.Outcome{}, err
	}
	doc, hash, err := p.SealRevision(draft.DocRevision{CommitmentID: c.ID, ChangeID: h.req.ChangeID, BaseRevision: c.Revision,
		Revision: c.Revision + 1}, adopt.Status, adopt.Code, adopt.Input)
	if err != nil {
		return action.Outcome{}, err
	}
	x, err := commitment.NewChange(h.req, c, lines, doc, hash, p.Doc.Order.OrderRevision, p.Doc.Order.OrderHash, p.SupplyValidUntil, d.Actor,
		d.Epoch, now)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertChange(ctx, exec, sc, x); err != nil {
		return action.Outcome{}, err
	}
	return changeOutcome(x), nil
}

// PrepareCommitmentChange prepares the next revision of an issued commitment
// (purchase.commitment.change.prepare; PUR-INV-011). Human or AI; it publishes no event.
func (s Service) PrepareCommitmentChange(ctx context.Context, cmd api.PrepareCommitmentChange) (api.ChangeReceipt, error) {
	validation := cmd.Validate()
	h := &prepareChange{s: s, cmd: cmd}
	if validation == nil {
		h.req = changeRequestOf(cmd)
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandPrepareCommitmentChange, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ChangeID,
		Reason: cmd.Reason}, cmd, validation, h)
	if err != nil {
		return api.ChangeReceipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return s.changeReceipt(ctx, res)
}

// decideChange runs purchase.commitment.change.decide.
type decideChange struct {
	s   Service
	cmd api.DecideCommitmentChange
	f   revisionFacts
	h   heldChange
}

func (h *decideChange) Prepare(ctx context.Context) error {
	h.f = revisionFacts{}
	if h.cmd.Decision != api.DecisionApprove {
		return nil
	}
	var err error
	h.f, err = h.s.changeFacts(ctx, h.cmd.ChangeID)
	return err
}

func (h *decideChange) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.h, err = lockChange(ctx, exec, h.cmd.ChangeID)
	return err
}

// Apply refuses, in this order: a change the decider does not see (NOT_FOUND), a draft not intact
// (REFERENCE_INVALID), Decidable's refusals (VERSION_CONFLICT, INVALID_STATE,
// INDEPENDENT_REVIEW_REQUIRED, REFERENCE_STALE, PURCHASE_COMMITMENT_CHANGE_STALE); then a rejection is
// recorded; an approval goes on with revise's refusals, a proposed revision that is no longer
// equal (PURCHASE_DRAFT_STALE), Approve's (the acknowledgements), a line below its verified
// deliveries (SUPPLIER_DELIVERY_EXCEEDED) and the upstream guards'.
func (h *decideChange) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	if err := changeDraft(ctx, d, h.h); err != nil {
		return action.Outcome{}, err
	}
	c, dr, x := h.h.l.c, h.h.l.dr, h.h.x
	if err := x.Decidable(h.cmd.ExpectedVersion, c, d.Actor, h.cmd.ChangeHash); err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	act := commitment.Act{By: d.Actor, Epoch: d.Epoch, At: now, Reason: h.cmd.Reason}
	if h.cmd.Decision == api.DecisionReject {
		x.Reject(act)
		if err := postgres.SaveChange(ctx, exec, sc, x, now); err != nil {
			return action.Outcome{}, err
		}
		return changeOutcome(x), nil
	}
	p, err := h.s.revise(ctx, h.f, c, dr, h.h.l.roots, x.Prepared.By, x.Prepared.Epoch, now, "/change/")
	if err != nil {
		return action.Outcome{}, err
	}
	if err = draft.RecomputeRevision(x.Document, p, "/args/change_id"); err != nil {
		return action.Outcome{}, err
	}
	doc, err := x.Decode()
	if err != nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/change_id", "The stored change is not intact")
	}
	acks := make([]commitment.Acknowledgement, len(h.cmd.Acknowledgements))
	for i, a := range h.cmd.Acknowledgements {
		acks[i] = commitment.Acknowledgement{OrderLineID: a.OrderLineID, Kind: a.Kind, Reason: a.Reason}
	}
	if err = x.Approve(commitment.ChangeDeviations(doc), acks, act); err != nil {
		return action.Outcome{}, err
	}
	verified, err := postgres.VerifiedOfCommitment(ctx, exec, sc, c.ID, "")
	if err != nil {
		return action.Outcome{}, err
	}
	if err = keeps(x.Lines, verified, lineOrder(x), "/change/"); err != nil {
		return action.Outcome{}, err
	}
	if _, err = h.s.guardRevision(ctx, exec, p, "/change/", now); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveChange(ctx, exec, sc, x, now); err != nil {
		return action.Outcome{}, err
	}
	return changeOutcome(x), nil
}

// DecideCommitmentChange approves or rejects a change (purchase.commitment.change.decide;
// PUR-INV-011). Human only, independent of the change's preparer and the commitment's requester.
func (s Service) DecideCommitmentChange(ctx context.Context, cmd api.DecideCommitmentChange) (api.ChangeReceipt, error) {
	h := &decideChange{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandDecideCommitmentChange, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ChangeID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.ChangeReceipt{}, err
	}
	return s.changeReceipt(ctx, res)
}

// applyChange runs purchase.commitment.change.apply.
type applyChange struct {
	s   Service
	cmd api.ApplyCommitmentChange
	f   revisionFacts
	h   heldChange
}

func (h *applyChange) Prepare(ctx context.Context) error {
	var err error
	h.f, err = h.s.changeFacts(ctx, h.cmd.ChangeID)
	return err
}

func (h *applyChange) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.h, err = lockChange(ctx, exec, h.cmd.ChangeID)
	return err
}

// Apply refuses, in this order: a change the applier does not see (NOT_FOUND), a draft not intact
// (REFERENCE_INVALID), Appliable's refusals (VERSION_CONFLICT, INVALID_STATE,
// PURCHASE_COMMITMENT_CHANGE_STALE), a line below its verified deliveries
// (SUPPLIER_DELIVERY_EXCEEDED), revise's refusals on the allocation roots as they are now (an increase
// another draft has taken the room for since is PURCHASE_ALLOCATION_EXCEEDED), a proposed revision
// that is no longer equal (PURCHASE_DRAFT_STALE) and the upstream guards'. Then the commitment moves
// to the change's revision, issued again; the allocation entries of the revision it replaces are
// released and those of the new one taken.
func (h *applyChange) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	if err := changeDraft(ctx, d, h.h); err != nil {
		return action.Outcome{}, err
	}
	c, dr, x := h.h.l.c, h.h.l.dr, h.h.x
	if err := x.Appliable(h.cmd.ExpectedVersion, c); err != nil {
		return action.Outcome{}, err
	}
	verified, err := postgres.VerifiedOfCommitment(ctx, exec, sc, c.ID, "")
	if err != nil {
		return action.Outcome{}, err
	}
	if err = keeps(x.Lines, verified, lineOrder(x), "/change/"); err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	p, err := h.s.revise(ctx, h.f, c, dr, h.h.l.roots, x.Prepared.By, x.Prepared.Epoch, now, "/change/")
	if err != nil {
		return action.Outcome{}, err
	}
	if err = draft.RecomputeRevision(x.Document, p, "/args/change_id"); err != nil {
		return action.Outcome{}, err
	}
	if _, err = h.s.guardRevision(ctx, exec, p, "/change/", now); err != nil {
		return action.Outcome{}, err
	}
	// The preparation judged the allocation without the draft's entries; the stored roots move to the
	// revision's quantities, each under one version.
	judged := map[allocation.Key]*allocation.Allocation{}
	for _, a := range p.Allocations {
		judged[a.Key] = a
	}
	var touched []*allocation.Allocation
	for _, l := range x.Lines {
		key := allocation.Key{OrderID: dr.OrderID, LineID: l.OrderLineID}
		a := h.h.l.roots[key]
		if a == nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/change_id", "The commitment's allocation is missing")
		}
		revision, demand := a.OrderRevision, a.Demand
		if j := judged[key]; j != nil {
			revision, demand = j.OrderRevision, j.Demand
		}
		if err = a.Move(dr.ID, c.Revision+1, l.Allocated, revision, a.Unit, demand, now, "/change/lines"); err != nil {
			return action.Outcome{}, err
		}
		touched = append(touched, a)
	}
	allocation.Sort(touched)
	r := c.Apply(x, commitment.Act{By: d.Actor, Epoch: d.Epoch, At: now})
	if err = postgres.SaveCommitment(ctx, exec, sc, c, now); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveChange(ctx, exec, sc, x, now); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveAllocations(ctx, exec, sc, touched, now); err != nil {
		return action.Outcome{}, err
	}
	lines := make([]map[string]any, 0, len(r.Lines))
	for _, l := range commitment.ChangeLines(r.Lines) {
		lines = append(lines, map[string]any{"order_line_id": l.OrderLineID, "allocated_quantity": text(l.Allocated),
			"overbuy_quantity": text(l.Overbuy), "purchase_quantity": text(l.Purchased()), "supplier_date": l.SupplierDate})
	}
	event := commitmentEvent(c, api.EventPurchaseCommitmentChanged, map[string]any{"order_revision": r.OrderRevision, "order_hash": r.OrderHash,
		"change_id": x.ID, "change_hash": x.Hash, "revision": r.Number, "base_revision": x.BaseRevision, "po_number": poNumber(c),
		"commitment_status": c.Status, "fulfilled": commitment.Fulfilled(r.Lines, verified), "lines": lines, "applied_by": d.Actor.ID})
	return changeOutcome(x, event), nil
}

// ApplyCommitmentChange makes an approved change the commitment's next revision
// (purchase.commitment.change.apply; PUR-INV-011). Human only.
func (s Service) ApplyCommitmentChange(ctx context.Context, cmd api.ApplyCommitmentChange) (api.ChangeReceipt, error) {
	h := &applyChange{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandApplyCommitmentChange, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ChangeID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.ChangeReceipt{}, err
	}
	return s.changeReceipt(ctx, res)
}

// cancelCommand runs purchase.commitment.cancel.
type cancelCommand struct {
	s        Service
	cmd      api.CancelCommitment
	readable map[string]bool
	l        locked
}

func (h *cancelCommand) Prepare(ctx context.Context) error {
	var err error
	h.readable, err = h.s.readable(ctx, h.cmd.Evidence)
	return err
}

func (h *cancelCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.l, err = lockCommitment(ctx, exec, h.cmd.CommitmentID, true)
	return err
}

// Apply refuses, in this order: a commitment the canceller does not see (NOT_FOUND), a draft not
// intact (REFERENCE_INVALID), Cancel's refusals (VERSION_CONFLICT, INVALID_STATE) and evidence the
// canceller does not read (REFERENCE_INVALID). Then each line keeps what was verified delivered of it
// and the allocation of the rest is released.
func (h *cancelCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	if _, err := commitmentDraft(ctx, d, h.l); err != nil {
		return action.Outcome{}, err
	}
	c, dr := h.l.c, h.l.dr
	current, err := c.Lines(dr)
	if err != nil {
		return action.Outcome{}, err
	}
	verified, err := postgres.VerifiedOfCommitment(ctx, exec, sc, c.ID, "")
	if err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	r, err := c.Cancel(h.cmd.ExpectedVersion, current, verified, commitment.Act{By: d.Actor, Epoch: d.Epoch, At: now, Reason: h.cmd.Reason},
		evidenceOf(h.cmd.Evidence))
	if err != nil {
		return action.Outcome{}, err
	}
	if err = cited(h.cmd.Evidence, h.readable); err != nil {
		return action.Outcome{}, err
	}
	var touched []*allocation.Allocation
	lines := make([]map[string]any, 0, len(r.Lines))
	for _, kept := range r.Lines {
		a := h.l.roots[allocation.Key{OrderID: dr.OrderID, LineID: kept.OrderLineID}]
		if a == nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/commitment_id", "The commitment's allocation is missing")
		}
		if err := a.Move(dr.ID, r.Number, kept.Allocated, a.OrderRevision, a.Unit, a.Demand, now, "/args/commitment_id"); err != nil {
			return action.Outcome{}, err
		}
		touched = append(touched, a)
		was := kernel.DecimalFromInt(0)
		if i := slices.IndexFunc(current, func(q commitment.LineQuantity) bool { return q.OrderLineID == kept.OrderLineID }); i >= 0 {
			was = current[i].Purchased()
		}
		lines = append(lines, map[string]any{"order_line_id": kept.OrderLineID, "verified_quantity": text(kept.Purchased()),
			"released_quantity": text(was.Sub(kept.Purchased()))})
	}
	allocation.Sort(touched)
	if err = postgres.SaveCommitment(ctx, exec, sc, c, now); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveAllocations(ctx, exec, sc, touched, now); err != nil {
		return action.Outcome{}, err
	}
	slices.SortFunc(lines, func(a, b map[string]any) int {
		return strings.Compare(a["order_line_id"].(string), b["order_line_id"].(string))
	})
	orderRevision, orderHash := c.OrderBinding()
	return h.s.commitmentOutcome(c, commitmentEvent(c, api.EventPurchaseCommitmentCancelled, map[string]any{"order_revision": orderRevision,
		"order_hash": orderHash, "po_number": poNumber(c), "revision": r.Number, "cancelled_by": d.Actor.ID, "lines": lines})), nil
}

// CancelCommitment cancels an issued or confirmed commitment on the supplier's evidence
// (purchase.commitment.cancel; PUR-INV-011). Human only.
func (s Service) CancelCommitment(ctx context.Context, cmd api.CancelCommitment) (api.CommitmentReceipt, error) {
	h := &cancelCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandCancelCommitment, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.CommitmentID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.CommitmentReceipt{}, err
	}
	return s.commitmentReceipt(ctx, res)
}

// changeReceipt completes res with what the stored change binds.
func (s Service) changeReceipt(ctx context.Context, res api.Receipt) (api.ChangeReceipt, error) {
	out := api.ChangeReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		x, err := postgres.LoadChange(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || x == nil {
			return err
		}
		out.CommitmentID, out.BaseRevision, out.AppliedRevision, out.ChangeHash = x.CommitmentID, x.BaseRevision, x.AppliedRevision, x.Hash
		return nil
	})
	return out, err
}
