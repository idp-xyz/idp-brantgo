//go:build integration

package bootstrap_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The TRD QuoteRoot suite (WBS S3-01, S3-08) drives quotes through trd/api against the composed
// PTY, CAT, INQ, POL, CST and EVD on PostgreSQL 16, on the frozen snapshot of the CST suite: RFQ
// rfq.1 (L1, 120 PCS of sku.1), the 660.00 USD cost snapshot under policy.1 (USD, quotes valid 7
// days). On top of it the customer customer.1 is confirmed in a second revision with a buyer and a
// billing address, its trading profile binds the settlement terms of the fixture and pins contact.1,
// and POL publishes the company profile, a sales tax application for sku.1, a quote document
// template and a quote number sequence. The author (a human with every permission and company data
// scope) drafts and freezes, as in Python. Test names cite the Python V0.50 test they port.

type trdWorld struct {
	*scenWorld
	snap                         cstapi.SnapshotReceipt
	company, sale, template, seq polapi.ConfigRef
	proof                        []trdapi.Evidence
}

func newTrdWorld(t *testing.T) *trdWorld {
	w, f, _ := newFrozenWorld(t)
	q := &trdWorld{scenWorld: w, snap: f, proof: []trdapi.Evidence{{SourceID: "evidence", Locator: "合成核实依据"}}}
	ed, rv := w.author, w.reviewer
	_, err := w.c.PTY.ReviseParty(ed, ptyapi.ReviseParty{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: 2, Reason: "补充地址"})
	w.must(err)
	_, err = w.c.PTY.SaveParty(ed, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: 3, Name: "customer.1",
		Roles: []string{ptyapi.RoleCustomer}, Addresses: []ptyapi.Address{{AddressID: "hq", Role: ptyapi.AddressBuyer, Address: "合成买方地址\n上海"},
			{AddressID: "ap", Role: ptyapi.AddressBilling, Address: "合成账单地址"}}})
	w.must(err)
	_, err = w.c.PTY.ConfirmParty(ed, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: 4,
		EvidenceIDs: []string{"evidence"}})
	w.must(err)
	saved, err := w.saveProfile(ed, "customer.1", 0, 2, terms(w.payment), terms(w.trade), ptyapi.PinnedContact{ContactID: "contact.1", RowVersion: 2})
	w.must(err)
	_, err = w.publishProfile(rv, "customer.1", saved.RowVersion, 1)
	w.must(err)
	win := w.win
	q.company = w.configure(ed, rv, polapi.KindCompanyProfile, "company.A", 0, win.from, time.Time{}, companyData("USD"), true)
	q.sale = w.configure(ed, rv, polapi.KindTaxApplication, "tax.sale", 0, win.from, time.Time{}, polapi.ConfigData{
		TaxApplication: &polapi.TaxApplication{Name: "sale", Side: polapi.TaxSideSales, SubjectKind: polapi.SubjectSKU, SubjectID: "sku.1",
			TaxProfile: w.tax, Jurisdiction: "CN", BasisNote: "synthetic"}}, true)
	q.template = w.configure(ed, rv, polapi.KindQuoteDocumentTemplate, "quote.template", 0, win.from, time.Time{}, polapi.ConfigData{
		QuoteDocumentTemplate: &polapi.QuoteDocumentTemplate{Name: "formal", Language: polapi.LanguageZhCN,
			RendererVersion: polapi.QuoteRendererVersion, Title: "报价单", Sections: polapi.QuoteSections(), FooterNote: "合成测试，不构成要约"}}, true)
	q.seq = w.sequence(ed, rv, "quote.seq", "QT-")
	return q
}

func tref(r polapi.ConfigRef) trdapi.ConfigRef {
	return trdapi.ConfigRef{Kind: r.Kind, ID: r.ConfigID, Revision: r.Revision}
}

func trefp(r polapi.ConfigRef) *trdapi.ConfigRef {
	c := tref(r)
	return &c
}

// data is the synthetic quote of Python examples/quote.json on this world's references.
func (q *trdWorld) data() trdapi.QuoteData {
	notes := "客户可见说明：合成样例\nKeep this line break."
	return trdapi.QuoteData{IssuerRef: trefp(q.company), CustomerRef: &trdapi.PartyRef{PartyID: "customer.1", Revision: 2},
		ContactRef:          &trdapi.ContactRef{ContactID: "contact.1", RowVersion: 2},
		TradingProfileRef:   &trdapi.TradingProfileRef{PartyID: "customer.1", Revision: 1},
		CustomerAddressRefs: []trdapi.AddressRoleRef{{PartyID: "customer.1", PartyRevision: 2, AddressID: "hq"}},
		RfqRef:              &trdapi.RfqRef{RfqID: "rfq.1", RowVersion: q.rfqVersion},
		CostSnapshotRef:     &trdapi.CostSnapshotRef{SnapshotID: q.snap.SnapshotID, ResultHash: q.snap.ResultHash},
		Lines: []trdapi.Line{{LineID: "L1", RfqLineID: "L1", Sku: trdapi.SkuRef{SkuID: "sku.1", SpecRevision: q.spec}, Quantity: "120.000000",
			UnitRef: tref(q.pcs), UnitPrice: "8.0000", PriceBasisQuantity: "1", DiscountRatio: "0.00", TaxApplicationRef: tref(q.sale),
			Evidence: q.proof}},
		CurrencyRef: trefp(q.usd), PricingPolicyRef: trefp(q.policy), TradeTermsRef: trefp(q.trade), PaymentTermsRef: trefp(q.payment),
		DocumentTemplateRef: trefp(q.template), SequenceRef: trefp(q.seq), ExpiresAt: kernel.FormatTimestamp(time.Now().Add(72 * time.Hour)),
		CustomerNotes: &notes, Evidence: q.proof}
}

