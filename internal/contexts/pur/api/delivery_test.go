package api

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The closed contracts of the supplier delivery commands (S5-15, api/delivery.go): a claim states
// commitment lines once each with a positive quantity, a UN/LOCODE, a UTC timestamp and evidence; a
// new delivery states no reason and a correction one; a verification states lines with zero or more,
// a rejection none and its reason.
func TestSupplierDeliveryContracts(t *testing.T) {
	ev := []Evidence{{SourceID: "receipt.1", Locator: "page 1"}}
	run := func(name string, err error, want string) {
		t.Helper()
		if got := outcome(err); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	rec := RecordSupplierDelivery{IdempotencyKey: "k", DeliveryID: "dl.1", CommitmentID: "c.1",
		Lines: []DeliveryClaim{{OrderLineID: "L1", Quantity: "60"}, {OrderLineID: "L2", Quantity: "0.5"}}, HandoverPlace: "CNNGB",
		HandedOverAt: "2026-11-12T09:00:00Z", Evidence: ev, FileIDs: []string{"file.1"}, SupplierReference: "DN-7"}
	run("record", rec.Validate(), "OK ")
	for want, edit := range map[string]func(c *RecordSupplierDelivery){
		"SCHEMA_INVALID /args/delivery_id":           func(c *RecordSupplierDelivery) { c.DeliveryID = "dl 1" },
		"SCHEMA_INVALID /args/expected_version":      func(c *RecordSupplierDelivery) { c.ExpectedVersion = -1 },
		"SCHEMA_INVALID /args/commitment_id":         func(c *RecordSupplierDelivery) { c.CommitmentID = "" },
		"SCHEMA_INVALID /args/lines":                 func(c *RecordSupplierDelivery) { c.Lines = nil },
		"SCHEMA_INVALID /args/lines/1/quantity":      func(c *RecordSupplierDelivery) { c.Lines[1].Quantity = "0" },
		"SCHEMA_INVALID /args/lines/0/order_line_id": func(c *RecordSupplierDelivery) { c.Lines[0].OrderLineID = "L 1" },
		"DUPLICATE_LINE_ID /args/lines/1/order_line_id": func(c *RecordSupplierDelivery) {
			c.Lines[1].OrderLineID = "L1"
		},
		"SCHEMA_INVALID /args/handover_place":     func(c *RecordSupplierDelivery) { c.HandoverPlace = "Ningbo" },
		"SCHEMA_INVALID /args/handed_over_at":     func(c *RecordSupplierDelivery) { c.HandedOverAt = "2026-11-12T17:00:00+08:00" },
		"SCHEMA_INVALID /args/file_ids/1":         func(c *RecordSupplierDelivery) { c.FileIDs = []string{"file.1", "file.1"} },
		"SCHEMA_INVALID /args/supplier_reference": func(c *RecordSupplierDelivery) { c.SupplierReference = strings.Repeat("x", 65) },
		"SCHEMA_INVALID /args/reason":             func(c *RecordSupplierDelivery) { c.Reason = "A new delivery has none" },
		"SCHEMA_INVALID /args/evidence":           func(c *RecordSupplierDelivery) { c.Evidence = nil },
	} {
		c := rec
		c.Lines = append([]DeliveryClaim{}, rec.Lines...)
		edit(&c)
		run(want, c.Validate(), want)
	}
	correction := rec
	correction.ExpectedVersion = 2
	run("correction without reason", correction.Validate(), "SCHEMA_INVALID /args/reason")
	correction.Reason = "The receipt counted 40"
	run("correction", correction.Validate(), "OK ")

	for place, want := range map[string]bool{"CNNGB": true, "USNY2": true, "DEHAM": true, "CN0GB": false, "cnngb": false, "C1NGB": false, "CNNG": false} {
		if Location(place) != want {
			t.Errorf("Location(%q) = %v", place, !want)
		}
	}

	verify := VerifySupplierDelivery{IdempotencyKey: "k", DeliveryID: "dl.1", ExpectedVersion: 1, Decision: DecisionVerify,
		Lines: []DeliveryVerifiedLine{{OrderLineID: "L1", VerifiedQuantity: "60"}, {OrderLineID: "L2", VerifiedQuantity: "0"}}}
	run("verify", verify.Validate(), "OK ")
	for want, edit := range map[string]func(c *VerifySupplierDelivery){
		"SCHEMA_INVALID /args/expected_version":          func(c *VerifySupplierDelivery) { c.ExpectedVersion = 0 },
		"SCHEMA_INVALID /args/decision":                  func(c *VerifySupplierDelivery) { c.Decision = "accept" },
		"SCHEMA_INVALID /args/lines":                     func(c *VerifySupplierDelivery) { c.Lines = nil },
		"SCHEMA_INVALID /args/lines/1/verified_quantity": func(c *VerifySupplierDelivery) { c.Lines[1].VerifiedQuantity = "-1" },
		"DUPLICATE_LINE_ID /args/lines/1/order_line_id":  func(c *VerifySupplierDelivery) { c.Lines[1].OrderLineID = "L1" },
		"SCHEMA_INVALID /args/evidence":                  func(c *VerifySupplierDelivery) { c.Evidence = []Evidence{} },
		"SCHEMA_INVALID /args/reason":                    func(c *VerifySupplierDelivery) { c.Reason = " " },
	} {
		c := verify
		c.Lines = append([]DeliveryVerifiedLine{}, verify.Lines...)
		edit(&c)
		run(want, c.Validate(), want)
	}
	reject := VerifySupplierDelivery{IdempotencyKey: "k", DeliveryID: "dl.1", ExpectedVersion: 1, Decision: DecisionReject,
		Reason: "The receipt shows another container"}
	run("reject", reject.Validate(), "OK ")
	noReason := reject
	noReason.Reason = ""
	run("reject without reason", noReason.Validate(), "SCHEMA_INVALID /args/reason")
	withLines := reject
	withLines.Lines = verify.Lines
	run("reject with lines", withLines.Validate(), "SCHEMA_INVALID /args/lines")
}

// SupplierDeliveryGuard v1 (R59): an unknown line scans from a row of NULLs and is
// SUPPLIER_DELIVERY_UNVERIFIED; a claim is SUPPLIER_DELIVERY_UNVERIFIED; a superseded version is
// REFERENCE_STALE and names the version verified now; the verified version serves. Lookup refs are
// bounded and checked.
func TestSupplierDeliveryGuardVerdicts(t *testing.T) {
	ref := DeliveryLineRef{DeliveryID: "dl.1", LineID: "L1", Version: 1}
	if args := SupplierDeliveryGuardArgs(ref); len(args) != 3 || args[0] != "dl.1" || args[1] != "L1" || args[2] != int64(1) {
		t.Fatalf("args %v", args)
	}
	if !strings.Contains(SupplierDeliveryGuardSQL, SupplierDeliveryGuardFunction+"($1, $2, $3)") {
		t.Fatal("the SQL calls another function")
	}
	scan := func(row ...any) func(dest ...any) error {
		return func(dest ...any) error {
			if len(dest) != len(row) {
				t.Fatalf("scan of %d columns into %d", len(row), len(dest))
			}
			for i, d := range dest {
				switch p := d.(type) {
				case *bool:
					*p = row[i].(bool)
				case **string:
					if s, ok := row[i].(string); ok {
						*p = &s
					}
				case **int64:
					if n, ok := row[i].(int64); ok {
						*p = &n
					}
				}
			}
			return nil
		}
	}
	unknown, err := ScanSupplierDeliveryGuard(ref, scan(false, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, CodeSupplierDeliveryUnverified))
	if err != nil || unknown.Known || unknown.Usable() || unknown.Ref != ref || outcome(unknown.Check("/p")) != "SUPPLIER_DELIVERY_UNVERIFIED /p" {
		t.Fatalf("unknown %+v %v", unknown, err)
	}
	row := func(status string, verified int64, quantity any, code any) []any {
		return []any{true, status, verified, "c.1", "o.1", "L1", "s.1", "sku.1", int64(2), "pcs", int64(1), quantity, code}
	}
	served, err := ScanSupplierDeliveryGuard(ref, scan(row("verified", 1, "60", nil)...))
	if err != nil || !served.Usable() || served.Check("/p") != nil || served.VerifiedQuantity != "60" || served.CommitmentID != "c.1" ||
		served.SpecRevision != 2 || served.UnitRevision != 1 || served.VerifiedVersion != 1 {
		t.Fatalf("served %+v %v", served, err)
	}
	claimed, _ := ScanSupplierDeliveryGuard(ref, scan(row("recorded", 0, nil, CodeSupplierDeliveryUnverified)...))
	if claimed.Usable() || outcome(claimed.Check("/p")) != "SUPPLIER_DELIVERY_UNVERIFIED /p" {
		t.Fatalf("claimed %+v", claimed)
	}
	stale, _ := ScanSupplierDeliveryGuard(ref, scan(row("superseded", 2, "60", CodeReferenceStale)...))
	r, _ := kernel.AsRejection(stale.Check("/p"))
	if stale.Usable() || r == nil || r.Code != CodeReferenceStale || r.Category != kernel.CategoryConflict || !strings.Contains(r.Fields[0].Reason, "version 2") {
		t.Fatalf("stale %+v %+v", stale, r)
	}
	if (DeliveryLine{Status: "verified"}).Check("/p") != nil ||
		outcome(DeliveryLine{Status: "rejected", Code: CodeSupplierDeliveryUnverified}.Check("/p")) != "SUPPLIER_DELIVERY_UNVERIFIED /p" {
		t.Fatal("delivery line")
	}

	if ValidateDeliveryLineRefs([]DeliveryLineRef{ref}) != nil ||
		outcome(ValidateDeliveryLineRefs([]DeliveryLineRef{{DeliveryID: "dl.1", LineID: "L1"}})) != "SCHEMA_INVALID /refs/0/version" ||
		outcome(ValidateDeliveryLineRefs([]DeliveryLineRef{{DeliveryID: "dl.1", LineID: "L 1", Version: 1}})) != "SCHEMA_INVALID /refs/0/line_id" ||
		outcome(ValidateDeliveryLineRefs(make([]DeliveryLineRef, MaxDeliveryLineRefs+1))) != "SCHEMA_INVALID /refs" {
		t.Fatal("refs")
	}
	for _, code := range []string{CodeSupplierDeliveryExceeded, CodeSupplierDeliveryUnverified} {
		if c, ok := Category(code); !ok || c != kernel.CategoryConflict {
			t.Errorf("%s: %s", code, c)
		}
	}
}
