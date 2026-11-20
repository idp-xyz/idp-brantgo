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
// Locators place its fields in its source and Pending names the fields the source does not settle
// (WBS S4-04b). ProposedBy is the actor whose save last changed the line.
type Line struct {
	ID              string
	SkuID           string
	Quantity        string
	UOM             string
	RequestedItem   string
	SourceID        string
	Locator         string
	Locators        []FieldLocator
	Pending         []PendingField
	SkuSpecRevision int64
	ProposedBy      kernel.ActorRef
}

// FieldLocator places a field of a line in the line's source; Quote is the source's own words.
type FieldLocator struct {
	Field   string
	Locator string
	Quote   string
}

// PendingField is a field its source does not settle, for a reason, with candidate values.
type PendingField struct {
	Field      string
	Reason     string
	Candidates []string
}

// The fields of a line that locators, pending fields and verifications name.
const (
	FieldRequestedItem = "requested_item"
	FieldSkuID         = "sku_id"
	FieldQuantity      = "quantity"
	FieldUOM           = "uom"
	FieldSourceID      = "source_id"
	FieldLineLocator   = "locator"
)

// PendingAmbiguous is the reason of a field that fits several values or places.
const PendingAmbiguous = "ambiguous"

// CodeSourceAmbiguous: a pending field still fits several values or places (error catalog).
const CodeSourceAmbiguous = "SOURCE_AMBIGUOUS"

// value returns the line's value of field.
func (l Line) value(field string) string {
	switch field {
	case FieldRequestedItem:
		return l.RequestedItem
	case FieldSkuID:
		return l.SkuID
	case FieldQuantity:
		return l.Quantity
	case FieldUOM:
		return l.UOM
	case FieldSourceID:
		return l.SourceID
	case FieldLineLocator:
		return l.Locator
	}
	return ""
}

func (l *Line) set(field, value string) {
	switch field {
	case FieldRequestedItem:
		l.RequestedItem = value
	case FieldSkuID:
		l.SkuID = value
	case FieldQuantity:
		l.Quantity = value
	case FieldUOM:
		l.UOM = value
	}
}

// locators returns the locators of field in order.
func (l Line) locators(field string) []string {
	var out []string
	for _, x := range l.Locators {
		if x.Field == field {
			out = append(out, x.Locator)
		}
	}
	return out
}

// pending returns the pending entry of field, or nil.
func (l Line) pending(field string) *PendingField {
	for i := range l.Pending {
		if l.Pending[i].Field == field {
			return &l.Pending[i]
		}
	}
	return nil
}

// sameContent reports whether two versions of a line state the same content.
func sameContent(a, b Line) bool {
	return a.ID == b.ID && a.SkuID == b.SkuID && a.Quantity == b.Quantity && a.UOM == b.UOM && a.RequestedItem == b.RequestedItem &&
		a.SourceID == b.SourceID && a.Locator == b.Locator && slices.Equal(a.Locators, b.Locators) &&
		slices.EqualFunc(a.Pending, b.Pending, func(x, y PendingField) bool {
			return x.Field == y.Field && x.Reason == y.Reason && slices.Equal(x.Candidates, y.Candidates)
		})
}

// Correction is a human's correction of a field an AI actor proposed, or the human's verification
// of a pending field (Clarification). RowVersion is the RFQ version it produced.
type Correction struct {
	RowVersion  int64
	Command     string
	LineID      string
	Field       string
	Kind        string
	From        string
	To          string
	Locator     string
	Reason      string
	Candidates  []string
	ProposedBy  kernel.ActorRef
	CorrectedBy kernel.ActorRef
}

// Kinds of corrections.
const (
	Changed   = "changed"
	Verified  = "verified"
	Relocated = "relocated"
	Removed   = "removed"
)

// Commands that record corrections.
const (
	CommandSaveDraft    = "rfq.save_draft"
	CommandConfirmScope = "rfq.confirm_scope"
)

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

