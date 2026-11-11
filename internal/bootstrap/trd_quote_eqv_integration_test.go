//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The freeze and readiness cases of Python V0.50 T03 A27–A29, A30 and T04 A45 that the S3-11
// equivalence review found without a Go test of their own, on the TRD world (trd_integration_test.go).
// Test names cite the Python test they port.

// qeqvReader reads TRD's business state in company.A over one pool for the whole test.
type qeqvReader struct {
	q  *trdWorld
	db *bentopg.DB
}

func qeqvReaderOf(q *trdWorld) qeqvReader {
	db, err := bentopg.NewDB(pgtest.AppPool(q.t))
	q.must(err)
	return qeqvReader{q: q, db: db}
}

// state is what business() reads — the row counts of TRD's tables, the roots' versions and control
// epochs, the revisions' statuses and TRD's Outbox events — in one transaction, the audit trail
// aside.
func (r qeqvReader) state() string {
	r.q.t.Helper()
	var parts []string
	scope := r.q.scope("company.A")
	r.q.must(bento.ScopeTransactor{DB: r.db, Scope: scope}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := r.db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		for _, table := range []string{"quote_root", "quote_revision", "quote_review", "quote_approval", "quote_margin_exception",
			"quote_document", "quote_commit_evidence", "quote_revision_closure", "quote_risk_hold", "quote_conversion_slot",
			"customer_response", "acceptance", "acceptance_effectivity", "action_result"} {
			var n int
			if err = exec.QueryRow(ctx, "SELECT count(*) FROM brant_trd."+table).Scan(&n); err != nil {
				return err
			}
			parts = append(parts, fmt.Sprintf("%s=%d", table, n))
		}
		for _, sql := range []string{
			`SELECT coalesce(string_agg(quote_id || ':' || root_version || ':' || control_epoch, ',' ORDER BY quote_id), '') FROM brant_trd.quote_root`,
			`SELECT coalesce(string_agg(quote_id || ':' || revision::text || status || ':' || coalesce(business_hash, ''), ','
				ORDER BY quote_id, revision), '') FROM brant_trd.quote_revision`,
		} {
			var v string
			if err = exec.QueryRow(ctx, sql).Scan(&v); err != nil {
				return err
			}
			parts = append(parts, v)
		}
		var events int
		if err = exec.QueryRow(ctx, "SELECT count(*) FROM bento.outbox WHERE source = $1 AND scope = $2", trdapi.Source,
			scope.String()).Scan(&events); err != nil {
			return err
		}
		parts = append(parts, fmt.Sprintf("events=%d", events))
		return nil
	}))
	return strings.Join(parts, " ")
}

// qeqvSalesCharge publishes a sales fee category "freight.sale" and its sales tax application in
// company.A and returns a separate charge of 10 USD of it.
func qeqvSalesCharge(q *trdWorld) (polapi.ConfigRef, trdapi.Charge) {
	q.t.Helper()
	fee := q.configure(q.author, q.reviewer, polapi.KindFeeCategory, "freight.sale", 0, q.win.from, time.Time{}, polapi.ConfigData{
		FeeCategory: &polapi.FeeCategory{Name: "销售运费", Category: "freight", Side: polapi.FeeSideSalesCharge, Inclusion: polapi.InclusionSeparate,
			AllocationBasis: polapi.AllocationQuantity, PolicyBasis: "Synthetic reviewed sales charge"}}, true)
	app := q.configure(q.author, q.reviewer, polapi.KindTaxApplication, "tax.freight.sale", 0, q.win.from, time.Time{}, polapi.ConfigData{
		TaxApplication: &polapi.TaxApplication{Name: "sales freight", Side: polapi.TaxSideSales, SubjectKind: polapi.SubjectFeeCategory,
			SubjectID: "freight.sale", TaxProfile: q.tax, Jurisdiction: "CN", BasisNote: "synthetic"}}, true)
	return fee, trdapi.Charge{ChargeID: "freight.sale", FeeCategoryRef: tref(fee), ObligationKey: "freight.sale.1", Amount: "10",
		Inclusion: trdapi.InclusionSeparate, TaxApplicationRef: tref(app), Evidence: q.proof}
}

// qeqvActivated saves d as quote id, freezes it, has the reviewer approve it for quote.activate and
// activates it, as the author.
func qeqvActivated(q *trdWorld, id string, d trdapi.QuoteData) trdapi.FormalizationReceipt {
	q.t.Helper()
	_, err := q.saveQuote(q.author, id, 0, d)
	q.must(err)
	f, err := q.freezeQuote(q.author, id, 1, 1)
	q.must(err)
	_, err = q.submit(q.author, id, f.RowVersion, 1, "review."+id)
	q.must(err)
	r, err := q.c.TRD.DecideReview(q.reviewer, q.approval(id, f.RowVersion+1, "review."+id, f.BusinessHash, "approval."+id))
	q.must(err)
	a, err := q.c.TRD.Activate(q.author, q.activateCmd(id, r.RowVersion, 1, "approval."+id))
	q.must(err)
	return a
}

