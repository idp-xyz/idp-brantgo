package api

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func invalid(path, reason string) error {
	return kernel.Reject(kernel.CodeInvalidKnownValue, path, reason)
}

// Reason reports whether s is a reason the contracts accept.
func Reason(s string) bool {
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

func custody(s, path string) error {
	switch s {
	case CustodyOwnWarehouse, CustodyExternalFactoryMaterial, CustodyExternalFactoryWIP, CustodyInTransit:
		return nil
	}
	return invalid(path, "")
}

func quantity(s, path string) (kernel.Decimal, error) {
	q, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil || q.Sign() <= 0 {
		return kernel.Decimal{}, schema(path)
	}
	return q, nil
}

func unit(id string, revision int64, path string) error {
	if err := idErr(id, path+"/unit_id"); err != nil {
		return err
	}
	if revision <= 0 {
		return schema(path + "/unit_revision")
	}
	return nil
}

func idErr(s, path string) error { return id(s, path) }

func when(s, path string) error {
	if s == "" {
		return nil
	}
	if _, err := kernel.ParseTimestamp(s); err != nil {
		return schema(path)
	}
	return nil
}

func money(currency string, minor int, amount, path string) error {
	if _, err := kernel.NewCurrency(currency, minor); err != nil {
		return schema(path + "/currency")
	}
	m, err := kernel.ParseMinor(kernel.MustCurrency(currency, minor), amount)
	if err != nil || m.Sign() < 0 || !m.Minor().IsInt64() {
		return schema(path + "/unit_cost_minor")
	}
	return nil
}

func basis(s, path string) error {
	q, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil || q.Sign() <= 0 {
		return schema(path)
	}
	return nil
}

func reasonOptional(s, path string) error {
	if s == "" {
		return nil
	}
	if !Reason(s) {
		return schema(path)
	}
	return nil
}

func balance(sku, location, batch, custodyKind, path string) error {
	if err := id(sku, path+"/sku_id"); err != nil {
		return err
	}
	if err := id(location, path+"/location_id"); err != nil {
		return err
	}
	if err := id(batch, path+"/batch_id"); err != nil {
		return err
	}
	return custody(custodyKind, path+"/custody_kind")
}

func lineage(links []LineageLink, unitID string, revision int64) error {
	if len(links) > 20 {
		return schema("/args/lineage")
	}
	for i, l := range links {
		p := "/args/lineage/" + strconv.Itoa(i)
		if _, err := quantity(l.Quantity, p+"/quantity"); err != nil {
			return err
		}
		batch := l.OutputBatchID != ""
		ship := l.ShipmentID != "" || l.ShipmentLineID != ""
		switch {
		case !kernel.ValidIdentifier(l.InputBatchID):
			return schema(p + "/input_batch_id")
		case batch == ship:
			return invalid(p, "")
		case batch && !kernel.ValidIdentifier(l.OutputBatchID):
			return schema(p + "/output_batch_id")
		case ship && (!kernel.ValidIdentifier(l.ShipmentID) || !kernel.ValidIdentifier(l.ShipmentLineID)):
			return schema(p + "/shipment_id")
		}
	}
	_ = unitID
	_ = revision
	return nil
}

func sourcePair(kind, sourceID, lineID string) error {
	switch kind {
	case "", "manual":
		if sourceID != "" || lineID != "" {
			return schema("/args/source_id")
		}
		return nil
	case SourceCommitment, SourceHoldForReuse:
		if err := id(sourceID, "/args/source_id"); err != nil {
			return err
		}
		return id(lineID, "/args/source_line_id")
	case SourceDelivery, SourceDeliveryLine, SourceShipmentLine:
		return invalid("/args/source_kind", "")
	default:
		if kind != "" && !kernel.ValidIdentifier(kind) {
			return schema("/args/source_kind")
		}
		if err := optionalID(sourceID, "/args/source_id"); err != nil {
			return err
		}
		return optionalID(lineID, "/args/source_line_id")
	}
}

// Validate checks the command's shape. The service calls it before the ledger.
func (c ConfigureModule) Validate() error {
	if c.ExpectedVersion < 0 {
		return schema("/args/expected_version")
	}
	return nil
}

// Validate checks a receipt.
func (c ReceiveStock) Validate() error {
	if err := id(c.MovementID, "/args/movement_id"); err != nil {
		return err
	}
	if err := balance(c.SkuID, c.LocationID, c.BatchID, c.CustodyKind, "/args"); err != nil {
		return err
	}
	if _, err := quantity(c.Quantity, "/args/quantity"); err != nil {
		return err
	}
	if err := unit(c.UnitID, c.UnitRevision, "/args"); err != nil {
		return err
	}
	if err := money(c.Currency, c.MinorUnits, c.UnitCostMinor, "/args"); err != nil {
		return err
	}
	if err := basis(c.PriceBasisQty, "/args/price_basis_qty"); err != nil {
		return err
	}
	if err := sourcePair(c.SourceKind, c.SourceID, c.SourceLineID); err != nil {
		return err
	}
	if err := reasonOptional(c.Reason, "/args/reason"); err != nil {
		return err
	}
	if err := when(c.ReceivedAt, "/args/received_at"); err != nil {
		return err
	}
	return lineage(c.Lineage, c.UnitID, c.UnitRevision)
}

// Validate checks an issue. A shipment line is not an argument a person may send (STK-INV-002).
func (c IssueStock) Validate() error {
	if err := id(c.MovementID, "/args/movement_id"); err != nil {
		return err
	}
	if err := balance(c.SkuID, c.LocationID, c.BatchID, c.CustodyKind, "/args"); err != nil {
		return err
	}
	if _, err := quantity(c.Quantity, "/args/quantity"); err != nil {
		return err
	}
	if err := unit(c.UnitID, c.UnitRevision, "/args"); err != nil {
		return err
	}
	if err := optionalID(c.ReservationID, "/args/reservation_id"); err != nil {
		return err
	}
	if c.SourceKind == SourceShipmentLine {
		return invalid("/args/source_kind", "")
	}
	if err := optionalID(c.SourceKind, "/args/source_kind"); err != nil {
		return err
	}
	if err := optionalID(c.SourceID, "/args/source_id"); err != nil {
		return err
	}
	if err := optionalID(c.SourceLineID, "/args/source_line_id"); err != nil {
		return err
	}
	if (c.SourceID == "") != (c.SourceLineID == "") && c.SourceKind != "" {
		return schema("/args/source_line_id")
	}
	if err := reasonOptional(c.Reason, "/args/reason"); err != nil {
		return err
	}
	return when(c.IssuedAt, "/args/issued_at")
}

// Validate checks an adjustment. A reason is required. adjust_in carries a cost layer.
func (c AdjustStock) Validate() error {
	if err := id(c.MovementID, "/args/movement_id"); err != nil {
		return err
	}
	if c.Direction != AdjustIn && c.Direction != AdjustOut {
		return invalid("/args/direction", "")
	}
	if err := balance(c.SkuID, c.LocationID, c.BatchID, c.CustodyKind, "/args"); err != nil {
		return err
	}
	if _, err := quantity(c.Quantity, "/args/quantity"); err != nil {
		return err
	}
	if err := unit(c.UnitID, c.UnitRevision, "/args"); err != nil {
		return err
	}
	if !Reason(c.Reason) {
		return schema("/args/reason")
	}
	if err := when(c.AdjustedAt, "/args/adjusted_at"); err != nil {
		return err
	}
	if c.Direction == AdjustIn {
		if err := money(c.Currency, c.MinorUnits, c.UnitCostMinor, "/args"); err != nil {
			return err
		}
		return basis(c.PriceBasisQty, "/args/price_basis_qty")
	}
	if c.Currency != "" || c.UnitCostMinor != "" || c.PriceBasisQty != "" || c.MinorUnits != 0 {
		return schema("/args/unit_cost_minor")
	}
	return nil
}

// Validate checks a reservation. OrderID and OrderLineID are both set or both empty.
func (c PlaceReservation) Validate() error {
	if err := id(c.ReservationID, "/args/reservation_id"); err != nil {
		return err
	}
	if err := balance(c.SkuID, c.LocationID, c.BatchID, c.CustodyKind, "/args"); err != nil {
		return err
	}
	if _, err := quantity(c.Quantity, "/args/quantity"); err != nil {
		return err
	}
	if err := unit(c.UnitID, c.UnitRevision, "/args"); err != nil {
		return err
	}
	if (c.OrderID == "") != (c.OrderLineID == "") {
		return schema("/args/order_line_id")
	}
	if err := optionalID(c.OrderID, "/args/order_id"); err != nil {
		return err
	}
	return optionalID(c.OrderLineID, "/args/order_line_id")
}

// Validate checks a release.
func (c ReleaseReservation) Validate() error {
	if err := id(c.ReservationID, "/args/reservation_id"); err != nil {
		return err
	}
	if c.ExpectedVersion < 1 {
		return schema("/args/expected_version")
	}
	return nil
}

// Validate checks a handover. A transfer or a return names a different destination.
func (c RecordHandover) Validate() error {
	switch c.Kind {
	case HandoverTransfer, HandoverConsume, HandoverReturn, HandoverScrap:
	default:
		return invalid("/args/kind", "")
	}
	if err := id(c.MovementID, "/args/movement_id"); err != nil {
		return err
	}
	if err := balance(c.SkuID, c.LocationID, c.BatchID, c.CustodyKind, "/args"); err != nil {
		return err
	}
	if _, err := quantity(c.Quantity, "/args/quantity"); err != nil {
		return err
	}
	if err := unit(c.UnitID, c.UnitRevision, "/args"); err != nil {
		return err
	}
	if !Reason(c.Reason) {
		return schema("/args/reason")
	}
	in := c.Kind == HandoverTransfer || c.Kind == HandoverReturn
	if !in {
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
	if err := balance(c.SkuID, c.ToLocationID, c.ToBatchID, c.ToCustodyKind, "/args"); err != nil {
		return err
	}
	if c.ToLocationID == c.LocationID && c.ToBatchID == c.BatchID && c.ToCustodyKind == c.CustodyKind {
		return invalid("/args/to_location_id", "")
	}
	return nil
}

// Validate checks a balance read.
func (k BalanceKey) Validate() error {
	return balance(k.SkuID, k.LocationID, k.BatchID, k.CustodyKind, "/")
}

// Validate checks an availability read. Quantity empty reports the balance without a comparison.
func (q AvailabilityQuery) Validate() error {
	if err := balance(q.SkuID, q.LocationID, q.BatchID, q.CustodyKind, "/"); err != nil {
		return err
	}
	if q.Quantity == "" {
		return nil
	}
	_, err := quantity(q.Quantity, "/quantity")
	return err
}
