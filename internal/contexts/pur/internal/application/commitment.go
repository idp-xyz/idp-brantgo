package application

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Purchase commitments (WBS S5-01; domain/commitment; api/commitment.go).
//
// Lock order (ADR-BR-011, tactical lockOrder "PUR 为采购承诺根与订单行需求分配"): the commitment root FOR
// UPDATE (Lock); for a decision and an issue then the DemandAllocation roots of the draft's lines FOR
// UPDATE in (order_id, order_line_id) order, after which the draft is read again, so a void that held
// them has committed and is seen; then the company's authority guard FOR SHARE (the ledger's
// Authorize, and Qualify of the decider at an issue); then the upstream guards in context code order
// (CAT SkuGuard, CST AdoptabilityGuard, POL ConfigGuard — the purchase order number sequence
// among them at an issue —, PTY PartyGuard, SUP SupplyConditionGuard, TRD OrderGuard), each FOR
// SHARE until PUR commits (ADR-BR-004 option B); then the commitment's row, the ledger's result,
// audit and Outbox. A void locks the draft and then the same allocation roots, so it serializes with
// a decision and an issue of the draft's commitment; a revocation and a supplier confirmation lock
// the commitment root only. A request creates the root and holds the order row FOR SHARE through
// OrderGuard, as DownstreamCommitments v1 (R71) needs of every command that adds to it.
//
// The decision and the issue re-check the draft (A47 #3): before the transaction they read the
// facts a preparation of the draft's request is judged on (facts: TRD, SUP, PTY, CAT, POL, CST, as
// the acting human sees them); inside it they prepare the request again on those facts against the
// locked allocation roots without the draft's own entries (recompute), require the result equal to
// the sealed draft field by field (PURCHASE_DRAFT_STALE otherwise), and run the owners' guards on
// every reference it adopts. Refusals of that preparation point into the draft (/draft/...).

// Numbers is POL's NumberIssue v1 (Context Map R48): number.issue, which POL runs as the acting
// actor in a transaction of its own before PUR's.
type Numbers interface {
	IssueNumber(ctx context.Context, cmd polapi.IssueNumber) (polapi.Receipt, error)
}

// Sources is EVD's SourceRef v1 and FileRef v1 (Context Map R76): the sources the acting actor
// reads, and a business file.
type Sources interface {
	ResolveSources(ctx context.Context, ids []string) (map[string]evdapi.SourceRef, error)
	GetFile(ctx context.Context, fileID string) (evdapi.FileView, error)
}

// locked is what a commitment command holds: the commitment root, its draft and, for a decision or
// an issue, the DemandAllocation roots of the draft's lines.
type locked struct {
	c     *commitment.Commitment
	dr    *draft.Draft
	roots map[allocation.Key]*allocation.Allocation
}

// lockCommitment locks commitment id FOR UPDATE and reads its draft; with roots, it then locks the
// DemandAllocation roots of the draft's lines FOR UPDATE and reads the draft again.
func lockCommitment(ctx context.Context, exec bentopg.Executor, id string, roots bool) (locked, error) {
	sc := scope(ctx)
	c, err := postgres.LoadCommitment(ctx, exec, sc, id, postgres.ForUpdate)
	if err != nil || c == nil {
		return locked{}, err
	}
	out := locked{c: c}
	if out.dr, err = postgres.LoadDraft(ctx, exec, sc, c.DraftID, postgres.NoLock); err != nil || out.dr == nil || !roots {
		return out, err
	}
	var keys []allocation.Key
	for _, l := range out.dr.Lines {
		if l.OrderLineID != "" {
			keys = append(keys, allocation.Key{OrderID: out.dr.OrderID, LineID: l.OrderLineID})
		}
	}
	slices.SortFunc(keys, allocation.Compare)
	if out.roots, err = postgres.LoadAllocations(ctx, exec, sc, keys, postgres.ForUpdate); err != nil {
		return out, err
	}
	out.dr, err = postgres.LoadDraft(ctx, exec, sc, c.DraftID, postgres.NoLock)
	return out, err
}

