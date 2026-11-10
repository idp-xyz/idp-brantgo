// Package rfq is the CustomerRfq aggregate: a customer's request for quotation with its lines and
// the sources they come from. A draft may lack anything but what it states must be valid; it is
// saved whole (INQ-INV-002). A human confirms its scope only when the customer, the contact, every
// line and every source are known and current and nothing is left unresolved (INQ-INV-001). A ready
// RFQ is cancelled or closed, never edited.
//
// References to parties, contacts, SKUs and sources are facts of PTY, CAT and EVD that INQ reads
// before its transaction (Context Map R01–R03, no guard lock); the domain judges them as Facts.
package rfq

import (
	"slices"
	"strconv"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Status of an RFQ.
type Status string

const (
	Draft     Status = "draft"
	Ready     Status = "ready"
	Cancelled Status = "cancelled"
	Closed    Status = "closed"
)

// Line is one requested item; empty fields are unknown. SkuSpecRevision is set by confirmation.
type Line struct {
	ID              string
	SkuID           string
	Quantity        string
	UOM             string
	RequestedItem   string
	SourceID        string
	Locator         string
	SkuSpecRevision int64
}

// Confirmation records what a ready RFQ was confirmed against.
type Confirmation struct {
	CustomerVersion int64
	ContactVersion  int64
	Evidence        []string
	By              kernel.ActorRef
	Epoch           int64
}

// Rfq is the aggregate root. Sources are every source the RFQ uses, sorted.
type Rfq struct {
	ID           string
	Status       Status
	RowVersion   int64
	CustomerID   string
	ContactID    string
	Lines        []Line
	Sources      []string
	Unresolved   []string
	OwnerActor   string
	OwnerTeam    string
	Confirmation *Confirmation
}

// New returns an unsaved draft the actor will own.
func New(id, ownerActor, ownerTeam string) *Rfq {
	return &Rfq{ID: id, Status: Draft, OwnerActor: ownerActor, OwnerTeam: ownerTeam}
}

// Content is the whole content of a draft save.
type Content struct {
	CustomerID string
	ContactID  string
	Lines      []Line
	SourceIDs  []string
	Unresolved []string
}

// Party, Contact and Sku are what the upstream contexts said about a reference the caller sees; a
// reference that is missing from Facts does not exist or is outside the caller's data scope.
type Party struct {
	Customer   bool
	Confirmed  bool
	RowVersion int64
}

type Contact struct {
	PartyID    string
	Confirmed  bool
	RowVersion int64
}

type Sku struct {
	Confirmed    bool
	SpecRevision int64
}

// Facts are the upstream answers a command is judged against.
type Facts struct {
	Parties  map[string]Party
	Contacts map[string]Contact
	Skus     map[string]Sku
	Sources  map[string]bool
}

// CheckSave admits a whole-draft save (Python _save_rfq): a missing RFQ only at expected version 0,
// an existing one only as a visible draft at its current version.
func CheckSave(r *Rfq, visible bool, expected int64) error {
	if r == nil {
		if expected != 0 {
			return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
		}
		return nil
	}
	return Check(r, visible, expected, Draft)
}

// Check admits a command on an existing, visible RFQ at its current version in an allowed state.
func Check(r *Rfq, visible bool, expected int64, allowed ...Status) error {
	switch {
	case r == nil || !visible:
		return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case r.RowVersion != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case !slices.Contains(allowed, r.Status):
		return kernel.Reject(kernel.CodeInvalidState, "/args/id", "")
	}
	return nil
}

// CheckDraft judges the content of a save in the Python order: duplicate line IDs, known
// quantities, then every reference. A draft may reference unconfirmed records; it may not reference
// records of the wrong kind, of another company or outside the caller's data scope.
func CheckDraft(d Content, f Facts) error {
	seen := map[string]bool{}
	for _, l := range d.Lines {
		if seen[l.ID] {
			return kernel.Reject(kernel.CodeDuplicateLineID, "/args/lines", "")
		}
		seen[l.ID] = true
	}
	for i, l := range d.Lines {
		if l.Quantity == "" {
			continue
		}
		q, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if err != nil || q.Sign() <= 0 {
			return kernel.Reject(kernel.CodeInvalidKnownValue, line(i, "quantity"), "")
		}
	}
	if d.CustomerID != "" {
		if p, ok := f.Parties[d.CustomerID]; !ok || !p.Customer {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/customer_id", "")
		}
	}
	if d.ContactID != "" {
		c, ok := f.Contacts[d.ContactID]
		if !ok || (d.CustomerID != "" && c.PartyID != d.CustomerID) {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/contact_id", "")
		}
	}
	for i, s := range d.SourceIDs {
		if !f.Sources[s] {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/source_ids/"+strconv.Itoa(i), "Source is not accessible")
		}
	}
	for i, l := range d.Lines {
		if _, ok := f.Skus[l.SkuID]; l.SkuID != "" && !ok {
			return kernel.Reject(kernel.CodeReferenceInvalid, line(i, "sku_id"), "")
		}
		if l.SourceID != "" && !f.Sources[l.SourceID] {
			return kernel.Reject(kernel.CodeReferenceInvalid, line(i, "source_id"), "Source is not accessible")
		}
	}
	return nil
}

// ReplaceDraft replaces the whole draft. The RFQ then uses its own sources and those of its lines.
func (r *Rfq) ReplaceDraft(d Content) {
	r.CustomerID, r.ContactID = d.CustomerID, d.ContactID
	r.Lines = slices.Clone(d.Lines)
	r.Unresolved = slices.Clone(d.Unresolved)
	r.Sources = slices.Clone(d.SourceIDs)
	for _, l := range d.Lines {
		if l.SourceID != "" {
			r.Sources = append(r.Sources, l.SourceID)
		}
	}
	slices.Sort(r.Sources)
	r.Sources = slices.Compact(r.Sources)
	r.RowVersion++
}

// Confirm makes the draft ready (Python _confirm_rfq, in its order) and records the upstream
// versions and the evidence it was confirmed against. The evidence must be readable and cover every
// source the RFQ uses; it is added to the RFQ's sources.
func (r *Rfq) Confirm(evidence []string, f Facts, by kernel.ActorRef, epoch int64) error {
	for i, s := range evidence {
		if !f.Sources[s] {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/evidence_ids/"+strconv.Itoa(i), "Source is not accessible")
		}
	}
	if r.CustomerID == "" || r.ContactID == "" {
		return kernel.Reject(kernel.CodeDraftNotReady, "/args/customer_id", "Customer and contact required")
	}
	customer, ok := f.Parties[r.CustomerID]
	if !ok || !customer.Customer || !customer.Confirmed {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/customer_id", "")
	}
	contact, ok := f.Contacts[r.ContactID]
	if !ok || !contact.Confirmed || contact.PartyID != r.CustomerID {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/contact_id", "")
	}
	if len(r.Unresolved) > 0 {
		return kernel.Reject(kernel.CodeDraftNotReady, "/args/unresolved_fields", "")
	}
	if len(r.Lines) == 0 {
		return kernel.Reject(kernel.CodeDraftNotReady, "/args/lines", "")
	}
	lines := slices.Clone(r.Lines)
	for i, l := range lines {
		for _, field := range [...]struct{ name, value string }{
			{"sku_id", l.SkuID}, {"quantity", l.Quantity}, {"uom", l.UOM}, {"source_id", l.SourceID}, {"locator", l.Locator},
		} {
			if field.value == "" {
				return kernel.Reject(kernel.CodeDraftNotReady, line(i, field.name), "")
			}
		}
		sku, ok := f.Skus[l.SkuID]
		if !ok || !sku.Confirmed {
			return kernel.Reject(kernel.CodeReferenceInvalid, line(i, "sku_id"), "")
		}
		lines[i].SkuSpecRevision = sku.SpecRevision
	}
	for _, s := range r.Sources {
		if !slices.Contains(evidence, s) {
			return kernel.Reject(kernel.CodeDraftNotReady, "/args/evidence_ids", "Human confirmation must cover all referenced sources")
		}
	}
	r.Lines = lines
	r.Sources = append(r.Sources, evidence...)
	slices.Sort(r.Sources)
	r.Sources = slices.Compact(r.Sources)
	r.Confirmation = &Confirmation{CustomerVersion: customer.RowVersion, ContactVersion: contact.RowVersion,
		Evidence: slices.Clone(evidence), By: by, Epoch: epoch}
	r.move(Ready)
	return nil
}

// Cancel cancels a draft or ready RFQ; its lines and sources stay.
func (r *Rfq) Cancel() { r.move(Cancelled) }

// Close closes a ready RFQ.
func (r *Rfq) Close() { r.move(Closed) }

func (r *Rfq) move(s Status) {
	r.Status = s
	r.RowVersion++
}

func line(i int, field string) string { return "/args/lines/" + strconv.Itoa(i) + "/" + field }
