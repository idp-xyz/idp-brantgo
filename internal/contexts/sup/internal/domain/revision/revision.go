// Package revision is the root-and-revision aggregate of SUP's supply conditions: SupplierOffer,
// PurchaseTerms and LogisticsOffer (tactical model SUP-AGG-SupplierOffer, -PurchaseTerms,
// -LogisticsOffer; T02 §13.2; SM-SUP-OFFER). A root has a root version every command compares and
// increments, and revisions with monotonic numbers:
//
//	draft      → active      verify (purchase terms: publish): a human who never edited the draft
//	active     → superseded  verifying the next revision, in the same transaction
//	active     → withdrawn   withdraw (purchase terms: retired by retire); the root has no active revision
//	superseded → withdrawn   withdraw of the exact superseded revision; nothing in force changes
//	draft      → discarded   discard_draft
//
// One draft revision is saved whole. Sealing it fixes its content and evidence for good: a
// correction is a new revision copied from a sealed one (revise), never an edit (SUP-INV-001); at
// sealing, the revision it supersedes must be the current active one, so two verifications of the
// same root never both win. Purchase terms are published by a human other than every editor of the
// draft (SUP-INV-003); SUP applies the same independence to offers, as no verification policy says
// otherwise. A root has at most one draft and one active revision.
//
// The domain keeps a revision's content as the canonical JSON the application encodes, with its
// hash and the header fields the guard and the lookups need.
package revision

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of SUP's error catalog rows the aggregate decides; package api declares the same codes.
const (
	CodeIndependentReviewRequired = "INDEPENDENT_REVIEW_REQUIRED"
	CodeSupplyConditionNotCurrent = "SUPPLY_CONDITION_NOT_CURRENT"
	CodeSupplyConditionExpired    = "SUPPLY_CONDITION_EXPIRED"
)

// Categories are the categories of the codes above.
var Categories = map[string]kernel.Category{
	CodeIndependentReviewRequired: kernel.CategoryInvalid,
	CodeSupplyConditionNotCurrent: kernel.CategoryConflict,
	CodeSupplyConditionExpired:    kernel.CategoryInvalid,
}

// Reject returns a rejection with one of the codes above at path.
func Reject(code, path string) error { return kernel.RejectAs(code, Categories[code], path, "") }

// Kind of supply condition.
type Kind string

const (
	SupplierOffer  Kind = "supplier_offer"
	PurchaseTerms  Kind = "purchase_terms"
	LogisticsOffer Kind = "logistics_offer"
)

// Status of a revision.
type Status string

const (
	Draft      Status = "draft"
	Active     Status = "active"
	Superseded Status = "superseded"
	Withdrawn  Status = "withdrawn"
	Retired    Status = "retired"
	Discarded  Status = "discarded"
)

// EndStatus is the status ending a sealed revision of kind takes: retired for purchase terms,
// withdrawn otherwise.
func (k Kind) EndStatus() Status {
	if k == PurchaseTerms {
		return Retired
	}
	return Withdrawn
}

// Evidence is a source a document or an act cites, and where.
type Evidence struct {
	SourceID string
	Locator  string
}

// Act is a recorded act on a revision: its verification, or its withdrawal, retirement or discard.
type Act struct {
	By       kernel.ActorRef
	Epoch    int64 // the authority epoch the act saw
	At       time.Time
	Reason   string
	Evidence []Evidence
}

// Header is what the guard and the lookups read of a revision's content without decoding it: its
// validity and, for purchase terms, the validity mode and the source offer revision. Zero values
// are unknown in a draft.
type Header struct {
	ValidFrom      time.Time
	ValidUntil     time.Time
	ValidityMode   string // purchase terms: independent_validity or dependent_validity
	SourceID       string // purchase terms: the source offer
	SourceRevision int64
}

// Dependent reports whether the revision is in force only while its source offer revision is.
func (h Header) Dependent() bool { return h.ValidityMode == "dependent_validity" }

// Content is a revision's canonical content, its SHA-256 and its header.
type Content struct {
	Data   []byte
	Hash   string
	Header Header
}

