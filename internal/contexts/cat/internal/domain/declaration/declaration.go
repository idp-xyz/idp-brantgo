// Package declaration is the DeclarationProfile aggregate (CAT-AGG-DeclarationProfile, UL
// CAT-DeclarationProfile, baseline 22.1 CNProductDeclarationProfile, fact MD-41): the verified
// profile of what one SKU specification revision is for customs, kept per commodity code table
// version (S5 D-22). A profile is identified by its profile ID; its key (SKU, specification
// revision, code table version) is fixed by its first save and unique in a company. Its content
// lives in revisions with monotonic numbers and a root version every command compares and
// increments (SM-CAT-DECLARATION):
//
//	draft    → verified    declaration_profile.verify: a human other than the revision's last editor
//	verified → superseded  declaration_profile.verify of the profile's next revision, in the same transaction
//
// One draft revision is saved whole until it is verified, by a human or by an AI agent: a draft is
// a candidate and is never used (CAT-INV-002). Updating a verified profile saves its next revision
// as a new draft; the verified revision stays in use until that draft is verified, and a verified
// or superseded revision never changes, so a declaration that adopted it keeps what it adopted.
//
// The classification parts are kept separately (D-22, tactical notes: 商品编码、出口管制分类与原产地证据
// 分别保存): the commodity code with its basis and sources, the declaration name and elements, the
// legal units with their conversion from the SKU's base unit, the origin basis, and the export
// control classification (not controlled, or a control list reference), each with its own basis.
// Checks run in the order of the other CAT aggregates: existence, root version, key, state,
// independence, evidence, SKU reference, business key.
package declaration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the error catalog V0.1 this aggregate decides beyond the shared ones; package api
// declares the same codes for callers.
const (
	CodeBusinessKeyConflict          = "BUSINESS_KEY_CONFLICT"          // 409: another profile has the key
	CodeIndependentReviewRequired    = "INDEPENDENT_REVIEW_REQUIRED"    // 422: the verifier last edited the revision
	CodeDeclarationProfileUnverified = "DECLARATION_PROFILE_UNVERIFIED" // 422: no verified profile answers the reference
	CodeReferenceStale               = "REFERENCE_STALE"                // 409: the revision was verified and is superseded
)

// Categories are the categories the error catalog gives the codes above.
var Categories = map[string]kernel.Category{
	CodeBusinessKeyConflict:          kernel.CategoryConflict,
	CodeIndependentReviewRequired:    kernel.CategoryInvalid,
	CodeDeclarationProfileUnverified: kernel.CategoryInvalid,
	CodeReferenceStale:               kernel.CategoryConflict,
}

// Reject returns a rejection with one of the codes above.
func Reject(code, path, reason string) *kernel.Rejection {
	return kernel.RejectAs(code, Categories[code], path, reason)
}

// Status of a revision.
type Status string

const (
	Draft      Status = "draft"
	Verified   Status = "verified"
	Superseded Status = "superseded"
)

// Export control classifications.
const (
	NotControlled = "not_controlled"
	Controlled    = "controlled"
)

// Origin criteria of the non-preferential rules of origin.
const (
	WhollyObtained            = "wholly_obtained"
	SubstantialTransformation = "substantial_transformation"
)

// Key is what a profile is kept for (D-22): one confirmed specification revision of a SKU under one
// version of the commodity code table.
type Key struct {
	SkuID            string
	SpecRevision     int64
	CodeTableVersion string
}

// Commodity is the commodity code (商品编码) in the key's code table version, with the basis of the
// classification and the EVD sources it rests on.
type Commodity struct {
	Code      string   `json:"code"`
	Basis     string   `json:"basis"`
	SourceIDs []string `json:"source_ids"`
}

// Element is one declaration element (申报要素), in the order the code table lists them.
type Element struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// LegalUnit is a legal unit of quantity (法定单位) and its conversion from the SKU's base unit: one
// base unit is PerBaseUnit legal units (a plain decimal string, exact), on the stated basis.
type LegalUnit struct {
	Unit        string `json:"unit"`
	PerBaseUnit string `json:"per_base_unit"`
	Basis       string `json:"basis"`
}

// Origin is the origin basis (原产地依据): the country of origin, the criterion it meets, the basis
// and its sources.
type Origin struct {
	Country   string   `json:"country"`
	Criterion string   `json:"criterion"`
	Basis     string   `json:"basis"`
	SourceIDs []string `json:"source_ids"`
}

// ExportControl is the export control classification: NotControlled, or Controlled with the control
// list entry it falls under, the basis and its sources. It is a classification, never a compliance
// conclusion: CNX decides trade compliance (CNX-INV-001).
type ExportControl struct {
	Status        string   `json:"status"`
	ListReference string   `json:"list_reference,omitempty"`
	Basis         string   `json:"basis"`
	SourceIDs     []string `json:"source_ids"`
}

