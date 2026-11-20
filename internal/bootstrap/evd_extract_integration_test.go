//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/ingest"
	"go.idp.xyz/idp-brant/internal/ai/ingest/ingesttest"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S4 B4 (WBS S4-04b): file extracts as EVD sources. EVD has the AI layer's extractor read a
// registered business file under Q01.upload.v0.1 and keeps the extract as a source tied to the file
// (kind file_extract: file, extractor version, extract SHA-256, located form). Synthetic files only
// (decision E-6).

// rawFile registers content as a business file of mime.
func (w *world) rawFile(ctx context.Context, id, name, mime string, content []byte) {
	w.t.Helper()
	_, err := w.c.EVD.RegisterFile(ctx, evdapi.RegisterFile{IdempotencyKey: w.key(), FileID: id, FileName: name, MimeType: mime,
		ContentBase64: base64.StdEncoding.EncodeToString(content)})
	w.must(err)
}

// extract registers the extract of file fileID as source id.
func (w *world) extract(ctx context.Context, id, fileID string) (evdapi.Receipt, error) {
	return w.c.EVD.RegisterSource(ctx, evdapi.RegisterSource{IdempotencyKey: w.key(), SourceID: id, SourceKind: evdapi.KindFileExtract, FileID: fileID})
}

func inquiryPDF() []byte {
	return ingesttest.PDF{Pages: []ingesttest.PDFPage{{Texts: []ingesttest.PDFText{
		{Font: "F1", Size: 14, X: 72, Y: 780, Text: "Request for Quotation RFQ-B4"},
		{Font: "F3", Size: 12, X: 72, Y: 750, Text: "1. ST01 red S qty 24 PCS"},
		{Font: "F3", Size: 12, X: 72, Y: 730, Text: "2. ST01 red M qty 36 PCS"},
	}}}}.Bytes()
}

