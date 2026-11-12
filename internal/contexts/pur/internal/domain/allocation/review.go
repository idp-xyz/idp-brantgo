package allocation

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The allocation review after an order change (tactical model PUR-AGG-DemandAllocation entity
// AllocationReview, SM-PUR-ALLOCATION-REVIEW; WF-06 step 4 and its idempotency and failure mode;
// event storming PL-09, ES-09 step 5; S5 decisions D-18, D-21; BT10). TRD's OrderAmended,
// OrderCancelled and OrderRemainderClosed say what the order needs of each line now (Target); Assess
// compares it with PUR's facts of the order — the DemandAllocation roots with their entries, the
// drafts and the commitments — and lists what the change touches as items: an order line allocated
// above its target, a prepared draft never committed holding such a line, an issued or confirmed
// commitment buying on it. One review is kept per (order, order revision, cause); a review opened for
// a later order version supersedes the open ones of earlier versions. A person settles a review by
// stating, for every item, the disposition actually made with PUR's commands — a draft voided, a
// commitment changed or cancelled — or no_action, each with a reason; the review makes none itself.
//
//	open ──allocation_review.settle (human)──▶ settled
//	  └───allocation_review.open (system, a later order version)──▶ superseded

// States, causes, kinds, impacts and dispositions (api constants).
const (
	ReviewOpen       = "open"
	ReviewSettled    = "settled"
	ReviewSuperseded = "superseded"

	CauseOrderAmended         = "order_amended"
	CauseOrderCancelled       = "order_cancelled"
	CauseOrderRemainderClosed = "order_remainder_closed"

	ItemAllocation = "allocation"
	ItemDraft      = "draft"
	ItemCommitment = "commitment"

	ImpactReduced              = "reduced"
	ImpactLineRemoved          = "line_removed"
	ImpactSpecificationChanged = "specification_changed"
	ImpactOrderCancelled       = "order_cancelled"
	ImpactRemainderClosed      = "remainder_closed"

	DispositionDraftVoided         = "draft_voided"
	DispositionCommitmentChanged   = "commitment_changed"
	DispositionCommitmentCancelled = "commitment_cancelled"
	DispositionNoAction            = "no_action"

	// ResponsibleRole is the role template that settles reviews (role templates: 采购).
	ResponsibleRole = "purchaser"

	// States of the drafts and commitments a review reads.
	draftPrepared       = "prepared"
	draftVoided         = "voided"
	commitmentIssued    = "issued"
	commitmentConfirmed = "confirmed"
	commitmentCancelled = "cancelled"
	changeApplied       = "applied"
)

// Spec is the SKU specification revision and unit of an order line, as the order states it or as a
// draft bought it.
type Spec struct {
	SkuID        string
	SpecRevision int64
	UnitID       string
}

// Target is what the order needs of line LineID after the change, in the line's unit. Spec is the
// specification the amended revision states (nil after an order's end, which states none).
type Target struct {
	LineID   string
	Quantity kernel.Decimal
	Spec     *Spec
}

// Change is what a TRD event says happened to the order: the order at revision Revision and hash
// Hash, TRD's row version of the order after it (Version), the cause and event, and what the order
// needs of each line it still has.
type Change struct {
	OrderID  string
	Revision int64
	Hash     string
	Version  int64
	Cause    string
	EventID  string
	Targets  map[string]Target
}

// DraftLine is one line of a draft: what it allocates and overbuys of the order line, and the
// specification it bought.
type DraftLine struct {
	LineID   string
	Quantity kernel.Decimal
	Overbuy  kernel.Decimal
	Spec     Spec
}

// HeldDraft is a draft of the order with its lines and its latest commitment, if any (CommitmentID
// and CommitmentStatus; Committed once a commitment of it was issued, whatever became of it since).
type HeldDraft struct {
	ID               string
	Hash             string
	Status           string
	SupplierID       string
	Committed        bool
	CommitmentID     string
	CommitmentStatus string
	Lines            []DraftLine
}

// CommitmentLine is what a commitment buys of an order line at its revision in force and what was
// verified delivered of it.
type CommitmentLine struct {
	LineID    string
	Allocated kernel.Decimal
	Overbuy   kernel.Decimal
	Purchased kernel.Decimal
	Verified  kernel.Decimal
}

// HeldCommitment is a commitment of the order at its revision in force.
type HeldCommitment struct {
	ID         string
	DraftID    string
	Status     string
	Revision   int64
	SupplierID string
	PONumber   string
	Fulfilled  bool
	Lines      []CommitmentLine
}

// Holdings are PUR's facts of one order: its DemandAllocation roots with their entries, its drafts
// and its commitments.
type Holdings struct {
	Allocations []*Allocation
	Drafts      []HeldDraft
	Commitments []HeldCommitment
}

// FactLine is what an item says of one order line (api.ReviewLineView).
type FactLine struct {
	OrderLineID       string `json:"order_line_id"`
	Impact            string `json:"impact"`
	TargetQuantity    string `json:"target_quantity"`
	UnitID            string `json:"unit_id,omitempty"`
	DemandQuantity    string `json:"demand_quantity,omitempty"`
	DemandRevision    int64  `json:"demand_revision,omitempty"`
	AllocatedQuantity string `json:"allocated_quantity,omitempty"`
	ExcessQuantity    string `json:"excess_quantity,omitempty"`
	OverbuyQuantity   string `json:"overbuy_quantity,omitempty"`
	PurchaseQuantity  string `json:"purchase_quantity,omitempty"`
	VerifiedQuantity  string `json:"verified_quantity,omitempty"`
}

// Holder is a draft holding an allocation item's line (api.ReviewHolderView).
type Holder struct {
	DraftID            string `json:"draft_id"`
	Quantity           string `json:"quantity"`
	CommitmentID       string `json:"commitment_id,omitempty"`
	CommitmentStatus   string `json:"commitment_status,omitempty"`
	CommitmentRevision int64  `json:"commitment_revision,omitempty"`
}

// Facts are what PUR knew of an item's object when the review opened; stored as JSON.
type Facts struct {
	Kind               string     `json:"kind"`
	OrderLineID        string     `json:"order_line_id,omitempty"`
	DraftID            string     `json:"draft_id,omitempty"`
	DraftHash          string     `json:"draft_hash,omitempty"`
	CommitmentID       string     `json:"commitment_id,omitempty"`
	CommitmentRevision int64      `json:"commitment_revision,omitempty"`
	Status             string     `json:"status,omitempty"`
	SupplierID         string     `json:"supplier_id,omitempty"`
	PONumber           string     `json:"po_number,omitempty"`
	Fulfilled          bool       `json:"fulfilled,omitempty"`
	Lines              []FactLine `json:"lines"`
	Holders            []Holder   `json:"holders,omitempty"`
}

// Disposition is how an item was settled: the disposition, the object it names, what that object
// showed of it when the review was settled (Revision: the commitment revision a change or the
// cancellation made; At: when the draft was voided, the change applied or the commitment cancelled)
// and the reason.
type Disposition struct {
	Kind         string
	DraftID      string
	ChangeID     string
	CommitmentID string
	Revision     int64
	At           time.Time
	Reason       string
}

// Item is one thing a review lists, numbered from 1.
type Item struct {
	No    int
	Facts Facts
	Done  *Disposition
}

// Act is who did something to a review, under which authority epoch, when and why.
type Act struct {
	By     kernel.ActorRef
	Epoch  int64
	At     time.Time
	Reason string
}

// Review is one allocation review.
type Review struct {
	ID           string
	OrderID      string
	Revision     int64
	Hash         string
	Version      int64 // TRD's row version of the order the review follows
	Cause        string
	EventID      string
	Status       string
	RowVersion   int64
	Role         string
	Items        []Item
	Opened       Act
	Settled      *Act
	SupersededBy string
	SupersededAt time.Time
	Stored       int64 // the row version as loaded (0: new)
}

// FactsJSON is the stored form of f.
func (f Facts) FactsJSON() string {
	b, _ := json.Marshal(f)
	return string(b)
}

// ParseFacts reads stored facts.
func ParseFacts(s string) (Facts, error) {
	var f Facts
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return Facts{}, fmt.Errorf("pur: stored review item facts: %w", err)
	}
	return f, nil
}

