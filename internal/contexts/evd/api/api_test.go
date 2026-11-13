package api

import (
	"encoding/json"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// A file_extract names only the file: no text (EVD reads it from the file through the extractor,
// AI-INV-004) and no channel; other kinds name no file (WBS S4-04b).
func TestRegisterSourceFileExtract(t *testing.T) {
	ok := RegisterSource{IdempotencyKey: "k1", SourceID: "src.x", SourceKind: KindFileExtract, FileID: "file.1"}
	for _, c := range []struct {
		edit func(*RegisterSource)
		want string
	}{
		{func(*RegisterSource) {}, "OK"},
		{func(c *RegisterSource) { c.ContentText = "forged text" }, "SCHEMA_INVALID /args/content_text"},
		{func(c *RegisterSource) { c.Provider = "mail" }, "SCHEMA_INVALID /args/source_kind"},
		{func(c *RegisterSource) { c.FileID = "" }, "SCHEMA_INVALID /args/file_id"},
		{func(c *RegisterSource) { c.SourceKind, c.ContentText = KindManualText, "text" }, "SCHEMA_INVALID /args/file_id"},
		{func(c *RegisterSource) { c.SourceKind, c.FileID = KindManualText, "" }, "SCHEMA_INVALID /args/content_text"},
	} {
		c2 := ok
		c.edit(&c2)
		got := "OK"
		if r, isRej := kernel.AsRejection(c2.Validate()); isRej {
			got = r.Code + " " + r.Fields[0].Path
		}
		if got != c.want {
			t.Errorf("%+v: %s, want %s", c2, got, c.want)
		}
	}
	// FileID is left out of the JSON while empty: earlier requests hash as before (X-INV-001).
	b, _ := json.Marshal(RegisterSource{SourceID: "src.1", ContentText: "t", SourceKind: KindManualText})
	if strings.Contains(string(b), "FileID") {
		t.Fatalf("%s", b)
	}
}
