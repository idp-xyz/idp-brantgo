package api

import (
	"context"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Official certificate results (WBS S5-10 CNX half; tactical model CNX-AGG-CertificateResult,
// CNX-INV-007; MD-40; baseline CN-R14, CN-AT14; Context Map R56 CertificateResultRef v1; S5 decisions
// E-1, D-17). The certificate is the external authority's; CNX registers it as an authorized human
// verified the original, and answers whether it applies to a shipment or export lot.
//
//	certificate.register  human only (documentation): certificate_kind certificate_of_origin|
//	                      license_copy|inspection_certificate, number and issuer (the issuer's number of a kind names
//	                      one certificate of the company), issued_on, the object it applies to {kind
//	                      sku_spec with sku_specs, or export_lot|shipment with id — FUL's, read through
//	                      CNX's port (R36) and recorded with its lines}, the destination market (ISO
//	                      3166-1 alpha-2), valid_from and valid_until, file_id (the EVD file of the
//	                      certificate), for a licence copy optionally license_id (CNX's export licence, of
//	                      the same number), basis (how the original was verified) and evidence_ids. The
//	                      verifier is the registering human. certificate_id "" with expected_version 0
//	                      registers a new certificate (over HTTP certificate_id is omitted, expected_version
//	                      0 sent); certificate_id with the certificate's row version
//	                      and a reason registers a correction as its next revision: history is never
//	                      overwritten, and the kind never changes.
//
// Request members in snake_case: certificate.register {certificate_id, expected_version, certificate_kind, number,
// issuer, issued_on, object {kind, id, sku_specs [{sku_id, spec_revision}]}, market, valid_from,
// valid_until, file_id, license_id, basis, evidence_ids, reason}.
const (
	CommandRegisterCertificate = "certificate.register"

	// Reads: a certificate with every revision, the company's certificates, and what
	// CertificateResultRef v1 answers now for a certificate on a shipment or export lot.
	CommandGetCertificate           = "certificate.get"
	CommandCertificates             = "certificate.list"
	CommandCertificateApplicability = "certificate.applicability"

	// CertificateRegistered (Context Map CNX facts): every registration and correction.
	EventCertificateRegistered = "idp-brant.cnx.certificate_registered.v1"
)

// Certificate kinds (CertificateKind), objects (ApplicableObject) and the certificate's status.
const (
	CertificateOfOrigin    = "certificate_of_origin"
	CertificateLicenseCopy = "license_copy"
	CertificateInspection  = "inspection_certificate"

	ObjectSkuSpec   = "sku_spec"
	ObjectExportLot = "export_lot"
	ObjectShipment  = "shipment"

	CertificateRegistered = "registered"
)

// A certificate revision's state at an instant, from its validity: LicenseValid, LicenseExpired and
// LicenseNotYetValid ("valid", "expired", "not_yet_valid").

// Reasons of CertificateResultRef v1's verdict.
const (
	CertificateApplicable    = "applicable"
	CertificateUnknown       = "unknown"        // REFERENCE_INVALID: no such certificate or revision in the caller's company
	CertificateTargetUnknown = "target_unknown" // REFERENCE_INVALID: FUL shows no such shipment or export lot, or not an intact one
	CertificateSuperseded    = "superseded"     // REFERENCE_STALE: a correction superseded the revision asked
	CertificateKindMismatch  = "kind"           // CERTIFICATE_NOT_APPLICABLE: not the kind the use needs
	CertificateMarket        = "market"         // CERTIFICATE_NOT_APPLICABLE: another destination market
	CertificateMarketUnknown = "market_unknown" // CERTIFICATE_NOT_APPLICABLE: the target's destination is not known
	CertificateNotYetValid   = "not_yet_valid"  // CERTIFICATE_NOT_APPLICABLE: before its validity
	CertificateExpired       = "expired"        // CERTIFICATE_NOT_APPLICABLE: at or after its validity's end
	CertificateGoods         = "goods"          // CERTIFICATE_NOT_APPLICABLE: a target line of another specification, shipment or lot
)

// Limits of the certificate contract.
const (
	MaxCertificateNumber  = 64
	MaxCertificateIssuer  = 200
	MaxCertificateSpecs   = 100
	MaxCertificateLookups = 100
	MaxCertificates       = 20 // certificates one bundle requirement names
)

var certificateKinds = []string{CertificateOfOrigin, CertificateLicenseCopy, CertificateInspection}

// CertificateObject is what a certificate applies to: SkuSpecs (kind sku_spec), or the FUL export lot
// or shipment ID (kinds export_lot, shipment).
type CertificateObject struct {
	Kind     string
	ID       string
	SkuSpecs []SpecRef
}

// RegisterCertificate registers an official certificate result, or a correction of one (human only).
type RegisterCertificate struct {
	IdempotencyKey  string
	CertificateID   string // "" registers a new certificate
	ExpectedVersion int64  // 0 for a new certificate; the certificate's row version for a correction
	CertificateKind string
	Number          string
	Issuer          string
	IssuedOn        string // YYYY-MM-DD
	Object          CertificateObject
	Market          string
	ValidFrom       string
	ValidUntil      string
	FileID          string
	LicenseID       string // license_copy only, optional
	Basis           string
	EvidenceIDs     []string
	Reason          string // a correction's reason; omitted for a new certificate
}

// Validate checks the closed contract.
func (c RegisterCertificate) Validate() error {
	correction := c.CertificateID != ""
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case correction && !kernel.ValidIdentifier(c.CertificateID):
		return schema("/args/certificate_id")
	case !correction && c.ExpectedVersion != 0, correction && (c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion):
		return schema("/args/expected_version")
	case !contains(certificateKinds, c.CertificateKind):
		return schema("/args/certificate_kind")
	case !Text(c.Number, MaxCertificateNumber):
		return schema("/args/number")
	case !Text(c.Issuer, MaxCertificateIssuer):
		return schema("/args/issuer")
	case !date(c.IssuedOn):
		return schema("/args/issued_on")
	}
	if err := c.Object.validate("/args/object"); err != nil {
		return err
	}
	switch {
	case !Country(c.Market):
		return schema("/args/market")
	case !timestamp(c.ValidFrom):
		return schema("/args/valid_from")
	case !timestamp(c.ValidUntil):
		return schema("/args/valid_until")
	case !kernel.ValidIdentifier(c.FileID):
		return schema("/args/file_id")
	case c.LicenseID != "" && (c.CertificateKind != CertificateLicenseCopy || !kernel.ValidIdentifier(c.LicenseID)):
		return schema("/args/license_id")
	case !Text(c.Basis, MaxRequirementText):
		return schema("/args/basis")
	case correction != (c.Reason != ""), c.Reason != "" && !Text(c.Reason, MaxReason):
		return schema("/args/reason")
	}
	from, _ := kernel.ParseTimestamp(c.ValidFrom)
	until, _ := kernel.ParseTimestamp(c.ValidUntil)
	if !until.After(from) {
		return schema("/args/valid_until")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 0)
}

func (o CertificateObject) validate(path string) error {
	switch o.Kind {
	case ObjectSkuSpec:
		if o.ID != "" {
			return schema(path + "/id")
		}
		if len(o.SkuSpecs) < 1 || len(o.SkuSpecs) > MaxCertificateSpecs {
			return schema(path + "/sku_specs")
		}
		for i, s := range o.SkuSpecs {
			p := path + "/sku_specs/" + strconv.Itoa(i)
			switch {
			case !kernel.ValidIdentifier(s.SkuID):
				return schema(p + "/sku_id")
			case s.SpecRevision < 1 || s.SpecRevision > maxVersion || slices.Index(o.SkuSpecs, s) != i:
				return schema(p + "/spec_revision")
			}
		}
	case ObjectExportLot, ObjectShipment:
		if !kernel.ValidIdentifier(o.ID) {
			return schema(path + "/id")
		}
		if len(o.SkuSpecs) > 0 {
			return schema(path + "/sku_specs")
		}
	default:
		return schema(path + "/kind")
	}
	return nil
}

func (RegisterCertificate) cnxCommand() {}

// CertificateReceipt answers certificate.register: the receipt (ObjectID the certificate ID, RowVersion
// its row version, Status registered) with the revision the command registered and its document's
// SHA-256, as stored.
type CertificateReceipt struct {
	Receipt
	CertificateID string
	Revision      int64
	ContentSHA256 string
}

// CertificateCommands is CNX's command entry point for certificate results.
type CertificateCommands interface {
	RegisterCertificate(ctx context.Context, cmd RegisterCertificate) (CertificateReceipt, error)
}

// CertificateResultRef v1 (Context Map R56 CNX→FUL; CNX-INV-007, MD-40) is how a consumer asks whether
// a certificate CNX registered applies to its use: FUL's certificate of origin record of a shipment
// (shipment_document.prepare, S5-08), CNX's own document bundles. It is a lookup the caller runs before
// its own transaction, with an active grant only (no read permission; not audited): a certificate's
// revisions never change, so what it answers of a revision stays true; a correction committed later
// makes that revision superseded, which the next lookup reports (REFERENCE_STALE).
//
// ResolveCertificates answers every use, in order. A use names the certificate (Version 0: its current
// revision), the kind the use needs, the target (an FUL shipment or export lot) and the instant. CNX
// reads the target through its port to FUL (R36, FUL's data scope: the caller must see it; FUL's
// shipment read needs ful.read), its destination from the consignee's PTY address country or else from
// the trade compliance evaluation of the order revision it ships (S5 D-8), and judges the revision:
//
//	Code ""                          applicable: of the kind, its market the destination, the instant in
//	                                 its validity, every target line covered (Reason applicable)
//	Code REFERENCE_INVALID           no such certificate or revision (unknown), no such target (target_unknown)
//	Code REFERENCE_STALE             the revision asked is superseded by a correction (superseded); its
//	                                 content is answered as registered
//	Code CERTIFICATE_NOT_APPLICABLE  Reason kind, market_unknown, market, not_yet_valid, expired or goods
//
// A caller refuses with Check and records Evidence:
//
//	results, err := cnx.ResolveCertificates(ctx, []cnxapi.CertificateUse{{Ref: ref, Kind: cnxapi.CertificateOfOrigin,
//		Target: cnxapi.ExportTarget{Kind: cnxapi.TargetShipment, ID: shipmentID}, At: now}})
//	if err == nil {
//		err = results[0].Check("/args/certificate/certificate_ref")
//	}
type CertificateLookups interface {
	ResolveCertificates(ctx context.Context, uses []CertificateUse) ([]CertificateResult, error)
}

// CertificateResultRefName is the lookup's name in a caller's commit evidence or sources.
const CertificateResultRefName = "cnx.certificate_result_ref.v1"

// CertificateRef names a certificate result: its ID and revision (0: the current one).
type CertificateRef struct {
	CertificateID string
	Version       int64
}

// CertificateUse is what a certificate is asked to apply to.
type CertificateUse struct {
	Ref    CertificateRef
	Kind   string
	Target ExportTarget
	At     time.Time
}

// Validate checks a use at path.
func (u CertificateUse) Validate(path string) error {
	switch {
	case !kernel.ValidIdentifier(u.Ref.CertificateID):
		return schema(path + "/ref/certificate_id")
	case u.Ref.Version < 0 || u.Ref.Version > maxVersion:
		return schema(path + "/ref/version")
	case !contains(certificateKinds, u.Kind):
		return schema(path + "/kind")
	case u.Target.Kind != TargetShipment && u.Target.Kind != TargetExportLot:
		return schema(path + "/target/kind")
	case !kernel.ValidIdentifier(u.Target.ID):
		return schema(path + "/target/id")
	case u.At.IsZero():
		return schema(path + "/at")
	}
	return nil
}

// CertificateResult is CertificateResultRef v1's answer for Use: the code and reason, and — unless the
// certificate or revision is unknown — what the revision judged states (Revision; CurrentRevision is
// the certificate's current one), its state at the use's instant, the target's destination as CNX
// established it ("" when not known) and which target lines it covers.
type CertificateResult struct {
	Use             CertificateUse
	Code            string
	Reason          string
	Applicable      bool
	CertificateID   string
	Revision        int64
	CurrentRevision int64
	ContentSHA256   string
	Kind            string
	Number          string
	Issuer          string
	IssuedOn        string
	Market          string
	ValidFrom       time.Time
	ValidUntil      time.Time
	State           string
	ObjectKind      string
	ObjectID        string
	FileID          string
	FileSHA256      string
	VerifiedBy      kernel.ActorRef
	RegisteredAt    time.Time
	Destination     string
	Covered         []string
	Uncovered       []string
}

// Check returns nil for an applicable current revision and the result's code at path otherwise:
// REFERENCE_INVALID, REFERENCE_STALE or CERTIFICATE_NOT_APPLICABLE (409), with the reason.
func (r CertificateResult) Check(path string) error {
	switch r.Code {
	case "":
		return nil
	case CodeCertificateNotApplicable, CodeReferenceStale:
		return Reject(r.Code, path, "Certificate "+r.Use.Ref.CertificateID+" "+r.Reason)
	}
	return kernel.Reject(r.Code, path, "Certificate "+r.Use.Ref.CertificateID+" "+r.Reason)
}

// Evidence is the value a caller records under CertificateResultRefName:
// certificate_id/v<revision>/<content sha256>/<reason>/<market>/<valid_until>.
func (r CertificateResult) Evidence() string {
	until := ""
	if !r.ValidUntil.IsZero() {
		until = kernel.FormatTimestamp(r.ValidUntil)
	}
	return r.CertificateID + "/v" + strconv.FormatInt(r.Revision, 10) + "/" + r.ContentSHA256 + "/" + r.Reason + "/" + r.Market + "/" + until
}

// CertificateLineView is a line of the shipment or export lot a certificate was registered for.
type CertificateLineView struct {
	ShipmentID   string
	LineID       string
	SkuID        string
	SpecRevision int64
	Quantity     string
	Unit         string
}

// CertificateObjectView is what a revision applies to, with what CNX read of a shipment or export lot.
type CertificateObjectView struct {
	Kind          string
	ID            string
	Version       int64
	OrderID       string
	OrderRevision int64
	SkuSpecs      []SpecRef
	Lines         []CertificateLineView
}

// CertificateRevisionView is a revision as registered, with its state as of the read.
type CertificateRevisionView struct {
	Revision       int64
	Kind           string
	Number         string
	Issuer         string
	IssuedOn       string
	Object         CertificateObjectView
	Market         string
	ValidFrom      time.Time
	ValidUntil     time.Time
	State          string
	File           EvidenceView
	LicenseID      string
	Basis          string
	Evidence       []EvidenceView
	Supersedes     int64
	Reason         string
	DocumentSHA256 string
	VerifiedBy     kernel.ActorRef
	AuthorityEpoch int64
	RegisteredAt   time.Time
}

// CertificateView is a certificate with every revision, oldest first; State is the current
// revision's as of CheckedAt.
type CertificateView struct {
	CertificateID   string
	Kind            string
	Status          string
	CurrentRevision int64
	RowVersion      int64
	State           string
	Revisions       []CertificateRevisionView
	RegisteredBy    kernel.ActorRef
	RegisteredAt    time.Time
	UpdatedAt       time.Time
	CheckedAt       time.Time
}

// CertificateSummary is one certificate of the company, its current revision as of CheckedAt.
type CertificateSummary struct {
	CertificateID   string
	Kind            string
	Number          string
	Issuer          string
	Market          string
	ObjectKind      string
	ObjectID        string
	ValidFrom       time.Time
	ValidUntil      time.Time
	State           string
	CurrentRevision int64
	RowVersion      int64
	CheckedAt       time.Time
}

// CertificateApplicabilityView is what CertificateResultRef v1 answers now for a certificate's current
// revision, of its own kind, on a shipment or export lot.
type CertificateApplicabilityView struct {
	CertificateResult
	CheckedAt time.Time
}

// CertificateQueries is the audited read of certificate results (cnx.read; certificates are
// company-wide, like licences).
type CertificateQueries interface {
	// GetCertificate returns a certificate with every revision, audited as certificate.get: NOT_FOUND
	// otherwise; a revision that does not match its hash is REFERENCE_INVALID.
	GetCertificate(ctx context.Context, certificateID string) (CertificateView, error)
	// Certificates returns the company's certificates, newest first, audited as certificate.list.
	Certificates(ctx context.Context) ([]CertificateSummary, error)
	// CertificateApplicability returns what CertificateResultRef v1 answers now for the certificate's
	// current revision on target, audited as certificate.applicability: NOT_FOUND for a certificate the
	// company lacks.
	CertificateApplicability(ctx context.Context, certificateID string, target ExportTarget) (CertificateApplicabilityView, error)
}

// certificates checks a requirement's certificate_ids at path: only for the certificate kinds of
// requirements, none when missing, 1 to MaxCertificates distinct identifiers.
func (r Requirement) certificates(path string) error {
	if len(r.CertificateIDs) == 0 {
		return nil
	}
	switch {
	case r.Kind != RequirementCertificateOfOrigin && r.Kind != RequirementInspectionCertificate && r.Kind != RequirementExportLicense,
		r.Status == RequirementMissing, len(r.CertificateIDs) > MaxCertificates:
		return schema(path)
	}
	seen := map[string]bool{}
	for i, id := range r.CertificateIDs {
		if !kernel.ValidIdentifier(id) || seen[id] {
			return schema(path + "/" + strconv.Itoa(i))
		}
		seen[id] = true
	}
	return nil
}

// CertificateLink is a certificate result a requirement names, as CNX held it at the check: the
// revision and its document's SHA-256, what it states, its state at the check and the lot lines it
// covers.
type CertificateLink struct {
	CertificateID  string
	Revision       int64
	DocumentSHA256 string
	Kind           string
	Number         string
	Issuer         string
	Market         string
	ValidFrom      string
	ValidUntil     string
	State          string
	Covered        []string
}
