// Package fulfilment holds the rules of the shipment review after an order change, an entity of
// FUL's OrderFulfilment root (tactical model FUL-AGG-OrderFulfilment entity ShipmentReview,
// SM-FUL-SHIPMENT-REVIEW; WF-06 step 5 with its idempotency and failure mode; event storming PL-09;
// S5 decisions D-18, D-21; BT10; WBS S5-09b, FUL part). The package knows no database: the
// application gives it what TRD's event said and what FUL holds of the order.
//
// TRD's OrderAmended, OrderCancelled and OrderRemainderClosed say what happened to an order (Change);
// Assess compares it with FUL's facts of the order (Holdings) and lists as items what the change
// leaves stale: a prepared shipment that can no longer be confirmed — bound to an earlier order
// revision than the amended one, or of an order that ended —, its current packing plan and actual
// packing, its current booking, the draft versions of its document set, and every granted commercial
// release such a shipment, or a confirmed shipment bound to an earlier revision, can no longer
// consume. Confirmed shipments stay facts. One review is kept per (order, order revision, cause); a
// review opened for a later order version supersedes the open ones of earlier versions. The
// merchandiser settles a review by stating, for every item, what was done with FUL's commands — the
// shipment cancelled, replaced by a shipment of the new revision that holds the item's counterpart,
// the release evaluated again — or no_action for a release of a confirmed shipment, each with a
// reason; the review does none of it itself.
//
//	open ──shipment_review.settle (human)──▶ settled
//	  └───shipment_review.open (system, a later order version)──▶ superseded
package fulfilment

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// States, causes, kinds, impacts and dispositions (api constants).
const (
	ReviewOpen       = "open"
	ReviewSettled    = "settled"
	ReviewSuperseded = "superseded"

	CauseOrderAmended         = "order_amended"
	CauseOrderCancelled       = "order_cancelled"
	CauseOrderRemainderClosed = "order_remainder_closed"

	ItemShipment = "shipment"
	ItemPacking  = "packing"
	ItemBooking  = "booking"
	ItemDocument = "document"
	ItemRelease  = "release"

	ImpactRevisionChanged = "revision_changed"
	ImpactOrderCancelled  = "order_cancelled"
	ImpactRemainderClosed = "remainder_closed"

	LineUnchanged            = "unchanged"
	LineReduced              = "reduced"
	LineRemoved              = "line_removed"
	LineSpecificationChanged = "specification_changed"

	DispositionShipmentCancelled = "shipment_cancelled"
	DispositionReplaced          = "replaced"
	DispositionReevaluated       = "reevaluated"
	DispositionNoAction          = "no_action"

	// ResponsibleRole is the role template that settles reviews (role templates: 跟单 holds
	// shipment_review.settle).
	ResponsibleRole = "merchandiser"

	// States of the shipments and releases a review reads.
	ShipmentPrepared  = "prepared"
	ShipmentConfirmed = "confirmed"
	ShipmentCancelled = "cancelled"
	ReleaseGranted    = "granted"
	ScopeShipment     = "shipment"
	ScopeDocuments    = "documents"
	PackingPlan       = "plan"
	PackingActual     = "actual"

	codeReferenceStale = "REFERENCE_STALE"
)

// Act is who did something, under which authority epoch, when, and why.
type Act struct {
	By     kernel.ActorRef
	Epoch  int64
	At     time.Time
	Reason string
}

// Target is a line of the amended order revision: its quantity, SKU specification revision and unit.
type Target struct {
	LineID       string
	Quantity     kernel.Decimal
	SkuID        string
	SpecRevision int64
	UnitID       string
}

// Change is what a TRD event says happened to the order: the order at revision Revision and hash
// Hash, TRD's row version of the order after it (Version), the cause and event, and the amended
// revision's lines (none after an end).
type Change struct {
	OrderID  string
	Revision int64
	Hash     string
	Version  int64
	Cause    string
	EventID  string
	Targets  map[string]Target
}

// HeldLine is a line of a shipment: what it ships of an order line, in which SKU specification and
// unit.
type HeldLine struct {
	LineID       string
	OrderLineID  string
	SkuID        string
	SpecRevision int64
	UnitID       string
	Quantity     kernel.Decimal
}