// qeqvHistory is quote id's history as GetQuote shows it, without the read's own instant.
func qeqvHistory(q *trdWorld, id string) string {
	q.t.Helper()
	v := q.getQuote(id)
	b, err := json.Marshal(v)
	q.must(err)
	return string(b)
}

// Python test_t03_a28_sales_terms.py test_public_duplicate_charge_rejection_has_no_success_or_business_residue
// (PYT-0330): two charges under one charge ID, each of another fee category, are refused without
// any business residue. Go refuses them when the draft is saved, before any freeze: DUPLICATE_LINE_ID
// at the second charge's ID (Python refuses the freeze with CHARGE_SCOPE_UNRESOLVED); no root, no
// revision, no ledger entry and no event is written, and the quote is not found.
func TestDuplicateChargeIDIsRefusedWithoutResidue(t *testing.T) {
	q := newTrdWorld(t)
	r := qeqvReaderOf(q)
	_, charge := qeqvSalesCharge(q)
	other := q.configure(q.author, q.reviewer, polapi.KindFeeCategory, "insurance.sale", 0, q.win.from, time.Time{}, polapi.ConfigData{
		FeeCategory: &polapi.FeeCategory{Name: "销售保险", Category: "insurance", Side: polapi.FeeSideSalesCharge, Inclusion: polapi.InclusionSeparate,
			AllocationBasis: polapi.AllocationQuantity, PolicyBasis: "Synthetic reviewed sales charge"}}, true)
	second := charge
	second.FeeCategoryRef, second.ObligationKey = tref(other), "insurance.sale.1"
	d := q.data()
	d.Charges = []trdapi.Charge{charge, second}
	before := r.state()
	_, err := q.saveQuote(q.author, "quote.charges", 0, d)
	if code(err) != kernel.CodeDuplicateLineID || path(err) != "/args/data/charges/1/charge_id" {
		t.Fatalf("two charges under one ID: %s at %s", code(err), path(err))
	}
	if after := r.state(); after != before {
		t.Fatalf("the refusal left business state:\n%s\n%s", before, after)
	}
	if _, err = q.c.TRD.GetQuote(q.author, "quote.charges"); code(err) != kernel.CodeNotFound {
		t.Fatalf("the refused quote: %v", err)
	}
}