// Revision is one revision of a supply condition. SavedBy is its last editor; Contributors are all
// its editors, sorted.
type Revision struct {
	Number       int64
	Status       Status
	Base         int64 // the sealed revision a revise copied; 0 for a first draft
	Content      Content
	SavedBy      kernel.ActorRef
	SavedEpoch   int64
	SavedAt      time.Time
	Contributors []string
	Verification *Act // nil until sealed
	SupersededAt time.Time
	End          *Act // the withdrawal, retirement or discard
}

// Sealed reports whether the revision was verified or published.
func (r *Revision) Sealed() bool { return r != nil && r.Verification != nil }

// Root is the aggregate root. Version is the root version; LastRevision the highest revision
// number ever assigned; EverSealed whether any revision was ever sealed. Working and Active are the
// revisions in those states; Named is the revision a command names when it is neither (the base of
// a revise, the target of a withdrawal), loaded by the caller.
type Root struct {
	Kind         Kind
	ID           string
	Version      int64
	LastRevision int64
	OwnerActor   string
	OwnerTeam    string
	EverSealed   bool
	Working      *Revision
	Active       *Revision
	Named        *Revision
}

// New returns a supply condition that is not stored yet, owned by the actor and team that create it.
func New(kind Kind, id, ownerActor, ownerTeam string) *Root {
	return &Root{Kind: kind, ID: id, OwnerActor: ownerActor, OwnerTeam: ownerTeam}
}

// Revision returns the revision number of r, whichever of Working, Active and Named it is, or nil.
func (r *Root) Revision(number int64) *Revision {
	for _, rev := range []*Revision{r.Working, r.Active, r.Named} {
		if rev != nil && rev.Number == number {
			return rev
		}
	}
	return nil
}

// CheckSave admits a whole-draft save: a new root only at expected version 0, an existing one only
// when visible, at its current root version, and with a draft or never sealed (state before
// references, as Python orders them).
func CheckSave(r *Root, visible bool, expected int64) error {
	if r == nil {
		if expected != 0 {
			return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
		}
		return nil
	}
	if err := Check(r, visible, expected); err != nil {
		return err
	}
	if r.Working == nil && (r.EverSealed || r.Active != nil) {
		return kernel.Reject(kernel.CodeInvalidState, "/args/id", "A verified condition is corrected through revise")
	}
	return nil
}

