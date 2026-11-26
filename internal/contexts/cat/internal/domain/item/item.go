// Package item is the Item aggregate (CAT-AGG-Item). A style does not replace the SKU. A draft
// revision is saved whole; a human confirms it and publishes that revision and its matrix. A later
// save opens the next draft, and confirming that draft supersedes the published revision.
package item

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Status of a style revision.
type Status string

const (
	Draft      Status = "draft"
	Confirmed  Status = "confirmed"
	Superseded Status = "superseded"
)

// CodeBusinessKeyConflict is BUSINESS_KEY_CONFLICT (409).
const CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"

// Param is one technical parameter.
type Param struct {
	Value    string `json:"value"`
	Unit     string `json:"unit"`
	Measured bool   `json:"measured"`
	SourceID string `json:"source_id"`
}

// Variant is one colour and size.
type Variant struct {
	ColorCode    string `json:"color_code"`
	SizeCode     string `json:"size_code"`
	SkuID        string `json:"sku_id"`
	SpecRevision int64  `json:"spec_revision"`
}

// Tech is the body of a revision.
type Tech struct {
	FabricComposition   string    `json:"fabric_composition"`
	FabricSpec          string    `json:"fabric_spec,omitempty"`
	FabricWidth         Param     `json:"fabric_width"`
	FabricGSM           Param     `json:"fabric_gsm"`
	WarpShrinkage       Param     `json:"warp_shrinkage"`
	WeftShrinkage       Param     `json:"weft_shrinkage"`
	DyeLotPolicy        string    `json:"dye_lot_policy"`
	SizeChartEvidenceID string    `json:"size_chart_evidence_id,omitempty"`
	Variants            []Variant `json:"variants"`
}

// Revision is one saved body.
type Revision struct {
	Number       int64
	Status       Status
	Hash         string
	Tech         Tech
	SavedBy      kernel.ActorRef
	SavedEpoch   int64
	SavedAt      time.Time
	ConfirmedBy  kernel.ActorRef
	ConfirmEpoch int64
	ConfirmedAt  time.Time
	EvidenceIDs  []string
	SupersededAt time.Time
}

// Item is the style root.
type Item struct {
	ID         string
	Profile    string
	StyleCode  string
	Version    int64
	Last       int64
	Current    int64
	OwnerActor string
	OwnerTeam  string
	Draft      *Revision
	Confirmed  *Revision
}

// New returns an unsaved style.
func New(id, profile, style, owner, team string) *Item {
	return &Item{ID: id, Profile: profile, StyleCode: style, OwnerActor: owner, OwnerTeam: team}
}

// CheckStyle refuses a style code another style already uses.
func CheckStyle(taken bool) error {
	if taken {
		return kernel.RejectAs(CodeBusinessKeyConflict, kernel.CategoryConflict, "/args/style_code", "Another item uses this style code")
	}
	return nil
}

// CheckSave admits a whole-draft save. Profile and style code are fixed by the first save.
func CheckSave(it *Item, expected int64, profile, style string) error {
	if it == nil {
		if expected != 0 {
			return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
		}
		return nil
	}
	if it.Version != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if it.Profile != profile || it.StyleCode != style {
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/style_code", "A style keeps the profile and style code of its first save")
	}
	return nil
}

// SourceIDs returns the evidence the technical parameters cite.
func (t Tech) SourceIDs() []string {
	ids := []string{t.FabricWidth.SourceID, t.FabricGSM.SourceID, t.WarpShrinkage.SourceID, t.WeftShrinkage.SourceID}
	if t.SizeChartEvidenceID != "" {
		ids = append(ids, t.SizeChartEvidenceID)
	}
	return slices.Compact(slices.Sorted(slices.Values(ids)))
}

func (t Tech) hash() string {
	doc, err := json.Marshal(t)
	if err != nil {
		panic("item: tech does not encode: " + err.Error())
	}
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:])
}

// Save replaces the draft or starts the next revision.
func (it *Item) Save(t Tech, by kernel.ActorRef, epoch int64, at time.Time) (*Revision, bool) {
	t.Variants = slices.Clone(t.Variants)
	slices.SortFunc(t.Variants, func(a, b Variant) int {
		if a.ColorCode != b.ColorCode {
			if a.ColorCode < b.ColorCode {
				return -1
			}
			return 1
		}
		if a.SizeCode < b.SizeCode {
			return -1
		}
		if a.SizeCode > b.SizeCode {
			return 1
		}
		return 0
	})
	for i := range []Param{t.FabricWidth, t.FabricGSM, t.WarpShrinkage, t.WeftShrinkage} {
		p := []*Param{&t.FabricWidth, &t.FabricGSM, &t.WarpShrinkage, &t.WeftShrinkage}[i]
		if d, err := kernel.ParseDecimal(p.Value, kernel.QuantityFormat); err == nil {
			if exact, ok := d.Exact(); ok {
				p.Value = exact
			}
		}
	}
	created := it.Draft == nil
	if created {
		it.Last++
		it.Draft = &Revision{Number: it.Last, Status: Draft}
	}
	d := it.Draft
	d.Tech = t
	d.Hash = t.hash()
	d.SavedBy, d.SavedEpoch, d.SavedAt = by, epoch, at
	it.Version++
	return d, created
}

// CheckConfirm admits confirming revision number.
func (it *Item) CheckConfirm(expected, number int64) (*Revision, error) {
	if it == nil {
		return nil, kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	}
	d := it.Draft
	switch {
	case it.Version != expected:
		return nil, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case number > it.Last:
		return nil, kernel.Reject(kernel.CodeNotFound, "/args/revision", "")
	case d == nil || d.Number != number:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/revision", "")
	}
	return d, nil
}

// Confirm publishes the draft and supersedes the published revision.
func (it *Item) Confirm(by kernel.ActorRef, epoch int64, evidence []string, at time.Time) *Revision {
	superseded := it.Confirmed
	if superseded != nil {
		superseded.Status, superseded.SupersededAt = Superseded, at
	}
	d := it.Draft
	d.Status = Confirmed
	d.ConfirmedBy, d.ConfirmEpoch, d.ConfirmedAt = by, epoch, at
	d.EvidenceIDs = slices.Clone(evidence)
	it.Confirmed, it.Draft = d, nil
	it.Current = d.Number
	it.Version++
	return superseded
}
