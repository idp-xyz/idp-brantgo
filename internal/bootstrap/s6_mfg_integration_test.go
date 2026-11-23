//go:build integration

package bootstrap_test

import (
	"net/http/httptest"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	mfgapi "go.idp.xyz/idp-brant/internal/contexts/mfg/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	stkapi "go.idp.xyz/idp-brant/internal/contexts/stk/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/identity/oidc"
	"go.idp.xyz/idp-brant/internal/kernel"
	"go.idp.xyz/idp-brant/internal/transport/httpapi"
)

// S6-02 on PostgreSQL 16: manufacturing defaults to off (MFG-INV-001), a collaboration plan and a
// subcontract move on their own roots, a progress claim does not invent stock or a quality release,
// and a material handover is one of four stock kinds (MFG-INV-002, R42).

func TestS6ManufacturingSubcontractAndHandover(t *testing.T) {
	w := newWorld(t)
	admin := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "mfg.admin"},
		Permissions: []string{mfgapi.CommandConfigure, mfgapi.CommandPublish, mfgapi.CommandIssue, mfgapi.CommandProgress,
			mfgapi.PermissionRead, mfgapi.PermissionAudit, stkapi.CommandConfigure, stkapi.CommandReceive, stkapi.CommandIssue,
			stkapi.PermissionRead, qltapi.CommandRecordInspection, qltapi.PermissionRead, "evd.read"},
		DataScope: "company"})
	mfg := w.c.MFG
	if mfg == nil {
		t.Fatal("manufacturing is not composed")
	}

	_, err := mfg.PublishPlan(admin, mfgapi.PublishPlan{IdempotencyKey: w.key(), PlanID: "plan.off", ExpectedVersion: 0,
		Target: mfgapi.StatusDraft, FactoryPartyID: "factory.1"})
	w.want(err, mfgapi.CodeDependencyNotSupported)
	_, err = mfg.IssueSubcontract(admin, mfgIssue("sc.off", "plan.off", "op.1", "1", "c.missing"))
	w.want(err, mfgapi.CodeDependencyNotSupported)
	_, err = mfg.RecordProgress(admin, mfgProgress("sc.off", "pg.off", 1, mfgapi.StatusInProgress, "1"))
	w.want(err, mfgapi.CodeDependencyNotSupported)
	_, err = mfg.RecordMaterialHandover(admin, mfgHandover("sc.off", "mv.off", "mv.off.in", mfgapi.HandoverTransfer, "loc.1", "loc.factory", "1"))
	w.want(err, mfgapi.CodeDependencyNotSupported)

	is := newIssuer(t)
	verifier, err := oidc.New(oidc.Config{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	c := w.c
	handler, err := httpapi.New(httpapi.Config{EVD: c.EVD, PTY: c.PTY, CAT: c.CAT, INQ: c.INQ, POL: c.POL, REF: c.REF, SUP: c.SUP,
		CST: c.CST, TRD: c.TRD, PUR: c.PUR, QLT: c.QLT, STL: c.STL, CNX: c.CNX, FUL: c.FUL, TSK: c.TSK, AI: c.AI, STK: c.STK, MFG: c.MFG,
		Verifier: oidc.HeaderVerifier{Tokens: verifier}, Clock: bento.SystemClock{}})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	httpAdmin := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "mfg.admin", "human")}
	agent := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "mfg.ai", "ai")}
	if status, out := agent.try("mfg", mfgapi.Contract, mfgapi.CommandConfigure, map[string]any{"enabled": true, "expected_version": 0}); status != 403 || out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI configure %d %v", status, out)
	}
	if status, out := agent.try("mfg", mfgapi.Contract, mfgapi.CommandPublish, map[string]any{"plan_id": "plan.ai", "expected_version": 0,
		"target": mfgapi.StatusDraft, "factory_party_id": "factory.1"}); status != 403 || out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI publish %d %v", status, out)
	}
	enabled := httpAdmin.command("mfg", mfgapi.Contract, mfgapi.CommandConfigure, map[string]any{"enabled": true, "expected_version": 0})
	if enabled["status"] != mfgapi.StatusEnabled || enabled["row_version"] != float64(1) {
		t.Fatalf("enable %v", enabled)
	}
	setting, err := mfg.GetSetting(admin)
	w.must(err)
	if !setting.Enabled || setting.RowVersion != 1 {
		t.Fatalf("setting %+v", setting)
	}

	ops := []mfgapi.OperationInput{{OperationID: "op.1", Name: "sew", Sequence: 1, SkuID: "sku.1", SpecRevision: 1, Quantity: "100", UnitID: "PCS", UnitRevision: 1}}
	mats := []mfgapi.MaterialInput{{MaterialID: "mat.1", Kind: mfgapi.MaterialOurSupply, SkuID: "sku.cloth", Quantity: "100", UnitID: "PCS", UnitRevision: 1}}
	pub, err := mfg.PublishPlan(admin, mfgapi.PublishPlan{IdempotencyKey: w.key(), PlanID: "plan.pub", ExpectedVersion: 0, Target: mfgapi.StatusPublished,
		FactoryPartyID: "factory.1", Operations: ops, Materials: mats})
	w.must(err)
	if pub.Status != mfgapi.StatusPublished || pub.RowVersion != 1 || pub.ObjectID != "plan.pub" || pub.Replayed {
		t.Fatalf("publish %+v", pub)
	}
	view, err := mfg.GetPlan(admin, "plan.pub")
	w.must(err)
	if view.Status != mfgapi.StatusPublished || len(view.Operations) != 1 || view.Operations[0].Quantity != "100" || len(view.Materials) != 1 {
		t.Fatalf("plan %+v", view)
	}

	draft, err := mfg.PublishPlan(admin, mfgapi.PublishPlan{IdempotencyKey: w.key(), PlanID: "plan.walk", ExpectedVersion: 0, Target: mfgapi.StatusDraft, FactoryPartyID: "factory.1"})
	w.must(err)
	if draft.Status != mfgapi.StatusDraft || draft.RowVersion != 1 {
		t.Fatalf("draft %+v", draft)
	}
	walk, err := mfg.PublishPlan(admin, mfgapi.PublishPlan{IdempotencyKey: w.key(), PlanID: "plan.walk", ExpectedVersion: 1, Target: mfgapi.StatusPublished,
		FactoryPartyID: "factory.1", Operations: ops, Materials: mats})
	w.must(err)
	if walk.Status != mfgapi.StatusPublished || walk.RowVersion != 2 {
		t.Fatalf("walk publish %+v", walk)
	}
	for _, step := range []struct {
		version int64
		target  string
	}{
		{2, mfgapi.StatusInProgress},
		{3, mfgapi.StatusPaused},
		{4, mfgapi.StatusInProgress},
		{5, mfgapi.StatusCompleted},
	} {
		walk, err = mfg.PublishPlan(admin, mfgapi.PublishPlan{IdempotencyKey: w.key(), PlanID: "plan.walk", ExpectedVersion: step.version, Target: step.target})
		w.must(err)
		if walk.Status != step.target || walk.RowVersion != step.version+1 {
			t.Fatalf("walk %s %+v", step.target, walk)
		}
	}
	_, err = mfg.PublishPlan(admin, mfgapi.PublishPlan{IdempotencyKey: w.key(), PlanID: "plan.walk", ExpectedVersion: 6, Target: mfgapi.StatusCancelled})
	w.want(err, kernel.CodeInvalidState)

	_, err = mfg.PublishPlan(admin, mfgapi.PublishPlan{IdempotencyKey: w.key(), PlanID: "plan.bad", ExpectedVersion: 0, Target: mfgapi.StatusPublished,
		FactoryPartyID: "factory.1", Operations: ops, Materials: mats})
	w.must(err)
	_, err = mfg.PublishPlan(admin, mfgapi.PublishPlan{IdempotencyKey: w.key(), PlanID: "plan.bad", ExpectedVersion: 1, Target: mfgapi.StatusCompleted})
	w.want(err, kernel.CodeInvalidState)
	bad, err := mfg.GetPlan(admin, "plan.bad")
	w.must(err)
	if bad.Status != mfgapi.StatusPublished || bad.RowVersion != 1 {
		t.Fatalf("illegal jump stored %+v", bad)
	}
	cancelled, err := mfg.PublishPlan(admin, mfgapi.PublishPlan{IdempotencyKey: w.key(), PlanID: "plan.cancel", ExpectedVersion: 0, Target: mfgapi.StatusCancelled, FactoryPartyID: "factory.1"})
	w.must(err)
	if cancelled.Status != mfgapi.StatusCancelled || cancelled.RowVersion != 1 {
		t.Fatalf("cancel %+v", cancelled)
	}

	key := w.key()
	issued, err := mfg.IssueSubcontract(admin, mfgIssueKey(key, "sc.1", "plan.pub", "op.1", "10", ""))
	w.must(err)
	if issued.Status != mfgapi.StatusIssued || issued.RowVersion != 1 || issued.ObjectID != "sc.1" {
		t.Fatalf("issue %+v", issued)
	}
	again, err := mfg.IssueSubcontract(admin, mfgIssueKey(key, "sc.1", "plan.pub", "op.1", "10", ""))
	w.must(err)
	if !again.Replayed || again.ActionID != issued.ActionID {
		t.Fatalf("issue replay %+v", again)
	}
	other := mfgIssueKey(w.key(), "sc.1", "plan.pub", "op.1", "9", "")
	_, err = mfg.IssueSubcontract(admin, other)
	w.want(err, mfgapi.CodeBusinessKeyConflict)
	_, err = mfg.IssueSubcontract(admin, mfgIssue("sc.over", "plan.pub", "op.1", "101", ""))
	w.want(err, kernel.CodeInvalidKnownValue)
	_, err = mfg.IssueSubcontract(admin, mfgIssue("sc.missing", "plan.pub", "op.1", "1", "c.missing"))
	w.want(err, kernel.CodeNotFound)
	if _, err = mfg.GetSubcontract(admin, "sc.missing"); code(err) != kernel.CodeNotFound {
		t.Fatalf("missing subcontract %s", code(err))
	}
	sc, err := mfg.GetSubcontract(admin, "sc.1")
	w.must(err)
	if sc.Status != mfgapi.StatusIssued || sc.Quantity != "10" || sc.CommitmentID != "" || sc.ProcessingFeeMinor != "0" || sc.PlanID != "plan.pub" {
		t.Fatalf("subcontract %+v", sc)
	}

	stockBefore := w.count("company.A", "brant_stk.stock_movement")
	releaseBefore := w.count("company.A", "brant_qlt.quality_release")
	planBefore, err := mfg.GetPlan(admin, "plan.pub")
	w.must(err)
	moved, err := mfg.RecordProgress(admin, mfgProgress("sc.1", "pg.1", 1, mfgapi.StatusInProgress, "4"))
	w.must(err)
	if moved.Status != mfgapi.StatusInProgress || moved.RowVersion != 2 {
		t.Fatalf("progress %+v", moved)
	}
	planAfter, err := mfg.GetPlan(admin, "plan.pub")
	w.must(err)
	if planAfter.RowVersion != planBefore.RowVersion || planAfter.Status != planBefore.Status {
		t.Fatalf("progress moved the plan from %+v to %+v", planBefore, planAfter)
	}
	if w.count("company.A", "brant_stk.stock_movement") != stockBefore || w.count("company.A", "brant_qlt.quality_release") != releaseBefore {
		t.Fatal("progress invented stock or a quality release")
	}
	done, err := mfg.RecordProgress(admin, mfgProgress("sc.1", "pg.2", 2, mfgapi.StatusCompleted, "6"))
	w.must(err)
	if done.Status != mfgapi.StatusCompleted || done.RowVersion != 3 {
		t.Fatalf("complete %+v", done)
	}
	_, err = mfg.RecordProgress(admin, mfgProgress("sc.1", "pg.3", 3, mfgapi.StatusCancelled, "1"))
	w.want(err, kernel.CodeInvalidState)
	_, err = mfg.IssueSubcontract(admin, mfgIssue("sc.cancel", "plan.pub", "op.1", "1", ""))
	w.must(err)
	stopped, err := mfg.RecordProgress(admin, mfgProgress("sc.cancel", "pg.cancel", 1, mfgapi.StatusCancelled, "1"))
	w.must(err)
	if stopped.Status != mfgapi.StatusCancelled {
		t.Fatalf("subcontract cancel %+v", stopped)
	}

	_, err = mfg.RecordMaterialHandover(admin, mfgHandover("sc.1", "mv.early", "mv.early.in", mfgapi.HandoverTransfer, "loc.1", "loc.factory", "1"))
	w.want(err, mfgapi.CodeDependencyNotSupported)
	if path(err) != "/args/kind" {
		t.Fatalf("stock off path %s", path(err))
	}
	if w.count("company.A", "brant_mfg.material_handover") != 0 {
		t.Fatal("a refused handover was stored")
	}

	w.must(enable(w, admin))
	_, err = w.c.STK.ReceiveStock(admin, receive("mv.recv", "sku.1", "100", "100", "1", "2026-01-01T00:00:00.000000Z"))
	w.must(err)
	for _, h := range []mfgapi.RecordMaterialHandover{
		mfgHandover("sc.1", "mv.xfer", "mv.xfer.in", mfgapi.HandoverTransfer, "loc.1", "loc.factory", "20"),
		mfgHandover("sc.1", "mv.consume", "", mfgapi.HandoverConsume, "loc.1", "", "5"),
		mfgHandover("sc.1", "mv.return", "mv.return.in", mfgapi.HandoverReturn, "loc.factory", "loc.1", "4"),
		mfgHandover("sc.1", "mv.scrap", "", mfgapi.HandoverScrap, "loc.factory", "", "1"),
	} {
		got, err := mfg.RecordMaterialHandover(admin, h)
		w.must(err)
		if got.ObjectID != h.MovementID || got.Status != h.Kind || got.Replayed {
			t.Fatalf("handover %s %+v", h.Kind, got)
		}
	}
	if w.count("company.A", "brant_mfg.material_handover") != 4 || w.count("company.A", "brant_stk.stock_movement") != 7 {
		t.Fatalf("handover rows mfg %d stk %d", w.count("company.A", "brant_mfg.material_handover"), w.count("company.A", "brant_stk.stock_movement"))
	}
	kinds := map[string]bool{}
	for _, key := range []stkapi.BalanceKey{
		{SkuID: "sku.1", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse},
		{SkuID: "sku.1", LocationID: "loc.factory", BatchID: "batch.1", CustodyKind: stkapi.CustodyExternalFactoryMaterial},
	} {
		moves, err := w.c.STK.ListMovements(admin, key)
		w.must(err)
		for _, m := range moves {
			kinds[m.Kind] = true
		}
	}
	for _, kind := range []string{stkapi.HandoverTransfer, stkapi.HandoverConsume, stkapi.HandoverReturn, stkapi.HandoverScrap} {
		if !kinds[kind] {
			t.Fatalf("missing kind %s in %v", kind, kinds)
		}
	}

	subject, err := mfg.ResolveInspectionSubject(admin, "sc.1", "op.1")
	w.must(err)
	if subject.Code != "" || subject.Quantity != "10" || subject.Hash == "" || subject.FactoryID != "factory.1" {
		t.Fatalf("subject %+v", subject)
	}
	_, err = w.c.QLT.RecordInspection(admin, qltapi.RecordInspection{IdempotencyKey: w.key(), InspectionID: "insp.mfg",
		Subject: qltapi.SubjectRef{Kind: qltapi.SubjectSubcontractOperation, ID: "sc.1", LineID: "op.1"}, InspectedQuantity: "1",
		ConformingQuantity: "1", NonconformingQuantity: "0", Sampling: qltapi.Sampling{Method: qltapi.SamplingFull},
		Standard: qltapi.StandardRef{SourceID: "std.missing"}, Inspector: qltapi.Inspector{Kind: qltapi.InspectorInternal},
		ReportFileID: "file.missing", InspectedAt: "2020-01-01T00:00:00.000000Z"})
	w.want(err, kernel.CodeReferenceInvalid)
	if path(err) != "/args/standard/source_id" {
		t.Fatalf("subject kind was not accepted: %s %s", code(err), path(err))
	}
	_, err = w.c.QLT.RecordInspection(admin, qltapi.RecordInspection{IdempotencyKey: w.key(), InspectionID: "insp.bad",
		Subject: qltapi.SubjectRef{Kind: "nope", ID: "sc.1", LineID: "op.1"}, InspectedQuantity: "1", ConformingQuantity: "1",
		NonconformingQuantity: "0", Sampling: qltapi.Sampling{Method: qltapi.SamplingFull}, Standard: qltapi.StandardRef{SourceID: "std.missing"},
		Inspector: qltapi.Inspector{Kind: qltapi.InspectorInternal}, ReportFileID: "file.missing", InspectedAt: "2020-01-01T00:00:00.000000Z"})
	w.want(err, kernel.CodeSchemaInvalid)
	if path(err) != "/args/subject/kind" {
		t.Fatalf("bad kind %s", path(err))
	}
	if w.count("company.A", "brant_qlt.inspection") != 0 {
		t.Fatal("a refused inspection was stored")
	}

	otherCompany := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "mfg.other"},
		Permissions: []string{mfgapi.PermissionRead}, DataScope: "company"})
	_, err = mfg.GetPlan(otherCompany, "plan.pub")
	w.want(err, kernel.CodeNotFound)
	if w.count("company.B", "brant_mfg.collaboration_plan") != 0 || w.count("company.A", "brant_mfg.collaboration_plan") == 0 {
		t.Fatal("company isolation failed")
	}

	var published, issuedEvent, progressed bool
	for _, ev := range w.events("company.A", mfgapi.Source) {
		switch ev.Type {
		case mfgapi.EventPlanPublished:
			published = true
		case mfgapi.EventSubcontractIssued:
			issuedEvent = true
		case mfgapi.EventProgressRecorded:
			progressed = true
		}
	}
	if !published || !issuedEvent || !progressed {
		t.Fatalf("events published %v issued %v progressed %v", published, issuedEvent, progressed)
	}
}

