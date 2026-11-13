// Package profile is the TradingProfile aggregate (PTY-AGG-TradingProfile, UL PTY-TradingProfile): the
// trading relationship between one party and one company of the tenant, identified by (tenant,
// company, party_id) and never shared across companies. Its content lives in revisions with
// monotonic numbers, like a POL configuration (SM-POL-REVISION):
//
//	working  → active      trading_profile.publish: a human other than the revision's last editor
//	active   → superseded  trading_profile.publish of the next revision, in the same transaction
//
// One working revision is saved whole until it is published; published revisions never change
// (PTY-INV-001). The root version counts every command and is the CAS of the aggregate; the
// revision number and the content hash say what the content is.
//
// A revision binds the party's confirmed revision it was drafted against, the settlement terms as
// exact POL revisions (SettlementTermsRef: payment_terms and trade_terms, ConfigRef v1), the
// business owner (OwnerRef) and the contacts with the row versions the editor saw (Python
// party_profile.contact_refs). A published revision keeps what it bound; once the party or a
// contact moves on, the revision is stale for new use and a new revision must bind the new
// versions. It may list permitted payers (S5R-D21, PermittedPayers): other confirmed parties whose
// payments count as the party's own, verified by the publication; a payer that changes later never
// makes the revision stale. Checks run in the order of the Python contracts: existence and
// visibility, root version, state, independence, references.
package profile

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the error catalog V0.1 this aggregate decides beyond the shared ones; package api
// declares the same codes for callers.
const (
	CodeReferenceStale            = "REFERENCE_STALE"             // 409: refresh the reference and resubmit
	CodeIndependentReviewRequired = "INDEPENDENT_REVIEW_REQUIRED" // 422
)

// Categories are the categories the error catalog gives the codes above.
var Categories = map[string]kernel.Category{
	CodeReferenceStale:            kernel.CategoryConflict,
	CodeIndependentReviewRequired: kernel.CategoryInvalid,
}

// Reject returns a rejection with one of the codes above.
func Reject(code, path, reason string) *kernel.Rejection {
	return kernel.RejectAs(code, Categories[code], path, reason)
}

// Status of a revision.
type Status string

const (
	Working    Status = "working"
	Active     Status = "active"
	Superseded Status = "superseded"
)

// Kinds of POL configuration the settlement terms name.
const (
	KindPaymentTerms = "payment_terms"
	KindTradeTerms   = "trade_terms"
)

// TermsRef is an exact POL configuration revision (ConfigRef v1: kind, id, revision).
type TermsRef struct {
	Kind     string `json:"kind"`
	ConfigID string `json:"id"`
	Revision int64  `json:"revision"`
}

// Owner is the business owner of the relationship in the company (OwnerRef): an actor and,
// optionally, the actor's team.
type Owner struct {
	ActorID string `json:"actor_id"`
	TeamID  string `json:"team_id,omitempty"`
}

// ContactRef is a contact of the party at the row version the editor saw.
type ContactRef struct {
	ContactID  string `json:"id"`
	RowVersion int64  `json:"row_version"`
}

// Content is the whole content of a revision. Contacts are sorted by ID; so are the permitted
// payers, which are omitted when there are none, so a revision saved before they existed keeps its
// content hash (S5R-D21).
type Content struct {
	PartyRevision   int64        `json:"party_revision"`
	PaymentTerms    TermsRef     `json:"payment_terms"`
	TradeTerms      TermsRef     `json:"trade_terms"`
	Owner           Owner        `json:"owner"`
	Contacts        []ContactRef `json:"contact_refs"`
	PermittedPayers []string     `json:"permitted_payers,omitempty"`
}

// Canonical returns c with its contacts and permitted payers sorted by ID, no contacts as an empty
// list and no permitted payers as none.
func (c Content) Canonical() Content {
	out := NewContent(c.PartyRevision, c.PaymentTerms, c.TradeTerms, c.Owner, c.Contacts)
	if len(c.PermittedPayers) > 0 {
		out.PermittedPayers = slices.Sorted(slices.Values(c.PermittedPayers))
	}
	return out
}

