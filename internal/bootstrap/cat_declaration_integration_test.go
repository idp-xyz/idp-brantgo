//go:build integration

package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The declaration profile suite (S5-10 CAT half) drives CAT-AGG-DeclarationProfile on PostgreSQL 16
// with the role templates: 单证 (documentation) drafts and verifies, a verifier is never the
// revision's last editor, an AI agent drafts candidates and never verifies. Python V0.50 has no
// declaration profile ("—（Python 未实现）"), so every test cites the design: CAT-INV-002,
// SM-CAT-DECLARATION, S5 D-22, MD-41, R39. All data is synthetic.

const codeTable = "CN-2026"

// declarationProfile is a synthetic profile of a cotton T-shirt for sku at revision rev.
func declarationProfile(key, id string, expected int64, sku string, rev int64, table string) catapi.SaveDeclarationProfile {
	return catapi.SaveDeclarationProfile{IdempotencyKey: key, ProfileID: id, ExpectedVersion: expected, SkuID: sku, SpecRevision: rev,
		CodeTableVersion: table,
		Commodity:        catapi.CommodityClassification{Code: "6109100021", Basis: "合成：针织棉制 T 恤，按合成编码表归类", SourceIDs: []string{"spec.1"}},
		DeclarationName:  "棉制针织T恤衫",
		Elements: []catapi.DeclarationElement{{Name: "品名", Value: "T恤衫"}, {Name: "品牌类型", Value: "无品牌"},
			{Name: "成分含量", Value: "100%棉"}},
		LegalUnits: []catapi.LegalUnitConversion{{Unit: "件", PerBaseUnit: "1", Basis: "一件即一 PCS"},
			{Unit: "千克", PerBaseUnit: "0.250", Basis: "合成：实测单件净重 0.25 千克"}},
		Origin:        catapi.OriginBasis{Country: "CN", Criterion: catapi.OriginWhollyObtained, Basis: "合成：境内生产与缝制"},
		ExportControl: catapi.ExportControlClassification{Status: catapi.ExportNotControlled, Basis: "合成：不在合成管制清单内"},
	}
}

func verifyProfile(key, id string, expected, revision int64, evidence ...string) catapi.VerifyDeclarationProfile {
	return catapi.VerifyDeclarationProfile{IdempotencyKey: key, ProfileID: id, ExpectedVersion: expected, Revision: revision,
		EvidenceIDs: evidence, Reason: "单证专业核实（合成）"}
}

func (w *world) resolveProfile(ctx context.Context, ref catapi.DeclarationProfileRef) catapi.ResolvedDeclarationProfile {
	w.t.Helper()
	got, err := w.c.CAT.ResolveDeclarationProfiles(ctx, []catapi.DeclarationProfileRef{ref})
	w.must(err)
	if len(got) != 1 {
		w.t.Fatalf("every reference is answered: %+v", got)
	}
	return got[ref]
}

// declarationWorld is a world with a confirmed SKU ST01.red.S, two documentation clerks, a
// purchaser, an AI agent holding both declaration profile permissions, and the synthetic sources
// spec.1 (the specification) and ruling.1 (a professional classification opinion).
func declarationWorld(t *testing.T) (w *world, doc1, doc2, purchaser, agent context.Context) {
	w = newWorld(t)
	editor, _ := w.polActors()
	w.sku(editor, "ST01.red.S", true, "evidence")
	doc1, doc2, purchaser = w.assign("doc.1", "documentation"), w.assign("doc.2", "documentation"), w.assign("purchaser.1", "purchaser")
	agent = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "agent.doc"), DataScope: "company",
		Permissions: []string{catapi.CommandSaveDeclarationProfile, catapi.CommandVerifyDeclarationProfile, "cat.read", "evd.read"}})
	w.source(doc1, "spec.1", "合成样例：ST01 规格书 r1，针织棉 T 恤，单件净重 0.25 千克。")
	w.source(doc2, "ruling.1", "合成样例：专业归类意见，商品编码 6109100021，不属于合成管制清单。")
	return w, doc1, doc2, purchaser, agent
}

