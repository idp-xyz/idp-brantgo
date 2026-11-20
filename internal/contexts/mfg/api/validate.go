package api

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func invalid(path, reason string) error {
	return kernel.Reject(kernel.CodeInvalidKnownValue, path, reason)
}

func note(s string) bool {
	if s == "" {
		return true
	}
	n := utf8.RuneCountInString(s)
	return n <= MaxNote && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

func reason(s string) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= MaxReason && utf8.ValidString(s) && strings.TrimSpace(s) != "" && strings.IndexFunc(s, unicode.IsControl) < 0
}

func id(s, path string) error {
	if !kernel.ValidIdentifier(s) {
		return schema(path)
	}
	return nil
}

func optionalID(s, path string) error {
	if s == "" {
		return nil
	}
	return id(s, path)
}

func quantity(s, path string) error {
	d, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil || d.Sign() <= 0 {
		return schema(path)
	}
	return nil
}

func unit(id string, revision int64, path string) error {
	if err := idPath(id, path+"/unit_id"); err != nil {
		return err
	}
	if revision < 1 {
		return schema(path + "/unit_revision")
	}
	return nil
}

func idPath(s, path string) error { return id(s, path) }

func custody(s, path string) error {
	switch s {
	case "own_warehouse", "external_factory_material", "external_factory_wip", "in_transit":
		return nil
	default:
		return invalid(path, "")
	}
}

// Validate checks a configure command. ExpectedVersion 0 creates the setting.
func (c ConfigureModule) Validate() error {
	if c.ExpectedVersion < 0 {
		return schema("/args/expected_version")
	}
	return nil
}

// Validate checks a publish command. The body is required when the plan is created.
func (c PublishPlan) Validate() error {
	if err := id(c.PlanID, "/args/plan_id"); err != nil {
		return err
	}
	if c.ExpectedVersion < 0 {
		return schema("/args/expected_version")
	}
	switch c.Target {
	case StatusDraft, StatusPublished, StatusInProgress, StatusPaused, StatusCompleted, StatusCancelled:
	default:
		return invalid("/args/target", "")
	}
	if !note(c.Note) {
		return schema("/args/note")
	}
	if c.ExpectedVersion == 0 {
		if err := id(c.FactoryPartyID, "/args/factory_party_id"); err != nil {
			return err
		}
		switch c.Target {
		case StatusDraft, StatusPublished, StatusCancelled:
		default:
			return kernel.Reject(kernel.CodeInvalidState, "/args/target", "A new plan starts as draft")
		}
	}
	if c.Target == StatusPublished && c.ExpectedVersion == 0 && (len(c.Operations) == 0 || len(c.Materials) == 0) {
		return kernel.Reject(kernel.CodeDraftNotReady, "/args/operations", "Material responsibility and operation granularity are required")
	}
	seenOp := map[string]bool{}
	seenSeq := map[int64]bool{}
	for i, op := range c.Operations {
		p := index("/args/operations", i)
		if err := id(op.OperationID, p+"/operation_id"); err != nil {
			return err
		}
		if seenOp[op.OperationID] {
			return kernel.Reject(kernel.CodeDuplicateLineID, p+"/operation_id", "")
		}
		seenOp[op.OperationID] = true
		if op.Sequence < 1 || seenSeq[op.Sequence] {
			return schema(p + "/sequence")
		}
		seenSeq[op.Sequence] = true
		if n := utf8.RuneCountInString(op.Name); n < 1 || n > MaxName || strings.IndexFunc(op.Name, unicode.IsControl) >= 0 {
			return schema(p + "/name")
		}
		if err := id(op.SkuID, p+"/sku_id"); err != nil {
			return err
		}
		if op.SpecRevision < 1 {
			return schema(p + "/spec_revision")
		}
		if err := quantity(op.Quantity, p+"/quantity"); err != nil {
			return err
		}
		if err := unit(op.UnitID, op.UnitRevision, p); err != nil {
			return err
		}
	}
	seenMat := map[string]bool{}
	for i, m := range c.Materials {
		p := index("/args/materials", i)
		if err := id(m.MaterialID, p+"/material_id"); err != nil {
			return err
		}
		if seenMat[m.MaterialID] {
			return kernel.Reject(kernel.CodeDuplicateLineID, p+"/material_id", "")
		}
		seenMat[m.MaterialID] = true
		switch m.Kind {
		case MaterialFactoryPurchased, MaterialOurSupply, MaterialCustomerSupply, MaterialMixed:
		default:
			return invalid(p+"/kind", "")
		}
		if err := id(m.SkuID, p+"/sku_id"); err != nil {
			return err
		}
		if err := quantity(m.Quantity, p+"/quantity"); err != nil {
			return err
		}
		if err := unit(m.UnitID, m.UnitRevision, p); err != nil {
			return err
		}
		if n := utf8.RuneCountInString(m.LossBasis); n > MaxLoss || strings.IndexFunc(m.LossBasis, unicode.IsControl) >= 0 {
			return schema(p + "/loss_basis")
		}
	}
	return nil
}

