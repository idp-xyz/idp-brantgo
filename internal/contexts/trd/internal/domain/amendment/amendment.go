// Package amendment is the order amendment of TRD (tactical model TRD-AGG-SalesOrder: OrderAmendment,
// AmendmentImpact; SM-TRD-ORDER-AMENDMENT; TRD-INV-018; UL TRD-OrderAmendment, UL-INV-27; WBS S3-15;
// event storming ES-09 and SIM-02 step 7), the only way an active sales order changes. Python has
// none of it; the design is the only source. It reads no store and keeps no state.
//
//	(none)  → draft             order.amendment.prepare, human or AI: binds the order's current
//	                            revision (Base), the change's source (the customer's response, a
//	                            revision of the customer's PO, or our proposal the customer
//	                            confirmed) and the complete proposed content (orderrev.Proposal),
//	                            with an impact assessment that may still be incomplete. The proposal
//	                            must yield a next revision (orderrev.Next: QUANTITY_BELOW_SHIPPED and
//	                            the rest) that changes something. Preparing a draft again replaces its
//	                            content, possibly on a newer order revision; every preparer is
//	                            recorded.
//	draft   → pending_approval  order.amendment.submit, human: the impact assessment covers the
//	                            seven areas (purchasing, materials, operations, quality, packing,
//	                            booking, payment plan), each unaffected, affected or open — open items
//	                            are listed, never left out (DRAFT_NOT_READY).
//	pending_approval → approved order.amendment.decide, human, independent of every preparer and the
//	          or rejected       submitter (REVIEWER_NOT_INDEPENDENT), restating the amendment hash
//	                            (ORDER_REVIEW_MISMATCH) while the order is still at the bound revision
//	                            (ORDER_AMENDMENT_STALE); a rejection gives a reason.
//	approved → applied          order.amendment.apply, human: the order is active and still at the
//	                            bound revision (ORDER_AMENDMENT_STALE: of two amendments of one
//	                            revision only the first applied wins), the decider is still
//	                            independent and qualified (ORDER_APPROVAL_REQUIRED), and the proposal
//	                            yields the next revision against the shipments confirmed now
//	                            (QUANTITY_BELOW_SHIPPED). The new order revision, its commit
//	                            evidence and OrderAmended are written in one transaction.
//	draft or pending_approval → withdrawn
//	                            order.amendment.withdraw, human, with a reason; a pending one's
//	                            approval request closes with it.
//
// The amendment's content — the order, the bound revision, the source, the normalized proposal
// and the impact assessment — is canonical JSON (brant.order-amendment/0.1.0, the quote's canonical
// form); its SHA-256 is the amendment hash, which the decision binds and the next revision names.
// A price, quantity, SKU, delivery or terms change makes the order's original approval inapplicable
// to the new revision (TRD-INV-018): the new revision stands on this amendment's approval.
package amendment

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Contract names the amendment's content document.
const Contract = "brant.order-amendment/0.1.0"

// Status is the state of an amendment (SM-TRD-ORDER-AMENDMENT).
type Status string

const (
	Draft           Status = "draft"
	PendingApproval Status = "pending_approval"
	Approved        Status = "approved"
	Applied         Status = "applied"
	Rejected        Status = "rejected"
	Withdrawn       Status = "withdrawn"
)

// Commands of SM-TRD-ORDER-AMENDMENT and the permissions a decider holds when the amendment is
// applied on their approval: the decision and TRD's read.
const (
	CommandPrepare  = "order.amendment.prepare"
	CommandSubmit   = "order.amendment.submit"
	CommandDecide   = "order.amendment.decide"
	CommandApply    = "order.amendment.apply"
	CommandWithdraw = "order.amendment.withdraw"

	PermissionDecide = "order.amendment.decide"
	PermissionRead   = quote.PermissionRead
)

// Kinds of an amendment's source (SM-TRD-ORDER-AMENDMENT prepare: 客户回复、客户 PO 修订或经客户确认的我方提议).
const (
	SourceCustomerResponse   = "customer_response"    // a CustomerResponse TRD recorded (quote.response.record; ES-09 step 1)
	SourceCustomerPORevision = "customer_po_revision" // a revision of the customer's purchase order, as EVD sources
	SourceConfirmedProposal  = "confirmed_proposal"   // our proposal and the customer's confirmation, as EVD sources
)

