package api

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Declaration profiles (CAT-AGG-DeclarationProfile, CAT-INV-002, SM-CAT-DECLARATION; baseline 22.1
// CNProductDeclarationProfile; fact MD-41; S5 D-22): what one SKU specification revision is for
// customs under one version of the commodity code table: the commodity code, the declaration name
// and elements, the legal units and their conversion from the SKU's base unit, the origin basis and
// the export control classification, each part with its own basis and sources. A draft is a
// candidate, saved by a human or an AI agent; only a human other than its last editor verifies it,
// and only a verified revision is ever used. Updating a verified profile saves its next revision as
// a draft; verifying it supersedes the verified one, which never changes. CNX resolves profiles
// through DeclarationProfileRef v1 (Context Map R39, no guard lock).
const (
	EventDeclarationProfileVerified = "idp-brant.cat.declaration_profile_verified.v1"

	CommandSaveDeclarationProfile   = "declaration_profile.save_draft"
	CommandVerifyDeclarationProfile = "declaration_profile.verify"

	// StatusVerified is the status of the revision in use; a profile's revisions are also
	// StatusDraft and StatusSuperseded.
	StatusVerified = "verified"

	// Export control classifications.
	ExportNotControlled = "not_controlled"
	ExportControlled    = "controlled"

	// Origin criteria of the non-preferential rules of origin.
	OriginWhollyObtained            = "wholly_obtained"
	OriginSubstantialTransformation = "substantial_transformation"

	// MaxDeclarationElements is the most declaration elements a profile lists.
	MaxDeclarationElements = 50
	// MaxLegalUnits is the most legal units of quantity: the first and the second.
	MaxLegalUnits = 2
	// MaxPartSources is the most sources one classification part cites.
	MaxPartSources = 20
)

// Codes of the declaration profile rows of the error catalog V0.1.
const (
	CodeIndependentReviewRequired    = "INDEPENDENT_REVIEW_REQUIRED"    // 422: the verifier last edited the revision
	CodeDeclarationProfileUnverified = "DECLARATION_PROFILE_UNVERIFIED" // 422: no verified profile answers the reference (CAT-INV-002)
)

// CommodityClassification is the commodity code (商品编码) of the profile's code table version, 6 to
// 12 digits without separators, with the basis of the classification and the EVD sources it rests
// on (SourceIDs optional, at most MaxPartSources).
type CommodityClassification struct {
	Code      string
	Basis     string
	SourceIDs []string
}

// DeclarationElement is one declaration element (申报要素), listed in the code table's order.
type DeclarationElement struct {
	Name  string
	Value string
}

// LegalUnitConversion is a legal unit of quantity (法定单位) and its conversion: one base unit of
// the SKU (PCS) is PerBaseUnit legal units, a positive plain decimal string with at most 12 integer
// and 12 fractional digits (kernel.FactorFormat), on the stated basis.
type LegalUnitConversion struct {
	Unit        string
	PerBaseUnit string
	Basis       string
}

// OriginBasis is the origin basis (原产地依据): the country of origin (ISO 3166-1 alpha-2 form), the
// criterion it meets (wholly_obtained or substantial_transformation), the basis and its sources.
type OriginBasis struct {
	Country   string
	Criterion string
	Basis     string
	SourceIDs []string
}

// ExportControlClassification is the export control classification: not_controlled, or controlled
// with ListReference, the control list and entry it falls under (absent when not controlled). It is
// a classification, not a trade compliance conclusion, which CNX decides.
type ExportControlClassification struct {
	Status        string
	ListReference string
	Basis         string
	SourceIDs     []string
}

// DeclarationProfile is the content of a revision, its classification parts kept separately (D-22).
type DeclarationProfile struct {
	Commodity       CommodityClassification
	DeclarationName string
	Elements        []DeclarationElement
	LegalUnits      []LegalUnitConversion
	Origin          OriginBasis
	ExportControl   ExportControlClassification
}

// SaveDeclarationProfile creates a profile at ExpectedVersion 0 or replaces its draft revision whole
// at the current root version; without a draft it starts one with the next revision number (the
// update of a verified profile). The key (SkuID, SpecRevision, CodeTableVersion) is fixed by the
// first save, repeated by every later one, and unique in the company (BUSINESS_KEY_CONFLICT). The SKU
// must be visible to the caller and SpecRevision one of its confirmed specification revisions, and
// the caller must be able to read every source the content cites. Open to AI agents: a draft is a
// candidate and never used (CAT-INV-002).
type SaveDeclarationProfile struct {
	IdempotencyKey   string
	ProfileID        string // args.id
	ExpectedVersion  int64
	SkuID            string
	SpecRevision     int64
	CodeTableVersion string // the commodity code table version, such as CN-2026
	Commodity        CommodityClassification
	DeclarationName  string // 申报名称
	Elements         []DeclarationElement
	LegalUnits       []LegalUnitConversion
	Origin           OriginBasis
	ExportControl    ExportControlClassification
}

// Profile returns the content the command saves.
func (c SaveDeclarationProfile) Profile() DeclarationProfile {
	return DeclarationProfile{Commodity: c.Commodity, DeclarationName: c.DeclarationName, Elements: c.Elements, LegalUnits: c.LegalUnits,
		Origin: c.Origin, ExportControl: c.ExportControl}
}

// VerifyDeclarationProfile makes the draft revision Revision the verified one and supersedes the
// verified revision. Only a human verifies, never the revision's last editor
// (INDEPENDENT_REVIEW_REQUIRED), citing the evidence verified and a reason; the verifier must be
// able to read the evidence and every source the revision cites, and see the SKU.
type VerifyDeclarationProfile struct {
	IdempotencyKey  string
	ProfileID       string // args.id
	ExpectedVersion int64
	Revision        int64
	EvidenceIDs     []string
	Reason          string
}

var commodityCode = regexp.MustCompile(`^[0-9]{6,12}$`)

// Validate checks the closed contract.
func (c SaveDeclarationProfile) Validate() error {
	if err := head(c.IdempotencyKey, c.ProfileID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	if err := (DeclarationProfileKey{SkuID: c.SkuID, SpecRevision: c.SpecRevision, CodeTableVersion: c.CodeTableVersion}).Validate("/args"); err != nil {
		return err
	}
	return c.Profile().Validate("/args")
}

// Validate checks the form of the content; path is where its members sit in the request.
func (p DeclarationProfile) Validate(path string) error {
	switch {
	case !commodityCode.MatchString(p.Commodity.Code):
		return schema(path + "/commodity/code")
	case !prose(p.Commodity.Basis):
		return schema(path + "/commodity/basis")
	}
	if err := sources(p.Commodity.SourceIDs, path+"/commodity/source_ids"); err != nil {
		return err
	}
	if !oneLine(p.DeclarationName, 200) {
		return schema(path + "/declaration_name")
	}
	if len(p.Elements) < 1 || len(p.Elements) > MaxDeclarationElements {
		return schema(path + "/elements")
	}
	for i, e := range p.Elements {
		at := path + "/elements/" + strconv.Itoa(i)
		switch {
		case !oneLine(e.Name, 100) || slices.IndexFunc(p.Elements, func(o DeclarationElement) bool { return o.Name == e.Name }) != i:
			return schema(at + "/name")
		case !oneLine(e.Value, 500):
			return schema(at + "/value")
		}
	}
	if len(p.LegalUnits) < 1 || len(p.LegalUnits) > MaxLegalUnits {
		return schema(path + "/legal_units")
	}
	for i, u := range p.LegalUnits {
		at := path + "/legal_units/" + strconv.Itoa(i)
		switch {
		case !oneLine(u.Unit, 32) || slices.IndexFunc(p.LegalUnits, func(o LegalUnitConversion) bool { return o.Unit == u.Unit }) != i:
			return schema(at + "/unit")
		case !positiveFactor(u.PerBaseUnit):
			return schema(at + "/per_base_unit")
		case !prose(u.Basis):
			return schema(at + "/basis")
		}
	}
	o := p.Origin
	switch {
	case !country(o.Country):
		return schema(path + "/origin/country")
	case o.Criterion != OriginWhollyObtained && o.Criterion != OriginSubstantialTransformation:
		return schema(path + "/origin/criterion")
	case !prose(o.Basis):
		return schema(path + "/origin/basis")
	}
	if err := sources(o.SourceIDs, path+"/origin/source_ids"); err != nil {
		return err
	}
	x := p.ExportControl
	switch {
	case x.Status != ExportNotControlled && x.Status != ExportControlled:
		return schema(path + "/export_control/status")
	case (x.Status == ExportControlled) != (x.ListReference != "") || (x.ListReference != "" && !oneLine(x.ListReference, 200)):
		return schema(path + "/export_control/list_reference")
	case !prose(x.Basis):
		return schema(path + "/export_control/basis")
	}
	return sources(x.SourceIDs, path+"/export_control/source_ids")
}

// Validate checks the closed contract.
func (c VerifyDeclarationProfile) Validate() error {
	if err := head(c.IdempotencyKey, c.ProfileID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if c.Revision < 1 || c.Revision > maxVersion {
		return schema("/args/revision")
	}
	if len(c.EvidenceIDs) < 1 || len(c.EvidenceIDs) > MaxEvidence {
		return schema("/args/evidence_ids")
	}
	for i, id := range c.EvidenceIDs {
		if !kernel.ValidIdentifier(id) || slices.Index(c.EvidenceIDs, id) != i {
			return schema("/args/evidence_ids")
		}
	}
	if !text(c.Reason, 1000) {
		return schema("/args/reason")
	}
	return nil
}

func sources(ids []string, path string) error {
	if len(ids) > MaxPartSources {
		return schema(path)
	}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) || slices.Index(ids, id) != i {
			return schema(path)
		}
	}
	return nil
}

// oneLine reports whether s is 1 to max characters of valid UTF-8 on one line: no control
// characters, at least one that is not white space and none at either end.
func oneLine(s string, max int) bool {
	if !text(s, max) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return false
	}
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	return !unicode.IsSpace(first) && !unicode.IsSpace(last)
}