func (q *trdWorld) saveQuote(ctx context.Context, id string, expected int64, d trdapi.QuoteData) (trdapi.Receipt, error) {
	return q.c.TRD.SaveDraft(ctx, trdapi.SaveDraft{IdempotencyKey: q.key(), QuoteID: id, ExpectedVersion: expected, Data: d})
}

func (q *trdWorld) freezeQuote(ctx context.Context, id string, expected, working int64) (trdapi.FreezeReceipt, error) {
	return q.c.TRD.Freeze(ctx, trdapi.Freeze{IdempotencyKey: q.key(), QuoteID: id, ExpectedVersion: expected, WorkingRevision: working})
}

// drafted saves the fixture's draft as a new quote id as the author.
func (q *trdWorld) drafted(id string) trdapi.Receipt {
	q.t.Helper()
	r, err := q.saveQuote(q.author, id, 0, q.data())
	q.must(err)
	return r
}

// frozenQuote saves and freezes the fixture's draft as a new quote id as the author.
func (q *trdWorld) frozenQuote(id string) trdapi.FreezeReceipt {
	q.t.Helper()
	q.drafted(id)
	f, err := q.freezeQuote(q.author, id, 1, 1)
	q.must(err)
	return f
}

func (q *trdWorld) getQuote(id string) trdapi.QuoteView {
	q.t.Helper()
	v, err := q.c.TRD.GetQuote(q.author, id)
	q.must(err)
	return v
}

// actorOf provisions an actor of kind in company.A with every permission, company data scope and
// team trade. Provisioning changes the company's authority epoch.
func (q *trdWorld) actorOf(kind kernel.ActorKind, id string) context.Context {
	return q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kind, ID: id}, Permissions: allPermissions,
		DataScope: "company", Team: "trade"})
}