// Content is the whole content of a revision.
type Content struct {
	Commodity       Commodity     `json:"commodity"`
	DeclarationName string        `json:"declaration_name"`
	Elements        []Element     `json:"elements"`
	LegalUnits      []LegalUnit   `json:"legal_units"`
	Origin          Origin        `json:"origin"`
	ExportControl   ExportControl `json:"export_control"`
}

// Normalize returns c in canonical form: source IDs sorted and unique, absent lists empty, each
// conversion factor in its shortest exact decimal form (1.50 is 1.5). Elements keep their order.
// The caller has checked the form of c (package api).
func Normalize(c Content) Content {
	sources := func(ids []string) []string {
		out := slices.Compact(slices.Sorted(slices.Values(ids)))
		if out == nil {
			out = []string{}
		}
		return out
	}
	c.Commodity.SourceIDs = sources(c.Commodity.SourceIDs)
	c.Origin.SourceIDs = sources(c.Origin.SourceIDs)
	c.ExportControl.SourceIDs = sources(c.ExportControl.SourceIDs)
	c.Elements = slices.Clone(c.Elements)
	if c.Elements == nil {
		c.Elements = []Element{}
	}
	c.LegalUnits = slices.Clone(c.LegalUnits)
	if c.LegalUnits == nil {
		c.LegalUnits = []LegalUnit{}
	}
	for i, u := range c.LegalUnits {
		if d, err := kernel.ParseDecimal(u.PerBaseUnit, kernel.FactorFormat); err == nil {
			if exact, ok := d.Exact(); ok {
				c.LegalUnits[i].PerBaseUnit = exact
			}
		}
	}
	return c
}

// SourceIDs returns every source the content cites, sorted and unique.
func (c Content) SourceIDs() []string {
	all := slices.Concat(c.Commodity.SourceIDs, c.Origin.SourceIDs, c.ExportControl.SourceIDs)
	return slices.Compact(slices.Sorted(slices.Values(all)))
}

// Canonical returns the canonical JSON of the content: the normalized content with the members in
// the order of the Go types.
func (c Content) Canonical() []byte {
	doc, err := json.Marshal(Normalize(c))
	if err != nil {
		panic("declaration: content does not marshal: " + err.Error()) // only strings and slices of them
	}
	return doc
}

// Hash returns the content hash: the SHA-256 of the canonical JSON, so equal content always hashes
// equally.
func (c Content) Hash() string {
	sum := sha256.Sum256(c.Canonical())
	return hex.EncodeToString(sum[:])
}

// Verification is the independent professional verification that made a revision usable.
type Verification struct {
	By          kernel.ActorRef
	Epoch       int64 // the authority epoch the verification saw
	At          time.Time
	EvidenceIDs []string
}

// Revision is one revision of a profile. SavedBy is its last editor.
type Revision struct {
	Number       int64
	Status       Status
	Content      Content
	Hash         string
	SavedBy      kernel.ActorRef
	SavedEpoch   int64
	SavedAt      time.Time
	Verification *Verification // nil until verified
	SupersededAt time.Time     // zero until superseded
}

// Root is the aggregate root. Version is the root version every command compares and increments;
// LastRevision the highest revision number ever assigned; Draft and Verified the revisions in those
// states (at most one each).
type Root struct {
	ID           string
	Key          Key
	Version      int64
	LastRevision int64
	Draft        *Revision
	Verified     *Revision
}

// New returns a profile of key that is not stored yet.
func New(id string, key Key) *Root { return &Root{ID: id, Key: key} }

// CheckSave admits a whole-revision save: a new profile only at expected version 0, an existing one
// only at its current root version and for its own key.
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
	case key.SkuID != r.Key.SkuID:
		return fixed("/args/sku_id")
	case key.SpecRevision != r.Key.SpecRevision:
		return fixed("/args/spec_revision")
	case key.CodeTableVersion != r.Key.CodeTableVersion:
		return fixed("/args/code_table_version")
	}
	return nil
}

func fixed(path string) error {
	return kernel.Reject(kernel.CodeInvalidKnownValue, path,
		"A declaration profile keeps the SKU specification revision and code table version of its first save")
}

