package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// ExportLicenseGuard v1 (Context Map R56 CNX->FUL, Customer/Supplier with a guard function,
// guardLock true; CNX-INV-002, FUL-INV-002, UL-INV-28/31, ADR-BR-014) lets FUL's commercial release
// (S5-07), when the trade compliance conclusion says the goods need a licence
// (TradeComplianceVerdict.LicenseRequired), re-check inside its own transaction that an export
// licence allocation covers the shipment's lines, and keep it so until that transaction ends. CNX
// publishes it as a SECURITY DEFINER function in its schema with a fixed search_path
// (migrations/cnx/000002_cnx_export_licences.sql):
//
//	brant_cnx.guard_export_license_v1(p_shipment_id text, p_at timestamptz)
//	    RETURNS TABLE (covered boolean, status text, reason text, license_id text, allocation_id text, quantity text,
//	                   unit text, value_minor text, currency text, lines_hash text, valid_until timestamptz, code text)
//
// It always returns one row, in the caller's scope (app.tenant_id and app.company_id), for the
// shipment's current allocation at the instant p_at the caller relies on it. It locks the licence
// row FOR SHARE, then the allocation row FOR SHARE, in the caller's transaction:
// export_license.allocate and .release lock the licence FOR UPDATE, so each either committed before
// the guard read it, and the verdict says so, or waits until the caller's transaction ends. A
// shipment without an allocation locks nothing. Covered is true, and code NULL, only for an
// allocation with quantity outstanding on a licence whose validity contains p_at; otherwise code is
// EXPORT_LICENSE_REQUIRED and status and reason say why: required (not_allocated, released — an
// external result released all of it) or expired (expired). quantity and value_minor are what
// remains outstanding (decimal in unit, minor units of currency); lines_hash covers the shipment
// lines the allocation was made for (LinesHash).
//
// S5R-D04 (one licence per declaration; the same signature and columns, v1): every line of the
// shipment is covered by the shipment's own allocation or by the allocation of the export lot that
// holds it. When the shipment's own allocation has nothing outstanding (or there is none) and its
// export lots hold allocations, those cover it together: the guard locks their licences FOR SHARE
// in licence ID order, then their allocations in allocation ID order, and answers allocated with
// reason lots_allocated when each has all its quantity outstanding on a licence valid at p_at —
// license_id and allocation_id list them comma-separated (allocation ID order), quantity and
// value_minor sum what is outstanding when the licences share unit and currency (NULL otherwise),
// valid_until is the earliest and lines_hash covers the shipment lines the lots' lines come from
// (the lot lines of one shipment line summed) — or required/released, required/quantity_exceeded
// (one released in part) or expired/expired with EXPORT_LICENSE_REQUIRED.
//
// The guard cannot see FUL's lines, so the caller passes them to Check: they must be the lines the
// allocation covers (same hash) and their quantity no more than what is outstanding; for
// lots_allocated, the shipment's lines must hash to what the lots' allocations cover together
// (otherwise lines_not_covered: a lot without an allocation, or lines changed). A caller locks
// its own aggregate roots FOR UPDATE first and calls the guard among the upstream guards in context
// code order (CNX first: CNX, PTY, PUR, QLT, STL, TRD; ADR-BR-011), after
// TradeComplianceGuard v1:
//
//	row := exec.QueryRow(ctx, cnxapi.ExportLicenseGuardSQL, cnxapi.ExportLicenseGuardArgs(shipmentID, now)...)
//	verdict, err := cnxapi.ScanExportLicenseGuard(shipmentID, row.Scan)
//	if err == nil {
//		err = verdict.Check("/args/shipment_id", lines) // []cnxapi.ShipmentLine of the shipment
//	}
//	// verdict.Evidence() is what the caller's commit evidence records under ExportLicenseGuardName.
const (
	ExportLicenseGuardFunction = "brant_cnx.guard_export_license_v1"
	ExportLicenseGuardSQL      = `SELECT covered, status, reason, license_id, allocation_id, quantity, unit, value_minor, currency, lines_hash,
		valid_until, code FROM brant_cnx.guard_export_license_v1($1, $2)`
	// ExportLicenseGuardName is the guard's name in a caller's commit evidence.
	ExportLicenseGuardName = "cnx.export_license_guard.v1"
)

// The guard's states and reasons. Check adds lines_changed and quantity_exceeded for lines the
// allocation does not cover.
const (
	LicenseStatusAllocated = "allocated"
	LicenseStatusRequired  = "required"
	LicenseStatusExpired   = "expired"

	LicenseReasonAllocated        = "allocated"
	LicenseReasonNotAllocated     = "not_allocated"
	LicenseReasonReleased         = "released"
	LicenseReasonExpired          = "expired"
	LicenseReasonLinesChanged     = "lines_changed"
	LicenseReasonQuantityExceeded = "quantity_exceeded"
	// S5R-D04: the allocations of the shipment's export lots cover it together; a line no lot
	// allocation covers.
	LicenseReasonLotsAllocated   = "lots_allocated"
	LicenseReasonLinesNotCovered = "lines_not_covered"
)