// NewContent returns content without permitted payers, its contacts sorted by ID.
func NewContent(partyRevision int64, payment, trade TermsRef, owner Owner, contacts []ContactRef) Content {
	c := Content{PartyRevision: partyRevision, PaymentTerms: payment, TradeTerms: trade, Owner: owner, Contacts: slices.Clone(contacts)}
	slices.SortFunc(c.Contacts, func(a, b ContactRef) int { return cmp.Compare(a.ContactID, b.ContactID) })
	if c.Contacts == nil {
		c.Contacts = []ContactRef{}
	}
	return c
}

// Hash returns the content hash: the SHA-256 of the canonical JSON of the content. Field order
// is fixed by the Go types and contacts are sorted, so equal content always hashes equally.
func (c Content) Hash() string {
	c = c.Canonical()
	doc, err := json.Marshal(c)
	if err != nil {
		panic("profile: content does not marshal: " + err.Error()) // only strings and integers
	}
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:])
}

// Publication is the independent review that made a revision active.
type Publication struct {
	By          kernel.ActorRef
	Epoch       int64 // the authority epoch the publication saw
	At          time.Time
	EvidenceIDs []string
}

// Revision is one revision of a trading profile. SavedBy is its last editor.
type Revision struct {
	Number      int64
	Status      Status
	Content     Content
	Hash        string
	SavedBy     kernel.ActorRef
	SavedEpoch  int64
	SavedAt     time.Time
	Publication *Publication // nil until published
	EndedAt     time.Time    // when it was superseded
}

// Root is the aggregate root. Version is the root version every command compares and increments;
// LastRevision the highest revision number ever assigned. Working and Active are the revisions in
// those states.
type Root struct {
	PartyID      string
	Version      int64
	LastRevision int64
	Working      *Revision
	Active       *Revision
}

// New returns a trading profile of partyID that is not stored yet.
func New(partyID string) *Root { return &Root{PartyID: partyID} }

// CheckSave admits a whole-revision save: a new profile only at expected version 0, an existing one
// only when its party is visible to the actor and at its current root version.
func CheckSave(r *Root, visible bool, expected int64) error {
	if r == nil {
		if expected != 0 {
			return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
		}
		return nil
	}
	return Check(r, visible, expected)
}

