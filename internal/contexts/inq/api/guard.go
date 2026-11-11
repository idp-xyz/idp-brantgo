package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// RfqRef v1 (Context Map R08 INQ→CST, R12 INQ→TRD) is a ready RFQ at an exact row version with its
// complete line set. A downstream context reads it before its own transaction through RfqLookups
// and binds RfqID, RowVersion and LinesHash; inside its transaction it re-checks the binding
// through RfqGuard v1, the guard function INQ publishes in its schema.

// CodeReferenceStale (409) means the RFQ a downstream fact bound is no longer ready at that row
// version with that line set: it was cancelled or closed, or the binding names another version.
const CodeReferenceStale = "REFERENCE_STALE"

// RfqRefLine is one line of an RFQ as RfqRef v1 carries it: what a costing or a quote must cover.
// SkuSpecRevision is the CAT specification revision the confirmation saw (0 before it).
type RfqRefLine struct {
	LineID          string
	SkuID           string
	SkuSpecRevision int64
	Quantity        string // decimal string as the RFQ stores it
	UOM             string
}

// RfqRef is what INQ says about one RFQ now: its status and row version, the customer party and
// the contact it asks for (empty while a draft lacks them; a ready RFQ has both), and its lines in
// line-ID order with their LinesHash. The customer and contact are as the RFQ row stores them at
// RowVersion; LinesHash and RfqGuard v1 cover the line set only.
type RfqRef struct {
	RfqID      string
	RowVersion int64
	Status     string
	CustomerID string
	ContactID  string
	Lines      []RfqRefLine
	LinesHash  string
}

// Ready reports whether the RFQ is ready: its scope is confirmed and its lines no longer change.
func (r RfqRef) Ready() bool { return r.Status == StatusReady }

// RfqLookups answers RfqRef v1 lookups of downstream contexts. It needs no permission beyond an
// active grant; RFQs that do not exist in the caller's company or lie outside its data scope are
// absent. Ask before opening your own transaction; inside it, use RfqGuard.
type RfqLookups interface {
	ResolveRfqs(ctx context.Context, ids []string) (map[string]RfqRef, error)
}

// RfqLinesHash is the fingerprint of a line set that RfqGuard v1 computes in SQL: the SHA-256, in
// lower-case hex, of the lines in byte order of their line IDs, each line its line ID, SKU ID,
// specification revision (empty for 0), quantity and unit joined by U+001F, the lines joined by
// U+001E. An empty line set hashes the empty string.
func RfqLinesHash(lines []RfqRefLine) string {
	sorted := slices.Clone(lines)
	slices.SortFunc(sorted, func(a, b RfqRefLine) int { return strings.Compare(a.LineID, b.LineID) })
	parts := make([]string, len(sorted))
	for i, l := range sorted {
		revision := ""
		if l.SkuSpecRevision != 0 {
			revision = strconv.FormatInt(l.SkuSpecRevision, 10)
		}
		parts[i] = strings.Join([]string{l.LineID, l.SkuID, revision, l.Quantity, l.UOM}, "\x1f")
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1e")))
	return hex.EncodeToString(sum[:])
}

// RfqGuard v1 (ADR-BR-004 option B, Context Map R08 "ready 询价与完整行集合", guard lock) lets a
// downstream context re-check, inside its own transaction, that the RFQ it binds is still ready at
// the row version it bound, with the line set it bound, and keeps it so until that transaction
// ends. INQ publishes it as a SECURITY DEFINER function in its schema with a fixed search_path
// (migrations/inq/000002_inq_rfq_guard.sql):
//
//	brant_inq.guard_rfq_ready_v1(p_rfq_id text, p_row_version bigint, p_lines_hash text)
//	    RETURNS TABLE (known boolean, status text, row_version bigint, lines_hash text, code text)
//
// It always returns one row. It locks the RFQ root row FOR SHARE in the caller's transaction, in
// the caller's scope (app.tenant_id and app.company_id, which bento.ScopeTransactor sets). Every
// INQ command that ends a ready RFQ (rfq.cancel, rfq.close) updates that row, so it either committed
// before the guard read the row, and the guard reports the RFQ stale, or waits until the caller's
// transaction ends. Lines change only while an RFQ is a draft, so a ready RFQ keeps its line set;
// the guard recomputes LinesHash anyway, so a binding of the wrong lines is stale too.
//
// A downstream command locks its own aggregate root FOR UPDATE first and calls the guard after
// (ADR-BR-011), with its own executor:
//
//	row := exec.QueryRow(ctx, inqapi.RfqGuardSQL, inqapi.RfqGuardArgs(binding)...)
//	verdict, err := inqapi.ScanRfqGuard(binding, row.Scan)
//	if err == nil {
//		err = verdict.Check("/args/rfq_ref")
//	}
//
// The guard does not apply the caller's data scope (own, team): the downstream checks visibility
// when it reads the RFQ through RfqLookups before its transaction.
const (
	RfqGuardFunction = "brant_inq.guard_rfq_ready_v1"
	RfqGuardSQL      = `SELECT known, status, row_version, lines_hash, code FROM brant_inq.guard_rfq_ready_v1($1, $2, $3)`
)

// RfqBinding is what a downstream fact binds of RfqRef v1: the RFQ, its row version and the hash
// of its line set.
type RfqBinding struct {
	RfqID      string
	RowVersion int64
	LinesHash  string
}

// Binding returns the binding of r.
func (r RfqRef) Binding() RfqBinding {
	return RfqBinding{RfqID: r.RfqID, RowVersion: r.RowVersion, LinesHash: r.LinesHash}
}

// RfqGuardArgs returns the arguments of RfqGuardSQL for b.
func RfqGuardArgs(b RfqBinding) []any { return []any{b.RfqID, b.RowVersion, b.LinesHash} }

// RfqGuardVerdict is the guard's answer for Binding. Known is false when no such RFQ exists in the
// caller's company (Code REFERENCE_INVALID); otherwise Status, RowVersion and LinesHash are the
// RFQ's now, and Code is empty when it is ready at the bound row version with the bound line set,
// REFERENCE_STALE otherwise.
type RfqGuardVerdict struct {
	Binding    RfqBinding
	Known      bool
	Status     string
	RowVersion int64
	LinesHash  string
	Code       string
}

// ScanRfqGuard reads the row of RfqGuardSQL for b through scan, such as a pgx row's Scan.
func ScanRfqGuard(b RfqBinding, scan func(dest ...any) error) (RfqGuardVerdict, error) {
	v := RfqGuardVerdict{Binding: b}
	var status, hash, code *string
	var version *int64
	if err := scan(&v.Known, &status, &version, &hash, &code); err != nil {
		return RfqGuardVerdict{}, err
	}
	if status != nil {
		v.Status = *status
	}
	if version != nil {
		v.RowVersion = *version
	}
	if hash != nil {
		v.LinesHash = *hash
	}
	if code != nil {
		v.Code = *code
	}
	return v, nil
}

// Ready reports whether the RFQ is ready as bound.
func (v RfqGuardVerdict) Ready() bool { return v.Known && v.Code == "" }

// Check returns nil when the RFQ is ready as bound, and INQ's rejection otherwise at path of the
// caller's request: REFERENCE_INVALID for an unknown RFQ, REFERENCE_STALE (409) for one that is no
// longer ready at the bound row version with the bound line set.
func (v RfqGuardVerdict) Check(path string) error {
	switch {
	case !v.Known:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	case v.Code != "":
		return kernel.RejectAs(CodeReferenceStale, kernel.CategoryConflict, path, "The RFQ is no longer ready at the bound version")
	}
	return nil
}