// Source is what the change comes from: its kind, the customer response for a response, and the
// EVD sources it rests on (required for a PO revision and a confirmed proposal).
type Source struct {
	Kind       string           `json:"kind"`
	ResponseID string           `json:"response_id,omitempty"`
	Evidence   []quote.Evidence `json:"evidence"`
}

// The areas an impact assessment covers (SM-TRD-ORDER-AMENDMENT prepare: 采购、材料、工序、质检、包装、订舱与
// 付款计划), in the order the assessment lists them.
const (
	AreaPurchasing  = "purchasing"
	AreaMaterials   = "materials"
	AreaOperations  = "operations"
	AreaQuality     = "quality"
	AreaPacking     = "packing"
	AreaBooking     = "booking"
	AreaPaymentPlan = "payment_plan"
)

var areas = []string{AreaPurchasing, AreaMaterials, AreaOperations, AreaQuality, AreaPacking, AreaBooking, AreaPaymentPlan}

// Areas returns the seven areas in their order.
func Areas() []string { return slices.Clone(areas) }

// What an assessment says of an area.
const (
	Unaffected = "unaffected" // the change does not touch it
	Affected   = "affected"   // the change touches it; the note says how
	Open       = "open"       // not settled yet; the note says what is open (未决项显式列出)
)

// Assessment is the impact assessment of one area: unaffected, affected or open, a note (required
// unless unaffected) and the evidence it rests on.
type Assessment struct {
	Area     string           `json:"area"`
	Status   string           `json:"status"`
	Note     string           `json:"note"`
	Evidence []quote.Evidence `json:"evidence"`
}

// Limits of an amendment's content.
const (
	MaxEvidence = 200
	maxNote     = 2000
	maxLocator  = 500
	maxReason   = 2000
)

// Content is what an amendment hashes: the order, the revision it binds, the source, the normalized
// proposal and the impact assessment.
type Content struct {
	Contract    string            `json:"amendment_contract"`
	OrderID     string            `json:"order_id"`
	AmendmentID string            `json:"amendment_id"`
	Base        orderrev.Link     `json:"base"`
	Source      Source            `json:"source"`
	Proposal    orderrev.Proposal `json:"proposal"`
	Impact      []Assessment      `json:"impact"`
}

// Application is how an approved amendment was applied: by whom and when, the revision it amended
// and the one it produced, and what changed.
type Application struct {
	Act  quote.Act
	From orderrev.Link
	To   orderrev.Link
	Diff *orderrev.Diff
}

// Amendment is an order amendment (OrderAmendment), identified within its order. Version moves on
// every change of it (expected_version). Prepared holds every preparation in order (the preparers,
// 编制人), Submitted, Decided (approve or reject), Applied and Withdrawn what moved it on.
type Amendment struct {
	ID        string
	OrderID   string
	Content   Content
	Hash      string
	Status    Status
	Version   int64
	Prepared  []quote.Act
	Submitted *quote.Act
	Decided   *quote.Act
	Applied   *Application
	Withdrawn *quote.Act
	Fresh     bool
	Changed   bool
}

// Order is what the amendment rules read of the order: its state and current revision.
type Order struct {
	Status  orderrev.Status
	Current *orderrev.Revision
}

// Preparation is what order.amendment.prepare states: the amendment (new, at ExpectedVersion 0, or
// a draft at its version), the order and the revision the preparer saw, the source, the proposal and
// the impact assessment so far.
type Preparation struct {
	AmendmentID     string
	OrderID         string
	ExpectedVersion int64
	Base            orderrev.Link
	Source          Source
	Proposal        orderrev.Proposal
	Impact          []Assessment
}

