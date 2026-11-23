//go:build integration

package bootstrap_test

import (
	"net/http"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/identity/oidc"
	"go.idp.xyz/idp-brant/internal/foundation/interop"
	"go.idp.xyz/idp-brant/internal/kernel"
	"go.idp.xyz/idp-brant/internal/transport/httpapi"
)

// The five channel commands on the CNX command route (S6-05, S6-D05). Nil interop is
// EXTERNAL_CHANNEL_NOT_CONFIGURED and the case stays confirmed. A timeout is unknown and is not
// a success. A stale receipt is stored on the channel chain and does not advance the case; a
// later manual receipt of that sequence is still stale and stores nothing in declaration_receipt.
// An AI actor is refused before the body is read. The connector is the in-process fake.
func TestCNXChannelCommandsOverHTTP(t *testing.T) {
	w := newInvoiceWorld(t)
	w.cnx.Interop = nil
	base, is := channelListen(t, w)
	doc := e2eWho(t, base, w.tenant, is, "doc.c3a", "human", 0)
	tax := e2eWho(t, base, w.tenant, is, "tax.1", "human", 40)
	w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.channel.http"},
		Permissions: []string{cnxapi.CommandRecordSubmission, cnxapi.CommandRecordReceipt, cnxapi.CommandRecordTaxResult, "cnx.read", "evd.read"},
		DataScope:   "company"})
	ai := e2eWho(t, base, w.tenant, is, "agent.channel.http", "ai", 80)
	evidence := []any{w.declare.evidence}
	clerk := "合成报关员（测试）"
	past := time.Now().Add(-2 * time.Hour).UTC()

	t.Run("ai", func(t *testing.T) {
		w.dlot("lot.http.ai", "a1", "10", "PCS")
		c := w.declare.confirmed("lot.http.ai", items("a1"))
		for _, command := range []string{cnxapi.CommandSubmitDeclarationChannel, cnxapi.CommandReconcileDeclarationChannel,
			cnxapi.CommandIngestDeclarationReceipts, cnxapi.CommandSubmitTaxChannel, cnxapi.CommandReconcileTaxChannel} {
			status, out := ai.do(http.MethodPost, "/api/brant/v1/cnx/commands/"+command, `{`)
			if status != http.StatusForbidden || out["code"] != kernel.CodePermissionDenied || errPath(out) != "/" {
				t.Fatalf("%s: %d %v", command, status, out)
			}
		}
		if v := declarationAt(t, doc, c.DeclarationID); v["status"] != cnxapi.DeclarationConfirmed || len(listField(v, "submissions")) != 0 {
			t.Fatalf("AI moved the case: %v", v["status"])
		}
	})

	t.Run("unconfigured", func(t *testing.T) {
		w.dlot("lot.http.none", "n1", "10", "PCS")
		c := w.declare.confirmed("lot.http.none", items("n1"))
		w.cnx.Interop = nil
		postCode(t, doc, "cnx", cnxapi.Contract, cnxapi.CommandSubmitDeclarationChannel, map[string]any{
			"declaration_id": c.DeclarationID, "expected_version": c.RowVersion, "submitter_name": clerk, "evidence_ids": evidence,
		}, http.StatusUnprocessableEntity, "EXTERNAL_CHANNEL_NOT_CONFIGURED", "/channel")
		if v := declarationAt(t, doc, c.DeclarationID); v["status"] != cnxapi.DeclarationConfirmed || len(listField(v, "submissions")) != 0 {
			t.Fatalf("unconfigured changed the case: %v submissions %d", v["status"], len(listField(v, "submissions")))
		}
	})

	t.Run("timeout", func(t *testing.T) {
		w.dlot("lot.http.unk", "u1", "10", "PCS")
		c := w.declare.confirmed("lot.http.unk", items("u1"))
		fake := &interop.Fake{Now: func() time.Time { return past }, Script: []interop.Step{{
			Behaviour: interop.BehaviourTimeoutHeld, Outcome: cnxapi.DeclarationAccepted, Sequence: 2, StatusCode: "SYN-ACCEPTED",
			OriginalText: "合成海关回执：受理（测试数据）", MappingVersion: "syn-customs-status.v1", DeclarationNumber: "SYN-DECL-HTTP"}}}
		w.cnx.Interop = fake
		unknown := postOK(t, doc, "cnx", cnxapi.Contract, cnxapi.CommandSubmitDeclarationChannel, map[string]any{
			"declaration_id": c.DeclarationID, "expected_version": c.RowVersion, "submitter_name": clerk, "evidence_ids": evidence})
		if unknown["status"] != cnxapi.DeclarationUnknown || unknown["object_id"] != c.DeclarationID || fake.Submits != 1 {
			t.Fatalf("timeout %v submits %d", unknown, fake.Submits)
		}
		log, err := w.cnx.ChannelLog(w.declare.doc, interop.ChannelCustomsDeclaration, c.DeclarationID)
		w.must(err)
		if log.Attempt == nil || log.Attempt.State != string(interop.Unknown) || log.Attempt.OperationID == "" {
			t.Fatalf("attempt %+v", log.Attempt)
		}
		held := log.Attempt.RowVersion
		postCode(t, doc, "cnx", cnxapi.Contract, cnxapi.CommandSubmitDeclarationChannel, map[string]any{
			"declaration_id": c.DeclarationID, "expected_version": unknown["row_version"], "submitter_name": clerk, "evidence_ids": evidence,
		}, http.StatusConflict, cnxapi.CodeExternalResultUnknown, "/args/declaration_id")
		if fake.Submits != 1 {
			t.Fatalf("second submit filed again: %d", fake.Submits)
		}
		log, err = w.cnx.ChannelLog(w.declare.doc, interop.ChannelCustomsDeclaration, c.DeclarationID)
		w.must(err)
		if log.Attempt.State != string(interop.Unknown) || log.Attempt.RowVersion != held {
			t.Fatalf("attempt moved %+v", log.Attempt)
		}
		accepted := postOK(t, doc, "cnx", cnxapi.Contract, cnxapi.CommandReconcileDeclarationChannel, map[string]any{
			"declaration_id": c.DeclarationID, "expected_version": unknown["row_version"], "evidence_ids": evidence})
		if accepted["status"] != cnxapi.DeclarationAccepted {
			t.Fatalf("lookup %v", accepted)
		}
		log, err = w.cnx.ChannelLog(w.declare.doc, interop.ChannelCustomsDeclaration, c.DeclarationID)
		w.must(err)
		if log.Attempt.State != string(interop.Succeeded) || log.Attempt.Outcome != cnxapi.DeclarationAccepted {
			t.Fatalf("settled %+v", log.Attempt)
		}
	})

	t.Run("stale", func(t *testing.T) {
		w.dlot("lot.http.stale", "s1", "10", "PCS")
		sub := w.declare.submitted("lot.http.stale", items("s1"))
		receipts := w.count("company.A", "brant_cnx.declaration_receipt")
		receivedAt := kernel.FormatTimestamp(time.Now().Add(-time.Minute))
		exportedAt := kernel.FormatTimestamp(time.Now().Add(-2 * time.Hour))
		ing := postOK(t, doc, "cnx", cnxapi.Contract, cnxapi.CommandIngestDeclarationReceipts, map[string]any{
			"declaration_id": sub.DeclarationID, "expected_version": sub.RowVersion, "receipts": []any{
				map[string]any{"outcome": cnxapi.DeclarationReleased, "sequence": 2, "status_code": "SYN-released",
					"original_text": "合成海关回执：released（测试数据）", "mapping_version": "syn-customs-status.v1",
					"declaration_number": "SYN-DECL-HTTP-S", "received_at": receivedAt, "exported_at": exportedAt},
				map[string]any{"outcome": cnxapi.DeclarationAccepted, "sequence": 1, "status_code": "SYN-accepted",
					"original_text": "合成海关回执：accepted（测试数据）", "mapping_version": "syn-customs-status.v1",
					"declaration_number": "SYN-DECL-HTTP-S", "received_at": receivedAt},
			}})
		if ing["status"] != cnxapi.DeclarationReleased {
			t.Fatalf("ingest %v", ing)
		}
		v := declarationAt(t, doc, sub.DeclarationID)
		if v["status"] != cnxapi.DeclarationReleased || v["last_sequence"] != float64(2) || len(listField(v, "receipts")) != 1 {
			t.Fatalf("case %v receipts %d", v["status"], len(listField(v, "receipts")))
		}
		if got := w.count("company.A", "brant_cnx.declaration_receipt"); got != receipts+1 {
			t.Fatalf("declaration receipts %d, before %d", got, receipts)
		}
		log, err := w.cnx.ChannelLog(w.declare.doc, interop.ChannelCustomsDeclaration, sub.DeclarationID)
		w.must(err)
		if len(log.Chain) != 2 || !log.Chain[0].Authoritative || log.Chain[0].Sequence != 2 ||
			log.Chain[1].Authoritative || log.Chain[1].RefusalCode != cnxapi.CodeDeclarationReceiptStale || log.Chain[1].Sequence != 1 {
			t.Fatalf("chain %+v", log.Chain)
		}
		postCode(t, doc, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, map[string]any{
			"declaration_id": sub.DeclarationID, "expected_version": ing["row_version"], "receipt_id": "rcpt.http.stale",
			"outcome": cnxapi.DeclarationAccepted, "external": map[string]any{"sequence": 1, "status_code": "SYN-accepted",
				"original_text": "合成海关回执：accepted（测试数据）", "received_at": receivedAt},
			"mapping_version": "syn-customs-status.v1", "declaration_number": "SYN-DECL-HTTP-S", "evidence_ids": evidence,
		}, http.StatusConflict, cnxapi.CodeDeclarationReceiptStale, "/args/external/sequence")
		if got := w.count("company.A", "brant_cnx.declaration_receipt"); got != receipts+1 {
			t.Fatalf("manual stale stored a receipt: %d, want %d", got, receipts+1)
		}
		if v = declarationAt(t, doc, sub.DeclarationID); v["status"] != cnxapi.DeclarationReleased || v["last_sequence"] != float64(2) {
			t.Fatalf("manual stale moved the case: %v sequence %v", v["status"], v["last_sequence"])
		}
	})

	t.Run("tax", func(t *testing.T) {
		inv := w.invoice(w.facts("SYN06HTTP", "120"))
		at := time.Now().UTC()
		w.lot("lot.http.tax", at, "t1", "100")
		planned, err := w.allocate(w.tax, "lot.http.tax", alloc("http1", "t1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "100"))
		w.must(err)
		cny := &cnxapi.CurrencyRef{ID: w.cny.ConfigID, Revision: w.cny.Revision}
		planned, err = w.record(w.tax, planned, cnxapi.RecordTaxResult{Stage: cnxapi.TaxPlanned, Currency: cny, Amount: "100.00", TaxPeriod: "2026-11",
			Review: &cnxapi.ProfessionalReview{Reviewer: "合成税务师（测试）", EvidenceIDs: []string{w.review}}})
		w.must(err)
		postCode(t, doc, "cnx", cnxapi.Contract, cnxapi.CommandSubmitTaxChannel, map[string]any{
			"case_id": planned.CaseID, "expected_version": planned.RowVersion, "evidence_ids": []any{w.receipt},
		}, http.StatusForbidden, kernel.CodePermissionDenied, "")
		fake := &interop.Fake{Script: []interop.Step{{Behaviour: interop.BehaviourTimeoutHeld, Outcome: cnxapi.TaxRejected,
			OriginalText: "合成税务机关退回（测试数据）"}}}
		w.cnx.Interop = fake
		unknown := postOK(t, tax, "cnx", cnxapi.Contract, cnxapi.CommandSubmitTaxChannel, map[string]any{
			"case_id": planned.CaseID, "expected_version": planned.RowVersion, "evidence_ids": []any{w.receipt}})
		if unknown["status"] != cnxapi.TaxUnknown || fake.Submits != 1 {
			t.Fatalf("tax timeout %v submits %d", unknown, fake.Submits)
		}
		rejected := postOK(t, tax, "cnx", cnxapi.Contract, cnxapi.CommandReconcileTaxChannel, map[string]any{
			"case_id": planned.CaseID, "expected_version": unknown["row_version"], "evidence_ids": []any{w.receipt}, "reason": "核对退回（测试）"})
		if rejected["status"] != cnxapi.TaxRejected {
			t.Fatalf("tax reconcile %v", rejected)
		}
		log, err := w.cnx.ChannelLog(w.tax, interop.ChannelTaxRefund, planned.CaseID)
		w.must(err)
		if log.Attempt == nil || log.Attempt.State != string(interop.Failed) || log.Attempt.Outcome != cnxapi.TaxRejected {
			t.Fatalf("tax attempt %+v", log.Attempt)
		}
	})
}

func channelListen(t *testing.T, w *invoiceWorld) (string, *issuer) {
	t.Helper()
	is := newIssuer(t)
	verifier, err := oidc.New(oidc.Config{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	c := w.c
	handler, err := httpapi.New(httpapi.Config{EVD: c.EVD, PTY: c.PTY, CAT: c.CAT, INQ: c.INQ, POL: c.POL, REF: c.REF,
		SUP: c.SUP, CST: c.CST, TRD: c.TRD, PUR: c.PUR, STL: c.STL, CNX: w.cnx,
		Verifier: oidc.HeaderVerifier{Tokens: verifier}, Clock: bento.SystemClock{}})
	w.must(err)
	return newHTTP(t, handler), is
}

func declarationAt(t *testing.T, c *client, id string) map[string]any {
	t.Helper()
	status, view := c.do(http.MethodGet, "/api/brant/v1/cnx/declaration_cases/"+id, nil)
	if status != http.StatusOK || view["ok"] != true {
		t.Fatalf("declaration %s: %d %v", id, status, view)
	}
	return view
}

func listField(view map[string]any, name string) []any {
	list, _ := view[name].([]any)
	return list
}
