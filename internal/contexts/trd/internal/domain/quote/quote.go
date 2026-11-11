package quote

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Status of a revision (SM-TRD-REVISION). TRD writes working, frozen, discarded and
// pending_approval; the other states come with activation, replacement, withdrawal and
// cancellation, and the storage already admits one active revision per root (TRD-INV-001).
type Status string

const (
	Working         Status = "working"
	Frozen          Status = "frozen"
	Discarded       Status = "discarded"
	PendingApproval Status = "pending_approval"
	Active          Status = "active"
	Superseded      Status = "superseded"
	Withdrawn       Status = "withdrawn"
	Cancelled       Status = "cancelled"
)

// Sealed reports whether a revision with status s was frozen: its commercial content is fixed.
func (s Status) Sealed() bool {
	return s != Working && s != Discarded
}

// Revision is one business revision of a quote.
type Revision struct {
	Number       int64
	Status       Status
	Base         int64 // the revision a revise copied, 0 for none
	Data         Data  // the draft as saved; a frozen revision keeps it as it was frozen
	Hash         string
	Contributors []string // actor IDs of everyone who saved or revised it
	SavedBy      kernel.ActorRef
	SavedEpoch   int64
	SavedAt      time.Time
	Seal         *Seal // frozen
	End          *End  // discarded
	Fresh        bool  // not stored yet
	Changed      bool  // stored, changed by this command
	StatusOnly   bool  // stored and sealed: only its status changed
}

// End is how a working revision was discarded.
type End struct {
	By       kernel.ActorRef
	Epoch    int64
	At       time.Time
	Reason   string
	Evidence []Evidence
}

// Quote is the QuoteRoot aggregate.
type Quote struct {
	ID              string
	RootVersion     int64
	LastRevision    int64
	OwnerActor      string
	OwnerTeam       string
	Labels          []string // internal; never part of the business hash
	InternalNote    string   // internal; never part of the business hash
	Control         Control
	Revisions       []*Revision
	Reviews         []*Review   // in the order they were requested
	Approvals       []*Approval // in the order they were given
	MetadataChanged bool
}

// New returns a quote root owned by owner and team, before its first save.
func New(id, owner, team string) *Quote {
	return &Quote{ID: id, OwnerActor: owner, OwnerTeam: team, Control: Control{State: Unbound}}
}

// Working returns the working revision, or nil.
func (q *Quote) Working() *Revision {
	for _, r := range q.Revisions {
		if r.Status == Working {
			return r
		}
	}
	return nil
}

// Active returns the active revision, or nil.
func (q *Quote) Active() *Revision {
	for _, r := range q.Revisions {
		if r.Status == Active {
			return r
		}
	}
	return nil
}

// Revision returns revision n, or nil.
func (q *Quote) Revision(n int64) *Revision {
	for _, r := range q.Revisions {
		if r.Number == n {
			return r
		}
	}
	return nil
}

// FrozenAt returns the revision a freeze committed at root version v, or nil (receipts of replayed
// freezes).
func (q *Quote) FrozenAt(v int64) *Revision {
	for _, r := range q.Revisions {
		if r.Seal != nil && r.Seal.RootVersion == v {
			return r
		}
	}
	return nil
}

// CheckSave admits quote.save_draft on q (nil: no root yet) at expected root version, before the
// draft is judged: a new root at version 0, otherwise the current root version (VERSION_CONFLICT)
// and a working revision to replace (INVALID_STATE: revise a frozen revision first, Python
// save_draft). A root the caller does not see is NOT_FOUND.
func CheckSave(q *Quote, visible bool, expected int64) error {
	if q == nil {
		if expected != 0 {
			return kernel.Reject(kernel.CodeNotFound, "/args/quote_id", "")
		}
		return nil
	}
	if !visible {
		return kernel.Reject(kernel.CodeNotFound, "/args/quote_id", "")
	}
	if q.RootVersion != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if q.Working() == nil {
		return kernel.Reject(kernel.CodeInvalidState, "/args/quote_id", "No working revision: revise a frozen revision first")
	}
	return nil
}