// Check admits a command on an existing profile at its current root version.
func Check(r *Root, expected int64) error {
	switch {
	case r == nil:
		return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case r.Version != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// CheckKey refuses creating a profile while another profile of the company has the same key (D-22:
// one profile per SKU specification revision and code table version).
func CheckKey(taken bool) error {
	if taken {
		return Reject(CodeBusinessKeyConflict, "/args/id",
			"Another declaration profile is kept for this SKU specification revision and code table version")
	}
	return nil
}

// Save replaces the draft revision whole, or starts one with the next revision number, made by its
// last editor by. It reports whether the revision is new. A verified revision is never touched.
func (r *Root) Save(c Content, by kernel.ActorRef, epoch int64, at time.Time) (*Revision, bool) {
	created := r.Draft == nil
	if created {
		r.LastRevision++
		r.Draft = &Revision{Number: r.LastRevision, Status: Draft}
	}
	d := r.Draft
	d.Content = Normalize(c)
	d.Hash, d.SavedBy, d.SavedEpoch, d.SavedAt = d.Content.Hash(), by, epoch, at
	r.Version++
	return d, created
}

// CheckVerify admits verifying revision number by the human by: NOT_FOUND for a number never
// assigned, INVALID_STATE unless it is the draft revision, INDEPENDENT_REVIEW_REQUIRED when by is
// its last editor (CAT-INV-002, role templates' independence row: 独立于该修订的最后编辑人). It
// returns the draft. The caller checks the evidence and the SKU reference next, then calls Verify.
func (r *Root) CheckVerify(number int64, by kernel.ActorRef) (*Revision, error) {
	d := r.Draft
	switch {
	case number > r.LastRevision:
		return nil, kernel.Reject(kernel.CodeNotFound, "/args/revision", "")
	case d == nil || d.Number != number:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/revision", "")
	case d.SavedBy.ID == by.ID:
		return nil, Reject(CodeIndependentReviewRequired, "/args/revision", "The verifier last edited this revision")
	}
	return d, nil
}

// Verify makes the draft revision the verified one for the human by, citing the evidence verified,
// and supersedes the verified revision, which it returns (nil when there was none). Call it after
// CheckVerify and the reference checks.
func (r *Root) Verify(by kernel.ActorRef, epoch int64, evidence []string, at time.Time) *Revision {
	superseded := r.Verified
	if superseded != nil {
		superseded.Status, superseded.SupersededAt = Superseded, at
	}
	d := r.Draft
	d.Status = Verified
	d.Verification = &Verification{By: by, Epoch: epoch, At: at, EvidenceIDs: slices.Sorted(slices.Values(evidence))}
	r.Verified, r.Draft = d, nil
	r.Version++
	return superseded
}

// CheckSku admits the SKU a profile is kept for: it exists and the actor sees it (REFERENCE_INVALID
// at /sku_id), and the specification revision was confirmed (REFERENCE_INVALID at /spec_revision).
// A profile is kept for every confirmed revision, also one the SKU has moved on from or of a SKU
// that stopped: goods made to that revision are still declared. confirmed is the SKU's number of
// confirmed revisions. Paths are relative; callers place them in their request.
func CheckSku(visible bool, confirmed, rev int64) error {
	switch {
	case !visible:
		return kernel.Reject(kernel.CodeReferenceInvalid, "/sku_id", "")
	case rev < 1 || rev > confirmed:
		return kernel.Reject(kernel.CodeReferenceInvalid, "/spec_revision", "Specification revision was never confirmed")
	}
	return nil
}

// Answer is the decision of DeclarationProfileRef v1 on one reference (CAT-INV-002).
type Answer struct {
	Revision *Revision // the revision the reference names now: the asked one, or the verified one for 0
	Code     string    // "" when Revision is verified; else why the reference is not usable
}

// Resolve decides a reference to profile r (nil when no profile has the key) at revision asked (0:
// the revision verified now). rev is revision asked as stored, nil when it does not exist; it is
// ignored for asked 0. The answer, in this order:
//   - asked 0: the verified revision; DECLARATION_PROFILE_UNVERIFIED when there is none (no profile,
//     or only a draft: a candidate is never used);
//   - asked n: REFERENCE_INVALID when the profile or revision n does not exist;
//     DECLARATION_PROFILE_UNVERIFIED when n is the draft; "" when n is the verified revision;
//     REFERENCE_STALE when n was verified and is superseded (the caller refreshes the reference; a
//     historic declaration keeps n, which never changed).
func Resolve(r *Root, asked int64, rev *Revision) Answer {
	switch {
	case asked == 0 && (r == nil || r.Verified == nil):
		return Answer{Code: CodeDeclarationProfileUnverified}
	case asked == 0:
		return Answer{Revision: r.Verified}
	case r == nil || rev == nil || asked > r.LastRevision || rev.Number != asked:
		return Answer{Code: kernel.CodeReferenceInvalid}
	}
	switch rev.Status {
	case Verified:
		return Answer{Revision: rev}
	case Superseded:
		return Answer{Revision: rev, Code: CodeReferenceStale}
	}
	return Answer{Revision: rev, Code: CodeDeclarationProfileUnverified}
}