// Prepare decides order.amendment.prepare on order o (draft → draft; a is nil for a new amendment)
// by by at now, against the shipments f FUL confirmed and the upstream facts. It refuses, in this
// order: an order that is not active (ORDER_NOT_ACTIVE); an amendment that is not a draft
// (INVALID_STATE) or not at the expected version (VERSION_CONFLICT; a new one has version 0); a
// stated revision that is not the order's current one (ORDER_AMENDMENT_STALE); a malformed source or
// impact; a proposal the next revision cannot be derived from (orderrev.Next); a proposal that
// changes nothing (INVALID_KNOWN_VALUE). It returns the amendment and the revision it would yield
// with what it changes (the approver's preview; applying derives it again).
func Prepare(a *Amendment, p Preparation, o Order, f orderrev.Fulfilment, facts orderrev.Facts, by kernel.ActorRef, epoch int64,
	now time.Time) (*Amendment, *orderrev.Revision, *orderrev.Diff, error) {
	if err := active(o); err != nil {
		return nil, nil, nil, err
	}
	switch {
	case a == nil && p.ExpectedVersion != 0, a != nil && a.Version != p.ExpectedVersion:
		return nil, nil, nil, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case a != nil && a.Status != Draft:
		return nil, nil, nil, kernel.Reject(kernel.CodeInvalidState, "/args/amendment_id", "The amendment is "+string(a.Status))
	case a != nil && (a.ID != p.AmendmentID || a.OrderID != p.OrderID):
		return nil, nil, nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/amendment_id", "The amendment is another order's")
	case !kernel.ValidIdentifier(p.AmendmentID):
		return nil, nil, nil, kernel.Reject(kernel.CodeSchemaInvalid, "/args/amendment_id", "")
	case p.OrderID != o.Current.OrderID:
		return nil, nil, nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "The revision is another order's")
	}
	if err := current(p.Base, o, "/args/expected_order_revision"); err != nil {
		return nil, nil, nil, err
	}
	if err := checkSource(p.Source); err != nil {
		return nil, nil, nil, err
	}
	impact, err := checkImpact(p.Impact)
	if err != nil {
		return nil, nil, nil, err
	}
	// Derive once to validate and normalize the proposal, hash the content, then derive the preview
	// that names the amendment.
	draft, d, err := orderrev.Next(o.Current, orderrev.AmendmentRef{ID: p.AmendmentID}, p.Proposal, f, facts, "/args/proposal")
	if err != nil {
		return nil, nil, nil, err
	}
	if d.Empty() {
		return nil, nil, nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/proposal", "An amendment changes the order")
	}
	c := Content{Contract: Contract, OrderID: p.OrderID, AmendmentID: p.AmendmentID, Base: p.Base, Source: normalSource(p.Source),
		Proposal: orderrev.ProposalOf(draft), Impact: impact}
	hash, err := contentHash(c)
	if err != nil {
		return nil, nil, nil, err
	}
	preview, d, err := orderrev.Next(o.Current, orderrev.AmendmentRef{ID: p.AmendmentID, Hash: hash}, c.Proposal, f, facts, "/args/proposal")
	if err != nil {
		return nil, nil, nil, err
	}
	if a == nil {
		a = &Amendment{ID: p.AmendmentID, OrderID: p.OrderID, Status: Draft, Fresh: true}
	}
	a.Content, a.Hash = c, hash
	a.Prepared = append(a.Prepared, quote.Act{By: by, Epoch: epoch, At: kernel.Instant(now)})
	a.Version++
	a.Changed = true
	return a, preview, d, nil
}

// Submit decides order.amendment.submit (draft → pending_approval) by by at now: the amendment is a
// draft (INVALID_STATE) at the expected version (VERSION_CONFLICT), the order is active
// (ORDER_NOT_ACTIVE) at the bound revision (ORDER_AMENDMENT_STALE), and the impact assessment covers
// every area (DRAFT_NOT_READY at /impact/<area>).
func (a *Amendment) Submit(expected int64, o Order, by kernel.ActorRef, epoch int64, now time.Time) error {
	if a.Status != Draft {
		return kernel.Reject(kernel.CodeInvalidState, "/args/amendment_id", "The amendment is "+string(a.Status))
	}
	if a.Version != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if err := a.bound(o); err != nil {
		return err
	}
	for _, area := range areas {
		if !slices.ContainsFunc(a.Content.Impact, func(x Assessment) bool { return x.Area == area }) {
			return kernel.Reject(kernel.CodeDraftNotReady, "/impact/"+area, "The impact assessment says nothing of "+area+
				"; state it unaffected, affected or open")
		}
	}
	act := quote.Act{By: by, Epoch: epoch, At: kernel.Instant(now)}
	a.Status, a.Submitted = PendingApproval, &act
	a.Version++
	a.Changed = true
	return nil
}

