// Package api is QLT's published contract (quality inspection and release, WBS S5-02; tactical model
// QLT-AGG-Inspection with Disposition, QLT-AGG-QualityRelease with ReleaseRevocation;
// SM-QLT-INSPECTION, SM-QLT-RELEASE; QLT-INV-001, QLT-INV-002; owner decision D-15; finding F-06).
// Python V0.50 has no quality context: the rules come from the design, and the tests cite its IDs.
//
//	inspection.record      human or AI: the result of an inspection of a subject — a PUR purchase
//	                       commitment line (InspectionSubject v1, R24) — in the subject's unit:
//	                       inspected = conforming + nonconforming, the sampling, the standard as an
//	                       EVD source (SourceRef v1, R72; no POL kind, D-15), the report as an EVD
//	                       business file (FileRef v1, R72) and the inspector: our own human recorder
//	                       (internal) or an external inspection agency (PTY service role
//	                       inspection, ServiceRoleRef v1, R73). The report is evidence only: no
//	                       inspection releases anything (F-06). A re-inspection or a correction
//	                       is a new record that supersedes the one it cites, with a reason; it
//	                       restates that result instead of adding to it (BT08, baseline 7.5), and
//	                       the valid inspections of a subject never cover more than its quantity.
//	disposition.decide     human only: the disposition of an inspection's whole nonconforming
//	                       quantity — rework, concession, return, scrap — with a reason. It never
//	                       creates supply, credit or a releasable quantity (UL QLT-Disposition).
//	quality_release.grant  human only (our authorized QC, F-06): a quantity of a subject, at most
//	                       the conforming quantity of its valid inspections at the subject's
//	                       specification less every quantity released before, revoked releases
//	                       included (QLT-INV-001), valid in [valid_from, valid_until); expiry is
//	                       derived, never written (tactical principle 3).
//	quality_release.revoke human only: revokes a release with a reason. FUL can no longer ship on
//	                       it, so what it has not shipped is frozen and what it shipped stays
//	                       (QLT-INV-002, BT21); revoking a revoked release changes nothing.
//
// FUL re-checks a release inside its own transaction through QualityReleaseGuard v1 (guard.go,
// R26) and reads it before through ReleaseLookups; it keeps its own consumption per release.
//
// Request members in snake_case: inspection.record {inspection_id, subject {kind, id, line_id},
// inspected_quantity, conforming_quantity, nonconforming_quantity, sampling {method, sample_size,
// plan}, standard {source_id}, inspector {kind, agency {party_id, service_role_id, revision}},
// report_file_id, inspected_at, supersedes_inspection_id, reason, evidence_ids};
// disposition.decide {inspection_id, expected_version, items [{action, quantity}], reason,
// evidence_ids}; quality_release.grant {release_id, subject {kind, id, line_id}, quantity,
// valid_from, valid_until, evidence_ids}; quality_release.revoke {release_id, reason,
// evidence_ids}.
package api

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Source is QLT's event source.
const Source = "idp-brant.qlt"

// Contract names QLT's command contract, the envelope's contract member on the command routes
// POST /api/brant/v1/qlt/commands/<command>.
const Contract = "brant.quality/0.1.0"

// Commands, reads, permissions and events (Context Map QLT, tactical model QLT aggregates).
const (
	CommandRecordInspection  = "inspection.record"
	CommandDecideDisposition = "disposition.decide"
	CommandGrantRelease      = "quality_release.grant"
	CommandRevokeRelease     = "quality_release.revoke"

	// Reads: an inspection, a release, a subject's inspections and releases, the audit trail.
	CommandGetInspection = "inspection.get"
	CommandGetRelease    = "quality_release.get"
	CommandGetSubject    = "inspection_subject.get"
	CommandAudit         = "qlt.audit"
	PermissionRead       = "qlt.read"
	PermissionAudit      = "qlt.audit"

	EventInspectionRecorded    = "idp-brant.qlt.inspection_recorded.v1"
	EventDispositionDecided    = "idp-brant.qlt.disposition_decided.v1"
	EventQualityReleased       = "idp-brant.qlt.quality_released.v1"
	EventQualityReleaseRevoked = "idp-brant.qlt.quality_release_revoked.v1"

	// SubjectCommitmentLine is a line of a PUR purchase commitment (InspectionSubject v1). A
	// verified supplier delivery line (S5-15) is not a subject yet.
	SubjectCommitmentLine = "purchase_commitment_line"

	// Inspectors (InspectorRef).
	InspectorInternal = "internal"        // our own human, who records the result
	InspectorAgency   = "external_agency" // an external inspection agency (PTY service role inspection)

	// Sampling methods.
	SamplingFull   = "full"
	SamplingSample = "sample"

	// Dispositions of a nonconforming quantity (UL QLT-Disposition).
	DispositionRework     = "rework"
	DispositionConcession = "concession"
	DispositionReturn     = "return"
	DispositionScrap      = "scrap"

	// States of an inspection (SM-QLT-INSPECTION).
	StatusRecorded   = "recorded"
	StatusDisposed   = "disposed"
	StatusSuperseded = "superseded"

	// States of a release (SM-QLT-RELEASE). StateNotYetValid and StateExpired are derived when read.
	StatusGranted    = "granted"
	StatusRevoked    = "revoked"
	StateNotYetValid = "not_yet_valid"
	StateExpired     = "expired"

	MaxReason       = 1000
	MaxPlan         = 200
	MaxEvidence     = 20
	MaxLookup       = 200
	MaxDispositions = 4
	maxVersion      = 2147483647
)

