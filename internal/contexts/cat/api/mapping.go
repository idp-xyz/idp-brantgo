package api

import (
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Item code mappings (CAT-AGG-ItemCodeMapping): a customer's or a supplier's own item code mapped to
// a SKU at one specification revision, in revisions a human publishes. Codes are matched exactly,
// never by text similarity; a code without a published mapping is unmapped and goes to manual
// verification, and CAT never substitutes a SKU for it.
const (
	EventItemCodeMappingPublished = "idp-brant.cat.item_code_mapping_published.v1"

	CommandSaveItemCodeMapping    = "item_code_mapping.save_draft"
	CommandPublishItemCodeMapping = "item_code_mapping.publish"

	// Roles of the party whose code is mapped.
	RoleCustomer = "customer"
	RoleSupplier = "supplier"

	// Statuses of a mapping revision besides StatusDraft.
	StatusActive     = "active"
	StatusSuperseded = "superseded"

	// Answers of an item code resolution.
	ItemCodeMapped   = "mapped"
	ItemCodeUnmapped = "unmapped"

	// MaxExternalCodeLength is the longest external item code in characters (Python code_mapping
	// external_code).
	MaxExternalCodeLength = 500
	// MaxLookup is the most codes or references one lookup answers.
	MaxLookup = 1000
	// MaxEvidence is the most sources a publication cites.
	MaxEvidence = 200

	maxVersion = 2147483647
)

// Codes CAT declares beyond the shared ones, with the category the error catalog V0.1 gives them.
const (
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT" // 409: another mapping is published for the item code, or another declaration profile has the key
	CodeReferenceStale      = "REFERENCE_STALE"       // 409: the specification revision is no longer current; refresh and resubmit
)

var codes = map[string]kernel.Category{
	CodeBusinessKeyConflict:          kernel.CategoryConflict,
	CodeReferenceStale:               kernel.CategoryConflict,
	CodeIndependentReviewRequired:    kernel.CategoryInvalid, // declaration.go
	CodeDeclarationProfileUnverified: kernel.CategoryInvalid, // declaration.go
}

// Category returns the category of a code CAT declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// Reject returns a rejection with a code CAT declares, for downstream contexts that pass a CAT
// verdict on at a path of their own request. It panics on a code CAT does not declare.
func Reject(code, path string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("cat: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, "")
}

// SaveItemCodeMapping creates a mapping at ExpectedVersion 0 or replaces its draft revision whole at
// the current root version; without a draft it starts one with the next revision number. The key
// (PartyID, PartyRole, ExternalCode) is fixed by the first save and repeated by every later one.
// The SKU must be visible to the caller and confirmed at SpecRevision now (CAT-INV-001).
type SaveItemCodeMapping struct {
	IdempotencyKey  string
	MappingID       string // args.id
	ExpectedVersion int64
	PartyID         string // args.party_id: a PTY party, stored as given (PTY is not upstream of CAT)
	PartyRole       string // args.party_role: customer or supplier
	ExternalCode    string // args.external_code: the party's own code, matched exactly
	SkuID           string // args.sku_id
	SpecRevision    int64  // args.spec_revision
}

// PublishItemCodeMapping makes the draft revision Revision the active one. Only a human publishes,
// citing the evidence verified and a reason; the SKU must still be confirmed at the revision's
// specification revision, no other mapping may be published for the same key, and the mapping's
// previously active revision is superseded in the same transaction.
type PublishItemCodeMapping struct {
	IdempotencyKey  string
	MappingID       string // args.id
	ExpectedVersion int64
	Revision        int64
	EvidenceIDs     []string
	Reason          string
}

// Validate checks the closed contract.
func (c SaveItemCodeMapping) Validate() error {
	if err := head(c.IdempotencyKey, c.MappingID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	if err := (ItemCode{PartyID: c.PartyID, PartyRole: c.PartyRole, ExternalCode: c.ExternalCode}).Validate("/args"); err != nil {
		return err
	}
	return SpecRef{SkuID: c.SkuID, SpecRevision: c.SpecRevision}.Validate("/args")
}

func (c PublishItemCodeMapping) Validate() error {
	if err := head(c.IdempotencyKey, c.MappingID, c.ExpectedVersion, 1); err != nil {
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

// ItemCode is an external item code of one party in one role: what ItemCodeMappingRef v1 is
// resolved by.
type ItemCode struct {
	PartyID      string
	PartyRole    string // customer or supplier
	ExternalCode string
}

// Validate checks the form of the code; path is where it sits in the request.
func (c ItemCode) Validate(path string) error {
	switch {
	case !kernel.ValidIdentifier(c.PartyID):
		return schema(path + "/party_id")
	case c.PartyRole != RoleCustomer && c.PartyRole != RoleSupplier:
		return schema(path + "/party_role")
	case !ExternalCode(c.ExternalCode):
		return schema(path + "/external_code")
	}
	return nil
}

// ExternalCode reports whether s is an external item code: 1–500 characters of valid UTF-8, at
// least one that is not white space (Python \S), no control characters, and no white space at
// either end, so that one printed code has one form.
func ExternalCode(s string) bool {
	if !text(s, MaxExternalCodeLength) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return false
	}
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	return !unicode.IsSpace(first) && !unicode.IsSpace(last)
}

// SpecRef names a SKU and one of its specification revisions, as a downstream fact stores SkuRef v1.
type SpecRef struct {
	SkuID        string
	SpecRevision int64
}

// Validate checks the form of the reference; path is where it sits in the request.
func (r SpecRef) Validate(path string) error {
	switch {
	case !kernel.ValidIdentifier(r.SkuID):
		return schema(path + "/sku_id")
	case r.SpecRevision < 1 || r.SpecRevision > maxVersion:
		return schema(path + "/spec_revision")
	}
	return nil
}

// ItemCodeMappingRef is ItemCodeMappingRef v1: a mapping and the revision a downstream fact adopted.
type ItemCodeMappingRef struct {
	MappingID string
	Revision  int64
}

// ResolvedItemCode is what CAT says about one item code now. Status is ItemCodeMapped with the
// active revision of the mapping published for the code and the SKU specification revision it
// maps to, or ItemCodeUnmapped with zero Mapping and Sku: the caller sends the code to manual
// verification and never substitutes a SKU. A mapped Sku can have been revised since publication;
// ask ResolveSpecs whether it is still current before adopting it for new use.
type ResolvedItemCode struct {
	Code    ItemCode
	Status  string
	Mapping ItemCodeMappingRef
	Sku     SpecRef
}

// Mapped reports whether the code has a published mapping.
func (r ResolvedItemCode) Mapped() bool { return r.Status == ItemCodeMapped }

// SpecStanding is whether a specification revision is the SKU's current confirmed one, for new
// use (CAT-INV-001). Current is false for the zero value. Code is empty when Current;
// REFERENCE_STALE when the revision was confirmed but the SKU has since been reopened for
// revision or confirmed again (refresh the reference and resubmit); REFERENCE_INVALID when the
// SKU does not exist in the caller's data scope, is inactive, or the revision was never confirmed.
// Status and CurrentRevision describe the SKU now: CurrentRevision is its latest confirmed
// revision while it is confirmed, else 0; both are empty for a SKU the caller does not see.
type SpecStanding struct {
	Ref             SpecRef
	Current         bool
	Code            string
	Status          string
	CurrentRevision int64
}

// Check returns nil for a current revision and the rejection CAT's verdict is otherwise, at path
// of the caller's request. A standing CAT did not give (the zero value) is REFERENCE_INVALID.
func (s SpecStanding) Check(path string) error {
	switch {
	case s.Current:
		return nil
	case s.Code == CodeReferenceStale:
		return Reject(s.Code, path)
	}
	return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
}

// ItemCodeMappingRevision is one revision of a mapping. Published revisions never change.
type ItemCodeMappingRevision struct {
	Revision     int64
	Status       string // draft, active or superseded
	Sku          SpecRef
	SavedBy      kernel.ActorRef // the last editor
	SavedAt      time.Time
	PublishedBy  kernel.ActorRef // zero before publication
	PublishedAt  time.Time
	EvidenceIDs  []string  // the evidence the publisher verified
	SupersededAt time.Time // zero until superseded
}

// ItemCodeMappingView is a mapping, its key and all its revisions, oldest first.
type ItemCodeMappingView struct {
	MappingID      string
	Code           ItemCode
	RootVersion    int64
	DraftRevision  int64 // 0: none
	ActiveRevision int64 // 0: none
	Revisions      []ItemCodeMappingRevision
}

// ValidateItemCodes checks the codes of a lookup: at most MaxLookup, each well formed.
func ValidateItemCodes(cs []ItemCode) error {
	if len(cs) > MaxLookup {
		return schema("/codes")
	}
	for i, c := range cs {
		if err := c.Validate("/codes/" + strconv.Itoa(i)); err != nil {
			return err
		}
	}
	return nil
}

// ValidateSpecRefs checks the references of a lookup: at most MaxLookup, each well formed.
func ValidateSpecRefs(rs []SpecRef) error {
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
