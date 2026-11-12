//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The formalization suite (WBS S3-04, S3-05; TRD-INV-001, 009, 010, 016; SC-03, SC-04 for the
// activation) on the TRD world: "editor" drafts, freezes, submits, activates, replaces, withdraws
// and cancels; "reviewer" approves; both humans with every permission and company data scope unless
// a test provisions them otherwise. POL issues the numbers from quote.seq (QT-<year>-NNNNN), counted
// in brant_pol.issued_number. Test names cite the Python V0.50 test they port.

func (q *trdWorld) activateCmd(id string, expected, revision int64, approval string) trdapi.Activate {
	return trdapi.Activate{IdempotencyKey: q.key(), QuoteID: id, ExpectedVersion: expected, BusinessRevision: revision, ApprovalID: approval,
		ExpectedApprovalEffectivityVersion: 1}
}

// approved freezes the fixture as quote id and has the reviewer approve revision 1 for
// quote.activate (approval "approval.<id>"); it returns the freeze and the decision.
func (q *trdWorld) approved(id string) (trdapi.FreezeReceipt, trdapi.ReviewReceipt) {
	q.t.Helper()
	f := q.frozenQuote(id)
	_, err := q.submit(q.author, id, f.RowVersion, 1, "review."+id)
	q.must(err)
	d, err := q.c.TRD.DecideReview(q.reviewer, q.approval(id, f.RowVersion+1, "review."+id, f.BusinessHash, "approval."+id))
	q.must(err)
	return f, d
}

// activated is approved and activated.
func (q *trdWorld) activated(id string) trdapi.FormalizationReceipt {
	q.t.Helper()
	_, d := q.approved(id)
	a, err := q.c.TRD.Activate(q.author, q.activateCmd(id, d.RowVersion, 1, "approval."+id))
	q.must(err)
	return a
}

// revisedAndApproved revises revision 1 of quote id at root version expected, freezes the new
// revision and has it approved as approval for target (replacing revision 1 for quote.replace); it
// returns the new revision, its business hash and the root version after the approval.
func (q *trdWorld) revisedAndApproved(id string, expected int64, target, review, approval string) (int64, string, int64) {
	q.t.Helper()
	r, err := q.c.TRD.Revise(q.author, trdapi.Revise{IdempotencyKey: q.key(), QuoteID: id, ExpectedVersion: expected, BaseRevision: 1})
	q.must(err)
	n := q.getQuote(id).WorkingRevision
	f, err := q.freezeQuote(q.author, id, r.RowVersion, n)
	q.must(err)
	submit := trdapi.SubmitReview{IdempotencyKey: q.key(), QuoteID: id, ExpectedVersion: f.RowVersion, BusinessRevision: n,
		ReviewRequestID: review, TargetCommand: target}
	if target == trdapi.TargetReplace {
		submit.ExpectedActiveRevision = 1
	}
	_, err = q.c.TRD.SubmitReview(q.author, submit)
	q.must(err)
	d, err := q.c.TRD.DecideReview(q.reviewer, q.approval(id, f.RowVersion+1, review, f.BusinessHash, approval))
	q.must(err)
	return n, f.BusinessHash, d.RowVersion
}

func (q *trdWorld) replaceCmd(id string, expected, revision int64, approval string) trdapi.Replace {
	return trdapi.Replace{IdempotencyKey: q.key(), QuoteID: id, ExpectedVersion: expected, BusinessRevision: revision, ExpectedActiveRevision: 1,
		ApprovalID: approval, ExpectedApprovalEffectivityVersion: 1}
}

// formalRows are the formal documents, commit evidence and numbers POL issued in company.A.
func (q *trdWorld) formalRows() (documents, commits, numbers int) {
	return q.count("company.A", "brant_trd.quote_document"), q.count("company.A", "brant_trd.quote_commit_evidence"),
		q.count("company.A", "brant_pol.issued_number")
}

func (q *trdWorld) statuses(id string) []string {
	var out []string
	for _, r := range q.getQuote(id).Revisions {
		out = append(out, r.Status)
	}
	return out
}

func number(n int) string {
	return "QT-" + shanghaiYear() + "-" + fmt.Sprintf("%05d", n)
}

func shanghaiYear() string {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	return strconv.Itoa(time.Now().In(loc).Year())
}

// formalAmount is one amount line of a frozen calculation.
type formalAmount struct {
	ID      string `json:"id"`
	Net     string `json:"net_minor"`
	Tax     string `json:"tax_minor"`
	Payable string `json:"payable_minor"`
}

// rendered is the formal document of revision rev of quote id with number, composed independently
// of TRD's renderer from what the frozen commercial document says and what PTY, CAT and POL say of
// its references, in the layout quote-text/0.1.0 documents (Python t03_candidate/document.py): the
// title, a blank line, the ten sections in the template's order separated by blank lines, and a
// final line break.
func (q *trdWorld) rendered(id string, rev int64, number string) string {
	q.t.Helper()
	var doc struct {
		QuoteID          string `json:"quote_id"`
		BusinessRevision int64  `json:"business_revision"`
		IssuedAt         string `json:"issued_at"`
		Data             struct {
			Lines []struct {
				LineID string `json:"line_id"`
				Sku    struct {
					ID       string `json:"sku_id"`
					Revision int64  `json:"spec_revision"`
				} `json:"sku_ref"`
				Quantity           string              `json:"quantity"`
				Unit               struct{ ID string } `json:"unit_ref"`
				UnitPrice          string              `json:"unit_price"`
				PriceBasisQuantity string              `json:"price_basis_quantity"`
				DiscountRatio      string              `json:"discount_ratio"`
			} `json:"lines"`
			Currency   struct{ ID string } `json:"currency_ref"`
			TradeTerms struct {
				ID       string
				Revision int64
			} `json:"trade_terms_ref"`
			Delivery struct {
				Date, Milestone string
				TimeZone        string `json:"time_zone"`
			} `json:"delivery"`
			ExpiresAt     string `json:"expires_at"`
			CustomerNotes string `json:"customer_notes"`
		} `json:"data"`
		Calculation struct {
			Lines    []formalAmount `json:"lines"`
			Net      string         `json:"net_minor"`
			Tax      string         `json:"tax_minor"`
			Payable  string         `json:"payable_minor"`
			Payments []struct {
				Stage   string `json:"stage_id"`
				Percent string `json:"percent"`
				Trigger string `json:"trigger"`
				Days    int    `json:"days"`
				Amount  string `json:"amount_minor"`
			} `json:"payments"`
		} `json:"calculation"`
		PartySnapshots struct {
			Issuer struct {
				LegalName string `json:"legal_name"`
				Address   string `json:"address"`
			} `json:"issuer"`
			Customer struct {
				Addresses []struct{ Role, Address string } `json:"addresses"`
			} `json:"customer"`
		} `json:"party_snapshots"`
	}
	v := q.getQuote(id)
	r := v.Revisions[rev-1]
	q.must(json.Unmarshal([]byte(r.Commercial), &doc))
	d, c := doc.Data, doc.Calculation
	party, err := q.c.PTY.GetParty(q.author, r.Data.CustomerRef.PartyID)
	q.must(err)
	customer := party.Revisions[slices.IndexFunc(party.Revisions, func(p ptyapi.PartyRevision) bool { return p.Revision == r.Data.CustomerRef.Revision })].Name
	contact, err := q.c.PTY.GetContact(q.author, r.Data.ContactRef.ContactID)
	q.must(err)
	config := func(kind, id string, revision int64) polapi.ConfigData {
		cv, err := q.c.POL.GetConfig(q.author, kind, id)
		q.must(err)
		return cv.Revisions[revision-1].Data
	}
	tmpl := config(polapi.KindQuoteDocumentTemplate, r.Data.DocumentTemplateRef.ID, r.Data.DocumentTemplateRef.Revision).QuoteDocumentTemplate
	tt := config(polapi.KindTradeTerms, d.TradeTerms.ID, d.TradeTerms.Revision).TradeTerms
	cur := config(polapi.KindCurrency, d.Currency.ID, r.Data.CurrencyRef.Revision).Currency
	address := ""
	for _, role := range []string{"buyer", "billing"} {
		for _, a := range doc.PartySnapshots.Customer.Addresses {
			if a.Role == role && address == "" {
				address = a.Address
			}
		}
	}
	var lines []string
	for _, l := range d.Lines {
		sku, err := q.c.CAT.GetSku(q.author, l.Sku.ID)
		q.must(err)
		item := sku.Revisions[slices.IndexFunc(sku.Revisions, func(s catapi.SpecRevision) bool { return s.Revision == l.Sku.Revision })].Name
		a := c.Lines[slices.IndexFunc(c.Lines, func(a formalAmount) bool { return a.ID == l.LineID })]
		lines = append(lines, strings.Join([]string{l.LineID, item, fmt.Sprintf("%s r%d", l.Sku.ID, l.Sku.Revision), l.Quantity + " " + l.Unit.ID,
			"price " + l.UnitPrice + " per " + l.PriceBasisQuantity, "discount " + l.DiscountRatio, "net " + a.Net, "tax " + a.Tax,
			"payable " + a.Payable}, " | "))
	}
	var payments []string
	for _, p := range c.Payments {
		payments = append(payments, strings.Join([]string{p.Stage, p.Percent + "%", p.Amount, p.Trigger, strconv.Itoa(p.Days) + " calendar days"}, " | "))
	}
	blocks := map[string]string{
		"issuer":    "Issuer: " + doc.PartySnapshots.Issuer.LegalName + "\n" + doc.PartySnapshots.Issuer.Address,
		"recipient": "Customer: " + customer + "\n" + address + "\nContact: " + contact.Name,
		"identity":  "Number: " + number + "\nQuote: " + doc.QuoteID + " r" + strconv.FormatInt(doc.BusinessRevision, 10) + "\nBusiness SHA-256: " + r.BusinessHash,
		"lines":     "Lines (all amounts in currency minor units):\n" + strings.Join(lines, "\n"),
		"totals": "Currency: " + d.Currency.ID + " | minor digits: " + strconv.Itoa(cur.MinorUnits) + "\nNet: " + c.Net + " | Tax: " + c.Tax +
			" | Payable: " + c.Payable,
		"terms": "Terms: " + tt.Incoterm + " " + tt.Edition + " | " + tt.ResponsibilityNote + "\nNamed place: " + tt.NamedPlace + " | Scope: " +
			d.TradeTerms.ID + " r" + strconv.FormatInt(d.TradeTerms.Revision, 10),
		"delivery": "Delivery: " + d.Delivery.Date + " | " + d.Delivery.Milestone + " | " + d.Delivery.TimeZone,
		"payments": "Payment plan (minor units; not receipts):\n" + strings.Join(payments, "\n"),
		"validity": "Commercial issue: " + doc.IssuedAt + "\nExpires exclusively: " + d.ExpiresAt,
		"notes":    d.CustomerNotes + "\n" + tmpl.FooterNote,
	}
	var sections []string
	for _, s := range tmpl.Sections {
		sections = append(sections, blocks[s])
	}
	return tmpl.Title + "\n\n" + strings.Join(sections, "\n\n") + "\n"
}