// Decision is what order.amendment.decide states: the amendment hash the decider reviewed, approve
// or reject, a reason (required to reject) and evidence.
type Decision struct {
	Hash     string
	Approve  bool
	Reason   string
	Evidence []quote.Evidence
}

// Decide decides order.amendment.decide (pending_approval → approved or rejected) by by at now. It
// refuses, in this order: a decider who prepared or submitted the amendment
// (REVIEWER_NOT_INDEPENDENT; role templates: 独立于变更的编制人与提交人); an amendment decided before
// (ORDER_REVIEW_IMMUTABLE) or not pending (INVALID_STATE); another version (VERSION_CONFLICT); an
// order that is not active or no longer at the bound revision (ORDER_NOT_ACTIVE,
// ORDER_AMENDMENT_STALE); a stated hash that is not the amendment's (ORDER_REVIEW_MISMATCH); a
// rejection without a reason (SCHEMA_INVALID). The caller checked the decider's current
// qualification (a human holding order.amendment.decide and trd.read who sees the order).
func (a *Amendment) Decide(expected int64, d Decision, o Order, by kernel.ActorRef, epoch int64, now time.Time) error {
	if !a.independent(by) {
		return fail(CodeReviewerNotIndependent, "/args/amendment_id", "The decider prepared or submitted the amendment")
	}
	switch a.Status {
	case PendingApproval:
	case Approved, Rejected, Applied:
		return fail(CodeOrderReviewImmutable, "/args/amendment_id", "The amendment was decided")
	default:
		return kernel.Reject(kernel.CodeInvalidState, "/args/amendment_id", "The amendment is "+string(a.Status))
	}
	if a.Version != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if err := a.bound(o); err != nil {
		return err
	}
	if d.Hash != a.Hash {
		return fail(CodeOrderReviewMismatch, "/args/amendment_hash", "The amendment has another hash")
	}
	if err := checkAct(by, d.Reason, d.Evidence, !d.Approve); err != nil {
		return err
	}
	act := quote.Act{By: by, Epoch: epoch, At: kernel.Instant(now), Reason: d.Reason, Evidence: slices.Clone(d.Evidence)}
	a.Status, a.Decided = Rejected, &act
	if d.Approve {
		a.Status = Approved
	}
	a.Version++
	a.Changed = true
	return nil
}

// Applying is what order.amendment.apply brings once its transaction holds the order: the expected
// amendment version, the command's idempotency key, the shipments FUL confirmed and the upstream
// facts read for the proposal, what the upstream guards answered (Upstream entries, the extension
// points not wired among them), the quote root's control epoch and the order's row version the
// apply commits at.
type Applying struct {
	ExpectedVersion int64
	IdempotencyKey  string
	Fulfilment      orderrev.Fulfilment
	Facts           orderrev.Facts
	Upstream        []quote.Entry
	ControlEpoch    int64
	RowVersion      int64
}

// Commit is the commit evidence of an applied amendment (TRD-INV-016): its entries in a fixed order.
type Commit struct {
	Entries []quote.Entry
}

