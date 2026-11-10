//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func (w *world) source(ctx context.Context, id, text string) string {
	w.t.Helper()
	r, err := w.c.EVD.RegisterSource(ctx, evdapi.RegisterSource{IdempotencyKey: w.key(), SourceID: id, SourceKind: evdapi.KindManualText, ContentText: text})
	if err != nil {
		w.t.Fatalf("register %s: %v", id, err)
	}
	return r.ObjectID
}

var email = evdapi.RegisterSource{SourceKind: evdapi.KindEmailText, Provider: "test", AccountRef: "acct.1",
	ExternalMessageID: "msg.1", AuthorClaim: "synthetic buyer", ContentText: "合成询价120件"}

// Python test_core.py test_same_key_replays_without_repeating_import_and_different_content_conflicts.
func TestSameKeyReplaysWithoutRepeatingImportAndDifferentContentConflicts(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	cmd := evdapi.RegisterSource{IdempotencyKey: "same", SourceID: "s", SourceKind: evdapi.KindManualText, ContentText: "same"}
	first, err := w.c.EVD.RegisterSource(ctx, cmd)
	w.must(err)
	second, err := w.c.EVD.RegisterSource(ctx, cmd)
	w.must(err)
	if !second.Replayed || second.ActionID != first.ActionID || first.Status != "registered" || first.RowVersion != 1 {
		t.Fatalf("first %+v, second %+v", first, second)
	}
	view, err := w.c.EVD.GetSource(ctx, "s")
	w.must(err)
	if len(view.Imports) != 1 {
		t.Fatalf("a replay must not record another import: %d", len(view.Imports))
	}
	cmd.ContentText = "different"
	_, err = w.c.EVD.RegisterSource(ctx, cmd)
	w.want(err, kernel.CodeIdempotencyPayloadConflict)
}

// Python test_core.py test_email_identity_deduplicates_origin_but_preserves_imports.
func TestEmailIdentityDeduplicatesOriginButPreservesImports(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	register := func(id, text string) (evdapi.Receipt, error) {
		c := email
		c.IdempotencyKey, c.SourceID, c.ContentText = w.key(), id, text
		return w.c.EVD.RegisterSource(ctx, c)
	}
	r1, err := register("s1", email.ContentText)
	w.must(err)
	r2, err := register("s2", email.ContentText)
	w.must(err)
	if r1.ObjectID != "s1" || r2.ObjectID != "s1" {
		t.Fatalf("the same message must resolve to its first origin: %s, %s", r1.ObjectID, r2.ObjectID)
	}
	view, err := w.c.EVD.GetSource(ctx, "s1")
	w.must(err)
	if len(view.Imports) != 2 {
		t.Fatalf("imports %d", len(view.Imports))
	}
	_, err = register("s3", "changed")
	w.want(err, evdapi.CodeSourceIdentityConflict)
	if view, _ = w.c.EVD.GetSource(ctx, "s1"); len(view.Imports) != 2 {
		t.Fatal("a refused registration must not record an import")
	}
	if _, err = w.c.EVD.GetSource(ctx, "s2"); code(err) != kernel.CodeNotFound {
		t.Fatalf("no origin s2 exists: %v", err)
	}
}

// Python test_core.py test_same_content_different_origins_can_support_different_rfqs (the EVD
// half; the RFQ half is in the INQ suite) and test_cross_company_reference_is_rejected_without_existence_leak
// (sources of another company never share content).
func TestSameContentIsSharedWithinACompanyOnly(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.source(ctx, "s1", "合成询价120件")
	w.source(ctx, "s2", "合成询价120件")
	a, err := w.c.EVD.GetSource(ctx, "s1")
	w.must(err)
	b, err := w.c.EVD.GetSource(ctx, "s2")
	w.must(err)
	if a.ContentSHA256 != b.ContentSHA256 || a.SourceID == b.SourceID {
		t.Fatalf("two origins of one content: %+v %+v", a, b)
	}
	if n := w.count("company.A", "brant_evd.source_content"); n != 1 {
		t.Fatalf("company.A stores %d contents, want 1", n)
	}
	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "human.1"}, Permissions: allPermissions})
	w.source(other, "s1", "合成询价120件")
	if n := w.count("company.B", "brant_evd.source_content"); n != 1 {
		t.Fatalf("company.B must store its own content, got %d", n)
	}
}

// Python test_core.py test_concurrent_duplicate_import_is_single_action.
func TestConcurrentDuplicateImportIsSingleAction(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	cmd := evdapi.RegisterSource{IdempotencyKey: "parallel.source", SourceID: "s", SourceKind: evdapi.KindManualText, ContentText: "same"}
	results, errs := make([]evdapi.Receipt, 2), make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); results[i], errs[i] = w.c.EVD.RegisterSource(ctx, cmd) }()
	}
	wg.Wait()
	w.must(errs[0])
	w.must(errs[1])
	if results[0].ActionID != results[1].ActionID || results[0].Replayed == results[1].Replayed {
		t.Fatalf("results %+v", results)
	}
	view, err := w.c.EVD.GetSource(ctx, "s")
	w.must(err)
	if len(view.Imports) != 1 {
		t.Fatalf("imports %d", len(view.Imports))
	}
}