// Check admits a command on an existing profile whose party the actor sees, at its current root
// version.
func Check(r *Root, visible bool, expected int64) error {
	switch {
	case r == nil || !visible:
		return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case r.Version != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Save replaces the working revision whole, or starts one with the next revision number, and makes
// by its last editor. It reports whether the revision is new.
func (r *Root) Save(c Content, by kernel.ActorRef, epoch int64, at time.Time) (*Revision, bool) {
	created := r.Working == nil
	if created {
		r.LastRevision++
		r.Working = &Revision{Number: r.LastRevision, Status: Working}
	}
	w := r.Working
	w.Content = c.Canonical()
	w.Hash, w.SavedBy, w.SavedEpoch, w.SavedAt = w.Content.Hash(), by, epoch, at
	r.Version++
	return w, created
}

// Publishable returns the working revision number for by to publish: it must exist and be the
// working revision, and by must not be its last editor (the publisher is independent of the
// editor, as for POL-INV-001 and Python foundation.publish). The caller checks the references next
// and then calls Publish.
func (r *Root) Publishable(number int64, by kernel.ActorRef) (*Revision, error) {
	w := r.Working
	switch {
	case number > r.LastRevision:
		return nil, kernel.Reject(kernel.CodeNotFound, "/args/revision", "")
	case w == nil || w.Number != number:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/revision", "")
	case w.SavedBy.ID == by.ID:
		return nil, Reject(CodeIndependentReviewRequired, "/args/revision", "")
	}
	return w, nil
}

// Publish makes the working revision active for the human by and supersedes the active one, which
// it returns. Call it after Publishable and the reference checks.
func (r *Root) Publish(by kernel.ActorRef, epoch int64, evidence []string, at time.Time) *Revision {
	w := r.Working
	superseded := r.Active
	if superseded != nil {
		superseded.Status, superseded.EndedAt = Superseded, at
	}
	w.Status = Active
	w.Publication = &Publication{By: by, Epoch: epoch, At: at, EvidenceIDs: slices.Clone(evidence)}
	r.Active, r.Working = w, nil
	r.Version++
	return superseded
}

// PartyFact is what PTY says about the profile's party now: whether it exists and the actor sees
// it, whether it is confirmed, and in which revision.
type PartyFact struct {
	Visible   bool
	Confirmed bool
	Revision  int64
}

// ContactFact is what PTY says about a contact now. A contact that does not exist or that the
// actor does not see has no fact.
type ContactFact struct {
	PartyID    string
	Confirmed  bool
	RowVersion int64
}

// PayerFact is what PTY says about a permitted payer now: whether it is confirmed. A party that does
// not exist or that the actor does not see has no fact.
type PayerFact struct {
	Confirmed bool
}

// Facts are the current state of a content's references inside PTY.
type Facts struct {
	Party    PartyFact
	Contacts map[string]ContactFact
	Payers   map[string]PayerFact
}

// CheckReferences requires the content to bind the party's current confirmed revision and its
// contacts at their current row versions, each confirmed and belonging to the party (Python
// party_profile: _master_ref confirmed, contact parent and row_version, party_row_version). A
// reference that does not exist, is not visible, is not confirmed or belongs elsewhere is
// REFERENCE_INVALID; one that names a version the reference has moved on from is REFERENCE_STALE.
// Paths are those of trading_profile.save_draft; publish re-paths them.
func CheckReferences(partyID string, c Content, f Facts) error {
	if !f.Party.Visible || !f.Party.Confirmed {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/id", "The party is not a confirmed party the actor sees")
	}
	if c.PartyRevision != f.Party.Revision {
		return Reject(CodeReferenceStale, "/args/party_revision", "The party is confirmed in revision "+strconv.FormatInt(f.Party.Revision, 10))
	}
	for i, ref := range c.Contacts {
		path := "/args/contact_refs/" + strconv.Itoa(i)
		fact, ok := f.Contacts[ref.ContactID]
		switch {
		case !ok || fact.PartyID != partyID:
			return kernel.Reject(kernel.CodeReferenceInvalid, path+"/id", "The contact is not a contact of the party the actor sees")
		case fact.RowVersion != ref.RowVersion:
			return Reject(CodeReferenceStale, path+"/row_version", "The contact is at row version "+strconv.FormatInt(fact.RowVersion, 10))
		case !fact.Confirmed:
			return kernel.Reject(kernel.CodeReferenceInvalid, path+"/id", "The contact is not confirmed")
		}
	}
	// Permitted payers (S5R-D21): other parties, confirmed, that the actor sees.
	for i, id := range c.PermittedPayers {
		path := "/args/permitted_payers/" + strconv.Itoa(i)
		fact, ok := f.Payers[id]
		switch {
		case id == partyID:
			return kernel.Reject(kernel.CodeInvalidKnownValue, path, "The party pays for itself without being listed")
		case !ok || !fact.Confirmed:
			return kernel.Reject(kernel.CodeReferenceInvalid, path, "The payer is not a confirmed party the actor sees")
		}
	}
	return nil
}

// Repath returns the rejection in err at path instead of its own, keeping its code, category and
// reason; other errors pass unchanged.
func Repath(err error, path string) error {
	if rej, ok := kernel.AsRejection(err); ok {
		return kernel.RejectAs(rej.Code, rej.Category, path, rej.Fields[0].Reason)
	}
	return err
}

// Use is why a published revision may or may not be used for new business now; empty means it
// may (TradingProfileRef v1 lookups).
type Use string

const (
	Usable              Use = ""
	UseSuperseded       Use = "superseded"        // a later revision is active
	UsePartyChanged     Use = "party_changed"     // the party is not confirmed in the bound revision
	UseContactChanged   Use = "contact_changed"   // a bound contact is not confirmed at the bound row version
	UsePartyUnavailable Use = "party_unavailable" // the party is not confirmed now (revised or deactivated)
)

// Usability decides whether rev, a published revision, serves new business now: it must be the
// active revision, its party confirmed in the bound revision and every bound contact confirmed at
// the bound row version. The revision itself never changes; a stale revision stays readable.
func Usability(rev *Revision, party PartyFact, contacts map[string]ContactFact) Use {
	switch {
	case rev.Status != Active:
		return UseSuperseded
	case !party.Confirmed:
		return UsePartyUnavailable
	case party.Revision != rev.Content.PartyRevision:
		return UsePartyChanged
	}
	for _, ref := range rev.Content.Contacts {
		if f, ok := contacts[ref.ContactID]; !ok || !f.Confirmed || f.RowVersion != ref.RowVersion {
			return UseContactChanged
		}
	}
	return Usable
}
