package api

import (
	"context"
	"slices"
	"strconv"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Styles (CAT-AGG-Item). A style is the company's style code and a matrix of colour, size and
// confirmed SKU. Technical parameters live on the revision, each with a unit, a measured flag and
// a source. item.save_draft is open to a human or an AI agent. item.confirm is human only and
// publishes an immutable revision. Neither command requires the apparel plugin to be active.
const (
	EventItemConfirmed = "idp-brant.cat.item_confirmed.v1"

	CommandSaveItem    = "item.save_draft"
	CommandConfirmItem = "item.confirm"

	ProfileGeneric = "generic"
	ProfileApparel = "apparel"

	DyeLotNone      = "none"
	DyeLotSameColor = "same_lot_per_color"
	DyeLotSameOrder = "same_lot_per_order"

	UnitCentimetre = "cm"
	UnitGSM        = "g/m2"
	UnitPercent    = "pct"

	// MaxVariants is the most colour and size cells one revision holds.
	MaxVariants = 400
)

// MeasuredValue is one technical parameter. Value is a QuantityFormat decimal. Measured false is
// an estimate and is stored as an estimate.
type MeasuredValue struct {
	Value            string
	Unit             string
	Measured         bool
	SourceEvidenceID string
}

// ItemVariant is one cell of the matrix.
type ItemVariant struct {
	ColorCode    string
	SizeCode     string
	SkuID        string
	SpecRevision int64
}

// SaveItem creates a style at ExpectedVersion 0 or replaces its draft revision at the current root
// version. DocumentProfile and StyleCode are fixed by the first save. StyleCode is unique in the
// company (BUSINESS_KEY_CONFLICT). Each cell's SKU specification must be that SKU's current
// confirmed revision.
type SaveItem struct {
	IdempotencyKey      string
	ItemID              string
	ExpectedVersion     int64
	DocumentProfile     string
	StyleCode           string
	FabricComposition   string
	FabricSpec          string
	FabricWidth         MeasuredValue
	FabricGSM           MeasuredValue
	WarpShrinkage       MeasuredValue
	WeftShrinkage       MeasuredValue
	DyeLotPolicy        string
	SizeChartEvidenceID string
	Variants            []ItemVariant
}

// ConfirmItem publishes draft revision Revision. Human only.
type ConfirmItem struct {
	IdempotencyKey  string
	ItemID          string
	ExpectedVersion int64
	Revision        int64
	EvidenceIDs     []string
}

// ItemRevisionView is one stored revision.
type ItemRevisionView struct {
	Revision            int64
	Status              string
	ContentHash         string
	FabricComposition   string
	FabricSpec          string
	FabricWidth         MeasuredValue
	FabricGSM           MeasuredValue
	WarpShrinkage       MeasuredValue
	WeftShrinkage       MeasuredValue
	DyeLotPolicy        string
	SizeChartEvidenceID string
	Variants            []ItemVariant
	EvidenceIDs         []string
	SavedBy             kernel.ActorRef
	ConfirmedBy         kernel.ActorRef
}

// ItemView is a style and its revisions, current revision last among the confirmed ones by number.
type ItemView struct {
	ItemID          string
	DocumentProfile string
	StyleCode       string
	RootVersion     int64
	CurrentRevision int64
	Revisions       []ItemRevisionView
}

// ItemCommands is the style command entry point.
type ItemCommands interface {
	SaveItem(ctx context.Context, cmd SaveItem) (Receipt, error)
	ConfirmItem(ctx context.Context, cmd ConfirmItem) (Receipt, error)
}

// ItemQueries is the style read.
type ItemQueries interface {
	GetItem(ctx context.Context, itemID string) (ItemView, error)
}

func (c SaveItem) Validate() error {
	if err := head(c.IdempotencyKey, c.ItemID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	switch c.DocumentProfile {
	case "", ProfileGeneric, ProfileApparel:
	default:
		return schema("/args/document_profile")
	}
	if !kernel.ValidIdentifier(c.StyleCode) {
		return schema("/args/style_code")
	}
	if !text(c.FabricComposition, 200) {
		return schema("/args/fabric_composition")
	}
	if c.FabricSpec != "" && !text(c.FabricSpec, 200) {
		return schema("/args/fabric_spec")
	}
	if err := measured(c.FabricWidth, UnitCentimetre, true, "/args/fabric_width"); err != nil {
		return err
	}
	if err := measured(c.FabricGSM, UnitGSM, true, "/args/fabric_gsm"); err != nil {
		return err
	}
	if err := measured(c.WarpShrinkage, UnitPercent, false, "/args/warp_shrinkage"); err != nil {
		return err
	}
	if err := measured(c.WeftShrinkage, UnitPercent, false, "/args/weft_shrinkage"); err != nil {
		return err
	}
	switch c.DyeLotPolicy {
	case DyeLotNone, DyeLotSameColor, DyeLotSameOrder:
	default:
		return schema("/args/dye_lot_policy")
	}
	if c.SizeChartEvidenceID != "" && !kernel.ValidIdentifier(c.SizeChartEvidenceID) {
		return schema("/args/size_chart_evidence_id")
	}
	if len(c.Variants) < 1 || len(c.Variants) > MaxVariants {
		return schema("/args/variants")
	}
	colors := map[string]bool{}
	skus := map[string]bool{}
	for i, v := range c.Variants {
		p := "/args/variants/" + strconv.Itoa(i)
		if !kernel.ValidIdentifier(v.ColorCode) || !kernel.ValidIdentifier(v.SizeCode) {
			return schema(p)
		}
		if !kernel.ValidIdentifier(v.SkuID) || v.SpecRevision < 1 {
			return schema(p + "/sku_id")
		}
		cell := v.ColorCode + "\x00" + v.SizeCode
		if colors[cell] || skus[v.SkuID] {
			return schema(p)
		}
		colors[cell], skus[v.SkuID] = true, true
	}
	return nil
}

func (c ConfirmItem) Validate() error {
	if err := head(c.IdempotencyKey, c.ItemID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if c.Revision < 1 {
		return schema("/args/revision")
	}
	return evidenceIDs(c.EvidenceIDs, "/args/evidence_ids")
}

func measured(v MeasuredValue, unit string, positive bool, path string) error {
	d, err := kernel.ParseDecimal(v.Value, kernel.QuantityFormat)
	if err != nil || v.Unit != unit || !kernel.ValidIdentifier(v.SourceEvidenceID) {
		return schema(path)
	}
	if positive {
		if d.Sign() <= 0 {
			return schema(path)
		}
		return nil
	}
	hundred, err := kernel.ParseDecimal("100", kernel.QuantityFormat)
	if err != nil || d.Sign() < 0 || d.Cmp(hundred) > 0 {
		return schema(path)
	}
	return nil
}

func evidenceIDs(ids []string, path string) error {
	if len(ids) < 1 || len(ids) > MaxEvidence {
		return schema(path)
	}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) || slices.Index(ids, id) != i {
			return schema(path)
		}
	}
	return nil
}
