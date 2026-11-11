//go:build integration

package bootstrap_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
)

// The activation, replacement and replay of a quote whose inputs lapse after its approval (WBS
// S3-04, S3-05; ADR-BR-004 option B; TRD-INV-009, 016), on the TRD world of the formalization suite.
// Python checks each consumed configuration and master record again when it formalizes; TRD does so
// with the owners' guards inside the activation's and the replacement's transaction, in context
// code order, and refuses with the owner's verdict at the bound input's path, before POL issues a
// number. A committed activation replays its stored receipt whatever lapsed since. Test and case
// names cite the Python V0.50 tests they port.

// feqvApprovedWith saves data as quote id, freezes it and has the reviewer approve revision 1 for
// quote.activate (approval "approval.<id>"); it returns the approval decision.
func feqvApprovedWith(q *trdWorld, id string, data trdapi.QuoteData) trdapi.ReviewReceipt {
	q.t.Helper()
	_, err := q.saveQuote(q.author, id, 0, data)
	q.must(err)
	f, err := q.freezeQuote(q.author, id, 1, 1)
	q.must(err)
	_, err = q.submit(q.author, id, f.RowVersion, 1, "review."+id)
	q.must(err)
	d, err := q.c.TRD.DecideReview(q.reviewer, q.approval(id, f.RowVersion+1, "review."+id, f.BusinessHash, "approval."+id))
	q.must(err)
	return d
}

// feqvSalesCharge publishes a sales freight fee category with its own sales tax application and
// returns the fixture's data with a separate 10 USD charge of it (as Python's sales_charge).
func feqvSalesCharge(q *trdWorld) (trdapi.QuoteData, polapi.ConfigRef) {
	q.t.Helper()
	fee := q.configure(q.author, q.reviewer, polapi.KindFeeCategory, "freight.sale", 0, q.win.from, time.Time{}, polapi.ConfigData{
		FeeCategory: &polapi.FeeCategory{Name: "销售运费", Category: "freight", Side: polapi.FeeSideSalesCharge, Inclusion: polapi.InclusionSeparate,
			AllocationBasis: polapi.AllocationQuantity, PolicyBasis: "Synthetic reviewed sales charge"}}, true)
	app := q.configure(q.author, q.reviewer, polapi.KindTaxApplication, "tax.freight.sale", 0, q.win.from, time.Time{}, polapi.ConfigData{
		TaxApplication: &polapi.TaxApplication{Name: "sales freight", Side: polapi.TaxSideSales, SubjectKind: polapi.SubjectFeeCategory,
			SubjectID: "freight.sale", TaxProfile: q.tax, Jurisdiction: "CN", BasisNote: "synthetic"}}, true)
	d := q.data()
	d.Charges = []trdapi.Charge{{ChargeID: "freight.sale", FeeCategoryRef: tref(fee), ObligationKey: "freight.sale.1", Amount: "10",
		Inclusion: trdapi.InclusionSeparate, TaxApplicationRef: tref(app), Evidence: q.proof}}
	return d, fee
}

// feqvRetire retires the active revision of config ref (root version 2: saved, then published).
func feqvRetire(q *trdWorld, ref polapi.ConfigRef, reason string) {
	q.t.Helper()
	_, err := q.c.POL.Retire(q.reviewer, polapi.Retire{IdempotencyKey: q.key(), Kind: ref.Kind, ConfigID: ref.ConfigID, ExpectedVersion: 2,
		Reason: reason})
	q.must(err)
}

// feqvNextProfile publishes revision 2 of customer.1's trading profile (the same terms and contact):
// revision 1, which the quote binds, is superseded.
func feqvNextProfile(q *trdWorld) {
	q.t.Helper()
	saved, err := q.saveProfile(q.author, "customer.1", 2, 2, terms(q.payment), terms(q.trade), ptyapi.PinnedContact{ContactID: "contact.1",
		RowVersion: 2})
	q.must(err)
	_, err = q.publishProfile(q.reviewer, "customer.1", saved.RowVersion, 2)
	q.must(err)
}

// feqvReviseContact reopens contact.1 (row version 2, the one the trading profile and the quote
// pin) as a draft; with confirm, it is confirmed again in a later row version.
func feqvReviseContact(q *trdWorld, confirm bool) {
	q.t.Helper()
	q.must(q.reviseContact(q.author, "contact.1", 2, q.key()))
	if confirm {
		_, err := q.c.PTY.ConfirmContact(q.author, ptyapi.ConfirmContact{IdempotencyKey: q.key(), ContactID: "contact.1", ExpectedVersion: 3,
			EvidenceIDs: []string{"evidence"}})
		q.must(err)
	}
}

