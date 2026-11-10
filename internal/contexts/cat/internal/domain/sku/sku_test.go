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
