// Package costbom is the versioned cost BOM (WBS S7-02). A draft is saved whole. A human who did
// not draft that revision verifies it and freezes the content hash. A size-graded line is read by
// the asked size only: a missing size is BOM_SIZE_GRADE_MISSING and the base size is never used.
package costbom

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Status of a cost BOM root or revision.
type Status string

const (
	StatusDraft      Status = "draft"
	StatusVerified   Status = "verified"
	StatusSuperseded Status = "superseded"
	StatusWithdrawn  Status = "withdrawn"
)

// Codes the guard and the size lookup return (error catalog).
const (
	CodeUnverified             = "BOM_UNVERIFIED"
	CodeSizeGradeMissing       = "BOM_SIZE_GRADE_MISSING"
	CodeSubstitutionUnapproved = "BOM_SUBSTITUTION_UNAPPROVED"
)

// Categories are the catalog categories of the codes this package returns.
var Categories = map[string]kernel.Category{
	CodeUnverified:             kernel.CategoryInvalid,
	CodeSizeGradeMissing:       kernel.CategoryInvalid,
	CodeSubstitutionUnapproved: kernel.CategoryInvalid,
}

const (
	GradeUniform    = "uniform"
	GradeSizeGraded = "size_graded"
)

// SupplyRef is an optional verified supply quote the line prices from. CST stores the reference;
// it does not read SUP in this command.
type SupplyRef struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// Grade is one size's net consumption. SizeCMDeltaMinor is an optional labour delta in minor units.
type Grade struct {
	SizeCode           string `json:"size_code"`
	ConsumptionPerUnit string `json:"consumption_per_unit"`
	SizeCMDeltaMinor   string `json:"size_cm_delta_minor,omitempty"`
	Currency           string `json:"currency,omitempty"`
	MinorUnits         int    `json:"minor_units,omitempty"`
}

// Line is one material. Grades is empty when GradingMode is uniform.
type Line struct {
	LineID             string     `json:"line_id"`
	MaterialKind       string     `json:"material_kind"`
	MaterialCode       string     `json:"material_code"`
	Unit               string     `json:"unit"`
	GradingMode        string     `json:"grading_mode"`
	ConsumptionPerUnit string     `json:"consumption_per_unit"`
	WastageRatio       string     `json:"wastage_ratio"`
	ShrinkageFactor    string     `json:"shrinkage_factor"`
	FabricWidthCM      string     `json:"fabric_width_cm,omitempty"`
	FabricGSM          string     `json:"fabric_gsm,omitempty"`
	Measured           bool       `json:"measured"`
	FormulaBasis       string     `json:"formula_basis"`
	Supply             *SupplyRef `json:"supply_ref,omitempty"`
	Grades             []Grade    `json:"size_grades,omitempty"`
}

// Consumption returns the net consumption of this line for size. Uniform lines use the line's own
// quantity. A size-graded line uses the row for size and no other row: the base size is not a fallback.
func (l Line) Consumption(size string) (string, error) {
	if l.GradingMode == GradeUniform {
		return l.ConsumptionPerUnit, nil
	}
	for _, g := range l.Grades {
		if g.SizeCode == size {
			return g.ConsumptionPerUnit, nil
		}
	}
	return "", kernel.RejectAs(CodeSizeGradeMissing, kernel.CategoryInvalid, "/size_code", "")
}

// Surcharge is a dye-lot minimum, a trim minimum, or a sample or pattern fee.
type Surcharge struct {
	SurchargeID  string `json:"surcharge_id"`
	Kind         string `json:"surcharge_kind"`
	Scope        string `json:"scope"`
	ColorCode    string `json:"color_code,omitempty"`
	MaterialCode string `json:"material_code,omitempty"`
	MCQThreshold string `json:"mcq_threshold,omitempty"`
	MCQUnit      string `json:"mcq_unit,omitempty"`
	AmountMinor  string `json:"amount_minor,omitempty"`
	Currency     string `json:"currency,omitempty"`
	MinorUnits   int    `json:"minor_units,omitempty"`
	Allocation   string `json:"allocation"`
	FeeCategory  string `json:"fee_category,omitempty"`
}

// Content is what verify freezes. Field order is the hash.
type Content struct {
	Profile      string      `json:"document_profile"`
	BaseSize     string      `json:"base_size_code"`
	ItemID       string      `json:"item_id,omitempty"`
	SkuID        string      `json:"sku_id,omitempty"`
	SpecRevision int64       `json:"spec_revision,omitempty"`
	Lines        []Line      `json:"lines"`
	Surcharges   []Surcharge `json:"surcharges,omitempty"`
}