// Python test_t03_formalization.py test_activate_writes_number_file_binding_evidence_and_outbox_atomically
// (PYT-0413), test_t03_a19_configuration_consumption.py
// test_published_template_is_consumed_and_bound_to_formal_file (PYT-0161) and
// test_t03_query_contract.py test_commit_history_returns_server_evidence_without_result_payload
// (PYT-0431), TRD-INV-009 and 016: quote.activate makes the approved revision active with POL's first
// number of the year for business reference quote.1:1, the formal document's bytes are exactly the
// quote-text/0.1.0 rendering of the frozen revision, bound to its business hash, the template
// revision and its content hash; one commit evidence (read back as a summary and the SHA-256 of
// its entries) and one QuoteActivated carrying the file reference are written in the same
// transaction; the key replays; reads verify the bytes; the lookups and the guard answer for it.
//
// Also, on the same activation:
//   - test_t03_a19_configuration_consumption.py test_published_sales_tax_application_is_consumed_and_bound
//     (PYT-0162): the line's published sales tax application is bound at the freeze — the line
//     names it and the frozen document's dependency manifest pins its revision and content hash,
//     covered by the business hash the formal binding names (Go keeps no per-document configuration
//     binding table; the template is bound by the formal binding itself);
//   - test_t03_a29_trade_parties.py test_public_freeze_persists_server_binding_and_renders_it
//     (PYT-0350): the freeze seals the server's party binding (the issuer's and the customer's
//     snapshots, the trading profile pinned in the dependency manifest) and the formal document
//     prints the issuer, the customer, its address and the contact (quote-text/0.1.0 does not print
//     the issuer's registration number, which stays in the commercial document);
//   - test_t03_cost_binding.py test_activate_rechecks_and_records_current_snapshot_qualification
//     (PYT-0410, the activation half): the commit evidence records the bound snapshot and CST's
//     current verdict on it (adoptable).
func TestActivateWritesNumberFileBindingEvidenceAndOutboxAtomically(t *testing.T) {
	q := newTrdWorld(t)
	f, d := q.approved("quote.1")
	cmd := q.activateCmd("quote.1", d.RowVersion, 1, "approval.quote.1")
	a, err := q.c.TRD.Activate(q.author, cmd)
	want := number(1)
	if err != nil || a.Status != trdapi.StatusActive || a.RowVersion != d.RowVersion+1 || a.BusinessRevision != 1 || a.BusinessHash != f.BusinessHash ||
		a.ApprovalID != "approval.quote.1" || a.Number != want || a.FileName != want+".txt" || a.MimeType != "text/plain" || a.ReplacedRevision != 0 ||
		a.Replayed || len(a.DocumentSHA256) != 64 {
		t.Fatalf("activate %+v, %v", a, err)
	}
	doc, err := q.c.TRD.GetFormalDocument(q.author, "quote.1", 1)
	q.must(err)
	expected := q.rendered("quote.1", 1, want)
	if string(doc.Content) != expected {
		t.Fatalf("the stored document is not the rendering:\n%s\n--- want ---\n%s", doc.Content, expected)
	}
	if sha256Hex(expected) != a.DocumentSHA256 || doc.Formal.SizeBytes != len(expected) || doc.Status != trdapi.StatusActive ||
		doc.Formal.TemplateRef != tref(q.template) || len(doc.Formal.TemplateHash) != 64 || doc.Formal.RendererVersion != "quote-text/0.1.0" ||
		doc.Formal.BusinessRef != "quote.1:1" || doc.Formal.SequenceRef != tref(q.seq) || doc.BusinessHash != f.BusinessHash {
		t.Fatalf("document %+v", doc.Formal)
	}
	v := q.getQuote("quote.1")
	rv := v.Revisions[0]
	if v.ActiveRevision != 1 || rv.Status != trdapi.StatusActive || rv.Formal == nil || rv.Formal.Number != want {
		t.Fatalf("quote %+v", v)
	}
	c := rv.Formal.Commit
	if c.Command != trdapi.CommandActivate || c.ApprovalID != "approval.quote.1" || c.EffectivityVersion != 1 || c.RootVersion != a.RowVersion ||
		c.CommittedBy != actorRef("human", "editor") || c.CostSnapshotID != q.snap.SnapshotID || len(c.EntriesSHA256) != 64 || c.AuthorityEpoch < 1 {
		t.Fatalf("commit evidence %+v", c)
	}
	var entries string
	q.must(q.execScan("company.A", `SELECT entries FROM brant_trd.quote_commit_evidence WHERE quote_id = 'quote.1'`, &entries))
	for _, name := range []string{`"number","value":"` + want, `"content_sha256","value":"` + a.DocumentSHA256, `"template_content_hash"`,
		`"approval_id","value":"approval.quote.1"`, `"cost_snapshot_adoptability","value":"adoptable"`, `"committed_at"`} {
		if !strings.Contains(entries, name) {
			t.Errorf("entries lack %s: %s", name, entries)
		}
	}
	if sha256Hex(entries) != c.EntriesSHA256 {
		t.Fatal("the entries' SHA-256")
	}
	feqvBindings(t, q, "quote.1", rv, string(doc.Content))
	if documents, commits, numbers := q.formalRows(); documents != 1 || commits != 1 || numbers != 1 {
		t.Fatalf("%d documents, %d commit evidence, %d numbers", documents, commits, numbers)
	}
	events := q.eventsOf(trdapi.EventQuoteActivated)
	if len(events) != 1 || events[0].Payload["number"] != want || events[0].Payload["file_sha256"] != a.DocumentSHA256 ||
		events[0].Payload["file_name"] != want+".txt" || events[0].Payload["business_hash"] != f.BusinessHash ||
		events[0].Payload["file_size"] != float64(len(expected)) {
		t.Fatalf("events %+v", events)
	}
	again, err := q.c.TRD.Activate(q.author, cmd)
	if err != nil || !again.Replayed || again.ActionID != a.ActionID || again.Number != want || again.DocumentSHA256 != a.DocumentSHA256 {
		t.Fatalf("replay %+v, %v", again, err)
	}
	if _, _, numbers := q.formalRows(); numbers != 1 || len(q.eventsOf(trdapi.EventQuoteActivated)) != 1 {
		t.Fatal("a replay issues nothing")
	}
	active, err := q.c.TRD.GetActiveRevision(q.author, "quote.1")
	if err != nil || active.Ref.BusinessRevision != 1 || !active.Active || active.Code != "" || active.Number != want ||
		active.DocumentSHA256 != a.DocumentSHA256 || active.OnHold || active.ConversionTaken || active.ApprovalID != "approval.quote.1" {
		t.Fatalf("active %+v, %v", active, err)
	}
	found, err := q.c.TRD.ResolveQuoteRevisions(q.author, []trdapi.QuoteRevisionRef{{QuoteID: "quote.1", BusinessRevision: 1},
		{QuoteID: "quote.1", BusinessRevision: 2}, {QuoteID: "quote.9", BusinessRevision: 1}})
	if err != nil || len(found) != 1 || found[trdapi.QuoteRevisionRef{QuoteID: "quote.1", BusinessRevision: 1}].Check("/x") != nil {
		t.Fatalf("lookup %+v, %v", found, err)
	}
	actives, err := q.c.TRD.ResolveActiveRevisions(q.author, []string{"quote.1", "quote.9"})
	if err != nil || len(actives) != 1 || actives["quote.1"].Number != want {
		t.Fatalf("active lookup %+v, %v", actives, err)
	}
	verdict := q.revisionGuard(trdapi.QuoteRevisionBinding{QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: f.BusinessHash})
	if !verdict.Usable() || verdict.Number != want || verdict.DocumentSHA256 != a.DocumentSHA256 || verdict.ActiveRevision != 1 ||
		verdict.CheckConvertible("/x") != nil {
		t.Fatalf("guard %+v", verdict)
	}
	if v := q.revisionGuard(trdapi.QuoteRevisionBinding{QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: sha256Hex("other")}); v.Code !=
		trdapi.CodeBusinessHashMismatch {
		t.Fatalf("guard on another hash %+v", v)
	}
}