// HeldPacking is a shipment's current packing record of a kind (plan or actual): its sequence number,
// cartons and packages.
type HeldPacking struct {
	Kind     string
	Seq      int64
	Cartons  int64
	Packages int64
}

// HeldBooking is a shipment's current booking version.
type HeldBooking struct {
	Version int64
	Number  string
	Vessel  string
	Voyage  string
	ETD     string
}

// HeldDocument is a draft version of a shipment's document set.
type HeldDocument struct {
	Kind    string
	Version int64
	Number  string
}

// HeldRelease is a commercial release of a shipment: its scope, state, row version and the number of
// its evaluations.
type HeldRelease struct {
	ID          string
	Scope       string
	Status      string
	RowVersion  int64
	Evaluations int64
}

// HeldShipment is a shipment of the order that is not cancelled, with the order revision it binds and
// what hangs on it.
type HeldShipment struct {
	ID            string
	Status        string
	RowVersion    int64
	Revision      int64
	Lines         []HeldLine
	Packing       []HeldPacking
	Booking       *HeldBooking
	DocumentSetID string
	Drafts        []HeldDocument
	Releases      []HeldRelease
}

// LineUnit is an order line in one unit.
type LineUnit struct {
	OrderLineID string
	UnitID      string
}

// Holdings are FUL's facts of one order: whether it has a fulfilment root (a shipment was ever
// prepared), its shipments that are not cancelled, by shipment ID, and what its confirmed shipments
// shipped of each order line over every revision.
type Holdings struct {
	Rooted    bool
	Shipments []HeldShipment
	Shipped   map[LineUnit]kernel.Decimal
}

// FactLine is what a shipment item says of one of its lines (api.ReviewLineView).
type FactLine struct {
	LineID            string `json:"line_id"`
	OrderLineID       string `json:"order_line_id"`
	SkuID             string `json:"sku_id"`
	SpecRevision      int64  `json:"spec_revision"`
	UnitID            string `json:"unit_id"`
	Quantity          string `json:"quantity"`
	TargetQuantity    string `json:"target_quantity,omitempty"`
	ShippedQuantity   string `json:"shipped_quantity,omitempty"`
	RemainingQuantity string `json:"remaining_quantity,omitempty"`
	Impact            string `json:"impact"`
}

// Facts are what FUL knew of an item's object when the review opened; stored as JSON.
type Facts struct {
	Kind               string     `json:"kind"`
	ShipmentID         string     `json:"shipment_id"`
	ShipmentStatus     string     `json:"shipment_status"`
	ShipmentVersion    int64      `json:"shipment_version"`
	ShipmentRevision   int64      `json:"shipment_revision"`
	Impact             string     `json:"impact"`
	Lines              []FactLine `json:"lines,omitempty"`
	PackingKind        string     `json:"packing_kind,omitempty"`
	PackingSeq         int64      `json:"packing_seq,omitempty"`
	Cartons            int64      `json:"cartons,omitempty"`
	Packages           int64      `json:"packages,omitempty"`
	BookingVersion     int64      `json:"booking_version,omitempty"`
	BookingNumber      string     `json:"booking_number,omitempty"`
	Vessel             string     `json:"vessel,omitempty"`
	Voyage             string     `json:"voyage,omitempty"`
	ETD                string     `json:"etd,omitempty"`
	DocumentSetID      string     `json:"document_set_id,omitempty"`
	DocumentKind       string     `json:"document_kind,omitempty"`
	DocumentVersion    int64      `json:"document_version,omitempty"`
	DocumentNumber     string     `json:"document_number,omitempty"`
	ReleaseID          string     `json:"release_id,omitempty"`
	ReleaseScope       string     `json:"release_scope,omitempty"`
	ReleaseStatus      string     `json:"release_status,omitempty"`
	ReleaseVersion     int64      `json:"release_version,omitempty"`
	ReleaseEvaluations int64      `json:"release_evaluations,omitempty"`
}

// JSON is the item's facts as stored: the members in their declared order.
func (f Facts) JSON() string {
	b, err := json.Marshal(f)
	if err != nil {
		panic("ful: review item facts: " + err.Error()) // strings and integers always encode
	}
	return string(b)
}

