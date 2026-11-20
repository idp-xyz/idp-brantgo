// Package inspection holds QLT's Inspection aggregate (tactical model QLT-AGG-Inspection with its
// Disposition, SM-QLT-INSPECTION, QLT-INV-001; F-06, BT08, baseline 7.5; S5R-D18, S5R-D36): the
// result of an inspection of a subject, judged on what PUR, EVD and PTY said of its references before
// QLT's transaction and on the subject's other inspections, locked in it.
//
// An inspection is recorded once and never edited. Its result is evidence of conformity, not a
// release (F-06): only quality_release.grant releases. A re-inspection or a correction is a new
// inspection that supersedes the one it cites and restates its result, so the conforming quantity
// of a subject is that of its valid (not superseded) inspections and never grows by inspecting the
// same goods again (BT08); the valid inspections of a subject never cover more than its quantity
// (INSPECTION_QUANTITY_EXCEEDED). A disposition decides what happens to the whole nonconforming
// quantity; it releases nothing itself, but the quantity it accepts by concession — on the
// customer's consent, which it cites as evidence — may be released next to the conforming quantity
// while the inspection is valid (S5R-D18, UL QLT-Disposition).
package inspection

import (
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// States of an inspection (SM-QLT-INSPECTION).
const (
	StatusRecorded   = "recorded"
	StatusDisposed   = "disposed"
	StatusSuperseded = "superseded"
)

// Inspector kinds, sampling methods and disposition actions (api constants).
const (
	InspectorInternal = "internal"
	InspectorAgency   = "external_agency"

	SamplingFull   = "full"
	SamplingSample = "sample"

	ActionRework     = "rework"
	ActionConcession = "concession"
	ActionReturn     = "return"
	ActionScrap      = "scrap"
)

// Codes the inspection rules give besides the shared kernel codes, with their categories.
const (
	CodeReferenceStale      = "REFERENCE_STALE"
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
	// CodeInspectionQuantityExceeded: the subject's valid inspections would cover more than its
	// quantity; a re-inspection supersedes the inspection of the same goods (error catalog, S5R).
	CodeInspectionQuantityExceeded = "INSPECTION_QUANTITY_EXCEEDED"
)

// Codes are the codes of this package and their categories (error catalog V0.1).
var Codes = map[string]kernel.Category{
	CodeReferenceStale:             kernel.CategoryConflict,
	CodeBusinessKeyConflict:        kernel.CategoryConflict,
	CodeInspectionQuantityExceeded: kernel.CategoryInvalid,
}

// Fail returns the rejection of code at path: a shared kernel code or one of codes.
func Fail(codes map[string]kernel.Category, code, path, reason string) error {
	if c, ok := codes[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

func fail(code, path, reason string) error { return Fail(Codes, code, path, reason) }

// SubjectRef names a subject: line LineID of purchase commitment ID.
type SubjectRef struct {
	Kind   string
	ID     string
	LineID string
}

// Spec is the specification and unit a result is counted at.
type Spec struct {
	SkuID        string
	SpecRevision int64
	UnitID       string
	UnitRevision int64
}

// Subject is a subject as PUR said it was when a record bound it (InspectionSubject v1): the
// commitment revision and hash, the supplier, the sales order line it serves, the specification,
// the unit and the quantity.
type Subject struct {
	Ref           SubjectRef
	Revision      int64
	Hash          string
	SupplierID    string
	OrderID       string
	OrderRevision int64
	OrderHash     string
	OrderLineID   string
	SkuID         string
	SpecRevision  int64
	UnitID        string
	UnitRevision  int64
	Quantity      kernel.Decimal
}

// Spec returns the specification and unit of s.
func (s Subject) Spec() Spec {
	return Spec{SkuID: s.SkuID, SpecRevision: s.SpecRevision, UnitID: s.UnitID, UnitRevision: s.UnitRevision}
}

// Act is who did something, under which authority epoch, when.
type Act struct {
	By    kernel.ActorRef
	Epoch int64
	At    time.Time
}

// Inspector is who inspected: our recorder (internal, ActorID) or an external agency (revision
// RoleRevision of service role ServiceRoleID of party PartyID).
type Inspector struct {
	Kind          string
	ActorID       string
	PartyID       string
	ServiceRoleID string
	RoleRevision  int64
}

// Sampling is how the inspected quantity was examined; SampleSize is nil for a full inspection.
type Sampling struct {
	Method     string
	SampleSize *kernel.Decimal
	Plan       string
}

// Binding is an EVD source or business file as a record binds it.
type Binding struct {
	ID       string
	SHA256   string
	MimeType string // files only
}

// Item disposes Quantity of the nonconforming quantity by Action.
type Item struct {
	Action   string
	Quantity kernel.Decimal
}

// Disposition is the decision on an inspection's whole nonconforming quantity.
type Disposition struct {
	Items    []Item
	Reason   string
	Evidence []string
	Decided  Act
}

// Inspection is the aggregate root. RowVersion is 1 when recorded and moves by one with the
// disposition and with the supersession. Seq numbers the subject's inspections from 1.
type Inspection struct {
	ID            string
	Status        string
	RowVersion    int64
	Seq           int64
	Subject       Subject
	Inspected     kernel.Decimal
	Conforming    kernel.Decimal
	Nonconforming kernel.Decimal
	Sampling      Sampling
	Standard      Binding
	Inspector     Inspector
	Report        Binding
	InspectedAt   time.Time
	Supersedes    string
	Reason        string
	Evidence      []string
	SupersededBy  string
	SupersededAt  time.Time
	RequestHash   string
	OwnerActor    string
	OwnerTeam     string
	Recorded      Act
	Disposition   *Disposition
}

// Valid reports whether the result counts: the inspection is not superseded.
func (i *Inspection) Valid() bool { return i.Status != StatusSuperseded }

// Counts reports whether the result counts for a subject at spec.
func (i *Inspection) Counts(spec Spec) bool { return i.Valid() && i.Subject.Spec() == spec }

// Agency is PTY's answer for the cited service role (ServiceRoleRef v1, R73) through QLT's port:
// whether the role is an inspection agency's (S5R-D36: inspection_agency, verified, no route
// required) and Check, which passes on PTY's verdict on it at a path.
type Agency struct {
	InspectionAgency bool
	Check            func(path string) error
}

// Request is an inspection.record as the rules judge it; the api checked its structure.
type Request struct {
	ID            string
	Subject       SubjectRef
	Inspected     kernel.Decimal
	Conforming    kernel.Decimal
	Nonconforming kernel.Decimal
	Sampling      Sampling
	StandardID    string
	InspectorKind string
	Agency        *Inspector // PartyID, ServiceRoleID and RoleRevision for an external agency
	ReportFileID  string
	InspectedAt   time.Time
	Supersedes    string
	Reason        string
	Evidence      []string
	RequestHash   string
}

// Inputs are the facts a recording is judged on.
type Inputs struct {
	Subject     *Subject               // PUR's subject now; nil when PUR shows the caller none
	SubjectCode string                 // PUR's reason the subject does not serve now; "" when it does
	Existing    []*Inspection          // the subject's inspections, locked
	Visible     func(*Inspection) bool // the recorder's data scope
	Sources     map[string]string      // the EVD sources the recorder reads: ID → content SHA-256
	Report      *Binding               // the report file EVD showed the recorder; nil when none
	Agency      *Agency                // PTY's answer for the cited agency; nil when PTY has none
	Recorder    kernel.ActorRef
	Epoch       int64
	Team        string
	Now         time.Time
}

// Record judges req on in and returns the new inspection and, for a re-inspection or correction,
// the inspection it supersedes, changed. It refuses, in this order: a subject PUR does not show
// (REFERENCE_INVALID) or that no longer serves (REFERENCE_STALE); an internal inspection recorded by
// anyone but a human (PERMISSION_DENIED: an AI may transcribe an agency's report, it is no
// inspector); an inspection in the future (INVALID_KNOWN_VALUE); a superseded inspection that is not
// one of the subject's the recorder sees (REFERENCE_INVALID) or no longer valid (INVALID_STATE); an
// inspected quantity that would make the subject's valid inspections cover more than its quantity
// (INSPECTION_QUANTITY_EXCEEDED: inspect again by superseding); an agency PTY does not show or whose
// role is not an inspection agency's (REFERENCE_INVALID) or that is not current (REFERENCE_STALE); a
// standard, evidence or report the recorder does not read (REFERENCE_INVALID).
func Record(req Request, in Inputs) (*Inspection, *Inspection, error) {
	s := in.Subject
	switch {
	case s == nil:
		return nil, nil, fail(kernel.CodeReferenceInvalid, "/args/subject", "PUR shows no such subject")
	case in.SubjectCode != "":
		return nil, nil, fail(CodeReferenceStale, "/args/subject", "The subject does not serve inspection: "+in.SubjectCode)
	case req.InspectorKind == InspectorInternal && in.Recorder.Kind != kernel.ActorHuman:
		return nil, nil, fail(kernel.CodePermissionDenied, "/args/inspector/kind", "An internal inspection is recorded by its human inspector")
	case req.InspectedAt.After(in.Now):
		return nil, nil, fail(kernel.CodeInvalidKnownValue, "/args/inspected_at", "The inspection lies in the future")
	}
	var old *Inspection
	if req.Supersedes != "" {
		i := slices.IndexFunc(in.Existing, func(e *Inspection) bool { return e.ID == req.Supersedes })
		if i < 0 || (in.Visible != nil && !in.Visible(in.Existing[i])) {
			return nil, nil, fail(kernel.CodeReferenceInvalid, "/args/supersedes_inspection_id", "No such inspection of the subject")
		}
		if old = in.Existing[i]; !old.Valid() {
			return nil, nil, fail(kernel.CodeInvalidState, "/args/supersedes_inspection_id", "The inspection is superseded already")
		}
	}
	covered := req.Inspected
	var seq int64
	for _, e := range in.Existing {
		seq = max(seq, e.Seq)
		if e.Valid() && e != old {
			covered = covered.Add(e.Inspected)
		}
	}
	if covered.Cmp(s.Quantity) > 0 {
		return nil, nil, fail(CodeInspectionQuantityExceeded, "/args/inspected_quantity",
			"The subject's valid inspections would cover more than its quantity; supersede the inspection of these goods instead")
	}
	inspector := Inspector{Kind: req.InspectorKind}
	switch req.InspectorKind {
	case InspectorInternal:
		inspector.ActorID = in.Recorder.ID
	case InspectorAgency:
		a := in.Agency
		if a == nil || !a.InspectionAgency || req.Agency == nil {
			return nil, nil, fail(kernel.CodeReferenceInvalid, "/args/inspector/agency", "No inspection agency of PTY")
		}
		if err := a.Check("/args/inspector/agency"); err != nil {
			return nil, nil, err
		}
		inspector.PartyID, inspector.ServiceRoleID, inspector.RoleRevision = req.Agency.PartyID, req.Agency.ServiceRoleID, req.Agency.RoleRevision
	}
	standard, ok := in.Sources[req.StandardID]
	if !ok {
		return nil, nil, fail(kernel.CodeReferenceInvalid, "/args/standard/source_id", "Source is not accessible")
	}
	if err := Readable(req.Evidence, in.Sources, "/args/evidence_ids"); err != nil {
		return nil, nil, err
	}
	if in.Report == nil || in.Report.ID != req.ReportFileID {
		return nil, nil, fail(kernel.CodeReferenceInvalid, "/args/report_file_id", "File is not accessible")
	}
	now := kernel.Instant(in.Now)
	i := &Inspection{ID: req.ID, Status: StatusRecorded, RowVersion: 1, Seq: seq + 1, Subject: *s, Inspected: req.Inspected,
		Conforming: req.Conforming, Nonconforming: req.Nonconforming, Sampling: req.Sampling,
		Standard: Binding{ID: req.StandardID, SHA256: standard}, Inspector: inspector, Report: *in.Report,
		InspectedAt: kernel.Instant(req.InspectedAt), Supersedes: req.Supersedes, Reason: req.Reason, Evidence: slices.Clone(req.Evidence),
		RequestHash: req.RequestHash, OwnerActor: in.Recorder.ID, OwnerTeam: in.Team, Recorded: Act{By: in.Recorder, Epoch: in.Epoch, At: now}}
	if old != nil {
		old.Status, old.RowVersion, old.SupersededBy, old.SupersededAt = StatusSuperseded, old.RowVersion+1, i.ID, now
	}
	return i, old, nil
}

// Answer decides a recording whose inspection ID is taken: the same request by the same recorder is
// answered from the stored inspection; anything else is BUSINESS_KEY_CONFLICT.
func (i *Inspection) Answer(requestHash string, recorder kernel.ActorRef) error {
	if i.RequestHash != requestHash || i.Recorded.By != recorder {
		return fail(CodeBusinessKeyConflict, "/args/inspection_id", "The inspection ID holds another inspection")
	}
	return nil
}

// Decide disposes the whole nonconforming quantity of i (disposition.decide). It refuses another
// version (VERSION_CONFLICT), an inspection that is superseded or disposed already, or has no
// nonconforming quantity (INVALID_STATE), items that do not add up to the nonconforming quantity
// (INVALID_KNOWN_VALUE), a concession that cites no evidence of the customer's consent
// (SCHEMA_INVALID at /args/evidence_ids, S5R-D18) and evidence the decider does not read
// (REFERENCE_INVALID).
func (i *Inspection) Decide(expected int64, items []Item, reason string, evidence []string, sources map[string]string, act Act) error {
	switch {
	case i.RowVersion != expected:
		return fail(kernel.CodeVersionConflict, "/args/expected_version", "")
	case i.Status != StatusRecorded:
		return fail(kernel.CodeInvalidState, "/args/inspection_id", "The inspection is "+i.Status)
	case i.Nonconforming.Sign() == 0:
		return fail(kernel.CodeInvalidState, "/args/inspection_id", "The inspection found nothing nonconforming")
	}
	var sum kernel.Decimal
	for _, it := range items {
		sum = sum.Add(it.Quantity)
	}
	if !sum.Equal(i.Nonconforming) {
		return fail(kernel.CodeInvalidKnownValue, "/args/items", "The items must dispose exactly the nonconforming quantity")
	}
	if ConsentRequired(items) && len(evidence) == 0 {
		return fail(kernel.CodeSchemaInvalid, "/args/evidence_ids", ConsentReason)
	}
	if err := Readable(evidence, sources, "/args/evidence_ids"); err != nil {
		return err
	}
	act.At = kernel.Instant(act.At)
	i.Status, i.RowVersion = StatusDisposed, i.RowVersion+1
	i.Disposition = &Disposition{Items: slices.Clone(items), Reason: reason, Evidence: slices.Clone(evidence), Decided: act}
	return nil
}

// ConsentReason is the reason of the refusal of a concession without evidence (S5R-D18).
const ConsentReason = "A concession cites the customer's consent as evidence"

// ConsentRequired reports whether a disposition of items accepts anything by concession, which must
// cite the customer's consent as evidence (S5R-D18, UL QLT-Disposition).
func ConsentRequired(items []Item) bool {
	return slices.ContainsFunc(items, func(it Item) bool { return it.Action == ActionConcession })
}

// Conceded returns the quantity i's disposition accepts by concession: zero for an inspection not
// disposed or disposed otherwise.
func (i *Inspection) Conceded() kernel.Decimal {
	var sum kernel.Decimal
	if i.Disposition != nil {
		for _, it := range i.Disposition.Items {
			if it.Action == ActionConcession {
				sum = sum.Add(it.Quantity)
			}
		}
	}
	return sum
}

// Concessions returns the approved concession quantity of a subject at spec: what the dispositions
// of its valid inspections at spec accept by concession (QLT-INV-001, S5R-D18). A superseded
// inspection's concession no longer counts: its re-inspection restates the result.
func Concessions(inspections []*Inspection, spec Spec) kernel.Decimal {
	var sum kernel.Decimal
	for _, i := range inspections {
		if i.Counts(spec) {
			sum = sum.Add(i.Conceded())
		}
	}
	return sum
}

// Reinspected reports whether every inspection among ids has been inspected again since instant at
// (S5R-D18, QLT-INV-002 "冻结量须复检后才能再放行"): it is superseded, and the inspection that is
// valid now at the end of its chain of supersessions was recorded at or after at. inspections are a
// subject's inspections, superseded ones included; an ID among ids that is not one of them is not
// reinspected, and neither is an empty list (fail closed).
func Reinspected(inspections []*Inspection, ids []string, at time.Time) bool {
	if len(ids) == 0 {
		return false
	}
	byID := make(map[string]*Inspection, len(inspections))
	for _, i := range inspections {
		byID[i.ID] = i
	}
	at = kernel.Instant(at)
	for _, id := range ids {
		i, ok := byID[id]
		if !ok || i.Valid() {
			return false
		}
		for n := 0; ok && !i.Valid() && n <= len(inspections); n++ {
			i, ok = byID[i.SupersededBy]
		}
		if !ok || !i.Valid() || i.Recorded.At.Before(at) {
			return false
		}
	}
	return true
}

// Readable refuses the first of ids, at path, that the actor does not read in EVD. A missing source
// and one outside the actor's data scope are refused alike.
func Readable(ids []string, sources map[string]string, path string) error {
	for n, id := range ids {
		if _, ok := sources[id]; !ok {
			return fail(kernel.CodeReferenceInvalid, path+"/"+strconv.Itoa(n), "Source is not accessible")
		}
	}
	return nil
}

// Totals returns what the valid inspections of a subject cover and, at spec, find conforming, with
// the IDs of those counted at spec in ID order.
func Totals(inspections []*Inspection, spec Spec) (covered, conforming kernel.Decimal, counted []string) {
	for _, i := range inspections {
		if !i.Valid() {
			continue
		}
		covered = covered.Add(i.Inspected)
		if i.Counts(spec) {
			conforming = conforming.Add(i.Conforming)
			counted = append(counted, i.ID)
		}
	}
	slices.Sort(counted)
	return covered, conforming, counted
}