func (q *trdWorld) frozenEvents() []event {
	var out []event
	for _, e := range q.events("company.A", trdapi.Source) {
		if e.Type == trdapi.EventQuoteFrozen {
			out = append(out, e)
		}
	}
	return out
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// lockWaiter reports whether some backend waits for a lock while running a statement containing text.
func lockWaiter(t *testing.T, text string) bool {
	var found bool
	if err := pgtest.AppPool(t).QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
		WHERE wait_event_type = 'Lock' AND position($1 in query) > 0)`, text).Scan(&found); err != nil {
		t.Fatal(err)
	}
	return found
}

// Python test_t03_quote_review.py test_revise_and_discard_preserve_sealed_history (PYT-0440) and the
// S3-01 half of test_t03_write_contract.py test_public_contract_executes_all_thirteen_actions
// (PYT-0455), TRD-INV-002 positive: an AI agent drafts, a human freezes; the frozen revision holds
// the commercial document its business hash covers, QuoteFrozen is in the Outbox, the key replays;
// the frozen revision takes no save and no second freeze; revise copies it into a new number,
// discard keeps the revision, and no number is reused.
func TestQuoteDraftFreezeReviseAndDiscard(t *testing.T) {
	q := newTrdWorld(t)
	agent := q.actorOf(kernel.ActorAI, "agent.1")
	r, err := q.saveQuote(agent, "quote.1", 0, q.data())
	if err != nil || r.Status != trdapi.StatusWorking || r.RowVersion != 1 || r.ObjectID != "quote.1" {
		t.Fatalf("save %+v, %v", r, err)
	}
	r, err = q.saveQuote(agent, "quote.1", 1, q.data())
	if err != nil || r.RowVersion != 2 {
		t.Fatalf("second save %+v, %v", r, err)
	}
	_, err = q.freezeQuote(agent, "quote.1", 2, 1)
	q.want(err, kernel.CodePermissionDenied)

	cmd := trdapi.Freeze{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: 2, WorkingRevision: 1}
	f, err := q.c.TRD.Freeze(q.author, cmd)
	if err != nil || f.Status != trdapi.StatusFrozen || f.RowVersion != 3 || f.BusinessRevision != 1 || len(f.BusinessHash) != 64 ||
		f.IssuedAt == "" || f.Replayed {
		t.Fatalf("freeze %+v, %v", f, err)
	}
	v := q.getQuote("quote.1")
	rev := v.Revisions[0]
	if v.WorkingRevision != 0 || v.RootVersion != 3 || rev.Status != trdapi.StatusFrozen || rev.BusinessHash != f.BusinessHash ||
		sha256Hex(rev.Commercial) != f.BusinessHash || rev.FrozenBy != actorRef("human", "editor") ||
		!slices.Equal(rev.Contributors, []string{"agent.1"}) || *rev.Data.CustomerNotes != *q.data().CustomerNotes {
		t.Fatalf("frozen revision %+v", rev)
	}
	var doc map[string]any
	q.must(json.Unmarshal([]byte(rev.Commercial), &doc))
	data := doc["data"].(map[string]any)
	if doc["quote_id"] != "quote.1" || doc["calculation"] != nil || doc["normalization_version"] != trdapi.NormalizationVersion ||
		data["customer_notes"] != "客户可见说明：合成样例\nKeep this line break." ||
		data["lines"].([]any)[0].(map[string]any)["quantity"] != "120" || !strings.Contains(rev.Commercial, "合成买方地址\\n上海") {
		t.Fatalf("commercial document %s", rev.Commercial)
	}
	manifest := map[string]bool{}
	for _, d := range doc["dependency_manifest"].([]any) {
		m := d.(map[string]any)
		manifest[m["kind"].(string)+":"+m["id"].(string)] = true
	}
	for _, want := range []string{"rfq:rfq.1", "cost_snapshot:" + q.snap.SnapshotID, "trading_profile:customer.1", "source:evidence",
		"company_profile:company.A", "tax_application:tax.sale", "tax_profile:tax.zero", "pricing_policy:policy.1"} {
		if !manifest[want] {
			t.Errorf("manifest lacks %s: %v", want, manifest)
		}
	}
	events := q.frozenEvents()
	if len(events) != 1 || events[0].Payload["business_hash"] != f.BusinessHash || events[0].Payload["quote_id"] != "quote.1" ||
		events[0].Payload["calculation_server_owned"] != false {
		t.Fatalf("events %+v", events)
	}
	again, err := q.c.TRD.Freeze(q.author, cmd)
	if err != nil || !again.Replayed || again.ActionID != f.ActionID || again.BusinessHash != f.BusinessHash || again.BusinessRevision != 1 {
		t.Fatalf("replay %+v, %v", again, err)
	}
	_, err = q.saveQuote(agent, "quote.1", 3, q.data())
	q.want(err, kernel.CodeInvalidState)
	_, err = q.freezeQuote(q.author, "quote.1", 3, 1)
	q.want(err, kernel.CodeInvalidState)

	r, err = q.c.TRD.Revise(agent, trdapi.Revise{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: 3, BaseRevision: 1})
	if err != nil || r.RowVersion != 4 || r.Status != trdapi.StatusWorking {
		t.Fatalf("revise %+v, %v", r, err)
	}
	_, err = q.c.TRD.Revise(agent, trdapi.Revise{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: 4, BaseRevision: 1})
	q.want(err, kernel.CodeInvalidState)
	r, err = q.c.TRD.DiscardDraft(agent, trdapi.DiscardDraft{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: 4,
		WorkingRevision: 2, Reason: "客户改需求", Evidence: q.proof})
	if err != nil || r.Status != trdapi.StatusDiscarded || r.RowVersion != 5 {
		t.Fatalf("discard %+v, %v", r, err)
	}
	r, err = q.c.TRD.Revise(q.author, trdapi.Revise{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: 5, BaseRevision: 1})
	q.must(err)
	v = q.getQuote("quote.1")
	if len(v.Revisions) != 3 || v.WorkingRevision != 3 || v.Revisions[1].Status != trdapi.StatusDiscarded || v.Revisions[1].BaseRevision != 1 ||
		v.Revisions[1].DiscardReason != "客户改需求" || v.Revisions[2].BaseRevision != 1 || v.Revisions[0].BusinessHash != f.BusinessHash ||
		v.Revisions[0].Commercial != rev.Commercial || !slices.Equal(v.Revisions[2].Contributors, []string{"agent.1", "editor"}) {
		t.Fatalf("history %+v", v.Revisions)
	}
	// The new working revision freezes with its own number and hash.
	f2, err := q.freezeQuote(q.author, "quote.1", 6, 3)
	if err != nil || f2.BusinessRevision != 3 || f2.BusinessHash == f.BusinessHash {
		t.Fatalf("second freeze %+v, %v", f2, err)
	}
}

// BC-AT09 and TRD-INV-002 (标签与备注变化不改变哈希): after the freeze, the labels and the internal
// note change the root version and nothing else; the business hash and the commercial document
// stay as they were.
func TestMetadataMovesRootVersionNotBusinessHash(t *testing.T) {
	q := newTrdWorld(t)
	f := q.frozenQuote("quote.1")
	before := q.getQuote("quote.1")
	agent := q.actorOf(kernel.ActorAI, "agent.1")
	r, err := q.c.TRD.UpdateMetadata(agent, trdapi.UpdateMetadata{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: f.RowVersion,
		Labels: []string{"重点客户", "VIP"}, InternalNote: "内部备注\n不发给客户"})
	if err != nil || r.RowVersion != f.RowVersion+1 || r.Status != trdapi.StatusUpdated {
		t.Fatalf("metadata %+v, %v", r, err)
	}
	after := q.getQuote("quote.1")
	if after.RootVersion != before.RootVersion+1 || !slices.Equal(after.Labels, []string{"重点客户", "VIP"}) ||
		after.InternalNote != "内部备注\n不发给客户" || after.Revisions[0].BusinessHash != f.BusinessHash ||
		after.Revisions[0].Commercial != before.Revisions[0].Commercial || strings.Contains(after.Revisions[0].Commercial, "内部备注") {
		t.Fatalf("after %+v", after)
	}
	_, err = q.c.TRD.UpdateMetadata(agent, trdapi.UpdateMetadata{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: f.RowVersion})
	q.want(err, kernel.CodeVersionConflict)
}

// TRD-INV-002 negative (数据库禁止修改封存行; the analogue of Python test_t03_a25_order_snapshot.py
// test_database_rejects_commercial_column_changes_and_delete for the quote): the application role
// cannot change or delete a frozen revision or a root's identity, nor seal a revision whose
// commercial document does not hash to its business hash.
func TestDatabaseRefusesToChangeFrozenRevision(t *testing.T) {
	q := newTrdWorld(t)
	q.frozenQuote("quote.1")
	_, err := q.c.TRD.Revise(q.author, trdapi.Revise{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: 2, BaseRevision: 1})
	q.must(err)
	for _, sql := range []string{
		`UPDATE brant_trd.quote_revision SET commercial = commercial || ' ' WHERE revision = 1`,
		`UPDATE brant_trd.quote_revision SET business_hash = repeat('0', 64) WHERE revision = 1`,
		`UPDATE brant_trd.quote_revision SET data = '{}' WHERE revision = 1`,
		`UPDATE brant_trd.quote_revision SET status = 'working' WHERE revision = 1`,
		`DELETE FROM brant_trd.quote_revision WHERE revision = 1`,
		`DELETE FROM brant_trd.quote_root`,
		`UPDATE brant_trd.quote_root SET owner_actor_id = 'someone'`,
		`UPDATE brant_trd.quote_root SET labels = '{x}'`,
		`UPDATE brant_trd.quote_root SET control_state = 'bound', control_task_id = 't', control_authority_epoch = 1`,
		`INSERT INTO brant_trd.quote_revision SELECT tenant_id, company_id, quote_id, 9, 'frozen', NULL, data, content_hash, contributors,
			saved_by_kind, saved_by_id, saved_epoch, saved_at, business_hash, commercial, issued_at, expires_at, frozen_by_kind, frozen_by_id,
			freeze_epoch, frozen_at, 99, NULL, NULL, NULL, NULL, NULL, NULL FROM brant_trd.quote_revision WHERE revision = 1`,
		// The working revision 2 sealed with another document than its hash covers.
		`UPDATE brant_trd.quote_revision SET status = 'frozen', business_hash = (SELECT business_hash FROM brant_trd.quote_revision WHERE revision = 1),
			commercial = '{}', issued_at = now(), expires_at = now() + interval '1 day', frozen_by_kind = 'human', frozen_by_id = 'x',
			freeze_epoch = 1, frozen_at = now(), frozen_root_version = 9 WHERE revision = 2`,
	} {
		if err := q.exec("company.A", sql); err == nil {
			t.Errorf("the database accepted %s", sql)
		}
	}
	if v := q.getQuote("quote.1"); v.Revisions[0].Status != trdapi.StatusFrozen || v.Revisions[1].Status != trdapi.StatusWorking {
		t.Fatalf("revisions %+v", v.Revisions)
	}
}

// TRD-INV-001 (同一报价根至多一个 active 修订) as storage: whichever of two first activations commits
// first wins, the other cannot add a second active revision of the root. Activation itself comes
// with the review wave; the index it relies on is checked here on a copy of the table with every
// constraint and index of brant_trd.quote_revision.
func TestOneActiveRevisionPerRoot(t *testing.T) {
	ctx := context.Background()
	tx, err := pgtest.MigratorPool(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `CREATE TEMP TABLE revision_copy (LIKE brant_trd.quote_revision INCLUDING ALL) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	insert := func(revision int, status string) error {
		_, err := tx.Exec(ctx, `SAVEPOINT s`)
		if err != nil {
			return err
		}
		doc := `{"r":` + string(rune('0'+revision)) + `}`
		_, err = tx.Exec(ctx, `INSERT INTO revision_copy (tenant_id, company_id, quote_id, revision, status, data, content_hash, contributors,
			saved_by_kind, saved_by_id, saved_epoch, saved_at, business_hash, commercial, issued_at, expires_at, frozen_by_kind, frozen_by_id,
			freeze_epoch, frozen_at, frozen_root_version)
			VALUES ('t', 'c', 'q', $1, $2, '{}', repeat('0', 64), '{a}', 'human', 'a', 1, now(), encode(sha256(convert_to($3, 'UTF8')), 'hex'), $3,
			now(), now() + interval '1 day', 'human', 'h', 1, now(), $1)`, revision, status, doc)
		if err != nil {
			_, _ = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT s`)
			return err
		}
		_, err = tx.Exec(ctx, `RELEASE SAVEPOINT s`)
		return err
	}
	if err = insert(1, "active"); err != nil {
		t.Fatal(err)
	}
	if err = insert(2, "frozen"); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if err = insert(3, "active"); !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName == "" {
		t.Fatalf("a second active revision: %v", err)
	}
	var def string
	if err = tx.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'brant_trd' AND indexname = 'quote_revision_active'`).
		Scan(&def); err != nil || !strings.Contains(def, "UNIQUE") || !strings.Contains(def, "'active'") {
		t.Fatalf("index %q, %v", def, err)
	}
}

// Python test_t03_write_contract.py test_idempotency_replay_and_changed_payload_are_atomic_and_authorized
// (PYT-0456): the same key with the same arguments replays even with another expected version;
// with other arguments it is IDEMPOTENCY_PAYLOAD_CONFLICT; a lost answer reconciles as committed,
// a key never sent as not_committed.
func TestQuoteIdempotencyAndReconciliation(t *testing.T) {
	q := newTrdWorld(t)
	cmd := trdapi.SaveDraft{IdempotencyKey: "save.1", QuoteID: "quote.1", Data: q.data()}
	first, err := q.c.TRD.SaveDraft(q.author, cmd)
	q.must(err)
	cmd.ExpectedVersion = 7
	again, err := q.c.TRD.SaveDraft(q.author, cmd)
	if err != nil || !again.Replayed || again.ActionID != first.ActionID {
		t.Fatalf("replay %+v, %v", again, err)
	}
	changed := cmd
	changed.Data.Lines = slices.Clone(cmd.Data.Lines)
	changed.Data.Lines[0].UnitPrice = "9"
	_, err = q.c.TRD.SaveDraft(q.author, changed)
	q.want(err, kernel.CodeIdempotencyPayloadConflict)
	rec, err := q.c.TRD.Reconcile(q.author, cmd)
	if err != nil || rec.State != kernel.ReconcileCommitted || rec.Receipt == nil || rec.Receipt.ActionID != first.ActionID {
		t.Fatalf("reconcile %+v, %v", rec, err)
	}
	rec, err = q.c.TRD.Reconcile(q.author, trdapi.Freeze{IdempotencyKey: "never.sent", QuoteID: "quote.1", ExpectedVersion: 1, WorkingRevision: 1})
	if err != nil || rec.State != kernel.ReconcileNotCommitted {
		t.Fatalf("reconcile a key never sent %+v, %v", rec, err)
	}
}

// Python test_t03_write_contract.py test_quote_root_own_team_and_company_scope_is_enforced
// (PYT-0458): another company never sees the quote; an actor with own data scope does not see
// another's quote; a teammate with team scope does.
func TestQuoteRootScopeIsEnforced(t *testing.T) {
	q := newTrdWorld(t)
	q.drafted("quote.1")
	other := q.provision(bootstrap.Grant{Scope: q.scope("company.B"), Actor: actorRef("human", "editor"), Permissions: allPermissions,
		DataScope: "company"})
	_, err := q.c.TRD.GetQuote(other, "quote.1")
	q.want(err, kernel.CodeNotFound)
	_, err = q.freezeQuote(other, "quote.1", 1, 1)
	q.want(err, kernel.CodeNotFound)
	own := q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: actorRef("human", "outsider"), Permissions: allPermissions,
		DataScope: "own", Team: "trade"})
	_, err = q.c.TRD.GetQuote(own, "quote.1")
	q.want(err, kernel.CodeNotFound)
	_, err = q.saveQuote(own, "quote.1", 1, q.data())
	q.want(err, kernel.CodeNotFound)
	mate := q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: actorRef("human", "mate"), Permissions: allPermissions,
		DataScope: "team", Team: "trade"})
	_, err = q.c.TRD.GetQuote(mate, "quote.1")
	q.must(err)
	if n := q.count("company.B", "brant_trd.quote_root"); n != 0 {
		t.Fatalf("company.B sees %d roots", n)
	}
}

