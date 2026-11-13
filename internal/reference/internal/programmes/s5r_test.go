package programmes

import (
	"slices"
	"testing"
)

// S5R-D15(a): regions are canonical (sorted) and part of the programme's content hash, which leaves
// them out when there are none, so a programme imported before regions existed hashes as before.
// The hashes of the programmes without regions were computed by the code of 90925f3 (the S5R base,
// before R2) and are pinned here.
func TestS5RRegionsAreCanonicalContent(t *testing.T) {
	a := NewProgramme("SYN-A", "Synthetic A", "embargo", []string{"AQ"}, "")
	b := NewProgramme("SYN-B", "Synthetic B", "sanctions", []string{"BV", "AQ"}, "goods")
	if a.Hash != "fbac5b20fee1ead12233916edfb289e7f338eee68bc710dcbcb0c8dae1543898" ||
		b.Hash != "87a59bd7aec0eae08d32243e77bdb8562043b0f56715b44be1163e8abc8f1589" ||
		ContentHash([]Programme{a, b}) != "ba9d99de5cc1dcf936d64f6e73c21755fe59719a1426baef6d31cc8dad781ca8" {
		t.Fatalf("programmes without regions hash as %s, %s", a.Hash, b.Hash)
	}
	if none := a.WithRegions(nil); none.Hash != a.Hash || none.Regions != nil || a.WithRegions([]string{}).Hash != a.Hash {
		t.Fatal("no regions are none")
	}
	r1, r2 := b.WithRegions([]string{"BV-02", "AQ-01"}), b.WithRegions([]string{"AQ-01", "BV-02"})
	if r1.Hash != r2.Hash || r1.Hash == b.Hash || !slices.Equal(r1.Regions, []string{"AQ-01", "BV-02"}) {
		t.Fatalf("regions %+v %+v", r1, r2)
	}
}
