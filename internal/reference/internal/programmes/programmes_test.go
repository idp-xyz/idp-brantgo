package programmes

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// E-3, ADR-BR-017: a programme is canonical (countries sorted) and hashed over its content; a
// version's hash is over its programmes in ID order. Python V0.50 has no country programmes, so
// the vectors come from the design (UL CNX-EmbargoCheck, role template country_programme.import).
func TestProgrammeIsCanonicalAndHashed(t *testing.T) {
	a := NewProgramme("SYN-EMB-1", "Synthetic embargo programme", "embargo", []string{"BV", "AQ"}, "")
	b := NewProgramme("SYN-EMB-1", "Synthetic embargo programme", "embargo", []string{"AQ", "BV"}, "")
	if a.Hash != b.Hash || len(a.Hash) != 64 || !slices.Equal(a.Countries, []string{"AQ", "BV"}) {
		t.Fatalf("country order must not change the content: %+v %+v", a, b)
	}
	// The canonical JSON fixes the field order, sorts the countries and states an absent scope as "":
	// {"programme_id":"SYN-EMB-1","name":"Synthetic embargo programme","measure":"embargo","countries":["AQ","BV"],"scope":""}
	sum := sha256.Sum256([]byte(`{"programme_id":"SYN-EMB-1","name":"Synthetic embargo programme","measure":"embargo","countries":["AQ","BV"],"scope":""}`))
	if a.Hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("canonical hash %s", a.Hash)
	}
	pairs := sha256.Sum256([]byte(`[["SYN-EMB-1","` + a.Hash + `"]]`))
	if ContentHash([]Programme{a}) != hex.EncodeToString(pairs[:]) {
		t.Fatal("a version hashes its [programme_id, content_hash] pairs")
	}
	for _, changed := range []Programme{
		NewProgramme("SYN-EMB-1", "Synthetic embargo programme", "embargo", []string{"AQ", "BV", "HM"}, ""),
		NewProgramme("SYN-EMB-1", "Synthetic embargo programme", "arms_embargo", []string{"AQ", "BV"}, ""),
		NewProgramme("SYN-EMB-1", "Synthetic embargo programme", "embargo", []string{"AQ", "BV"}, "All goods"),
		NewProgramme("SYN-EMB-1", "Synthetic embargo programme 2", "embargo", []string{"AQ", "BV"}, ""),
		NewProgramme("SYN-EMB-2", "Synthetic embargo programme", "embargo", []string{"AQ", "BV"}, ""),
	} {
		if changed.Hash == a.Hash {
			t.Errorf("%+v hashes like %+v", changed, a)
		}
	}
	other := NewProgramme("SYN-ARM-1", "Synthetic arms embargo", "arms_embargo", []string{"HM"}, "Arms and related materiel")
	if ContentHash([]Programme{a, other}) != ContentHash([]Programme{other, a}) || ContentHash([]Programme{a}) == ContentHash([]Programme{other}) {
		t.Fatal("a version's content hash is over its programmes in ID order")
	}
	if got := Countries([]Programme{a, other, b}); !slices.Equal(got, []string{"AQ", "BV", "HM"}) {
		t.Fatalf("distinct countries %v", got)
	}
	if NewProgramme("P", "n", "embargo", nil, "").Countries == nil {
		t.Fatal("countries are never null")
	}
}

func TestImportRules(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	v := &Version{ListCode: "cn.country_programmes", Version: "v1", PublishedOn: day(1)}
	first, err := Import(nil, "CN", false, nil, v)
	if err != nil || first.RowVersion != 1 || first.Current != "v1" || v.ImportSeq != 1 || first.Jurisdiction != "CN" {
		t.Fatalf("first import %+v %v", first, err)
	}
	codeOf := func(err error) string {
		if r, ok := kernel.AsRejection(err); ok {
			return r.Code + " " + r.Fields[0].Path
		}
		return "OK"
	}
	next := func(version string, published time.Time) *Version {
		return &Version{ListCode: "cn.country_programmes", Version: version, PublishedOn: published}
	}
	if _, err = Import(first, "CN", true, v, next("v1", day(1))); codeOf(err) != "BUSINESS_KEY_CONFLICT /args/version" {
		t.Fatalf("a version is imported once: %v", err)
	}
	if _, err = Import(first, "UN", false, v, next("v2", day(2))); codeOf(err) != "INVALID_KNOWN_VALUE /args/jurisdiction" {
		t.Fatalf("a list keeps its jurisdiction: %v", err)
	}
	if _, err = Import(first, "CN", false, v, next("v0", day(1).Add(-24*time.Hour))); codeOf(err) != "INVALID_KNOWN_VALUE /args/published_on" {
		t.Fatalf("no older version replaces the current one: %v", err)
	}
	v2 := next("v2", day(1))
	second, err := Import(first, "CN", false, v, v2)
	if err != nil || second.RowVersion != 2 || second.Current != "v2" || v2.ImportSeq != 2 {
		t.Fatalf("second import %+v %v", second, err)
	}
}