// feqvBindings checks, on revision rv activated with the formal document content, what the freeze
// bound and the activation recorded (PYT-0162, PYT-0350, PYT-0410 on
// TestActivateWritesNumberFileBindingEvidenceAndOutboxAtomically).
func feqvBindings(t *testing.T, q *trdWorld, id string, rv trdapi.RevisionView, content string) {
	t.Helper()
	var doc struct {
		Data struct {
			Lines []struct {
				TaxApplicationRef trdapi.ConfigRef `json:"tax_application_ref"`
			} `json:"lines"`
		} `json:"data"`
		PartySnapshots struct {
			Issuer struct {
				CompanyID      string `json:"company_id"`
				LegalName      string `json:"legal_name"`
				RegistrationNo string `json:"registration_no"`
				Address        string `json:"address"`
			} `json:"issuer"`
			Customer struct {
				PartyID       string `json:"party_id"`
				PartyRevision int64  `json:"party_revision"`
				Addresses     []struct {
					Role, Address string
				} `json:"addresses"`
			} `json:"customer"`
		} `json:"party_snapshots"`
		Manifest []struct {
			Kind        string `json:"kind"`
			ID          string `json:"id"`
			Version     int64  `json:"version"`
			ContentHash string `json:"content_hash"`
		} `json:"dependency_manifest"`
	}
	q.must(json.Unmarshal([]byte(rv.Commercial), &doc))
	pinned := func(kind, id string, version int64) bool {
		return slices.ContainsFunc(doc.Manifest, func(d struct {
			Kind        string `json:"kind"`
			ID          string `json:"id"`
			Version     int64  `json:"version"`
			ContentHash string `json:"content_hash"`
		}) bool {
			return d.Kind == kind && d.ID == id && d.Version == version && len(d.ContentHash) == 64
		})
	}
	// PYT-0162: the line's published sales tax application is consumed and pinned.
	if len(doc.Data.Lines) != 1 || doc.Data.Lines[0].TaxApplicationRef != tref(q.sale) ||
		!pinned(polapi.KindTaxApplication, q.sale.ConfigID, q.sale.Revision) {
		t.Errorf("the sales tax application is not bound: %+v, manifest %+v", doc.Data.Lines, doc.Manifest)
	}
	// PYT-0350: the server's party binding is sealed and printed.
	issuer, customer := doc.PartySnapshots.Issuer, doc.PartySnapshots.Customer
	if issuer.CompanyID != "company.A" || issuer.LegalName == "" || issuer.RegistrationNo == "" || customer.PartyID != "customer.1" ||
		customer.PartyRevision != 2 || len(customer.Addresses) == 0 || !pinned("trading_profile", "customer.1", 1) ||
		!pinned(polapi.KindCompanyProfile, q.company.ConfigID, q.company.Revision) {
		t.Errorf("the party binding %+v %+v, manifest %+v", issuer, customer, doc.Manifest)
	}
	for _, s := range []string{issuer.LegalName, issuer.Address, "Customer: customer.1", customer.Addresses[0].Address, "Contact: contact.1"} {
		if !strings.Contains(content, s) {
			t.Errorf("the formal document does not print %q", s)
		}
	}
	// PYT-0410: the commit evidence records the bound snapshot and CST's verdict on it now.
	c := rv.Formal.Commit
	var entries string
	q.must(q.execScan("company.A", `SELECT entries FROM brant_trd.quote_commit_evidence WHERE quote_id = '`+id+`'`, &entries))
	if c.CostSnapshotID != q.snap.SnapshotID || !strings.Contains(entries, `"cost_snapshot_adoptability","value":"adoptable"`) ||
		!strings.Contains(entries, q.snap.SnapshotID) {
		t.Errorf("the commit evidence does not record the snapshot's qualification: %+v %s", c, entries)
	}
}