func exact(d kernel.Decimal) string {
	s, _ := d.Exact()
	return s
}

// touched is one order line the change touches: what the order needs of it now and why.
type touched struct {
	target kernel.Decimal
	impact string
}

// lineImpact decides whether the change touches line a, the DemandAllocation root of one order line
// with drafts drafts: its effective allocation is above what the order now needs of it, the line is
// gone, or the specification bought is not the one the revision states (BT10 改色).
func lineImpact(c Change, a *Allocation, drafts []HeldDraft) (touched, bool) {
	t, present := c.Targets[a.Key.LineID]
	out := touched{target: t.Quantity}
	switch c.Cause {
	case CauseOrderCancelled:
		out.impact = ImpactOrderCancelled
	case CauseOrderRemainderClosed:
		out.impact = ImpactRemainderClosed
	default:
		switch {
		case !present:
			out = touched{impact: ImpactLineRemoved}
		case t.Spec != nil && specChanged(*t.Spec, a, drafts):
			out = touched{impact: ImpactSpecificationChanged}
		default:
			out.impact = ImpactReduced
		}
	}
	if !present {
		out.target = kernel.Decimal{}
	}
	return out, a.Allocated.Cmp(out.target) > 0
}

// specChanged reports whether a draft holding line a bought another specification or unit than s.
func specChanged(s Spec, a *Allocation, drafts []HeldDraft) bool {
	for _, d := range drafts {
		if !a.Holds(d.ID) {
			continue
		}
		for _, l := range d.Lines {
			if l.LineID == a.Key.LineID && l.Spec != s {
				return true
			}
		}
	}
	return false
}