// ParseFacts reads stored facts.
func ParseFacts(doc string) (Facts, error) {
	var f Facts
	err := json.Unmarshal([]byte(doc), &f)
	return f, err
}

// Disposition is how an item was settled: the disposition, the replacement shipment and the order
// revision it binds (replaced), the counterpart found (Ref) and its state (Status), when it happened
// (At: when the item's shipment was cancelled, or when the release was last evaluated) and the
// reason.
type Disposition struct {
	Kind       string
	ShipmentID string
	Revision   int64
	Ref        string
	Status     string
	At         time.Time
	Reason     string
}

// Item is one item of a review: its number, its facts, and its disposition once settled.
type Item struct {
	No    int
	Facts Facts
	Done  *Disposition
}

// Review is a shipment review: the order revision and change it follows, the TRD event and order row
// version that opened it, its state, row version (1 open, 2 settled or superseded), responsible role
// and items, who opened and settled it, and the review that superseded it.
type Review struct {
	ID           string
	OrderID      string
	Revision     int64
	Hash         string
	Version      int64
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
}

func exact(d kernel.Decimal) string {
	s, _ := d.Exact()
	return s
}

// Stale reports whether change c leaves shipment s, which is not cancelled, unable to be confirmed
// or released on its binding: after an amendment, a shipment bound to an earlier order revision
// (TRD's OrderGuard answers REFERENCE_STALE to it); after an end, every shipment (ORDER_NOT_ACTIVE).
func (c Change) Stale(s HeldShipment) bool {
	return c.Cause != CauseOrderAmended || s.Revision < c.Revision
}

func (c Change) impact() string {
	switch c.Cause {
	case CauseOrderCancelled:
		return ImpactOrderCancelled
	case CauseOrderRemainderClosed:
		return ImpactRemainderClosed
	}
	return ImpactRevisionChanged
}

// lines are what shipment s ships, set against the amended revision: whether the line is still
// there, in the same specification and unit, and whether what is left of it after the order's
// confirmed shipments still holds the shipment's quantity. After an end nothing is left.
func (c Change) lines(s HeldShipment, shipped map[LineUnit]kernel.Decimal) []FactLine {
	out := make([]FactLine, 0, len(s.Lines))
	for _, l := range s.Lines {
		f := FactLine{LineID: l.LineID, OrderLineID: l.OrderLineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision, UnitID: l.UnitID,
			Quantity: exact(l.Quantity)}
		t, ok := c.Targets[l.OrderLineID]
		switch {
		case c.Cause != CauseOrderAmended:
			f.ShippedQuantity, f.RemainingQuantity, f.Impact = exact(shipped[LineUnit{l.OrderLineID, l.UnitID}]), "0", c.impact()
		case !ok:
			f.ShippedQuantity, f.RemainingQuantity, f.Impact = exact(shipped[LineUnit{l.OrderLineID, l.UnitID}]), "0", LineRemoved
		default:
			done := shipped[LineUnit{l.OrderLineID, t.UnitID}]
			left := t.Quantity.Sub(done)
			if left.Sign() < 0 {
				left = kernel.DecimalFromInt(0)
			}
			f.TargetQuantity, f.ShippedQuantity, f.RemainingQuantity = exact(t.Quantity), exact(done), exact(left)
			switch {
			case t.SkuID != l.SkuID || t.SpecRevision != l.SpecRevision || t.UnitID != l.UnitID:
				f.Impact = LineSpecificationChanged
			case l.Quantity.Cmp(left) > 0:
				f.Impact = LineReduced
			default:
				f.Impact = LineUnchanged
			}
		}
		out = append(out, f)
	}
	return out
}

var packingOrder = map[string]int{PackingPlan: 0, PackingActual: 1}