// loadCommitment reads commitment id and its draft without locks, for the steps before the
// transaction.
func (s Service) loadCommitment(ctx context.Context, id string) (locked, error) {
	var l locked
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		if l.c, err = postgres.LoadCommitment(ctx, exec, scope(ctx), id, postgres.NoLock); err != nil || l.c == nil {
			return err
		}
		l.dr, err = postgres.LoadDraft(ctx, exec, scope(ctx), l.c.DraftID, postgres.NoLock)
		return err
	})
	return l, err
}

// commitmentDraft refuses a commitment d's actor does not see (NOT_FOUND) and a draft that is not
// intact (REFERENCE_INVALID), and returns what the draft says to the commitment.
func commitmentDraft(ctx context.Context, d authorization.Decision, l locked) (commitment.Draft, error) {
	if l.c == nil || !d.Sees(l.c.OwnerActor, l.c.OwnerTeam) {
		return commitment.Draft{}, kernel.Reject(kernel.CodeNotFound, "/args/commitment_id", "")
	}
	if l.dr == nil {
		return commitment.Draft{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/commitment_id", "The commitment's draft is missing")
	}
	if err := l.dr.Verify(scope(ctx), "/args/commitment_id"); err != nil {
		return commitment.Draft{}, err
	}
	return commitment.From(l.dr)
}

// recheck is what a decision or an issue read before its transaction to prepare the draft's
// request again.
type recheck struct {
	read  bool
	in    draft.Inputs
	order *Order
}

func (s Service) recheckFacts(ctx context.Context, dr *draft.Draft, at time.Time) (recheck, error) {
	if dr == nil {
		return recheck{}, nil
	}
	req, err := dr.DecodeRequest()
	if err != nil {
		return recheck{}, nil //nolint:nilerr // a request that does not decode is judged by Verify in the transaction: REFERENCE_INVALID
	}
	in, order, err := s.facts(ctx, req, at)
	return recheck{read: true, in: in, order: order}, err
}

func rejectAs(code, path, reason string) error {
	c, ok := api.Category(code)
	if !ok {
		return kernel.Reject(code, path, reason)
	}
	return kernel.RejectAs(code, c, path, reason)
}

// recompute prepares dr's request again on f at instant now against roots without dr's own
// entries, and requires the result equal to dr field by field (A47 #3, PUR-INV-009), in this order:
// an order the actor does not read (REFERENCE_INVALID), TRD's verdict on it (ORDER_NOT_ACTIVE,
// ORDER_APPROVAL_REVOKED, ORDER_APPROVAL_REQUIRED), an order moved to another revision or hash
// (PURCHASE_DRAFT_STALE), the preparation's own refusals (SUP's SUPPLY_CONDITION_NOT_CURRENT and
// SUPPLY_CONDITION_EXPIRED, CAT's, POL's and PTY's verdicts, the delivery, the allocation ...) at the
// draft's paths (/draft/...), and any difference of the result (PURCHASE_DRAFT_STALE).
func (s Service) recompute(ctx context.Context, dr *draft.Draft, f recheck, roots map[allocation.Key]*allocation.Allocation, now time.Time) (*draft.Prepared, error) {
	if !f.read {
		return nil, kernel.Reject(kernel.CodeVersionConflict, "/args/commitment_id", "The commitment changed while the command was prepared")
	}
	o := f.in.Order
	if f.order == nil || o == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/draft/order_id", "The actor does not read the draft's order")
	}
	if err := o.Check("/draft/order_id"); err != nil {
		return nil, err
	}
	if o.OrderRevision != dr.OrderRevision || o.OrderHash != dr.OrderHash {
		return nil, rejectAs(api.CodePurchaseDraftStale, "/draft/order_id", "The order moved to revision "+itoa(o.OrderRevision)+
			"; prepare a new draft")
	}
	req, err := dr.DecodeRequest()
	if err != nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/commitment_id", "The stored draft is not intact")
	}
	alloc := make(map[allocation.Key]*allocation.Allocation, len(roots))
	for k, a := range roots {
		alloc[k] = a.Without(dr.ID)
	}
	in := f.in
	in.Scope, in.Now, in.Preparer, in.Epoch, in.Allocations = scope(ctx), kernel.Instant(now), dr.Prepared.By, dr.Prepared.Epoch, alloc
	p, err := draft.Prepare(req, in)
	if err != nil {
		return nil, moved(err, "/args/", "/draft/")
	}
	if err = dr.Recompute(p, "/args/commitment_id"); err != nil {
		return nil, err
	}
	relocate(p)
	return p, nil
}

