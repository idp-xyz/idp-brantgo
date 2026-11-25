//go:build integration

package bootstrap_test

import (
	"context"
	"sync"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/plugin"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S7-02: a cost BOM is not adoptable until a different costing human verifies it. A size the
// revision does not store is BOM_SIZE_GRADE_MISSING. A decided substitution cannot be updated.
// An apparel quote that cites an unverified BOM does not freeze; a generic quote still does.

func TestCostBOMGuardVerificationSizeGradeAndSubstitution(t *testing.T) {
	w := newWorld(t)
	drafter := w.actor("bom.drafter", cstapi.CommandSaveCostBOM, cstapi.CommandVerifyCostBOM, cstapi.CommandRequestSubstitution, "cst.read")
	checker := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "bom.checker"},
		Permissions: []string{cstapi.CommandVerifyCostBOM, cstapi.CommandWithdrawCostBOM, cstapi.CommandDecideSubstitution, "cst.read"},
		DataScope:   "company"})
	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "bom.other"},
		Permissions: []string{cstapi.CommandSaveCostBOM, "cst.read"}, DataScope: "company"})

	saved, err := w.c.CST.SaveCostBOM(drafter, bomDraft("bom.1", "apparel", true, cstapi.GradingUniform, nil))
	w.must(err)
	if v := w.costBOMGuard("company.A", cstapi.CostBOMRef{BomID: "bom.1", Revision: 1}); v.Check("/bom") == nil || code(v.Check("/bom")) != cstapi.CodeBOMUnverified {
		t.Fatalf("draft guard %+v", v)
	}
	_, err = w.c.CST.VerifyCostBOM(drafter, cstapi.VerifyCostBOM{IdempotencyKey: w.key(), BomID: "bom.1", ExpectedVersion: saved.RowVersion, Revision: 1})
	w.want(err, kernel.CodePermissionDenied)
	verified, err := w.c.CST.VerifyCostBOM(checker, cstapi.VerifyCostBOM{IdempotencyKey: w.key(), BomID: "bom.1", ExpectedVersion: saved.RowVersion, Revision: 1})
	w.must(err)
	if verified.Status != "verified" {
		t.Fatalf("status %s", verified.Status)
	}
	hash := bomHash(t, w)
	if v := w.costBOMGuard("company.A", cstapi.CostBOMRef{BomID: "bom.1", Revision: 1, ContentSHA256: hash}); v.Check("/bom") != nil || v.ContentSHA256 != hash {
		t.Fatalf("verified guard %+v", v)
	}
	if v := w.costBOMGuard("company.A", cstapi.CostBOMRef{BomID: "bom.1", Revision: 1, ContentSHA256: "0000000000000000000000000000000000000000000000000000000000000000"}); code(v.Check("/bom")) != kernel.CodeReferenceInvalid {
		t.Fatalf("hash mismatch %+v", v)
	}
	withdrawn, err := w.c.CST.WithdrawCostBOM(checker, cstapi.WithdrawCostBOM{IdempotencyKey: w.key(), BomID: "bom.1", ExpectedVersion: verified.RowVersion,
		Revision: 1, Reason: "口径作废"})
	w.must(err)
	if withdrawn.Status != "withdrawn" {
		t.Fatalf("withdraw %s", withdrawn.Status)
	}
	if code(w.costBOMGuard("company.A", cstapi.CostBOMRef{BomID: "bom.1", Revision: 1, ContentSHA256: hash}).Check("/bom")) != cstapi.CodeBOMUnverified {
		t.Fatal("withdrawn BOM stayed adoptable")
	}

	grades := []cstapi.SizeGrade{{SizeCode: "S", ConsumptionPerUnit: "1.45"}, {SizeCode: "2XL", ConsumptionPerUnit: "1.78"}}
	sized, err := w.c.CST.SaveCostBOM(drafter, bomDraft("bom.size", "apparel", true, cstapi.GradingSized, grades))
	w.must(err)
	if _, err = w.c.CST.VerifyCostBOM(checker, cstapi.VerifyCostBOM{IdempotencyKey: w.key(), BomID: "bom.size", ExpectedVersion: sized.RowVersion, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if err = w.costBOMGuard("company.A", cstapi.CostBOMRef{BomID: "bom.size", Revision: 1, SizeCode: "S"}).Check("/bom"); err != nil {
		t.Fatal(err)
	}
	for _, size := range []string{"3XL", "M"} {
		if code(w.costBOMGuard("company.A", cstapi.CostBOMRef{BomID: "bom.size", Revision: 1, SizeCode: size}).Check("/bom")) != cstapi.CodeBOMSizeGradeMissing {
			t.Fatalf("size %s fell back or was accepted", size)
		}
	}

	unmeasured, err := w.c.CST.SaveCostBOM(drafter, bomDraft("bom.raw", "apparel", false, cstapi.GradingUniform, nil))
	w.must(err)
	if _, err = w.c.CST.VerifyCostBOM(checker, cstapi.VerifyCostBOM{IdempotencyKey: w.key(), BomID: "bom.raw", ExpectedVersion: unmeasured.RowVersion, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if code(w.costBOMGuard("company.A", cstapi.CostBOMRef{BomID: "bom.raw", Revision: 1}).Check("/bom")) != cstapi.CodeBOMUnverified {
		t.Fatal("unmeasured BOM was adoptable")
	}

	if w.count("company.B", "brant_cst.cost_bom") != 0 {
		t.Fatal("company.B sees company.A's cost BOM")
	}
	if w.costBOMGuard("company.B", cstapi.CostBOMRef{BomID: "bom.size", Revision: 1, SizeCode: "S"}).Known {
		t.Fatal("company.B's guard saw the BOM")
	}
	if _, err = w.c.CST.SaveCostBOM(other, bomDraft("bom.1", "generic", true, cstapi.GradingUniform, nil)); err != nil {
		t.Fatal(err)
	}
	if w.count("company.A", "brant_cst.cost_bom") < 3 || w.count("company.B", "brant_cst.cost_bom") != 1 {
		t.Fatalf("counts A %d B %d", w.count("company.A", "brant_cst.cost_bom"), w.count("company.B", "brant_cst.cost_bom"))
	}

	_, err = w.c.CST.RequestSubstitution(drafter, substitution("bom.size", "sub.1", sized.RowVersion+1))
	w.must(err)
	_, err = w.c.CST.DecideSubstitution(drafter, cstapi.DecideSubstitution{IdempotencyKey: w.key(), BomID: "bom.size", ExpectedVersion: sized.RowVersion + 2,
		SubstitutionID: "sub.1", Decision: cstapi.DecisionApprove})
	w.want(err, kernel.CodePermissionDenied)
	decided, err := w.c.CST.DecideSubstitution(checker, cstapi.DecideSubstitution{IdempotencyKey: w.key(), BomID: "bom.size", ExpectedVersion: sized.RowVersion + 2,
		SubstitutionID: "sub.1", Decision: cstapi.DecisionApprove})
	w.must(err)
	if decided.Status != "approved" {
		t.Fatalf("decision %s", decided.Status)
	}
	if err = w.exec("company.A", `UPDATE brant_cst.cost_bom_substitution SET reason = 'changed' WHERE substitution_id = 'sub.1'`); err == nil {
		t.Fatal("a decided substitution row was updated")
	}
	if code(w.costBOMGuard("company.A", cstapi.CostBOMRef{BomID: "bom.size", Revision: 1, SizeCode: "S"}).Check("/bom")) != cstapi.CodeBOMUnverified {
		t.Fatal("superseded revision stayed adoptable")
	}
	if err = w.costBOMGuard("company.A", cstapi.CostBOMRef{BomID: "bom.size", Revision: 2, SizeCode: "S"}).Check("/bom"); err != nil {
		t.Fatalf("approved revision: %v", err)
	}
}

func TestTwoConcurrentCostBOMVerifiesHaveOneWinner(t *testing.T) {
	w := newWorld(t)
	drafter := w.actor("race.drafter", cstapi.CommandSaveCostBOM)
	checker := func(id string) context.Context {
		return w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id},
			Permissions: []string{cstapi.CommandVerifyCostBOM, "cst.read"}, DataScope: "company"})
	}
	a, b := checker("race.a"), checker("race.b")
	saved, err := w.c.CST.SaveCostBOM(drafter, bomDraft("bom.race", "apparel", true, cstapi.GradingUniform, nil))
	w.must(err)
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	ctxs := []context.Context{a, b}
	for i := range ctxs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = w.c.CST.VerifyCostBOM(ctxs[i], cstapi.VerifyCostBOM{IdempotencyKey: w.key(), BomID: "bom.race",
				ExpectedVersion: saved.RowVersion, Revision: 1})
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
			t.Fatalf("concurrent verify %s", code(err))
		}
	}
	if wins != 1 {
		t.Fatalf("winners %d, errors %v", wins, errs)
	}
}