// revisionGuard calls QuoteRevisionGuard v1 in company.A as a downstream transaction would.
func (q *trdWorld) revisionGuard(ref trdapi.QuoteRevisionBinding) trdapi.QuoteRevisionVerdict {
	q.t.Helper()
	var v trdapi.QuoteRevisionVerdict
	q.must(q.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = trdapi.ScanQuoteRevisionGuard(ref, exec.QueryRow(ctx, trdapi.QuoteRevisionGuardSQL, trdapi.QuoteRevisionGuardArgs(ref)...).Scan)
		return err
	}))
	return v
}

// inTRD runs fn in a transaction of the application role in company.A.
func (q *trdWorld) inTRD(fn func(ctx context.Context, exec bentopg.Executor) error) error {
	db, err := bentopg.NewDB(pgtest.AppPool(q.t))
	if err != nil {
		q.t.Fatal(err)
	}
	return bento.ScopeTransactor{DB: db, Scope: q.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, exec)
	})
}

// convert plays TRD's order step on quote id, as the next wave's order preparation takes the
// conversion slot: in one transaction it locks the quote root FOR UPDATE, asks QuoteRevisionGuard
// v1 whether revision rev at hash is convertible, moves the root version and takes the slot for
// order; hold, when given, runs before the transaction ends.
func (q *trdWorld) convert(id string, rev int64, hash, order string, hold func()) error {
	return q.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
		s := q.scope("company.A")
		var version int64
		if err := exec.QueryRow(ctx, `SELECT root_version FROM brant_trd.quote_root WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3
			FOR UPDATE`, string(s.Tenant), string(s.Company), id).Scan(&version); err != nil {
			return err
		}
		ref := trdapi.QuoteRevisionBinding{QuoteID: id, BusinessRevision: rev, BusinessHash: hash}
		v, err := trdapi.ScanQuoteRevisionGuard(ref, exec.QueryRow(ctx, trdapi.QuoteRevisionGuardSQL, trdapi.QuoteRevisionGuardArgs(ref)...).Scan)
		if err == nil {
			err = v.CheckConvertible("/args/quote_revision_ref")
		}
		if err != nil {
			return err
		}
		if _, err = exec.Exec(ctx, `UPDATE brant_trd.quote_root SET root_version = root_version + 1, updated_at = now()
			WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3`, string(s.Tenant), string(s.Company), id); err != nil {
			return err
		}
		if _, err = exec.Exec(ctx, `INSERT INTO brant_trd.quote_conversion_slot (tenant_id, company_id, quote_id, slot, revision, business_hash,
			order_id, taken_by_kind, taken_by_id, take_epoch, taken_at, root_version) VALUES ($1, $2, $3, 'full_v1', $4, $5, $6, 'human', 'clerk', 1,
			now(), $7)`, string(s.Tenant), string(s.Company), id, rev, hash, order, version+1); err != nil {
			return err
		}
		if hold != nil {
			hold()
		}
		return nil
	})
}

// Python test_t03_formalization.py test_expired_or_revoked_approval_leaves_no_formal_artifacts
// (PYT-0414) and test_t03_a35_time_qualification.py
// test_expired_offset_blocks_replacement_and_preserves_active_revision (PYT-0400) and
// test_committed_replay_remains_successful_after_expiry (PYT-0399): a revoked approval
// (APPROVAL_REVOKED) and a lapsed one (APPROVAL_EXPIRED) release nothing — no document, no commit
// evidence, no event, and POL issues no number, because the activation checks before it asks for
// one; an activation committed before its approval lapsed replays after; a lapsed replacement
// approval leaves the active revision active.
func TestExpiredOrRevokedApprovalLeavesNoFormalArtifacts(t *testing.T) {
	q := newTrdWorld(t)
	_, d := q.approved("quote.1")
	r, err := q.c.TRD.RevokeApproval(q.reviewer, trdapi.RevokeApproval{IdempotencyKey: q.key(), QuoteID: "quote.1", ApprovalID: "approval.quote.1",
		ExpectedEffectivityVersion: 1, Reason: "客户信用变化", Evidence: q.proof})
	q.must(err)
	if r.Status != trdapi.ApprovalRevoked || r.RowVersion != d.RowVersion+1 {
		t.Fatalf("revoke %+v", r)
	}
	// At the effectivity version it was approved at, the command is stale (Python's public adapter:
	// VERSION_CONFLICT); at the revoked one, the approval is APPROVAL_REVOKED.
	_, err = q.c.TRD.Activate(q.author, q.activateCmd("quote.1", r.RowVersion, 1, "approval.quote.1"))
	if code(err) != kernel.CodeVersionConflict || path(err) != "/args/expected_approval_effectivity_version" {
		t.Fatalf("a revoked approval at version 1: %s at %s", code(err), path(err))
	}
	revoked := q.activateCmd("quote.1", r.RowVersion, 1, "approval.quote.1")
	revoked.ExpectedApprovalEffectivityVersion = 2
	_, err = q.c.TRD.Activate(q.author, revoked)
	if code(err) != trdapi.CodeApprovalRevoked {
		t.Fatalf("a revoked approval: %s", code(err))
	}
	// Approvals lapsing three seconds after they are given: quote.4's is used before, quote.2's after.
	shortly := func(id string) (trdapi.ReviewReceipt, time.Time) {
		f := q.frozenQuote(id)
		_, err := q.submit(q.author, id, f.RowVersion, 1, "review."+id)
		q.must(err)
		short := q.approval(id, f.RowVersion+1, "review."+id, f.BusinessHash, "approval."+id)
		lapse := time.Now().Add(3 * time.Second)
		short.ExpiresAt = kernel.FormatTimestamp(lapse)
		d, err := q.c.TRD.DecideReview(q.reviewer, short)
		q.must(err)
		return d, lapse
	}
	d4, _ := shortly("quote.4")
	used := q.activateCmd("quote.4", d4.RowVersion, 1, "approval.quote.4")
	a4, err := q.c.TRD.Activate(q.author, used)
	q.must(err)
	d2, lapse := shortly("quote.2")
	time.Sleep(time.Until(lapse) + 50*time.Millisecond)
	_, err = q.c.TRD.Activate(q.author, q.activateCmd("quote.2", d2.RowVersion, 1, "approval.quote.2"))
	if code(err) != trdapi.CodeApprovalExpired {
		t.Fatalf("a lapsed approval: %s", code(err))
	}
	// test_t03_a35_time_qualification.py test_committed_replay_remains_successful_after_expiry
	// (PYT-0399): the committed activation replays after its approval lapsed.
	again, err := q.c.TRD.Activate(q.author, used)
	if err != nil || !again.Replayed || again.ActionID != a4.ActionID || again.Number != a4.Number {
		t.Fatalf("a replay after the lapse %+v, %v", again, err)
	}
	if documents, commits, numbers := q.formalRows(); documents != 1 || commits != 1 || numbers != 1 || len(q.eventsOf(trdapi.EventQuoteActivated)) != 1 {
		t.Fatalf("%d documents, %d commit evidence, %d numbers after the refusals", documents, commits, numbers)
	}
	if s := q.statuses("quote.2"); !slices.Equal(s, []string{trdapi.StatusPendingApproval}) {
		t.Fatalf("quote.2 %v", s)
	}
	// A replacement approval lapsing before its use leaves the active revision (PYT-0400).
	a := q.activated("quote.3")
	r3, err := q.c.TRD.Revise(q.author, trdapi.Revise{IdempotencyKey: q.key(), QuoteID: "quote.3", ExpectedVersion: a.RowVersion, BaseRevision: 1})
	q.must(err)
	f3, err := q.freezeQuote(q.author, "quote.3", r3.RowVersion, 2)
	q.must(err)
	_, err = q.c.TRD.SubmitReview(q.author, trdapi.SubmitReview{IdempotencyKey: q.key(), QuoteID: "quote.3", ExpectedVersion: f3.RowVersion,
		BusinessRevision: 2, ReviewRequestID: "review.3", TargetCommand: trdapi.TargetReplace, ExpectedActiveRevision: 1})
	q.must(err)
	short := q.approval("quote.3", f3.RowVersion+1, "review.3", f3.BusinessHash, "approval.3")
	lapse = time.Now().Add(2 * time.Second)
	short.ExpiresAt = kernel.FormatTimestamp(lapse)
	d3, err := q.c.TRD.DecideReview(q.reviewer, short)
	q.must(err)
	time.Sleep(time.Until(lapse) + 50*time.Millisecond)
	_, err = q.c.TRD.Replace(q.author, q.replaceCmd("quote.3", d3.RowVersion, 2, "approval.3"))
	if code(err) != trdapi.CodeApprovalExpired {
		t.Fatalf("a lapsed replacement approval: %s", code(err))
	}
	if s := q.statuses("quote.3"); !slices.Equal(s, []string{trdapi.StatusActive, trdapi.StatusPendingApproval}) {
		t.Fatalf("quote.3 %v", s)
	}
	if _, _, numbers := q.formalRows(); numbers != 2 {
		t.Fatalf("%d numbers: the lapsed replacement took one", numbers)
	}
}

