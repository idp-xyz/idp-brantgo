package lists

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// name-key-v1: the normalization screening matches under (S1-09; no fuzzy or AI matching).
func TestNameKeyNormalization(t *testing.T) {
	for _, c := range []struct{ name, key string }{
		{"Acme Trading Co., Ltd.", "acme trading co ltd"},
		{"ACME  TRADING CO LTD", "acme trading co ltd"},
		{"Ａｃｍｅ　Ｔｒａｄｉｎｇ", "acme trading"},              // full-width letters and space (NFKD)
		{"Société Générale", "societe generale"},      // diacritics dropped
		{"Müller GmbH & Co. KG", "muller gmbh co kg"}, // & separates words
		{"STRAßE Holdings", "strasse holdings"},
		{"O'Brien Shipping", "obrien shipping"}, // apostrophes removed, not separators
		{"O’Brien Shipping", "obrien shipping"},
		{"S.A. Industries", "sa industries"},
		{"Smith-Jones/Partners", "smith jones partners"},
		{"华为技术有限公司", "华为技术有限公司"},
		{"华为技术有限公司。", "华为技术有限公司"},
		{"ＫＰｉ．０３３", "kpi033"},
		{"ﬁnance house", "finance house"}, // ligature
		{"  ---  ", ""},
		{"Smith and Sons", "smith and sons"}, // no word mapping: & and "and" differ
	} {
		if got := NameKey(c.name); got != c.key {
			t.Errorf("NameKey(%q) = %q, want %q", c.name, got, c.key)
		}
	}
	if NameKey("Smith & Sons") == NameKey("Smith and Sons") {
		t.Error("name-key-v1 maps no words")
	}
}

func TestEntryIsCanonicalAndHashed(t *testing.T) {
	a, err := NewEntry("E1", "Acme Trading Co., Ltd.", []string{"ACME TRADING", "Acme Co"}, "entity", "CN")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewEntry("E1", "Acme Trading Co., Ltd.", []string{"Acme Co", "ACME TRADING"}, "entity", "CN")
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash != b.Hash || len(a.Hash) != 64 || a.Aliases[0] != "ACME TRADING" {
		t.Fatalf("alias order must not change the content: %+v %+v", a, b)
	}
	if len(a.Keys) != 3 || a.Keys[0].Key != "acme trading co ltd" || a.Keys[0].Name != "Acme Trading Co., Ltd." {
		t.Fatalf("keys %+v", a.Keys)
	}
	same, _ := NewEntry("E1", "Acme Trading", []string{"ACME TRADING"}, "entity", "")
	if len(same.Keys) != 1 {
		t.Fatalf("names with one key keep one: %+v", same.Keys)
	}
	changed, _ := NewEntry("E1", "Acme Trading Co., Ltd.", []string{"ACME TRADING", "Acme Co", "Acme HK"}, "entity", "CN")
	if changed.Hash == a.Hash {
		t.Fatal("a new alias changes the entry's content hash")
	}
	if e, err := NewEntry("E2", "㍿", nil, "entity", ""); err != nil || e.Keys[0].Key != "株式会社" {
		t.Fatalf("compatibility characters decompose: %+v %v", e, err)
	}
	if _, err := NewEntry("E3", strings.Repeat("㍿", 400), nil, "entity", ""); err == nil {
		t.Fatal("a name whose key exceeds the index limit is refused")
	}
	other, _ := NewEntry("E0", "Globex", nil, "entity", "")
	if ContentHash([]Entry{a, other}) != ContentHash([]Entry{other, a}) || ContentHash([]Entry{a}) == ContentHash([]Entry{changed}) {
		t.Fatal("a version's content hash is over its entries in ID order")
	}
}

func TestImportRules(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	v := &Version{ListCode: "cn.uel", Version: "v1", PublishedOn: day(1)}
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
	if _, err = Import(first, "CN", true, v, &Version{ListCode: "cn.uel", Version: "v1", PublishedOn: day(1)}); codeOf(err) != "BUSINESS_KEY_CONFLICT /args/version" {
		t.Fatalf("a version is imported once: %v", err)
	}
	if _, err = Import(first, "foreign", false, v, &Version{ListCode: "cn.uel", Version: "v2", PublishedOn: day(2)}); codeOf(err) != "INVALID_KNOWN_VALUE /args/jurisdiction" {
		t.Fatalf("a list keeps its jurisdiction: %v", err)
	}
	if _, err = Import(first, "CN", false, v, &Version{ListCode: "cn.uel", Version: "v0", PublishedOn: day(1).Add(-24 * time.Hour)}); codeOf(err) != "INVALID_KNOWN_VALUE /args/published_on" {
		t.Fatalf("no older version replaces the current one: %v", err)
	}
	v2 := &Version{ListCode: "cn.uel", Version: "v2", PublishedOn: day(1)}
	second, err := Import(first, "CN", false, v, v2)
	if err != nil || second.RowVersion != 2 || second.Current != "v2" || v2.ImportSeq != 2 {
		t.Fatalf("second import %+v %v", second, err)
	}
}
