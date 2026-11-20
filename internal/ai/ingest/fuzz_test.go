package ingest

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var mimes = []string{evdapi.MimeText, evdapi.MimePDF, evdapi.MimeXLSX, "text/csv"}

// FuzzExtract asserts for every reader that an extract is deterministic, within the profile, sealed
// over its canonical form and locatable (each segment at its own locator), and that every refusal
// is INPUT_PROFILE_UNSUPPORTED without an extract (WBS S4-04: fuzz targets for each reader); unit B4
// adds that the located form reads back into the same extract (ParseCanonical).
func FuzzExtract(f *testing.F) {
	f.Add(uint8(0), []byte(inquiry))
	f.Add(uint8(0), []byte("a\r\n\r\nb\tc\n"))
	f.Add(uint8(1), document().Bytes())
	f.Add(uint8(2), workbook().Bytes())
	f.Fuzz(func(t *testing.T, kind uint8, data []byte) {
		p := Q01Upload
		p.MaxCharacters = 2000
		mime := mimes[int(kind)%len(mimes)]
		e, err := p.Extract(mime, data)
		again, err2 := p.Extract(mime, data)
		if !reflect.DeepEqual(e, again) || !reflect.DeepEqual(err, err2) {
			t.Fatalf("two reads differ: %v / %v", err, err2)
		}
		if err != nil {
			var u *Unsupported
			r, ok := kernel.AsRejection(err)
			if e != nil || !errors.As(err, &u) || !ok || r.Code != "INPUT_PROFILE_UNSUPPORTED" || !strings.HasPrefix(r.Fields[0].Path, "/file") {
				t.Fatalf("extract %v, error %v", e, err)
			}
			return
		}
		if e.Characters > p.MaxCharacters || e.SHA256 != sha(e.Canonical()) || e.FileSHA256 != sha(data) || len(e.Segments) == 0 {
			t.Fatalf("extract %+v", e)
		}
		// B4: what EVD stores reads back into the same extract.
		if back, err := ParseCanonical(e.Canonical()); err != nil || !reflect.DeepEqual(back, e) {
			t.Fatalf("the located form does not read back: %v", err)
		}
		for _, s := range e.Segments {
			got, err := e.At(s.Locator)
			if err != nil || got != s.Text {
				t.Fatalf("At(%q) = %q, %v; segment %q", s.Locator, got, err, s.Text)
			}
			for _, r := range s.Text {
				if control(r) && (e.Kind != KindXLSX || r != '\n') {
					t.Fatalf("control character %U in %q", r, s.Text)
				}
			}
		}
		if q := []rune(e.Segments[0].Text); len(q) > 0 {
			quote := string(q[:min(3, len(q))])
			found := e.Find(quote)
			if len(found) == 0 {
				t.Fatalf("Find(%q) found nothing", quote)
			}
			for _, loc := range found {
				if !e.Verify(loc, quote) {
					t.Fatalf("Find(%q) gave %s, which does not verify", quote, loc)
				}
			}
		}
	})
}