// Python test_t03_formalization.py test_duplicate_formalization_is_rejected_without_second_number
// (PYT-0415): a second activation of the formalized revision — at the old root version
// VERSION_CONFLICT, at the new one INVALID_STATE (the revision is active) — writes nothing and takes
// no second number.
func TestDuplicateFormalizationIsRejectedWithoutSecondNumber(t *testing.T) {
	q := newTrdWorld(t)
	_, d := q.approved("quote.1")
	a, err := q.c.TRD.Activate(q.author, q.activateCmd("quote.1", d.RowVersion, 1, "approval.quote.1"))
	q.must(err)
	_, err = q.c.TRD.Activate(q.author, q.activateCmd("quote.1", d.RowVersion, 1, "approval.quote.1"))
	q.want(err, kernel.CodeVersionConflict)
	_, err = q.c.TRD.Activate(q.author, q.activateCmd("quote.1", a.RowVersion, 1, "approval.quote.1"))
	q.want(err, kernel.CodeInvalidState)
	if documents, commits, numbers := q.formalRows(); documents != 1 || commits != 1 || numbers != 1 || len(q.eventsOf(trdapi.EventQuoteActivated)) != 1 {
		t.Fatalf("%d documents, %d commit evidence, %d numbers", documents, commits, numbers)
	}
}

// TRD-INV-001 (SC-03 for the first activation): two revisions of one root, both approved for
// quote.activate, activated at once with different keys: exactly one is active, the other is
// refused (VERSION_CONFLICT) with nothing of it written; its number, issued before its transaction,
// is a gap in POL's sequence and nothing else. Two activations with one key commit one action and
// one event; the other is its replay.
func TestTwoFirstActivationsHaveOneWinner(t *testing.T) {
	q := newTrdWorld(t)
	_, d := q.approved("quote.1")
	n, _, version := q.revisedAndApproved("quote.1", d.RowVersion, trdapi.TargetActivate, "review.1b", "approval.1b")
	cmds := []trdapi.Activate{q.activateCmd("quote.1", version, 1, "approval.quote.1"), q.activateCmd("quote.1", version, n, "approval.1b")}
	got := make([]string, 2)
	var wg sync.WaitGroup
	for i := range cmds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := q.c.TRD.Activate(q.author, cmds[i])
			got[i] = code(err)
		}()
	}
	wg.Wait()
	sorted := slices.Sorted(slices.Values(got))
	if !slices.Equal(sorted, []string{"OK", kernel.CodeVersionConflict}) {
		t.Fatalf("two first activations: %v", got)
	}
	v := q.getQuote("quote.1")
	actives := 0
	for _, r := range v.Revisions {
		if r.Status == trdapi.StatusActive {
			actives++
		}
	}
	documents, commits, numbers := q.formalRows()
	if actives != 1 || documents != 1 || commits != 1 || numbers > 2 || len(q.eventsOf(trdapi.EventQuoteActivated)) != 1 {
		t.Fatalf("%d active, %d documents, %d commit evidence, %d numbers", actives, documents, commits, numbers)
	}
	// One key sent twice at once.
	_, d2 := q.approved("quote.2")
	cmd := q.activateCmd("quote.2", d2.RowVersion, 1, "approval.quote.2")
	receipts := make([]trdapi.FormalizationReceipt, 2)
	for i := range receipts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			receipts[i], err = q.c.TRD.Activate(q.author, cmd)
			got[i] = code(err)
		}()
	}
	wg.Wait()
	if !slices.Equal(got, []string{"OK", "OK"}) || receipts[0].ActionID != receipts[1].ActionID || receipts[0].Replayed == receipts[1].Replayed ||
		receipts[0].Number != receipts[1].Number {
		t.Fatalf("one key twice: %v %+v", got, receipts)
	}
	if n := len(q.eventsOf(trdapi.EventQuoteActivated)); n != 2 {
		t.Fatalf("%d QuoteActivated for two quotes", n)
	}
}

// Python test_t03_formalization.py test_replace_supersedes_active_revision_and_writes_new_formal_artifacts
// (PYT-0416): quote.replace makes the approved revision active and the active one superseded in one
// transaction, with a second number and document; QuoteReplaced names the superseded revision;
// both documents stay readable.
func TestReplaceSupersedesActiveRevisionAndWritesNewFormalArtifacts(t *testing.T) {
	q := newTrdWorld(t)
	a := q.activated("quote.1")
	n, hash, version := q.revisedAndApproved("quote.1", a.RowVersion, trdapi.TargetReplace, "review.2", "approval.2")
	r, err := q.c.TRD.Replace(q.author, q.replaceCmd("quote.1", version, n, "approval.2"))
	if err != nil || r.Status != trdapi.StatusActive || r.BusinessRevision != n || r.BusinessHash != hash || r.ReplacedRevision != 1 ||
		r.Number != number(2) || r.RowVersion != version+1 {
		t.Fatalf("replace %+v, %v", r, err)
	}
	if s := q.statuses("quote.1"); !slices.Equal(s, []string{trdapi.StatusSuperseded, trdapi.StatusActive}) {
		t.Fatalf("statuses %v", s)
	}
	old, err := q.c.TRD.GetFormalDocument(q.author, "quote.1", 1)
	q.must(err)
	doc, err := q.c.TRD.GetFormalDocument(q.author, "quote.1", n)
	q.must(err)
	if old.Status != trdapi.StatusSuperseded || old.Formal.Number != a.Number || string(doc.Content) != q.rendered("quote.1", n, r.Number) ||
		doc.Formal.Commit.ReplacedRevision != 1 || doc.Formal.Commit.Command != trdapi.CommandReplace {
		t.Fatalf("documents %+v %+v", old.Formal, doc.Formal)
	}
	if documents, commits, numbers := q.formalRows(); documents != 2 || commits != 2 || numbers != 2 {
		t.Fatalf("%d documents, %d commit evidence, %d numbers", documents, commits, numbers)
	}
	replaced := q.eventsOf(trdapi.EventQuoteReplaced)
	if len(replaced) != 1 || replaced[0].Payload["replaced_revision"] != float64(1) || replaced[0].Payload["number"] != number(2) ||
		len(q.eventsOf(trdapi.EventQuoteActivated)) != 1 {
		t.Fatalf("events %+v", replaced)
	}
	if found, err := q.c.TRD.ResolveQuoteRevisions(q.author, []trdapi.QuoteRevisionRef{{QuoteID: "quote.1", BusinessRevision: 1}}); err != nil ||
		found[trdapi.QuoteRevisionRef{QuoteID: "quote.1", BusinessRevision: 1}].Code != trdapi.CodeQuoteNotCurrent {
		t.Fatalf("the superseded revision %+v, %v", found, err)
	}
}