// relocate points the references p adopted at the draft (/draft/...) rather than at a request.
func relocate(p *draft.Prepared) {
	to := func(path string) string {
		if strings.HasPrefix(path, "/args/") {
			return "/draft/" + strings.TrimPrefix(path, "/args/")
		}
		return path
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

// guardDraft runs the upstream guards on what p adopted and on dr's order (guard): a refusal at the
// order is at /draft/order_id, and an order at another revision or hash PURCHASE_DRAFT_STALE.
func (s Service) guardDraft(ctx context.Context, exec bentopg.Executor, p *draft.Prepared, dr *draft.Draft, at time.Time) error {
	_, err := s.guard(ctx, exec, p, OrderBinding{OrderID: dr.OrderID, OrderRevision: dr.OrderRevision, OrderHash: dr.OrderHash}, at)
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 && rej.Fields[0].Path == "/args/order_id" {
		if rej.Code == api.CodeReferenceStale {
			return rejectAs(api.CodePurchaseDraftStale, "/draft/order_id", "The order moved since the draft was prepared")
		}
		return moved(err, "/args/order_id", "/draft/order_id")
	}
	return err
}

// moved returns err with its path's prefix from replaced by to.
func moved(err error, from, to string) error {
	rej, ok := kernel.AsRejection(err)
	if !ok || len(rej.Fields) == 0 {
		return err
	}
	out := *rej
	out.Fields = slices.Clone(rej.Fields)
	if p := out.Fields[0].Path; strings.HasPrefix(p, from) {
		out.Fields[0].Path = to + strings.TrimPrefix(p, from)
	}
	return &out
}

func evidenceOf(es []api.Evidence) []commitment.Evidence {
	out := make([]commitment.Evidence, len(es))
	for i, e := range es {
		out[i] = commitment.Evidence{SourceID: e.SourceID, Locator: e.Locator}
	}
	return out
}

// readable asks EVD which of es's sources the actor reads, before the transaction.
func (s Service) readable(ctx context.Context, es []api.Evidence) (map[string]bool, error) {
	out := map[string]bool{}
	var ids []string
	for _, e := range es {
		if !slices.Contains(ids, e.SourceID) {
			ids = append(ids, e.SourceID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	refs, err := s.Upstream.Sources.ResolveSources(ctx, ids)
	for id := range refs {
		out[id] = true
	}
	return out, err
}

// cited refuses evidence the actor does not read (REFERENCE_INVALID at it).
func cited(es []api.Evidence, readable map[string]bool) error {
	for i, e := range es {
		if !readable[e.SourceID] {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/evidence/"+itoa(int64(i))+"/source_id", "No source the actor reads")
		}
	}
	return nil
}

// qualified reports whether the decider of c's approval is still a human holding
// purchase.commitment.decide and pur.read who sees the commitment (A47 #2: 生效前重查裁决人的当前资格),
// read FOR SHARE through the authority guard.
func (s Service) qualified(ctx context.Context, exec bentopg.Executor, c *commitment.Commitment) (bool, error) {
	if c.Decision == nil || !c.Decision.Approve {
		return true, nil // Issuable refuses on the state
	}
	h, err := s.Ledger.Authorizer.Qualify(ctx, exec, c.Decision.By.ID)
	if err != nil {
		return false, err
	}
	return h.Actor.Kind == kernel.ActorHuman && h.Holds(commitment.PermissionDecide) && h.Holds(commitment.PermissionRead) &&
		h.Sees(c.OwnerActor, c.OwnerTeam), nil
}

func (s Service) commitmentOutcome(c *commitment.Commitment, events ...action.Event) action.Outcome {
	return action.Outcome{ObjectID: c.ID, RowVersion: c.RowVersion, Status: c.Status, Events: events}
}

func commitmentEvent(c *commitment.Commitment, eventType string, fields map[string]any) action.Event {
	base := map[string]any{"commitment_id": c.ID, "draft_id": c.DraftID, "draft_hash": c.DraftHash, "order_id": c.OrderID,
		"order_revision": c.OrderRevision, "supplier_id": c.SupplierID}
	for k, v := range fields {
		base[k] = v
	}
	return action.Event{Type: eventType, AggregateType: "PurchaseCommitment", AggregateID: c.ID, AggregateVersion: c.RowVersion, Fields: base}
}

// requestCommand runs purchase.commitment.request: it locks the commitment ID (a taken one is
// answered), reads the draft, decides, holds the draft's order FOR SHARE through OrderGuard v1 and
// inserts the commitment.
type requestCommand struct {
	s        Service
	cmd      api.RequestCommitment
	existing *commitment.Commitment
	replay   bool
}

func (h *requestCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.replay = false
	h.existing, err = postgres.LoadCommitment(ctx, exec, scope(ctx), h.cmd.CommitmentID, postgres.ForUpdate)
	return err
}

// Apply refuses, in this order: a commitment ID taken by another request (VERSION_CONFLICT), a
// draft the requester does not see (NOT_FOUND), one not intact (REFERENCE_INVALID), Request's
// refusals, and the order's (OrderGuard v1: ORDER_NOT_ACTIVE, ORDER_APPROVAL_REVOKED; another
// revision or hash PURCHASE_DRAFT_STALE).
func (h *requestCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	if c := h.existing; c != nil {
		if !d.Sees(c.OwnerActor, c.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/commitment_id", "")
		}
		if err := c.Answer(h.cmd.DraftID, h.cmd.DraftHash, d.Actor); err != nil {
			return action.Outcome{}, err
		}
		h.replay = true
		return h.s.commitmentOutcome(c), nil
	}
	dr, err := postgres.LoadDraft(ctx, exec, sc, h.cmd.DraftID, postgres.NoLock)
	if err != nil {
		return action.Outcome{}, err
	}
	if dr == nil || !d.Sees(dr.OwnerActor, dr.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/draft_id", "")
	}
	if err = dr.Verify(sc, "/args/draft_id"); err != nil {
		return action.Outcome{}, err
	}
	cd, err := commitment.From(dr)
	if err != nil {
		return action.Outcome{}, err
	}
	owners, err := postgres.CommitmentsOfDraft(ctx, exec, sc, dr.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	live := make([]commitment.Live, len(owners))
	for i, o := range owners {
		live[i] = commitment.Live{ID: o.ID, Status: o.Status}
	}
	c, err := commitment.Request(h.cmd.CommitmentID, cd, h.cmd.DraftHash, live, d.Actor, d.Epoch, h.s.Clock.Now(), d.Team)
	if err != nil {
		return action.Outcome{}, err
	}
	v, err := h.s.Upstream.Orders.GuardOrder(ctx, exec, OrderBinding{OrderID: dr.OrderID, OrderRevision: dr.OrderRevision, OrderHash: dr.OrderHash})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = orderCheck(v, "/args/draft_id"); err != nil {
		if rej, _ := kernel.AsRejection(err); rej != nil && rej.Code == api.CodeReferenceStale {
			return action.Outcome{}, rejectAs(api.CodePurchaseDraftStale, "/args/draft_id", "The draft's order moved; prepare a new draft")
		}
		return action.Outcome{}, err
	}
	if err = postgres.InsertCommitment(ctx, exec, sc, c); err != nil {
		return action.Outcome{}, err
	}
	return h.s.commitmentOutcome(c), nil
}

// RequestCommitment requests the commitment of a prepared draft (purchase.commitment.request; A47
// #1). Human or AI; it publishes no event (no consumer).
func (s Service) RequestCommitment(ctx context.Context, cmd api.RequestCommitment) (api.CommitmentReceipt, error) {
	h := &requestCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandRequestCommitment, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.CommitmentID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.CommitmentReceipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return s.commitmentReceipt(ctx, res)
}

// decideCommand runs purchase.commitment.decide.
type decideCommand struct {
	s   Service
	cmd api.DecideCommitment
	f   recheck
	l   locked
}

// Prepare reads, for an approval, the facts the draft's request is prepared again on, as the decider
// sees them now.
func (h *decideCommand) Prepare(ctx context.Context) error {
	h.f = recheck{}
	if h.cmd.Decision != api.DecisionApprove {
		return nil
	}
	l, err := h.s.loadCommitment(ctx, h.cmd.CommitmentID)
	if err != nil {
		return err
	}
	h.f, err = h.s.recheckFacts(ctx, l.dr, h.s.Clock.Now())
	return err
}

func (h *decideCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.l, err = lockCommitment(ctx, exec, h.cmd.CommitmentID, true)
	return err
}

// Apply refuses, in this order: a commitment the decider does not see (NOT_FOUND), a draft not
// intact (REFERENCE_INVALID), Decidable's refusals (VERSION_CONFLICT, INVALID_STATE,
// INDEPENDENT_REVIEW_REQUIRED, the draft voided, REFERENCE_STALE for another draft hash); then a
// rejection is recorded; an approval goes on with recompute's refusals, Approve's (the validity,
// the acknowledgements) and the upstream guards'.
func (h *decideCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	cd, err := commitmentDraft(ctx, d, h.l)
	if err != nil {
		return action.Outcome{}, err
	}
	c := h.l.c
	if err = c.Decidable(h.cmd.ExpectedVersion, cd, d.Actor, h.cmd.DraftHash); err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	act := commitment.Act{By: d.Actor, Epoch: d.Epoch, At: now, Reason: h.cmd.Reason}
	if h.cmd.Decision == api.DecisionReject {
		c.Reject(act)
		if err = postgres.SaveCommitment(ctx, exec, scope(ctx), c, now); err != nil {
			return action.Outcome{}, err
		}
		return h.s.commitmentOutcome(c), nil
	}
	p, err := h.s.recompute(ctx, h.l.dr, h.f, h.l.roots, now)
	if err != nil {
		return action.Outcome{}, err
	}
	until, _ := kernel.ParseTimestamp(h.cmd.ValidUntil)
	acks := make([]commitment.Acknowledgement, len(h.cmd.Acknowledgements))
	for i, a := range h.cmd.Acknowledgements {
		acks[i] = commitment.Acknowledgement{OrderLineID: a.OrderLineID, Kind: a.Kind, Reason: a.Reason}
	}
	if err = c.Approve(cd, until, acks, act); err != nil {
		return action.Outcome{}, err
	}
	if err = h.s.guardDraft(ctx, exec, p, h.l.dr, now); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveCommitment(ctx, exec, scope(ctx), c, now); err != nil {
		return action.Outcome{}, err
	}
	return h.s.commitmentOutcome(c, commitmentEvent(c, api.EventPurchaseCommitmentApproved, map[string]any{"order_hash": c.OrderHash,
		"valid_until": kernel.FormatTimestamp(c.Decision.ValidUntil), "decided_by": c.Decision.By.ID})), nil
}

// DecideCommitment approves or rejects a requested commitment (purchase.commitment.decide; A47 #2 to
// #5, PUR-INV-009). Human only, independent of the draft's preparer and the requester.
func (s Service) DecideCommitment(ctx context.Context, cmd api.DecideCommitment) (api.CommitmentReceipt, error) {
	h := &decideCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandDecideCommitment, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.CommitmentID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.CommitmentReceipt{}, err
	}
	return s.commitmentReceipt(ctx, res)
}

// revokeCommand runs purchase.commitment.revoke on the commitment root alone.
type revokeCommand struct {
	s   Service
	cmd api.RevokeCommitment
	c   *commitment.Commitment
}

func (h *revokeCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.c, err = postgres.LoadCommitment(ctx, exec, scope(ctx), h.cmd.CommitmentID, postgres.ForUpdate)
	return err
}

func (h *revokeCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	c := h.c
	if c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/commitment_id", "")
	}
	now := kernel.Instant(h.s.Clock.Now())
	if err := c.Revoke(h.cmd.ExpectedVersion, commitment.Act{By: d.Actor, Epoch: d.Epoch, At: now, Reason: h.cmd.Reason}); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.SaveCommitment(ctx, exec, scope(ctx), c, now); err != nil {
		return action.Outcome{}, err
	}
	return h.s.commitmentOutcome(c, commitmentEvent(c, api.EventPurchaseCommitmentRevoked, map[string]any{"revoked_by": d.Actor.ID})), nil
}

// RevokeCommitment revokes an approval before it is issued (purchase.commitment.revoke; A47 #6).
// Human only.
func (s Service) RevokeCommitment(ctx context.Context, cmd api.RevokeCommitment) (api.CommitmentReceipt, error) {
	h := &revokeCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandRevokeCommitment, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.CommitmentID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.CommitmentReceipt{}, err
	}
	return s.commitmentReceipt(ctx, res)
}

// issueCommand runs purchase.commitment.issue.
type issueCommand struct {
	s        Service
	cmd      api.IssueCommitment
	f        recheck
	readable map[string]bool
	file     *evdapi.FileView
	seqHash  string
	number   string
	l        locked
}

func (h *issueCommand) sequence() polapi.ConfigRef {
	return polapi.ConfigRef{Kind: polapi.KindNumberSequence, ConfigID: h.cmd.SequenceRef.ID, Revision: h.cmd.SequenceRef.Revision}
}

// admit runs the checks the issue makes before anything upstream: visibility, the draft's
// integrity, the decider's qualification (read FOR SHARE through the authority guard) and
// Issuable.
func (h *issueCommand) admit(ctx context.Context, exec bentopg.Executor, d authorization.Decision, l locked, now time.Time) error {
	cd, err := commitmentDraft(ctx, d, l)
	if err != nil {
		return err
	}
	ok, err := h.s.qualified(ctx, exec, l.c)
	if err != nil {
		return err
	}
	return l.c.Issuable(h.cmd.ExpectedVersion, cd, now, ok)
}

// Prepare runs, before the transaction, the checks the transaction will run, so a refusal costs no
// number; reads the evidence (EVD), the file sent, the number sequence (POL: a purchase_order
// sequence in force) and the draft's facts; and has POL issue the purchase order number for the
// commitment (number.issue as the issuer, NumberIssue v1, R48; business reference the commitment
// ID, so a retry and a later issue get the same number back; S3-D59).
func (h *issueCommand) Prepare(ctx context.Context) error {
	h.f, h.readable, h.file, h.seqHash, h.number = recheck{}, nil, nil, "", ""
	now := h.s.Clock.Now()
	var l locked
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		var err error
		if l.c, err = postgres.LoadCommitment(ctx, exec, scope(ctx), h.cmd.CommitmentID, postgres.NoLock); err != nil {
			return err
		}
		if l.c != nil {
			if l.dr, err = postgres.LoadDraft(ctx, exec, scope(ctx), l.c.DraftID, postgres.NoLock); err != nil {
				return err
			}
		}
		return h.admit(ctx, exec, d, l, now)
	})
	if err != nil {
		return err
	}
	if h.readable, err = h.s.readable(ctx, h.cmd.Evidence); err != nil {
		return err
	}
	if h.cmd.FileID != "" {
		f, err := h.s.Upstream.Sources.GetFile(ctx, h.cmd.FileID)
		if err != nil {
			return factErr(err, "/args/file_id")
		}
		h.file = &f
	}
	ref := h.sequence()
	resolved, err := h.s.Upstream.Configs.ResolveConfigs(ctx, []polapi.ConfigRef{ref}, now)
	if err != nil {
		return err
	}
	r, ok := resolved[ref]
	if !ok {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/sequence_ref", "No such number sequence")
	}
	if err = r.Check("/args/sequence_ref"); err != nil {
		return err
	}
	if ns := r.Data.NumberSequence; ns == nil || ns.DocumentType != polapi.DocumentTypePurchaseOrder {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/sequence_ref", "Not a purchase order number sequence")
	}
	h.seqHash = r.ContentHash
	if h.f, err = h.s.recheckFacts(ctx, l.dr, now); err != nil {
		return err
	}
	issued, err := h.s.Upstream.Numbers.IssueNumber(ctx, polapi.IssueNumber{IdempotencyKey: numberKey(api.CommandIssueCommitment, h.cmd.IdempotencyKey),
		Sequence: ref, BusinessRef: h.cmd.CommitmentID})
	if err != nil {
		return moved(err, "/args/sequence", "/args/sequence_ref")
	}
	h.number = issued.ObjectID
	return nil
}

// numberKey is the idempotency key of POL's number.issue for PUR command name sent with key.
func numberKey(name, key string) string { return "pur." + draft.Hash([]byte(name + "\n" + key))[:56] }

// factErr passes on an upstream lookup's refusal at path: NOT_FOUND there is REFERENCE_INVALID here.
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

func (h *issueCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.l, err = lockCommitment(ctx, exec, h.cmd.CommitmentID, true)
	return err
}

// Apply refuses, in this order: admit's refusals (NOT_FOUND, REFERENCE_INVALID, VERSION_CONFLICT,
// INVALID_STATE, PURCHASE_COMMITMENT_NOT_APPROVED, APPROVAL_EXPIRED, OVERBUY_NOT_APPROVED), evidence
// the issuer does not read (REFERENCE_INVALID), the time it was sent (INVALID_KNOWN_VALUE), then
// recompute's, the number sequence's guard (POL ConfigGuard at the content hash read before:
// REFERENCE_STALE) and the draft's guards.
func (h *issueCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	now := kernel.Instant(h.s.Clock.Now())
	if err := h.admit(ctx, exec, d, h.l, now); err != nil {
		return action.Outcome{}, err
	}
	if err := cited(h.cmd.Evidence, h.readable); err != nil {
		return action.Outcome{}, err
	}
	if h.number == "" || (h.cmd.FileID != "" && (h.file == nil || h.file.FileID != h.cmd.FileID)) {
		return action.Outcome{}, errors.New("pur: the issue was not prepared")
	}
	c := h.l.c
	sent, _ := kernel.ParseTimestamp(h.cmd.SentAt)
	is := commitment.Issue{Act: commitment.Act{By: d.Actor, Epoch: d.Epoch, At: now}, Number: h.number,
		Sequence: commitment.Sequence{ID: h.cmd.SequenceRef.ID, Revision: h.cmd.SequenceRef.Revision}, SentAt: sent,
		Evidence: evidenceOf(h.cmd.Evidence)}
	if h.file != nil {
		is.FileID, is.FileSHA256 = h.file.FileID, h.file.ContentSHA256
	}
	if err := c.Send(is); err != nil {
		return action.Outcome{}, err
	}
	p, err := h.s.recompute(ctx, h.l.dr, h.f, h.l.roots, now)
	if err != nil {
		return action.Outcome{}, err
	}
	seq := h.sequence()
	p.Configs[draft.ConfigKey{Kind: seq.Kind, ID: seq.ConfigID, Revision: seq.Revision}] = draft.Use{Path: "/args/sequence_ref", ContentHash: h.seqHash}
	if err = h.s.guardDraft(ctx, exec, p, h.l.dr, now); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveCommitment(ctx, exec, scope(ctx), c, now); err != nil {
		return action.Outcome{}, err
	}
	lineIDs := make([]string, len(h.l.dr.Lines))
	for i, l := range h.l.dr.Lines {
		lineIDs[i] = l.OrderLineID
	}
	slices.Sort(lineIDs)
	return h.s.commitmentOutcome(c, commitmentEvent(c, api.EventPurchaseCommitmentIssued, map[string]any{"order_hash": c.OrderHash,
		"currency": c.Currency, "po_number": c.Issue.Number, "sent_at": kernel.FormatTimestamp(c.Issue.SentAt), "order_line_ids": lineIDs})), nil
}

// IssueCommitment issues an approved commitment to the supplier (purchase.commitment.issue; A47 #6,
// #7; PUR-INV-010). Human only.
func (s Service) IssueCommitment(ctx context.Context, cmd api.IssueCommitment) (api.CommitmentReceipt, error) {
	h := &issueCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandIssueCommitment, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.CommitmentID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.CommitmentReceipt{}, err
	}
	return s.commitmentReceipt(ctx, res)
}

