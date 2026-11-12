// Package certificate holds CNX's CertificateResult (tactical model CNX-AGG-CertificateResult with
// its value objects CertificateKind, Issuer, ApplicableObject, Market and ValidityInterval;
// CNX-INV-007; MD-40; baseline CN-R14, CN-AT14; WBS S5-10 CNX half): an official result an external
// authority issued — a certificate of origin, a copy of an export licence, an inspection certificate
// — as an authorized human registered it after verifying the original, and whether it applies to an
// FUL shipment or export lot (CertificateResultRef v1, Context Map R56).
//
// The certificate is the root; its revisions are immutable (只追加；更正形成新版本). A registration is
// revision 1; a correction is the next revision, stating the whole certificate again with a reason;
// the earlier revisions stay as they were registered. The root's kind is fixed by the first
// registration: a correction corrects what the certificate states, not what it is.
//
// A certificate applies to a use (CNX-INV-007, MD-40 "适用对象、商品、批次、市场与有效期明确"; "另一规格、
// 市场或已过期的证书不能复用") only when it is of the kind the use needs, its market is the destination
// of the shipment or export lot it is used for, the instant of the use lies in its validity, and it
// covers every line of the target: each line's shipment, SKU and specification revision is among the
// SKU specifications it names, or among the lines of the shipment or export lot it was registered for.
// Brant records what the authority issued and the human verified; it judges no origin criterion and
// gives no legal opinion (GTC-05: preferential origin is outside the first release).
package certificate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The domain's own codes (error catalog V0.1, both 409): the certificate's object, market or validity
// is not the use's; the revision asked is superseded by a correction.
const (
	CodeCertificateNotApplicable = "CERTIFICATE_NOT_APPLICABLE"
	CodeReferenceStale           = "REFERENCE_STALE"
)

// Categories are the categories of the domain's own codes.
var Categories = map[string]kernel.Category{CodeCertificateNotApplicable: kernel.CategoryConflict, CodeReferenceStale: kernel.CategoryConflict}

// Kinds, objects, the root's status, states of a revision at an instant and the reasons of a verdict.
const (
	KindOrigin      = "certificate_of_origin"
	KindLicenseCopy = "license_copy"
	KindInspection  = "inspection_certificate"

	ObjectSkuSpec   = "sku_spec"
	ObjectExportLot = "export_lot"
	ObjectShipment  = "shipment"

	Registered = "registered"

	Valid       = "valid"
	Expired     = "expired"
	NotYetValid = "not_yet_valid"

	ReasonApplicable    = "applicable"
	ReasonUnknown       = "unknown"        // no such certificate, or no such revision
	ReasonTargetUnknown = "target_unknown" // FUL shows no such shipment or export lot, or not an intact one
	ReasonSuperseded    = "superseded"     // a correction superseded the revision asked
	ReasonKind          = "kind"
	ReasonMarket        = "market"
	ReasonMarketUnknown = "market_unknown" // the target's destination is not known
	ReasonNotYetValid   = "not_yet_valid"
	ReasonExpired       = "expired"
	ReasonGoods         = "goods" // a line of the target is of another specification, shipment or lot
)

// Kinds are the certificate kinds.
var Kinds = []string{KindOrigin, KindLicenseCopy, KindInspection}

// Spec is a SKU specification revision.
type Spec struct {
	SkuID        string `json:"sku_id"`
	SpecRevision int64  `json:"spec_revision"`
}

// Line is a line of a shipment or export lot: the shipment it ships with, its line ID (FUL's shipment
// line, or the export lot's line), SKU specification revision, quantity and unit.
type Line struct {
	ShipmentID   string `json:"shipment_id"`
	LineID       string `json:"line_id"`
	SkuID        string `json:"sku_id"`
	SpecRevision int64  `json:"spec_revision"`
	Quantity     string `json:"quantity,omitempty"`
	Unit         string `json:"unit,omitempty"`
}