// Check admits a command on an existing root the caller sees at expected root version.
func Check(q *Quote, visible bool, expected int64) error {
	if q == nil || !visible {
		return kernel.Reject(kernel.CodeNotFound, "/args/quote_id", "")
	}
	if q.RootVersion != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Save replaces the working revision whole with d by by, starting revision 1 of a new root
// (Python save_draft). The saver joins the revision's contributors.
func (q *Quote) Save(d Data, by kernel.ActorRef, epoch int64, now time.Time) *Revision {
	w := q.Working()
	if w == nil {
		q.LastRevision++
		w = &Revision{Number: q.LastRevision, Status: Working, Fresh: true}
		q.Revisions = append(q.Revisions, w)
	}
	w.Data, w.Hash = d, d.Hash()
	if !slices.Contains(w.Contributors, by.ID) {
		w.Contributors = append(w.Contributors, by.ID)
	}
	w.SavedBy, w.SavedEpoch, w.SavedAt, w.Changed = by, epoch, kernel.Instant(now), true
	q.RootVersion++
	return w
}

// Revise copies frozen revision base into a new working revision while none exists (Python
// revise): the content without anything the freeze derived, and the base's contributors with the
// reviser. The frozen revision stays as it is.
func (q *Quote) Revise(base int64, by kernel.ActorRef, epoch int64, now time.Time) (*Revision, error) {
	if q.Working() != nil {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/quote_id", "A working revision exists")
	}
	b := q.Revision(base)
	if b == nil || !b.Status.Sealed() {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/base_revision", "Only a frozen revision is revised")
	}
	q.LastRevision++
	contributors := slices.Clone(b.Contributors)
	if !slices.Contains(contributors, by.ID) {
		contributors = append(contributors, by.ID)
	}
	w := &Revision{Number: q.LastRevision, Status: Working, Base: base, Data: b.Data, Hash: b.Data.Hash(), Contributors: contributors,
		SavedBy: by, SavedEpoch: epoch, SavedAt: kernel.Instant(now), Fresh: true}
	q.Revisions = append(q.Revisions, w)
	q.RootVersion++
	return w, nil
}

// Discard ends working revision working with a reason and evidence (Python discard_draft). The
// revision is kept; its number is never reused.
func (q *Quote) Discard(working int64, by kernel.ActorRef, epoch int64, now time.Time, reason string, evidence []Evidence) (*Revision, error) {
	w := q.Working()
	if w == nil || w.Number != working {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/working_revision", "")
	}
	w.Status, w.Changed = Discarded, true
	w.End = &End{By: by, Epoch: epoch, At: kernel.Instant(now), Reason: reason, Evidence: slices.Clone(evidence)}
	q.RootVersion++
	return w, nil
}

// CheckFreeze admits the freeze of working revision working before any upstream fact is judged.
func (q *Quote) CheckFreeze(working int64) error {
	w := q.Working()
	if w == nil || w.Number != working {
		return kernel.Reject(kernel.CodeInvalidState, "/args/working_revision", "")
	}
	return nil
}

// Freeze seals working revision working with the seal SealDraft computed: the revision becomes
// frozen and its commercial document and business hash never change (TRD-INV-002).
func (q *Quote) Freeze(working int64, s *Seal, by kernel.ActorRef, epoch int64, now time.Time) (*Revision, error) {
	if err := q.CheckFreeze(working); err != nil {
		return nil, err
	}
	w := q.Working()
	q.RootVersion++
	s.By, s.Epoch, s.At, s.RootVersion = by, epoch, kernel.Instant(now), q.RootVersion
	w.Status, w.Seal, w.Changed = Frozen, s, true
	return w, nil
}

// UpdateMetadata replaces the internal labels and note (BC-AT09): the root version moves, no
// revision and no business hash changes (TRD-INV-002).
func (q *Quote) UpdateMetadata(labels []string, note string) {
	q.Labels, q.InternalNote, q.MetadataChanged = slices.Clone(labels), note, true
	if q.Labels == nil {
		q.Labels = []string{}
	}
	q.RootVersion++
}
