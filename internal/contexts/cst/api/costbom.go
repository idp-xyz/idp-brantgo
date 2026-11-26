package api

import (
	"context"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Cost BOM commands (WBS S7-02). save_draft and substitution.request admit a human or an AI actor
// at the gateway; every writing HTTP route still refuses actor kind ai before the body. verify,
// withdraw and substitution.decide are human only.
const (
	CommandSaveCostBOM         = "cost_bom.save_draft"
	CommandVerifyCostBOM       = "cost_bom.verify"
	CommandWithdrawCostBOM     = "cost_bom.withdraw"
	CommandRequestSubstitution = "cost_bom.substitution.request"
	CommandDecideSubstitution  = "cost_bom.substitution.decide"
	EventCostBOMVerified       = "idp-brant.cst.cost_bom_verified.v1"
	EventCostBOMWithdrawn      = "idp-brant.cst.cost_bom_withdrawn.v1"
	EventCostBOMSubstitution   = "idp-brant.cst.cost_bom_substitution_decided.v1"
)

const (
	MaterialShell       = "shell_fabric"
	MaterialLining      = "lining"
	MaterialInterlining = "interlining"
	MaterialTrim        = "trim"
	MaterialPackaging   = "packaging"
	MaterialLabor       = "labor_cm"

	GradingUniform = "uniform"
	GradingSized   = "size_graded"

	FormulaDirect = "direct_qty"
	FormulaWidth  = "width_length_m"
	FormulaGSM    = "gsm_weight_kg"

	SurchargeDyeLot  = "dye_lot_mcq"
	SurchargeTrim    = "trim_moq_surcharge"
	SurchargeSample  = "sample_dev_fee"
	SurchargePattern = "pattern_fee"

	SurchargePerColor = "per_color"
	SurchargePerStyle = "per_style"
	SurchargePerOrder = "per_order"

	AllocationLump = "order_lump_sum"
	AllocationQty  = "allocate_by_qty"

	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

// SizeGrade is one size's net consumption. An empty delta means no size labour premium.
type SizeGrade struct {
	SizeCode           string
	ConsumptionPerUnit string
	SizeCMDeltaMinor   string
	Currency           string
	MinorUnits         int
}

// CostBOMLine is one material row.
type CostBOMLine struct {
	LineID             string
	MaterialKind       string
	MaterialCode       string
	Unit               string
	GradingMode        string
	ConsumptionPerUnit string
	WastageRatio       string
	ShrinkageFactor    string
	FabricWidthCM      string
	FabricGSM          string
	Measured           bool
	FormulaBasis       string
	Supply             *SupplyRef
	Grades             []SizeGrade
}

// CostBOMSurcharge is a dye-lot minimum or a sample or pattern fee. AmountMinor is optional;
// a threshold without an amount is stored and refused later by the estimator, not here.
type CostBOMSurcharge struct {
	SurchargeID  string
	Kind         string
	Scope        string
	ColorCode    string
	MaterialCode string
	MCQThreshold string
	MCQUnit      string
	AmountMinor  string
	Currency     string
	MinorUnits   int
	Allocation   string
	FeeCategory  string
}

// CostBOMData is the whole draft.
type CostBOMData struct {
	DocumentProfile string
	BaseSizeCode    string
	ItemID          string
	SkuID           string
	SpecRevision    int64
	Lines           []CostBOMLine
	Surcharges      []CostBOMSurcharge
}

// SaveCostBOM creates a cost BOM at expected version 0 or replaces its draft.
type SaveCostBOM struct {
	IdempotencyKey  string
	BomID           string `contract:"required,id"`
	ExpectedVersion int64  `contract:"min=0"`
	Data            CostBOMData
}

// VerifyCostBOM freezes the draft revision. The caller is not one of its drafters.
type VerifyCostBOM struct {
	IdempotencyKey  string
	BomID           string `contract:"required,id"`
	ExpectedVersion int64  `contract:"required"`
	Revision        int64  `contract:"required"`
}

// WithdrawCostBOM withdraws a verified or superseded revision.
type WithdrawCostBOM struct {
	IdempotencyKey  string
	BomID           string `contract:"required,id"`
	ExpectedVersion int64  `contract:"required"`
	Revision        int64  `contract:"required"`
	Reason          string `contract:"required"`
}

// RequestSubstitution records a material substitution against a verified BOM.
type RequestSubstitution struct {
	IdempotencyKey         string
	BomID                  string `contract:"required,id"`
	ExpectedVersion        int64  `contract:"required"`
	SubstitutionID         string `contract:"required,id"`
	OriginalLineID         string
	OriginalMaterialCode   string
	SubstituteMaterialCode string
	ApplicableQuantity     string
	TargetSpecRevision     int64
	CostDeltaMinor         string
	Currency               string
	MinorUnits             int
	LeadDaysDelta          int
	SampleImpact           bool
	Reason                 string
}

// DecideSubstitution approves or rejects a request. Approve writes a new BOM revision.
type DecideSubstitution struct {
	IdempotencyKey  string
	BomID           string `contract:"required,id"`
	ExpectedVersion int64  `contract:"required"`
	SubstitutionID  string `contract:"required,id"`
	Decision        string `contract:"enum=approve|reject"`
}

// CostBOMCommands is the cost BOM write side.
type CostBOMCommands interface {
	SaveCostBOM(ctx context.Context, cmd SaveCostBOM) (Receipt, error)
	VerifyCostBOM(ctx context.Context, cmd VerifyCostBOM) (Receipt, error)
	WithdrawCostBOM(ctx context.Context, cmd WithdrawCostBOM) (Receipt, error)
	RequestSubstitution(ctx context.Context, cmd RequestSubstitution) (Receipt, error)
	DecideSubstitution(ctx context.Context, cmd DecideSubstitution) (Receipt, error)
}

func (SaveCostBOM) cstCommand()         {}
func (VerifyCostBOM) cstCommand()       {}
func (WithdrawCostBOM) cstCommand()     {}
func (RequestSubstitution) cstCommand() {}
func (DecideSubstitution) cstCommand()  {}

// Validate checks the closed contract.
func (c SaveCostBOM) Validate() error {
	if err := head(c.IdempotencyKey, c.BomID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	return c.Data.validate("/args/data")
}

// Validate checks the closed contract.
func (c VerifyCostBOM) Validate() error {
	if err := head(c.IdempotencyKey, c.BomID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	return revision(c.Revision, "/args/revision")
}

// Validate checks the closed contract.
func (c WithdrawCostBOM) Validate() error {
	if err := head(c.IdempotencyKey, c.BomID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if err := revision(c.Revision, "/args/revision"); err != nil {
		return err
	}
	return reason(c.Reason)
}

// Validate checks the closed contract.
func (c RequestSubstitution) Validate() error {
	if err := head(c.IdempotencyKey, c.BomID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if !kernel.ValidIdentifier(c.SubstitutionID) {
		return schema("/args/substitution_id")
	}
	return substitutionBody(c, "/args")
}

// Validate checks the closed contract.
func (c DecideSubstitution) Validate() error {
	if err := head(c.IdempotencyKey, c.BomID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if !kernel.ValidIdentifier(c.SubstitutionID) {
		return schema("/args/substitution_id")
	}
	if c.Decision != DecisionApprove && c.Decision != DecisionReject {
		return schema("/args/decision")
	}
	return nil
}

func (d CostBOMData) validate(path string) error {
	switch d.DocumentProfile {
	case "", "generic", "apparel":
	default:
		return schema(path + "/document_profile")
	}
	if !kernel.ValidIdentifier(d.BaseSizeCode) {
		return schema(path + "/base_size_code")
	}
	if d.ItemID == "" && d.SkuID == "" {
		return schema(path + "/item_id")
	}
	if d.ItemID != "" && !kernel.ValidIdentifier(d.ItemID) {
		return schema(path + "/item_id")
	}
	if d.SkuID != "" && !kernel.ValidIdentifier(d.SkuID) {
		return schema(path + "/sku_id")
	}
	if (d.SkuID == "") != (d.SpecRevision == 0) {
		return schema(path + "/spec_revision")
	}
	if d.SpecRevision != 0 {
		if err := revision(d.SpecRevision, path+"/spec_revision"); err != nil {
			return err
		}
	}
	if len(d.Lines) < 1 || len(d.Lines) > MaxLines {
		return schema(path + "/lines")
	}
	seen := map[string]bool{}
	for i, l := range d.Lines {
		if err := l.validate(index(path+"/lines", i)); err != nil {
			return err
		}
		if seen[l.LineID] {
			return schema(index(path+"/lines", i) + "/line_id")
		}
		seen[l.LineID] = true
	}
	if len(d.Surcharges) > MaxRequiredFees {
		return schema(path + "/surcharges")
	}
	fees := map[string]bool{}
	for i, s := range d.Surcharges {
		if err := s.validate(index(path+"/surcharges", i)); err != nil {
			return err
		}
		if fees[s.SurchargeID] {
			return schema(index(path+"/surcharges", i) + "/surcharge_id")
		}
		fees[s.SurchargeID] = true
	}
	return nil
}

func (l CostBOMLine) validate(path string) error {
	switch {
	case !kernel.ValidIdentifier(l.LineID):
		return schema(path + "/line_id")
	case !oneOf(l.MaterialKind, MaterialShell, MaterialLining, MaterialInterlining, MaterialTrim, MaterialPackaging, MaterialLabor):
		return schema(path + "/material_kind")
	case !kernel.ValidIdentifier(l.MaterialCode):
		return schema(path + "/material_code")
	case !kernel.ValidIdentifier(l.Unit):
		return schema(path + "/unit")
	case l.GradingMode != GradingUniform && l.GradingMode != GradingSized:
		return schema(path + "/grading_mode")
	case !decimalOK(l.ConsumptionPerUnit, kernel.QuantityFormat, true):
		return schema(path + "/consumption_per_unit")
	case !decimalOK(l.WastageRatio, kernel.PriceFormat, false):
		return schema(path + "/wastage_ratio")
	case !decimalOK(l.ShrinkageFactor, kernel.PriceFormat, true):
		return schema(path + "/shrinkage_factor")
	case l.FabricWidthCM != "" && !decimalOK(l.FabricWidthCM, kernel.QuantityFormat, true):
		return schema(path + "/fabric_width_cm")
	case l.FabricGSM != "" && !decimalOK(l.FabricGSM, kernel.QuantityFormat, true):
		return schema(path + "/fabric_gsm")
	case !oneOf(l.FormulaBasis, FormulaDirect, FormulaWidth, FormulaGSM):
		return schema(path + "/formula_basis")
	}
	if l.FormulaBasis == FormulaWidth && l.FabricWidthCM == "" {
		return schema(path + "/fabric_width_cm")
	}
	if l.FormulaBasis == FormulaGSM && l.FabricGSM == "" {
		return schema(path + "/fabric_gsm")
	}
	if err := supply(l.Supply, path+"/supply_ref"); err != nil {
		return err
	}
	if l.GradingMode == GradingUniform {
		if len(l.Grades) != 0 {
			return schema(path + "/grades")
		}
		return nil
	}
	if len(l.Grades) < 1 || len(l.Grades) > 40 {
		return schema(path + "/grades")
	}
	sizes := map[string]bool{}
	for i, g := range l.Grades {
		p := index(path+"/grades", i)
		switch {
		case !kernel.ValidIdentifier(g.SizeCode):
			return schema(p + "/size_code")
		case !decimalOK(g.ConsumptionPerUnit, kernel.QuantityFormat, true):
			return schema(p + "/consumption_per_unit")
		case sizes[g.SizeCode]:
			return schema(p + "/size_code")
		}
		sizes[g.SizeCode] = true
		if g.SizeCMDeltaMinor != "" || g.Currency != "" || g.MinorUnits != 0 {
			if err := money(g.SizeCMDeltaMinor, g.Currency, g.MinorUnits, p+"/size_cm_delta_minor"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s CostBOMSurcharge) validate(path string) error {
	switch {
	case !kernel.ValidIdentifier(s.SurchargeID):
		return schema(path + "/surcharge_id")
	case !oneOf(s.Kind, SurchargeDyeLot, SurchargeTrim, SurchargeSample, SurchargePattern):
		return schema(path + "/kind")
	case !oneOf(s.Scope, SurchargePerColor, SurchargePerStyle, SurchargePerOrder):
		return schema(path + "/scope")
	case s.ColorCode != "" && !kernel.ValidIdentifier(s.ColorCode):
		return schema(path + "/color_code")
	case s.MaterialCode != "" && !kernel.ValidIdentifier(s.MaterialCode):
		return schema(path + "/material_code")
	case (s.MCQThreshold == "") != (s.MCQUnit == ""):
		return schema(path + "/mcq_threshold")
	case s.MCQThreshold != "" && !decimalOK(s.MCQThreshold, kernel.QuantityFormat, true):
		return schema(path + "/mcq_threshold")
	case s.MCQUnit != "" && !kernel.ValidIdentifier(s.MCQUnit):
		return schema(path + "/mcq_unit")
	case !oneOf(s.Allocation, AllocationLump, AllocationQty):
		return schema(path + "/allocation")
	case s.FeeCategory != "" && !kernel.ValidIdentifier(s.FeeCategory):
		return schema(path + "/fee_category")
	}
	if s.AmountMinor != "" || s.Currency != "" || s.MinorUnits != 0 {
		return money(s.AmountMinor, s.Currency, s.MinorUnits, path+"/amount_minor")
	}
	return nil
}

func substitutionBody(c RequestSubstitution, path string) error {
	switch {
	case !kernel.ValidIdentifier(c.OriginalLineID):
		return schema(path + "/original_line_id")
	case !kernel.ValidIdentifier(c.OriginalMaterialCode):
		return schema(path + "/original_material_code")
	case !kernel.ValidIdentifier(c.SubstituteMaterialCode):
		return schema(path + "/substitute_material_code")
	case !decimalOK(c.ApplicableQuantity, kernel.QuantityFormat, true):
		return schema(path + "/applicable_quantity")
	case c.TargetSpecRevision < 1:
		return schema(path + "/target_spec_revision")
	case c.LeadDaysDelta < -3650 || c.LeadDaysDelta > 3650:
		return schema(path + "/lead_days_delta")
	case !text(c.Reason, maxText):
		return schema(path + "/reason")
	}
	return money(c.CostDeltaMinor, c.Currency, c.MinorUnits, path+"/cost_delta_minor")
}

func supply(r *SupplyRef, path string) error {
	if r == nil {
		return nil
	}
	if !kernel.ValidIdentifier(r.Kind) || !kernel.ValidIdentifier(r.ID) {
		return schema(path)
	}
	return revision(r.Revision, path+"/revision")
}

func oneOf(s string, allowed ...string) bool {
	for _, a := range allowed {
		if s == a {
			return true
		}
	}
	return false
}

func decimalOK(s string, f kernel.DecimalFormat, positiveOnly bool) bool {
	d, err := kernel.ParseDecimal(s, f)
	if err != nil {
		return false
	}
	if positiveOnly {
		return d.Sign() > 0
	}
	return d.Sign() >= 0
}

func money(minor, currency string, units int, path string) error {
	c, err := kernel.NewCurrency(currency, units)
	if err != nil {
		return schema(path)
	}
	if _, err = kernel.ParseMinor(c, minor); err != nil {
		return schema(path)
	}
	return nil
}