// SubjectRef names an inspection subject: line LineID of purchase commitment ID (Kind
// SubjectCommitmentLine).
type SubjectRef struct {
	Kind   string
	ID     string
	LineID string
}

// Validate checks the reference's form at path.
func (r SubjectRef) Validate(path string) error {
	switch {
	case r.Kind != SubjectCommitmentLine:
		return schema(path + "/kind")
	case !kernel.ValidIdentifier(r.ID):
		return schema(path + "/id")
	case !kernel.ValidIdentifier(r.LineID):
		return schema(path + "/line_id")
	}
	return nil
}

// Sampling is how the inspected quantity was examined: every unit (full) or a sample of
// SampleSize units under Plan (such as "AQL 2.5 GII"), which the standard source states.
type Sampling struct {
	Method     string
	SampleSize string
	Plan       string
}

// StandardRef is the standard the result was judged against: an EVD source the recorder reads;
// the inspection binds its content hash, the standard's version (BT08).
type StandardRef struct {
	SourceID string
}

// AgencyRef is an external inspection agency: revision Revision of service role ServiceRoleID of
// party PartyID, whose role is inspection (PTY ServiceRoleRef v1, R73).
type AgencyRef struct {
	PartyID       string
	ServiceRoleID string
	Revision      int64
}

// Inspector is who inspected: our recorder (InspectorInternal, no Agency) or an external agency
// (InspectorAgency with Agency).
type Inspector struct {
	Kind   string
	Agency *AgencyRef
}

// RecordInspection records inspection InspectionID of Subject. Quantities are in the subject's
// unit (kernel.QuantityFormat): InspectedQuantity is positive and the sum of ConformingQuantity and
// NonconformingQuantity. ReportFileID is the EVD file of the report; InspectedAt (UTC timestamp)
// when the inspection took place. SupersedesInspectionID names the valid inspection of the same
// subject this one replaces (re-inspection or correction), with Reason. EvidenceIDs are further
// EVD sources. Human or AI; an internal inspection is recorded by its human inspector.
type RecordInspection struct {
	IdempotencyKey         string
	InspectionID           string
	Subject                SubjectRef
	InspectedQuantity      string
	ConformingQuantity     string
	NonconformingQuantity  string
	Sampling               Sampling
	Standard               StandardRef
	Inspector              Inspector
	ReportFileID           string
	InspectedAt            string
	SupersedesInspectionID string
	Reason                 string
	EvidenceIDs            []string
}

// DispositionItem disposes Quantity of the nonconforming quantity by Action.
type DispositionItem struct {
	Action   string
	Quantity string
}

// DecideDisposition disposes the whole nonconforming quantity of inspection InspectionID at
// ExpectedVersion, each action at most once, for Reason. Human only.
type DecideDisposition struct {
	IdempotencyKey  string
	InspectionID    string
	ExpectedVersion int64
	Items           []DispositionItem
	Reason          string
	EvidenceIDs     []string
}

// GrantRelease releases Quantity of Subject (in the subject's unit) as release ReleaseID, valid
// from ValidFrom (UTC timestamp; the decision's instant when empty) until ValidUntil. Human only.
type GrantRelease struct {
	IdempotencyKey string
	ReleaseID      string
	Subject        SubjectRef
	Quantity       string
	ValidFrom      string
	ValidUntil     string
	EvidenceIDs    []string
}