// The tactical model's humanOnlyCommands (quote.freeze, object_control.stop, object_control.release)
// refuse AI agents and the system with PERMISSION_DENIED, whatever they hold; object_control.bind
// admits the system and refuses an AI agent; drafts admit AI agents and refuse the system (Python
// test_t03_quote_review.py test_ai_cannot_decide_review_and_rollback_leaves_no_event, the freeze
// half; test_t03_write_contract.py test_schema_ai_authority_and_server_control_are_enforced).
func TestHumanOnlyQuoteCommandsRefuseAIAndSystem(t *testing.T) {
	q := newTrdWorld(t)
	agent, system := q.actorOf(kernel.ActorAI, "agent.1"), q.actorOf(kernel.ActorSystem, "tsk.1")
	q.drafted("quote.1")
	for _, ctx := range []context.Context{agent, system} {
		_, err := q.freezeQuote(ctx, "quote.1", 1, 1)
		q.want(err, kernel.CodePermissionDenied)
		_, err = q.c.TRD.StopControl(ctx, trdapi.StopControl{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedControlEpoch: 1, Reason: "接管"})
		q.want(err, kernel.CodePermissionDenied)
		_, err = q.c.TRD.ReleaseControl(ctx, trdapi.ReleaseControl{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedControlEpoch: 1, Reason: "释放"})
		q.want(err, kernel.CodePermissionDenied)
	}
	_, err := q.c.TRD.BindControl(agent, trdapi.BindControl{IdempotencyKey: q.key(), QuoteID: "quote.1", TaskID: "task.1"})
	q.want(err, kernel.CodePermissionDenied)
	_, err = q.saveQuote(system, "quote.2", 0, q.data())
	q.want(err, kernel.CodePermissionDenied)
	b, err := q.c.TRD.BindControl(system, trdapi.BindControl{IdempotencyKey: q.key(), QuoteID: "quote.1", TaskID: "task.1"})
	if err != nil || b.RowVersion != 1 || b.Status != trdapi.ControlBound {
		t.Fatalf("bind by the system %+v, %v", b, err)
	}
	if v := q.getQuote("quote.1"); v.WorkingRevision != 1 || v.RootVersion != 1 || len(q.frozenEvents()) != 0 {
		t.Fatalf("after the refusals %+v", v)
	}
}