// Assess lists what change c touches of holdings h, in order: the allocation items by order line,
// then the draft items by draft ID, then the commitment items by commitment ID. A draft is listed
// when it is prepared, was never committed and holds a touched line; a commitment when it is issued
// or confirmed and its draft holds a touched line.
func Assess(c Change, h Holdings) []Item {
	drafts := map[string]HeldDraft{}
	for _, d := range h.Drafts {
		drafts[d.ID] = d
	}
	byDraft := map[string]HeldCommitment{}
	for _, cm := range h.Commitments {
		if cm.Status == commitmentIssued || cm.Status == commitmentConfirmed || cm.Status == commitmentCancelled {
			byDraft[cm.DraftID] = cm
		}
	}
	roots := slices.Clone(h.Allocations)
	Sort(roots)
	hit := map[string]touched{}
	var items []Item
	for _, a := range roots {
		t, ok := lineImpact(c, a, h.Drafts)
		if !ok {
			continue
		}
		hit[a.Key.LineID] = t
		f := Facts{Kind: ItemAllocation, OrderLineID: a.Key.LineID, Lines: []FactLine{{OrderLineID: a.Key.LineID, Impact: t.impact,
			TargetQuantity: exact(t.target), UnitID: a.Unit.ID, DemandQuantity: exact(a.Demand), DemandRevision: a.OrderRevision,
			AllocatedQuantity: exact(a.Allocated), ExcessQuantity: exact(a.Allocated.Sub(t.target))}}, Holders: []Holder{}}
		for _, e := range a.Entries {
			if !e.Effective() {
				continue
			}
			hd := Holder{DraftID: e.DraftID, Quantity: exact(e.Quantity)}
			if cm, ok := byDraft[e.DraftID]; ok {
				hd.CommitmentID, hd.CommitmentStatus, hd.CommitmentRevision = cm.ID, cm.Status, cm.Revision
			}
			f.Holders = append(f.Holders, hd)
		}
		items = append(items, Item{Facts: f})
	}
	held := func(draftID string) []string {
		var lines []string
		for _, a := range roots {
			if _, ok := hit[a.Key.LineID]; ok && a.Holds(draftID) {
				lines = append(lines, a.Key.LineID)
			}
		}
		return lines
	}
	for _, id := range slices.Sorted(maps.Keys(drafts)) {
		d := drafts[id]
		lines := held(id)
		if d.Status != draftPrepared || d.Committed || len(lines) == 0 {
			continue
		}
		f := Facts{Kind: ItemDraft, DraftID: d.ID, DraftHash: d.Hash, Status: d.Status, SupplierID: d.SupplierID, CommitmentID: d.CommitmentID,
			Lines: []FactLine{}}
		if d.CommitmentID != "" {
			f.Status = d.Status + "/" + d.CommitmentStatus
		}
		for _, l := range d.Lines {
			if t, ok := hit[l.LineID]; ok {
				f.Lines = append(f.Lines, FactLine{OrderLineID: l.LineID, Impact: t.impact, TargetQuantity: exact(t.target), UnitID: l.Spec.UnitID,
					AllocatedQuantity: exact(l.Quantity), OverbuyQuantity: exact(l.Overbuy), PurchaseQuantity: exact(l.Quantity.Add(l.Overbuy))})
			}
		}
		items = append(items, Item{Facts: f})
	}
	commitments := slices.Clone(h.Commitments)
	slices.SortFunc(commitments, func(a, b HeldCommitment) int { return cmp.Compare(a.ID, b.ID) })
	for _, cm := range commitments {
		if (cm.Status != commitmentIssued && cm.Status != commitmentConfirmed) || len(held(cm.DraftID)) == 0 {
			continue
		}
		f := Facts{Kind: ItemCommitment, DraftID: cm.DraftID, CommitmentID: cm.ID, CommitmentRevision: cm.Revision, Status: cm.Status,
			SupplierID: cm.SupplierID, PONumber: cm.PONumber, Fulfilled: cm.Fulfilled, Lines: []FactLine{}}
		unit := map[string]string{}
		if d, ok := drafts[cm.DraftID]; ok {
			for _, l := range d.Lines {
				unit[l.LineID] = l.Spec.UnitID
			}
		}
		for _, l := range cm.Lines {
			if t, ok := hit[l.LineID]; ok {
				f.Lines = append(f.Lines, FactLine{OrderLineID: l.LineID, Impact: t.impact, TargetQuantity: exact(t.target), UnitID: unit[l.LineID],
					AllocatedQuantity: exact(l.Allocated), OverbuyQuantity: exact(l.Overbuy), PurchaseQuantity: exact(l.Purchased),
					VerifiedQuantity: exact(l.Verified)})
			}
		}
		items = append(items, Item{Facts: f})
	}
	for i := range items {
		items[i].No = i + 1
	}
	return items
}