// CAT-INV-002 and SM-CAT-DECLARATION end to end: an AI candidate is saved as a draft and never
// used (DECLARATION_PROFILE_UNVERIFIED); the AI cannot verify; the revision's last editor cannot
// verify (INDEPENDENT_REVIEW_REQUIRED); another documentation clerk verifies on readable evidence.
// An update is the next revision; verifying it supersedes the first, whose content stays exactly
// as verified (history is not overwritten); the database holds the same rules below the commands.
func TestDeclarationProfileCandidateIsVerifiedIndependently(t *testing.T) {
	w, doc1, doc2, purchaser, agent := declarationWorld(t)
	ref := catapi.DeclarationProfileRef{SkuID: "ST01.red.S", SpecRevision: 1, CodeTableVersion: codeTable}

	cmd := declarationProfile(w.key(), "decl.ST01.red.S", 0, "ST01.red.S", 1, codeTable)
	first, err := w.c.CAT.SaveDeclarationProfile(agent, cmd)
	w.must(err)
	if first.ObjectID != "decl.ST01.red.S" || first.RowVersion != 1 || first.Status != catapi.StatusDraft || first.Replayed {
		t.Fatalf("candidate %+v", first)
	}
	replay, err := w.c.CAT.SaveDeclarationProfile(agent, cmd)
	w.must(err)
	if !replay.Replayed || replay.ActionID != first.ActionID {
		t.Fatalf("replay %+v of %+v", replay, first)
	}
	// CAT-INV-002 negative: an unverified profile is never usable for declaration documents.
	r := w.resolveProfile(doc1, ref)
	if r.Verified || r.Code != catapi.CodeDeclarationProfileUnverified || r.ProfileID != "decl.ST01.red.S" || r.DraftRevision != 1 ||
		r.CurrentRevision != 0 || r.Revision != 0 || r.Profile.Commodity.Code != "" || code(r.Check("/lines/0/declaration_profile")) != catapi.CodeDeclarationProfileUnverified {
		t.Fatalf("a candidate is answered: %+v", r)
	}
	asked := ref
	asked.Revision = 1
	if r = w.resolveProfile(doc1, asked); r.Code != catapi.CodeDeclarationProfileUnverified || r.Status != catapi.StatusDraft ||
		r.Profile.Commodity.Code != "" || r.ContentHash != "" {
		t.Fatalf("a draft's content is never answered: %+v", r)
	}

	_, err = w.c.CAT.VerifyDeclarationProfile(agent, verifyProfile(w.key(), "decl.ST01.red.S", 1, 1, "ruling.1"))
	w.want(err, kernel.CodePermissionDenied) // declaration_profile.verify is human only
	_, err = w.c.CAT.SaveDeclarationProfile(purchaser, declarationProfile(w.key(), "decl.other", 0, "ST01.red.S", 1, "CN-2027"))
	w.want(err, kernel.CodePermissionDenied) // the purchaser's template has no declaration profiles

	edit := declarationProfile(w.key(), "decl.ST01.red.S", 1, "ST01.red.S", 1, codeTable)
	edit.Elements[0].Value = "针织T恤衫"
	_, err = w.c.CAT.SaveDeclarationProfile(doc1, edit)
	w.must(err)
	_, err = w.c.CAT.VerifyDeclarationProfile(doc1, verifyProfile(w.key(), "decl.ST01.red.S", 2, 1, "ruling.1"))
	if code(err) != catapi.CodeIndependentReviewRequired || path(err) != "/args/revision" {
		t.Fatalf("the last editor verified its own revision: %v", err)
	}
	if rej, _ := kernel.AsRejection(err); rej.Category != kernel.CategoryInvalid {
		t.Fatalf("INDEPENDENT_REVIEW_REQUIRED is 422: %+v", rej)
	}
	_, err = w.c.CAT.VerifyDeclarationProfile(doc2, verifyProfile(w.key(), "decl.ST01.red.S", 2, 1, "ruling.unknown"))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence_ids/0" {
		t.Fatalf("unreadable evidence: %v", err)
	}
	verified, err := w.c.CAT.VerifyDeclarationProfile(doc2, verifyProfile(w.key(), "decl.ST01.red.S", 2, 1, "ruling.1"))
	w.must(err)
	if verified.RowVersion != 3 || verified.Status != catapi.StatusVerified {
		t.Fatalf("verified %+v", verified)
	}
	// CAT-INV-002 positive: SKU specification r1 is bound to the verified profile v1.
	r = w.resolveProfile(doc1, ref)
	if !r.Verified || r.Code != "" || r.Revision != 1 || r.CurrentRevision != 1 || r.DraftRevision != 0 || r.Status != catapi.StatusVerified ||
		r.Check("/x") != nil || r.VerifiedBy != actorRef("human", "doc.2") || !slices.Equal(r.EvidenceIDs, []string{"ruling.1"}) ||
		r.VerifiedAt.IsZero() || len(r.ContentHash) != 64 || r.Adopted() != asked {
		t.Fatalf("verified answer %+v", r)
	}
	p := r.Profile
	if p.Commodity.Code != "6109100021" || p.Elements[0] != (catapi.DeclarationElement{Name: "品名", Value: "针织T恤衫"}) || len(p.Elements) != 3 ||
		p.LegalUnits[1] != (catapi.LegalUnitConversion{Unit: "千克", PerBaseUnit: "0.25", Basis: "合成：实测单件净重 0.25 千克"}) ||
		p.Origin.Country != "CN" || p.ExportControl.Status != catapi.ExportNotControlled || p.ExportControl.ListReference != "" ||
		!slices.Equal(p.Commodity.SourceIDs, []string{"spec.1"}) {
		t.Fatalf("verified content %+v", p)
	}
	audit, err := w.c.CAT.Audit(w.actor("auditor", "cat.audit"))
	w.must(err)
	if !slices.ContainsFunc(audit, func(e kernel.AuditEntry) bool {
		return e.Command == catapi.CommandVerifyDeclarationProfile && e.Actor.ID == "doc.1" && e.Outcome == "rejected" &&
			e.Code == catapi.CodeIndependentReviewRequired
	}) || !slices.ContainsFunc(audit, func(e kernel.AuditEntry) bool {
		return e.Command == catapi.CommandVerifyDeclarationProfile && e.Actor.ID == "doc.2" && e.Outcome == "succeeded" &&
			e.Reason == "单证专业核实（合成）"
	}) {
		t.Fatalf("the refusal and the verification are audited: %+v", audit)
	}

	// The update of a verified profile is its next revision; r1 stays in use until r2 is verified.
	update := declarationProfile(w.key(), "decl.ST01.red.S", 3, "ST01.red.S", 1, codeTable)
	update.ExportControl = catapi.ExportControlClassification{Status: catapi.ExportControlled, ListReference: "合成管制清单 1C999",
		Basis: "合成：复核后列入合成清单", SourceIDs: []string{"ruling.1"}}
	saved, err := w.c.CAT.SaveDeclarationProfile(doc2, update)
	w.must(err)
	if saved.RowVersion != 4 || saved.Status != catapi.StatusDraft {
		t.Fatalf("next revision %+v", saved)
	}
	if r = w.resolveProfile(doc1, ref); r.Revision != 1 || !r.Verified || r.DraftRevision != 2 || r.Profile.ExportControl.Status != catapi.ExportNotControlled {
		t.Fatalf("a draft does not change the answer: %+v", r)
	}
	_, err = w.c.CAT.VerifyDeclarationProfile(doc1, verifyProfile(w.key(), "decl.ST01.red.S", 4, 2, "ruling.1"))
	w.must(err)
	if r = w.resolveProfile(doc1, ref); r.Revision != 2 || !r.Verified || r.Profile.ExportControl.ListReference != "合成管制清单 1C999" {
		t.Fatalf("r2 is the verified revision: %+v", r)
	}
	// The first verified revision is stale for new use and still says exactly what was verified.
	if r = w.resolveProfile(doc1, asked); r.Code != catapi.CodeReferenceStale || r.Verified || r.Status != catapi.StatusSuperseded ||
		r.CurrentRevision != 2 || r.Profile.ExportControl.Status != catapi.ExportNotControlled || r.Profile.Elements[0].Value != "针织T恤衫" ||
		r.VerifiedBy.ID != "doc.2" || code(r.Check("/x")) != catapi.CodeReferenceStale {
		t.Fatalf("r1 after r2: %+v", r)
	}
	never := ref
	never.Revision = 3
	if r = w.resolveProfile(doc1, never); r.Code != kernel.CodeReferenceInvalid || r.Revision != 0 {
		t.Fatalf("a revision never assigned: %+v", r)
	}

	v, err := w.c.CAT.GetDeclarationProfile(purchaser, "decl.ST01.red.S")
	w.must(err)
	if v.Key != ref.Key() || v.RootVersion != 5 || v.VerifiedRevision != 2 || v.DraftRevision != 0 || len(v.Revisions) != 2 {
		t.Fatalf("profile %+v", v)
	}
	r1, r2 := v.Revisions[0], v.Revisions[1]
	if r1.Status != catapi.StatusSuperseded || r1.SupersededAt.IsZero() || r1.SavedBy != actorRef("human", "doc.1") || r1.VerifiedBy.ID != "doc.2" ||
		r1.ExportControl.Status != catapi.ExportNotControlled || r2.Status != catapi.StatusVerified || r2.SavedBy.ID != "doc.2" ||
		r2.VerifiedBy.ID != "doc.1" || !r2.SupersededAt.IsZero() || r1.ContentHash == r2.ContentHash {
		t.Fatalf("revisions %+v", v.Revisions)
	}

	var events []event
	for _, e := range w.events("company.A", catapi.Source) {
		if e.Type == catapi.EventDeclarationProfileVerified {
			events = append(events, e)
		}
	}
	if len(events) != 2 {
		t.Fatalf("one event per verification, none per draft: %+v", events)
	}
	e1, e2 := events[0].Payload, events[1].Payload
	if e1["profile_id"] != "decl.ST01.red.S" || e1["revision"] != float64(1) || e1["root_version"] != float64(3) || e1["sku_id"] != "ST01.red.S" ||
		e1["spec_revision"] != float64(1) || e1["code_table_version"] != codeTable || e1["content_hash"] != r1.ContentHash ||
		e1["aggregate_type"] != "DeclarationProfile" || e2["revision"] != float64(2) || e2["superseded_revision"] != float64(1) {
		t.Fatalf("payloads %+v %+v", e1, e2)
	}
	if _, ok := e1["superseded_revision"]; ok {
		t.Fatal("nothing was superseded by r1")
	}
	for _, field := range []string{"commodity", "commodity_code", "export_control"} {
		if _, ok := e2[field]; ok {
			t.Fatalf("events carry identifiers, versions and the hash, not the classification: %+v", e2)
		}
	}

	// Below the commands: the trigger keeps verified and superseded revisions as they are, the
	// CHECK keeps the verifier independent of the last editor, the grants delete nothing and leave
	// the key alone.
	_, err = w.c.CAT.SaveDeclarationProfile(doc1, declarationProfile(w.key(), "decl.ST01.red.S", 5, "ST01.red.S", 1, codeTable))
	w.must(err)
	for _, c := range []struct{ sql, want string }{
		{`UPDATE brant_cat.declaration_profile_revision SET content = jsonb_set(content, '{commodity,code}', '"6109100099"')
			WHERE profile_id = 'decl.ST01.red.S' AND revision = 2`, "23000"},
		{`UPDATE brant_cat.declaration_profile_revision SET status = 'verified', superseded_at = NULL
			WHERE profile_id = 'decl.ST01.red.S' AND revision = 1`, "23000"},
		{`UPDATE brant_cat.declaration_profile SET root_version = 1 WHERE profile_id = 'decl.ST01.red.S'`, "23000"},
		// The CHECK decides before the index of verified revisions sees a second one.
		{`UPDATE brant_cat.declaration_profile_revision SET status = 'verified', verified_by_kind = 'human', verified_by_id = 'doc.1',
			verify_epoch = 1, verified_at = now(), evidence_ids = ARRAY['ruling.1'] WHERE profile_id = 'decl.ST01.red.S' AND revision = 3`, "23514"},
		{`UPDATE brant_cat.declaration_profile_revision SET content = content - 'origin' WHERE profile_id = 'decl.ST01.red.S' AND revision = 3`, "23514"},
		{`UPDATE brant_cat.declaration_profile SET code_table_version = 'CN-2027' WHERE profile_id = 'decl.ST01.red.S'`, "42501"},
		{`DELETE FROM brant_cat.declaration_profile_revision WHERE profile_id = 'decl.ST01.red.S'`, "42501"},
	} {
		got, _, _ := strings.Cut(sqlState(w.exec("company.A", c.sql)), " ")
		if got != c.want {
			t.Errorf("%s: %s, want %s", c.sql, got, c.want)
		}
	}
	if n := w.count("company.A", "brant_cat.declaration_profile_revision WHERE status = 'superseded'"); n != 1 {
		t.Fatalf("%d superseded revisions", n)
	}
}

