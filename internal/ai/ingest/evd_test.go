package ingest

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/codes"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The extractor EVD registers file_extract sources with (WBS S4-04b, unit B4) gives EVD the
// extract's identity, its text and its located form; reading the located form back gives the same
// extract, so the locators of a stored extract resolve as they did when it was read (BC-AT27).
func TestEVDExtractorRoundTrips(t *testing.T) {
	for _, f := range []struct {
		mime    string
		content []byte
	}{
		{evdapi.MimeText, []byte("合成询价\r\n款号 ST-100\t数量 120 件\n\\ backslash\n")},
		{evdapi.MimeXLSX, workbook().Bytes()},
		{evdapi.MimePDF, document().Bytes()},
	} {
		x, err := EVD{Profile: Q01Upload}.Extract(f.mime, f.content)
		if err != nil {
			t.Fatalf("%s: %v", f.mime, err)
		}
		e, err := Q01Upload.Extract(f.mime, f.content)
		if err != nil {
			t.Fatal(err)
		}
		if x.SHA256 != e.SHA256 || x.Text != e.Text() || x.LocatedForm != string(e.Canonical()) || x.Characters != e.Characters ||
			x.Kind != string(e.Kind) || x.Extractor != e.Extractor || x.Profile != Q01Upload.ID || x.FileSHA256 != e.FileSHA256 || x.FileID != "" {
			t.Fatalf("%s: %+v", f.mime, x.ExtractRef)
		}
		back, err := ParseCanonical([]byte(x.LocatedForm))
		if err != nil {
			t.Fatalf("%s: %v", f.mime, err)
		}
		if !reflect.DeepEqual(back, e) {
			t.Fatalf("%s: read back\n%+v\nwant\n%+v", f.mime, back, e)
		}
		view := evdapi.ExtractView{SourceID: "src.1", ExtractRef: x.ExtractRef, LocatedForm: x.LocatedForm}
		if got, err := FromEVD(view); err != nil || !reflect.DeepEqual(got, e) {
			t.Fatalf("%s: from EVD %v", f.mime, err)
		}
		view.Extractor = "brant.ingest.other/9"
		if _, err := FromEVD(view); !errors.Is(err, ErrLocatedForm) {
			t.Fatalf("%s: a view naming another extractor: %v", f.mime, err)
		}
	}
}

// A file outside the profile reaches EVD as the rejection INPUT_PROFILE_UNSUPPORTED with the limit,
// never as a partial extract (baseline 14.2).
func TestEVDExtractorRefusesOutsideTheProfile(t *testing.T) {
	x, err := EVD{Profile: Q01Upload}.Extract(evdapi.MimeText, []byte(strings.Repeat("字", Q01Upload.MaxCharacters+1)))
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != codes.InputProfileUnsupported || !strings.Contains(rej.Fields[0].Reason, "limit=characters") || x.LocatedForm != "" {
		t.Fatalf("%+v %v", x, err)
	}
}

// Only what Canonical writes is read back: a changed byte, a changed count, an unknown record, a
// bad escape or a missing final line feed is refused.
func TestParseCanonicalRefusesOtherForms(t *testing.T) {
	e, err := Q01Upload.Extract(evdapi.MimeXLSX, workbook().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	form := string(e.Canonical())
	for name, bad := range map[string]string{
		"no final line feed":  strings.TrimSuffix(form, "\n"),
		"header":              strings.Replace(form, "brant-extract/1", "brant-extract/2", 1),
		"characters":          strings.Replace(form, "characters\t", "characters\t1", 1),
		"unknown record":      form + "comment\tx\n",
		"bad escape":          strings.Replace(form, "备注\\nnote", "备注\\xnote", 1),
		"raw carriage":        strings.Replace(form, "备注\\nnote", "备注\rnote", 1),
		"note twice":          form + "note\t" + NoteHiddenSheet + "\t'Q1 2026'\n",
		"pages of a workbook": strings.Replace(form, "characters\t", "pages\t1\ncharacters\t", 1),
		"empty":               "",
	} {
		if bad == form {
			t.Fatalf("%s: the case changes nothing", name)
		}
		if _, err := ParseCanonical([]byte(bad)); !errors.Is(err, ErrLocatedForm) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