// Effect is what an order change does to PUR's reviews of the order.
type Effect string

const (
	// EffectOpened: a review is opened now (and earlier open ones are superseded).
	EffectOpened Effect = "review_opened"
	// EffectAlreadyOpen: a review of this order revision and cause exists (opened before, settled or
	// superseded since); nothing is opened twice and nothing settled is reopened.
	EffectAlreadyOpen Effect = "review_exists"
	// EffectLater: a review of the order follows a later order version; the change is older news.
	EffectLater Effect = "later_review"
	// EffectNothingHeld: PUR holds nothing of the order.
	EffectNothingHeld Effect = "nothing_held"
	// EffectNothingTouched: the change touches nothing PUR holds and no review of the order is open.
	EffectNothingTouched Effect = "nothing_touched"
)

// Plan decides what change c does, given the order's reviews and the items Assess listed: whether a
// review opens, and which open reviews it supersedes (WF-06: 各下游按（订单、订单修订）只开启一次复核；更晚修订的复核
// 取代未处置的旧复核). A review opens when something is touched, or when an open review of an earlier
// order version is to be superseded (then possibly without items).
func Plan(c Change, reviews []*Review, h Holdings, items []Item) (Effect, []*Review) {
	if slices.ContainsFunc(reviews, func(r *Review) bool { return r.Revision == c.Revision && r.Cause == c.Cause }) {
		return EffectAlreadyOpen, nil
	}
	if slices.ContainsFunc(reviews, func(r *Review) bool { return r.Version >= c.Version }) {
		return EffectLater, nil
	}
	var open []*Review
	for _, r := range reviews {
		if r.Status == ReviewOpen {
			open = append(open, r)
		}
	}
	switch {
	case len(h.Allocations) == 0 && len(open) == 0:
		return EffectNothingHeld, nil
	case len(items) == 0 && len(open) == 0:
		return EffectNothingTouched, nil
	}
	return EffectOpened, open
}

// OpenReview returns the review change c opens with items, by the system actor act.
func OpenReview(id string, c Change, items []Item, act Act) *Review {
	act.At = kernel.Instant(act.At)
	return &Review{ID: id, OrderID: c.OrderID, Revision: c.Revision, Hash: c.Hash, Version: c.Version, Cause: c.Cause, EventID: c.EventID,
		Status: ReviewOpen, RowVersion: 1, Role: ResponsibleRole, Items: items, Opened: act}
}