// Python test_t03_formalization.py test_cancel_clears_current_pointer_without_deleting_formal_history
// (PYT-0417): quote.cancel ends the active revision: the root has no active revision, the formal
// document and commit evidence stay readable, QuoteCancelled is published; a second cancellation is
// QUOTE_NOT_CURRENT; the guard answers QUOTE_NOT_CURRENT.
func TestCancelClearsCurrentPointerWithoutDeletingFormalHistory(t *testing.T) {
	q := newTrdWorld(t)
	a := q.activated("quote.1")
	cmd := trdapi.Cancel{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: a.RowVersion, BusinessRevision: 1, Reason: "客户取消项目",
		Evidence: q.proof}
	r, err := q.c.TRD.Cancel(q.author, cmd)
	if err != nil || r.Status != trdapi.StatusCancelled || r.BusinessRevision != 1 || r.BusinessHash != a.BusinessHash || r.RowVersion != a.RowVersion+1 {
		t.Fatalf("cancel %+v, %v", r, err)
	}
	again, err := q.c.TRD.Cancel(q.author, cmd)
	if err != nil || !again.Replayed || again.BusinessRevision != 1 {
		t.Fatalf("replay %+v, %v", again, err)
	}
	v := q.getQuote("quote.1")
	if v.ActiveRevision != 0 || v.Revisions[0].Status != trdapi.StatusCancelled || v.Revisions[0].Closed == nil ||
		v.Revisions[0].Closed.Reason != "客户取消项目" || v.Revisions[0].Formal == nil {
		t.Fatalf("quote %+v", v.Revisions[0])
	}
	doc, err := q.c.TRD.GetFormalDocument(q.author, "quote.1", 1)
	if err != nil || doc.Status != trdapi.StatusCancelled || doc.Formal.DocumentSHA256 != a.DocumentSHA256 {
		t.Fatalf("document after the cancellation %+v, %v", doc.Formal, err)
	}
	_, err = q.c.TRD.GetActiveRevision(q.author, "quote.1")
	q.want(err, trdapi.CodeNoActiveRevision)
	_, err = q.c.TRD.Cancel(q.author, trdapi.Cancel{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: r.RowVersion, BusinessRevision: 1,
		Reason: "again", Evidence: q.proof})
	q.want(err, trdapi.CodeQuoteNotCurrent)
	if v := q.revisionGuard(trdapi.QuoteRevisionBinding{QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: a.BusinessHash}); v.Code !=
		trdapi.CodeQuoteNotCurrent || v.Status != trdapi.StatusCancelled || v.ActiveRevision != 0 {
		t.Fatalf("guard %+v", v)
	}
	if documents, commits, _ := q.formalRows(); documents != 1 || commits != 1 || len(q.eventsOf(trdapi.EventQuoteCancelled)) != 1 {
		t.Fatalf("%d documents, %d commit evidence", documents, commits)
	}
}

// Python test_t03_quote_review.py test_withdraw_closes_open_review_and_invalidates_revision
// (PYT-0441): withdrawing a pending revision cancels its open review, the reviewer can no longer
// decide it, and it is never activated; a frozen revision is withdrawn too; the active revision is
// not (INVALID_STATE); evidence the actor does not read is REFERENCE_INVALID; QuoteWithdrawn is
// published.
func TestWithdrawClosesOpenReviewAndInvalidatesRevision(t *testing.T) {
	q := newTrdWorld(t)
	f := q.frozenQuote("quote.1")
	s, err := q.submit(q.author, "quote.1", f.RowVersion, 1, "review.1")
	q.must(err)
	_, err = q.c.TRD.Withdraw(q.author, trdapi.Withdraw{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: s.RowVersion,
		BusinessRevision: 1, Reason: "客户撤回", Evidence: []trdapi.Evidence{{SourceID: "unknown.source", Locator: "x"}}})
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence/0/source_id" {
		t.Fatalf("unreadable evidence: %s at %s", code(err), path(err))
	}
	w, err := q.c.TRD.Withdraw(q.author, trdapi.Withdraw{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: s.RowVersion,
		BusinessRevision: 1, Reason: "客户撤回", Evidence: q.proof})
	if err != nil || w.Status != trdapi.StatusWithdrawn || w.BusinessRevision != 1 || w.RowVersion != s.RowVersion+1 {
		t.Fatalf("withdraw %+v, %v", w, err)
	}
	v := q.getQuote("quote.1")
	if v.Revisions[0].Status != trdapi.StatusWithdrawn || v.Reviews[0].Status != trdapi.ReviewCancelled || v.Reviews[0].DecidedBy !=
		actorRef("human", "editor") || v.Revisions[0].Closed == nil || v.Revisions[0].Closed.Status != trdapi.StatusWithdrawn {
		t.Fatalf("quote %+v %+v", v.Revisions[0], v.Reviews)
	}
	_, err = q.c.TRD.DecideReview(q.reviewer, q.approval("quote.1", w.RowVersion, "review.1", f.BusinessHash, "approval.1"))
	q.want(err, kernel.CodeInvalidState)
	ev := q.eventsOf(trdapi.EventQuoteWithdrawn)
	if len(ev) != 1 || ev[0].Payload["review_request_id"] != "review.1" {
		t.Fatalf("events %+v", ev)
	}
	f2 := q.frozenQuote("quote.2")
	if _, err = q.c.TRD.Withdraw(q.author, trdapi.Withdraw{IdempotencyKey: q.key(), QuoteID: "quote.2", ExpectedVersion: f2.RowVersion,
		BusinessRevision: 1, Reason: "不再需要", Evidence: q.proof}); err != nil {
		t.Fatalf("withdraw a frozen revision: %v", err)
	}
	a := q.activated("quote.3")
	_, err = q.c.TRD.Withdraw(q.author, trdapi.Withdraw{IdempotencyKey: q.key(), QuoteID: "quote.3", ExpectedVersion: a.RowVersion,
		BusinessRevision: 1, Reason: "r", Evidence: q.proof})
	q.want(err, kernel.CodeInvalidState)
	agent := q.actorOf(kernel.ActorAI, "agent.1")
	_, err = q.c.TRD.Cancel(agent, trdapi.Cancel{IdempotencyKey: q.key(), QuoteID: "quote.3", ExpectedVersion: a.RowVersion, BusinessRevision: 1,
		Reason: "r", Evidence: q.proof})
	q.want(err, kernel.CodePermissionDenied)
	_, err = q.c.TRD.Activate(agent, q.activateCmd("quote.3", a.RowVersion, 1, "approval.quote.3"))
	q.want(err, kernel.CodePermissionDenied)
}