// D-22: a profile is kept per SKU specification revision and code table version, one per key in a
// company; the key is fixed; only confirmed revisions of SKUs the clerk sees get one, also a
// revision the SKU has moved on from (goods made to it are still declared), while the new revision
// needs its own; the content's sources must be readable; profiles stay in their company.
func TestDeclarationProfileIdentityPerSkuSpecAndCodeTable(t *testing.T) {
	w, doc1, doc2, _, _ := declarationWorld(t)
	editor, _ := w.polActors()
	w.sku(editor, "ST01.draft", false)

	_, err := w.c.CAT.SaveDeclarationProfile(doc1, declarationProfile(w.key(), "p.2026", 0, "ST01.red.S", 1, codeTable))
	w.must(err)
	_, err = w.c.CAT.SaveDeclarationProfile(doc1, declarationProfile(w.key(), "p.2026.b", 0, "ST01.red.S", 1, codeTable))
	if code(err) != catapi.CodeBusinessKeyConflict || path(err) != "/args/id" {
		t.Fatalf("a second profile of one key: %v", err)
	}
	if rej, _ := kernel.AsRejection(err); rej.Category != kernel.CategoryConflict {
		t.Fatalf("BUSINESS_KEY_CONFLICT is a conflict: %+v", rej)
	}
	err = w.exec("company.A", `INSERT INTO brant_cat.declaration_profile (tenant_id, company_id, profile_id, sku_id, spec_revision,
		code_table_version, root_version, last_revision, created_at, updated_at)
		VALUES (current_setting('app.tenant_id'), 'company.A', 'p.direct', 'ST01.red.S', 1, 'CN-2026', 1, 1, now(), now())`)
	if got := sqlState(err); got != "23505 declaration_profile_key" {
		t.Fatalf("the unique key below the command: %s", got)
	}
	_, err = w.c.CAT.SaveDeclarationProfile(doc1, declarationProfile(w.key(), "p.2027", 0, "ST01.red.S", 1, "CN-2027"))
	w.must(err) // another code table version is another profile
	for _, c := range []struct {
		cmd      catapi.SaveDeclarationProfile
		code, at string
	}{
		{declarationProfile("", "p.2026", 1, "ST01.red.S", 2, codeTable), kernel.CodeInvalidKnownValue, "/args/spec_revision"},
		{declarationProfile("", "p.2026", 1, "ST01.red.S", 1, "CN-2027"), kernel.CodeInvalidKnownValue, "/args/code_table_version"},
		{declarationProfile("", "p.unknown", 0, "ST01.unknown", 1, codeTable), kernel.CodeReferenceInvalid, "/args/sku_id"},
		{declarationProfile("", "p.draft", 0, "ST01.draft", 1, codeTable), kernel.CodeReferenceInvalid, "/args/spec_revision"},
		{declarationProfile("", "p.r2", 0, "ST01.red.S", 2, codeTable), kernel.CodeReferenceInvalid, "/args/spec_revision"},
	} {
		c.cmd.IdempotencyKey = w.key()
		if _, err = w.c.CAT.SaveDeclarationProfile(doc1, c.cmd); code(err) != c.code || path(err) != c.at {
			t.Errorf("%s %s r%d %s: %v, want %s at %s", c.cmd.ProfileID, c.cmd.SkuID, c.cmd.SpecRevision, c.cmd.CodeTableVersion, err, c.code, c.at)
		}
	}
	unreadable := declarationProfile(w.key(), "p.2026", 1, "ST01.red.S", 1, codeTable)
	unreadable.Origin.SourceIDs = []string{"origin.unknown"}
	if _, err = w.c.CAT.SaveDeclarationProfile(doc1, unreadable); code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/origin/source_ids/0" {
		t.Fatalf("a source the clerk cannot read: %v", err)
	}

	// The SKU moves on to r2: r1's profile is still verified for goods of r1; r2 has none yet.
	_, err = w.c.CAT.ReviseSpec(editor, catapi.ReviseSpec{IdempotencyKey: w.key(), SkuID: "ST01.red.S", ExpectedVersion: 2, Reason: "合成技术变更"})
	w.must(err)
	_, err = w.c.CAT.ConfirmSku(editor, catapi.ConfirmSku{IdempotencyKey: w.key(), SkuID: "ST01.red.S", ExpectedVersion: 3, EvidenceIDs: []string{"evidence"}})
	w.must(err)
	_, err = w.c.CAT.VerifyDeclarationProfile(doc2, verifyProfile(w.key(), "p.2026", 1, 1, "ruling.1"))
	w.must(err)
	refs := []catapi.DeclarationProfileRef{
		{SkuID: "ST01.red.S", SpecRevision: 1, CodeTableVersion: codeTable},
		{SkuID: "ST01.red.S", SpecRevision: 2, CodeTableVersion: codeTable},
		{SkuID: "ST01.red.S", SpecRevision: 1, CodeTableVersion: "CN-2027"},
		{SkuID: "ST01.unknown", SpecRevision: 1, CodeTableVersion: codeTable, Revision: 1},
		{SkuID: "ST01.red.S", SpecRevision: 1, CodeTableVersion: codeTable},
	}
	got, err := w.c.CAT.ResolveDeclarationProfiles(doc1, refs)
	w.must(err)
	if len(got) != 4 {
		t.Fatalf("every reference is answered once: %+v", got)
	}
	for i, want := range []string{"", catapi.CodeDeclarationProfileUnverified, catapi.CodeDeclarationProfileUnverified, kernel.CodeReferenceInvalid} {
		if r := got[refs[i]]; r.Ref != refs[i] || r.Code != want || r.Verified != (want == "") {
			t.Errorf("%+v: %+v, want %q", refs[i], r, want)
		}
	}
	if r := got[refs[1]]; r.ProfileID != "" || r.DraftRevision != 0 {
		t.Fatalf("no profile is kept for r2: %+v", r)
	}
	if r := got[refs[2]]; r.ProfileID != "p.2027" || r.DraftRevision != 1 {
		t.Fatalf("CN-2027 is a draft: %+v", r)
	}
	_, err = w.c.CAT.ResolveDeclarationProfiles(doc1, []catapi.DeclarationProfileRef{{SkuID: "ST01.red.S", SpecRevision: 1}})
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/refs/0/code_table_version" {
		t.Fatalf("a reference without its code table version: %v", err)
	}
	_, err = w.c.CAT.SaveDeclarationProfile(doc1, declarationProfile(w.key(), "p.r2", 0, "ST01.red.S", 2, codeTable))
	w.must(err)

	list, err := w.c.CAT.ListDeclarationProfiles(doc2, "ST01.red.S")
	w.must(err)
	var ids []string
	for _, s := range list.Profiles {
		ids = append(ids, s.ProfileID+"@"+strconv.FormatInt(s.Key.SpecRevision, 10)+"/"+s.Key.CodeTableVersion+
			":"+strconv.FormatInt(s.VerifiedRevision, 10)+"/"+strconv.FormatInt(s.DraftRevision, 10))
	}
	if list.SkuID != "ST01.red.S" || !slices.Equal(ids, []string{"p.2026@1/CN-2026:1/0", "p.2027@1/CN-2027:0/1", "p.r2@2/CN-2026:0/1"}) {
		t.Fatalf("profiles of the SKU: %v", ids)
	}
	if none, err := w.c.CAT.ListDeclarationProfiles(doc2, "ST01.draft"); err != nil || len(none.Profiles) != 0 {
		t.Fatalf("a SKU without profiles: %+v %v", none, err)
	}

	// A verifier reads what it verifies: every source the revision cites, not only its own evidence.
	narrow := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "doc.own"), DataScope: "own",
		Permissions: []string{catapi.CommandVerifyDeclarationProfile, "cat.read", "evd.read", "source.register"}})
	w.source(narrow, "ruling.own", "合成样例：本人核实依据。")
	_, err = w.c.CAT.VerifyDeclarationProfile(narrow, verifyProfile(w.key(), "p.2027", 1, 1, "ruling.own"))
	if rej, _ := kernel.AsRejection(err); code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/revision" ||
		!strings.Contains(rej.Fields[0].Reason, "spec.1") {
		t.Fatalf("a revision citing a source the verifier cannot read: %v", err)
	}

	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: actorRef("human", "doc.1"), DataScope: "company",
		Permissions: []string{"cat.read", catapi.CommandSaveDeclarationProfile}})
	_, err = w.c.CAT.GetDeclarationProfile(other, "p.2026")
	w.want(err, kernel.CodeNotFound)
	if r := w.resolveProfile(other, refs[0]); r.Verified || r.ProfileID != "" || r.Code != catapi.CodeDeclarationProfileUnverified {
		t.Fatalf("another company's profile: %+v", r)
	}
	if n := w.count("company.B", "brant_cat.declaration_profile_revision"); n != 0 {
		t.Fatalf("FORCE RLS shows company.B %d revisions of company.A", n)
	}
	if n := w.count("company.A", "brant_cat.declaration_profile"); n != 3 {
		t.Fatalf("company.A keeps %d profiles", n)
	}
}