// Supersede marks open review r superseded by review by at instant at.
func (r *Review) Supersede(by string, at time.Time) {
	r.Status, r.RowVersion, r.SupersededBy, r.SupersededAt = ReviewSuperseded, r.RowVersion+1, by, kernel.Instant(at)
}

// Now is what PUR shows now of the objects a settlement names: drafts (state and when voided),
// commitments (state, revision in force and when cancelled), changes (commitment, state, the
// revision they were bound to and the one their application made, when applied) and the effective
// allocation of each order line of the order.
type Now struct {
	Drafts      map[string]DraftNow
	Commitments map[string]CommitmentNow
	Changes     map[string]ChangeNow
	Allocated   map[string]kernel.Decimal
}

// DraftNow is a draft as PUR shows it now.
type DraftNow struct {
	Status   string
	VoidedAt time.Time
}

// CommitmentNow is a commitment as PUR shows it now.
type CommitmentNow struct {
	Status      string
	Revision    int64
	CancelledAt time.Time
}

// ChangeNow is a commitment change as PUR shows it now.
type ChangeNow struct {
	CommitmentID    string
	Status          string
	BaseRevision    int64
	AppliedRevision int64
	AppliedAt       time.Time
}

// Settlement is one item's stated disposition.
type Settlement struct {
	ItemNo       int
	Kind         string
	DraftID      string
	ChangeID     string
	CommitmentID string
	Reason       string
}

func itemPath(i int, member string) string { return "/args/items/" + strconv.Itoa(i) + member }

// Settle settles review r at expected with one disposition per item (ss), by human act, on what PUR
// shows now (SM-PUR-ALLOCATION-REVIEW open → settled: "每项有处置（作废草稿、取消或变更承诺、确认无需处理）与理由").
// It refuses, in this order: another version (VERSION_CONFLICT), a superseded review (REFERENCE_STALE:
// the later order version's review is the one to settle), a settled one (INVALID_STATE), an item
// the review does not have (REFERENCE_INVALID), an item without a disposition (DRAFT_NOT_READY), a
// disposition that does not fit the item's kind (INVALID_KNOWN_VALUE), an object that is not the
// item's (REFERENCE_INVALID), and a disposition that has not happened — the draft is not voided, the
// change is not applied or was bound to an earlier revision than the one the review saw, the
// commitment is not cancelled, or an allocation item's line is still allocated above its target
// (INVALID_STATE; no_action states why nothing is done).
func (r *Review) Settle(expected int64, ss []Settlement, now Now, act Act) error {
	switch {
	case r.RowVersion != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case r.Status == ReviewSuperseded:
		return kernel.RejectAs(CodeReferenceStale, Codes[CodeReferenceStale], "/args/review_id",
			"A review of a later order version superseded this one: "+r.SupersededBy)
	case r.Status != ReviewOpen:
		return kernel.Reject(kernel.CodeInvalidState, "/args/review_id", "The review is "+r.Status)
	}
	stated := map[int]int{}
	for i, s := range ss {
		if s.ItemNo < 1 || s.ItemNo > len(r.Items) {
			return kernel.Reject(kernel.CodeReferenceInvalid, itemPath(i, "/item_no"), "The review has no such item")
		}
		stated[s.ItemNo] = i
	}
	for _, it := range r.Items {
		if _, ok := stated[it.No]; !ok {
			return kernel.Reject(kernel.CodeDraftNotReady, "/args/items", "Item "+strconv.Itoa(it.No)+" has no disposition")
		}
	}
	done := make([]*Disposition, len(r.Items))
	for i, s := range ss {
		d, err := r.Items[s.ItemNo-1].judge(i, s, now)
		if err != nil {
			return err
		}
		done[s.ItemNo-1] = d
	}
	for i := range r.Items {
		r.Items[i].Done = done[i]
	}
	act.At = kernel.Instant(act.At)
	r.Status, r.RowVersion, r.Settled = ReviewSettled, r.RowVersion+1, &act
	return nil
}