// ADR-BR-004 option B for the activation (SC-04, Context Map R11, R13, R14, R15): what the approved
// revision binds must still be in force when it is activated. A CST source denial, a party
// revision and a SKU specification revision held off by an activation holding the guards wait
// until it commits; once they committed, the next activation is refused with the owner's verdict at
// the stored commercial document's path, nothing is written and POL issues no number. Each lapse
// alone is refused the same way: a retired sales tax application (Python test_t03_a28_sales_terms.py
// test_retired_sales_application_blocks_formalization_without_any_business_residue, PYT-0331;
// CONFIG_NOT_ACTIVE where Python says REFERENCE_STALE) and a retired template. Python's
// test_revoked_publication_evidence_blocks_formalization_atomically revokes the verification of a
// template's publication evidence; POL has no such revocation (it checks evidence only when it
// publishes, S1-D16), so the retired template is the nearest Go lapse, not a port of that test.
func TestGuardedInputsLapsingBeforeActivationAreRefused(t *testing.T) {
	q := newTrdWorld(t)
	d := newDownstream(q.world)
	_, d1 := q.approved("quote.1")
	_, d2 := q.approved("quote.2")
	counter := q.counter()
	cmd := q.activateCmd("quote.1", d1.RowVersion, 1, "approval.quote.1")
	blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandActivate, cmd.IdempotencyKey)
	done := make(chan error, 1)
	go func() {
		_, err := q.c.TRD.Activate(q.author, cmd)
		done <- err
	}()
	if !awaitInHold(t, "the activation holds its guards and waits at its action result", func() bool { return d.blocks(blocker) }, done) {
		release()
		return
	}
	activation := blockedBy(t, blocker)[0]
	changed := make(chan timed, 3)
	partyKey, specKey := q.key(), q.key()
	go func() {
		_, err := q.c.CST.AttestSource(q.reviewer, q.denyCmd("src.1", counter))
		changed <- timed{time.Now(), err}
	}()
	go func() {
		err := q.reviseParty(q.author, "customer.1", 5, partyKey)
		changed <- timed{time.Now(), err}
	}()
	go func() {
		err := q.reviseSpec(q.author, "sku.1", 2, specKey)
		changed <- timed{time.Now(), err}
	}()
	ok := awaitInHold(t, "the denial and both revisions wait on the activation", func() bool { return len(blockedBy(t, activation)) == 3 }, changed)
	released := time.Now()
	release()
	if !ok {
		return
	}
	if err := <-done; err != nil {
		t.Fatalf("the activation holding the guards: %v", err)
	}
	for range 3 {
		if r := <-changed; r.err != nil || !r.at.After(released) {
			t.Fatalf("an upstream change %v, after the activation %v", r.err, r.at.After(released))
		}
	}
	_, _, before := q.formalRows()
	_, err := q.c.TRD.Activate(q.author, q.activateCmd("quote.2", d2.RowVersion, 1, "approval.quote.2"))
	if code(err) != catapi.CodeReferenceStale || path(err) != "/data/lines/0/sku_ref" {
		t.Fatalf("after the upstream changes: %s at %s", code(err), path(err))
	}
	if documents, _, numbers := q.formalRows(); documents != 1 || numbers != before || len(q.eventsOf(trdapi.EventQuoteActivated)) != 1 {
		t.Fatalf("%d documents, %d numbers", documents, numbers)
	}
	if s := q.statuses("quote.2"); !slices.Equal(s, []string{trdapi.StatusPendingApproval}) {
		t.Fatalf("quote.2 %v", s)
	}

	// Each lapse alone, in a world of its own: the owner's verdict at the bound input.
	for name, c := range map[string]struct {
		lapse      func(q *trdWorld)
		code, path string
	}{
		"CST source denial": {func(q *trdWorld) {
			_, err := q.c.CST.AttestSource(q.reviewer, q.denyCmd("src.1", q.counter()))
			q.must(err)
		}, cstapi.CodeCostSnapshotIneligible, "/data/cost_snapshot_ref"},
		"PTY party revision": {func(q *trdWorld) { q.must(q.reviseParty(q.author, "customer.1", 5, q.key())) }, trdapi.CodeReferenceStale,
			"/data/trading_profile_ref"},
		"POL sales tax application retired (PYT-0331)": {func(q *trdWorld) {
			_, err := q.c.POL.Retire(q.reviewer, polapi.Retire{IdempotencyKey: q.key(), Kind: q.sale.Kind, ConfigID: q.sale.ConfigID,
				ExpectedVersion: 2, Reason: "停用税适用"})
			q.must(err)
		}, polapi.CodeConfigNotActive, "/data/lines/0/tax_application_ref"},
		"POL template retired": {func(q *trdWorld) {
			_, err := q.c.POL.Retire(q.reviewer, polapi.Retire{IdempotencyKey: q.key(), Kind: q.template.Kind, ConfigID: q.template.ConfigID,
				ExpectedVersion: 2, Reason: "停用模板"})
			q.must(err)
		}, polapi.CodeConfigNotActive, "/data/document_template_ref"},
	} {
		w := newTrdWorld(t)
		_, dw := w.approved("quote.1")
		c.lapse(w)
		_, err := w.c.TRD.Activate(w.author, w.activateCmd("quote.1", dw.RowVersion, 1, "approval.quote.1"))
		if code(err) != c.code || path(err) != c.path {
			t.Errorf("%s: %s at %s, want %s at %s", name, code(err), path(err), c.code, c.path)
		}
		if documents, commits, numbers := w.formalRows(); documents != 0 || commits != 0 || numbers != 0 {
			t.Errorf("%s: %d documents, %d commit evidence, %d numbers", name, documents, commits, numbers)
		}
	}
}

// failInsert makes every insert into table in w's tenant fail until the returned function drops
// the trigger: an injected storage failure inside the formalization's transaction.
func (w *scenWorld) failInsert(table string) func() {
	w.t.Helper()
	migrator := pgtest.MigratorPool(w.t)
	trigger := strings.ReplaceAll(pgtest.UniqueID(w.t, "fail_"), ".", "_")
	pgtest.Schema(w.t, migrator, pgtest.AppPool(w.t), "brant_t_fail_", fmt.Sprintf(`
CREATE FUNCTION {{schema}}.fail() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'test storage failure'; END $$;
CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW WHEN (NEW.tenant_id = '%s') EXECUTE FUNCTION {{schema}}.fail();`, trigger, table, w.tenant))
	return func() {
		if _, err := migrator.Exec(context.Background(), "DROP TRIGGER "+trigger+" ON "+table); err != nil {
			w.t.Fatal(err)
		}
	}
}

// TRD-INV-009 and 016 (WBS S3-04: 文件或编号失败整体回滚; Python test_t03_write_contract.py
// test_action_result_failure_rolls_back_quote_event_and_root, PYT-0460, for the activation): a
// template the renderer cannot render and an actor POL refuses a number leave nothing and take no
// number; a failure storing the document, or the Outbox, after POL issued the number rolls the whole
// formalization back — no document, no commit evidence, no event, the revision pending — and the
// retry with the same key formalizes with that same number, so the sequence has no gap.
func TestNumberRenderAndFileFailuresLeaveNothing(t *testing.T) {
	q := newTrdWorld(t)
	// Rendering: a zh-CN template (quote-text/0.1.0 has an en layout only).
	zh := q.configure(q.author, q.reviewer, polapi.KindQuoteDocumentTemplate, "quote.template.zh", 0, q.win.from, time.Time{},
		polapi.ConfigData{QuoteDocumentTemplate: &polapi.QuoteDocumentTemplate{Name: "zh", Language: polapi.LanguageZhCN,
			RendererVersion: polapi.QuoteRendererVersion, Title: "报价单", Sections: polapi.QuoteSections(), FooterNote: "合成"}}, true)
	data := q.data()
	data.DocumentTemplateRef = trefp(zh)
	_, err := q.saveQuote(q.author, "quote.zh", 0, data)
	q.must(err)
	f, err := q.freezeQuote(q.author, "quote.zh", 1, 1)
	q.must(err)
	_, err = q.submit(q.author, "quote.zh", f.RowVersion, 1, "review.zh")
	q.must(err)
	dz, err := q.c.TRD.DecideReview(q.reviewer, q.approval("quote.zh", f.RowVersion+1, "review.zh", f.BusinessHash, "approval.zh"))
	q.must(err)
	_, err = q.c.TRD.Activate(q.author, q.activateCmd("quote.zh", dz.RowVersion, 1, "approval.zh"))
	if code(err) != trdapi.CodeDependencyNotSupported || path(err) != "/template/language" {
		t.Fatalf("a zh-CN template: %s at %s", code(err), path(err))
	}
	// Numbering: an activator POL does not let issue numbers. Reading the facts: an activator who does
	// not read PTY (the document prints the customer's and the contact's names, read from PTY before
	// the transaction). This is not Python's read permission on the configurations the quote
	// consumes: POL's lookups need only an active grant (S1-D15), so Go has none to check.
	_, d := q.approved("quote.1")
	clerk := q.regrant("clerk", "company", without(allPermissions, "number.issue"))
	_, err = q.c.TRD.Activate(clerk, q.activateCmd("quote.1", d.RowVersion, 1, "approval.quote.1"))
	q.want(err, kernel.CodePermissionDenied)
	blind := q.regrant("blind", "company", without(allPermissions, "pty.read"))
	_, err = q.c.TRD.Activate(blind, q.activateCmd("quote.1", d.RowVersion, 1, "approval.quote.1"))
	q.want(err, kernel.CodePermissionDenied)
	if documents, commits, numbers := q.formalRows(); documents != 0 || commits != 0 || numbers != 0 {
		t.Fatalf("%d documents, %d commit evidence, %d numbers after the render, read and number refusals", documents, commits, numbers)
	}
	// Storage: the document, then the Outbox, fail after the number was issued.
	for _, fault := range []struct {
		name string
		arm  func() func()
	}{
		{"the document", func() func() { return q.failInsert("brant_trd.quote_document") }},
		{"the Outbox", func() func() { return q.failOutbox(trdapi.EventQuoteActivated) }},
	} {
		id := "quote." + strings.ReplaceAll(fault.name, " ", ".")
		_, d := q.approved(id)
		cmd := q.activateCmd(id, d.RowVersion, 1, "approval."+id)
		documentsBefore, commitsBefore, issued := q.formalRows()
		drop := fault.arm()
		_, err := q.c.TRD.Activate(q.author, cmd)
		if _, isRejection := kernel.AsRejection(err); err == nil || isRejection || !strings.Contains(err.Error(), "test storage failure") {
			t.Fatalf("%s failing: %v", fault.name, err)
		}
		documents, commits, numbers := q.formalRows()
		if documents != documentsBefore || commits != commitsBefore || numbers != issued+1 {
			t.Fatalf("%s failing: %d documents, %d commit evidence, %d numbers (one issued before the transaction)", fault.name, documents,
				commits, numbers)
		}
		if s := q.statuses(id); !slices.Equal(s, []string{trdapi.StatusPendingApproval}) {
			t.Fatalf("%s failing: %v", fault.name, s)
		}
		drop()
		a, err := q.c.TRD.Activate(q.author, cmd)
		if err != nil || a.Replayed || a.Number != number(issued+1) {
			t.Fatalf("%s: the retry %+v, %v", fault.name, a, err)
		}
		if _, _, numbers := q.formalRows(); numbers != issued+1 {
			t.Fatalf("%s: the retry took another number (%d)", fault.name, numbers)
		}
	}
	if n := len(q.eventsOf(trdapi.EventQuoteActivated)); n != 2 {
		t.Fatalf("%d QuoteActivated", n)
	}
}