// Concurrency under the root lock and the unique key: two creations of one key have one winner
// (the other is BUSINESS_KEY_CONFLICT whether it saw the winner or collided on the index and was
// retried), and two verifications of one draft by two clerks have one winner (the other saw the root
// version move: VERSION_CONFLICT); a profile never has two verified revisions.
func TestConcurrentDeclarationProfileCommandsHaveOneWinner(t *testing.T) {
	w, doc1, doc2, _, _ := declarationWorld(t)
	doc3 := w.assign("doc.3", "documentation")
	race := func(calls ...func() (catapi.Receipt, error)) ([]catapi.Receipt, []error) {
		receipts, errs := make([]catapi.Receipt, len(calls)), make([]error, len(calls))
		var wg sync.WaitGroup
		for i := range calls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				receipts[i], errs[i] = calls[i]()
			}()
		}
		wg.Wait()
		return receipts, errs
	}
	for round := range 8 {
		table := "CN-R" + strconv.Itoa(round)
		a := declarationProfile(w.key(), "race."+strconv.Itoa(round)+".a", 0, "ST01.red.S", 1, table)
		b := declarationProfile(w.key(), "race."+strconv.Itoa(round)+".b", 0, "ST01.red.S", 1, table)
		receipts, errs := race(func() (catapi.Receipt, error) { return w.c.CAT.SaveDeclarationProfile(doc1, a) },
			func() (catapi.Receipt, error) { return w.c.CAT.SaveDeclarationProfile(doc1, b) })
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("round %d: want one creator: %v", round, errs)
		}
		winner := 0
		if errs[0] != nil {
			winner = 1
		}
		if code(errs[1-winner]) != catapi.CodeBusinessKeyConflict || receipts[winner].Status != catapi.StatusDraft {
			t.Fatalf("round %d: winner %+v, loser %v", round, receipts[winner], errs[1-winner])
		}
		id := receipts[winner].ObjectID
		receipts, errs = race(func() (catapi.Receipt, error) {
			return w.c.CAT.VerifyDeclarationProfile(doc2, verifyProfile(w.key(), id, 1, 1, "ruling.1"))
		}, func() (catapi.Receipt, error) {
			return w.c.CAT.VerifyDeclarationProfile(doc3, verifyProfile(w.key(), id, 1, 1, "ruling.1"))
		})
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("round %d: want one verifier: %v", round, errs)
		}
		winner = 0
		if errs[0] != nil {
			winner = 1
		}
		if code(errs[1-winner]) != kernel.CodeVersionConflict || receipts[winner].Status != catapi.StatusVerified || receipts[winner].RowVersion != 2 {
			t.Fatalf("round %d: winner %+v, loser %v", round, receipts[winner], errs[1-winner])
		}
	}
	if n := w.count("company.A", "brant_cat.declaration_profile"); n != 8 {
		t.Fatalf("%d profiles for 8 keys", n)
	}
	if n := w.count("company.A", "brant_cat.declaration_profile_revision WHERE status = 'verified'"); n != 8 {
		t.Fatalf("%d verified revisions for 8 profiles", n)
	}
}

