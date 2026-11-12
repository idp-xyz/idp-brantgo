package api

import (
	"errors"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func outcome(err error) string {
	if err == nil {
		return "OK"
	}
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return err.Error()
}

func prepare() PrepareShipment {
	return PrepareShipment{IdempotencyKey: "k", ShipmentID: "shipment.1", OrderID: "order.1", Route: RouteSupplierDirect,
		Lines: []ShipmentLine{{LineID: "S1", OrderLineID: "L1", Quantity: "200", SupplierDelivery: DeliveryLineRef{DeliveryID: "delivery.1",
			LineID: "L1", Version: 1}, QualityReleaseID: "rel.1"}}}
}

// The closed contract of shipment.prepare (SM-FUL-SHIPMENT, R26, R27, R59; D-10 routes).
func TestPrepareShipmentContract(t *testing.T) {
	for _, c := range []struct {
		edit func(*PrepareShipment)
		want string
	}{
		{func(*PrepareShipment) {}, "OK"},
		{func(c *PrepareShipment) { c.Route = RouteStock }, "OK"}, // refused by the domain: DEPENDENCY_NOT_SUPPORTED
		{func(c *PrepareShipment) { c.IdempotencyKey = "" }, "SCHEMA_INVALID /idempotency_key"},
		{func(c *PrepareShipment) { c.ShipmentID = "a b" }, "SCHEMA_INVALID /args/shipment_id"},
		{func(c *PrepareShipment) { c.OrderID = "" }, "SCHEMA_INVALID /args/order_id"},
		{func(c *PrepareShipment) { c.Route = "air" }, "SCHEMA_INVALID /args/route"},
		{func(c *PrepareShipment) { c.Lines = nil }, "SCHEMA_INVALID /args/lines"},
		{func(c *PrepareShipment) { c.Lines = make([]ShipmentLine, MaxLines+1) }, "SCHEMA_INVALID /args/lines"},
		{func(c *PrepareShipment) { c.Lines[0].LineID = "" }, "SCHEMA_INVALID /args/lines/0/line_id"},
		{func(c *PrepareShipment) { c.Lines = append(c.Lines, c.Lines[0]) }, "DUPLICATE_LINE_ID /args/lines/1/line_id"},
		{func(c *PrepareShipment) { c.Lines[0].OrderLineID = "" }, "SCHEMA_INVALID /args/lines/0/order_line_id"},
		{func(c *PrepareShipment) { c.Lines[0].Quantity = "0" }, "SCHEMA_INVALID /args/lines/0/quantity"},
		{func(c *PrepareShipment) { c.Lines[0].Quantity = "1.0000001" }, "SCHEMA_INVALID /args/lines/0/quantity"},
		{func(c *PrepareShipment) { c.Lines[0].SupplierDelivery.DeliveryID = "" }, "SCHEMA_INVALID /args/lines/0/supplier_delivery/delivery_id"},
		{func(c *PrepareShipment) { c.Lines[0].SupplierDelivery.LineID = "" }, "SCHEMA_INVALID /args/lines/0/supplier_delivery/line_id"},
		{func(c *PrepareShipment) { c.Lines[0].SupplierDelivery.Version = 0 }, "SCHEMA_INVALID /args/lines/0/supplier_delivery/version"},
		{func(c *PrepareShipment) { c.Lines[0].QualityReleaseID = "" }, "SCHEMA_INVALID /args/lines/0/quality_release_id"},
	} {
		cmd := prepare()
		cmd.Lines = append([]ShipmentLine{}, cmd.Lines...)
		c.edit(&cmd)
		if got := outcome(cmd.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", cmd, got, c.want)
		}
	}
}

func packing() RecordPacking {
	return RecordPacking{IdempotencyKey: "k", ShipmentID: "shipment.1", ExpectedVersion: 1, Packages: []Package{
		{Cartons: 10, CartonFrom: 1, Contents: []PackageContent{{LineID: "S1", QuantityPerCarton: "12"}}, GrossWeightKg: "10.5", NetWeightKg: "9.5", VolumeCbm: "0.05"},
		{Cartons: 8, CartonFrom: 11, Contents: []PackageContent{{LineID: "S1", QuantityPerCarton: "10"}}, GrossWeightKg: "9", NetWeightKg: "8", VolumeCbm: "0.05"},
	}, EvidenceIDs: []string{"packing.list"}}
}

// The closed contracts of packing.plan and packing.record_actual (UL FUL-ActualPackage: cartons,
// contents per carton, gross and net weight, volume, carton numbers).
func TestPackingContracts(t *testing.T) {
	for _, c := range []struct {
		edit func(*RecordPacking)
		want string
	}{
		{func(*RecordPacking) {}, "OK"},
		{func(c *RecordPacking) { c.ExpectedVersion = 0 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *RecordPacking) { c.Packages = nil }, "SCHEMA_INVALID /args/packages"},
		{func(c *RecordPacking) { c.Packages[0].Cartons = 0 }, "SCHEMA_INVALID /args/packages/0/cartons"},
		{func(c *RecordPacking) { c.Packages[0].CartonFrom = 0 }, "SCHEMA_INVALID /args/packages/0/carton_from"},
		{func(c *RecordPacking) { c.Packages[1].CartonFrom = 10 }, "INVALID_KNOWN_VALUE /args/packages/1/carton_from"},
		{func(c *RecordPacking) { c.Packages[0].CartonFrom = MaxCartonNumber }, "SCHEMA_INVALID /args/packages/0/carton_from"},
		{func(c *RecordPacking) { c.Packages[0].Contents = nil }, "SCHEMA_INVALID /args/packages/0/contents"},
		{func(c *RecordPacking) {
			c.Packages[0].Contents = append(c.Packages[0].Contents, c.Packages[0].Contents[0])
		}, "DUPLICATE_LINE_ID /args/packages/0/contents/1/line_id"},
		{func(c *RecordPacking) { c.Packages[0].Contents[0].QuantityPerCarton = "-1" }, "SCHEMA_INVALID /args/packages/0/contents/0/quantity_per_carton"},
		{func(c *RecordPacking) { c.Packages[0].GrossWeightKg = "" }, "SCHEMA_INVALID /args/packages/0/gross_weight_kg"},
		{func(c *RecordPacking) { c.Packages[0].VolumeCbm = "" }, "SCHEMA_INVALID /args/packages/0/volume_cbm"},
		{func(c *RecordPacking) { c.Packages[0].NetWeightKg = "11" }, "INVALID_KNOWN_VALUE /args/packages/0/net_weight_kg"},
		{func(c *RecordPacking) { c.EvidenceIDs = []string{"a", "a"} }, "SCHEMA_INVALID /args/evidence_ids/1"},
	} {
		cmd := packing()
		cmd.Packages = append([]Package{}, cmd.Packages...)
		for i := range cmd.Packages {
			cmd.Packages[i].Contents = append([]PackageContent{}, cmd.Packages[i].Contents...)
		}
		c.edit(&cmd)
		if got := outcome(cmd.Validate()); got != c.want {
			t.Errorf("%s: %s, want %s", c.want, got, c.want)
		}
	}
	plan := PlanPacking{IdempotencyKey: "k", ShipmentID: "shipment.1", ExpectedVersion: 1, Packages: []Package{{Cartons: 20,
		Contents: []PackageContent{{LineID: "S1", QuantityPerCarton: "10"}}}}}
	if err := plan.Validate(); err != nil {
		t.Fatal("a plan may leave numbers, weights and volume out:", err)
	}
	plan.Packages[0].GrossWeightKg = "0"
	if outcome(plan.Validate()) != "SCHEMA_INVALID /args/packages/0/gross_weight_kg" {
		t.Fatal(outcome(plan.Validate()))
	}
}

func TestCancelAndConfirmContracts(t *testing.T) {
	cancel := CancelShipment{IdempotencyKey: "k", ShipmentID: "shipment.1", ExpectedVersion: 1, Reason: "客户推迟"}
	if cancel.Validate() != nil {
		t.Fatal("cancel")
	}
	for _, r := range []string{"", " ", "a\x07", strings.Repeat("x", MaxReason+1)} {
		cancel.Reason = r
		if outcome(cancel.Validate()) != "SCHEMA_INVALID /args/reason" {
			t.Errorf("reason %q", r)
		}
	}
	confirm := ConfirmShipment{IdempotencyKey: "k", ShipmentID: "shipment.1", ExpectedVersion: 2, EvidenceIDs: []string{"bl"}}
	if confirm.Validate() != nil {
		t.Fatal("confirm")
	}
	confirm.EvidenceIDs = make([]string, MaxEvidence+1)
	if outcome(confirm.Validate()) != "SCHEMA_INVALID /args/evidence_ids" {
		t.Fatal(outcome(confirm.Validate()))
	}
	confirm.EvidenceIDs, confirm.ExpectedVersion = nil, maxVersion+1
	if outcome(confirm.Validate()) != "SCHEMA_INVALID /args/expected_version" {
		t.Fatal(outcome(confirm.Validate()))
	}
}

// The codes FUL declares and their catalog categories (error catalog V0.1: 409 conflict, 422 invalid).
func TestCodes(t *testing.T) {
	want := map[string]kernel.Category{CodeShipmentQuantityExceeded: kernel.CategoryConflict, CodeQualityReleaseRequired: kernel.CategoryConflict,
		CodeQualityReleaseInsufficient: kernel.CategoryConflict, CodeSupplierDeliveryUnverified: kernel.CategoryConflict,
		CodeSupplierDeliveryInsufficient: kernel.CategoryConflict, CodePartyScreeningNotClear: kernel.CategoryConflict,
		CodeOrderNotActive: kernel.CategoryInvalid, CodeOrderApprovalRequired: kernel.CategoryInvalid, CodeOrderApprovalRevoked: kernel.CategoryInvalid,
		CodeReferenceStale: kernel.CategoryConflict, CodeBusinessKeyConflict: kernel.CategoryConflict, CodeDependencyNotSupported: kernel.CategoryInvalid,
		CodeCommercialReleaseBlocked: kernel.CategoryConflict, CodeTradeComplianceNotClear: kernel.CategoryConflict,
		CodeExportLicenseRequired: kernel.CategoryConflict, CodeShipmentDocumentsUnreconciled: kernel.CategoryConflict,
		CodeDocumentIntegrityFailure: kernel.CategoryInvalid}
	got := Codes()
	if len(got) != len(want) {
		t.Fatal(got)
	}
	for code, c := range want {
		if cat, ok := Category(code); !ok || cat != c || got[code] != c {
			t.Errorf("%s: %s", code, cat)
		}
	}
	if r := Reject(CodeShipmentQuantityExceeded, "/x", ""); r.Category != kernel.CategoryConflict {
		t.Fatal(r)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an undeclared code panics")
		}
	}()
	_ = Reject("SURPRISE", "/", "")
}