// TRD-INV-010 and SC-03 on PostgreSQL 16: a replacement and a conversion of the same active
// revision have one winner in every commit order. The conversion committing first (it holds the
// root while the replacement waits for it) refuses the replacement (VERSION_CONFLICT, then
// ROOT_CONVERSION_LOCKED at the current version, cancellation too); the replacement committing
// first (it holds the root at its action result while the conversion waits) makes the conversion's
// guard answer QUOTE_NOT_CURRENT; two replacements of one revision have one winner; unsynchronized,
// replace and convert still have exactly one.
func TestReplaceAndConvertHaveOneWinner(t *testing.T) {
	q := newTrdWorld(t)
	d := newDownstream(q.world)
	setup := func(id string) (n int64, version int64, hash string) {
		a := q.activated(id)
		n, _, version = q.revisedAndApproved(id, a.RowVersion, trdapi.TargetReplace, "review."+id+".2", "approval."+id+".2")
		return n, version, a.BusinessHash
	}

	// The conversion first.
	n, version, hash := setup("quote.1")
	holding, hold := make(chan struct{}), make(chan struct{})
	converted := make(chan error, 1)
	go func() {
		converted <- q.convert("quote.1", 1, hash, "order.1", func() { close(holding); <-hold })
	}()
	<-holding
	replaced := make(chan error, 1)
	go func() {
		_, err := q.c.TRD.Replace(q.author, q.replaceCmd("quote.1", version, n, "approval.quote.1.2"))
		replaced <- err
	}()
	ok := awaitInHold(t, "the replacement waits for the converting root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, replaced)
	close(hold)
	if !ok {
		return
	}
	if err := <-converted; err != nil {
		t.Fatalf("the conversion: %v", err)
	}
	if err := <-replaced; code(err) != kernel.CodeVersionConflict {
		t.Fatalf("a replacement behind the conversion: %s", code(err))
	}
	v := q.getQuote("quote.1")
	if v.Conversion == nil || v.Conversion.OrderID != "order.1" || !slices.Equal(q.statuses("quote.1"), []string{trdapi.StatusActive, trdapi.StatusPendingApproval}) {
		t.Fatalf("after the conversion %+v %v", v.Conversion, q.statuses("quote.1"))
	}
	_, err := q.c.TRD.Replace(q.author, q.replaceCmd("quote.1", v.RootVersion, n, "approval.quote.1.2"))
	q.want(err, trdapi.CodeRootConversionLocked)
	_, err = q.c.TRD.Cancel(q.author, trdapi.Cancel{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: v.RootVersion, BusinessRevision: 1,
		Reason: "r", Evidence: q.proof})
	q.want(err, trdapi.CodeRootConversionLocked)
	if err = q.convert("quote.1", 1, hash, "order.again", nil); code(err) != trdapi.CodeConversionSlotConflict {
		t.Fatalf("a second conversion: %s", code(err))
	}

	// The replacement first.
	n, version, hash = setup("quote.2")
	cmd := q.replaceCmd("quote.2", version, n, "approval.quote.2.2")
	blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandReplace, cmd.IdempotencyKey)
	go func() {
		_, err := q.c.TRD.Replace(q.author, cmd)
		replaced <- err
	}()
	if !awaitInHold(t, "the replacement waits at its action result", func() bool { return d.blocks(blocker) }, replaced) {
		release()
		return
	}
	go func() { converted <- q.convert("quote.2", 1, hash, "order.2", nil) }()
	ok = awaitInHold(t, "the conversion waits for the replacing root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, converted)
	release()
	if !ok {
		return
	}
	if err := <-replaced; err != nil {
		t.Fatalf("the replacement: %v", err)
	}
	if err := <-converted; code(err) != trdapi.CodeQuoteNotCurrent {
		t.Fatalf("a conversion behind the replacement: %s", code(err))
	}
	if v := q.getQuote("quote.2"); v.Conversion != nil || v.ActiveRevision != n {
		t.Fatalf("after the replacement %+v", v)
	}

	// Two replacements of revision 1, and an unsynchronized replace and convert.
	a := q.activated("quote.3")
	n3, _, v3 := q.revisedAndApproved("quote.3", a.RowVersion, trdapi.TargetReplace, "review.3.2", "approval.3.2")
	r, err := q.c.TRD.Revise(q.author, trdapi.Revise{IdempotencyKey: q.key(), QuoteID: "quote.3", ExpectedVersion: v3, BaseRevision: 1})
	q.must(err)
	f, err := q.freezeQuote(q.author, "quote.3", r.RowVersion, n3+1)
	q.must(err)
	_, err = q.c.TRD.SubmitReview(q.author, trdapi.SubmitReview{IdempotencyKey: q.key(), QuoteID: "quote.3", ExpectedVersion: f.RowVersion,
		BusinessRevision: n3 + 1, ReviewRequestID: "review.3.3", TargetCommand: trdapi.TargetReplace, ExpectedActiveRevision: 1})
	q.must(err)
	d3, err := q.c.TRD.DecideReview(q.reviewer, q.approval("quote.3", f.RowVersion+1, "review.3.3", f.BusinessHash, "approval.3.3"))
	q.must(err)
	got := make([]string, 2)
	var wg sync.WaitGroup
	for i, c := range []trdapi.Replace{q.replaceCmd("quote.3", d3.RowVersion, n3, "approval.3.2"), q.replaceCmd("quote.3", d3.RowVersion, n3+1,
		"approval.3.3")} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := q.c.TRD.Replace(q.author, c)
			got[i] = code(err)
		}()
	}
	wg.Wait()
	if s := slices.Sorted(slices.Values(got)); !slices.Equal(s, []string{"OK", kernel.CodeVersionConflict}) {
		t.Fatalf("two replacements: %v", got)
	}
	n4, version4, hash4 := setup("quote.4")
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := q.c.TRD.Replace(q.author, q.replaceCmd("quote.4", version4, n4, "approval.quote.4.2"))
		got[0] = code(err)
	}()
	go func() {
		defer wg.Done()
		got[1] = code(q.convert("quote.4", 1, hash4, "order.4", nil))
	}()
	wg.Wait()
	if (got[0] == "OK") == (got[1] == "OK") {
		t.Fatalf("replace and convert: %v", got)
	}
}

// SM-TRD-APPROVAL 生效后撤销登记风险暂停 (UL TRD-RiskHold): revoking the approval the active revision was
// formalized with registers a risk hold in the revocation's transaction; QuoteApprovalRevoked says
// so; the revision stays active with its history, but the reads, the lookups and the guard refuse
// its new use (APPROVAL_REVOKED) and the conversion slot cannot be taken (the database refuses it
// too); a replacement on a new approval ends it.
func TestRevocationAfterActivationRegistersARiskHold(t *testing.T) {
	q := newTrdWorld(t)
	a := q.activated("quote.1")
	r, err := q.c.TRD.RevokeApproval(q.reviewer, trdapi.RevokeApproval{IdempotencyKey: q.key(), QuoteID: "quote.1", ApprovalID: "approval.quote.1",
		ExpectedEffectivityVersion: 1, Reason: "客户信用恶化", Evidence: q.proof})
	if err != nil || r.RowVersion != a.RowVersion+1 {
		t.Fatalf("revoke %+v, %v", r, err)
	}
	ev := q.eventsOf(trdapi.EventQuoteApprovalRevoked)
	if len(ev) != 1 || ev[0].Payload["risk_hold"] != true || ev[0].Payload["held_revision"] != float64(1) || ev[0].Payload["hold_code"] !=
		trdapi.CodeApprovalRevoked {
		t.Fatalf("event %+v", ev)
	}
	v := q.getQuote("quote.1")
	if len(v.Holds) != 1 || v.Holds[0].HoldID != "approval_revoked:approval.quote.1" || v.Holds[0].BusinessRevision != 1 ||
		v.Holds[0].RegisteredBy != actorRef("human", "reviewer") || v.ActiveRevision != 1 {
		t.Fatalf("holds %+v", v.Holds)
	}
	active, err := q.c.TRD.GetActiveRevision(q.author, "quote.1")
	if err != nil || !active.Active || !active.OnHold || active.Code != trdapi.CodeApprovalRevoked || active.Check("/x") == nil {
		t.Fatalf("active %+v, %v", active, err)
	}
	if g := q.revisionGuard(trdapi.QuoteRevisionBinding{QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: a.BusinessHash}); g.Code !=
		trdapi.CodeApprovalRevoked || !g.OnHold {
		t.Fatalf("guard %+v", g)
	}
	if err = q.convert("quote.1", 1, a.BusinessHash, "order.1", nil); code(err) != trdapi.CodeApprovalRevoked {
		t.Fatalf("convert a held revision: %s", code(err))
	}
	if err = q.exec("company.A", fmt.Sprintf(`INSERT INTO brant_trd.quote_conversion_slot (tenant_id, company_id, quote_id, slot, revision,
		business_hash, order_id, taken_by_kind, taken_by_id, take_epoch, taken_at, root_version) VALUES ('%s', 'company.A', 'quote.1', 'full_v1', 1,
		'%s', 'order.x', 'human', 'clerk', 1, now(), 99)`, q.tenant, a.BusinessHash)); err == nil {
		t.Fatal("the database takes the slot of a held revision")
	}
	n, _, version := q.revisedAndApproved("quote.1", r.RowVersion, trdapi.TargetReplace, "review.2", "approval.2")
	if _, err = q.c.TRD.Replace(q.author, q.replaceCmd("quote.1", version, n, "approval.2")); err != nil {
		t.Fatalf("replace the held revision: %v", err)
	}
	if active, err = q.c.TRD.GetActiveRevision(q.author, "quote.1"); err != nil || active.Code != "" || active.Ref.BusinessRevision != n {
		t.Fatalf("after the replacement %+v, %v", active, err)
	}
}