// Python test_t03_write_contract.py test_action_result_failure_rolls_back_quote_event_and_root
// (PYT-0460) and TRD-INV-016: a failing Outbox write rolls the freeze back whole — the revision stays
// working, no succeeded audit entry — and the same key freezes once the store accepts the write.
func TestOutboxFailureRollsBackFreeze(t *testing.T) {
	q := newTrdWorld(t)
	q.drafted("quote.1")
	cmd := trdapi.Freeze{IdempotencyKey: "freeze.1", QuoteID: "quote.1", ExpectedVersion: 1, WorkingRevision: 1}
	drop := q.failOutbox(trdapi.EventQuoteFrozen)
	_, err := q.c.TRD.Freeze(q.author, cmd)
	if _, isRejection := kernel.AsRejection(err); err == nil || isRejection || !strings.Contains(err.Error(), "test storage failure") {
		t.Fatalf("freeze with a failing Outbox: %v", err)
	}
	if v := q.getQuote("quote.1"); v.RootVersion != 1 || v.Revisions[0].Status != trdapi.StatusWorking {
		t.Fatalf("after the failure %+v", v)
	}
	entries, err := q.c.TRD.Audit(q.author)
	q.must(err)
	for _, e := range entries {
		if e.Command == trdapi.CommandFreeze && e.Outcome == "succeeded" {
			t.Fatal("the rolled back freeze was audited as succeeded")
		}
	}
	drop()
	f, err := q.c.TRD.Freeze(q.author, cmd)
	if err != nil || f.Replayed || f.BusinessRevision != 1 {
		t.Fatalf("retry %+v, %v", f, err)
	}
}