// Apply decides order.amendment.apply (approved → applied; SM-TRD-ORDER active → active) by by under
// authority epoch epoch at now, and returns the next order revision, what it changes and the commit
// evidence. It refuses, in this order: an amendment not yet approved (ORDER_APPROVAL_REQUIRED) or
// ended (INVALID_STATE); another version (VERSION_CONFLICT); an order that is not active
// (ORDER_NOT_ACTIVE) or no longer at the bound revision (ORDER_AMENDMENT_STALE: 订单修订未变（CAS）);
// a decider no longer independent (REVIEWER_NOT_INDEPENDENT) or no longer an active human grant
// holding order.amendment.decide and trd.read that sees the order (ORDER_APPROVAL_REQUIRED, as an
// order approval whose reviewer lost the qualification); then whatever orderrev.Next refuses of the
// proposal against the shipments confirmed now (QUANTITY_BELOW_SHIPPED and the rest).
func (a *Amendment) Apply(x Applying, o Order, standings quote.Standings, by kernel.ActorRef, epoch int64,
	now time.Time) (*orderrev.Revision, *orderrev.Diff, *Commit, error) {
	switch a.Status {
	case Approved:
	case Draft, PendingApproval:
		return nil, nil, nil, fail(CodeOrderApprovalRequired, "/args/amendment_id", "The amendment is "+string(a.Status)+", not approved")
	default:
		return nil, nil, nil, kernel.Reject(kernel.CodeInvalidState, "/args/amendment_id", "The amendment is "+string(a.Status))
	}
	if a.Version != x.ExpectedVersion {
		return nil, nil, nil, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if err := a.bound(o); err != nil {
		return nil, nil, nil, err
	}
	if err := a.Admit(standings); err != nil {
		return nil, nil, nil, err
	}
	if err := checkAct(by, "", nil, false); err != nil {
		return nil, nil, nil, err
	}
	next, d, err := orderrev.Next(o.Current, orderrev.AmendmentRef{ID: a.ID, Hash: a.Hash}, a.Content.Proposal, x.Fulfilment, x.Facts, "/proposal")
	if err != nil {
		return nil, nil, nil, err
	}
	at := kernel.Instant(now)
	act := quote.Act{By: by, Epoch: epoch, At: at}
	a.Status = Applied
	a.Applied = &Application{Act: act, From: o.Current.Link(), To: next.Link(), Diff: d}
	a.Version++
	a.Changed = true
	return next, d, &Commit{Entries: a.entries(x, o.Current.Link(), next.Link(), by, epoch, at)}, nil
}

// Admit checks, when the amendment is used, that its decider is still independent of its preparers
// and submitter (REVIEWER_NOT_INDEPENDENT) and still an active human grant holding
// order.amendment.decide and trd.read that sees the order (ORDER_APPROVAL_REQUIRED otherwise), as
// standings reads it now (TRD-INV-013's rule for order approvals, applied to the amendment's).
func (a *Amendment) Admit(standings quote.Standings) error {
	d := a.Decided
	if a.Status != Approved || d == nil {
		return fail(CodeOrderApprovalRequired, "/args/amendment_id", "")
	}
	if !a.independent(d.By) {
		return fail(CodeReviewerNotIndependent, "/args/amendment_id", "")
	}
	s, err := standings(d.By.ID)
	if err != nil {
		return err
	}
	if d.By.Kind != kernel.ActorHuman || !s.Human || !s.SeesRoot || !slices.Contains(s.Permissions, PermissionDecide) ||
		!slices.Contains(s.Permissions, PermissionRead) {
		return fail(CodeOrderApprovalRequired, "/args/amendment_id", "The amendment's decider is no longer qualified")
	}
	return nil
}

// Withdraw decides order.amendment.withdraw (draft or pending_approval → withdrawn) by act: another
// state is INVALID_STATE, another version VERSION_CONFLICT; a human gives the reason
// (申请人或负责人；理由: who may withdraw is the command's permission). A pending amendment's approval
// request closes with it.
func (a *Amendment) Withdraw(expected int64, act quote.Act) error {
	if a.Status != Draft && a.Status != PendingApproval {
		return kernel.Reject(kernel.CodeInvalidState, "/args/amendment_id", "The amendment is "+string(a.Status))
	}
	if a.Version != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if err := checkAct(act.By, act.Reason, act.Evidence, true); err != nil {
		return err
	}
	act.At, act.Evidence = kernel.Instant(act.At), slices.Clone(act.Evidence)
	a.Status, a.Withdrawn = Withdrawn, &act
	a.Version++
	a.Changed = true
	return nil
}

// Stale reports whether the order no longer is at the revision the amendment binds: it can be
// withdrawn (draft, pending) or prepared again (draft), never applied.
func (a *Amendment) Stale(o Order) bool {
	return o.Current == nil || a.Content.Base != o.Current.Link()
}

// Preparers returns everyone who prepared the amendment, once each, in order.
func (a *Amendment) Preparers() []kernel.ActorRef {
	var out []kernel.ActorRef
	for _, p := range a.Prepared {
		if !slices.Contains(out, p.By) {
			out = append(out, p.By)
		}
	}
	return out
}

// OpenItems returns the areas the impact assessment leaves open, in their order.
func (a *Amendment) OpenItems() []string {
	var out []string
	for _, x := range a.Content.Impact {
		if x.Status == Open {
			out = append(out, x.Area)
		}
	}
	return out
}

// independent reports whether by neither prepared nor submitted the amendment.
func (a *Amendment) independent(by kernel.ActorRef) bool {
	for _, p := range a.Prepared {
		if p.By.ID == by.ID {
			return false
		}
	}
	return a.Submitted == nil || a.Submitted.By.ID != by.ID
}

// bound refuses a command on an order that is not active, or no longer at the bound revision.
func (a *Amendment) bound(o Order) error {
	if err := active(o); err != nil {
		return err
	}
	return current(a.Content.Base, o, "/args/amendment_id")
}

// entries are the commit evidence of the apply, in a fixed order: the command and its key, the
// order, the amendment and its hash, the revision amended and the one produced, the source, who
// prepared, submitted and decided, the open items, where the shipments came from and what they
// were, the upstream answers, the epochs, the row version, who committed and when.
func (a *Amendment) entries(x Applying, from, to orderrev.Link, by kernel.ActorRef, epoch int64, at time.Time) []quote.Entry {
	actor := func(r kernel.ActorRef) string { return string(r.Kind) + ":" + r.ID }
	var preparers []string
	for _, p := range a.Preparers() {
		preparers = append(preparers, actor(p))
	}
	src := a.Content.Source.Kind
	if a.Content.Source.ResponseID != "" {
		src += ":" + a.Content.Source.ResponseID
	}
	var shipped []string
	for _, l := range slices.Sorted(maps.Keys(x.Fulfilment.Shipped)) {
		shipped = append(shipped, l+"="+x.Fulfilment.Shipped[l].String())
	}
	fulfilment := x.Fulfilment.Source
	if strings.TrimSpace(fulfilment) == "" {
		fulfilment = orderrev.NotWired
	}
	out := []quote.Entry{{Name: "command", Value: CommandApply}, {Name: "idempotency_key", Value: x.IdempotencyKey},
		{Name: "order_id", Value: a.OrderID}, {Name: "amendment_id", Value: a.ID}, {Name: "amendment_hash", Value: a.Hash},
		{Name: "from_order_revision", Value: itoa(from.Revision)}, {Name: "from_order_hash", Value: from.Hash},
		{Name: "to_order_revision", Value: itoa(to.Revision)}, {Name: "to_order_hash", Value: to.Hash},
		{Name: "source", Value: src}, {Name: "prepared_by", Value: strings.Join(preparers, ",")},
		{Name: "submitted_by", Value: actor(a.Submitted.By)}, {Name: "decided_by", Value: actor(a.Decided.By)},
		{Name: "decided_at", Value: kernel.FormatTimestamp(a.Decided.At)}, {Name: "open_items", Value: strings.Join(a.OpenItems(), ",")},
		{Name: "fulfilment_source", Value: fulfilment}, {Name: "shipped", Value: strings.Join(shipped, ",")}}
	out = append(out, x.Upstream...)
	return append(out, quote.Entry{Name: "authority_epoch", Value: itoa(epoch)}, quote.Entry{Name: "control_epoch", Value: itoa(x.ControlEpoch)},
		quote.Entry{Name: "row_version", Value: itoa(x.RowVersion)}, quote.Entry{Name: "committed_by", Value: actor(by)},
		quote.Entry{Name: "committed_at", Value: kernel.FormatTimestamp(at)})
}

// active refuses an order that is not active (ORDER_NOT_ACTIVE).
func active(o Order) error {
	if o.Status != orderrev.Active || o.Current == nil {
		return fail(CodeOrderNotActive, "/args/order_id", "The order is "+string(o.Status))
	}
	return nil
}

// current refuses a bound revision that is not the order's current one (ORDER_AMENDMENT_STALE).
func current(base orderrev.Link, o Order, path string) error {
	if base != o.Current.Link() {
		return fail(CodeOrderAmendmentStale, path, "The order is at revision "+itoa(o.Current.Number))
	}
	return nil
}

// checkSource checks the source's shape: a known kind, the response of a response and only then,
// evidence of 1–200 sources for a PO revision or a confirmed proposal (0–200 for a response).
func checkSource(s Source) error {
	switch s.Kind {
	case SourceCustomerResponse:
		if !kernel.ValidIdentifier(s.ResponseID) {
			return kernel.Reject(kernel.CodeSchemaInvalid, "/args/source/response_id", "")
		}
	case SourceCustomerPORevision, SourceConfirmedProposal:
		if s.ResponseID != "" {
			return kernel.Reject(kernel.CodeSchemaInvalid, "/args/source/response_id", "Only a customer response names one")
		}
		if len(s.Evidence) == 0 {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/source/evidence", "The change rests on the customer's document")
		}
	default:
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/source/kind", "")
	}
	return checkEvidence(s.Evidence, "/args/source/evidence")
}

// checkImpact checks an impact assessment (complete or not) and returns it in the areas' order:
// known areas once each, a known status, a note of at most 2000 characters that an affected or open
// area needs, and its evidence.
func checkImpact(impact []Assessment) ([]Assessment, error) {
	seen := map[string]bool{}
	for i, x := range impact {
		at := "/args/impact/" + strconv.Itoa(i)
		switch {
		case !slices.Contains(areas, x.Area):
			return nil, kernel.Reject(kernel.CodeSchemaInvalid, at+"/area", "")
		case seen[x.Area]:
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, at+"/area", "Each area is assessed once")
		case x.Status != Unaffected && x.Status != Affected && x.Status != Open:
			return nil, kernel.Reject(kernel.CodeSchemaInvalid, at+"/status", "")
		case !utf8.ValidString(x.Note) || utf8.RuneCountInString(x.Note) > maxNote:
			return nil, kernel.Reject(kernel.CodeSchemaInvalid, at+"/note", "")
		case x.Status != Unaffected && strings.TrimSpace(x.Note) == "":
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, at+"/note", "An affected or open area says how")
		}
		if err := checkEvidence(x.Evidence, at+"/evidence"); err != nil {
			return nil, err
		}
		seen[x.Area] = true
	}
	out := make([]Assessment, 0, len(impact))
	for _, area := range areas {
		if i := slices.IndexFunc(impact, func(x Assessment) bool { return x.Area == area }); i >= 0 {
			x := impact[i]
			x.Evidence = evidenceOf(x.Evidence)
			out = append(out, x)
		}
	}
	return out, nil
}

