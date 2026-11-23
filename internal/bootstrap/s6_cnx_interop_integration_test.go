//go:build integration

package bootstrap_test

import (
	"testing"
	"time"

	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/foundation/interop"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CN-AT06 and CN-AT13 (S6-D05). The connector is the in-process fake: nothing dials a Single
// Window or a tax bureau. A timeout is unknown. A lookup then records the professional result or,
// when the other side has nothing, not_received. A stale receipt stays on the chain and does not
// advance the declaration. The manual submitted → approved path is unchanged.
func TestCNXChannelUnknownAndStaleReceipts(t *testing.T) {
	w := newInvoiceWorld(t)
	doc := w.declare.doc
	past := time.Now().Add(-2 * time.Hour).UTC()
	clerk := "合成报关员（测试）"
	evidence := []string{w.declare.evidence}

	t.Run("unconfigured", func(t *testing.T) {
		w.dlot("lot.cn.none", "n1", "10", "PCS")
		c := w.declare.confirmed("lot.cn.none", items("n1"))
		w.cnx.Interop = nil
		_, err := w.cnx.SubmitDeclarationChannel(doc, cnxapi.SubmitDeclarationChannel{IdempotencyKey: w.key(), DeclarationID: c.DeclarationID,
			ExpectedVersion: c.RowVersion, SubmitterName: clerk, EvidenceIDs: evidence})
		if code(err) != "EXTERNAL_CHANNEL_NOT_CONFIGURED" || path(err) != "/channel" {
			t.Fatalf("%s %s", code(err), path(err))
		}
		if v := w.declaration(c.DeclarationID); v.Status != cnxapi.DeclarationConfirmed || len(v.Submissions) != 0 {
			t.Fatalf("confirmed declaration changed: %+v", v.Status)
		}
	})

	t.Run("ai", func(t *testing.T) {
		w.dlot("lot.cn.ai", "a1", "10", "PCS")
		c := w.declare.confirmed("lot.cn.ai", items("a1"))
		_, err := w.cnx.SubmitDeclarationChannel(w.declarationAgent("agent.cnx.ch"), cnxapi.SubmitDeclarationChannel{IdempotencyKey: w.key(),
			DeclarationID: c.DeclarationID, ExpectedVersion: c.RowVersion, SubmitterName: clerk, EvidenceIDs: evidence})
		w.want(err, kernel.CodePermissionDenied)
	})

	t.Run("CN-AT13", func(t *testing.T) {
		w.dlot("lot.cn13", "c1", "10", "PCS")
		c := w.declare.confirmed("lot.cn13", items("c1"))
		w.cnx.Interop = &interop.Fake{Now: func() time.Time { return past }, Script: []interop.Step{{
			Behaviour: interop.BehaviourTimeoutHeld, Outcome: cnxapi.DeclarationAccepted, Sequence: 2, StatusCode: "SYN-ACCEPTED",
			OriginalText: "合成海关回执：受理（测试数据）", MappingVersion: "syn-customs-status.v1", DeclarationNumber: "SYN-DECL-CN13"}}}
		ans, err := w.cnx.SubmitDeclarationChannel(doc, cnxapi.SubmitDeclarationChannel{IdempotencyKey: w.key(), DeclarationID: c.DeclarationID,
			ExpectedVersion: c.RowVersion, SubmitterName: clerk, EvidenceIDs: evidence})
		w.must(err)
		if ans.Status != cnxapi.DeclarationUnknown || ans.ConnectorState != string(interop.Unknown) || ans.OperationID == "" || ans.AttemptNo != 1 {
			t.Fatalf("timeout %+v", ans)
		}
		log, err := w.cnx.ChannelLog(doc, interop.ChannelCustomsDeclaration, c.DeclarationID)
		w.must(err)
		held := log.Attempt.RowVersion
		_, err = w.cnx.SubmitDeclarationChannel(doc, cnxapi.SubmitDeclarationChannel{IdempotencyKey: w.key(), DeclarationID: c.DeclarationID,
			ExpectedVersion: ans.RowVersion, SubmitterName: clerk, EvidenceIDs: evidence})
		if code(err) != cnxapi.CodeExternalResultUnknown {
			t.Fatalf("second submit %s %s", code(err), path(err))
		}
		log, err = w.cnx.ChannelLog(doc, interop.ChannelCustomsDeclaration, c.DeclarationID)
		w.must(err)
		if log.Attempt.State != string(interop.Unknown) || log.Attempt.RowVersion != held || log.Attempt.OperationID != ans.OperationID {
			t.Fatalf("attempt moved %+v", log.Attempt)
		}
		got, err := w.cnx.ReconcileDeclarationChannel(doc, cnxapi.ReconcileDeclarationChannel{IdempotencyKey: w.key(), DeclarationID: c.DeclarationID,
			ExpectedVersion: ans.RowVersion, EvidenceIDs: evidence})
		w.must(err)
		if got.Status != cnxapi.DeclarationAccepted || got.ConnectorState != string(interop.Succeeded) || got.Outcome != cnxapi.DeclarationAccepted ||
			got.OperationID != ans.OperationID {
			t.Fatalf("lookup %+v", got)
		}

		w.dlot("lot.cn13b", "b1", "10", "PCS")
		lost := w.declare.confirmed("lot.cn13b", items("b1"))
		w.cnx.Interop = &interop.Fake{Now: func() time.Time { return past }, Script: []interop.Step{{Behaviour: interop.BehaviourTimeoutLost}}}
		unknown, err := w.cnx.SubmitDeclarationChannel(doc, cnxapi.SubmitDeclarationChannel{IdempotencyKey: w.key(), DeclarationID: lost.DeclarationID,
			ExpectedVersion: lost.RowVersion, SubmitterName: clerk, EvidenceIDs: evidence})
		w.must(err)
		if unknown.Status != cnxapi.DeclarationUnknown || unknown.ConnectorState != string(interop.Unknown) {
			t.Fatalf("lost %+v", unknown)
		}
		miss, err := w.cnx.ReconcileDeclarationChannel(doc, cnxapi.ReconcileDeclarationChannel{IdempotencyKey: w.key(), DeclarationID: lost.DeclarationID,
			ExpectedVersion: unknown.RowVersion, EvidenceIDs: evidence, Reason: "海关查询无此申报（测试）"})
		w.must(err)
		if miss.Status != cnxapi.DeclarationNotReceived || miss.ConnectorState != string(interop.NotReceived) || miss.OperationID != unknown.OperationID {
			t.Fatalf("not received %+v", miss)
		}
		next := w.declare.confirmed("lot.cn13b", items("b1"))
		if next.DeclarationID == lost.DeclarationID || next.Attempt != 2 {
			t.Fatalf("new attempt %+v", next)
		}
		w.cnx.Interop = &interop.Fake{Now: func() time.Time { return past }, Script: []interop.Step{{
			Behaviour: interop.BehaviourAccept, Outcome: cnxapi.DeclarationAccepted, Sequence: 1, StatusCode: "SYN-ACCEPTED",
			OriginalText: "合成海关回执：受理（测试数据）", MappingVersion: "syn-customs-status.v1", DeclarationNumber: "SYN-DECL-CN13B"}}}
		again, err := w.cnx.SubmitDeclarationChannel(doc, cnxapi.SubmitDeclarationChannel{IdempotencyKey: w.key(), DeclarationID: next.DeclarationID,
			ExpectedVersion: next.RowVersion, SubmitterName: clerk, EvidenceIDs: evidence})
		w.must(err)
		if again.Status != cnxapi.DeclarationAccepted || again.OperationID == miss.OperationID || again.ConnectorState != string(interop.Succeeded) {
			t.Fatalf("retry %+v", again)
		}
		old, err := w.cnx.ChannelLog(doc, interop.ChannelCustomsDeclaration, lost.DeclarationID)
		w.must(err)
		if old.Attempt.State != string(interop.NotReceived) || old.Attempt.OperationID != miss.OperationID {
			t.Fatalf("old attempt overwritten %+v", old.Attempt)
		}
	})

	t.Run("CN-AT06", func(t *testing.T) {
		w.dlot("lot.cn06", "s1", "10", "PCS")
		sub := w.declare.submitted("lot.cn06", items("s1"))
		before := w.count("company.A", "brant_cnx.declaration_receipt")
		receivedAt := kernel.FormatTimestamp(time.Now().Add(-30 * time.Second))
		exportedAt := kernel.FormatTimestamp(time.Now().Add(-2 * time.Hour))
		in := func(outcome string, sequence int64, exported string) cnxapi.ChannelReceiptIn {
			return cnxapi.ChannelReceiptIn{Outcome: outcome, Sequence: sequence, StatusCode: "SYN-" + outcome, OriginalText: "合成海关回执：" + outcome + "（测试数据）",
				MappingVersion: "syn-customs-status.v1", DeclarationNumber: "SYN-DECL-CN06", ReceivedAt: receivedAt, ExportedAt: exported}
		}
		release := in(cnxapi.DeclarationReleased, 3, exportedAt)
		ing, err := w.cnx.IngestDeclarationReceipts(doc, cnxapi.IngestDeclarationReceipts{IdempotencyKey: w.key(), DeclarationID: sub.DeclarationID,
			ExpectedVersion: sub.RowVersion, Receipts: []cnxapi.ChannelReceiptIn{release, in(cnxapi.DeclarationAccepted, 2, ""), in(cnxapi.DeclarationReceived, 1, ""), release}})
		w.must(err)
		if ing.Status != cnxapi.DeclarationReleased || ing.LastSequence != 3 || len(ing.Chain) != 3 {
			t.Fatalf("burst %+v chain %d", ing.Status, len(ing.Chain))
		}
		if !ing.Chain[0].Authoritative || ing.Chain[0].Sequence != 3 || ing.Chain[0].RefusalCode != "" || ing.Chain[0].IngressNo != 1 {
			t.Fatalf("release %+v", ing.Chain[0])
		}
		for _, row := range ing.Chain[1:] {
			if row.Authoritative || row.RefusalCode != cnxapi.CodeDeclarationReceiptStale {
				t.Fatalf("stale %+v", row)
			}
		}
		if ing.Chain[1].Sequence != 2 || ing.Chain[2].Sequence != 1 {
			t.Fatalf("order %+v %+v", ing.Chain[1], ing.Chain[2])
		}
		if got := w.count("company.A", "brant_cnx.declaration_receipt"); got != before+1 {
			t.Fatalf("declaration receipts %d, before %d", got, before)
		}
		v := w.declaration(sub.DeclarationID)
		if v.Status != cnxapi.DeclarationReleased || v.LastSequence != 3 || len(v.Receipts) != 1 {
			t.Fatalf("case %+v receipts %d", v.Status, len(v.Receipts))
		}
		_, err = w.cnx.RecordReceipt(doc, cnxapi.RecordReceipt{IdempotencyKey: w.key(), DeclarationID: v.DeclarationID, ExpectedVersion: v.RowVersion,
			ReceiptID: "rcpt." + w.key(), Outcome: cnxapi.DeclarationAccepted, External: &cnxapi.ExternalReceipt{Sequence: 2, StatusCode: "SYN-ACCEPTED",
				OriginalText: "合成海关回执：受理（测试数据）", ReceivedAt: receivedAt}, MappingVersion: "syn-customs-status.v1",
			DeclarationNumber: "SYN-DECL-CN06", EvidenceIDs: evidence})
		if code(err) != cnxapi.CodeDeclarationReceiptStale {
			t.Fatalf("manual stale %s %s", code(err), path(err))
		}
		if got := w.count("company.A", "brant_cnx.declaration_receipt"); got != before+1 {
			t.Fatalf("manual stale stored a receipt: %d", got)
		}
		if log, err := w.cnx.ChannelLog(doc, interop.ChannelCustomsDeclaration, sub.DeclarationID); err != nil || len(log.Chain) != 3 {
			t.Fatalf("chain %v %+v", err, log.Chain)
		}
	})

	t.Run("tax", func(t *testing.T) {
		inv := w.invoice(w.facts("SYN0605", "120"))
		cny := &cnxapi.CurrencyRef{ID: w.cny.ConfigID, Revision: w.cny.Revision}
		at := time.Now().UTC()
		plan := func(lot, line, qty, id string) cnxapi.TaxCaseReceipt {
			t.Helper()
			w.lot(lot, at, line, qty)
			c, err := w.allocate(w.tax, lot, alloc(id, line, inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, qty))
			w.must(err)
			c, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxPlanned, Currency: cny, Amount: "100.00", TaxPeriod: "2026-11",
				Review: &cnxapi.ProfessionalReview{Reviewer: "合成税务师（测试）", EvidenceIDs: []string{w.review}}})
			w.must(err)
			return c
		}
		manual := plan("lot.ch.m", "m1", "20", "chm1")
		manual, err := w.record(w.tax, manual, cnxapi.RecordTaxResult{Stage: cnxapi.TaxSubmitted, ExternalReference: "SB-MANUAL-1", EvidenceIDs: []string{w.receipt}})
		w.must(err)
		manual, err = w.record(w.tax, manual, cnxapi.RecordTaxResult{Stage: cnxapi.TaxApproved, Currency: cny, Amount: "20.00",
			ResultText: "合成核准（测试，手工）", EvidenceIDs: []string{w.receipt}})
		w.must(err)
		if manual.Status != cnxapi.TaxApproved {
			t.Fatalf("manual %+v", manual)
		}

		rejected := plan("lot.ch.r", "r1", "100", "chr1")
		w.cnx.Interop = &interop.Fake{Script: []interop.Step{{Behaviour: interop.BehaviourTimeoutHeld, Outcome: cnxapi.TaxRejected,
			OriginalText: "合成税务机关退回（测试数据）"}}}
		unk, err := w.cnx.SubmitTaxChannel(w.tax, cnxapi.SubmitTaxChannel{IdempotencyKey: w.key(), CaseID: rejected.CaseID,
			ExpectedVersion: rejected.RowVersion, EvidenceIDs: []string{w.receipt}})
		w.must(err)
		if unk.Status != cnxapi.TaxUnknown || unk.ConnectorState != string(interop.Unknown) {
			t.Fatalf("tax timeout %+v", unk)
		}
		got, err := w.cnx.ReconcileTaxChannel(w.tax, cnxapi.ReconcileTaxChannel{IdempotencyKey: w.key(), CaseID: rejected.CaseID,
			ExpectedVersion: unk.RowVersion, EvidenceIDs: []string{w.receipt}, Reason: "核对退回（测试）"})
		w.must(err)
		if got.Status != cnxapi.TaxRejected || got.ConnectorState != string(interop.Failed) || got.OperationID != unk.OperationID {
			t.Fatalf("tax rejected %+v", got)
		}

		lost := plan("lot.ch.l", "l1", "100", "chl1")
		w.cnx.Interop = &interop.Fake{Script: []interop.Step{{Behaviour: interop.BehaviourTimeoutLost}}}
		unk, err = w.cnx.SubmitTaxChannel(w.tax, cnxapi.SubmitTaxChannel{IdempotencyKey: w.key(), CaseID: lost.CaseID,
			ExpectedVersion: lost.RowVersion, EvidenceIDs: []string{w.receipt}})
		w.must(err)
		miss, err := w.cnx.ReconcileTaxChannel(w.tax, cnxapi.ReconcileTaxChannel{IdempotencyKey: w.key(), CaseID: lost.CaseID,
			ExpectedVersion: unk.RowVersion, EvidenceIDs: []string{w.receipt}, Reason: "税务机关无此申报（测试）"})
		w.must(err)
		if miss.Status != cnxapi.TaxNotReceived || miss.ConnectorState != string(interop.NotReceived) {
			t.Fatalf("tax not received %+v", miss)
		}
		again, err := w.allocate(w.tax, "lot.ch.l", alloc("chl2", "l1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "100"))
		w.must(err)
		if again.CaseID == lost.CaseID {
			t.Fatalf("retry reused %s", again.CaseID)
		}
		again, err = w.record(w.tax, again, cnxapi.RecordTaxResult{Stage: cnxapi.TaxPlanned, Currency: cny, Amount: "100.00", TaxPeriod: "2026-11",
			Review: &cnxapi.ProfessionalReview{Reviewer: "合成税务师（测试）", EvidenceIDs: []string{w.review}}})
		w.must(err)
		w.cnx.Interop = &interop.Fake{Script: []interop.Step{{Behaviour: interop.BehaviourAccept, Outcome: cnxapi.TaxApproved,
			OriginalText: "合成核准（测试数据）", AmountMinor: 10000, CurrencyCode: "CNY"}}}
		approved, err := w.cnx.SubmitTaxChannel(w.tax, cnxapi.SubmitTaxChannel{IdempotencyKey: w.key(), CaseID: again.CaseID,
			ExpectedVersion: again.RowVersion, EvidenceIDs: []string{w.receipt}, Currency: cny})
		w.must(err)
		if approved.Status != cnxapi.TaxApproved || approved.ConnectorState != string(interop.Succeeded) || approved.OperationID == miss.OperationID {
			t.Fatalf("tax retry %+v", approved)
		}
		old, err := w.cnx.ChannelLog(w.tax, interop.ChannelTaxRefund, lost.CaseID)
		w.must(err)
		if old.Attempt == nil || old.Attempt.State != string(interop.NotReceived) || old.Attempt.OperationID != miss.OperationID {
			t.Fatalf("old tax attempt %+v", old.Attempt)
		}
	})

	if w.count("company.B", "brant_cnx.channel_attempt") != 0 || w.count("company.B", "brant_cnx.channel_receipt") != 0 {
		t.Fatal("company B sees channel rows")
	}
}