// ShipmentFacts v1 as TRD reads it (R70, D-1).
func TestShipmentFactsScan(t *testing.T) {
	if !strings.Contains(OrderShippedSQL, OrderShippedFunction+"($1)") {
		t.Fatal("the SQL calls another function")
	}
	if args := OrderShippedArgs("order.1"); len(args) != 1 || args[0] != "order.1" {
		t.Fatal(args)
	}
	l, err := ScanOrderShipped(func(dest ...any) error {
		*dest[0].(*string), *dest[1].(*string), *dest[2].(*int64), *dest[3].(*string), *dest[4].(*int64) = "L1", "pcs", 1, "800", 2
		return nil
	})
	if err != nil || l != (ShippedLine{OrderLineID: "L1", UnitID: "pcs", UnitRevision: 1, Quantity: "800", Shipments: 2}) {
		t.Fatalf("%+v %v", l, err)
	}
	boom := errors.New("scan")
	if _, err = ScanOrderShipped(func(...any) error { return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if ValidateOrderIDs([]string{"order.1"}) != nil || outcome(ValidateOrderIDs([]string{"ok", ""})) != "SCHEMA_INVALID /order_ids/1" ||
		ValidateOrderIDs(make([]string, MaxShippedLookups+1)) == nil {
		t.Fatal("lookup ids")
	}
}