// Object is what a certificate applies to: SKU specification revisions (Specs), or an FUL export lot
// or shipment (ID) with what CNX read of it at the registration — its row version, the order revision
// it ships and its lines.
type Object struct {
	Kind          string `json:"kind"`
	ID            string `json:"id,omitempty"`
	Version       int64  `json:"version,omitempty"`
	OrderID       string `json:"order_id,omitempty"`
	OrderRevision int64  `json:"order_revision,omitempty"`
	Specs         []Spec `json:"sku_specs,omitempty"`
	Lines         []Line `json:"lines,omitempty"`
}

// covers reports whether the object covers line l of a target.
func (o Object) covers(l Line) bool {
	if o.Kind == ObjectSkuSpec {
		return slices.Contains(o.Specs, Spec{SkuID: l.SkuID, SpecRevision: l.SpecRevision})
	}
	return slices.ContainsFunc(o.Lines, func(c Line) bool {
		return c.ShipmentID == l.ShipmentID && c.SkuID == l.SkuID && c.SpecRevision == l.SpecRevision
	})
}

// Evidence is an EVD source or file and its content hash.
type Evidence struct {
	SourceID      string `json:"source_id"`
	ContentSHA256 string `json:"content_sha256"`
}

// Actor is who registered a revision.
type Actor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Content is what a revision states of the certificate: what the authority issued and how the
// registering human verified the original (Basis, Evidence).
type Content struct {
	Kind       string     `json:"kind"`
	Number     string     `json:"number"`
	Issuer     string     `json:"issuer"`
	IssuedOn   string     `json:"issued_on"`
	Object     Object     `json:"object"`
	Market     string     `json:"market"`
	ValidFrom  string     `json:"valid_from"`
	ValidUntil string     `json:"valid_until"`
	File       Evidence   `json:"file"`
	LicenseID  string     `json:"license_id,omitempty"`
	Basis      string     `json:"basis"`
	Evidence   []Evidence `json:"evidence"`
}

// DocumentContract names a revision document's layout.
const DocumentContract = "brant.china-export.certificate-result/0.1.0"

// Document is a revision as stored: the certificate, the revision, what it states, why a correction
// was made, who verified and registered it under which authority epoch, when.
type Document struct {
	Contract      string `json:"contract"`
	CertificateID string `json:"certificate_id"`
	Revision      int    `json:"revision"`
	Content
	Supersedes     int    `json:"supersedes,omitempty"`
	Reason         string `json:"reason,omitempty"`
	VerifiedBy     Actor  `json:"verified_by"`
	AuthorityEpoch int64  `json:"authority_epoch"`
	RegisteredAt   string `json:"registered_at"`
}

// Revision is a stored revision: its document, bytes and SHA-256, and its validity.
type Revision struct {
	Document
	Bytes    []byte
	SHA256   string
	Validity kernel.ValidityInterval
}

// State is the revision's state at at, from its validity.
func (r *Revision) State(at time.Time) string {
	switch {
	case at.Before(r.Validity.From):
		return NotYetValid
	case r.Validity.Contains(at):
		return Valid
	}
	return Expired
}

// validity reads a document's validity [valid_from, valid_until).
func validity(d Document) (kernel.ValidityInterval, error) {
	from, err := kernel.ParseTimestamp(d.ValidFrom)
	if err != nil {
		return kernel.ValidityInterval{}, err
	}
	until, err := kernel.ParseTimestamp(d.ValidUntil)
	if err != nil {
		return kernel.ValidityInterval{}, err
	}
	v, err := kernel.NewValidityInterval(from, until)
	if err != nil || v.Until.IsZero() {
		return kernel.ValidityInterval{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/valid_until", "")
	}
	return v, nil
}

// Seal stores d as canonical JSON (fields in declaration order, no HTML escaping) with its SHA-256.
func Seal(d Document) (*Revision, error) {
	d.Contract = DocumentContract
	if d.Evidence == nil {
		d.Evidence = []Evidence{}
	}
	v, err := validity(d)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(d); err != nil {
		return nil, err
	}
	body := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	sum := sha256.Sum256(body)
	return &Revision{Document: d, Bytes: body, SHA256: hex.EncodeToString(sum[:]), Validity: v}, nil
}

// Unseal reads a stored revision; false when it does not match its SHA-256.
func Unseal(b []byte, hash string) (*Revision, bool, error) {
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, false, nil
	}
	var d Document
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, false, err
	}
	v, err := validity(d)
	if err != nil {
		return nil, false, err
	}
	return &Revision{Document: d, Bytes: b, SHA256: hash, Validity: v}, true, nil
}