// judge checks settlement s (the i-th stated) of item it against what PUR shows now.
func (it Item) judge(i int, s Settlement, now Now) (*Disposition, error) {
	f := it.Facts
	d := &Disposition{Kind: s.Kind, DraftID: s.DraftID, ChangeID: s.ChangeID, CommitmentID: s.CommitmentID, Reason: s.Reason}
	if s.Kind == DispositionNoAction {
		return d, nil
	}
	mismatch := kernel.Reject(kernel.CodeInvalidKnownValue, itemPath(i, "/disposition"), "A "+f.Kind+" item is not settled by "+s.Kind)
	// The drafts and commitments (with the revision the review saw) the item is about.
	drafts, commitments := map[string]bool{}, map[string]int64{}
	switch f.Kind {
	case ItemDraft:
		drafts[f.DraftID] = true
	case ItemCommitment:
		commitments[f.CommitmentID] = f.CommitmentRevision
	case ItemAllocation:
		for _, h := range f.Holders {
			if h.CommitmentID == "" {
				drafts[h.DraftID] = true
			} else if h.CommitmentStatus != commitmentCancelled {
				commitments[h.CommitmentID] = h.CommitmentRevision
			}
		}
	}
	switch s.Kind {
	case DispositionDraftVoided:
		if f.Kind == ItemCommitment {
			return nil, mismatch
		}
		if !drafts[s.DraftID] {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, itemPath(i, "/draft_id"), "The item is not about draft "+s.DraftID)
		}
		dr, ok := now.Drafts[s.DraftID]
		if !ok || dr.Status != draftVoided {
			return nil, kernel.Reject(kernel.CodeInvalidState, itemPath(i, "/draft_id"), "Draft "+s.DraftID+" is not voided")
		}
		d.At = dr.VoidedAt
	case DispositionCommitmentChanged:
		if f.Kind == ItemDraft {
			return nil, mismatch
		}
		x, ok := now.Changes[s.ChangeID]
		seen, about := commitments[x.CommitmentID]
		if !ok || !about {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, itemPath(i, "/change_id"), "Change "+s.ChangeID+" is not of the item's commitment")
		}
		if x.Status != changeApplied || x.BaseRevision < seen {
			return nil, kernel.Reject(kernel.CodeInvalidState, itemPath(i, "/change_id"),
				"Change "+s.ChangeID+" is not applied to the commitment since the review saw its revision "+strconv.FormatInt(seen, 10))
		}
		d.CommitmentID, d.Revision, d.At = x.CommitmentID, x.AppliedRevision, x.AppliedAt
	case DispositionCommitmentCancelled:
		if f.Kind == ItemDraft {
			return nil, mismatch
		}
		if _, about := commitments[s.CommitmentID]; !about {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, itemPath(i, "/commitment_id"), "The item is not about commitment "+s.CommitmentID)
		}
		cm, ok := now.Commitments[s.CommitmentID]
		if !ok || cm.Status != commitmentCancelled {
			return nil, kernel.Reject(kernel.CodeInvalidState, itemPath(i, "/commitment_id"), "Commitment "+s.CommitmentID+" is not cancelled")
		}
		d.Revision, d.At = cm.Revision, cm.CancelledAt
	}
	if f.Kind == ItemAllocation {
		l := f.Lines[0]
		target, err := kernel.ParseDecimal(l.TargetQuantity, kernel.QuantityFormat)
		if err != nil {
			return nil, fmt.Errorf("pur: review item %d target %q: %w", it.No, l.TargetQuantity, err)
		}
		if allocated := now.Allocated[l.OrderLineID]; allocated.Cmp(target) > 0 {
			return nil, kernel.Reject(kernel.CodeInvalidState, itemPath(i, "/disposition"), "Order line "+l.OrderLineID+" still allocates "+
				exact(allocated)+" above the "+l.TargetQuantity+" the order needs; state no_action with the reason")
		}
	}
	return d, nil
}

// Verify checks a stored review for what the database also holds: items numbered from 1, a
// disposition on every item exactly when the review is settled.
func (r *Review) Verify() error {
	for i, it := range r.Items {
		if it.No != i+1 || (it.Done != nil) != (r.Status == ReviewSettled) {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/review_id", "The review record is not intact")
		}
	}
	return nil
}