// RevokeRelease revokes release ReleaseID for Reason. Human only.
type RevokeRelease struct {
	IdempotencyKey string
	ReleaseID      string
	Reason         string
	EvidenceIDs    []string
}

// Command is one of QLT's commands.
type Command interface{ qltCommand() }

func (RecordInspection) qltCommand()  {}
func (DecideDisposition) qltCommand() {}
func (GrantRelease) qltCommand()      {}
func (RevokeRelease) qltCommand()     {}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func invalid(path, reason string) error {
	return kernel.Reject(kernel.CodeInvalidKnownValue, path, reason)
}

// Reason reports whether s is a reason: 1 to MaxReason characters of valid UTF-8, not only white
// space, without control characters.
func Reason(s string) bool { return text(s, MaxReason) }

func text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s) && strings.TrimSpace(s) != "" && strings.IndexFunc(s, unicode.IsControl) < 0
}

// Quantity parses s as a quantity in kernel.QuantityFormat.
func Quantity(s string) (kernel.Decimal, bool) {
	q, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	return q, err == nil
}

func evidence(ids []string, path string) error {
	if len(ids) > MaxEvidence {
		return schema(path)
	}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) {
			return schema(path + "/" + strconv.Itoa(i))
		}
	}
	return nil
}

// Validate checks the closed contract: identifiers, quantities (inspected positive and the sum of
// conforming and nonconforming: INVALID_KNOWN_VALUE otherwise), the sampling (a sample of at most
// the inspected quantity), the standard, the inspector (an agency exactly for an external one), the
// report, the instant, and a reason exactly when the record supersedes another.
func (c RecordInspection) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.InspectionID):
		return schema("/args/inspection_id")
	}
	if err := c.Subject.Validate("/args/subject"); err != nil {
		return err
	}
	inspected, ok := Quantity(c.InspectedQuantity)
	if !ok || inspected.Sign() <= 0 {
		return schema("/args/inspected_quantity")
	}
	conforming, ok := Quantity(c.ConformingQuantity)
	if !ok {
		return schema("/args/conforming_quantity")
	}
	nonconforming, ok := Quantity(c.NonconformingQuantity)
	if !ok {
		return schema("/args/nonconforming_quantity")
	}
	if !conforming.Add(nonconforming).Equal(inspected) {
		return invalid("/args/conforming_quantity", "Conforming and nonconforming quantities must add up to the inspected quantity")
	}
	s := c.Sampling
	switch s.Method {
	case SamplingFull:
		if s.SampleSize != "" {
			return schema("/args/sampling/sample_size")
		}
	case SamplingSample:
		size, ok := Quantity(s.SampleSize)
		if !ok || size.Sign() <= 0 {
			return schema("/args/sampling/sample_size")
		}
		if size.Cmp(inspected) > 0 {
			return invalid("/args/sampling/sample_size", "The sample is larger than the inspected quantity")
		}
	default:
		return schema("/args/sampling/method")
	}
	if s.Plan != "" && !text(s.Plan, MaxPlan) {
		return schema("/args/sampling/plan")
	}
	if !kernel.ValidIdentifier(c.Standard.SourceID) {
		return schema("/args/standard/source_id")
	}
	switch c.Inspector.Kind {
	case InspectorInternal:
		if c.Inspector.Agency != nil {
			return schema("/args/inspector/agency")
		}
	case InspectorAgency:
		a := c.Inspector.Agency
		switch {
		case a == nil:
			return schema("/args/inspector/agency")
		case !kernel.ValidIdentifier(a.PartyID):
			return schema("/args/inspector/agency/party_id")
		case !kernel.ValidIdentifier(a.ServiceRoleID):
			return schema("/args/inspector/agency/service_role_id")
		case a.Revision < 1 || a.Revision > maxVersion:
			return schema("/args/inspector/agency/revision")
		}
	default:
		return schema("/args/inspector/kind")
	}
	if !kernel.ValidIdentifier(c.ReportFileID) {
		return schema("/args/report_file_id")
	}
	if _, err := kernel.ParseTimestamp(c.InspectedAt); err != nil {
		return schema("/args/inspected_at")
	}
	switch {
	case c.SupersedesInspectionID == "":
		if c.Reason != "" {
			return schema("/args/reason")
		}
	case !kernel.ValidIdentifier(c.SupersedesInspectionID) || c.SupersedesInspectionID == c.InspectionID:
		return schema("/args/supersedes_inspection_id")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// Validate checks the closed contract: 1 to MaxDispositions items of known, distinct actions with
// positive quantities, and a reason.
func (c DecideDisposition) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.InspectionID):
		return schema("/args/inspection_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case len(c.Items) < 1 || len(c.Items) > MaxDispositions:
		return schema("/args/items")
	}
	seen := map[string]bool{}
	for i, it := range c.Items {
		p := "/args/items/" + strconv.Itoa(i)
		switch it.Action {
		case DispositionRework, DispositionConcession, DispositionReturn, DispositionScrap:
		default:
			return schema(p + "/action")
		}
		if q, ok := Quantity(it.Quantity); !ok || q.Sign() <= 0 {
			return schema(p + "/quantity")
		}
		if seen[it.Action] {
			return kernel.Reject(kernel.CodeDuplicateLineID, p+"/action", "")
		}
		seen[it.Action] = true
	}
	if !Reason(c.Reason) {
		return schema("/args/reason")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// Validate checks the closed contract: a positive quantity and a validity that ends after it
// starts (INVALID_KNOWN_VALUE otherwise).
func (c GrantRelease) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ReleaseID):
		return schema("/args/release_id")
	}
	if err := c.Subject.Validate("/args/subject"); err != nil {
		return err
	}
	if q, ok := Quantity(c.Quantity); !ok || q.Sign() <= 0 {
		return schema("/args/quantity")
	}
	until, err := kernel.ParseTimestamp(c.ValidUntil)
	if err != nil {
		return schema("/args/valid_until")
	}
	if c.ValidFrom != "" {
		from, err := kernel.ParseTimestamp(c.ValidFrom)
		if err != nil {
			return schema("/args/valid_from")
		}
		if !kernel.Instant(from).Before(kernel.Instant(until)) {
			return invalid("/args/valid_until", "The validity must end after it starts")
		}
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// Validate checks the closed contract.
func (c RevokeRelease) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ReleaseID):
		return schema("/args/release_id")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// Receipt is the closed success answer of a command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// InspectionReceipt answers inspection.record and disposition.decide: the receipt (ObjectID the
