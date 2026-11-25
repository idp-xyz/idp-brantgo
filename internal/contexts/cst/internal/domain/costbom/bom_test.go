package costbom

import (
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func human(id string) kernel.ActorRef { return kernel.ActorRef{Kind: kernel.ActorHuman, ID: id} }

func sample() Content {
	return Content{Profile: "apparel", BaseSize: "M", SkuID: "sku.1", SpecRevision: 1, Lines: []Line{{
		LineID: "L1", MaterialKind: "shell_fabric", MaterialCode: "FAB.1", Unit: "M", GradingMode: GradeSizeGraded,
		ConsumptionPerUnit: "1.45", WastageRatio: "0.03", ShrinkageFactor: "1.04", FabricWidthCM: "150", FabricGSM: "180",
		Measured: true, FormulaBasis: "width_length_m",
		Grades: []Grade{{SizeCode: "S", ConsumptionPerUnit: "1.45"}, {SizeCode: "2XL", ConsumptionPerUnit: "1.78"}},
	}}}
}

// A size the BOM does not store is refused. The base size M is not used in its place.
func TestSizeGradeDoesNotFallBackToTheBaseSize(t *testing.T) {
	line := sample().Lines[0]
	if got, err := line.Consumption("S"); err != nil || got != "1.45" {
		t.Fatalf("S: %s %v", got, err)
	}
	if got, err := line.Consumption("2XL"); err != nil || got != "1.78" {
		t.Fatalf("2XL: %s %v", got, err)
	}
	got, err := line.Consumption("3XL")
	rej, ok := kernel.AsRejection(err)
	if got != "" || !ok || rej.Code != CodeSizeGradeMissing {
		t.Fatalf("3XL: %q %v", got, err)
	}
	uniform := line
	uniform.GradingMode = GradeUniform
	uniform.Grades = nil
	uniform.ConsumptionPerUnit = "1.50"
	if got, err := uniform.Consumption("3XL"); err != nil || got != "1.50" {
		t.Fatalf("uniform: %s %v", got, err)
	}
}

// The drafter does not verify. Another human freezes a hash that changes when the content changes.
func TestVerifyRefusesTheDrafterAndFreezesTheHash(t *testing.T) {
	b := New("bom.1", "drafter", "trade")
	if _, err := b.Save(sample(), human("drafter"), 1, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify(1, human("drafter"), 2, time.Unix(2, 0)); err == nil {
		t.Fatal("drafter verified")
	}
	r, err := b.Verify(1, human("checker"), 2, time.Unix(2, 0))
	if err != nil || r.Hash == "" || b.Status != StatusVerified || slices.Contains(r.Contributors, "checker") {
		t.Fatalf("%v %+v", err, r)
	}
	again := sample()
	again.Lines[0].Grades[1].ConsumptionPerUnit = "1.79"
	b2 := New("bom.2", "drafter", "")
	if _, err := b2.Save(again, human("drafter"), 1, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	r2, err := b2.Verify(1, human("checker"), 2, time.Unix(2, 0))
	if err != nil || r2.Hash == r.Hash {
		t.Fatalf("hash changed with the size grade: %v %s %s", err, r.Hash, r2.Hash)
	}
}