// Python test_core.py test_limits_unknown_contract_and_sensitive_audit (the EVD half; the unknown
// contract is the HTTP transport's) and test_manual_text_with_channel_identity_is_closed_schema_rejection.
func TestLimitsAndClosedSchemaAndNoContentInAudit(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	oversized := evdapi.RegisterSource{IdempotencyKey: w.key(), SourceID: "s", SourceKind: evdapi.KindManualText, ContentText: strings.Repeat("秘", 50001)}
	_, err := w.c.EVD.RegisterSource(ctx, oversized)
	w.want(err, kernel.CodeSchemaInvalid)
	manualWithChannel := evdapi.RegisterSource{IdempotencyKey: w.key(), SourceID: "s", SourceKind: evdapi.KindManualText, ContentText: "text",
		Provider: "mail", AccountRef: "a", ExternalMessageID: "m"}
	_, err = w.c.EVD.RegisterSource(ctx, manualWithChannel)
	w.want(err, kernel.CodeSchemaInvalid)
	emailWithoutAuthor := email
	emailWithoutAuthor.IdempotencyKey, emailWithoutAuthor.SourceID, emailWithoutAuthor.AuthorClaim = w.key(), "s", ""
	_, err = w.c.EVD.RegisterSource(ctx, emailWithoutAuthor)
	w.want(err, kernel.CodeSchemaInvalid)
	w.source(ctx, "s", "PRIVATE_SOURCE_TEXT")
	audit, err := w.c.EVD.Audit(ctx)
	w.must(err)
	for _, e := range audit {
		if strings.Contains(e.Reason+e.ObjectID+e.Command+e.Code, "PRIVATE_SOURCE_TEXT") {
			t.Fatalf("source text in the audit trail: %+v", e)
		}
	}
	if got := []string{audit[0].Code, audit[1].Code, audit[2].Code, audit[3].Outcome}; strings.Join(got, " ") !=
		"SCHEMA_INVALID SCHEMA_INVALID SCHEMA_INVALID succeeded" {
		t.Fatalf("audit %v", got)
	}
}

// Python test_core.py test_denied_reader_cannot_get_source and test_source_read_and_denied_read_are_audited.
func TestSourceReadsAreAuditedAndDeniedReadsToo(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.source(ctx, "src.1", "合成询价120件")
	_, err := w.c.EVD.GetSource(ctx, "src.1")
	w.must(err)
	noRead := w.actor("no.read", "source.register")
	_, err = w.c.EVD.GetSource(noRead, "src.1")
	w.want(err, kernel.CodePermissionDenied)
	blocked := w.actor("blocked")
	_, err = w.c.EVD.GetSource(blocked, "src.1")
	w.want(err, kernel.CodePermissionDenied)
	audit, err := w.c.EVD.Audit(ctx)
	w.must(err)
	last := audit[len(audit)-1]
	if audit[1].Outcome != "read" || last.Code != kernel.CodePermissionDenied || last.Actor.ID != "blocked" {
		t.Fatalf("audit %+v", audit)
	}
}

// Python test_foundation.py test_source_deduplication_replay_uses_canonical_id.
func TestSourceDeduplicationReplayUsesCanonicalID(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("editor", allPermissions...)
	c := email
	c.IdempotencyKey, c.SourceID, c.ContentText = w.key(), "s1", "same"
	_, err := w.c.EVD.RegisterSource(ctx, c)
	w.must(err)
	c.IdempotencyKey, c.SourceID = "canonical.replay", "s2"
	first, err := w.c.EVD.RegisterSource(ctx, c)
	w.must(err)
	second, err := w.c.EVD.RegisterSource(ctx, c)
	w.must(err)
	if first.ObjectID != "s1" || !second.Replayed || first.ActionID != second.ActionID {
		t.Fatalf("first %+v, second %+v", first, second)
	}
}

// Python test_foundation.py test_private_source_cannot_be_probed_by_guessing_body and the source
// half of test_own_team_scope_and_replay_do_not_leak.
func TestPrivateSourceCannotBeProbed(t *testing.T) {
	w := newWorld(t)
	owner := w.actor("private.owner", allPermissions...)
	outsider := w.actor("private.outsider", allPermissions...)
	w.source(owner, "secret", "secret-body")
	var errs []error
	for _, body := range []string{"secret-body", "wrong-guess"} {
		_, err := w.c.EVD.RegisterSource(outsider, evdapi.RegisterSource{IdempotencyKey: w.key(), SourceID: "secret", SourceKind: evdapi.KindManualText, ContentText: body})
		errs = append(errs, err)
	}
	if code(errs[0]) != kernel.CodeNotFound || code(errs[1]) != kernel.CodeNotFound || path(errs[0]) != path(errs[1]) {
		t.Fatalf("a guessed body must look like a wrong one: %v / %v", errs[0], errs[1])
	}
	if _, err := w.c.EVD.GetSource(outsider, "secret"); code(err) != kernel.CodeNotFound {
		t.Fatalf("outsider read: %v", err)
	}
	refs, err := w.c.EVD.ResolveSources(outsider, []string{"secret"})
	w.must(err)
	if len(refs) != 0 {
		t.Fatalf("outsider resolves %v", refs)
	}
	w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "private.outsider"},
		Permissions: allPermissions, DataScope: authorization.DataScopeCompany})
	if _, err := w.c.EVD.GetSource(outsider, "secret"); err != nil {
		t.Fatalf("company data scope reads every source: %v", err)
	}
}