// prose reports whether s is a basis text: 1 to 1000 characters of valid UTF-8, not only white
// space, with no control characters but line feeds and tabs.
func prose(s string) bool {
	return text(s, 1000) && strings.TrimSpace(s) != "" &&
		strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) < 0
}

func country(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

func positiveFactor(s string) bool {
	d, err := kernel.ParseDecimal(s, kernel.FactorFormat)
	return err == nil && d.Sign() > 0
}

// DeclarationProfileKey is what a profile is kept for (D-22): one confirmed specification revision
// of a SKU under one commodity code table version. A company keeps at most one profile per key.
type DeclarationProfileKey struct {
	SkuID            string
	SpecRevision     int64
	CodeTableVersion string
}

// Validate checks the form of the key; path is where its members sit in the request.
func (k DeclarationProfileKey) Validate(path string) error {
	switch {
	case !kernel.ValidIdentifier(k.SkuID):
		return schema(path + "/sku_id")
	case k.SpecRevision < 1 || k.SpecRevision > maxVersion:
		return schema(path + "/spec_revision")
	case !kernel.ValidIdentifier(k.CodeTableVersion):
		return schema(path + "/code_table_version")
	}
	return nil
}

// DeclarationProfileRef is DeclarationProfileRef v1 (R39) as a downstream fact stores it: the key and
// the profile revision adopted. In a lookup, Revision 0 asks for the revision verified now.
type DeclarationProfileRef struct {
	SkuID            string
	SpecRevision     int64
	CodeTableVersion string
	Revision         int64
}

// Key returns the key the reference names.
func (r DeclarationProfileRef) Key() DeclarationProfileKey {
	return DeclarationProfileKey{SkuID: r.SkuID, SpecRevision: r.SpecRevision, CodeTableVersion: r.CodeTableVersion}
}

// Validate checks the form of the reference; path is where it sits in the request.
func (r DeclarationProfileRef) Validate(path string) error {
	if err := r.Key().Validate(path); err != nil {
		return err
	}
	if r.Revision < 0 || r.Revision > maxVersion {
		return schema(path + "/revision")
	}
	return nil
}

// ValidateDeclarationProfileRefs checks the references of a lookup: at most MaxLookup, each well
// formed.
func ValidateDeclarationProfileRefs(rs []DeclarationProfileRef) error {
	if len(rs) > MaxLookup {
		return schema("/refs")
	}
	for i, r := range rs {
		if err := r.Validate("/refs/" + strconv.Itoa(i)); err != nil {
			return err
		}
	}
	return nil
}

// ResolvedDeclarationProfile is what CAT says about one DeclarationProfileRef v1 now (CAT-INV-002).
//
// Verified is true, with Code empty, when the revision answered is the profile's verified one: for
// Ref.Revision 0 the revision verified now, else exactly Ref.Revision. Otherwise Code says why the
// reference is not usable: DECLARATION_PROFILE_UNVERIFIED when no profile has the key, the profile
// has only a draft, or Ref.Revision is the draft (a candidate, never used); REFERENCE_STALE when
// Ref.Revision was verified and is superseded by CurrentRevision (refresh for new use; a historic
// declaration keeps the revision it adopted, which never changes); REFERENCE_INVALID when
// Ref.Revision does not exist.
//
// Profile, ContentHash, VerifiedBy, VerifiedAt and EvidenceIDs describe the revision answered when
// it is verified or superseded; a draft's content is not answered. ProfileID, CurrentRevision and
// DraftRevision describe the profile now (empty and 0 when no profile has the key). The answer does
// not judge the SKU: whether the specification revision is current for new business is SkuRef v1's
// (ResolveSpecs, SkuGuard v1).
type ResolvedDeclarationProfile struct {
	Ref             DeclarationProfileRef
	Verified        bool
	Code            string
	ProfileID       string
	Revision        int64  // the revision answered; 0 when there is none
	Status          string // its status: draft, verified or superseded
	CurrentRevision int64  // the profile's verified revision now; 0: none
	DraftRevision   int64  // the profile's draft revision now; 0: none
	ContentHash     string
	Profile         DeclarationProfile
	VerifiedBy      kernel.ActorRef
	VerifiedAt      time.Time
	EvidenceIDs     []string
}

// Adopted returns the reference a downstream fact stores for the revision answered.
func (r ResolvedDeclarationProfile) Adopted() DeclarationProfileRef {
	ref := r.Ref
	ref.Revision = r.Revision
	return ref
}

// Check returns nil for a verified answer and CAT's rejection otherwise, at path of the caller's
// request. An answer CAT did not give (the zero value) is DECLARATION_PROFILE_UNVERIFIED.
func (r ResolvedDeclarationProfile) Check(path string) error {
	switch {
	case r.Verified:
		return nil
	case r.Code == CodeReferenceStale:
		return Reject(CodeReferenceStale, path)
	case r.Code == kernel.CodeReferenceInvalid:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	return Reject(CodeDeclarationProfileUnverified, path)
}

// DeclarationProfileRevision is one revision of a profile with its content. Verified and superseded
// revisions never change.
type DeclarationProfileRevision struct {
	Revision    int64
	Status      string // draft, verified or superseded
	ContentHash string
	DeclarationProfile
	SavedBy      kernel.ActorRef // the last editor
	SavedAt      time.Time
	VerifiedBy   kernel.ActorRef // zero before verification
	VerifiedAt   time.Time
	EvidenceIDs  []string  // the evidence the verifier verified
	SupersededAt time.Time // zero until superseded
}

// DeclarationProfileView is a profile, its key and all its revisions, oldest first.
type DeclarationProfileView struct {
	ProfileID        string
	Key              DeclarationProfileKey
	RootVersion      int64
	DraftRevision    int64 // 0: none
	VerifiedRevision int64 // 0: none
	Revisions        []DeclarationProfileRevision
}

// DeclarationProfileSummary is a profile of a SKU and where its revisions stand.
type DeclarationProfileSummary struct {
	ProfileID        string
	Key              DeclarationProfileKey
	RootVersion      int64
	DraftRevision    int64
	VerifiedRevision int64
}

// DeclarationProfileList is every profile kept for a SKU, by specification revision and code table
// version.
type DeclarationProfileList struct {
	SkuID    string
	Profiles []DeclarationProfileSummary
}

// DeclarationCommands are CAT's declaration profile commands. Errors are *kernel.Rejection for
// refusals. The receipt's ObjectID is the profile ID, RowVersion the root version and Status the
// status of the revision the command wrote (draft or verified).
type DeclarationCommands interface {
	SaveDeclarationProfile(ctx context.Context, cmd SaveDeclarationProfile) (Receipt, error)
	VerifyDeclarationProfile(ctx context.Context, cmd VerifyDeclarationProfile) (Receipt, error)
}

// DeclarationProfileLookups is DeclarationProfileRef v1 for downstream contexts (R39, CNX).
type DeclarationProfileLookups interface {
	// ResolveDeclarationProfiles answers every reference asked (CAT-INV-002). Profiles belong to
	// the company; it needs no permission beyond an active grant. Ask before opening your own
	// transaction; R39 has no guard: a verified revision never changes, so a reference adopted
	// keeps its meaning, and a later verification only makes it stale for new use.
	ResolveDeclarationProfiles(ctx context.Context, refs []DeclarationProfileRef) (map[DeclarationProfileRef]ResolvedDeclarationProfile, error)
}

// DeclarationQueries is the read side of declaration profiles.
type DeclarationQueries interface {
	DeclarationProfileLookups
	// GetDeclarationProfile returns a profile and its revisions, audited as declaration_profile.get.
	GetDeclarationProfile(ctx context.Context, profileID string) (DeclarationProfileView, error)
	// ListDeclarationProfiles returns the profiles kept for a SKU, audited as
	// declaration_profile.list; a SKU without profiles has an empty list.
	ListDeclarationProfiles(ctx context.Context, skuID string) (DeclarationProfileList, error)
}
