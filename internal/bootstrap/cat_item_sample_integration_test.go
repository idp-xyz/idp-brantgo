//go:build integration

package bootstrap_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/plugin"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CAT styles and sample approvals (S7-02). A matrix is confirmed onto SKU specification revisions.
// SampleGuard v1 accepts the bound revision, refuses a newer one and a missing approval, and fails
// closed after revoke. Another company sees none of it. An apparel quote that names a sample stage
// cannot freeze without that approval; a generic quote still freezes.

func (w *world) companyHuman(company, id string) context.Context {
	return w.provision(bootstrap.Grant{Scope: w.scope(company), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id},
		Permissions: allPermissions, DataScope: "company"})
}

func (w *world) sampleGuard(company string, ref catapi.SampleRef) catapi.SampleGuardVerdict {
	w.t.Helper()
	db, done := w.helperDB()
	defer done()
	var v catapi.SampleGuardVerdict
	err := bento.ScopeTransactor{DB: db, Scope: w.scope(company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		v, err = catapi.ScanSampleGuard(ref, exec.QueryRow(ctx, catapi.SampleGuardSQL, catapi.SampleGuardArgs(ref)...).Scan)
		return err
	})
	w.must(err)
	return v
}

func styleTech(color, size, sku string) catapi.SaveItem {
	param := func(value, unit string, measured bool) catapi.MeasuredValue {
		return catapi.MeasuredValue{Value: value, Unit: unit, Measured: measured, SourceEvidenceID: "src.1"}
	}
	return catapi.SaveItem{DocumentProfile: catapi.ProfileApparel, StyleCode: "ST01", FabricComposition: "棉针织",
		FabricWidth: param("150.5", catapi.UnitCentimetre, false), FabricGSM: param("180", catapi.UnitGSM, true),
		WarpShrinkage: param("3.25", catapi.UnitPercent, false), WeftShrinkage: param("2", catapi.UnitPercent, false),
		DyeLotPolicy: catapi.DyeLotSameColor, SizeChartEvidenceID: "src.1",
		Variants: []catapi.ItemVariant{{ColorCode: color, SizeCode: size, SkuID: sku, SpecRevision: 1}}}
}

