package quote

import (
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Formalization and the end of a revision (WBS S3-04, S3-05; SM-TRD-REVISION after the review;
// TRD-INV-001, 009, 010, 016; Python t03_storage activate, withdraw, cancel and T03_reference.sql
// t03_document_binding, t03_commit_evidence, t03_risk):
//
//	pending_approval → active      quote.activate: the approval's target is quote.activate and the
//	                               root has no active revision (one winner per root, TRD-INV-001)
//	pending_approval → active      quote.replace: the approval's target is quote.replace of the
//	active → superseded            active revision, which is superseded in the same command, while
//	                               the conversion slot is free (TRD-INV-010)
//	frozen, pending_approval →     quote.withdraw: an open review of the revision is cancelled; its
//	withdrawn                      approvals can never release it again
//	active → cancelled             quote.cancel: the exact active revision, while the conversion
//	                               slot is free
//
// A formalization binds the revision's formal document — its number, file and template against the
// business hash (QuoteDocumentBinding, TRD-INV-009), the bytes kept with it — and the command's
// commit evidence (TRD-INV-016) to the revision, both once; the command writes them with the state
// change, its action result, audit entry and event in one transaction. Whether the approval releases
// the revision is Releasable's decision, made in the same transaction before Formalize.
//
// A risk hold (RiskHold; UL TRD-RiskHold) pauses the forward use of an active revision: revoking the
// approval its commit evidence names registers one (SM-TRD-APPROVAL: 生效后撤销登记风险暂停). The
// revision stays active and its history unchanged; nothing lifts the hold (no shortcut in this
// batch): a replacement supersedes the revision, or a cancellation ends it. The conversion slot
// (ConversionSlot, full_v1) is taken once per root by the order step for the active, unheld revision
// (TakeSlot) and kept for good; while it is taken, the active revision is neither replaced nor
// cancelled (ROOT_CONVERSION_LOCKED).

// The conversion slot a root has (Python t03_order conversion_slot).
const SlotFullV1 = "full_v1"

// Causes of a risk hold and the code a use of the held revision is refused with.
const (
	HoldApprovalRevoked = "approval_revoked"
)

// Entry is one named value of commit evidence.
type Entry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Document is a revision's formal document: its number (issued by POL for BusinessRef from
// Sequence), the business hash it binds, the template revision and content hash it was rendered
// with, the renderer, and the file — name, MIME type, SHA-256, size and bytes. Content is nil when the document was loaded
// without its bytes.
type Document struct {
	Number          string
	Sequence        Ref
	BusinessRef     string
	BusinessHash    string // the business hash the binding binds
	Template        Ref
	TemplateHash    string
	RendererVersion string
	FileName        string
	MimeType        string
	ContentSHA256   string
	SizeBytes       int
	Content         []byte
}

// Commit is the commit evidence of a formalization: the command and its idempotency key (which name
// its action result), the approval at its effectivity version, the revision a replacement
// superseded, the cost snapshot the guards found adoptable, the authority and control epochs and
// root version it committed at, who committed it and when, and every entry in a fixed order.
type Commit struct {
	Command            string
	IdempotencyKey     string
	ApprovalID         string
	EffectivityVersion int64
	Replaced           int64 // quote.replace: the revision it superseded
	CostSnapshotID     string
	AuthorityEpoch     int64
	ControlEpoch       int64
	RootVersion        int64
	By                 kernel.ActorRef
	At                 time.Time
	Entries            []Entry
}

// EntriesJSON returns the entries as stored: canonical JSON, the bytes their SHA-256 covers.
func (c Commit) EntriesJSON() ([]byte, error) {
	entries := c.Entries
	if entries == nil {
		entries = []Entry{}
	}
	tree, err := TreeOf(entries)
	if err != nil {
		return nil, err
	}
	return Canonical(tree)
}

// Formalization is what a revision's activation or replacement bound to it.
type Formalization struct {
	Document Document
	Commit   Commit
	Fresh    bool // not stored yet
}

// Closure is how a revision was withdrawn or cancelled: its new status, who closed it, why, on which
// evidence, and the root version the command committed at.
type Closure struct {
	Status      Status
	Closed      Act
	RootVersion int64
	Fresh       bool
}

// Hold is a risk hold on a formal revision: its cause (approval_revoked: ApprovalID), the code a use
// of the revision is refused with, who registered it, and the root version it was registered at.
type Hold struct {
	ID          string // cause ":" approval ID
	Revision    int64
	Cause       string
	ApprovalID  string
	Code        string
	Registered  Act
	RootVersion int64
	Fresh       bool
}

// Slot is the root's conversion slot: the active revision and business hash it was taken for, the
// order it converted into, who took it, and the root version it was taken at.
type Slot struct {
	Slot         string
	Revision     int64
	BusinessHash string
	OrderID      string
	Taken        Act
	RootVersion  int64
	Fresh        bool
}

// Hold returns the risk hold of revision n, or nil.
func (q *Quote) Hold(n int64) *Hold {
	for _, h := range q.Holds {
		if h.Revision == n {
			return h
		}
	}
	return nil
}

// FormalizedAt returns the revision a formalization committed at root version v, or nil (receipts
// of replayed activations and replacements).
func (q *Quote) FormalizedAt(v int64) *Revision {
	for _, r := range q.Revisions {
		if r.Formal != nil && r.Formal.Commit.RootVersion == v {
			return r
		}
	}
	return nil
}

// ClosedAt returns the revision a withdrawal or cancellation committed at root version v, or nil.
func (q *Quote) ClosedAt(v int64) *Revision {
	for _, r := range q.Revisions {
		if r.Closed != nil && r.Closed.RootVersion == v {
			return r
		}
	}
	return nil
}

// CheckConversion refuses a replacement of the active revision while the conversion slot is taken
// (ROOT_CONVERSION_LOCKED, TRD-INV-010): the root converted into an order, which keeps the revision
// it converted; and while a live acceptance accepts the active revision (ACCEPTANCE_ALREADY_EXISTS,
// CheckContract).
func (q *Quote) CheckConversion(target string) error {
	if target != TargetReplace {
		return nil
	}
	if q.Slot != nil {
		return fail(CodeRootConversionLocked, "/args/quote_id", "The quote was converted into an order; its active revision stays")
	}
	if active := q.Active(); active != nil {
		return q.CheckContract(active.Number, "/args/expected_active_revision")
	}
	return nil
}

// CheckContract refuses ending revision n — superseding, withdrawing or cancelling it — while a live
// (valid or disputed) acceptance accepts it (ACCEPTANCE_ALREADY_EXISTS at path): the customer's
// acceptance formed a contract on it, and the owner's decision of 2026-11-11 keeps the revision until
// a human invalidates the acceptance (acceptance.invalidate, with a reason and evidence).
func (q *Quote) CheckContract(n int64, path string) error {
	if ids := q.Contracts[n]; len(ids) > 0 {
		return fail(CodeAcceptanceAlreadyExists, path, "Acceptance "+ids[0]+" formed a contract on the revision; invalidate it first")
	}
	return nil
}

// Formalize is what quote.activate or quote.replace brings to the root once Releasable admitted
// the approval in the same transaction: the release, the command's idempotency key, the formal
// document rendered with its number, the document binding's evidence entries (document.Binding),
// what the upstream guards answered (Upstream, with the cost snapshot they found adoptable), and the
// root's control epoch.
type Formalize struct {
	Release        Release
	IdempotencyKey string
	Document       Document
	Binding        []Entry
	Upstream       []Entry
	CostSnapshotID string
	ControlEpoch   int64
}

// Formalize activates revision f.Release.Revision with approval a at now, by by under authority
// epoch epoch: its formal document and commit evidence are bound to it, it becomes active and, for
// a replacement, the active revision it names becomes superseded. The root version moves once. It
// returns the revision and the one superseded (nil for an activation). It refuses what Releasable
// would (a revision not pending, an approval of another revision, target or effectivity, a root not
// as the target expects it) and a revision already formalized (BUSINESS_KEY_CONFLICT).
func (q *Quote) Formalize(f Formalize, a *Approval, by kernel.ActorRef, epoch int64, now time.Time) (*Revision, *Revision, error) {
	rev := q.Revision(f.Release.Revision)
	switch {
	case rev == nil:
		return nil, nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/business_revision", "")
	case rev.Formal != nil:
		return nil, nil, fail(CodeBusinessKeyConflict, "/args/business_revision", "The revision has its formal document")
	case rev.Status != PendingApproval || rev.Seal == nil:
		return nil, nil, kernel.Reject(kernel.CodeInvalidState, "/args/business_revision", "The revision is not pending approval")
	case a == nil || a.Revision != rev.Number || a.Effectivity != Valid || a.Target != f.Release.Target ||
		a.BusinessHash != rev.Seal.BusinessHash || (a.Target == TargetReplace && a.ExpectedActive != f.Release.ExpectedActive):
		return nil, nil, fail(CodeApprovalReferenceMismatch, "/args/approval_id", "The approval does not release this revision for this command")
	}
	if err := q.CheckConversion(a.Target); err != nil {
		return nil, nil, err
	}
	if err := q.target(a.Target, a.ExpectedActive, "/args/approval_id"); err != nil {
		return nil, nil, err
	}
	d := f.Document
	if d.Number == "" || d.BusinessHash != rev.Seal.BusinessHash || len(d.Content) != d.SizeBytes || Hash(d.Content) != d.ContentSHA256 {
		return nil, nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/document", "The formal document is not its bound bytes")
	}
	at := kernel.Instant(now)
	q.RootVersion++
	var replaced *Revision
	if a.Target == TargetReplace {
		replaced = q.Active()
		replaced.Status, replaced.Changed, replaced.StatusOnly = Superseded, true, true
	}
	c := Commit{Command: a.Target, IdempotencyKey: f.IdempotencyKey, ApprovalID: a.ID, EffectivityVersion: a.EffectivityVersion,
		CostSnapshotID: f.CostSnapshotID, AuthorityEpoch: epoch, ControlEpoch: f.ControlEpoch, RootVersion: q.RootVersion, By: by, At: at}
	own := []Entry{{"command", c.Command}, {"idempotency_key", c.IdempotencyKey}, {"approval_id", a.ID},
		{"approval_effectivity_version", itoa(a.EffectivityVersion)}, {"review_request_id", a.ReviewID}}
	if replaced != nil {
		c.Replaced = replaced.Number
		own = append(own, Entry{"replaced_revision", itoa(replaced.Number)})
	}
	own = append(own, Entry{"authority_epoch", itoa(epoch)}, Entry{"control_epoch", itoa(f.ControlEpoch)},
		Entry{"root_version", itoa(q.RootVersion)}, Entry{"committed_by", string(by.Kind) + ":" + by.ID},
		Entry{"committed_at", kernel.FormatTimestamp(at)})
	c.Entries = append(append(slices.Clone(f.Binding), own...), f.Upstream...)
	rev.Formal = &Formalization{Document: d, Commit: c, Fresh: true}
	rev.Status, rev.Changed, rev.StatusOnly = Active, true, true
	return rev, replaced, nil
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Withdraw ends frozen or pending revision n before it takes effect (quote.withdraw; Python
// withdraw), by by at now for reason on evidence: an open review of it is cancelled with the same
// act, and its approvals can never release it again (Releasable needs a pending revision). The root
// version moves. A revision a live acceptance accepts is not withdrawn (CheckContract: such a
// revision is active, so this states the owner's rule for every end of a revision). It returns the
// revision and the review it cancelled, if any.
func (q *Quote) Withdraw(n int64, by kernel.ActorRef, epoch int64, now time.Time, reason string, evidence []Evidence) (*Revision, *Review, error) {
	rev := q.Revision(n)
	switch {
	case rev == nil:
		return nil, nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/business_revision", "")
	case rev.Status != Frozen && rev.Status != PendingApproval:
		return nil, nil, kernel.Reject(kernel.CodeInvalidState, "/args/business_revision", "Only a frozen or pending revision is withdrawn")
	}
	if err := q.CheckContract(n, "/args/business_revision"); err != nil {
		return nil, nil, err
	}
	act := Act{By: by, Epoch: epoch, At: kernel.Instant(now), Reason: reason, Evidence: slices.Clone(evidence)}
	var cancelled *Review
	for _, r := range q.Reviews {
		if r.Revision == n && r.Status == ReviewOpen {
			decided := act
			r.Status, r.Decided, r.Changed = ReviewCancelled, &decided, true
			cancelled = r
		}
	}
	q.RootVersion++
	rev.Status, rev.Changed, rev.StatusOnly = Withdrawn, true, true
	rev.Closed = &Closure{Status: Withdrawn, Closed: act, RootVersion: q.RootVersion, Fresh: true}
	return rev, cancelled, nil
}

// Cancel ends the active revision n (quote.cancel; Python cancel): QUOTE_NOT_CURRENT unless n is
// the active revision, ROOT_CONVERSION_LOCKED while the conversion slot is taken,
// ACCEPTANCE_ALREADY_EXISTS while a live acceptance accepts it (CheckContract). Its formal document
// and commit evidence stay. The root version moves.
func (q *Quote) Cancel(n int64, by kernel.ActorRef, epoch int64, now time.Time, reason string, evidence []Evidence) (*Revision, error) {
	active := q.Active()
	if active == nil || active.Number != n {
		return nil, fail(CodeQuoteNotCurrent, "/args/business_revision", "Only the active revision is cancelled")
	}
	if q.Slot != nil {
		return nil, fail(CodeRootConversionLocked, "/args/quote_id", "The quote was converted into an order")
	}
	if err := q.CheckContract(n, "/args/business_revision"); err != nil {
		return nil, err
	}
	q.RootVersion++
	active.Status, active.Changed, active.StatusOnly = Cancelled, true, true
	active.Closed = &Closure{Status: Cancelled, Closed: Act{By: by, Epoch: epoch, At: kernel.Instant(now), Reason: reason,
		Evidence: slices.Clone(evidence)}, RootVersion: q.RootVersion, Fresh: true}
	return active, nil
}

// hold registers a risk hold on the active revision when approval a, just revoked, is the one its
// commit evidence names; nil otherwise. The root version the revocation moved to is its own.
func (q *Quote) hold(a *Approval, by kernel.ActorRef, epoch int64, at time.Time) *Hold {
	active := q.Active()
	if active == nil || active.Formal == nil || active.Formal.Commit.ApprovalID != a.ID {
		return nil
	}
	id := HoldApprovalRevoked + ":" + a.ID
	for _, h := range q.Holds {
		if h.ID == id {
			return nil
		}
	}
	h := &Hold{ID: id, Revision: active.Number, Cause: HoldApprovalRevoked, ApprovalID: a.ID, Code: CodeApprovalRevoked,
		Registered: Act{By: by, Epoch: epoch, At: at}, RootVersion: q.RootVersion, Fresh: true}
	q.Holds = append(q.Holds, h)
	return h
}

// Usable decides whether formal revision n serves new business now (customer responses,
// acceptance, conversion into an order): REFERENCE_INVALID for a revision never formalized,
// BUSINESS_HASH_MISMATCH for another business hash (businessHash "" skips the comparison),
// QUOTE_NOT_CURRENT when it is not the active revision, and the hold's code (APPROVAL_REVOKED) while
// a risk hold pauses it. Whether the conversion slot is free is TakeSlot's.
func (q *Quote) Usable(n int64, businessHash, path string) error {
	rev := q.Revision(n)
	switch {
	case rev == nil || rev.Formal == nil || rev.Seal == nil:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "The revision has no formal document")
	case businessHash != "" && rev.Seal.BusinessHash != businessHash:
		return fail(CodeBusinessHashMismatch, path, "The revision has another business hash")
	case rev.Status != Active:
		return fail(CodeQuoteNotCurrent, path, "The revision is not the quote's active revision")
	}
	if h := q.Hold(n); h != nil {
		return fail(h.Code, path, "A risk hold pauses the revision: "+h.Cause)
	}
	return nil
}

// FormalApproval decides, when an order of revision n activates (TRD-INV-014: 正式化批准未撤销、审核人仍合格),
// whether the approval its commit evidence names still stands on the reviewers' standings now: the
// revision has commit evidence (REFERENCE_INVALID otherwise, Python PYT-0239), its approval is
// still valid (APPROVAL_REVOKED), the reviewer is still independent of the revision's authors
// (INDEPENDENT_REVIEW_REQUIRED) and an active human grant holding quote.review.decide and trd.read
// that sees the quote (APPROVAL_STALE, T03 A26), and so is a margin exception's approver with the
// capability. The approval's and the quote's expiry are not judged: the formalization judged them,
// and the contract the order rests on formed afterwards (ADR-BR-013, TRD-INV-017). It returns the
// approval.
func (q *Quote) FormalApproval(n int64, standings Standings, path string) (*Approval, error) {
	rev := q.Revision(n)
	if rev == nil || rev.Formal == nil || rev.Seal == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "The revision has no commit evidence")
	}
	a := q.Approval(rev.Formal.Commit.ApprovalID)
	switch {
	case a == nil || a.Revision != n || a.BusinessHash != rev.Seal.BusinessHash:
		return nil, fail(CodeApprovalRequired, path, "The commit evidence names no approval of the revision")
	case a.Effectivity != Valid:
		return nil, fail(CodeApprovalRevoked, path, "The revision's formalization approval was revoked")
	}
	review := q.Review(a.ReviewID)
	if review == nil {
		return nil, fail(CodeApprovalStale, path, "The approval's review is missing")
	}
	if slices.Contains(authors(rev, review), a.Approved.By.ID) {
		return nil, fail(CodeIndependentReviewRequired, path, "")
	}
	s, err := standings(a.Approved.By.ID)
	if err != nil {
		return nil, err
	}
	if a.Approved.By.Kind != kernel.ActorHuman || !s.qualified(PermissionReview, PermissionRead) {
		return nil, fail(CodeApprovalStale, path, "The quote's reviewer is no longer qualified")
	}
	if e := a.Exception; e != nil {
		if s, err = standings(e.Approved.By.ID); err != nil {
			return nil, err
		}
		if e.Approved.By.Kind != kernel.ActorHuman || !s.qualified(PermissionReview, PermissionRead, PermissionMarginException) {
			return nil, fail(CodeApprovalStale, path, "The margin exception's approver is no longer qualified")
		}
	}
	return a, nil
}

// TakeSlot takes the root's conversion slot for its active revision n at businessHash, for order
// orderID (the order step, TRD-INV-010): the revision must be usable (Usable), and a slot already
// taken is CONVERSION_SLOT_CONFLICT. The root version moves; the slot is kept for good.
func (q *Quote) TakeSlot(n int64, businessHash, orderID string, by kernel.ActorRef, epoch int64, now time.Time) (*Slot, error) {
	if err := q.Usable(n, businessHash, "/args/business_revision"); err != nil {
		return nil, err
	}
	if q.Slot != nil {
		return nil, fail(CodeConversionSlotConflict, "/args/quote_id", "The quote's full_v1 conversion slot is taken")
	}
	q.RootVersion++
	q.Slot = &Slot{Slot: SlotFullV1, Revision: n, BusinessHash: businessHash, OrderID: orderID, Taken: Act{By: by, Epoch: epoch,
		At: kernel.Instant(now)}, RootVersion: q.RootVersion, Fresh: true}
	return q.Slot, nil
}

// FreshFX checks, at a formalization's instant at, that the sales exchange rate a revision was
// calculated with is still usable (Python _current_sales_fx; T03 A32, PYT-0383): observed at or
// before at and at most maxAge, the pricing policy's maximum age, before it (FX_STALE at path).
func FreshFX(observed time.Time, maxAge time.Duration, at time.Time, path string) error {
	now, seen := kernel.Instant(at), kernel.Instant(observed)
	switch {
	case maxAge <= 0:
		return kernel.Reject(kernel.CodeInvalidKnownValue, path, "The pricing policy sets no maximum exchange rate age")
	case observed.IsZero() || seen.After(now):
		return fail(CodeFXStale, path, "The exchange rate was observed after the formalization")
	case now.Sub(seen) > maxAge:
		return fail(CodeFXStale, path, "The exchange rate is older than the pricing policy allows")
	}
	return nil
}