// Certificate is the CertificateResult root: its kind, the current revision's key (issuer and number)
// and the revision it stands on; RowVersion counts its revisions.
type Certificate struct {
	ID              string
	Kind            string
	Issuer          string
	Number          string
	CurrentRevision int
	RowVersion      int64
	RegisteredBy    kernel.ActorRef
	Epoch           int64
	RegisteredAt    time.Time
	UpdatedAt       time.Time
}

// Registration is a certificate.register as the application decided its references: the content
// with the object, file, evidence and licence CNX resolved, and for a correction the reason.
type Registration struct {
	Content Content
	Reason  string
	By      kernel.ActorRef
	Epoch   int64
	At      time.Time
}

// Open checks a registration against the certificate it corrects (expected 0 and c nil: a new
// certificate), in this order: a correction of a certificate the company lacks (NOT_FOUND), another row
// version (VERSION_CONFLICT), another kind than the certificate's (INVALID_KNOWN_VALUE).
func Open(c *Certificate, expected int64, kind string) error {
	switch {
	case expected == 0 && c == nil:
		return nil
	case c == nil:
		return kernel.Reject(kernel.CodeNotFound, "/args/certificate_id", "")
	case c.RowVersion != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case c.Kind != kind:
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/certificate_kind", "The certificate is a "+c.Kind+"; a correction keeps its kind")
	}
	return nil
}

// Register decides a registration (expected 0, c nil: a new certificate id) or a correction of c at
// row version expected whose current revision is current: Open's refusals, then a correction that
// states exactly what the current revision states (INVALID_KNOWN_VALUE: nothing to correct); only a
// human registers (PERMISSION_DENIED; the authorization policy refuses anyone else first). It returns
// the root as it will stand and the sealed revision.
func Register(c *Certificate, current *Revision, id string, expected int64, r Registration) (*Certificate, *Revision, error) {
	if r.By.Kind != kernel.ActorHuman {
		return nil, nil, kernel.Reject(kernel.CodePermissionDenied, "/actor", "Only a human registers a certificate result")
	}
	if err := Open(c, expected, r.Content.Kind); err != nil {
		return nil, nil, err
	}
	at := kernel.Instant(r.At)
	d := Document{CertificateID: id, Revision: 1, Content: r.Content, Reason: r.Reason, VerifiedBy: Actor{Kind: string(r.By.Kind), ID: r.By.ID},
		AuthorityEpoch: r.Epoch, RegisteredAt: kernel.FormatTimestamp(at)}
	out := &Certificate{ID: id, Kind: r.Content.Kind, Issuer: r.Content.Issuer, Number: r.Content.Number, CurrentRevision: 1, RowVersion: 1,
		RegisteredBy: r.By, Epoch: r.Epoch, RegisteredAt: at, UpdatedAt: at}
	if c != nil {
		if current != nil && same(current.Content, r.Content) {
			return nil, nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/certificate_id", "The correction states what revision "+
				strconv.Itoa(current.Revision)+" states")
		}
		d.Revision, d.Supersedes = c.CurrentRevision+1, c.CurrentRevision
		*out = *c
		out.Issuer, out.Number, out.CurrentRevision, out.RowVersion, out.UpdatedAt = r.Content.Issuer, r.Content.Number, d.Revision,
			c.RowVersion+1, at
	}
	rev, err := Seal(d)
	if err != nil {
		return nil, nil, err
	}
	return out, rev, nil
}