// Concurrency on the root (root_version CAS under the root's FOR UPDATE lock): two first saves of
// one quote, two saves at one version and two freezes with different keys each have exactly one
// winner; two freezes with the same key commit one action and one QuoteFrozen event, the other is
// its replay (the analogue of Python test_t03_a24_write_control.py
// test_two_local_activations_with_current_task_keep_one_event, PYT-0202, without a binding).
func TestConcurrentQuoteWritesHaveOneWinner(t *testing.T) {
	q := newTrdWorld(t)
	race := func(n int, fn func(i int) error) []string {
		var wg sync.WaitGroup
		out := make([]string, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out[i] = code(fn(i))
			}()
		}
		wg.Wait()
		slices.Sort(out)
		return out
	}
	data := q.data()
	save := func(expected int64, key string) error {
		_, err := q.c.TRD.SaveDraft(q.author, trdapi.SaveDraft{IdempotencyKey: key, QuoteID: "quote.1", ExpectedVersion: expected, Data: data})
		return err
	}
	keys := []string{"race.0", "race.1"}
	got := race(2, func(i int) error { return save(0, keys[i]+".create") })
	if !slices.Equal(got, []string{"OK", kernel.CodeVersionConflict}) {
		t.Fatalf("two first saves: %v", got)
	}
	got = race(2, func(i int) error { return save(1, keys[i]+".save") })
	if !slices.Equal(got, []string{"OK", kernel.CodeVersionConflict}) {
		t.Fatalf("two saves at version 1: %v", got)
	}
	got = race(2, func(i int) error {
		_, err := q.c.TRD.Freeze(q.author, trdapi.Freeze{IdempotencyKey: keys[i] + ".freeze", QuoteID: "quote.1", ExpectedVersion: 2, WorkingRevision: 1})
		return err
	})
	if got[0] != "OK" || (got[1] != kernel.CodeVersionConflict && got[1] != kernel.CodeInvalidState) {
		t.Fatalf("two freezes with different keys: %v", got)
	}
	q.drafted("quote.2")
	cmd := trdapi.Freeze{IdempotencyKey: "same.key", QuoteID: "quote.2", ExpectedVersion: 1, WorkingRevision: 1}
	receipts := make([]trdapi.FreezeReceipt, 2)
	got = race(2, func(i int) error {
		var err error
		receipts[i], err = q.c.TRD.Freeze(q.author, cmd)
		return err
	})
	if !slices.Equal(got, []string{"OK", "OK"}) || receipts[0].ActionID != receipts[1].ActionID || receipts[0].Replayed == receipts[1].Replayed {
		t.Fatalf("two freezes with one key: %v %+v", got, receipts)
	}
	if n := len(q.frozenEvents()); n != 2 {
		t.Fatalf("%d QuoteFrozen events for two frozen quotes", n)
	}
}