// Assess lists the items change c leaves stale of what FUL holds of the order (h), numbered from 1:
// per stale shipment by shipment ID — a prepared one with its lines, then its current packing plan
// and actual packing, its current booking, the drafts of its document set by kind and version, and
// its granted releases (shipment scope first); a confirmed one with its granted releases only (its
// shipment release was consumed by the confirmation; the confirmed shipment stays).
func Assess(c Change, h Holdings) []Item {
	var items []Item
	add := func(f Facts) { items = append(items, Item{No: len(items) + 1, Facts: f}) }
	shipments := slices.Clone(h.Shipments)
	slices.SortFunc(shipments, func(a, b HeldShipment) int { return cmp.Compare(a.ID, b.ID) })
	for _, s := range shipments {
		if !c.Stale(s) || (s.Status != ShipmentPrepared && s.Status != ShipmentConfirmed) {
			continue
		}
		base := Facts{ShipmentID: s.ID, ShipmentStatus: s.Status, ShipmentVersion: s.RowVersion, ShipmentRevision: s.Revision, Impact: c.impact()}
		if s.Status == ShipmentPrepared {
			f := base
			f.Kind, f.Lines = ItemShipment, c.lines(s, h.Shipped)
			add(f)
			packing := slices.Clone(s.Packing)
			slices.SortFunc(packing, func(a, b HeldPacking) int { return cmp.Compare(packingOrder[a.Kind], packingOrder[b.Kind]) })
			for _, p := range packing {
				f := base
				f.Kind, f.PackingKind, f.PackingSeq, f.Cartons, f.Packages = ItemPacking, p.Kind, p.Seq, p.Cartons, p.Packages
				add(f)
			}
			if b := s.Booking; b != nil {
				f := base
				f.Kind, f.BookingVersion, f.BookingNumber, f.Vessel, f.Voyage, f.ETD = ItemBooking, b.Version, b.Number, b.Vessel, b.Voyage, b.ETD
				add(f)
			}
			drafts := slices.Clone(s.Drafts)
			slices.SortFunc(drafts, func(a, b HeldDocument) int {
				return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Version, b.Version))
			})
			for _, d := range drafts {
				f := base
				f.Kind, f.DocumentSetID, f.DocumentKind, f.DocumentVersion, f.DocumentNumber = ItemDocument, s.DocumentSetID, d.Kind, d.Version, d.Number
				add(f)
			}
		}
		releases := slices.Clone(s.Releases)
		slices.SortFunc(releases, func(a, b HeldRelease) int { return cmp.Compare(scopeOrder(a.Scope), scopeOrder(b.Scope)) })
		for _, r := range releases {
			if r.Status != ReleaseGranted {
				continue
			}
			f := base
			f.Kind, f.ReleaseID, f.ReleaseScope, f.ReleaseStatus, f.ReleaseVersion, f.ReleaseEvaluations = ItemRelease, r.ID, r.Scope, r.Status,
				r.RowVersion, r.Evaluations
			add(f)
		}
	}
	return items
}

func scopeOrder(scope string) int {
	if scope == ScopeShipment {
		return 0
	}
	return 1
}

// Effect is what an order change does to FUL's reviews of the order.
type Effect string

const (
	// EffectOpened: a review is opened now (and earlier open ones are superseded).
	EffectOpened Effect = "review_opened"
	// EffectAlreadyOpen: a review of this order revision and cause exists (opened before, settled or
	// superseded since); nothing is opened twice and nothing settled is reopened.
	EffectAlreadyOpen Effect = "review_exists"
	// EffectLater: a review of the order follows a later order version; the change is older news.
	EffectLater Effect = "later_review"
	// EffectNothingHeld: FUL never prepared a shipment of the order.
	EffectNothingHeld Effect = "nothing_held"
	// EffectNothingStale: the change leaves nothing of the order stale and no review of it is open.
	EffectNothingStale Effect = "nothing_stale"
)

// Plan decides what change c does, given the order's reviews and the items Assess listed: whether a
// review opens, and which open reviews it supersedes (WF-06: 各下游按（订单、订单修订）只开启一次复核；更晚修订的复核
// 取代未处置的旧复核). A review opens when something is stale, or when an open review of an earlier order
// version is to be superseded (then possibly without items).
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
	case !h.Rooted:
		return EffectNothingHeld, nil
	case len(items) == 0 && len(open) == 0:
		return EffectNothingStale, nil
	}
	return EffectOpened, open
}