// confirmCommand runs purchase.supplier_confirmation.record on the commitment root alone.
type confirmCommand struct {
	s        Service
	cmd      api.RecordSupplierConfirmation
	readable map[string]bool
	c        *commitment.Commitment
}

func (h *confirmCommand) Prepare(ctx context.Context) error {
	var err error
	h.readable, err = h.s.readable(ctx, h.cmd.Evidence)
	return err
}

func (h *confirmCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.c, err = postgres.LoadCommitment(ctx, exec, scope(ctx), h.cmd.CommitmentID, postgres.ForUpdate)
	return err
}

// Apply refuses, in this order: a commitment the recorder does not see (NOT_FOUND), Confirm's
// refusals (VERSION_CONFLICT, INVALID_STATE, INVALID_KNOWN_VALUE) and evidence the recorder does not
// read (REFERENCE_INVALID).
func (h *confirmCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	c := h.c
	if c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/commitment_id", "")
	}
	now := kernel.Instant(h.s.Clock.Now())
	received, _ := kernel.ParseTimestamp(h.cmd.ReceivedAt)
	conf := commitment.Confirmation{Act: commitment.Act{By: d.Actor, Epoch: d.Epoch, At: now}, Result: h.cmd.Result, ReceivedAt: received,
		Evidence: evidenceOf(h.cmd.Evidence), Deviation: h.cmd.Deviation, SupplierReference: h.cmd.SupplierReference}
	if err := c.Confirm(h.cmd.ExpectedVersion, conf); err != nil {
		return action.Outcome{}, err
	}
	if err := cited(h.cmd.Evidence, h.readable); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.SaveCommitment(ctx, exec, scope(ctx), c, now); err != nil {
		return action.Outcome{}, err
	}
	return h.s.commitmentOutcome(c, commitmentEvent(c, api.EventSupplierConfirmationRecorded, map[string]any{"po_number": c.Issue.Number,
		"result": h.cmd.Result, "received_at": kernel.FormatTimestamp(received), "commitment_status": c.Status})), nil
}