// Python test_t03_a33_reviewer_qualification.py
// test_reviewer_revocation_blocks_replacement_without_disturbing_active_revision (PYT-0389),
// test_committed_public_replay_keeps_original_fact_after_reviewer_revocation (PYT-0390) and
// test_t03_a35_time_qualification.py test_committed_replay_remains_successful_after_expiry
// (PYT-0399, the same rule): a reviewer who lost quote.review.decide no longer releases the
// replacement (APPROVAL_STALE), the active revision stays and nothing is written; the committed
// activation replays its original receipt whatever became of the reviewer.
func TestReviewerRevocationBlocksReplacementWithoutDisturbingActiveRevision(t *testing.T) {
	q := newTrdWorld(t)
	_, d := q.approved("quote.1")
	cmd := q.activateCmd("quote.1", d.RowVersion, 1, "approval.quote.1")
	a, err := q.c.TRD.Activate(q.author, cmd)
	q.must(err)
	n, _, version := q.revisedAndApproved("quote.1", a.RowVersion, trdapi.TargetReplace, "review.2", "approval.2")
	q.regrant("reviewer", "company", without(allPermissions, "quote.review.decide"))
	_, err = q.c.TRD.Replace(q.author, q.replaceCmd("quote.1", version, n, "approval.2"))
	q.want(err, trdapi.CodeApprovalStale)
	if s := q.statuses("quote.1"); !slices.Equal(s, []string{trdapi.StatusActive, trdapi.StatusPendingApproval}) {
		t.Fatalf("statuses %v", s)
	}
	if documents, commits, numbers := q.formalRows(); documents != 1 || commits != 1 || numbers != 1 {
		t.Fatalf("%d documents, %d commit evidence, %d numbers", documents, commits, numbers)
	}
	again, err := q.c.TRD.Activate(q.author, cmd)
	if err != nil || !again.Replayed || again.ActionID != a.ActionID || again.Number != a.Number || again.DocumentSHA256 != a.DocumentSHA256 {
		t.Fatalf("replay %+v, %v", again, err)
	}
}

// Python test_t03_a33_reviewer_qualification.py
// test_revocation_committing_first_blocks_concurrent_formalization (PYT-0391), both commit orders:
// an activation holding the reviewer's grant rows FOR SHARE (it waits at its action result) makes a
// change of the reviewer's access wait until it commits; once the change committed, the next
// activation on that reviewer's approval is APPROVAL_STALE with nothing written.
func TestReviewerRevocationAndActivationDecideByCommitOrder(t *testing.T) {
	q := newTrdWorld(t)
	d := newDownstream(q.world)
	_, d1 := q.approved("quote.1")
	_, d2 := q.approved("quote.2")
	cmd := q.activateCmd("quote.1", d1.RowVersion, 1, "approval.quote.1")
	blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandActivate, cmd.IdempotencyKey)
	done := make(chan error, 1)
	go func() {
		_, err := q.c.TRD.Activate(q.author, cmd)
		done <- err
	}()
	if !awaitInHold(t, "the activation waits at its action result", func() bool { return d.blocks(blocker) }, done) {
		release()
		return
	}
	activation := blockedBy(t, blocker)[0]
	revoked := make(chan timed, 1)
	go func() {
		_, err := q.platform.Provision(context.Background(), bootstrap.Grant{Scope: q.scope("company.A"), Actor: actorRef("human", "reviewer"),
			Permissions: without(allPermissions, "quote.review.decide"), DataScope: "company", Team: "trade"})
		revoked <- timed{time.Now(), err}
	}()
	ok := awaitInHold(t, "the revocation waits on the activation", func() bool { return len(blockedBy(t, activation)) == 1 }, revoked)
	released := time.Now()
	release()
	if !ok {
		return
	}
	if err := <-done; err != nil {
		t.Fatalf("the activation: %v", err)
	}
	if r := <-revoked; r.err != nil || !r.at.After(released) {
		t.Fatalf("the revocation %v, after the activation %v", r.err, r.at.After(released))
	}
	_, err := q.c.TRD.Activate(q.author, q.activateCmd("quote.2", d2.RowVersion, 1, "approval.quote.2"))
	q.want(err, trdapi.CodeApprovalStale)
	if documents, _, numbers := q.formalRows(); documents != 1 || numbers != 1 {
		t.Fatalf("%d documents, %d numbers", documents, numbers)
	}
}

// Python test_t03_a34_ready_reviewer.py test_missing_commit_evidence_cannot_report_ready
// (PYT-0394) and TRD-INV-009 at the database: a revision becomes active only with its commit
// evidence, is cancelled only with its closure, a formal document and commit evidence never change
// and are never deleted, and the guard and the lookups know no formal revision without them.
func TestDatabaseRefusesFormalStatesWithoutTheirEvidence(t *testing.T) {
	q := newTrdWorld(t)
	q.approved("quote.1")
	q.activated("quote.2")
	for name, sql := range map[string]string{
		"active without commit evidence":   `UPDATE brant_trd.quote_revision SET status = 'active' WHERE quote_id = 'quote.1'`,
		"cancelled without a closure":      `UPDATE brant_trd.quote_revision SET status = 'cancelled' WHERE quote_id = 'quote.2'`,
		"superseded without a replacement": `UPDATE brant_trd.quote_revision SET status = 'superseded' WHERE quote_id = 'quote.2'`,
		"a document's bytes changed":       `UPDATE brant_trd.quote_document SET size_bytes = size_bytes WHERE quote_id = 'quote.2'`,
		"a document deleted":               `DELETE FROM brant_trd.quote_document WHERE quote_id = 'quote.2'`,
		"commit evidence deleted":          `DELETE FROM brant_trd.quote_commit_evidence WHERE quote_id = 'quote.2'`,
		"a document of another business hash": `INSERT INTO brant_trd.quote_document SELECT tenant_id, company_id, 'quote.1', 1, business_hash, 'QT-X-1',
			sequence_kind, sequence_id, sequence_revision, business_ref, template_kind, template_id, template_revision, template_content_hash,
			renderer_version, 'QT-X-1.txt', mime_type, content_sha256, size_bytes, content, created_at FROM brant_trd.quote_document
			WHERE quote_id = 'quote.2'`,
	} {
		if err := q.exec("company.A", sql); err == nil {
			t.Errorf("%s: the database took it", name)
		}
	}
	if v := q.revisionGuard(trdapi.QuoteRevisionBinding{QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: sha256Hex("x")}); v.Known ||
		v.Code != kernel.CodeReferenceInvalid {
		t.Fatalf("guard on a pending revision %+v", v)
	}
	found, err := q.c.TRD.ResolveQuoteRevisions(q.author, []trdapi.QuoteRevisionRef{{QuoteID: "quote.1", BusinessRevision: 1}})
	if err != nil || len(found) != 0 {
		t.Fatalf("lookup of a pending revision %+v, %v", found, err)
	}
	if _, err = q.c.TRD.GetFormalDocument(q.author, "quote.1", 1); code(err) != trdapi.CodeDocumentBindingMissing {
		t.Fatalf("the document of a pending revision: %s", code(err))
	}
}