func TestApparelQuoteFreezeRefusesAnUnverifiedCostBOM(t *testing.T) {
	q := newTrdWorld(t)
	generic, err := q.saveQuote(q.author, "quote.generic", 0, q.data())
	q.must(err)
	if _, err = q.freezeQuote(q.author, "quote.generic", generic.RowVersion, 1); err != nil {
		t.Fatalf("generic quote: %v", err)
	}
	_, err = q.c.EXT.Enable(q.author, plugin.EnablePlugin{IdempotencyKey: q.key(), PluginID: plugin.ApparelPluginID})
	q.must(err)
	if _, err = q.c.CST.SaveCostBOM(q.author, bomDraft("bom.quote", "apparel", true, cstapi.GradingUniform, nil)); err != nil {
		t.Fatal(err)
	}
	d := q.apparelData()
	d.CostBOMRef = &trdapi.CostBOMRef{BomID: "bom.quote", Revision: 1}
	if _, err = q.saveQuote(q.author, "quote.apparel", 0, d); err != nil {
		t.Fatal(err)
	}
	_, err = q.freezeQuote(q.author, "quote.apparel", 1, 1)
	q.want(err, cstapi.CodeBOMUnverified)
}

func bomDraft(id, profile string, measured bool, mode string, grades []cstapi.SizeGrade) cstapi.SaveCostBOM {
	return cstapi.SaveCostBOM{IdempotencyKey: "save." + id, BomID: id, Data: cstapi.CostBOMData{
		DocumentProfile: profile, BaseSizeCode: "M", SkuID: "sku.1", SpecRevision: 1,
		Lines: []cstapi.CostBOMLine{{LineID: "L1", MaterialKind: cstapi.MaterialShell, MaterialCode: "FAB.1", Unit: "M",
			GradingMode: mode, ConsumptionPerUnit: "1.45", WastageRatio: "0.03", ShrinkageFactor: "1.04",
			FabricWidthCM: "150", FabricGSM: "180", Measured: measured, FormulaBasis: cstapi.FormulaWidth, Grades: grades}},
	}}
}

