package api

import (
	"errors"
	"strings"
	"testing"
)

// CommitmentShipped v1 (R82, PUR-INV-013): the SQL PUR runs after its commitment lock, and the scan
// of one row. The function body, its locks and its scope are the migration's and the PostgreSQL test's.
func TestCommitmentShippedSQLAndScan(t *testing.T) {
	if CommitmentShippedFunction != "brant_ful.commitment_shipped_v1" {
		t.Fatal(CommitmentShippedFunction)
	}
	if !strings.Contains(CommitmentShippedSQL, "FROM brant_ful.commitment_shipped_v1($1)") ||
		!strings.Contains(CommitmentShippedSQL, "order_line_id, unit_id, unit_revision, shipped_quantity, shipments") ||
		strings.Contains(strings.ToUpper(CommitmentShippedSQL), "FOR UPDATE") ||
		strings.Contains(strings.ToUpper(CommitmentShippedSQL), "FOR SHARE") {
		t.Fatal(CommitmentShippedSQL)
	}
	if args := CommitmentShippedArgs("commitment.1"); len(args) != 1 || args[0] != "commitment.1" {
		t.Fatalf("args %v", args)
	}
	got, err := ScanCommitmentShipped(func(dest ...any) error {
		*(dest[0].(*string)) = "L1"
		*(dest[1].(*string)) = "PCS"
		*(dest[2].(*int64)) = 3
		*(dest[3].(*string)) = "60"
		*(dest[4].(*int64)) = 2
		return nil
	})
	if err != nil || got != (CommitmentShipped{OrderLineID: "L1", UnitID: "PCS", UnitRevision: 3, Quantity: "60", Shipments: 2}) {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err = ScanCommitmentShipped(func(dest ...any) error { return errors.New("scan") }); err == nil || err.Error() != "scan" {
		t.Fatal(err)
	}
}