// A text, an XLSX and a PDF file each become a file_extract source whose text, located form and
// SHA-256 are the extractor's (EVD holds no parsing code); the located form reads back into the
// same extract, so its locators resolve as when it was read (BC-AT27). A replay answers the stored
// receipt; the same source ID with the same file is the same origin with a new import; another
// source ID shares the extract; another file under a known source ID is SOURCE_IDENTITY_CONFLICT;
// text supplied with a file_extract is SCHEMA_INVALID (AI-INV-004: the text comes from the file);
// a file outside the profile is INPUT_PROFILE_UNSUPPORTED with its limit and nothing is stored
// (baseline 14.2); extracts are immutable (EVD-INV-001) and hash-checked by the database.
func TestFileExtractSourcesAreReadByThePlatform(t *testing.T) {
	w := newWorld(t)
	clerk := w.actor("clerk", allPermissions...)
	files := map[string]struct {
		name, mime string
		content    []byte
	}{
		"file.txt":  {"inquiry.txt", evdapi.MimeText, []byte("合成询价 RFQ-B4\n款号 ST01 红 S 数量 24 件\n")},
		"file.xlsx": {"inquiry.xlsx", evdapi.MimeXLSX, inquiryWorkbook()},
		"file.pdf":  {"inquiry.pdf", evdapi.MimePDF, inquiryPDF()},
	}
	for id, f := range files {
		w.rawFile(clerk, id, f.name, f.mime, f.content)
		r, err := w.extract(clerk, "src."+id, id)
		w.must(err)
		if r.ObjectID != "src."+id || r.Status != "registered" {
			t.Fatalf("%s: %+v", id, r)
		}
		want, err := ingest.Q01Upload.Extract(f.mime, f.content)
		w.must(err)
		s, err := w.c.EVD.GetSource(clerk, "src."+id)
		w.must(err)
		if s.SourceKind != evdapi.KindFileExtract || s.ContentText != want.Text() || s.Extract == nil || s.Extract.FileID != id ||
			s.Extract.SHA256 != want.SHA256 || s.Extract.Extractor != want.Extractor || s.Extract.Profile != ingest.Q01Upload.ID ||
			s.Extract.Characters != want.Characters || s.Extract.FileSHA256 != want.FileSHA256 {
			t.Fatalf("%s: source %+v %+v", id, s, s.Extract)
		}
		v, err := w.c.EVD.GetExtract(clerk, "src."+id)
		w.must(err)
		back, err := ingest.FromEVD(v)
		if err != nil || !reflect.DeepEqual(back, want) {
			t.Fatalf("%s: located form %v", id, err)
		}
	}
	// A replay answers the stored receipt; a second registration of the same file under the same ID
	// is the same origin with a new import; another ID shares the extract.
	cmd := evdapi.RegisterSource{IdempotencyKey: "k.extract", SourceID: "src.again", SourceKind: evdapi.KindFileExtract, FileID: "file.xlsx"}
	first, err := w.c.EVD.RegisterSource(clerk, cmd)
	w.must(err)
	replay, err := w.c.EVD.RegisterSource(clerk, cmd)
	if err != nil || !replay.Replayed || replay.ActionID != first.ActionID {
		t.Fatalf("replay %+v %v", replay, err)
	}
	_, err = w.extract(clerk, "src.again", "file.xlsx")
	w.must(err)
	again, err := w.c.EVD.GetSource(clerk, "src.again")
	w.must(err)
	if len(again.Imports) != 2 || w.count("company.A", "brant_evd.file_extract") != 3 || w.count("company.A", "brant_evd.source_origin") != 4 {
		t.Fatalf("imports %d, extracts %d", len(again.Imports), w.count("company.A", "brant_evd.file_extract"))
	}
	_, err = w.extract(clerk, "src.again", "file.pdf")
	w.want(err, evdapi.CodeSourceIdentityConflict)
	// Two first registrations of one file's extract at once share one extract (the unique index on
	// file and extractor; the ledger retries the loser, which then finds it).
	w.rawFile(clerk, "file.twice", "twice.txt", evdapi.MimeText, []byte("并发登记的合成询价\n"))
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = w.extract(clerk, fmt.Sprintf("src.twice.%d", i), "file.twice")
		}()
	}
	wg.Wait()
	w.must(errors.Join(errs...))
	if n := w.count("company.A", "brant_evd.file_extract"); n != 4 {
		t.Fatalf("%d extracts after concurrent registrations", n)
	}
	_, err = w.c.EVD.RegisterSource(clerk, evdapi.RegisterSource{IdempotencyKey: w.key(), SourceID: "src.forged", SourceKind: evdapi.KindFileExtract,
		FileID: "file.xlsx", ContentText: "伪造的数量 9999"})
	w.want(err, kernel.CodeSchemaInvalid)
	_, err = w.extract(clerk, "src.none", "file.missing")
	if code(err) != kernel.CodeNotFound || path(err) != "/args/file_id" {
		t.Fatalf("missing file: %v", err)
	}
	// Four sheets are one too many for Q01.upload.v0.1: refused with the limit, never truncated.
	wide := ingesttest.Workbook{Sheets: []ingesttest.Sheet{{Name: "a", Cells: []ingesttest.Cell{{Ref: "A1", Text: "x"}}}, {Name: "b"}, {Name: "c"}, {Name: "d"}}}
	w.rawFile(clerk, "file.wide", "wide.xlsx", evdapi.MimeXLSX, wide.Bytes())
	_, err = w.extract(clerk, "src.wide", "file.wide")
	if rej, ok := kernel.AsRejection(err); !ok || rej.Code != evdapi.CodeInputProfileUnsupported || rej.Fields[0].Path != "/args/file_id" ||
		!strings.Contains(rej.Fields[0].Reason, "limit=sheets") {
		t.Fatalf("outside the profile: %v", err)
	}
	_, err = w.c.EVD.GetSource(clerk, "src.wide")
	w.want(err, kernel.CodeNotFound)
	if n := w.count("company.A", "brant_evd.file_extract"); n != 4 {
		t.Fatalf("%d extracts after a refusal", n)
	}
	// EVD-INV-001: no change, no deletion, and a hash that is not the located form's is refused.
	for _, sql := range []string{
		`UPDATE brant_evd.file_extract SET located_form = located_form || 'x'`,
		`DELETE FROM brant_evd.file_extract`,
		`INSERT INTO brant_evd.file_extract SELECT tenant_id, company_id, 'extract.forged', 'file.wide', file_sha256, extract_kind, extractor,
			profile, characters, pages, sheets, sha256, located_form || 'segment	x!A1	9999\n', created_at FROM brant_evd.file_extract LIMIT 1`,
	} {
		if err := w.exec("company.A", sql); err == nil {
			t.Fatalf("%s succeeded", sql)
		}
	}
	// The source's text is not written to the audit trail; the extract reads are.
	audit, err := w.c.EVD.Audit(clerk)
	w.must(err)
	reads := 0
	for _, e := range audit {
		if strings.Contains(e.Reason, "ST01") {
			t.Fatalf("content in the audit: %+v", e)
		}
		if e.Command == "source.extract.get" && e.Outcome == "read" {
			reads++
		}
	}
	if reads != 3 {
		t.Fatalf("%d audited extract reads", reads)
	}
}