// Validate checks an issue. CommitmentID empty cites no PUR demand. OrderID and OrderLineID are
// the commercial anchor; when a commitment is cited, OrderLineID is that demand's line.
func (c IssueSubcontract) Validate() error {
	if err := id(c.SubcontractID, "/args/subcontract_id"); err != nil {
		return err
	}
	if err := id(c.PlanID, "/args/plan_id"); err != nil {
		return err
	}
	if err := id(c.OperationID, "/args/operation_id"); err != nil {
		return err
	}
	if err := quantity(c.Quantity, "/args/quantity"); err != nil {
		return err
	}
	if err := unit(c.UnitID, c.UnitRevision, "/args"); err != nil {
		return err
	}
	if len(c.Currency) != 3 || strings.ToUpper(c.Currency) != c.Currency {
		return schema("/args/currency")
	}
	if c.MinorUnits < 0 || c.MinorUnits > 4 {
		return schema("/args/minor_units")
	}
	cur, err := kernel.NewCurrency(c.Currency, c.MinorUnits)
	if err != nil {
		return schema("/args/currency")
	}
	fee, err := kernel.ParseMinor(cur, c.ProcessingFeeMinor)
	if err != nil || fee.Sign() < 0 {
		return schema("/args/processing_fee_minor")
	}
	if err := optionalID(c.CommitmentID, "/args/commitment_id"); err != nil {
		return err
	}
	if err := id(c.OrderID, "/args/order_id"); err != nil {
		return err
	}
	return id(c.OrderLineID, "/args/order_line_id")
}

// Validate checks a progress claim.
func (c RecordProgress) Validate() error {
	if err := id(c.SubcontractID, "/args/subcontract_id"); err != nil {
		return err
	}
	if err := id(c.ProgressID, "/args/progress_id"); err != nil {
		return err
	}
	if c.ExpectedVersion < 1 {
		return schema("/args/expected_version")
	}
	if err := id(c.OperationID, "/args/operation_id"); err != nil {
		return err
	}
	switch c.ProgressKind {
	case StatusInProgress, StatusCompleted, StatusCancelled:
	default:
		return invalid("/args/progress_kind", "")
	}
	if err := quantity(c.Quantity, "/args/quantity"); err != nil {
		return err
	}
	if err := id(c.UnitID, "/args/unit_id"); err != nil {
		return err
	}
	if _, err := kernel.ParseTimestamp(c.ReportedAt); err != nil {
		return schema("/args/reported_at")
	}
	return nil
}

// Validate checks a handover. A transfer or a return names a different destination. Consume and
// scrap name none (MFG-INV-002).
func (c RecordMaterialHandover) Validate() error {
	if err := id(c.SubcontractID, "/args/subcontract_id"); err != nil {
		return err
	}
	switch c.Kind {
	case HandoverTransfer, HandoverConsume, HandoverReturn, HandoverScrap:
	default:
		return invalid("/args/kind", "A handover is a transfer, a consume, a return or a scrap")
	}
	if err := id(c.MovementID, "/args/movement_id"); err != nil {
		return err
	}
	if err := id(c.SkuID, "/args/sku_id"); err != nil {
		return err
	}
	if err := id(c.LocationID, "/args/location_id"); err != nil {
		return err
	}
	if err := id(c.BatchID, "/args/batch_id"); err != nil {
		return err
	}
	if err := custody(c.CustodyKind, "/args/custody_kind"); err != nil {
		return err
	}
	if err := quantity(c.Quantity, "/args/quantity"); err != nil {
		return err
	}
	if err := unit(c.UnitID, c.UnitRevision, "/args"); err != nil {
		return err
	}
	if !reason(c.Reason) {
		return schema("/args/reason")
	}
	moving := c.Kind == HandoverTransfer || c.Kind == HandoverReturn
	if !moving {
		if c.CounterMovementID != "" || c.ToLocationID != "" || c.ToBatchID != "" || c.ToCustodyKind != "" {
			return schema("/args/to_location_id")
		}
		return nil
	}
	if err := id(c.CounterMovementID, "/args/counter_movement_id"); err != nil {
		return err
	}
	if c.CounterMovementID == c.MovementID {
		return schema("/args/counter_movement_id")
	}
	if err := id(c.ToLocationID, "/args/to_location_id"); err != nil {
		return err
	}
	if err := id(c.ToBatchID, "/args/to_batch_id"); err != nil {
		return err
	}
	if err := custody(c.ToCustodyKind, "/args/to_custody_kind"); err != nil {
		return err
	}
	if c.ToLocationID == c.LocationID && c.ToBatchID == c.BatchID && c.ToCustodyKind == c.CustodyKind {
		return invalid("/args/to_location_id", "The destination is a different balance")
	}
	return nil
}

func index(path string, i int) string {
	return path + "/" + itoa(i)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