func TestItemMatrixAndSampleGuard(t *testing.T) {
	w := newWorld(t)
	editor := w.companyHuman("company.A", "editor")
	inspector := w.companyHuman("company.A", "inspector")
	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "ai.1"},
		Permissions: allPermissions, DataScope: "company"})
	other := w.companyHuman("company.B", "other")
	w.source(editor, "src.1", "text")
	w.sku(editor, "sku.red.s", true, "src.1")
	w.sku(editor, "sku.red.m", true, "src.1")

	item := styleTech("RED", "S", "sku.red.s")
	item.IdempotencyKey = w.key()
	item.ItemID = "item.1"
	item.Variants = append(item.Variants, catapi.ItemVariant{ColorCode: "RED", SizeCode: "M", SkuID: "sku.red.m", SpecRevision: 1})
	saved, err := w.c.CAT.SaveItem(editor, item)
	w.must(err)
	replay, err := w.c.CAT.SaveItem(editor, item)
	w.must(err)
	if !replay.Replayed || replay.RowVersion != saved.RowVersion {
		t.Fatalf("replay %+v, saved %+v", replay, saved)
	}
	_, err = w.c.CAT.ConfirmItem(ai, catapi.ConfirmItem{IdempotencyKey: w.key(), ItemID: "item.1", ExpectedVersion: saved.RowVersion, Revision: 1, EvidenceIDs: []string{"src.1"}})
	w.want(err, kernel.CodePermissionDenied)
	confirmed, err := w.c.CAT.ConfirmItem(editor, catapi.ConfirmItem{IdempotencyKey: w.key(), ItemID: "item.1", ExpectedVersion: saved.RowVersion, Revision: 1, EvidenceIDs: []string{"src.1"}})
	w.must(err)
	view, err := w.c.CAT.GetItem(editor, "item.1")
	w.must(err)
	if view.CurrentRevision != 1 || view.StyleCode != "ST01" || len(view.Revisions) != 1 || view.Revisions[0].Status != catapi.StatusConfirmed ||
		len(view.Revisions[0].Variants) != 2 || !view.Revisions[0].FabricGSM.Measured || view.Revisions[0].FabricWidth.Value != "150.5" {
		t.Fatalf("item %+v", view)
	}
	if _, err = w.c.CAT.GetItem(other, "item.1"); code(err) != kernel.CodeNotFound {
		t.Fatalf("other company sees the style: %v", err)
	}
	// Row-level security itself: a bare count as the application role in another company's scope.
	for _, table := range []string{"brant_cat.item", "brant_cat.item_revision", "brant_cat.item_sku_variant"} {
		if a, b := w.count("company.A", table), w.count("company.B", table); a == 0 || b != 0 {
			t.Fatalf("%s: company.A sees %d rows, company.B sees %d", table, a, b)
		}
	}

	pack := strings.Repeat("ab", 32)
	draft, err := w.c.CAT.SaveSample(ai, catapi.SaveSample{IdempotencyKey: w.key(), SampleApprovalID: "sample.1", SkuID: "sku.red.s",
		SpecRevision: 1, SampleStage: catapi.StagePPSample, TechPackSHA256: pack, EvidenceIDs: []string{"src.1"}})
	w.must(err)
	_, err = w.c.CAT.ApproveSample(ai, catapi.ApproveSample{IdempotencyKey: w.key(), SampleApprovalID: "sample.1", ExpectedVersion: draft.RowVersion,
		Revision: 1, EvidenceIDs: []string{"src.1"}})
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.c.CAT.SaveSample(editor, catapi.SaveSample{IdempotencyKey: w.key(), SampleApprovalID: "sample.editor", SkuID: "sku.red.m",
		SpecRevision: 1, SampleStage: catapi.StageFit, TechPackSHA256: pack, EvidenceIDs: []string{"src.1"}})
	w.must(err)
	own, err := w.c.CAT.SaveSample(editor, catapi.SaveSample{IdempotencyKey: w.key(), SampleApprovalID: "sample.own", SkuID: "sku.red.s",
		SpecRevision: 1, SampleStage: catapi.StageProto, TechPackSHA256: pack, EvidenceIDs: []string{"src.1"}})
	w.must(err)
	_, err = w.c.CAT.ApproveSample(editor, catapi.ApproveSample{IdempotencyKey: w.key(), SampleApprovalID: "sample.own", ExpectedVersion: own.RowVersion,
		Revision: 1, EvidenceIDs: []string{"src.1"}})
	w.want(err, catapi.CodeIndependentReviewRequired)

	approved, err := w.c.CAT.ApproveSample(inspector, catapi.ApproveSample{IdempotencyKey: w.key(), SampleApprovalID: "sample.1",
		ExpectedVersion: draft.RowVersion, Revision: 1, EvidenceIDs: []string{"src.1"}})
	w.must(err)
	for _, table := range []string{"brant_cat.sample_approval", "brant_cat.sample_approval_revision"} {
		if a, b := w.count("company.A", table), w.count("company.B", table); a == 0 || b != 0 {
			t.Fatalf("%s: company.A sees %d rows, company.B sees %d", table, a, b)
		}
	}
	bound := catapi.SampleRef{SkuID: "sku.red.s", SpecRevision: 1, Stage: catapi.StagePPSample}
	if got := code(w.sampleGuard("company.A", bound).Check("/stage")); got != "OK" {
		t.Fatalf("bound spec: %s", got)
	}
	newer := bound
	newer.SpecRevision = 2
	if got := code(w.sampleGuard("company.A", newer).Check("/stage")); got != catapi.CodeSampleSpecMismatch {
		t.Fatalf("newer spec: %s", got)
	}
	missing := catapi.SampleRef{SkuID: "sku.red.m", SpecRevision: 1, Stage: catapi.StagePPSample}
	if got := code(w.sampleGuard("company.A", missing).Check("/stage")); got != catapi.CodeSampleApprovalRequired {
		t.Fatalf("missing approval: %s", got)
	}
	if got := code(w.sampleGuard("company.B", bound).Check("/stage")); got != kernel.CodeReferenceInvalid {
		t.Fatalf("other company guard: %s", got)
	}
	w.must(w.reviseSpec(editor, "sku.red.s", 2, w.key()))
	w.reconfirmSku(editor, "sku.red.s", 3)
	if got := code(w.sampleGuard("company.A", bound).Check("/stage")); got != "OK" {
		t.Fatalf("bound spec after the SKU moved: %s", got)
	}
	moved := bound
	moved.SpecRevision = 2
	if got := code(w.sampleGuard("company.A", moved).Check("/stage")); got != catapi.CodeSampleSpecMismatch {
		t.Fatalf("the new specification: %s", got)
	}
	_, err = w.c.CAT.RevokeSample(inspector, catapi.RevokeSample{IdempotencyKey: w.key(), SampleApprovalID: "sample.1",
		ExpectedVersion: approved.RowVersion, Reason: "面料变更"})
	w.must(err)
	if got := code(w.sampleGuard("company.A", bound).Check("/stage")); got != catapi.CodeSampleApprovalRequired {
		t.Fatalf("revoked: %s", got)
	}
	_ = confirmed
	var definer, executable, public bool
	var config []string
	if err = pgtest.AppPool(t).QueryRow(context.Background(), `SELECT p.prosecdef, p.proconfig,
		has_function_privilege('brant_app', p.oid, 'EXECUTE'), has_function_privilege('public', p.oid, 'EXECUTE')
		FROM pg_proc p WHERE p.oid = $1::regprocedure`, "brant_cat.guard_sample_approved_v1(text, bigint, text)").
		Scan(&definer, &config, &executable, &public); err != nil {
		t.Fatal(err)
	}
	if !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" || !executable || public {
		t.Fatalf("guard attributes definer %v config %v app %v public %v", definer, config, executable, public)
	}
}