// Python test_core.py test_audit_waits_for_prior_revocation_transaction: a read that starts while a
// revocation is uncommitted waits for it and is then denied.
func TestAuditWaitsForAPriorRevocation(t *testing.T) {
	w := newWorld(t)
	reader := w.actor("audit.reader", "evd.audit")
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- bento.ScopeTransactor{DB: db, Scope: w.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
			exec, err := db.RequireExecutor(ctx)
			if err != nil {
				return err
			}
			if _, err = (authorization.Grants{}).Revoke(ctx, exec, w.scope("company.A"), "audit.reader", time.Now()); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	read := make(chan error, 1)
	go func() { _, err := w.c.EVD.Audit(reader); read <- err }()
	select {
	case err := <-read:
		t.Fatalf("the read must wait for the open revocation, got %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	w.must(<-done)
	w.want(<-read, kernel.CodePermissionDenied)
}

func (w *world) file(ctx context.Context, id, name, mime, content, supersedes string) (evdapi.Receipt, error) {
	return w.c.EVD.RegisterFile(ctx, evdapi.RegisterFile{IdempotencyKey: w.key(), FileID: id, FileName: name, MimeType: mime,
		ContentBase64: base64.StdEncoding.EncodeToString([]byte(content)), SupersedesFileID: supersedes})
}

// Python test_foundation.py test_attachment_immutable_versions_and_access.
func TestFilesAreImmutableVersionsWithAccess(t *testing.T) {
	w := newWorld(t)
	editor := w.actor("editor", allPermissions...)
	r, err := w.file(editor, "f1", "evidence.txt", evdapi.MimeText, "原文", "")
	w.must(err)
	if r.Status != "registered" {
		t.Fatalf("register %+v", r)
	}
	_, err = w.file(editor, "f2", "evidence.txt", evdapi.MimeText, "new", "f1")
	w.must(err)
	f, err := w.c.EVD.GetFile(editor, "f1")
	w.must(err)
	if string(f.Content) != "原文" {
		t.Fatalf("f1 content %q", f.Content)
	}
	outsider := w.actor("outsider", allPermissions...)
	_, err = w.c.EVD.GetFile(outsider, "f1")
	w.want(err, kernel.CodeNotFound)
	_, err = w.c.EVD.RegisterFile(editor, evdapi.RegisterFile{IdempotencyKey: w.key(), FileID: "invalid", FileName: "evidence.txt",
		MimeType: evdapi.MimeText, ContentBase64: "%%%bad"})
	w.want(err, evdapi.CodeFileInvalid)
	if err = w.exec("company.A", `UPDATE brant_evd.business_file SET file_name = 'x.txt'`); err == nil {
		t.Fatal("business files must be immutable")
	}
	if ev := w.events("company.A", evdapi.Source); len(ev) != 2 || ev[1].Payload["supersedes_file_id"] != "f1" {
		t.Fatalf("events %+v", ev)
	}
}

// Python test_foundation.py test_file_signature_size_and_branching_are_explicit.
func TestFileSignatureAndBranchingAreExplicit(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("editor", allPermissions...)
	_, err := w.file(ctx, "pdf", "x.pdf", evdapi.MimePDF, "not a pdf", "")
	w.want(err, evdapi.CodeFileInvalid)
	_, err = w.file(ctx, "root", "a.txt", evdapi.MimeText, "original", "")
	w.must(err)
	_, err = w.file(ctx, "version2", "a.txt", evdapi.MimeText, "original", "root")
	w.must(err)
	_, err = w.file(ctx, "branch", "a.txt", evdapi.MimeText, "original", "root")
	w.want(err, evdapi.CodeBusinessKeyConflict)
	_, err = w.file(ctx, "root", "a.txt", evdapi.MimeText, "again", "")
	w.want(err, evdapi.CodeBusinessKeyConflict)
}

// Python test_foundation.py test_attachment_limit_and_large_valid_content.
func TestFileSizeLimit(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("editor", allPermissions...)
	_, err := w.file(ctx, "large", "large.txt", evdapi.MimeText, strings.Repeat("A", 1100*1024), "")
	w.must(err)
	_, err = w.file(ctx, "too.large", "large.txt", evdapi.MimeText, strings.Repeat("A", evdapi.MaxFileBytes+1), "")
	w.want(err, evdapi.CodeFileInvalid)
}
