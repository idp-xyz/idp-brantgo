package sku

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return "OK"
}

func TestConfirmationNeedsNameAndPCSAndCountsRevisions(t *testing.T) {
	s := New("sku.1", "alice", "")
	s.ReplaceDraft("", UnitPCS)
	if got := code(s.Confirm()); got != "DRAFT_NOT_READY /args/name" {
		t.Fatal(got)
	}
	s.ReplaceDraft("ST01 red S", "")
	if got := code(s.Confirm()); got != "DRAFT_NOT_READY /args/base_uom" {
		t.Fatal(got)
	}
	s.ReplaceDraft("ST01 red S", UnitPCS)
	if err := s.Confirm(); err != nil {
		t.Fatal(err)
	}
	s.Reopen()
	s.ReplaceDraft("ST01 red S v2", UnitPCS)
	if err := s.Confirm(); err != nil {
		t.Fatal(err)
	}
	if s.Revision != 2 || s.RowVersion != 7 || s.Status != Confirmed {
		t.Fatalf("%+v", s)
	}
	if got := code(CheckSave(s, true, 7)); got != "INVALID_STATE /args/id" {
		t.Fatal(got)
	}
}

// CAT-INV-001 and the SKU half of Python test_foundation.py
// test_master_revision_invalidates_old_sku_profile_for_new_conversion: after a revision and a new
// confirmation the old specification revision is no longer current for new use. Python answers
// REFERENCE_INVALID through its sku_profile row version; the error catalog's REFERENCE_STALE (the
// reference's version no longer applies; refresh and resubmit) is the code for it here.
func TestOnlyTheCurrentConfirmedSpecificationRevisionIsReferenced(t *testing.T) {
	spec := func(s *Sku, visible bool, rev int64) string { return code(CheckSpec(s, visible, rev)) }
	s := New("sku", "alice", "")
	s.ReplaceDraft("测试产品", UnitPCS)
	if got := spec(nil, true, 1); got != "REFERENCE_INVALID /sku_id" {
		t.Fatal("an unknown SKU:", got)
	}
	if got := spec(s, true, 1); got != "REFERENCE_INVALID /spec_revision" {
		t.Fatal("a draft never confirmed is never referenced:", got)
	}
	if err := s.Confirm(); err != nil {
		t.Fatal(err)
	}
	if got := spec(s, true, 1); got != "OK" {
		t.Fatal("confirmed r1:", got)
	}
	if got := spec(s, false, 1); got != "REFERENCE_INVALID /sku_id" {
		t.Fatal("outside the data scope:", got)
	}
	if got := spec(s, true, 2); got != "REFERENCE_INVALID /spec_revision" {
		t.Fatal("a revision never confirmed:", got)
	}
	s.Reopen() // sku.revise_spec
	if got := spec(s, true, 1); got != "REFERENCE_STALE /spec_revision" {
		t.Fatal("r1 under revision:", got)
	}
	if err := s.Confirm(); err != nil {
		t.Fatal(err)
	}
	if old, now := spec(s, true, 1), spec(s, true, 2); old != "REFERENCE_STALE /spec_revision" || now != "OK" {
		t.Fatalf("after r2: r1 %s, r2 %s", old, now)
	}
	if rej, _ := kernel.AsRejection(CheckSpec(s, true, 1)); rej.Category != kernel.CategoryConflict {
		t.Fatalf("REFERENCE_STALE is 409 in the error catalog: %+v", rej)
	}
	s.Deactivate()
	if got := spec(s, true, 2); got != "REFERENCE_INVALID /sku_id" {
		t.Fatal("an inactive SKU has no revision for new business:", got)
	}
}