func mfgIssue(id, planID, operationID, qty, commitment string) mfgapi.IssueSubcontract {
	return mfgIssueKey("issue."+id, id, planID, operationID, qty, commitment)
}

func mfgIssueKey(key, id, planID, operationID, qty, commitment string) mfgapi.IssueSubcontract {
	return mfgapi.IssueSubcontract{IdempotencyKey: key, SubcontractID: id, PlanID: planID, OperationID: operationID, Quantity: qty,
		UnitID: "PCS", UnitRevision: 1, Currency: "USD", MinorUnits: 2, ProcessingFeeMinor: "0", CommitmentID: commitment,
		OrderID: "order.1", OrderLineID: "L1"}
}

func mfgProgress(subcontractID, progressID string, version int64, kind, qty string) mfgapi.RecordProgress {
	return mfgapi.RecordProgress{IdempotencyKey: "progress." + progressID, SubcontractID: subcontractID, ProgressID: progressID,
		ExpectedVersion: version, OperationID: "op.1", ProgressKind: kind, Quantity: qty, UnitID: "PCS", ReportedAt: "2026-01-02T00:00:00.000000Z"}
}

func mfgHandover(subcontractID, movementID, counter, kind, from, to, qty string) mfgapi.RecordMaterialHandover {
	custody := stkapi.CustodyOwnWarehouse
	if from == "loc.factory" {
		custody = stkapi.CustodyExternalFactoryMaterial
	}
	cmd := mfgapi.RecordMaterialHandover{IdempotencyKey: "handover." + movementID, SubcontractID: subcontractID, MovementID: movementID,
		CounterMovementID: counter, Kind: kind, SkuID: "sku.1", LocationID: from, BatchID: "batch.1", CustodyKind: custody,
		Quantity: qty, UnitID: "PCS", UnitRevision: 1, Reason: "material " + kind}
	if kind == mfgapi.HandoverTransfer || kind == mfgapi.HandoverReturn {
		cmd.ToLocationID, cmd.ToBatchID, cmd.ToCustodyKind = to, "batch.1", stkapi.CustodyExternalFactoryMaterial
		if kind == mfgapi.HandoverReturn {
			cmd.CustodyKind, cmd.ToCustodyKind = stkapi.CustodyExternalFactoryMaterial, stkapi.CustodyOwnWarehouse
		}
	}
	return cmd
}