func substitution(bom, id string, version int64) cstapi.RequestSubstitution {
	return cstapi.RequestSubstitution{IdempotencyKey: "req." + id, BomID: bom, ExpectedVersion: version, SubstitutionID: id,
		OriginalLineID: "L1", OriginalMaterialCode: "FAB.1", SubstituteMaterialCode: "FAB.2", ApplicableQuantity: "10",
		TargetSpecRevision: 2, CostDeltaMinor: "20000", Currency: "USD", MinorUnits: 2, LeadDaysDelta: 7, SampleImpact: true,
		Reason: "缺缸替代"}
}

func bomHash(t *testing.T, w *world) string {
	t.Helper()
	for _, e := range w.events("company.A", cstapi.Source) {
		if e.Type == cstapi.EventCostBOMVerified {
			hash, _ := e.Payload["content_sha256"].(string)
			if hash != "" {
				return hash
			}
		}
	}
	t.Fatal("verify wrote no content hash")
	return ""
}

func (w *world) costBOMGuard(company string, ref cstapi.CostBOMRef) cstapi.CostBOMVerdict {
	w.t.Helper()
	db, done := w.helperDB()
	defer done()
	var verdict cstapi.CostBOMVerdict
	err := bento.ScopeTransactor{DB: db, Scope: w.scope(company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		verdict, err = cstapi.ScanCostBOMGuard(ref, exec.QueryRow(ctx, cstapi.CostBOMGuardSQL, cstapi.CostBOMGuardArgs(ref)...).Scan)
		return err
	})
	w.must(err)
	return verdict
}