// ReplaceDraft replaces the whole draft as saved by by. The RFQ then uses its own sources and those
// of its lines. A line keeps the actor who proposed it while its content stays the same; a changed
// or new line is by's. When a human changes, settles or removes a line an AI actor proposed, each
// change is returned as a correction (BC-AT27 逐行修正: every human correction of a candidate is
// recorded, whichever command makes it).
func (r *Rfq) ReplaceDraft(d Content, by kernel.ActorRef) []Correction {
	old := make(map[string]Line, len(r.Lines))
	for _, l := range r.Lines {
		old[l.ID] = l
	}
	human := by.Kind == kernel.ActorHuman
	lines := slices.Clone(d.Lines)
	var out []Correction
	kept := map[string]bool{}
	for i, l := range lines {
		kept[l.ID] = true
		prev, ok := old[l.ID]
		if ok && sameContent(prev, l) {
			lines[i].ProposedBy = prev.ProposedBy
			continue
		}
		lines[i].ProposedBy = by
		if ok && human && prev.ProposedBy.Kind == kernel.ActorAI {
			out = append(out, corrections(prev, lines[i])...)
		}
	}
	if human {
		for _, prev := range r.Lines {
			if !kept[prev.ID] && prev.ProposedBy.Kind == kernel.ActorAI {
				out = append(out, Correction{LineID: prev.ID, Kind: Removed, ProposedBy: prev.ProposedBy})
			}
		}
	}
	r.CustomerID, r.ContactID = d.CustomerID, d.ContactID
	r.Lines = lines
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
	for i := range out {
		out[i].RowVersion, out[i].Command, out[i].CorrectedBy = r.RowVersion, CommandSaveDraft, by
	}
	return out
}

// corrections compares a line an AI actor proposed with the human's version of it, field by field:
// a changed value, a kept value whose places changed, or a kept value whose pending entry is gone.
func corrections(prev, next Line) []Correction {
	var out []Correction
	for _, field := range []string{FieldRequestedItem, FieldSkuID, FieldQuantity, FieldUOM, FieldSourceID, FieldLineLocator} {
		from, to := prev.value(field), next.value(field)
		p, n := prev.pending(field), next.pending(field)
		kind := ""
		switch {
		case from != to:
			kind = Changed
		case !slices.Equal(prev.locators(field), next.locators(field)):
			kind = Relocated
		case p != nil && n == nil:
			kind = Verified
		default:
			continue
		}
		c := Correction{LineID: prev.ID, Field: field, Kind: kind, From: from, To: to, ProposedBy: prev.ProposedBy}
		if locs := next.locators(field); len(locs) > 0 {
			c.Locator = locs[0]
		}
		if p != nil {
			c.Reason, c.Candidates = p.Reason, slices.Clone(p.Candidates)
		}
		out = append(out, c)
	}
	return out
}

// Verification is a human's verification of one line: the fields whose value the human states.
type Verification struct {
	LineID string
	Fields []VerifiedField
}

// VerifiedField is a field's verified value and, optionally, the locator it was verified at.
type VerifiedField struct {
	Field   string
	Value   string
	Locator string
}

// Confirm makes the draft ready (Python _confirm_rfq, in its order) and records the upstream
// versions and the evidence it was confirmed against. The evidence must be readable and cover every
// source the RFQ uses; it is added to the RFQ's sources.
//
// The human's line-by-line verification (WBS S4-04b) is applied once the lines are known to exist:
// a verification names a draft line (REFERENCE_INVALID) and a locator of the field when it names
// one (REFERENCE_INVALID); a field placed in several places that is pending as ambiguous needs the
// place verified (SOURCE_AMBIGUOUS). When any line was proposed by an AI actor, every line must be
// verified (DRAFT_NOT_READY). Then no field may be left pending: SOURCE_AMBIGUOUS for an ambiguous
// one, DRAFT_NOT_READY for any other (BC-AT05: a contradiction waits, never smoothed over). Every
// verified field is returned as a correction (changed or verified).
func (r *Rfq) Confirm(evidence []string, verified []Verification, f Facts, by kernel.ActorRef, epoch int64) ([]Correction, error) {
	out, err := r.confirm(evidence, verified, f, by, epoch)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].RowVersion, out[i].Command, out[i].CorrectedBy = r.RowVersion, CommandConfirmScope, by
	}
	return out, nil
}