// ExportLicenseGuardArgs returns the arguments of ExportLicenseGuardSQL for shipmentID at at.
func ExportLicenseGuardArgs(shipmentID string, at time.Time) []any {
	return []any{shipmentID, kernel.Instant(at)}
}

// ShipmentLine is a shipment line as FUL holds it: the line, its SKU specification revision and its
// quantity (a decimal of at most 6 fractional digits) in Unit.
type ShipmentLine struct {
	LineID       string
	SkuID        string
	SpecRevision int64
	Quantity     string
	Unit         string
}

// LinesHash is the SHA-256 (hex) of lines in line-ID order, one "line_id|sku_id|spec_revision|
// quantity|unit" per line joined by "\n", the quantity with six fractional digits: what an
// allocation records of the lines it covers and what Check compares.
func LinesHash(lines []ShipmentLine) (string, error) {
	sorted := slices.Clone(lines)
	slices.SortFunc(sorted, func(a, b ShipmentLine) int { return strings.Compare(a.LineID, b.LineID) })
	parts := make([]string, 0, len(sorted))
	for _, l := range sorted {
		q, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if err != nil {
			return "", err
		}
		parts = append(parts, l.LineID+"|"+l.SkuID+"|"+strconv.FormatInt(l.SpecRevision, 10)+"|"+q.StringFixed(6)+"|"+l.Unit)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

// ExportLicenseVerdict is the guard's answer for ShipmentID. LicenseID, AllocationID, Quantity,
// Unit, ValueMinor, Currency, LinesHash and ValidUntil are the current allocation's ("" and zero
// without one).
type ExportLicenseVerdict struct {
	ShipmentID   string
	Covered      bool
	Status       string
	Reason       string
	LicenseID    string
	AllocationID string
	Quantity     string
	Unit         string
	ValueMinor   string
	Currency     string
	LinesHash    string
	ValidUntil   time.Time
	Code         string
}

// ScanExportLicenseGuard reads the row of ExportLicenseGuardSQL for shipmentID through scan, such
// as a pgx row's Scan.
func ScanExportLicenseGuard(shipmentID string, scan func(dest ...any) error) (ExportLicenseVerdict, error) {
	v := ExportLicenseVerdict{ShipmentID: shipmentID}
	var status, reason, license, allocation, quantity, unit, value, currency, hash, code *string
	var until *time.Time
	if err := scan(&v.Covered, &status, &reason, &license, &allocation, &quantity, &unit, &value, &currency, &hash, &until, &code); err != nil {
		return ExportLicenseVerdict{}, err
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{status, &v.Status}, {reason, &v.Reason}, {license, &v.LicenseID}, {allocation, &v.AllocationID}, {quantity, &v.Quantity},
		{unit, &v.Unit}, {value, &v.ValueMinor}, {currency, &v.Currency}, {hash, &v.LinesHash}, {code, &v.Code}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	if until != nil {
		v.ValidUntil = kernel.Instant(*until)
	}
	return v, nil
}

// Check returns nil when the allocation covers lines — the guard says covered without a code,
// lines hash to the allocation's LinesHash and their quantity is no more than what is outstanding —
// and EXPORT_LICENSE_REQUIRED (409) at path of the caller's request otherwise, with the status and
// reason. For lots_allocated (S5R-D04) the lines must hash to what the export lots' allocations
// cover together; the guard already checked that each holds all its quantity.
func (v ExportLicenseVerdict) Check(path string, lines []ShipmentLine) error {
	if !v.Covered || v.Code != "" {
		return Reject(CodeExportLicenseRequired, path, "Export licence "+v.Status+": "+v.Reason)
	}
	hash, err := LinesHash(lines)
	if v.Reason == LicenseReasonLotsAllocated {
		if err != nil || hash != v.LinesHash {
			return Reject(CodeExportLicenseRequired, path, "Export licence "+v.Status+": "+LicenseReasonLinesNotCovered)
		}
		return nil
	}
	if err != nil || hash != v.LinesHash {
		return Reject(CodeExportLicenseRequired, path, "Export licence "+v.Status+": "+LicenseReasonLinesChanged)
	}
	outstanding, err := kernel.ParseDecimal(v.Quantity, kernel.QuantityFormat)
	if err != nil {
		return errors.Join(Reject(CodeExportLicenseRequired, path, "Export licence quantity unreadable"), err)
	}
	total := kernel.DecimalFromInt(0)
	for _, l := range lines {
		q, _ := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		total = total.Add(q)
		if l.Unit != v.Unit {
			return Reject(CodeExportLicenseRequired, path, "Export licence "+v.Status+": "+LicenseReasonLinesChanged)
		}
	}
	if total.Cmp(outstanding) > 0 {
		return Reject(CodeExportLicenseRequired, path, "Export licence "+v.Status+": "+LicenseReasonQuantityExceeded)
	}
	return nil
}

// Evidence is the value a caller's commit evidence records under ExportLicenseGuardName:
// status/reason/license_id/allocation_id/lines_hash.
func (v ExportLicenseVerdict) Evidence() string {
	return v.Status + "/" + v.Reason + "/" + v.LicenseID + "/" + v.AllocationID + "/" + v.LinesHash
}