// OpenReview returns the review change c opens with items, by the system actor act.
func OpenReview(id string, c Change, items []Item, act Act) *Review {
	act.At, act.Reason = kernel.Instant(act.At), ""
	return &Review{ID: id, OrderID: c.OrderID, Revision: c.Revision, Hash: c.Hash, Version: c.Version, Cause: c.Cause, EventID: c.EventID,
		Status: ReviewOpen, RowVersion: 1, Role: ResponsibleRole, Items: items, Opened: act}
}

// Supersede marks open review r superseded by review by at instant at.
func (r *Review) Supersede(by string, at time.Time) {
	r.Status, r.RowVersion, r.SupersededBy, r.SupersededAt = ReviewSuperseded, r.RowVersion+1, by, kernel.Instant(at)
}

// ReleaseNow is a commercial release as FUL shows it now.
type ReleaseNow struct {
	ID          string
	Status      string
	Evaluations int64
	EvaluatedAt time.Time
}

// ShipmentNow is a shipment as FUL shows it now: its order, state, bound order revision and
// cancellation, and what hangs on it — the latest packing record of each kind, the latest booking
// version (0: none), the latest version of each document kind in its set, its release of each scope.
type ShipmentNow struct {
	OrderID     string
	Status      string
	Revision    int64
	CancelledAt time.Time
	Packing     map[string]int64
	Booking     int64
	Documents   map[string]int64
	Releases    map[string]ReleaseNow
}

// Now is what FUL shows now of the shipments a settlement names: the items' shipments and the
// replacements.
type Now struct {
	Shipments map[string]ShipmentNow
}

// Settlement is one item's stated disposition.
type Settlement struct {
	ItemNo     int
	Kind       string
	ShipmentID string
	Reason     string
}

func itemPath(i int, member string) string { return "/args/items/" + strconv.Itoa(i) + member }