func checkEvidence(evidence []quote.Evidence, path string) error {
	if len(evidence) > MaxEvidence {
		return kernel.Reject(kernel.CodeSchemaInvalid, path, "")
	}
	for i, e := range evidence {
		if !kernel.ValidIdentifier(e.SourceID) || strings.TrimSpace(e.Locator) == "" || utf8.RuneCountInString(e.Locator) > maxLocator {
			return kernel.Reject(kernel.CodeSchemaInvalid, path+"/"+strconv.Itoa(i), "Evidence names a source and a locator")
		}
	}
	return nil
}

// checkAct checks who acts and why: a human, with a reason of at most 2000 characters when one is
// needed, and well-formed evidence.
func checkAct(by kernel.ActorRef, reason string, evidence []quote.Evidence, needReason bool) error {
	switch {
	case by.Kind != kernel.ActorHuman:
		return kernel.Reject(kernel.CodePermissionDenied, "/", "A human decides, applies and withdraws amendments")
	case needReason && strings.TrimSpace(reason) == "":
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/reason", "A reason is required")
	case utf8.RuneCountInString(reason) > maxReason:
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/reason", "")
	}
	return checkEvidence(evidence, "/args/evidence")
}

func normalSource(s Source) Source {
	s.Evidence = evidenceOf(s.Evidence)
	return s
}

func evidenceOf(e []quote.Evidence) []quote.Evidence {
	if e == nil {
		return []quote.Evidence{}
	}
	return slices.Clone(e)
}

// contentHash returns the amendment hash: the SHA-256 of the content's canonical JSON.
func contentHash(c Content) (string, error) {
	tree, err := quote.TreeOf(c)
	if err != nil {
		return "", err
	}
	doc, err := quote.Canonical(tree)
	if err != nil {
		return "", err
	}
	return quote.Hash(doc), nil
}

// Document returns the content's canonical JSON, the bytes the amendment hash covers.
func (a *Amendment) Document() ([]byte, error) {
	tree, err := quote.TreeOf(a.Content)
	if err != nil {
		return nil, err
	}
	return quote.Canonical(tree)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