// What the freeze binds must be in effect at its instant (Python test_t03_snapshot.py
// test_stale_master_row_version_rolls_back_formalization, PYT-0454, for the freeze): an older RFQ
// version, an older customer revision, a retired template, a quantity other than the RFQ's and a
// validity beyond the pricing policy's are each refused with the owner's verdict or TRD's code, and
// the revision stays working.
func TestFreezeRefusesWhatIsNotInEffect(t *testing.T) {
	q := newTrdWorld(t)
	old := q.configure(q.author, q.reviewer, polapi.KindQuoteDocumentTemplate, "quote.template.old", 0, q.win.from, time.Time{},
		polapi.ConfigData{QuoteDocumentTemplate: &polapi.QuoteDocumentTemplate{Name: "old", Language: polapi.LanguageEn,
			RendererVersion: polapi.QuoteRendererVersion, Title: "Quotation", Sections: polapi.QuoteSections()}}, true)
	_, err := q.c.POL.Retire(q.reviewer, polapi.Retire{IdempotencyKey: q.key(), Kind: old.Kind, ConfigID: old.ConfigID, ExpectedVersion: 2,
		Reason: "停用旧模板"})
	q.must(err)
	cases := []struct {
		name string
		edit func(d *trdapi.QuoteData)
		code string
		path string
	}{
		{"an older RFQ version", func(d *trdapi.QuoteData) { d.RfqRef.RowVersion = 1 }, trdapi.CodeReferenceStale, "/args/data/rfq_ref"},
		{"an older customer revision", func(d *trdapi.QuoteData) { d.CustomerRef.Revision = 1 }, trdapi.CodeReferenceStale, "/args/data/customer_ref"},
		{"a retired template", func(d *trdapi.QuoteData) { d.DocumentTemplateRef = trefp(old) }, polapi.CodeConfigNotActive, "/args/data/document_template_ref"},
		{"another quantity", func(d *trdapi.QuoteData) { d.Lines[0].Quantity = "100" }, trdapi.CodeBaseQuantityMismatch, "/args/data/lines/0/quantity"},
		{"a validity of eight days", func(d *trdapi.QuoteData) { d.ExpiresAt = kernel.FormatTimestamp(time.Now().Add(8 * 24 * time.Hour)) },
			trdapi.CodeQuoteValidityInvalid, "/args/data/expires_at"},
		{"no snapshot", func(d *trdapi.QuoteData) { d.CostSnapshotRef = nil }, trdapi.CodeCostSnapshotMissing, "/args/data/cost_snapshot_ref"},
	}
	for i, c := range cases {
		id := "quote." + string(rune('a'+i))
		d := q.data()
		c.edit(&d)
		_, err := q.saveQuote(q.author, id, 0, d)
		q.must(err)
		_, err = q.freezeQuote(q.author, id, 1, 1)
		if code(err) != c.code || (c.path != "" && path(err) != c.path) {
			t.Errorf("%s: %s at %s, want %s at %s", c.name, code(err), path(err), c.code, c.path)
		}
		if v := q.getQuote(id); v.Revisions[0].Status != trdapi.StatusWorking || v.RootVersion != 1 {
			t.Errorf("%s: %+v", c.name, v.Revisions[0])
		}
	}
	if len(q.frozenEvents()) != 0 {
		t.Fatal("a refused freeze left an event")
	}
}

// ADR-BR-004 option B for the freeze against CST (Context Map R11, SC-04 for the freeze): the freeze
// calls AdoptabilityGuard v1 after locking its root. While a freeze holds the guard (it waits at
// its own action result behind a transaction holding its key), a source denial of the snapshot waits
// for it and commits after it: the quote is frozen. Once the denial has committed, the next freeze
// on the snapshot is refused (COST_SNAPSHOT_INELIGIBLE) and its revision stays working.
func TestFreezeAdoptabilityGuardDecidesCommitOrder(t *testing.T) {
	q := newTrdWorld(t)
	d := newDownstream(q.world)
	q.drafted("quote.1")
	q.drafted("quote.2")
	counter := q.counter()
	cmd := trdapi.Freeze{IdempotencyKey: "freeze.held", QuoteID: "quote.1", ExpectedVersion: 1, WorkingRevision: 1}
	blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandFreeze, cmd.IdempotencyKey)
	frozen := make(chan error, 1)
	go func() {
		_, err := q.c.TRD.Freeze(q.author, cmd)
		frozen <- err
	}()
	if !awaitInHold(t, "the freeze holds the guard and waits at its action result", func() bool { return d.blocks(blocker) }, frozen) {
		release()
		return
	}
	denied := make(chan timed, 1)
	go func() {
		_, err := q.c.CST.AttestSource(q.reviewer, q.denyCmd("src.1", counter))
		denied <- timed{time.Now(), err}
	}()
	ok := awaitInHold(t, "the denial waits on the freeze's guard", func() bool { return lockWaiter(t, "brant_cst.source_standing") }, denied)
	released := time.Now()
	release()
	if !ok {
		return
	}
	if err := <-frozen; err != nil {
		t.Fatalf("the freeze holding the guard: %v", err)
	}
	r := <-denied
	if r.err != nil || !r.at.After(released) {
		t.Fatalf("denial %v, after the freeze %v", r.err, r.at.After(released))
	}
	if v := q.getQuote("quote.1"); v.Revisions[0].Status != trdapi.StatusFrozen {
		t.Fatalf("quote.1 %+v", v.Revisions[0])
	}
	_, err := q.freezeQuote(q.author, "quote.2", 1, 1)
	if code(err) != cstapi.CodeCostSnapshotIneligible || path(err) != "/args/data/cost_snapshot_ref" {
		t.Fatalf("a freeze after the denial: %s at %s", code(err), path(err))
	}
	if v := q.getQuote("quote.2"); v.Revisions[0].Status != trdapi.StatusWorking || len(q.frozenEvents()) != 1 {
		t.Fatalf("quote.2 %+v", v.Revisions[0])
	}
}