func same(a, b Content) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// Target is a shipment or export lot a certificate is used for, as CNX established it: its
// destination country ("" when not known) and its lines.
type Target struct {
	Kind        string
	ID          string
	Destination string
	Lines       []Line
}

// Use is what a certificate is asked to apply to: the kind the use needs, the target, the instant.
type Use struct {
	Kind   string
	Target Target
	At     time.Time
}

// Verdict is whether revision Revision applies to a use. Applicable and Reason judge the revision
// asked; Current reports whether it is the certificate's current revision (a superseded revision is
// REFERENCE_STALE whatever it says). Covered and Uncovered are the target's line IDs.
type Verdict struct {
	Applicable bool
	Reason     string
	Current    bool
	State      string
	Covered    []string
	Uncovered  []string
}

// Code is the verdict's refusal code: REFERENCE_STALE for a superseded revision,
// CERTIFICATE_NOT_APPLICABLE for one that does not apply, "" otherwise.
func (v Verdict) Code() string {
	switch {
	case !v.Current:
		return CodeReferenceStale
	case !v.Applicable:
		return CodeCertificateNotApplicable
	}
	return ""
}

// Judge decides whether revision r of a certificate whose current revision is current applies to
// use u (CNX-INV-007), in this order: another kind (kind), an unknown destination (market_unknown) or
// another one (market), the use's instant before the validity (not_yet_valid) or at or after its end
// (expired), a target line the certificate does not cover (goods). It always reports the coverage.
func Judge(r *Revision, current int, u Use) Verdict {
	v := Verdict{Current: r.Revision == current, State: r.State(u.At), Covered: []string{}, Uncovered: []string{}}
	for _, l := range u.Target.Lines {
		if r.Object.covers(l) {
			v.Covered = append(v.Covered, l.LineID)
		} else {
			v.Uncovered = append(v.Uncovered, l.LineID)
		}
	}
	switch {
	case r.Kind != u.Kind:
		v.Reason = ReasonKind
	case u.Target.Destination == "":
		v.Reason = ReasonMarketUnknown
	case r.Market != u.Target.Destination:
		v.Reason = ReasonMarket
	case v.State == NotYetValid:
		v.Reason = ReasonNotYetValid
	case v.State == Expired:
		v.Reason = ReasonExpired
	case len(u.Target.Lines) == 0 || len(v.Uncovered) > 0:
		v.Reason = ReasonGoods
	default:
		v.Applicable, v.Reason = true, ReasonApplicable
	}
	return v
}

// NotApplicable returns CERTIFICATE_NOT_APPLICABLE at path with the reason.
func NotApplicable(path, reason string) *kernel.Rejection {
	return kernel.RejectAs(CodeCertificateNotApplicable, Categories[CodeCertificateNotApplicable], path, reason)
}

// Explain is the human reason of a verdict's refusal for revision r and use u.
func Explain(r *Revision, u Use, v Verdict) string {
	switch v.Reason {
	case ReasonKind:
		return "The certificate is a " + r.Kind + ", not a " + u.Kind
	case ReasonMarketUnknown:
		return "The destination of the " + u.Target.Kind + " is not known, so the certificate's market " + r.Market + " cannot be confirmed"
	case ReasonMarket:
		return "The certificate is for market " + r.Market + ", the " + u.Target.Kind + " goes to " + u.Target.Destination
	case ReasonNotYetValid:
		return "The certificate is valid from " + r.ValidFrom
	case ReasonExpired:
		return "The certificate expired at " + r.ValidUntil
	case ReasonGoods:
		if len(u.Target.Lines) == 0 {
			return "The " + u.Target.Kind + " has no lines"
		}
		return "The certificate does not cover line " + v.Uncovered[0] + " of the " + u.Target.Kind + " (another specification, shipment or lot)"
	}
	return ""
}