// RecordSupplierConfirmation records the supplier's answer to an issued commitment
// (purchase.supplier_confirmation.record; A47 #7). Human or AI.
func (s Service) RecordSupplierConfirmation(ctx context.Context, cmd api.RecordSupplierConfirmation) (api.CommitmentReceipt, error) {
	h := &confirmCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandRecordSupplierConfirmation, IdempotencyKey: cmd.IdempotencyKey,
		ObjectID: cmd.CommitmentID}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.CommitmentReceipt{}, err
	}
	return s.commitmentReceipt(ctx, res)
}

// commitmentReceipt completes res with what the stored commitment binds.
func (s Service) commitmentReceipt(ctx context.Context, res api.Receipt) (api.CommitmentReceipt, error) {
	out := api.CommitmentReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		c, err := postgres.LoadCommitment(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || c == nil {
			return err
		}
		out.DraftID, out.DraftHash, out.OrderID, out.OrderRevision, out.OrderHash = c.DraftID, c.DraftHash, c.OrderID, c.OrderRevision, c.OrderHash
		out.SupplierID, out.Currency = c.SupplierID, c.Currency
		if c.Decision != nil {
			out.ValidUntil = c.Decision.ValidUntil
		}
		if c.Issue != nil {
			out.PONumber = c.Issue.Number
		}
		return nil
	})
	return out, err
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