// Hash is the SHA-256 of the canonical JSON. Lines, grades and surcharges are ordered by id so
// two equal BOMs hash the same.
func (c Content) Hash() string {
	c.Lines = slices.Clone(c.Lines)
	slices.SortFunc(c.Lines, func(a, b Line) int {
		if a.LineID < b.LineID {
			return -1
		}
		if a.LineID > b.LineID {
			return 1
		}
		return 0
	})
	for i := range c.Lines {
		c.Lines[i].Grades = slices.Clone(c.Lines[i].Grades)
		slices.SortFunc(c.Lines[i].Grades, func(a, b Grade) int {
			if a.SizeCode < b.SizeCode {
				return -1
			}
			if a.SizeCode > b.SizeCode {
				return 1
			}
			return 0
		})
	}
	c.Surcharges = slices.Clone(c.Surcharges)
	slices.SortFunc(c.Surcharges, func(a, b Surcharge) int {
		if a.SurchargeID < b.SurchargeID {
			return -1
		}
		if a.SurchargeID > b.SurchargeID {
			return 1
		}
		return 0
	})
	doc, err := json.Marshal(c)
	if err != nil {
		panic(fmt.Sprintf("costbom: content does not encode: %v", err))
	}
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:])
}

// End is a withdrawal.
type End struct {
	By     kernel.ActorRef
	Epoch  int64
	At     time.Time
	Reason string
}

// Revision is one saved content. Fresh and Changed tell the store what to write. LinesReplaced
// replaces the child rows of a draft.
type Revision struct {
	Number        int64
	Status        Status
	Lines         []Line
	Surcharges    []Surcharge
	Hash          string
	Contributors  []string
	SavedBy       kernel.ActorRef
	SavedEpoch    int64
	SavedAt       time.Time
	VerifiedBy    kernel.ActorRef
	VerifyEpoch   int64
	VerifiedAt    time.Time
	SupersededAt  time.Time
	End           *End
	Fresh         bool
	Changed       bool
	LinesReplaced bool
}

// BOM is the root.
type BOM struct {
	ID             string
	RootVersion    int64
	LastRevision   int64
	ActiveRevision int64
	Status         Status
	Profile        string
	BaseSize       string
	ItemID         string
	SkuID          string
	SpecRevision   int64
	OwnerActor     string
	OwnerTeam      string
	Revisions      []*Revision
}

// New returns an unsaved root the actor will own.
func New(id, ownerActor, ownerTeam string) *BOM {
	return &BOM{ID: id, OwnerActor: ownerActor, OwnerTeam: ownerTeam}
}

// Revision returns revision n, or nil.
func (b *BOM) Revision(n int64) *Revision {
	if b == nil {
		return nil
	}
	for _, r := range b.Revisions {
		if r.Number == n {
			return r
		}
	}
	return nil
}

func (b *BOM) with(s Status) *Revision {
	if b == nil {
		return nil
	}
	for _, r := range b.Revisions {
		if r.Status == s {
			return r
		}
	}
	return nil
}

// Draft returns the draft revision, or nil.
func (b *BOM) Draft() *Revision { return b.with(StatusDraft) }

// Active returns the verified revision the root names, or nil.
func (b *BOM) Active() *Revision {
	if b == nil || b.ActiveRevision == 0 {
		return nil
	}
	return b.Revision(b.ActiveRevision)
}