// Check admits a command on an existing, visible root at its current root version.
func Check(r *Root, visible bool, expected int64) error {
	switch {
	case r == nil || !visible:
		return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case r.Version != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Save replaces the draft revision whole, or starts one with the next revision number when the root
// was never sealed (T02 §13.2: a sealed root takes a new draft only through Revise). The editor
// joins the draft's contributors. It reports whether the revision is new.
func (r *Root) Save(c Content, by kernel.ActorRef, epoch int64, at time.Time) (*Revision, bool, error) {
	created := r.Working == nil
	if created {
		if r.EverSealed || r.Active != nil {
			return nil, false, kernel.Reject(kernel.CodeInvalidState, "/args/id", "A verified condition is corrected through revise")
		}
		r.LastRevision++
		r.Working = &Revision{Number: r.LastRevision, Status: Draft}
	}
	w := r.Working
	w.Content, w.SavedBy, w.SavedEpoch, w.SavedAt = c, by, epoch, at
	w.Contributors = contribute(w.Contributors, by.ID)
	r.Version++
	return w, created, nil
}

// Revise starts a new draft revision from the content of the sealed revision base; the root has no
// draft. The active revision stays as it is.
func (r *Root) Revise(base *Revision, by kernel.ActorRef, epoch int64, at time.Time) (*Revision, error) {
	if r.Working != nil || !base.Sealed() {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/base_revision", "")
	}
	r.LastRevision++
	w := &Revision{Number: r.LastRevision, Status: Draft, Base: base.Number, Content: base.Content, SavedBy: by, SavedEpoch: epoch,
		SavedAt: at, Contributors: []string{by.ID}}
	r.Working = w
	r.Version++
	return w, nil
}

// Discard ends the draft revision number for a recorded reason; its number is not reused.
func (r *Root) Discard(number int64, by kernel.ActorRef, epoch int64, reason string, at time.Time) (*Revision, error) {
	w := r.Working
	if w == nil || w.Number != number {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/working_revision", "")
	}
	w.Status, w.End = Discarded, &Act{By: by, Epoch: epoch, At: at, Reason: reason}
	r.Working = nil
	r.Version++
	return w, nil
}

// CheckSeal admits sealing the draft revision number by the human by, in this order: the draft is
// the one named (INVALID_STATE), supersedes names the active revision, 0 when there is none
// (VERSION_CONFLICT, SUP-INV-001), and by never edited the draft (INDEPENDENT_REVIEW_REQUIRED,
// SUP-INV-003). The content is judged by the caller after this.
func (r *Root) CheckSeal(number, supersedes int64, by kernel.ActorRef) error {
	w := r.Working
	if w == nil || w.Number != number {
		return kernel.Reject(kernel.CodeInvalidState, "/args/working_revision", "")
	}
	active := int64(0)
	if r.Active != nil {
		active = r.Active.Number
	}
	if supersedes != active {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/supersedes_revision", "The revision to supersede is not the active one")
	}
	if slices.Contains(w.Contributors, by.ID) || w.SavedBy.ID == by.ID {
		return Reject(CodeIndependentReviewRequired, "/args/working_revision")
	}
	return nil
}

// Seal makes the draft revision the active one with its verification act, after CheckSeal, and
// supersedes the active revision, which it returns (nil when there was none).
func (r *Root) Seal(by kernel.ActorRef, epoch int64, evidence []Evidence, at time.Time) *Revision {
	superseded := r.Active
	if superseded != nil {
		superseded.Status, superseded.SupersededAt = Superseded, at
	}
	w := r.Working
	w.Status = Active
	w.Verification = &Act{By: by, Epoch: epoch, At: at, Evidence: slices.Clone(evidence)}
	r.Active, r.Working, r.EverSealed = w, nil, true
	r.Version++
	return superseded
}

// End withdraws (purchase terms: retires) the sealed revision target, which must be active or
// superseded: an unverified draft is never withdrawn. Ending the active revision leaves the root
// without one; no older revision comes back.
func (r *Root) End(target *Revision, by kernel.ActorRef, epoch int64, reason string, evidence []Evidence, at time.Time) error {
	if target == nil || (target.Status != Active && target.Status != Superseded) {
		return kernel.Reject(kernel.CodeInvalidState, "/args/target_revision", "")
	}
	if r.Active == target {
		r.Active = nil
	}
	target.Status = r.Kind.EndStatus()
	target.End = &Act{By: by, Epoch: epoch, At: at, Reason: reason, Evidence: slices.Clone(evidence)}
	r.Version++
	return nil
}

// InForce is the verdict of SupplyConditionGuard v1 on a revision at instant at, from its current
// state: "" when it is active, was verified at or before at and its validity contains at;
// SUPPLY_CONDITION_NOT_CURRENT when it is not active (a nil revision never is) or was verified
// later; SUPPLY_CONDITION_EXPIRED when its validity does not contain at. The guard function in
// migrations/sup/000002_sup_supply_condition_guard.sql decides the same.
func InForce(rev *Revision, at time.Time) string {
	if rev == nil || rev.Status != Active || rev.Verification == nil || rev.Verification.At.After(at) {
		return CodeSupplyConditionNotCurrent
	}
	h := rev.Content.Header
	if at.Before(h.ValidFrom) || !at.Before(h.ValidUntil) {
		return CodeSupplyConditionExpired
	}
	return ""
}

// InForceWith is InForce for a revision that may depend on a source offer revision: dependent
// purchase terms are in force only while source is (T02 §13.8, A46).
func InForceWith(rev, source *Revision, at time.Time) string {
	if code := InForce(rev, at); code != "" || !rev.Content.Header.Dependent() {
		return code
	}
	return InForce(source, at)
}

// contribute adds id to the sorted contributors once.
func contribute(list []string, id string) []string {
	if slices.Contains(list, id) {
		return list
	}
	out := append(slices.Clone(list), id)
	slices.Sort(out)
	return out
}