// A customer revised after the draft was saved (Context Map R13, 主体停用后仍转单): while the party is
// reopened as a draft, and after it is confirmed in a later revision, the freeze of a draft bound to
// the earlier revision is refused (REFERENCE_STALE at the customer) and nothing is frozen.
func TestFreezeRefusesCustomerRevisedAfterTheDraft(t *testing.T) {
	q := newTrdWorld(t)
	q.drafted("quote.1")
	q.drafted("quote.2")
	_, err := q.c.PTY.ReviseParty(q.author, ptyapi.ReviseParty{IdempotencyKey: q.key(), PartyID: "customer.1", ExpectedVersion: 5, Reason: "更名"})
	q.must(err)
	_, err = q.freezeQuote(q.author, "quote.1", 1, 1)
	if code(err) != trdapi.CodeReferenceStale || path(err) != "/args/data/customer_ref" {
		t.Fatalf("a revised customer: %s at %s", code(err), path(err))
	}
	_, err = q.c.PTY.SaveParty(q.author, ptyapi.SaveParty{IdempotencyKey: q.key(), PartyID: "customer.1", ExpectedVersion: 6, Name: "customer.1",
		Roles: []string{ptyapi.RoleCustomer}, Addresses: []ptyapi.Address{{AddressID: "hq", Role: ptyapi.AddressBuyer, Address: "新地址"}}})
	q.must(err)
	_, err = q.c.PTY.ConfirmParty(q.author, ptyapi.ConfirmParty{IdempotencyKey: q.key(), PartyID: "customer.1", ExpectedVersion: 7,
		EvidenceIDs: []string{"evidence"}})
	q.must(err)
	_, err = q.freezeQuote(q.author, "quote.2", 1, 1)
	if code(err) != trdapi.CodeReferenceStale || path(err) != "/args/data/customer_ref" {
		t.Fatalf("a customer in a later revision: %s at %s", code(err), path(err))
	}
	if len(q.frozenEvents()) != 0 {
		t.Fatal("nothing is frozen")
	}
}

// blockedBy returns the backends that wait for a lock pid holds.
func blockedBy(t *testing.T, pid int32) []int32 {
	rows, err := pgtest.AppPool(t).Query(context.Background(), `SELECT pid FROM pg_stat_activity WHERE $1 = ANY (pg_blocking_pids(pid))
		ORDER BY pid`, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int32
	for rows.Next() {
		var p int32
		if err = rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// ADR-BR-004 option B for the freeze against PTY and CAT (Context Map R13, R14, guard locks): the
// freeze holds TradingProfileGuard v1, AddressRoleGuard v1 and SkuGuard v1 until it commits, so a
// revision of the customer and a revision of the SKU's specification sent while it is held both
// wait for it and commit after it; the next freeze is refused with their verdicts.
func TestFreezeHoldsPartyAndSkuGuards(t *testing.T) {
	q := newTrdWorld(t)
	d := newDownstream(q.world)
	q.drafted("quote.1")
	q.drafted("quote.2")
	cmd := trdapi.Freeze{IdempotencyKey: "freeze.held", QuoteID: "quote.1", ExpectedVersion: 1, WorkingRevision: 1}
	blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandFreeze, cmd.IdempotencyKey)
	frozen := make(chan error, 1)
	go func() {
		_, err := q.c.TRD.Freeze(q.author, cmd)
		frozen <- err
	}()
	if !awaitInHold(t, "the freeze holds its guards and waits at its action result", func() bool { return d.blocks(blocker) }, frozen) {
		release()
		return
	}
	freeze := blockedBy(t, blocker)[0]
	changed := make(chan timed, 2)
	partyKey, specKey := q.key(), q.key()
	go func() {
		err := q.reviseParty(q.author, "customer.1", 5, partyKey)
		changed <- timed{time.Now(), err}
	}()
	go func() {
		err := q.reviseSpec(q.author, "sku.1", 2, specKey)
		changed <- timed{time.Now(), err}
	}()
	ok := awaitInHold(t, "both upstream revisions wait on the freeze", func() bool { return len(blockedBy(t, freeze)) == 2 }, changed)
	released := time.Now()
	release()
	if !ok {
		return
	}
	if err := <-frozen; err != nil {
		t.Fatalf("the freeze holding the guards: %v", err)
	}
	for range 2 {
		if r := <-changed; r.err != nil || !r.at.After(released) {
			t.Fatalf("an upstream revision %v, after the freeze %v", r.err, r.at.After(released))
		}
	}
	_, err := q.freezeQuote(q.author, "quote.2", 1, 1)
	if code(err) != trdapi.CodeReferenceStale {
		t.Fatalf("a freeze after the revisions: %s at %s", code(err), path(err))
	}
}