// S5-10 over HTTP with OIDC access tokens and the role templates: an AI agent saves a candidate
// (declaration_profile.save_draft), cannot verify it (403); the documentation clerk who last edited
// it is refused (422 INDEPENDENT_REVIEW_REQUIRED); another clerk verifies; both read routes answer.
func TestDeclarationProfileOverHTTP(t *testing.T) {
	w, _, _, _, _ := declarationWorld(t)
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	as := func(actor, kind string, n int) *client {
		return &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", actor, kind), n: n}
	}
	agent, doc1, doc2 := as("agent.doc", "ai", 0), as("doc.1", "human", 100), as("doc.2", "human", 200)
	args := map[string]any{"id": "decl.http", "expected_version": 0, "sku_id": "ST01.red.S", "spec_revision": 1, "code_table_version": codeTable,
		"commodity":        map[string]any{"code": "6109100021", "basis": "合成：AI 归类候选", "source_ids": []string{"spec.1"}},
		"declaration_name": "棉制针织T恤衫",
		"elements":         []any{map[string]any{"name": "品名", "value": "T恤衫"}, map[string]any{"name": "成分含量", "value": "100%棉"}},
		"legal_units":      []any{map[string]any{"unit": "件", "per_base_unit": "1", "basis": "一件即一 PCS"}},
		"origin":           map[string]any{"country": "CN", "criterion": "wholly_obtained", "basis": "合成：境内生产"},
		"export_control":   map[string]any{"status": "not_controlled", "basis": "合成：不在合成管制清单内"}}
	if r := agent.command("cat", catapi.Contract, catapi.CommandSaveDeclarationProfile, args); r["status"] != "draft" || r["row_version"] != float64(1) {
		t.Fatalf("candidate %v", r)
	}
	verify := map[string]any{"id": "decl.http", "expected_version": 1, "revision": 1, "evidence_ids": []string{"ruling.1"}, "reason": "单证专业核实（合成）"}
	if status, out := agent.try("cat", catapi.Contract, catapi.CommandVerifyDeclarationProfile, verify); status != http.StatusForbidden ||
		out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("an AI agent verified: %d %v", status, out)
	}
	args["expected_version"], args["declaration_name"] = 1, "棉制针织T恤"
	doc1.command("cat", catapi.Contract, catapi.CommandSaveDeclarationProfile, args)
	verify["expected_version"] = 2
	if status, out := doc1.try("cat", catapi.Contract, catapi.CommandVerifyDeclarationProfile, verify); status != http.StatusUnprocessableEntity ||
		out["code"] != catapi.CodeIndependentReviewRequired {
		t.Fatalf("the last editor verified: %d %v", status, out)
	}
	if r := doc2.command("cat", catapi.Contract, catapi.CommandVerifyDeclarationProfile, verify); r["status"] != "verified" || r["row_version"] != float64(3) {
		t.Fatalf("verify %v", r)
	}
	status, v := doc1.do("GET", "/api/brant/v1/cat/declaration_profiles/decl.http", nil)
	revs, _ := v["revisions"].([]any)
	if status != http.StatusOK || v["verified_revision"] != float64(1) || len(revs) != 1 || v["key"].(map[string]any)["code_table_version"] != codeTable {
		t.Fatalf("profile %d %v", status, v)
	}
	rev := revs[0].(map[string]any)
	if rev["status"] != "verified" || rev["declaration_name"] != "棉制针织T恤" || rev["commodity"].(map[string]any)["code"] != "6109100021" ||
		rev["saved_by"].(map[string]any)["id"] != "doc.1" || rev["verified_by"].(map[string]any)["id"] != "doc.2" ||
		rev["legal_units"].([]any)[0].(map[string]any)["per_base_unit"] != "1" {
		t.Fatalf("revision %v", rev)
	}
	status, list := doc2.do("GET", "/api/brant/v1/cat/skus/ST01.red.S/declaration_profiles", nil)
	profiles, _ := list["profiles"].([]any)
	if status != http.StatusOK || len(profiles) != 1 || profiles[0].(map[string]any)["profile_id"] != "decl.http" ||
		profiles[0].(map[string]any)["verified_revision"] != float64(1) {
		t.Fatalf("list %d %v", status, list)
	}
	if status, out := doc2.do("GET", "/api/brant/v1/cat/declaration_profiles/decl.unknown", nil); status != http.StatusNotFound {
		t.Fatalf("unknown profile %d %v", status, out)
	}
}