// What lapses between the approval and the activation refuses the activation with the owner's
// verdict at the bound input, leaving nothing (no document, commit evidence, number or event) and
// the revision pending approval:
//   - test_t03_a28_sales_terms.py test_retired_sales_fee_blocks_formalization_without_any_business_residue
//     (PYT-0332): the sales charge's fee category retired;
//   - test_t03_a29_trade_parties.py test_retired_issuer_blocks_activation (PYT-0357): the issuer's
//     company profile retired;
//   - test_t03_a30_contact_profiles.py test_retired_contact_profile_blocks_formalization (PYT-0367)
//     and test_new_contact_profile_revision_blocks_old_reference (PYT-0369): Brant has no contact
//     profile; the trading profile pins the contact's row version, and the contact reopened as a
//     draft (the retirement's analogue) or confirmed again in a later row version no longer serves
//     the quote (PTY's TradingProfileGuard: contact_changed, REFERENCE_STALE where Python says
//     CONFIG_NOT_ACTIVE);
//   - test_t03_a27_cost_scope.py test_freeze_to_activation_does_not_trust_stale_scope_receipt
//     (PYT-0295): Python edits the RFQ's quantity after the freeze; a ready RFQ cannot change its
//     lines in Go, so the RFQ closed after the freeze is the change, and the activation does not
//     trust the freeze's binding: the RFQ's state is judged again (CST's AdoptabilityGuard, which
//     asks INQ's RfqGuard for the snapshot's RFQ, is the first guard to refuse).
func TestLapsedInputsBlockActivation(t *testing.T) {
	for _, c := range []struct {
		name       string
		data       func(q *trdWorld) (trdapi.QuoteData, func())
		code, path string
	}{
		{"PYT-0332 fee category retired", func(q *trdWorld) (trdapi.QuoteData, func()) {
			d, fee := feqvSalesCharge(q)
			return d, func() { feqvRetire(q, fee, "停用销售运费类别") }
		}, polapi.CodeConfigNotActive, "/data/charges/0/fee_category_ref"},
		{"PYT-0357 issuer company profile retired", func(q *trdWorld) (trdapi.QuoteData, func()) {
			return q.data(), func() { feqvRetire(q, q.company, "停用公司档案") }
		}, polapi.CodeConfigNotActive, "/data/issuer_ref"},
		{"PYT-0367 contact reopened", func(q *trdWorld) (trdapi.QuoteData, func()) {
			return q.data(), func() { feqvReviseContact(q, false) }
		}, ptyapi.CodeReferenceStale, "/data/trading_profile_ref"},
		{"PYT-0369 contact confirmed in a later row version", func(q *trdWorld) (trdapi.QuoteData, func()) {
			return q.data(), func() { feqvReviseContact(q, true) }
		}, ptyapi.CodeReferenceStale, "/data/trading_profile_ref"},
		{"PYT-0295 RFQ closed after the freeze", func(q *trdWorld) (trdapi.QuoteData, func()) {
			return q.data(), func() {
				_, err := q.c.INQ.CloseRfq(q.author, inqapi.CloseRfq{IdempotencyKey: q.key(), RfqID: "rfq.1", ExpectedVersion: q.rfqVersion,
					Reason: "客户不再推进"})
				q.must(err)
			}
		}, "COST_SNAPSHOT_INELIGIBLE", "/data/cost_snapshot_ref"},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := newTrdWorld(t)
			data, lapse := c.data(q)
			d := feqvApprovedWith(q, "quote.1", data)
			lapse()
			_, err := q.c.TRD.Activate(q.author, q.activateCmd("quote.1", d.RowVersion, 1, "approval.quote.1"))
			if code(err) != c.code || path(err) != c.path {
				t.Fatalf("activation: %s at %s, want %s at %s", code(err), path(err), c.code, c.path)
			}
			if documents, commits, numbers := q.formalRows(); documents != 0 || commits != 0 || numbers != 0 ||
				len(q.eventsOf(trdapi.EventQuoteActivated)) != 0 {
				t.Fatalf("%d documents, %d commit evidence, %d numbers", documents, commits, numbers)
			}
			if s := q.statuses("quote.1"); !slices.Equal(s, []string{trdapi.StatusPendingApproval}) {
				t.Fatalf("statuses %v", s)
			}
		})
	}
}

// A replacement checks its own inputs again: once one lapsed after its approval the replacement is
// refused with the owner's verdict, nothing is written, and the active revision stays active and
// keeps serving (its number and document unchanged):
//   - test_t03_a28_sales_terms.py test_replacement_rechecks_current_sales_tax_and_preserves_old_active
//     (PYT-0337): the sales tax application retired (CONFIG_NOT_ACTIVE where Python says
//     REFERENCE_STALE);
//   - test_t03_a29_trade_parties.py test_replacement_rechecks_profile (PYT-0358): the trading profile
//     superseded by a new published revision (PTY: REFERENCE_STALE, Python retires the profile:
//     CONFIG_NOT_ACTIVE);
//   - test_t03_a30_contact_profiles.py test_replacement_rechecks_contact_profile (PYT-0372): the
//     pinned contact reopened (no contact profile in Brant; TradingProfileGuard: contact_changed).
func TestLapsedInputsBlockReplacementAndKeepTheActiveRevision(t *testing.T) {
	for _, c := range []struct {
		name       string
		lapse      func(q *trdWorld)
		code, path string
	}{
		{"PYT-0337 sales tax application retired", func(q *trdWorld) { feqvRetire(q, q.sale, "停用税适用") }, polapi.CodeConfigNotActive,
			"/data/lines/0/tax_application_ref"},
		{"PYT-0358 trading profile superseded", feqvNextProfile, ptyapi.CodeReferenceStale, "/data/trading_profile_ref"},
		{"PYT-0372 contact reopened", func(q *trdWorld) { feqvReviseContact(q, false) }, ptyapi.CodeReferenceStale, "/data/trading_profile_ref"},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := newTrdWorld(t)
			a := q.activated("quote.1")
			n, _, version := q.revisedAndApproved("quote.1", a.RowVersion, trdapi.TargetReplace, "review.2", "approval.2")
			c.lapse(q)
			_, err := q.c.TRD.Replace(q.author, q.replaceCmd("quote.1", version, n, "approval.2"))
			if code(err) != c.code || path(err) != c.path {
				t.Fatalf("replacement: %s at %s, want %s at %s", code(err), path(err), c.code, c.path)
			}
			if s := q.statuses("quote.1"); !slices.Equal(s, []string{trdapi.StatusActive, trdapi.StatusPendingApproval}) {
				t.Fatalf("statuses %v", s)
			}
			if documents, commits, numbers := q.formalRows(); documents != 1 || commits != 1 || numbers != 1 ||
				len(q.eventsOf(trdapi.EventQuoteReplaced)) != 0 {
				t.Fatalf("%d documents, %d commit evidence, %d numbers", documents, commits, numbers)
			}
			v := q.getQuote("quote.1")
			if v.ActiveRevision != 1 || v.Revisions[0].Formal == nil || v.Revisions[0].Formal.Number != a.Number ||
				v.Revisions[0].Formal.DocumentSHA256 != a.DocumentSHA256 {
				t.Fatalf("the active revision %+v", v.Revisions[0])
			}
		})
	}
}