// Check admits a command on an existing visible root at its current version.
func Check(b *BOM, visible bool, expected int64) error {
	switch {
	case b == nil || !visible:
		return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case b.RootVersion != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

func (b *BOM) content(r *Revision) Content {
	return Content{Profile: b.Profile, BaseSize: b.BaseSize, ItemID: b.ItemID, SkuID: b.SkuID,
		SpecRevision: b.SpecRevision, Lines: r.Lines, Surcharges: r.Surcharges}
}

func (b *BOM) applyIdentity(c Content) {
	b.Profile, b.BaseSize, b.ItemID, b.SkuID, b.SpecRevision = c.Profile, c.BaseSize, c.ItemID, c.SkuID, c.SpecRevision
}

// Save writes content as the draft. A verified BOM is not saved over. A withdrawn BOM starts a new draft.
func (b *BOM) Save(c Content, by kernel.ActorRef, epoch int64, now time.Time) (*Revision, error) {
	if b.Status == StatusVerified {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/id", "")
	}
	now = kernel.Instant(now)
	b.applyIdentity(c)
	b.RootVersion++
	if d := b.Draft(); d != nil {
		if !slices.Contains(d.Contributors, by.ID) {
			d.Contributors = append(d.Contributors, by.ID)
		}
		d.Lines, d.Surcharges = c.Lines, c.Surcharges
		d.SavedBy, d.SavedEpoch, d.SavedAt = by, epoch, now
		d.Changed, d.LinesReplaced = true, true
		return d, nil
	}
	b.LastRevision++
	b.Status = StatusDraft
	b.ActiveRevision = 0
	r := &Revision{Number: b.LastRevision, Status: StatusDraft, Lines: c.Lines, Surcharges: c.Surcharges,
		Contributors: []string{by.ID}, SavedBy: by, SavedEpoch: epoch, SavedAt: now, Fresh: true, LinesReplaced: true}
	b.Revisions = append(b.Revisions, r)
	return r, nil
}

// Verify freezes revision n. The verifier is a human and not one of its drafters.
func (b *BOM) Verify(n int64, by kernel.ActorRef, epoch int64, now time.Time) (*Revision, error) {
	if by.Kind != kernel.ActorHuman {
		return nil, kernel.Reject(kernel.CodePermissionDenied, "/", "")
	}
	r := b.Revision(n)
	if r == nil || r.Status != StatusDraft {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/revision", "")
	}
	if slices.Contains(r.Contributors, by.ID) {
		return nil, kernel.Reject(kernel.CodePermissionDenied, "/", "")
	}
	now = kernel.Instant(now)
	r.Hash = b.content(r).Hash()
	r.Status = StatusVerified
	r.VerifiedBy, r.VerifyEpoch, r.VerifiedAt = by, epoch, now
	r.Changed = true
	b.Status = StatusVerified
	b.ActiveRevision = r.Number
	b.RootVersion++
	return r, nil
}

// Withdraw ends a verified or superseded revision and leaves the root withdrawn when it was the active one.
func (b *BOM) Withdraw(n int64, by kernel.ActorRef, epoch int64, now time.Time, reason string) (*Revision, error) {
	if by.Kind != kernel.ActorHuman {
		return nil, kernel.Reject(kernel.CodePermissionDenied, "/", "")
	}
	r := b.Revision(n)
	if r == nil || (r.Status != StatusVerified && r.Status != StatusSuperseded) {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/revision", "")
	}
	now = kernel.Instant(now)
	if b.ActiveRevision == r.Number {
		b.Status = StatusWithdrawn
	}
	r.Status = StatusWithdrawn
	r.End = &End{By: by, Epoch: epoch, At: now, Reason: reason}
	r.Changed = true
	b.RootVersion++
	return r, nil
}

// ApproveSubstitution appends a verified revision with line's material changed from fromCode to
// toCode, and supersedes the active revision. by is the decider and must not be the requester,
// who is recorded as the drafter of the new revision.
func (b *BOM) ApproveSubstitution(lineID, fromCode, toCode, requester string, by kernel.ActorRef, epoch int64, now time.Time) (*Revision, error) {
	if by.Kind != kernel.ActorHuman || by.ID == requester {
		return nil, kernel.Reject(kernel.CodePermissionDenied, "/", "")
	}
	active := b.Active()
	if active == nil || active.Status != StatusVerified {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/bom_id", "")
	}
	lines := slices.Clone(active.Lines)
	found := false
	for i := range lines {
		if lines[i].LineID != lineID {
			continue
		}
		if lines[i].MaterialCode != fromCode {
			return nil, kernel.Reject(kernel.CodeInvalidState, "/args/original_material_code", "")
		}
		lines[i].MaterialCode = toCode
		lines[i].Grades = slices.Clone(lines[i].Grades)
		found = true
	}
	if !found {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/original_line_id", "")
	}
	now = kernel.Instant(now)
	active.Status = StatusSuperseded
	active.SupersededAt = now
	active.Changed = true
	b.LastRevision++
	b.RootVersion++
	r := &Revision{Number: b.LastRevision, Status: StatusVerified, Lines: lines, Surcharges: slices.Clone(active.Surcharges),
		Contributors: []string{requester}, SavedBy: by, SavedEpoch: epoch, SavedAt: now,
		VerifiedBy: by, VerifyEpoch: epoch, VerifiedAt: now, Fresh: true, LinesReplaced: true}
	r.Hash = b.content(r).Hash()
	b.ActiveRevision = r.Number
	b.Status = StatusVerified
	b.Revisions = append(b.Revisions, r)
	return r, nil
}
