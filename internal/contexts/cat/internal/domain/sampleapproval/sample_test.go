package sampleapproval

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func TestApproveIsIndependentAndRevokeFailsClosed(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	editor := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "editor"}
	inspector := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "inspector"}
	a := New("sample.1", "sku.1", StagePPSample, editor.ID, "")
	rev, fresh := a.Save(DraftContent{SpecRevision: 1, TechPackSHA256: "ab", EvidenceIDs: []string{"src.2", "src.1"}}, editor, 1, now)
	if !fresh || rev.Number != 1 || len(rev.EvidenceIDs) != 2 || rev.EvidenceIDs[0] != "src.1" {
		t.Fatalf("draft %+v", rev)
	}
	if _, err := a.CheckApprove(a.Version, 1, editor); err == nil {
		t.Fatal("the editor approved")
	}
	if _, err := a.CheckApprove(a.Version, 1, inspector); err != nil {
		t.Fatal(err)
	}
	a.Approve(inspector, 2, now)
	if a.Status != Approved || a.Approved == nil || a.Approved.SpecRevision != 1 || a.Current != 1 {
		t.Fatalf("approved %+v", a)
	}
	if _, err := a.CheckRevoke(a.Version); err != nil {
		t.Fatal(err)
	}
	a.Revoke(inspector, 3, "fabric changed", now)
	if a.Status != Revoked || a.Approved != nil {
		t.Fatalf("revoked %+v", a)
	}
	if _, err := a.CheckRevoke(a.Version); err == nil {
		t.Fatal("revoked twice")
	}
}