func TestApparelFreezeRequiresTheNamedSample(t *testing.T) {
	q := newTrdWorld(t)
	q.frozenQuote("quote.generic")
	_, err := q.c.EXT.Enable(q.author, plugin.EnablePlugin{IdempotencyKey: q.key(), PluginID: plugin.ApparelPluginID})
	q.must(err)
	d := q.apparelData()
	d.Lines[0].SampleStage = catapi.StagePPSample
	_, err = q.saveQuote(q.author, "quote.apparel", 0, d)
	q.must(err)
	_, err = q.freezeQuote(q.author, "quote.apparel", 1, 1)
	q.want(err, catapi.CodeSampleApprovalRequired)
	pack := strings.Repeat("cd", 32)
	saved, err := q.c.CAT.SaveSample(q.author, catapi.SaveSample{IdempotencyKey: q.key(), SampleApprovalID: "sample.pp",
		SkuID: "sku.1", SpecRevision: q.spec, SampleStage: catapi.StagePPSample, TechPackSHA256: pack, EvidenceIDs: []string{"evidence"}})
	q.must(err)
	approved, err := q.c.CAT.ApproveSample(q.reviewer, catapi.ApproveSample{IdempotencyKey: q.key(), SampleApprovalID: "sample.pp",
		ExpectedVersion: saved.RowVersion, Revision: 1, EvidenceIDs: []string{"evidence"}})
	q.must(err)
	if _, err = q.freezeQuote(q.author, "quote.apparel", 1, 1); err != nil {
		t.Fatalf("freeze after approval: %v", err)
	}

	// An apparel quote that names no stage freezes without an approval.
	plain, err := q.saveQuote(q.author, "quote.apparel.plain", 0, q.apparelData())
	q.must(err)
	if _, err = q.freezeQuote(q.author, "quote.apparel.plain", plain.RowVersion, 1); err != nil {
		t.Fatalf("apparel quote that names no stage: %v", err)
	}

	// After the approval is revoked a new quote that names the stage does not freeze.
	_, err = q.c.CAT.RevokeSample(q.reviewer, catapi.RevokeSample{IdempotencyKey: q.key(), SampleApprovalID: "sample.pp",
		ExpectedVersion: approved.RowVersion, Reason: "面料变更"})
	q.must(err)
	again, err := q.saveQuote(q.author, "quote.apparel.revoked", 0, d)
	q.must(err)
	_, err = q.freezeQuote(q.author, "quote.apparel.revoked", again.RowVersion, 1)
	q.want(err, catapi.CodeSampleApprovalRequired)

	// A generic quote that names a stage is refused: nothing would check it.
	g := q.data()
	g.Lines[0].SampleStage = catapi.StagePPSample
	named, err := q.saveQuote(q.author, "quote.generic.stage", 0, g)
	q.must(err)
	_, err = q.freezeQuote(q.author, "quote.generic.stage", named.RowVersion, 1)
	q.want(err, plugin.CodeDocumentProfileMismatch)
}

// Two inspectors approve the same sample draft at once: one approval, one version conflict.
func TestTwoConcurrentSampleApprovalsHaveOneWinner(t *testing.T) {
	w := newWorld(t)
	editor := w.companyHuman("company.A", "editor")
	w.source(editor, "src.1", "text")
	w.sku(editor, "sku.race", true, "src.1")
	draft, err := w.c.CAT.SaveSample(editor, catapi.SaveSample{IdempotencyKey: w.key(), SampleApprovalID: "sample.race", SkuID: "sku.race",
		SpecRevision: 1, SampleStage: catapi.StagePPSample, TechPackSHA256: strings.Repeat("ab", 32), EvidenceIDs: []string{"src.1"}})
	w.must(err)
	ctxs := []context.Context{w.companyHuman("company.A", "inspector.a"), w.companyHuman("company.A", "inspector.b")}
	errs := make([]error, len(ctxs))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range ctxs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = w.c.CAT.ApproveSample(ctxs[i], catapi.ApproveSample{IdempotencyKey: w.key(), SampleApprovalID: "sample.race",
				ExpectedVersion: draft.RowVersion, Revision: 1, EvidenceIDs: []string{"src.1"}})
		}(i)
	}
	close(start)
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch code(err) {
		case "OK":
			wins++
		case kernel.CodeVersionConflict:
		default:
			t.Fatalf("concurrent approve: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("winners %d, errors %v", wins, errs)
	}
	if got := code(w.sampleGuard("company.A", catapi.SampleRef{SkuID: "sku.race", SpecRevision: 1, Stage: catapi.StagePPSample}).Check("/stage")); got != "OK" {
		t.Fatalf("guard after the race: %s", got)
	}
}
