// Package mapping is the ItemCodeMapping aggregate: the versioned mapping of one external item
// code, a customer's or a supplier's own code for an item, to an internal SKU at one specification
// revision (CAT-AGG-ItemCodeMapping; Python V0.50 foundation kind code_mapping). Codes are matched
// exactly, never by text similarity, and a code without a published mapping stays unmapped for
// manual verification: CAT never substitutes a SKU for it.
//
// A mapping is identified by its mapping ID; its key (party, role, external code) is fixed by its
// first save. Its content, the SkuRef it maps to, lives in revisions with monotonic numbers and a
// root version every command compares and increments:
//
//	draft  → active      item_code_mapping.publish, by a human
//	active → superseded  item_code_mapping.publish of the mapping's next revision, in the same transaction
//
// One draft revision is saved whole until it is published; published revisions never change. At
// most one revision is active per key across all mappings of a company: publishing a second
// mapping of a key another mapping has published is BUSINESS_KEY_CONFLICT. A revision maps to a
// SKU specification revision that is current when it is saved and when it is published
// (CAT-INV-001, decided by sku.CheckSpec). Checks run in the Python order: existence, root version,
// key, state, evidence, SKU reference, business key.
package mapping

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// CodeBusinessKeyConflict is the error catalog's BUSINESS_KEY_CONFLICT (409); package api declares
// the same code for callers.
const CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"

// Role is the role of the party whose code a mapping maps.
type Role string

const (
	Customer Role = "customer"
	Supplier Role = "supplier"
)

// Status of a revision.
type Status string

const (
	Draft      Status = "draft"
	Active     Status = "active"
	Superseded Status = "superseded"
)

// Key is what a mapping maps: the external item code of one party in one role. The party is PTY's;
// PTY is not upstream of CAT, so CAT stores PartyID as given and the consuming context, which has
// both upstreams, checks it against PTY.
type Key struct {
	PartyID      string
	PartyRole    Role
	ExternalCode string
}

// Target is the SkuRef v1 a revision maps its key to: a SKU and one confirmed specification
// revision of it, referenced separately (CAT-INV-001).
type Target struct {
	SkuID        string
	SpecRevision int64
}

// Publication is the human verification that made a revision active.
type Publication struct {
	By          kernel.ActorRef
	Epoch       int64 // the authority epoch the publication saw
	At          time.Time
	EvidenceIDs []string
}

// Revision is one revision of a mapping. SavedBy is its last editor.
type Revision struct {
	Number       int64
	Status       Status
	Target       Target
	SavedBy      kernel.ActorRef
	SavedEpoch   int64
	SavedAt      time.Time
	Publication  *Publication // nil until published
	SupersededAt time.Time    // zero until superseded
}

// Root is the aggregate root. Version is the root version every command compares and increments;
// LastRevision the highest revision number ever assigned; Draft and Active the revisions in those
// states.
type Root struct {
	ID           string
	Key          Key
	Version      int64
	LastRevision int64
	Draft        *Revision
	Active       *Revision
}

// New returns a mapping of key that is not stored yet.
func New(id string, key Key) *Root { return &Root{ID: id, Key: key} }

// CheckSave admits a whole-revision save: a new mapping only at expected version 0, an existing
// one only at its current root version and for its own key.
func CheckSave(r *Root, expected int64, key Key) error {
	if r == nil {
		if expected != 0 {
			return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
		}
		return nil
	}
	if err := Check(r, expected); err != nil {
		return err
	}
	switch {
	case key.PartyID != r.Key.PartyID:
		return fixed("/args/party_id")
	case key.PartyRole != r.Key.PartyRole:
		return fixed("/args/party_role")
	case key.ExternalCode != r.Key.ExternalCode:
		return fixed("/args/external_code")
	}
	return nil
}

func fixed(path string) error {
	return kernel.Reject(kernel.CodeInvalidKnownValue, path, "A mapping keeps the item code of its first save")
}

// Check admits a command on an existing mapping at its current root version.
func Check(r *Root, expected int64) error {
	switch {
	case r == nil:
		return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case r.Version != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Save replaces the draft revision whole, or starts one with the next revision number, made by its
// last editor by. It reports whether the revision is new.
func (r *Root) Save(t Target, by kernel.ActorRef, epoch int64, at time.Time) (*Revision, bool) {
	created := r.Draft == nil
	if created {
		r.LastRevision++
		r.Draft = &Revision{Number: r.LastRevision, Status: Draft}
	}
	d := r.Draft
	d.Target, d.SavedBy, d.SavedEpoch, d.SavedAt = t, by, epoch, at
	r.Version++
	return d, created
}

// CheckPublish admits publishing revision number: NOT_FOUND for a number never assigned,
// INVALID_STATE unless it is the draft revision.
func (r *Root) CheckPublish(number int64) error {
	switch {
	case number > r.LastRevision:
		return kernel.Reject(kernel.CodeNotFound, "/args/revision", "")
	case r.Draft == nil || r.Draft.Number != number:
		return kernel.Reject(kernel.CodeInvalidState, "/args/revision", "")
	}
	return nil
}

// CheckKey refuses publishing while another mapping of the company has an active revision for the
// same key (Python foundation.publish, kind code_mapping).
func CheckKey(taken bool) error {
	if taken {
		return kernel.RejectAs(CodeBusinessKeyConflict, kernel.CategoryConflict, "/args/id", "Another mapping is published for this item code")
	}
	return nil
}

// Publish makes the draft revision active for the human by, citing the evidence verified, and
// supersedes the active revision; it returns the superseded revision, if any. Callers check
// CheckPublish, the evidence, the SKU reference and CheckKey first.
func (r *Root) Publish(by kernel.ActorRef, epoch int64, evidence []string, at time.Time) *Revision {
	superseded := r.Active
	if superseded != nil {
		superseded.Status, superseded.SupersededAt = Superseded, at
	}
	d := r.Draft
	d.Status = Active
	d.Publication = &Publication{By: by, Epoch: epoch, At: at, EvidenceIDs: evidence}
	r.Active, r.Draft = d, nil
	r.Version++
	return superseded
}