// Python test_t03_a28_sales_terms.py test_late_freeze_failure_rolls_back_sales_proof_events_and_success_ledger
// (PYT-0342), test_t03_a29_trade_parties.py test_late_failure_rolls_back_and_allows_retry (PYT-0363)
// and test_t03_a30_contact_profiles.py test_late_failure_rolls_back_and_retries (PYT-0378); TRD-INV-016:
// a freeze of a quote with a sales charge that fails at its last write (the Outbox) leaves TRD's
// business state as it was — no sealed revision, sales terms binding, trade-party snapshot, ledger
// entry or event — and the same key then freezes once, sealing the cost scope binding, the sales
// terms binding with the charge, the customer at its revision with its buyer address, the contact at
// its row version and the trading profile in the dependency manifest. (Python binds a contact
// profile; Brant pins the contact's row version in PTY's trading profile.)
func TestLateFreezeFailureRollsBackEveryBinding(t *testing.T) {
	q := newTrdWorld(t)
	r := qeqvReaderOf(q)
	_, charge := qeqvSalesCharge(q)
	d := q.data()
	d.Charges = []trdapi.Charge{charge}
	_, err := q.saveQuote(q.author, "quote.late", 0, d)
	q.must(err)
	cmd := trdapi.Freeze{IdempotencyKey: "freeze.late", QuoteID: "quote.late", ExpectedVersion: 1, WorkingRevision: 1}
	before := r.state()
	drop := q.failOutbox(trdapi.EventQuoteFrozen)
	_, err = q.c.TRD.Freeze(q.author, cmd)
	if _, isRejection := kernel.AsRejection(err); err == nil || isRejection || !strings.Contains(err.Error(), "test storage failure") {
		t.Fatalf("freeze with a failing Outbox: %v", err)
	}
	if after := r.state(); after != before {
		t.Fatalf("the failed freeze left business state:\n%s\n%s", before, after)
	}
	drop()
	f, err := q.c.TRD.Freeze(q.author, cmd)
	if err != nil || f.Replayed || f.BusinessRevision != 1 || f.Status != trdapi.StatusFrozen {
		t.Fatalf("retry %+v, %v", f, err)
	}
	var doc struct {
		Data struct {
			Contact struct {
				ContactID  string `json:"contact_id"`
				RowVersion int64  `json:"row_version"`
			} `json:"contact_ref"`
		} `json:"data"`
		Calculation struct {
			CostScope struct {
				Contract string `json:"contract"`
			} `json:"cost_scope_binding"`
			SalesTerms struct {
				Contract string `json:"contract"`
				Taxes    []struct {
					Role string `json:"role"`
				} `json:"tax_bindings"`
				Charges []struct {
					ObligationKey string `json:"obligation_key"`
					Amount        string `json:"amount"`
				} `json:"charge_bindings"`
			} `json:"sales_terms_binding"`
		} `json:"calculation"`
		Parties struct {
			Customer struct {
				PartyID       string `json:"party_id"`
				PartyRevision int64  `json:"party_revision"`
				Addresses     []struct {
					Role string `json:"role"`
				} `json:"addresses"`
			} `json:"customer"`
		} `json:"party_snapshots"`
		Manifest []struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"dependency_manifest"`
	}
	rev := q.getQuote("quote.late").Revisions[0]
	q.must(json.Unmarshal([]byte(rev.Commercial), &doc))
	c := doc.Calculation
	if rev.BusinessHash != f.BusinessHash || c.CostScope.Contract != "brant.quote-cost-scope/0.1.0" ||
		c.SalesTerms.Contract != "brant.quote-sales-terms/0.1.0" || len(c.SalesTerms.Taxes) != 2 || len(c.SalesTerms.Charges) != 1 ||
		c.SalesTerms.Charges[0].ObligationKey != "freight.sale.1" || doc.Parties.Customer.PartyID != "customer.1" ||
		doc.Parties.Customer.PartyRevision != 2 || len(doc.Parties.Customer.Addresses) != 1 || doc.Parties.Customer.Addresses[0].Role != "buyer" ||
		doc.Data.Contact.ContactID != "contact.1" || doc.Data.Contact.RowVersion != 2 ||
		!slices.ContainsFunc(doc.Manifest, func(m struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		}) bool {
			return m.Kind == "trading_profile" && m.ID == "customer.1"
		}) {
		t.Fatalf("the retried freeze sealed %s", rev.Commercial)
	}
}

// qeqvReasonCodes are the codes of readiness's reasons.
func qeqvReasonCodes(rs []trdapi.Reason) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Code)
	}
	return out
}

// Python test_t03_a28_sales_terms.py test_quote_ready_rechecks_sales_fee_and_leaves_history_unchanged
// (PYT-0338): an active quote with a separate sales charge is ready; once POL retires the charge's
// fee category, quote_ready reports it not ready (CONFIG_NOT_ACTIVE, the sales fee category as POL's
// ConfigGuard v1 judges it now) and the quote's history and TRD's business state stay as they were.
func TestQuoteReadyRechecksTheSalesFeeWithoutRewritingHistory(t *testing.T) {
	q := newTrdWorld(t)
	r := qeqvReaderOf(q)
	fee, charge := qeqvSalesCharge(q)
	d := q.data()
	d.Charges = []trdapi.Charge{charge}
	qeqvActivated(q, "quote.fee", d)
	if ready := q.readiness(q.author, "quote.fee", 0); !ready.Ready {
		t.Fatalf("before the retirement: %+v", ready)
	}
	history, before := qeqvHistory(q, "quote.fee"), r.state()
	_, err := q.c.POL.Retire(q.reviewer, polapi.Retire{IdempotencyKey: q.key(), Kind: fee.Kind, ConfigID: fee.ConfigID, ExpectedVersion: 2,
		Reason: "停用销售运费类别"})
	q.must(err)
	ready := q.readiness(q.author, "quote.fee", 0)
	if ready.Ready || !slices.Contains(qeqvReasonCodes(ready.Reasons), polapi.CodeConfigNotActive) {
		t.Fatalf("after the retirement: %+v", ready)
	}
	if qeqvHistory(q, "quote.fee") != history || r.state() != before {
		t.Fatal("quote_ready rewrote the quote's history")
	}
}

// Python test_t03_a29_trade_parties.py test_active_quote_ready_rechecks_profile_without_rewriting_history
// (PYT-0355): once the customer's trading profile the quote bound is superseded by a new published
// revision (PTY's lapse; Python retires the party profile), the active quote is not ready
// (REFERENCE_STALE from TradingProfileGuard v1, Python CONFIG_NOT_ACTIVE), and its history and TRD's
// business state stay as they were.
func TestQuoteReadyRechecksTheTradingProfileWithoutRewritingHistory(t *testing.T) {
	q := newTrdWorld(t)
	r := qeqvReaderOf(q)
	q.activated("quote.1")
	if ready := q.readiness(q.author, "quote.1", 0); !ready.Ready {
		t.Fatalf("before the lapse: %+v", ready)
	}
	history, before := qeqvHistory(q, "quote.1"), r.state()
	saved, err := q.saveProfile(q.author, "customer.1", 2, 2, terms(q.payment), terms(q.trade), ptyapi.PinnedContact{ContactID: "contact.1", RowVersion: 2})
	q.must(err)
	_, err = q.publishProfile(q.reviewer, "customer.1", saved.RowVersion, 2)
	q.must(err)
	ready := q.readiness(q.author, "quote.1", 0)
	if ready.Ready || !slices.Contains(qeqvReasonCodes(ready.Reasons), trdapi.CodeReferenceStale) {
		t.Fatalf("after the lapse: %+v", ready)
	}
	if qeqvHistory(q, "quote.1") != history || r.state() != before {
		t.Fatal("quote_ready rewrote the quote's history")
	}
}

// Python test_t03_a27_cost_scope.py test_current_scope_failure_does_not_rewrite_committed_quote_history
// (PYT-0296): after the activation the scope the quote was costed on no longer holds — Python edits
// the RFQ line's quantity; an RFQ in Brant is never edited once ready, so INQ cancels it — and
// quote_ready reports the quote not ready while the active revision, its data, calculation and
// business hash stay as they were. The reason is the upstream guards' first refusal in context code
// order: CST's AdoptabilityGuard v1 no longer adopts a snapshot of a cancelled RFQ
// (COST_SNAPSHOT_INELIGIBLE; Python COMPARISON_BASIS_MISMATCH, which the rebinding rule gives in
// TestBoundComparesTheFrozenBindings).
func TestQuoteReadyAfterTheRfqEndsKeepsTheHistory(t *testing.T) {
	q := newTrdWorld(t)
	r := qeqvReaderOf(q)
	a := q.activated("quote.1")
	history, before := qeqvHistory(q, "quote.1"), r.state()
	_, err := q.c.INQ.CancelRfq(q.author, inqapi.CancelRfq{IdempotencyKey: q.key(), RfqID: "rfq.1", ExpectedVersion: q.rfqVersion,
		Reason: "客户取消询价"})
	q.must(err)
	ready := q.readiness(q.author, "quote.1", 0)
	if ready.Ready || !slices.Contains(reasonsOf(ready.Reasons), "upstream:"+cstapi.CodeCostSnapshotIneligible) {
		t.Fatalf("after the RFQ ended: %+v", ready)
	}
	v := q.getQuote("quote.1")
	if v.ActiveRevision != 1 || v.Revisions[0].Status != trdapi.StatusActive || v.Revisions[0].BusinessHash != a.BusinessHash ||
		qeqvHistory(q, "quote.1") != history || r.state() != before {
		t.Fatalf("quote_ready rewrote the quote's history: %+v", v.Revisions[0])
	}
}

// Python test_t04_a45_response_query_recovery.py test_closed_request_and_response_schemas (PYT-0534),
// the customer response read: the read takes the response's ID and nothing else (the api is typed,
// the HTTP route a path parameter, so no member can be injected), the answer is a typed view; an ID
// that is blank, too long or not an identifier finds nothing (NOT_FOUND; Python SCHEMA_INVALID) and
// discloses nothing, for the response and for its acceptance, while the recorded IDs read. (An
// empty acceptance ID, which the HTTP route's path segment never yields, is left out: the api answers
// it with a storage error, not a refusal.)
func TestCustomerResponseReadsRefuseMalformedIDs(t *testing.T) {
	q := newTrdWorld(t)
	q.mail()
	_, v := q.accepted("quote.1")
	if r, err := q.c.TRD.GetCustomerResponse(q.author, "response.quote.1"); err != nil || r.ResponseID != "response.quote.1" {
		t.Fatalf("the response %+v, %v", r, err)
	}
	if a, err := q.c.TRD.GetAcceptance(q.author, v.ObjectID); err != nil || a.AcceptanceID != v.ObjectID {
		t.Fatalf("the acceptance %+v, %v", a, err)
	}
	for _, id := range []string{"", " ", strings.Repeat("x", 65), "回复 A45", "response.quote.1 "} {
		if _, err := q.c.TRD.GetCustomerResponse(q.author, id); code(err) != kernel.CodeNotFound {
			t.Errorf("response %q: %v", id, err)
		}
		if id == "" {
			continue
		}
		if _, err := q.c.TRD.GetAcceptance(q.author, id); code(err) != kernel.CodeNotFound {
			t.Errorf("acceptance %q: %v", id, err)
		}
	}
}