// CT16: access to the file is re-checked whenever its extract is read, resolved or replayed. An
// extract origin someone else registered is visible only to a reader who also sees the file now; a
// reader whose evd.read is revoked is refused; the registrant who no longer sees the file cannot
// read the extract nor replay its registration. History is not erased: a reader who sees both still
// reads it.
func TestFileExtractAccessIsRecheckedOnEveryRead(t *testing.T) {
	w := newWorld(t)
	scope := w.scope("company.A")
	grant := func(id, team string, ds authorization.DataScope, permissions ...string) context.Context {
		return w.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id}, Permissions: permissions,
			DataScope: ds, Team: team})
	}
	owner := grant("file.owner", "team.files", authorization.DataScopeOwn, allPermissions...)
	bob := grant("bob", "team.sales", authorization.DataScopeCompany, allPermissions...)
	carol := grant("carol", "team.sales", authorization.DataScopeTeam, "evd.read")
	dave := grant("dave", "team.audit", authorization.DataScopeCompany, "evd.read")
	w.rawFile(owner, "file.1", "inquiry.xlsx", evdapi.MimeXLSX, inquiryWorkbook())
	register := evdapi.RegisterSource{IdempotencyKey: "k.bob", SourceID: "src.x", SourceKind: evdapi.KindFileExtract, FileID: "file.1"}
	_, err := w.c.EVD.RegisterSource(bob, register)
	w.must(err)
	w.source(bob, "src.note", "bob's own note")

	// Carol sees bob's origins (her team) but not the owner's file: the extract is not hers to read.
	_, err = w.c.EVD.GetSource(carol, "src.note")
	w.must(err)
	_, err = w.c.EVD.GetSource(carol, "src.x")
	w.want(err, kernel.CodeNotFound)
	_, err = w.c.EVD.GetExtract(carol, "src.x")
	w.want(err, kernel.CodeNotFound)
	refs, err := w.c.EVD.ResolveSources(carol, []string{"src.x", "src.note"})
	w.must(err)
	if _, ok := refs["src.x"]; ok || len(refs) != 1 {
		t.Fatalf("carol resolves %v", refs)
	}
	// Dave sees both; when his evd.read is revoked he reads nothing.
	_, err = w.c.EVD.GetExtract(dave, "src.x")
	w.must(err)
	grant("dave", "team.audit", authorization.DataScopeCompany)
	_, err = w.c.EVD.GetExtract(dave, "src.x")
	w.want(err, kernel.CodePermissionDenied)
	// Bob loses sight of the file: neither a read nor a replay of his own registration answers.
	grant("bob", "team.sales", authorization.DataScopeOwn, allPermissions...)
	_, err = w.c.EVD.GetExtract(bob, "src.x")
	w.want(err, kernel.CodeNotFound)
	_, err = w.c.EVD.RegisterSource(bob, register)
	w.want(err, kernel.CodeNotFound)
	// The registration stays; whoever sees the source and the file reads it.
	grant("dave", "team.audit", authorization.DataScopeCompany, "evd.read")
	v, err := w.c.EVD.GetExtract(dave, "src.x")
	if err != nil || v.FileID != "file.1" || len(v.LocatedForm) == 0 {
		t.Fatalf("%+v %v", v.ExtractRef, err)
	}
}
