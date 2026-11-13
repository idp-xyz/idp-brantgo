package source

import "testing"

// A file extract is readable only by a reader who sees both the origin and the file it was read
// from (CT16); other origins need only their own owner seen.
func TestReadableNeedsTheFileForAnExtract(t *testing.T) {
	sees := func(actor, _ string) bool { return actor == "alice" }
	manual := Origin{Kind: ManualText, OwnerActor: "alice"}
	extract := Origin{Kind: FileExtract, OwnerActor: "alice", File: Owner{Actor: "bob"}}
	own := Origin{Kind: FileExtract, OwnerActor: "alice", File: Owner{Actor: "alice"}}
	if !manual.Readable(sees) || extract.Readable(sees) || !own.Readable(sees) {
		t.Fatal("readability")
	}
}

// A registration under a known ID matches only the same file and extract; another file or another
// extract of it (a new extractor version) is a conflict (EVD-INV-001).
func TestMatchComparesTheExtract(t *testing.T) {
	reg := Registration{OriginID: "src.x", Kind: FileExtract, Text: "t", Extract: ExtractID{FileID: "file.1", SHA256: "aa"}}
	known := &Origin{ID: "src.x", Kind: FileExtract, ContentHash: reg.ContentHash(), Extract: reg.Extract}
	if o, err := Match(reg, known, nil); err != nil || o != known {
		t.Fatalf("%v %v", o, err)
	}
	for _, other := range []ExtractID{{FileID: "file.2", SHA256: "aa"}, {FileID: "file.1", SHA256: "bb"}} {
		r := reg
		r.Extract = other
		if _, err := Match(r, known, nil); err == nil {
			t.Fatalf("%+v matched", other)
		}
	}
}
