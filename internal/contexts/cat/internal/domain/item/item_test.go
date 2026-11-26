package item

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func tech() Tech {
	return Tech{
		FabricComposition: "cotton",
		FabricWidth:       Param{Value: "150", Unit: "cm", Measured: true, SourceID: "src.1"},
		FabricGSM:         Param{Value: "180", Unit: "g/m2", Measured: false, SourceID: "src.1"},
		WarpShrinkage:     Param{Value: "3", Unit: "pct", Measured: false, SourceID: "src.1"},
		WeftShrinkage:     Param{Value: "2", Unit: "pct", Measured: false, SourceID: "src.1"},
		DyeLotPolicy:      "none",
		Variants:          []Variant{{ColorCode: "NAVY", SizeCode: "M", SkuID: "sku.1", SpecRevision: 1}},
	}
}

func TestConfirmKeepsAnUnmeasuredParameter(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	editor := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "merch.1"}
	it := New("item.1", "apparel", "STYLE-1", editor.ID, "")
	rev, fresh := it.Save(tech(), editor, 1, now)
	if !fresh || rev.Number != 1 || rev.Tech.FabricGSM.Measured || rev.Tech.WarpShrinkage.Measured {
		t.Fatalf("draft %+v fresh %v", rev, fresh)
	}
	if _, err := it.CheckConfirm(it.Version, 1); err != nil {
		t.Fatal(err)
	}
	it.Confirm(editor, 2, []string{"src.1"}, now)
	if it.Draft != nil || it.Confirmed == nil || it.Confirmed.Tech.FabricGSM.Measured || it.Current != 1 {
		t.Fatalf("confirmed %+v draft %v", it.Confirmed, it.Draft)
	}
	again, fresh := it.Save(tech(), editor, 3, now)
	if !fresh || again.Number != 2 {
		t.Fatalf("next draft %+v %v", again, fresh)
	}
	old := it.Confirm(editor, 4, []string{"src.1"}, now)
	if old == nil || old.Status != Superseded || it.Current != 2 || it.Confirmed.Number != 2 {
		t.Fatalf("supersede %+v current %d", old, it.Current)
	}
}

func TestStyleCodeIsFixed(t *testing.T) {
	it := New("item.1", "generic", "STYLE-1", "h", "")
	it.Save(tech(), kernel.ActorRef{Kind: kernel.ActorHuman, ID: "h"}, 1, time.Now())
	if err := CheckSave(it, it.Version, "apparel", "STYLE-1"); err == nil {
		t.Fatal("profile changed")
	}
	if CheckStyle(true) == nil || CheckStyle(false) != nil {
		t.Fatal("style code")
	}
}