// Settle settles review r at expected with one disposition per item (ss), by human act, on what FUL
// shows now (SM-FUL-SHIPMENT-REVIEW open → settled: "装箱、订舱与未签发单证逐项处置；已确认出运保留"). It refuses,
// in this order: another version (VERSION_CONFLICT), a superseded review (REFERENCE_STALE: the later
// order version's review is the one to settle), a settled one (INVALID_STATE), an item the review
// does not have (REFERENCE_INVALID), an item without a disposition (INVALID_KNOWN_VALUE at
// /args/items), then per stated item a disposition that does not fit it (INVALID_KNOWN_VALUE), a
// replacement that is not a shipment of the order (REFERENCE_INVALID), and a disposition that has not
// happened — the shipment is not cancelled, the replacement binds an earlier revision, is cancelled
// or lacks the item's counterpart, the release was not evaluated again (INVALID_STATE).
func (r *Review) Settle(expected int64, ss []Settlement, now Now, act Act) error {
	switch {
	case r.RowVersion != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case r.Status == ReviewSuperseded:
		return kernel.RejectAs(codeReferenceStale, kernel.CategoryConflict, "/args/review_id",
			"A review of a later order version superseded this one: "+r.SupersededBy)
	case r.Status != ReviewOpen:
		return kernel.Reject(kernel.CodeInvalidState, "/args/review_id", "The review is "+r.Status)
	}
	for i, s := range ss {
		if s.ItemNo < 1 || s.ItemNo > len(r.Items) {
			return kernel.Reject(kernel.CodeReferenceInvalid, itemPath(i, "/item_no"), "The review has no such item")
		}
	}
	for _, it := range r.Items {
		if !slices.ContainsFunc(ss, func(s Settlement) bool { return s.ItemNo == it.No }) {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/items", "Item "+strconv.Itoa(it.No)+" has no disposition")
		}
	}
	done := make([]*Disposition, len(r.Items))
	for i, s := range ss {
		d, err := r.judge(r.Items[s.ItemNo-1], i, s, now)
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

// judge checks settlement s (the i-th stated) of item it against what FUL shows now.
func (r *Review) judge(it Item, i int, s Settlement, now Now) (*Disposition, error) {
	f := it.Facts
	d := &Disposition{Kind: s.Kind, Reason: s.Reason}
	prepared := f.ShipmentStatus == ShipmentPrepared
	mismatch := func(why string) error {
		return kernel.Reject(kernel.CodeInvalidKnownValue, itemPath(i, "/disposition"), "A "+f.Kind+" item of a "+f.ShipmentStatus+
			" shipment is not settled by "+s.Kind+": "+why)
	}
	state := func(member, why string) error {
		return kernel.Reject(kernel.CodeInvalidState, itemPath(i, member), why)
	}
	switch s.Kind {
	case DispositionNoAction:
		if f.Kind != ItemRelease || prepared {
			return nil, mismatch("no_action settles only a release of a confirmed shipment, which stays as it is")
		}
	case DispositionReevaluated:
		if f.Kind != ItemRelease {
			return nil, mismatch("only a release is evaluated again")
		}
		rel, ok := now.Shipments[f.ShipmentID].Releases[f.ReleaseScope]
		if !ok || rel.ID != f.ReleaseID || rel.Evaluations <= f.ReleaseEvaluations {
			return nil, state("/disposition", "Release "+f.ReleaseID+" was not evaluated again since the review opened")
		}
		d.Ref, d.Status, d.At = rel.ID, rel.Status, rel.EvaluatedAt
	case DispositionShipmentCancelled:
		if !prepared {
			return nil, mismatch("a confirmed shipment stays")
		}
		sh := now.Shipments[f.ShipmentID]
		if sh.Status != ShipmentCancelled {
			return nil, state("/disposition", "Shipment "+f.ShipmentID+" is "+sh.Status+", not cancelled")
		}
		d.At = sh.CancelledAt
	case DispositionReplaced:
		if !prepared {
			return nil, mismatch("a confirmed shipment stays")
		}
		if r.Cause != CauseOrderAmended {
			return nil, mismatch("an ended order has no later revision to re-plan on")
		}
		if s.ShipmentID == f.ShipmentID {
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, itemPath(i, "/shipment_id"), "A shipment does not replace itself")
		}
		nw, ok := now.Shipments[s.ShipmentID]
		if !ok || nw.OrderID != r.OrderID {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, itemPath(i, "/shipment_id"), "Shipment "+s.ShipmentID+" is not a shipment of order "+
				r.OrderID)
		}
		switch {
		case nw.Status == ShipmentCancelled:
			return nil, state("/shipment_id", "Shipment "+s.ShipmentID+" is cancelled")
		case nw.Revision < r.Revision:
			return nil, state("/shipment_id", "Shipment "+s.ShipmentID+" binds order revision "+strconv.FormatInt(nw.Revision, 10)+
				"; the review follows revision "+strconv.FormatInt(r.Revision, 10))
		}
		old := now.Shipments[f.ShipmentID]
		if old.Status != ShipmentCancelled {
			return nil, state("/disposition", "Shipment "+f.ShipmentID+" is "+old.Status+", not cancelled")
		}
		d.ShipmentID, d.Revision, d.Status, d.At = s.ShipmentID, nw.Revision, nw.Status, old.CancelledAt
		lacks := func(what string) error { return state("/shipment_id", "Shipment "+s.ShipmentID+" has no "+what) }
		switch f.Kind {
		case ItemPacking:
			seq, ok := nw.Packing[f.PackingKind]
			if !ok {
				return nil, lacks(f.PackingKind + " packing")
			}
			d.Ref = f.PackingKind + "/" + strconv.FormatInt(seq, 10)
		case ItemBooking:
			if nw.Booking == 0 {
				return nil, lacks("booking")
			}
			d.Ref = "booking/" + strconv.FormatInt(nw.Booking, 10)
		case ItemDocument:
			v, ok := nw.Documents[f.DocumentKind]
			if !ok {
				return nil, lacks(f.DocumentKind)
			}
			d.Ref = f.DocumentKind + "/" + strconv.FormatInt(v, 10)
		case ItemRelease:
			rel, ok := nw.Releases[f.ReleaseScope]
			if !ok {
				return nil, lacks(f.ReleaseScope + " release")
			}
			d.Ref, d.Status = rel.ID, rel.Status
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