func (r *Rfq) confirm(evidence []string, verified []Verification, f Facts, by kernel.ActorRef, epoch int64) ([]Correction, error) {
	for i, s := range evidence {
		if !f.Sources[s] {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/evidence_ids/"+strconv.Itoa(i), "Source is not accessible")
		}
	}
	if r.CustomerID == "" || r.ContactID == "" {
		return nil, kernel.Reject(kernel.CodeDraftNotReady, "/args/customer_id", "Customer and contact required")
	}
	customer, ok := f.Parties[r.CustomerID]
	if !ok || !customer.Customer || !customer.Confirmed {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/customer_id", "")
	}
	contact, ok := f.Contacts[r.ContactID]
	if !ok || !contact.Confirmed || contact.PartyID != r.CustomerID {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/contact_id", "")
	}
	if len(r.Unresolved) > 0 {
		return nil, kernel.Reject(kernel.CodeDraftNotReady, "/args/unresolved_fields", "")
	}
	if len(r.Lines) == 0 {
		return nil, kernel.Reject(kernel.CodeDraftNotReady, "/args/lines", "")
	}
	lines, out, err := r.verify(verified)
	if err != nil {
		return nil, err
	}
	for i, l := range lines {
		if len(l.Pending) == 0 {
			continue
		}
		// The first field left pending answers: no pending field confirms, whatever its reason.
		p := l.Pending[0]
		if p.Reason == PendingAmbiguous {
			return nil, kernel.RejectAs(CodeSourceAmbiguous, kernel.CategoryInvalid, line(i, p.Field), "Verify the value and its place")
		}
		return nil, kernel.Reject(kernel.CodeDraftNotReady, line(i, p.Field), "Pending: "+p.Reason)
	}
	for i, l := range lines {
		for _, field := range [...]struct{ name, value string }{
			{"sku_id", l.SkuID}, {"quantity", l.Quantity}, {"uom", l.UOM}, {"source_id", l.SourceID}, {"locator", l.Locator},
		} {
			if field.value == "" {
				return nil, kernel.Reject(kernel.CodeDraftNotReady, line(i, field.name), "")
			}
		}
		sku, ok := f.Skus[l.SkuID]
		if !ok || !sku.Confirmed {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, line(i, "sku_id"), "")
		}
		lines[i].SkuSpecRevision = sku.SpecRevision
	}
	for _, s := range r.Sources {
		if !slices.Contains(evidence, s) {
			return nil, kernel.Reject(kernel.CodeDraftNotReady, "/args/evidence_ids", "Human confirmation must cover all referenced sources")
		}
	}
	r.Lines = lines
	r.Sources = append(r.Sources, evidence...)
	slices.Sort(r.Sources)
	r.Sources = slices.Compact(r.Sources)
	r.Confirmation = &Confirmation{CustomerVersion: customer.RowVersion, ContactVersion: contact.RowVersion,
		Evidence: slices.Clone(evidence), By: by, Epoch: epoch}
	r.move(Ready)
	return out, nil
}

// verify applies the human's verifications to a copy of the lines and returns it with the
// corrections they record.
func (r *Rfq) verify(verified []Verification) ([]Line, []Correction, error) {
	lines := make([]Line, len(r.Lines))
	index := make(map[string]int, len(r.Lines))
	candidates := false
	for i, l := range r.Lines {
		lines[i] = l
		lines[i].Locators, lines[i].Pending = slices.Clone(l.Locators), slices.Clone(l.Pending)
		index[l.ID] = i
		candidates = candidates || l.ProposedBy.Kind == kernel.ActorAI
	}
	var out []Correction
	covered := map[string]bool{}
	for k, v := range verified {
		p := "/args/verified_lines/" + strconv.Itoa(k)
		i, ok := index[v.LineID]
		if !ok {
			return nil, nil, kernel.Reject(kernel.CodeReferenceInvalid, p+"/line_id", "No such line in the draft")
		}
		covered[v.LineID] = true
		l := &lines[i]
		for j, vf := range v.Fields {
			q := p + "/fields/" + strconv.Itoa(j)
			places := l.locators(vf.Field)
			pending := l.pending(vf.Field)
			switch {
			case vf.Locator != "" && !slices.Contains(places, vf.Locator):
				return nil, nil, kernel.Reject(kernel.CodeReferenceInvalid, q+"/locator", "Not a locator of the field")
			case vf.Locator == "" && pending != nil && pending.Reason == PendingAmbiguous && len(places) > 1:
				return nil, nil, kernel.RejectAs(CodeSourceAmbiguous, kernel.CategoryInvalid, q+"/locator", "Name the place verified")
			}
			if vf.Field == FieldQuantity {
				if d, err := kernel.ParseDecimal(vf.Value, kernel.QuantityFormat); err != nil || d.Sign() <= 0 {
					return nil, nil, kernel.Reject(kernel.CodeInvalidKnownValue, q+"/value", "")
				}
			}
			c := Correction{LineID: l.ID, Field: vf.Field, Kind: Verified, From: l.value(vf.Field), To: vf.Value, Locator: vf.Locator,
				ProposedBy: l.ProposedBy}
			if c.From != c.To {
				c.Kind = Changed
			}
			if pending != nil {
				c.Reason, c.Candidates = pending.Reason, slices.Clone(pending.Candidates)
			}
			out = append(out, c)
			l.set(vf.Field, vf.Value)
			if vf.Locator != "" {
				l.Locators = slices.DeleteFunc(l.Locators, func(x FieldLocator) bool { return x.Field == vf.Field && x.Locator != vf.Locator })
			}
			l.Pending = slices.DeleteFunc(l.Pending, func(x PendingField) bool { return x.Field == vf.Field })
		}
	}
	if candidates {
		for _, l := range lines {
			if !covered[l.ID] {
				return nil, nil, kernel.Reject(kernel.CodeDraftNotReady, "/args/verified_lines", "Human confirmation must verify every line")
			}
		}
	}
	return lines, out, nil
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