// A committed activation replays its stored receipt (same action, number and document) after its
// inputs lapsed, and neither the replay nor the lapse rewrites the quote's history:
//   - test_t03_a28_sales_terms.py test_committed_quote_replay_keeps_original_sales_terms_after_retirement
//     (PYT-0346): the sales tax application retired;
//   - test_t03_a29_trade_parties.py test_committed_replay_survives_retirement (PYT-0361): the
//     trading profile superseded;
//   - test_t03_a30_contact_profiles.py test_ready_and_committed_replay_preserve_history_after_retirement
//     (PYT-0374): the pinned contact reopened; quote_ready, which judges the active revision's inputs
//     now, is not ready (TradingProfileGuard's REFERENCE_STALE where Python says CONFIG_NOT_ACTIVE)
//     and changes nothing.
func TestCommittedActivationReplaysAfterItsInputsLapse(t *testing.T) {
	for _, c := range []struct {
		name   string
		lapse  func(q *trdWorld)
		reason string // a readiness reason after the lapse, as check:code; "" not checked
	}{
		{"PYT-0346 sales tax application retired", func(q *trdWorld) { feqvRetire(q, q.sale, "停用税适用") }, ""},
		{"PYT-0361 trading profile superseded", feqvNextProfile, ""},
		{"PYT-0374 contact reopened", func(q *trdWorld) { feqvReviseContact(q, false) }, "upstream:" + ptyapi.CodeReferenceStale},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := newTrdWorld(t)
			_, d := q.approved("quote.1")
			cmd := q.activateCmd("quote.1", d.RowVersion, 1, "approval.quote.1")
			a, err := q.c.TRD.Activate(q.author, cmd)
			q.must(err)
			history := q.getQuote("quote.1")
			c.lapse(q)
			before := q.business()
			if c.reason != "" {
				r := q.readiness(q.author, "quote.1", 0)
				if r.Ready || !slices.Contains(reasonsOf(r.Reasons), c.reason) || r.BusinessRevision != 1 {
					t.Fatalf("readiness after the lapse %+v", r)
				}
			}
			again, err := q.c.TRD.Activate(q.author, cmd)
			if err != nil || !again.Replayed || again.ActionID != a.ActionID || again.Number != a.Number || again.DocumentSHA256 != a.DocumentSHA256 ||
				again.BusinessHash != a.BusinessHash {
				t.Fatalf("replay %+v, %v", again, err)
			}
			if q.business() != before {
				t.Fatal("the replay or the readiness wrote TRD's business state")
			}
			after := q.getQuote("quote.1")
			if after.RootVersion != history.RootVersion || after.ActiveRevision != 1 || len(after.Revisions) != 1 ||
				after.Revisions[0].BusinessHash != history.Revisions[0].BusinessHash || after.Revisions[0].Commercial != history.Revisions[0].Commercial ||
				after.Revisions[0].Status != trdapi.StatusActive || feqvJSON(t, after.Revisions[0].Formal) != feqvJSON(t, history.Revisions[0].Formal) {
				t.Fatalf("the history changed:\n%+v\n%+v", history.Revisions[0], after.Revisions[0])
			}
			if _, _, numbers := q.formalRows(); numbers != 1 || len(q.eventsOf(trdapi.EventQuoteActivated)) != 1 {
				t.Fatal("the replay issued something")
			}
		})
	}
}

// feqvJSON is v as JSON, for comparing views.
func feqvJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