// inspection, Status its state) and what the inspection binds. A record answered from the stored
// inspection (the same request by the same recorder under another key) is Replayed.
type InspectionReceipt struct {
	Receipt
	Subject                SubjectRef
	SupersedesInspectionID string
	InspectedQuantity      string
	ConformingQuantity     string
	NonconformingQuantity  string
	UnitID                 string
}

// ReleaseReceipt answers quality_release.grant and quality_release.revoke: the receipt (ObjectID
// the release, Status granted or revoked) and what the release binds. A grant answered from the
// stored release, and a revocation of a release revoked before, are Replayed.
type ReleaseReceipt struct {
	Receipt
	Subject    SubjectRef
	Quantity   string
	UnitID     string
	ValidFrom  time.Time
	ValidUntil time.Time
}

// SubjectBinding is a subject as a record binds it: what PUR said of it when the record was made —
// the commitment revision and content hash, the supplier, the order line it serves (order
// revision and hash), the SKU specification revision, the unit and the subject's quantity.
type SubjectBinding struct {
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
	Quantity      string
}

// SourceBinding is an EVD source as a record binds it.
type SourceBinding struct {
	SourceID      string
	ContentSHA256 string
}

// FileBinding is an EVD business file as a record binds it.
type FileBinding struct {
	FileID        string
	ContentSHA256 string
	MimeType      string
}

// InspectorView is who inspected: our recorder (ActorID) or an external agency.
type InspectorView struct {
	Kind    string
	ActorID string
	Agency  *AgencyRef
}

// DispositionView is the disposition of an inspection's nonconforming quantity.
type DispositionView struct {
	Items          []DispositionItem
	Reason         string
	EvidenceIDs    []string
	DecidedBy      kernel.ActorRef
	DecidedAt      time.Time
	AuthorityEpoch int64
}

// InspectionView is an inspection as the reads show it.
type InspectionView struct {
	InspectionID           string
	Status                 string
	RowVersion             int64
	Subject                SubjectBinding
	InspectedQuantity      string
	ConformingQuantity     string
	NonconformingQuantity  string
	Sampling               Sampling
	Standard               SourceBinding
	Inspector              InspectorView
	Report                 FileBinding
	InspectedAt            time.Time
	SupersedesInspectionID string
	Reason                 string
	EvidenceIDs            []string
	SupersededBy           string
	SupersededAt           time.Time
	RecordedBy             kernel.ActorRef
	RecordedAt             time.Time
	AuthorityEpoch         int64
	Disposition            *DispositionView
}

// RevocationView is the revocation of a release.
type RevocationView struct {
	Reason         string
	EvidenceIDs    []string
	RevokedBy      kernel.ActorRef
	RevokedAt      time.Time
	AuthorityEpoch int64
}

// ReleaseView is a release as the reads show it: State is decided at CheckedAt (granted,
// not_yet_valid, expired or revoked); Conforming and ReleasedBefore are the quantities it was
// judged on, InspectionIDs the valid inspections they came from.
type ReleaseView struct {
	ReleaseID      string
	Status         string
	State          string
	RowVersion     int64
	Subject        SubjectBinding
	Quantity       string
	ValidFrom      time.Time
	ValidUntil     time.Time
	Conforming     string
	ReleasedBefore string
	InspectionIDs  []string
	EvidenceIDs    []string
	GrantedBy      kernel.ActorRef
	GrantedAt      time.Time
	AuthorityEpoch int64
	Revocation     *RevocationView
	CheckedAt      time.Time
}

// SubjectInspection is one inspection of a subject. InspectionID is empty for one the reader does
// not see; Counted says whether its result counts for the subject's specification now.
type SubjectInspection struct {
	InspectionID string
	Status       string
	Inspected    string
	Conforming   string
	Counted      bool
}

// SubjectRelease is one release of a subject. ReleaseID is empty for one the reader does not see.
type SubjectRelease struct {
	ReleaseID string
	Status    string
	State     string
	Quantity  string
	Counted   bool
}

// SubjectView is a subject's quality standing now: PUR's subject (its specification and unit), the
// quantity its valid inspections cover and find conforming at that specification, what was
// released (revoked releases included), what may still be released, and the records.
type SubjectView struct {
	Subject     SubjectBinding
	Inspected   string
	Conforming  string
	Released    string
	Releasable  string
	Inspections []SubjectInspection
	Releases    []SubjectRelease
	CheckedAt   time.Time
}

// Commands is QLT's command entry point. Errors are *kernel.Rejection for refusals. An error that
// is application.ErrCommitUncertain from IDP-Bento-Go means the outcome is unknown: the caller sends
// the same command with the same idempotency key again, or reconciles it.
type Commands interface {
	RecordInspection(ctx context.Context, cmd RecordInspection) (InspectionReceipt, error)
	DecideDisposition(ctx context.Context, cmd DecideDisposition) (InspectionReceipt, error)
	GrantRelease(ctx context.Context, cmd GrantRelease) (ReleaseReceipt, error)
	RevokeRelease(ctx context.Context, cmd RevokeRelease) (ReleaseReceipt, error)
}

// Queries is QLT's audited read side.
type Queries interface {
	// GetInspection returns an inspection the caller sees, audited as inspection.get (qlt.read):
	// NOT_FOUND otherwise; REFERENCE_INVALID when the caller no longer reads its subject in PUR or
	// a source it cites in EVD.
	GetInspection(ctx context.Context, inspectionID string) (InspectionView, error)
	// GetRelease returns a release the caller sees, audited as quality_release.get (qlt.read):
	// NOT_FOUND otherwise; REFERENCE_INVALID when the caller no longer reads its subject in PUR.
	GetRelease(ctx context.Context, releaseID string) (ReleaseView, error)
	// GetSubject returns a subject's quality standing, audited as inspection_subject.get (qlt.read):
	// NOT_FOUND when PUR does not show the caller the subject.
	GetSubject(ctx context.Context, ref SubjectRef) (SubjectView, error)
	// Audit returns QLT's audit trail (qlt.audit).
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

// Auditor records a request an inbound adapter refused before it became a command.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Reconciliation is what became of a command whose answer the caller lost.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string
	ObjectID string
	Receipt  *Receipt
}

// Reconciler recovers from an unknown outcome without executing anything (contract 06, 动作对账).
// Reconciling needs qlt.read, not the command's own permission.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}
