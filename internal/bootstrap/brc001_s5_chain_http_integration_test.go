//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S5 acceptance chain D1 (S5 plan §5 "End-to-end acceptance chain (D1): BRC-001 from order to
// settlement, write-off and China materials"; event storming ES-01 continued by WF-08, WF-02, WF-07
// and WF-05; Context Map CN-M01 manual paths): BRC-001 from the active order through purchase
// commitment, quality release, supplier delivery, shipment, documents, export lot and declaration to
// the sales and purchase settlements, the write-off, the China materials, the order's end and its
// operating result, end to end on PostgreSQL 16 over the HTTP API, every command by the human the
// step names with its own OIDC access token of the test's local issuer.
//
// It first runs the S3-12 chain (TestBRC001FullChainOverHTTP's steps, toPurchaseDraft): ES-01 steps
// 1–19 on the four lines ST01 red/blue × S/M of 24 + 36 + 12 + 48 = 120 PCS, supplier.A at 5.00 USD,
// cost 660.00, quote QT-2026-00001 at 8.00 = 960.00 USD, payments P1 30 % order_confirmed 288.00 and
// P2 70 % after_shipment 30 days 672.00, FCA, and the purchase draft of 600.00 USD. Since unit B4 that
// chain holds §5 steps 1–6: the E-2 credit policy, customer.1's limit of 10,000.00 USD (two credit
// managers above the top tier, STL-INV-005), the reservation of the 672.00 (D-7), the four CAT
// declaration profiles (doc.1 drafts, doc.2 verifies, CAT-INV-002), CNX's clear trade compliance
// conclusion and the activation whose commit evidence names both guards. Then, steps 7–27:
//
//   - The events: brant-worker -dispatch in process — the assembled relay (Platform.Relay) publishing
//     to the dispatcher of every composed context's subscriptions (Contexts.Subscriptions), over a
//     throwaway Bento outbox into which the platform's Outbox events of the chain's tenant are copied
//     unchanged, oldest first (as TestTRDOrderEventsReachSTLThroughTheRelay). OrderActivated opens
//     the order account (stl.order_account) and consumes the reservation (stl.order_credit, PL-12);
//     OrderClosed releases it (PL-13). Every delivered event is delivered again by a second
//     dispatcher: nothing changes (Inbox dedup by event ID), and each consumer's Inbox holds it once,
//     processed.
//   - 7 (WF-02, STL-INV-002): treasury records customer.1's 288.00 and applies it to P1: the
//     shipment's payment condition is satisfied.
//   - 8 (A47, PUR-INV-009/010): purchaser requests commitment.1 of purchase.1, order.reviewer approves
//     it (recomputed equal, 600.00, valid until the supply condition ends), purchaser issues it
//     (PO-2026-00001) and records supplier.A's confirmation as issued; voiding the draft is now
//     PURCHASE_DRAFT_COMMITTED (TD-AT06).
//   - 9 (QLT-INV-001, F-06): qc records the external agency's final inspection report of each
//     commitment line (24, 36, 12, 48 inspected, all conforming) and grants quality releases of 120
//     PCS in all.
//   - 10 (CN-R07): merchandiser plans the order's milestones and records the cargo ready
//     (packing_complete) as internal_verified on evidence.
//   - 11 (TD-AT14, PUR-INV-008): merchandiser records supplier.A's handover of the 120 PCS at CNNGB on
//     the forwarder's cargo receipt; purchaser verifies all of it.
//   - 12 (FUL-INV-001, BT11): merchandiser prepares shipment.1 of the four lines from the verified
//     delivery lines and the quality releases, plans and records its packing (10 cartons of 12) and
//     records the booking with provider.1, the buyer's nominated forwarder under FCA.
//   - 13 (WF-07, FUL-INV-002): doc.1 evaluates the shipment's commercial release: granted, binding QLT
//     120, PTY clear, PUR verified 120, CNX clear (no licence), STL P1 288.00 applied, TRD active.
//   - 14: merchandiser confirms shipment.1 (120 ≤ 120).
//   - 15 (FUL-INV-004, BT12): doc.1 prepares the commercial invoice (960.00 USD), the packing list and
//     the bill of lading record, reads the reconciliation (0 differences) and issues them.
//   - 16 (D-5): doc.1 evaluates the documents release (P2 is open account on credit, not document
//     controlled: granted) and records the original B/L's release.
//   - 17 (R36): merchandiser registers export lot lot.1 of the 120 PCS, 960.00 USD.
//   - 18 (CN-R05, CN-R07, CNX-INV-003/006): doc.1 prepares the declaration of lot.1 on the verified
//     profiles, confirms it, records the manual submission and the customs receipt (released, the
//     original text and mapping v1, the export date): states kept apart.
//   - 19 (STL-INV-001, D-4): treasury confirms the sales statement of the four shipment lines: the
//     receivable is 960.00, P1's application moves to it (open 672.00), the credit exposure stays
//     672.00 (no double count).
//   - 20 (CN-AT07, CN-R10, STL-INV-006): treasury records the customer's 672.00 remitted, 670.00
//     credited; applying 672.00 is FUND_DIFFERENCE_UNEXPLAINED; credit.manager.1 resolves the 2.00 as
//     our bank charge; treasury applies 670.00 + 2.00: the receivable is 0, the exposure 0.
//   - 21: treasury confirms the purchase statement of the verified delivery (120 × 5.00 = 600.00),
//     records the payment of 600.00 to supplier.A and applies it: the payable is 0.
//   - 22 (CNX-INV-005, CN-AT18): purchaser registers the scan of supplier.A's synthetic VAT special
//     invoice; tax.officer imports it and verifies it, matching the four commitment lines (120).
//   - 23 (CNX-INV-004, CN-AT10/12): tax.officer allocates the invoice line to the four export lines of
//     lot.1 for export_refund under the policy in force at the export date; the case states no amount
//     of its own; the officer records the expected result with its basis.
//   - 24 (CNX-INV-008, CN-AT14): doc.1 checks the document bundle of lot.1 (CI, PL, B/L, declaration
//     receipt, invoice: complete); tax.officer resolves the external reconciliation of the declared
//     value against the CI value.
//   - 25 (TRD-INV-019, PL-13): merchandiser closes order.1 (shipped 120 = 120); the consumer releases
//     the reservation: exposure 0.
//   - 26 (STL-INV-010): credit.officer records provider.1's freight of 60.00 USD as estimated,
//     treasury confirms it on the forwarder's bill, credit.officer allocates it to the order.
//   - 27 (STL-INV-011, BT18): treasury reviews and closes the order result: confirmed 960.00 − 600.00 −
//     60.00 = 300.00 (31.25 %), equal to the quote's calculation; CLM recorded not_enabled.
//
// Final assertions (§5): order 960.00; reservation 672.00 reserved → consumed → released; shipped
// 120; CI 960.00; export lot 960.00; receivable 960.00 → 0 (288.00 + 670.00 + 2.00 approved charge);
// payable 600.00 → 0; no unchecked guard in any commit evidence; every event processed once; the
// order result settled at 300.00. The chain's steps are deterministic: two runs differ only in IDs
// the server generates, hashes over them and instants.
//
// What is HTTP and what is not: every business command is POST /api/brant/v1/{bc}/commands/{command}
// with the closed envelope and every read a GET route, each with the acting human's token. Fixtures
// (no business HTTP route): the tenant, the humans' grants and role templates, the Inbox actors of
// the consumers (Platform.ProvisionInbox, as brant-admin inbox), all before the first command; the
// in-process relay and dispatcher. With BRANT_BRC001_S5_REPORT set to a file path the test writes the
// chain record as JSON (the S3 record's shape plus the dispatches).
func TestBRC001OrderToSettlementOverHTTP(t *testing.T) {
	c := newBRC001S5(t)
	c.toPurchaseDraft()
	c.configurations()
	c.dispatch("after the activation (S3 chain, §5 step 6)")
	c.activationConsumed()
	c.receiptP1()        // 7
	c.commitment()       // 8
	c.quality()          // 9
	c.milestones()       // 10
	c.delivery()         // 11
	c.shipment()         // 12
	c.shipmentRelease()  // 13
	c.confirm()          // 14
	c.documents()        // 15
	c.documentsRelease() // 16
	c.exportLot()        // 17
	c.declaration()      // 18
	c.salesStatement()   // 19
	c.collection()       // 20
	c.payable()          // 21
	c.invoice()          // 22
	c.taxCase()          // 23
	c.bundle()           // 24
	c.closeOrder()       // 25
	c.dispatch("after order.close (§5 step 25)")
	c.reservationReleased()
	c.charge() // 26
	c.result() // 27
	c.dispatch("at the end of the chain")
	c.finalAssertions()
}

// The S5 chain's humans besides the S3 chain's (role templates of the archive's role catalogue).
const (
	brcMerchandiser  = "merchandiser"   // 跟单 (merchandiser)
	brcQC            = "qc"             // 质检 (quality_inspector)
	brcTreasury      = "treasury"       // 资金 (treasury)
	brcTaxOfficer    = "tax.officer"    // 税务专员 (tax_specialist)
	brcCreditOfficer = "credit.officer" // 信用专员 (credit_officer)
)

// brcDispatch is one run of the in-process relay as the record shows it.
type brcDispatch struct {
	When        string         `json:"when"`
	AfterStep   int            `json:"after_step"`
	Events      int            `json:"events"`
	ByType      map[string]int `json:"by_type"`
	Processed   map[string]int `json:"processed"` // "consumer ← event type": the Inbox records, each processed once
	Redelivered int            `json:"redelivered"`
	Facts       map[string]any `json:"facts,omitempty"`
}

// brcS5 is the S5 chain: the S3 chain's driver, the relay into the composed contexts' consumers and
// what the S5 steps returned.
type brcS5 struct {
	*brc001
	ctx       context.Context
	subs      []messaging.Subscription
	events    *orderEvents
	second    *messaging.Dispatcher
	delivered []eventing.Envelope
	runs      []brcDispatch

	bank, ciEn, plEn, ciSeq, plSeq, scTemplate, scSeq map[string]any
	draftHash, poNumber, shipmentID, documentSet      string
	shipmentVersion                                   int64
	releaseID, documentsReleaseID, declarationID      string
	declarationNumber, exportedAt                     string
	invoiceID, taxCaseID, bundleID, salesContract     string
}

func newBRC001S5(t *testing.T) *brcS5 {
	b := newBRC001(t)
	b.report = "BRANT_BRC001_S5_REPORT"
	b.record.WBS = "S5 D1 (S5 plan §5; S5-01..S5-05, S5-07, S5-08, S5-12, S5-13, S5-15, S5-17)"
	b.record.Parity = "BC-AT01 continued: BRC-001 order → settlement, write-off and China materials"
	b.record.NotExecuted = []string{
		"sending the purchase order and the supplier's confirmation through a channel: purchase.commitment.issue and " +
			"purchase.supplier_confirmation.record record the human sending and receiving as facts on EVD evidence",
		"the external inspection agency's system: qc records the agency's report (an EVD file) as a fact",
		"the carrier's and the forwarder's systems: the booking, the B/L record and the forwarder's bill are entered from EVD evidence",
		"China customs (single window): the declaration's submission and receipt are recorded manually (CN-M01), " +
			"the receipt's original text is synthetic",
		"the bank: fund records are entered by treasury from synthetic bank advices",
		"the tax authority: the VAT special invoice is a synthetic scan imported and verified by tax.officer; no refund is " +
			"submitted, approved or received, the case stays expected",
		"CLM: the order result records claims not_enabled (S5 has no claims context)",
		"the Outbox relay to an external sink: the relay publishes to the in-process dispatcher (brant-worker -dispatch)",
		"a real identity provider",
	}
	b.record.Fixtures = append(b.record.Fixtures,
		brcFixture{What: "role templates of merchandiser (merchandiser), qc (quality_inspector), treasury (treasury), tax.officer " +
			"(tax_specialist) and credit.officer (credit_officer)", How: "bootstrap.Platform.AssignRoles with company data scope",
			Why: "operator-only; no business HTTP route"},
		brcFixture{What: "number.issue for doc.1 besides the documentation template",
			How: "bootstrap.Platform.Provision with the documentation template's permissions plus number.issue",
			Why: "POL NumberIssue v1 numbers the commercial invoice and the packing list as the acting human; the documentation template lacks number.issue (C2's reported gap)"},
		brcFixture{What: "the Inbox actors of every consumer the composed contexts subscribe",
			How: "bootstrap.Platform.ProvisionInbox (as brant-admin inbox)", Why: "operator-only; a consumer's commands run as system:inbox.<consumer>"},
		brcFixture{What: "the event relay", How: "bootstrap.Platform.Relay publishing to messaging.Dispatcher of Contexts.Subscriptions (brant-worker " +
			"-dispatch in process) over a throwaway Bento outbox fed with the platform Outbox's events of the tenant, unchanged",
			Why: "a relay on the shared bento.outbox would also claim other tests' events"})
	for actor, role := range map[string]string{brcMerchandiser: "跟单", brcQC: "质检", brcTreasury: "资金", brcTaxOfficer: "税务专员",
		brcCreditOfficer: "信用专员"} {
		b.record.Actors[actor] = role
	}
	b.w.assign(brcMerchandiser, "merchandiser")
	b.w.assign(brcQC, "quality_inspector")
	b.w.assign(brcTreasury, "treasury")
	b.w.assign(brcTaxOfficer, "tax_specialist")
	b.w.assign(brcCreditOfficer, "credit_officer")
	tpl, ok := authorization.TemplateByCode("documentation")
	if !ok {
		t.Fatal("no documentation role template")
	}
	b.w.provision(bootstrap.Grant{Scope: b.w.scope("company.A"), Actor: actorRef("human", brcDocumentation),
		Permissions: append(slices.Clone(tpl.Permissions), "number.issue"), DataScope: "company"})
	c := &brcS5{brc001: b, ctx: context.Background()}
	c.subs = b.w.c.Subscriptions()
	c.events = newOrderEvents(t, c.subs...)
	var err error
	if c.second, err = messaging.NewDispatcher(c.subs...); err != nil {
		t.Fatal(err)
	}
	if _, err = b.w.platform.ProvisionInbox(c.ctx, b.w.scope("company.A"), c.subs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.record.Dispatches = c.runs })
	return c
}

// step names the phase of §5 step n.
func (c *brcS5) step(n int, what string) { c.phase = fmt.Sprintf("§5 step %d: %s", n, what) }

// cfgRef is a POL reference as the commands take it.
func cfgRef(kind, id string, revision int) map[string]any {
	return map[string]any{"kind": kind, "id": id, "revision": revision}
}

// usdRef is USD revision 1 as CNX's and STL's currency members take it.
func (c *brcS5) usdRef() map[string]any { return map[string]any{"id": "USD", "revision": 1} }

// now and today are the instant and the UTC day a step states its facts at: each step's own wall
// time, so the stated instants follow the order of the steps.
func now() string { return kernel.FormatTimestamp(time.Now()) }

func today() string { return time.Now().UTC().Format(time.DateOnly) }

// fileOf has actor register EVD file id with text as its content.
func (c *brcS5) fileOf(actor, id, name, text string) {
	c.cmd(actor, "evd", evdapi.Contract, "file.register", map[string]any{"id": id, "file_name": name, "mime_type": evdapi.MimeText,
		"content_base64": base64.StdEncoding.EncodeToString([]byte(text))})
}

// copyAll enqueues into the throwaway outbox, oldest first, every Outbox event of the chain's
// tenant and company not copied before, unchanged.
func (c *brcS5) copyAll() []eventing.Envelope {
	c.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(c.t))
	c.w.must(err)
	q, err := db.ReadExecutor(c.ctx)
	c.w.must(err)
	scope := c.w.scope("company.A")
	rows, err := q.Query(c.ctx, `SELECT source, event_id, event_type, event_version, spec_version, subject, partition_key, occurred_at,
		recorded_at, coalesce(correlation_id, ''), coalesce(causation_id, ''), coalesce(traceparent, ''), coalesce(tracestate, ''), content_type, payload
		FROM bento.outbox WHERE scope = $1 ORDER BY enqueued_seq`, scope.String())
	c.w.must(err)
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (eventing.Envelope, error) {
		env := eventing.Envelope{Scope: scope.String()}
		var version int64
		var payload []byte
		err := row.Scan(&env.Source, &env.ID, &env.Type, &version, &env.SpecVersion, &env.Subject, &env.PartitionKey, &env.OccurredAt, &env.RecordedAt,
			&env.CorrelationID, &env.CausationID, &env.TraceParent, &env.TraceState, &env.ContentType, &payload)
		env.Version, env.Payload = eventing.EventVersion(version), json.RawMessage(payload)
		return env, err
	})
	c.w.must(err)
	var out []eventing.Envelope
	for _, env := range all {
		if !c.events.copied[env.ID] {
			c.events.copied[env.ID] = true
			out = append(out, env)
		}
	}
	c.events.enqueue(out...)
	return out
}

// outboxCount is the number of the chain's events in the platform's Outbox (other tests' events in
// the shared Outbox are not counted).
func (c *brcS5) outboxCount() int {
	c.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(c.t))
	c.w.must(err)
	q, err := db.ReadExecutor(c.ctx)
	c.w.must(err)
	var n int
	c.w.must(q.QueryRow(c.ctx, `SELECT count(*) FROM bento.outbox WHERE scope = $1`, c.w.scope("company.A").String()).Scan(&n))
	return n
}

// inbox returns, for the events, every Inbox record as "consumer ← event type" with its status.
func (c *brcS5) inbox(envs []eventing.Envelope) map[eventing.EventID][]string {
	c.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(c.t))
	c.w.must(err)
	q, err := db.ReadExecutor(c.ctx)
	c.w.must(err)
	ids := make([]string, len(envs))
	for i, e := range envs {
		ids[i] = string(e.ID)
	}
	rows, err := q.Query(c.ctx, `SELECT event_id, consumer, status FROM bento.inbox WHERE event_id = ANY($1) ORDER BY consumer`, ids)
	c.w.must(err)
	out := map[eventing.EventID][]string{}
	for rows.Next() {
		var id, consumer, status string
		c.w.must(rows.Scan(&id, &consumer, &status))
		out[eventing.EventID(id)] = append(out[eventing.EventID(id)], consumer+"="+status)
	}
	c.w.must(rows.Err())
	return out
}

// dispatch relays every new event of the tenant to the composed contexts' consumers until none is
// left, requires each published and each consumer's Inbox record processed, then delivers every one
// again through a second dispatcher and requires that nothing changed (no new Outbox event, the same
// Inbox records).
func (c *brcS5) dispatch(when string) {
	c.t.Helper()
	envs := c.copyAll()
	c.events.drain(c.events.relay(5))
	c.events.published(envs...)
	subscribed := map[eventing.EventType][]string{}
	for _, s := range c.subs {
		for _, typ := range s.EventTypes {
			subscribed[typ] = append(subscribed[typ], string(s.Consumer))
		}
	}
	records := c.inbox(envs)
	d := brcDispatch{When: when, AfterStep: len(c.record.Steps), Events: len(envs), ByType: map[string]int{}, Processed: map[string]int{}}
	for _, e := range envs {
		d.ByType[string(e.Type)]++
		want := slices.Compact(slices.Sorted(slices.Values(subscribed[e.Type])))
		var got []string
		for _, r := range records[e.ID] {
			consumer, status, _ := strings.Cut(r, "=")
			if status != "PROCESSED" {
				c.t.Fatalf("%s %s: consumer %s left it %s", e.Type, e.ID, consumer, status)
			}
			got = append(got, consumer)
			d.Processed[consumer+" ← "+string(e.Type)]++
		}
		if !slices.Equal(got, want) {
			c.t.Fatalf("%s %s: Inbox records of %v, want one of each of %v", e.Type, e.ID, got, want)
		}
	}
	before := c.outboxCount()
	for _, e := range envs {
		if err := c.second.Publish(c.ctx, e); err != nil {
			c.t.Fatalf("redelivery of %s %s: %v", e.Type, e.ID, err)
		}
	}
	if after, again := c.outboxCount(), c.inbox(envs); after != before || fmt.Sprint(again) != fmt.Sprint(records) {
		c.t.Fatalf("a redelivery changed something: Outbox %d → %d, Inbox %v → %v", before, after, records, again)
	}
	d.Redelivered = len(envs)
	c.delivered = append(c.delivered, envs...)
	c.runs = append(c.runs, d)
}

// dispatchFact adds key-value pairs to the last dispatch's facts.
func (c *brcS5) dispatchFact(kv ...any) {
	d := &c.runs[len(c.runs)-1]
	if d.Facts == nil {
		d.Facts = map[string]any{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		d.Facts[kv[i].(string)] = kv[i+1]
	}
}

// reservation reads res.order.1.
func (c *brcS5) reservation() map[string]any {
	return c.get(brcSales, "/api/brant/v1/stl/credit_reservations/res.order.1")
}

// exposure reads customer.1's credit exposure in minor units.
func (c *brcS5) exposure(actor string) string {
	v := c.get(actor, "/api/brant/v1/stl/customers/customer.1/credit")
	c.fact("exposure_currency", v["exposure_currency"], "exposure_minor", v["exposure_minor"])
	return brcStr(v["exposure_minor"])
}

// activationConsumed is §5 step 6's consumer half: OrderActivated opened order.1's account and
// consumed res.order.1 as system:inbox.stl.order_credit (PL-12).
func (c *brcS5) activationConsumed() {
	c.step(6, "STL consumers of OrderActivated")
	v := c.reservation()
	if v["status"] != stlapi.ReservationConsumed || v["portion_minor"] != "67200" || brcDig(v, "consumed_by", "id") != "inbox.stl.order_credit" {
		c.t.Fatalf("reservation after the activation: %v", v)
	}
	c.fact("status", v["status"], "version", v["version"], "portion_minor", v["portion_minor"], "consumed_by", v["consumed_by"])
	acc := c.get(brcTreasury, "/api/brant/v1/stl/order_accounts/order.1")
	if acc["order_revision"] != float64(1) || len(brcList(acc["stages"])) != 2 {
		c.t.Fatalf("order account %v", acc)
	}
	c.fact("order_revision", acc["order_revision"], "stages", len(brcList(acc["stages"])), "conditions", acc["conditions"])
	c.dispatchFact("reservation", v["status"], "order_account", "open")
}

// configurations publishes the POL configurations the S5 steps cite, drafted by admin and
// published by config.reviewer as in the S3 chain's foundation, before the first S5 step (the export
// tax policy must be in force at the lot's export date, CN-AT12): the company's USD bank account,
// the purchase order, commercial invoice and packing list number sequences, the English
// shipment-text/0.1.0 templates of the commercial invoice and the packing list, the export tax
// policy with its professional review, and the order-text/0.1.0 sales contract template with its
// number sequence (the S/C the declaration refers to).
func (c *brcS5) configurations() {
	c.step(1, "POL configurations of the S5 steps")
	c.bank = c.config(polapi.KindBankAccount, "bank.usd", map[string]any{"owner_kind": polapi.OwnerCompany, "owner_id": "company.A",
		"bank_name": "synthetic bank", "account_number": "6222000011112222", "currency": "USD", "purpose": polapi.AccountBoth})
	c.config("number_sequence", "po.seq", map[string]any{"document_type": polapi.DocumentTypePurchaseOrder, "prefix": "PO-", "width": 5,
		"reset": polapi.ResetAnnual, "time_zone": "Asia/Shanghai"})
	template := func(id, kind, title string) map[string]any {
		sections, _ := polapi.ShipmentDocumentSections(kind)
		for _, s := range []string{"consignee", "marks"} {
			if !slices.Contains(sections, s) {
				sections = append(slices.Clone(sections), s)
			}
		}
		return c.config(polapi.KindShipmentDocumentTemplate, id, map[string]any{"name": id, "document_kind": kind, "language": polapi.LanguageEn,
			"renderer_version": polapi.ShipmentRendererVersion, "title": title, "sections": sections,
			"footer_note": "合成测试单证，非真实贸易"})
	}
	c.ciEn = template("ship.ci.en", polapi.ShipmentDocumentCommercialInvoice, "COMMERCIAL INVOICE")
	c.plEn = template("ship.pl.en", polapi.ShipmentDocumentPackingList, "PACKING LIST")
	sequence := func(id, documentType, prefix string) map[string]any {
		return c.config("number_sequence", id, map[string]any{"document_type": documentType, "prefix": prefix, "width": 5, "reset": polapi.ResetAnnual,
			"time_zone": "Asia/Shanghai"})
	}
	c.ciSeq, c.plSeq = sequence("ci.seq", polapi.DocumentTypeInvoice, "CI-"), sequence("pl.seq", polapi.DocumentTypePackingList, "PL-")
	c.source(brcAdmin, "tax.review", "合成专业复核记录：出口退税方式按合成政策“免抵退”登记，仅供测试，不构成税务意见。")
	// The publication cites the professional review the policy names (POL), besides the
	// configuration evidence.
	c.cmd(brcAdmin, "pol", polapi.Contract, "config.save_draft", map[string]any{"kind": polapi.KindExportTaxPolicy, "id": "export.tax",
		"expected_version": 0, "valid_from": kernel.FormatTimestamp(c.from), "data": map[string]any{"name": "synthetic export tax policy",
			"refund_method": polapi.RefundExemptCreditRefund, "policy_basis": "合成政策依据（测试用）", "professional_review_evidence_id": "tax.review"}})
	c.cmd(brcController, "pol", polapi.Contract, "config.publish", map[string]any{"kind": polapi.KindExportTaxPolicy, "id": "export.tax",
		"expected_version": 1, "revision": 1, "evidence_ids": []string{"evidence", "tax.review"}, "reason": "核实合成出口退税政策配置"})
	sections, _ := polapi.OrderDocumentSections(polapi.OrderDocumentSalesContract)
	c.scTemplate = c.config(polapi.KindOrderDocumentTemplate, "order.sc", map[string]any{"name": "order.sc",
		"document_kind": polapi.OrderDocumentSalesContract, "language": polapi.LanguageEn, "renderer_version": polapi.OrderRendererVersion,
		"title": "SALES CONTRACT", "sections": sections, "clauses": []string{"Quality as per the approved sample."},
		"footer_note": "合成测试，非真实合同"})
	c.scSeq = c.config("number_sequence", "order.seq", map[string]any{"document_type": polapi.DocumentTypeOrder, "prefix": "SC-", "width": 5,
		"reset": polapi.ResetAnnual, "time_zone": "Asia/Shanghai"})
}

// receiptP1 is §5 step 7: the customer's 288.00 recorded on the company's USD account and applied
// to P1; the shipment's payment condition is satisfied.
func (c *brcS5) receiptP1() {
	c.step(7, "STL receipt of P1 and its application")
	c.source(brcDocumentation, "bank.p1", "合成银行收款通知：customer.1 汇入 288.00 USD（P1 预付 30%），MT103-BRC-P1。")
	r := c.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, map[string]any{"fund_record_id": "fund.p1", "kind": stlapi.FundReceipt,
		"counterparty_id": "customer.1", "currency": cfgRef("currency", "USD", 1), "bank_account": c.bank, "received_amount": "288.00",
		"remitted_amount": "288.00", "value_date": now(), "bank_reference": "MT103-BRC-P1", "evidence_ids": []string{"bank.p1"}})
	if r["status"] != "recorded" {
		c.t.Fatalf("fund.record %v", r)
	}
	r = c.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, map[string]any{"application_id": "app.p1", "fund_record_id": "fund.p1",
		"target": map[string]any{"kind": stlapi.TargetOrderStage, "order_id": "order.1", "stage_id": "P1"}, "amount": "288.00"})
	if r["status"] != "applied" {
		c.t.Fatalf("application.apply %v", r)
	}
	acc := c.get(brcTreasury, "/api/brant/v1/stl/order_accounts/order.1")
	conditions := brcList(acc["conditions"])
	if len(conditions) != 2 || brcDig(conditions, 0, "scope") != stlapi.ScopeShipment || brcDig(conditions, 0, "status") != "satisfied" ||
		brcDig(acc, "stages", 0, "applied_minor") != "28800" {
		c.t.Fatalf("order account after P1 %v", acc)
	}
	c.fact("stage_P1_applied_minor", brcDig(acc, "stages", 0, "applied_minor"), "conditions", conditions)
	c.record.Amounts["receipt_p1"] = brcMajor(c.t, "28800") + " USD"
}

// commitment is §5 step 8 (A47 #1–#8, PUR-INV-009/010).
func (c *brcS5) commitment() {
	c.step(8, "PUR purchase commitment")
	d := c.get(brcPurchaser, "/api/brant/v1/pur/purchase_drafts/purchase.1")
	c.draftHash = brcStr(d["draft_hash"])
	r := c.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandRequestCommitment, map[string]any{"commitment_id": "commitment.1",
		"draft_id": "purchase.1", "draft_hash": c.draftHash})
	if r["status"] != purapi.CommitmentRequested {
		c.t.Fatalf("request %v", r)
	}
	r = c.cmd(brcOrderReviewer, "pur", purapi.Contract, purapi.CommandDecideCommitment, map[string]any{"commitment_id": "commitment.1",
		"expected_version": 1, "draft_hash": c.draftHash, "decision": purapi.DecisionApprove, "valid_until": kernel.FormatTimestamp(c.until)})
	if r["status"] != purapi.CommitmentApproved {
		c.t.Fatalf("decide %v", r)
	}
	c.source(brcPurchaser, "po.sent", "合成采购订单发送记录：commitment.1 经邮件发给 supplier.A，ST01 四个规格共 120 PCS，600.00 USD。")
	r = c.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandIssueCommitment, map[string]any{"commitment_id": "commitment.1",
		"expected_version": 2, "sequence_ref": map[string]any{"id": "po.seq", "revision": 1}, "sent_at": now(),
		"evidence": []any{map[string]any{"source_id": "po.sent", "locator": "发送记录"}}})
	if r["status"] != purapi.CommitmentIssued {
		c.t.Fatalf("issue %v", r)
	}
	c.source(brcPurchaser, "supplier.ack", "合成供应商确认：supplier.A 按采购订单原样确认（数量、单价、交期）。")
	r = c.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandRecordSupplierConfirmation, map[string]any{"commitment_id": "commitment.1",
		"expected_version": 3, "result": purapi.ConfirmationAsIssued, "received_at": now(),
		"evidence": []any{map[string]any{"source_id": "supplier.ack", "locator": "确认函"}}, "supplier_reference": "SA-OC-001"})
	if r["status"] != purapi.CommitmentConfirmed {
		c.t.Fatalf("supplier confirmation %v", r)
	}
	v := c.get(brcPurchaser, "/api/brant/v1/pur/purchase_commitments/commitment.1")
	c.poNumber = brcStr(v["po_number"])
	if v["status"] != purapi.CommitmentConfirmed || !strings.HasPrefix(c.poNumber, "PO-") || v["draft_hash"] != c.draftHash {
		c.t.Fatalf("commitment %v", v)
	}
	c.fact("status", v["status"], "po_number", c.poNumber, "draft_hash", c.draftHash)
	c.refused(brcPurchaser, "pur", purapi.Contract, purapi.CommandVoidDraft, map[string]any{"draft_id": "purchase.1", "expected_version": 1,
		"reason": "采购承诺已发出后试图作废草稿"}, http.StatusConflict, purapi.CodePurchaseDraftCommitted)
	d = c.get(brcPurchaser, "/api/brant/v1/pur/purchase_drafts/purchase.1")
	if d["commitment_state"] != purapi.CommitmentCommitted || d["net_minor"] != "60000" {
		c.t.Fatalf("draft after the issue %v", d)
	}
	c.fact("commitment_state", d["commitment_state"], "net_minor", d["net_minor"])
	c.record.Amounts["purchase_commitment"] = brcMajor(c.t, "60000") + " USD"
}

// quality is §5 step 9: the external agency's report of every commitment line and the quality
// releases (QLT-INV-001: released ≤ conforming).
func (c *brcS5) quality() {
	c.step(9, "QLT inspection and quality release")
	c.party(brcBuyer, "agency.1", []string{ptyapi.RoleSupplier}, "evidence")
	c.cmd(brcBuyer, "pty", ptyapi.Contract, "service_role.verify", map[string]any{"id": "agency.1", "expected_version": 2,
		"service_role_id": "inspection.1", "role": ptyapi.ServiceInspection, "service_code": "final.inspection", "origin": "CNNGB", "destination": "CNNGB",
		"valid_from": kernel.FormatTimestamp(c.from), "valid_until": kernel.FormatTimestamp(c.until), "evidence_ids": []string{"evidence"}})
	c.source(brcQC, "standard", "合成检验标准：AQL 2.5，一般检验水平 II，ST01 规格书 TECH-01-r1。")
	c.fileOf(brcQC, "report.final", "final-inspection-ST01.txt", "合成第三方终检报告（agency.1）：ST01 红/蓝 × S/M，24 + 36 + 12 + 48 = 120 PCS 全检，全部合格。")
	var released int64
	for _, l := range brcLines {
		subject := map[string]any{"kind": qltapi.SubjectCommitmentLine, "id": "commitment.1", "line_id": l.id}
		c.cmd(brcQC, "qlt", qltapi.Contract, qltapi.CommandRecordInspection, map[string]any{"inspection_id": "insp." + l.id, "subject": subject,
			"inspected_quantity": l.qty, "conforming_quantity": l.qty, "nonconforming_quantity": "0", "sampling": map[string]any{"method": qltapi.SamplingFull},
			"standard": map[string]any{"source_id": "standard"}, "inspector": map[string]any{"kind": qltapi.InspectorAgency,
				"agency": map[string]any{"party_id": "agency.1", "service_role_id": "inspection.1", "revision": 1}},
			"report_file_id": "report.final", "inspected_at": now()})
		r := c.cmd(brcQC, "qlt", qltapi.Contract, qltapi.CommandGrantRelease, map[string]any{"release_id": "rel." + l.id, "subject": subject,
			"quantity": l.qty, "valid_until": kernel.FormatTimestamp(time.Now().Add(30 * 24 * time.Hour))})
		if r["status"] != qltapi.StatusGranted {
			c.t.Fatalf("release of %s %v", l.id, r)
		}
		v := c.get(brcQC, "/api/brant/v1/qlt/quality_releases/rel."+l.id)
		if v["quantity"] != l.qty || v["state"] != qltapi.StatusGranted {
			c.t.Fatalf("release %s %v", l.id, v)
		}
		released += kernel.MustDecimal(l.qty).Floor().Int64()
	}
	if released != 120 {
		c.t.Fatalf("released %d", released)
	}
	c.fact("released", released)
}

// milestones is §5 step 10 (CN-R07: a supplier's claim, our verification and an accepted fact stay
// apart).
func (c *brcS5) milestones() {
	c.step(10, "FUL milestones")
	day := func(d int) string { return time.Now().UTC().AddDate(0, 0, d).Format(time.DateOnly) }
	c.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandPlanMilestones, map[string]any{"plan_id": "plan.order.1", "expected_version": 0,
		"subject": map[string]any{"kind": "order", "id": "order.1"}, "nodes": []any{
			map[string]any{"node_id": "M1", "node_type": "production_complete", "planned_on": day(0)},
			map[string]any{"node_id": "M2", "node_type": "inspection", "planned_on": day(0), "predecessors": []string{"M1"}},
			map[string]any{"node_id": "M3", "node_type": "packing_complete", "planned_on": day(0), "predecessors": []string{"M2"}},
			map[string]any{"node_id": "M4", "node_type": "supplier_handover", "planned_on": day(1), "predecessors": []string{"M3"}},
			map[string]any{"node_id": "M5", "node_type": "actual_departure", "planned_on": day(7), "predecessors": []string{"M4"}}}})
	c.source(brcMerchandiser, "cargo.ready", "合成跟单核实记录：supplier.A 工厂 120 PCS 已完成包装，10 箱，待交接（现场照片与箱单）。")
	c.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandRecordMilestoneActual, map[string]any{"plan_id": "plan.order.1", "expected_version": 1,
		"node_id": "M3", "evidence_level": "internal_verified", "occurred_at": now(), "evidence_ids": []string{"cargo.ready"},
		"note": "货好（cargo ready），跟单现场核实"})
	v := c.get(brcMerchandiser, "/api/brant/v1/ful/milestone_plans/plan.order.1")
	if len(brcList(v["actuals"])) != 1 || brcDig(v, "actuals", 0, "evidence_level") != "internal_verified" {
		c.t.Fatalf("milestone plan %v", v)
	}
	c.fact("revision", v["revision"], "actuals", len(brcList(v["actuals"])), "node", "M3 packing_complete", "evidence_level", "internal_verified")
}

// delivery is §5 step 11: supplier.A's handover of the 120 PCS at CNNGB, recorded by the
// merchandiser and verified by the purchaser (PUR-INV-008; the recorder never verifies).
func (c *brcS5) delivery() {
	c.step(11, "PUR supplier delivery")
	c.source(brcMerchandiser, "forwarder.receipt", "合成货代进仓单：provider.1 于 CNNGB 收到 supplier.A 交来 ST01 120 PCS，10 箱。")
	var lines, verified []any
	for _, l := range brcLines {
		lines = append(lines, map[string]any{"order_line_id": l.id, "quantity": l.qty})
		verified = append(verified, map[string]any{"order_line_id": l.id, "verified_quantity": l.qty})
	}
	c.cmd(brcMerchandiser, "pur", purapi.Contract, purapi.CommandRecordSupplierDelivery, map[string]any{"delivery_id": "delivery.1", "expected_version": 0,
		"commitment_id": "commitment.1", "lines": lines, "handover_place": "CNNGB", "handed_over_at": now(),
		"evidence": []any{map[string]any{"source_id": "forwarder.receipt", "locator": "进仓单"}}, "supplier_reference": "SA-DN-001"})
	r := c.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandVerifySupplierDelivery, map[string]any{"delivery_id": "delivery.1",
		"expected_version": 1, "decision": "verify", "lines": verified, "evidence": []any{map[string]any{"source_id": "forwarder.receipt", "locator": "进仓单"}}})
	if r["status"] != purapi.DeliveryVerified {
		c.t.Fatalf("verify %v", r)
	}
	v := c.get(brcPurchaser, "/api/brant/v1/pur/purchase_commitments/commitment.1/deliveries")
	c.fact("deliveries", v["lines"])
}

// shipment is §5 step 12: shipment.1 prepared from the verified delivery lines and the releases,
// its packing planned and recorded (10 cartons of 12 PCS), and its booking with provider.1.
func (c *brcS5) shipment() {
	c.step(12, "FUL shipment, packing and booking")
	c.shipmentID = "shipment.1"
	var lines []any
	for i, l := range brcLines {
		lines = append(lines, map[string]any{"line_id": fmt.Sprintf("S%d", i+1), "order_line_id": l.id, "quantity": l.qty,
			"supplier_delivery": map[string]any{"delivery_id": "delivery.1", "line_id": l.id, "version": 1}, "quality_release_id": "rel." + l.id})
	}
	r := c.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandPrepareShipment, map[string]any{"shipment_id": c.shipmentID, "order_id": "order.1",
		"route": fulapi.RouteSupplierDirect, "lines": lines})
	if r["status"] != fulapi.StatusPrepared {
		c.t.Fatalf("prepare %v", r)
	}
	c.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandPlanPacking, map[string]any{"shipment_id": c.shipmentID, "expected_version": 1,
		"packages": c.packages(false), "evidence_ids": []string{"cargo.ready"}})
	r = c.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandRecordPacking, map[string]any{"shipment_id": c.shipmentID, "expected_version": 2,
		"packages": c.packages(true), "evidence_ids": []string{"forwarder.receipt"}})
	c.shipmentVersion = brcNum(r["row_version"])
	c.source(brcMerchandiser, "booking.confirmation", "合成订舱确认：买方指定货代 provider.1（FCA），SYNTHETIC STAR 026E，CNNGB→DEHAM，1×40HQ。")
	c.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandRecordBooking, map[string]any{"shipment_id": c.shipmentID, "expected_version": 0,
		"service_role": map[string]any{"party_id": "provider.1", "service_role_id": "role.1", "revision": 1}, "booking_number": "NGB2611001",
		"vessel": "SYNTHETIC STAR", "voyage": "026E", "etd": today(), "cut_off": kernel.FormatTimestamp(time.Now().UTC().Truncate(time.Hour)),
		"container_type": "40HQ", "container_count": 1,
		"port_of_loading": "CNNGB", "port_of_discharge": "DEHAM", "evidence_ids": []string{"booking.confirmation"}})
	v := c.get(brcMerchandiser, "/api/brant/v1/ful/shipments/"+c.shipmentID)
	if v["status"] != fulapi.StatusPrepared || len(brcList(v["lines"])) != len(brcLines) {
		c.t.Fatalf("shipment %v", v)
	}
	c.fact("status", v["status"], "row_version", v["row_version"], "lines", len(brcList(v["lines"])))
}

// packages are shipment.1's cartons: 12 PCS each, numbered 1–10 (actual: with weights and volume).
func (c *brcS5) packages(actual bool) []any {
	var out []any
	from := 1
	for i, l := range brcLines {
		n := int(kernel.MustDecimal(l.qty).Floor().Int64() / 12)
		p := map[string]any{"cartons": n, "contents": []any{map[string]any{"line_id": fmt.Sprintf("S%d", i+1), "quantity_per_carton": "12"}}}
		if actual {
			p["carton_from"], p["gross_weight_kg"], p["net_weight_kg"], p["volume_cbm"] = from, "6.5", "6", "0.05"
		}
		out = append(out, p)
		from += n
	}
	return out
}

// releaseEvaluation reads a commercial release and returns its latest evaluation.
func (c *brcS5) releaseEvaluation(id string) map[string]any {
	v := c.get(brcDocumentation, "/api/brant/v1/ful/commercial_releases/"+id)
	evaluations := brcList(v["evaluations"])
	if len(evaluations) == 0 {
		c.t.Fatalf("release %s %v", id, v)
	}
	return brcMap(evaluations[len(evaluations)-1])
}

// shipmentRelease is §5 step 13: doc.1 evaluates the shipment's commercial release (WF-07).
func (c *brcS5) shipmentRelease() {
	c.step(13, "FUL commercial release of the shipment")
	r := c.cmd(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandEvaluateRelease, map[string]any{"shipment_id": c.shipmentID,
		"scope": fulapi.ScopeShipment})
	c.releaseID = brcStr(r["object_id"])
	if r["status"] != fulapi.ReleaseGranted {
		c.t.Fatalf("evaluate %v", r)
	}
	e := c.releaseEvaluation(c.releaseID)
	b := brcMap(e["bindings"])
	var qlt int64
	for _, q := range brcList(b["quality_releases"]) {
		qlt += kernel.MustDecimal(brcStr(brcDig(q, "released_quantity"))).Floor().Int64()
	}
	var pur int64
	for _, d := range brcList(b["supplier_deliveries"]) {
		pur += kernel.MustDecimal(brcStr(brcDig(d, "verified_quantity"))).Floor().Int64()
	}
	if e["outcome"] != fulapi.ReleaseGranted || qlt != 120 || pur != 120 || brcDig(b, "trade_compliance", "status") != cnxapi.StatusClear ||
		brcDig(b, "trade_compliance", "license_required") != false || brcDig(b, "screenings", 0, "status") != "clear" ||
		brcDig(b, "payment_condition", "status") != "satisfied" || brcDig(b, "payment_condition", "applied_minor") != "28800" ||
		brcDig(b, "order", "status") != trdapi.OrderActive {
		c.t.Fatalf("release evaluation %v", e)
	}
	c.fact("release_id", c.releaseID, "outcome", e["outcome"], "quality_released", qlt, "delivery_verified", pur,
		"trade_compliance", brcDig(b, "trade_compliance", "status"), "license_required", false, "screening", brcDig(b, "screenings", 0, "status"),
		"payment_condition", brcDig(b, "payment_condition", "status"), "payment_applied_minor", brcDig(b, "payment_condition", "applied_minor"),
		"order", brcDig(b, "order", "status"), "guards_sha256", e["guards_sha256"])
}

// confirm is §5 step 14: merchandiser confirms shipment.1 (FUL-INV-001 120 ≤ 120), consuming the
// release.
func (c *brcS5) confirm() {
	c.step(14, "FUL shipment confirmation")
	r := c.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": c.shipmentID,
		"expected_version": c.shipmentVersion, "evidence_ids": []string{"forwarder.receipt"}})
	if r["status"] != fulapi.StatusConfirmed {
		c.t.Fatalf("confirm %v", r)
	}
	v := c.get(brcMerchandiser, "/api/brant/v1/ful/shipments/"+c.shipmentID)
	if v["status"] != fulapi.StatusConfirmed || len(brcList(brcDig(v, "confirmation", "unchecked_guards"))) != 0 {
		c.t.Fatalf("confirmed shipment %v", v["confirmation"])
	}
	c.fact("status", v["status"], "unchecked_guards", len(brcList(brcDig(v, "confirmation", "unchecked_guards"))), "guards",
		len(brcList(brcDig(v, "confirmation", "guards"))), "guards_sha256", brcDig(v, "confirmation", "guards_sha256"))
	f := c.get(brcMerchandiser, "/api/brant/v1/ful/orders/order.1/fulfilment")
	var shipped int64
	for i := range brcLines {
		shipped += kernel.MustDecimal(brcStr(brcDig(f, "lines", i, "shipped"))).Floor().Int64()
	}
	if shipped != 120 {
		c.t.Fatalf("fulfilment %v", f)
	}
	c.fact("shipped", shipped)
	c.record.Amounts["shipped"] = fmt.Sprintf("%d PCS", shipped)
}

// documentParticulars are the particulars of shipment.1's commercial invoice and packing list in
// English: the consignee (the buyer, customer.1's hq address), the shipping marks and the four SKUs'
// descriptions.
func documentParticulars() map[string]any {
	var descriptions []any
	for _, l := range brcLines {
		descriptions = append(descriptions, map[string]any{"sku_id": l.sku, "language": fulapi.LanguageEn, "text": "Cotton knitted T-shirt " + l.sku})
	}
	return map[string]any{"consignee": map[string]any{"name": "customer.1", "address": "合成买方地址\n汉堡"}, "marks": "CUSTOMER.1\nHAMBURG\nC/NO. 1-10",
		"port_of_loading": "CNNGB", "port_of_discharge": "DEHAM", "descriptions": descriptions}
}

// documents is §5 step 15 (FUL-INV-004, BT12): doc.1 prepares the commercial invoice, the packing
// list and the bill of lading record of shipment.1, reads the reconciliation and issues the set.
func (c *brcS5) documents() {
	c.step(15, "FUL shipment documents")
	c.documentSet = "docs.shipment.1"
	prepare := func(expected int64, kind string, extra map[string]any) map[string]any {
		args := map[string]any{"document_set_id": c.documentSet, "shipment_id": c.shipmentID, "expected_version": expected, "document_kind": kind}
		for k, v := range extra {
			args[k] = v
		}
		return c.cmd(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandPrepareShipmentDocument, args)
	}
	prepare(0, fulapi.DocumentCommercialInvoice, map[string]any{"templates": []any{c.ciEn}, "sequence_ref": c.ciSeq, "seller_ref": c.company,
		"particulars": documentParticulars()})
	prepare(1, fulapi.DocumentPackingList, map[string]any{"templates": []any{c.plEn}, "sequence_ref": c.plSeq, "seller_ref": c.company,
		"particulars": documentParticulars()})
	c.fileOf(brcDocumentation, "bl.scan", "bl-SYNSTAR026E0001.txt", "合成提单扫描件：SYNTHETIC STAR 026E，CNNGB→DEHAM，10 CTNS，65 KGS，0.5 CBM。")
	r := prepare(2, fulapi.DocumentBillOfLading, map[string]any{"bill_of_lading": map[string]any{"number": "SYNSTAR026E0001", "file_id": "bl.scan",
		"vessel": "SYNTHETIC STAR", "voyage": "026E", "port_of_loading": "CNNGB", "port_of_discharge": "DEHAM", "shipped_on_board": today(),
		"consignee": map[string]any{"name": "customer.1", "address": "合成买方地址\n汉堡"}, "marks": "CUSTOMER.1\nHAMBURG\nC/NO. 1-10",
		"packages": 10, "gross_weight_kg": "65", "volume_cbm": "0.5", "container_numbers": []string{"SYNU1234565"}}})
	set := c.get(brcDocumentation, "/api/brant/v1/ful/document_sets/"+c.documentSet)
	if brcDig(set, "reconciliation", "state") != "reconciled" || len(brcList(brcDig(set, "reconciliation", "differences"))) != 0 {
		c.t.Fatalf("reconciliation %v", set["reconciliation"])
	}
	c.fact("reconciliation", brcDig(set, "reconciliation", "state"), "differences", 0, "reconciliation_sha256", brcDig(set, "reconciliation", "sha256"))
	r = c.cmd(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandIssueShipmentDocuments, map[string]any{"document_set_id": c.documentSet,
		"expected_version": r["row_version"]})
	if r["status"] != fulapi.DocumentIssued {
		c.t.Fatalf("issue %v", r)
	}
	set = c.get(brcDocumentation, "/api/brant/v1/ful/document_sets/"+c.documentSet)
	ci := c.issued(set, fulapi.DocumentCommercialInvoice)
	content := c.get(brcDocumentation, fmt.Sprintf("/api/brant/v1/ful/document_sets/%s/documents/%s/versions/%v/%s", c.documentSet,
		fulapi.DocumentCommercialInvoice, ci["version"], fulapi.LanguageEn))
	text, err := base64.StdEncoding.DecodeString(brcStr(content["content"]))
	c.w.must(err)
	for _, want := range []string{"Net: 960.00 | Tax: 0.00 | Total: 960.00", "Quantity: 120 PCS", "Currency: USD", "Port of loading: CNNGB"} {
		if !strings.Contains(string(text), want) {
			c.t.Fatalf("the commercial invoice lacks %q:\n%s", want, text)
		}
	}
	c.fact("ci_number", ci["number"], "ci_sha256", ci["content_sha256"], "ci_states", "Net: 960.00 | Tax: 0.00 | Total: 960.00", "pl_number", c.issued(set, fulapi.DocumentPackingList)["number"],
		"bl_number", "SYNSTAR026E0001")
	c.record.Amounts["commercial_invoice"] = "960.00 USD"
}

// issued is the issued version of kind in a document set view.
func (c *brcS5) issued(set map[string]any, kind string) map[string]any {
	for _, d := range brcList(set["documents"]) {
		if brcDig(d, "document_kind") == kind && brcDig(d, "status") == fulapi.DocumentIssued {
			return brcMap(d)
		}
	}
	c.t.Fatalf("no issued %s in %v", kind, set)
	return nil
}

// documentsRelease is §5 step 16 (D-5): the documents release of the confirmed shipment, granted
// with P2 on credit and not document-controlled, and the original B/L's release.
func (c *brcS5) documentsRelease() {
	c.step(16, "FUL documents release")
	r := c.cmd(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandEvaluateRelease, map[string]any{"shipment_id": c.shipmentID,
		"scope": fulapi.ScopeDocuments})
	c.documentsReleaseID = brcStr(r["object_id"])
	if r["status"] != fulapi.ReleaseGranted {
		c.t.Fatalf("evaluate documents %v", r)
	}
	e := c.releaseEvaluation(c.documentsReleaseID)
	c.fact("release_id", c.documentsReleaseID, "outcome", e["outcome"], "payment_condition", brcDig(e, "bindings", "payment_condition", "status"),
		"required_minor", brcDig(e, "bindings", "payment_condition", "required_minor"))
	c.source(brcDocumentation, "bl.release", "合成正本提单放单记录：三份正本提单交买方指定货代 provider.1（FCA）。")
	r = c.cmd(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandRecordDocumentRelease, map[string]any{"shipment_id": c.shipmentID,
		"expected_version": r["row_version"], "release_kind": fulapi.ReleaseOriginalBL, "evidence_ids": []string{"bl.release"}})
	if r["status"] != fulapi.ReleaseConsumed {
		c.t.Fatalf("document release %v", r)
	}
	v := c.get(brcDocumentation, "/api/brant/v1/ful/commercial_releases/"+c.documentsReleaseID)
	c.fact("status", v["status"], "release_kind", brcDig(v, "consumption", "release_kind"))
}

// exportLot is §5 step 17 (R36): lot.1 of the four shipment lines, 120 PCS, 960.00 USD.
func (c *brcS5) exportLot() {
	c.step(17, "FUL export lot")
	var lines []any
	for i, l := range brcLines {
		lines = append(lines, map[string]any{"line_id": fmt.Sprintf("E%d", i+1), "shipment_line_id": fmt.Sprintf("S%d", i+1), "quantity": l.qty})
	}
	c.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandRegisterExportLot, map[string]any{"export_lot_id": "lot.1", "shipment_id": c.shipmentID,
		"lines": lines})
	v := c.get(brcMerchandiser, "/api/brant/v1/ful/export_lots/lot.1")
	if brcDig(v, "totals", "net_minor") != "96000" || v["reconciliation"] != "matched" || len(brcList(v["lines"])) != len(brcLines) {
		c.t.Fatalf("export lot %v", v)
	}
	c.fact("net_minor", brcDig(v, "totals", "net_minor"), "reconciliation", v["reconciliation"], "lines", len(brcList(v["lines"])))
	c.record.Amounts["export_lot"] = brcMajor(c.t, "96000") + " USD"
}

// declaration is §5 step 18 (CN-M01 manual path; CN-R05, CN-R07, CNX-INV-003, CNX-INV-006).
func (c *brcS5) declaration() {
	c.step(18, "CNX customs declaration")
	// The declaration refers to the order's sales contract (S/C): sales generates it from TRD's active
	// order revision (order.document.generate; TRD's sales documents, which CNX binds).
	o := c.get(brcSales, "/api/brant/v1/trd/orders/order.1")
	r := c.cmd(brcSales, "trd", trdapi.Contract, "order.document.generate", map[string]any{"order_id": "order.1", "expected_order_revision": 1,
		"expected_order_hash": o["order_hash"], "document_kind": trdapi.DocumentSalesContract, "template_ref": c.scTemplate, "sequence_ref": c.scSeq})
	if r["status"] != trdapi.StatusGenerated {
		c.t.Fatalf("order.document.generate %v", r)
	}
	var lines []any
	for i := range brcLines {
		lines = append(lines, map[string]any{"line_id": fmt.Sprintf("E%d", i+1)})
	}
	r = c.cmd(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandPrepareDeclaration, map[string]any{"expected_version": 0,
		"export_lot_id": "lot.1", "code_table_version": codeTable, "items": []any{map[string]any{"item_no": 1, "lines": lines}}})
	c.declarationID = brcStr(r["object_id"])
	r = c.cmd(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandConfirmDeclaration, map[string]any{"declaration_id": c.declarationID,
		"expected_version": r["row_version"], "version": 1, "reason": "申报要素与已核实的申报档案一致（合成）"})
	c.source(brcDocumentation, "declaration.sent", "合成报关单人工申报记录：单一窗口录入并提交 lot.1（ST01 120 PCS，960.00 USD），操作编号 SW-BRC-0001。")
	r = c.cmd(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordSubmission, map[string]any{"declaration_id": c.declarationID,
		"expected_version": r["row_version"], "operation_reference": "SW-BRC-0001", "submitter": map[string]any{"kind": "own_clerk", "name": "doc.1"},
		"submitted_at": now(), "evidence_ids": []string{"declaration.sent"}})
	c.source(brcDocumentation, "customs.receipt", "合成海关回执：报关单 SYN-2026-000001 放行，出口日期为当日（合成原文）。")
	// Customs' export date is the receipt's day; the export tax policy was published before it.
	received := time.Now()
	c.exportedAt = kernel.FormatTimestamp(received.Add(-time.Millisecond))
	c.declarationNumber = "SYN-2026-000001"
	r = c.cmd(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, map[string]any{"declaration_id": c.declarationID,
		"expected_version": r["row_version"], "receipt_id": "receipt.1", "outcome": "released", "external": map[string]any{"sequence": 1,
			"status_code": "P", "original_text": "【合成】海关已放行 / Released by customs (synthetic)", "received_at": kernel.FormatTimestamp(received)},
		"mapping_version": "v1", "declaration_number": c.declarationNumber, "exported_at": c.exportedAt, "evidence_ids": []string{"customs.receipt"}})
	if r["status"] != "released" {
		c.t.Fatalf("receipt %v", r)
	}
	v := c.get(brcDocumentation, "/api/brant/v1/cnx/declaration_cases/"+c.declarationID)
	sales := brcList(brcDig(v, "versions", 0, "sales_documents"))
	if v["status"] != "released" || v["customs_released"] != true || v["declaration_number"] != c.declarationNumber || len(sales) != 1 ||
		brcDig(sales, 0, "kind") != trdapi.DocumentSalesContract || !strings.HasPrefix(brcStr(brcDig(sales, 0, "number")), "SC-") ||
		brcDig(v, "versions", 0, "lot_value_minor") != "96000" {
		c.t.Fatalf("declaration %v", v)
	}
	c.salesContract = brcStr(brcDig(sales, 0, "number"))
	c.fact("declaration_id", c.declarationID, "status", v["status"], "customs_released", v["customs_released"], "declaration_number",
		v["declaration_number"], "sales_contract", c.salesContract, "lot_value_minor", brcDig(v, "versions", 0, "lot_value_minor"),
		"mapping_version", brcDig(v, "receipts", 0, "mapping_version"), "original_text_kept",
		brcDig(v, "receipts", 0, "original_text") != nil)
	// The export date is CNX's own fact from the receipt; FUL's departure milestone stays unrecorded
	// (CN-R07: customs release, departure and acceptance are separate facts).
	plan := c.get(brcMerchandiser, "/api/brant/v1/ful/milestone_plans/plan.order.1")
	if len(brcList(plan["actuals"])) != 1 {
		c.t.Fatalf("milestones after the customs release %v", plan)
	}
	c.fact("ful_actuals", len(brcList(plan["actuals"])))
}

// salesStatement is §5 step 19 (STL-INV-001, D-4).
func (c *brcS5) salesStatement() {
	c.step(19, "STL sales statement and receivable")
	var sources []any
	for i := range brcLines {
		sources = append(sources, map[string]any{"shipment_id": c.shipmentID, "line_id": fmt.Sprintf("S%d", i+1)})
	}
	c.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmSettlement, map[string]any{"statement_id": "stmt.sales.1",
		"kind": stlapi.StatementSales, "sources": sources})
	v := c.get(brcTreasury, "/api/brant/v1/stl/receivables/stmt.sales.1")
	if v["amount_minor"] != "96000" || v["applied_minor"] != "28800" || v["open_minor"] != "67200" {
		c.t.Fatalf("receivable %v", v)
	}
	c.fact("amount_minor", v["amount_minor"], "applied_minor", v["applied_minor"], "open_minor", v["open_minor"], "installments", installmentsView(v))
	app := c.get(brcTreasury, "/api/brant/v1/stl/applications/app.p1")
	if app["status"] != "moved" {
		c.t.Fatalf("P1's application after the statement %v", app)
	}
	c.fact("p1_application", app["status"])
	if e := c.exposure(brcCreditOfficer); e != "67200" {
		c.t.Fatalf("exposure after the statement %s", e)
	}
	c.record.Amounts["receivable"] = brcMajor(c.t, "96000") + " USD"
}

// installmentsView lists a balance view's installments as "stage amount applied open".
func installmentsView(v map[string]any) []string {
	var out []string
	for _, i := range brcList(v["installments"]) {
		out = append(out, fmt.Sprintf("%v %v applied %v open %v", brcDig(i, "stage_id"), brcDig(i, "amount_minor"), brcDig(i, "applied_minor"),
			brcDig(i, "open_minor")))
	}
	return out
}

// collection is §5 step 20 (CN-AT07 shape, CN-R10, STL-INV-006, D-16).
func (c *brcS5) collection() {
	c.step(20, "STL collection of P2 with a bank charge")
	c.source(brcDocumentation, "bank.p2", "合成银行收款通知：customer.1 汇出 672.00 USD，入账 670.00 USD，差额 2.00 未说明，MT103-BRC-P2。")
	r := c.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, map[string]any{"fund_record_id": "fund.p2", "kind": stlapi.FundReceipt,
		"counterparty_id": "customer.1", "currency": cfgRef("currency", "USD", 1), "bank_account": c.bank, "received_amount": "670.00",
		"remitted_amount": "672.00", "value_date": now(), "bank_reference": "MT103-BRC-P2", "evidence_ids": []string{"bank.p2"}})
	c.fact("difference_minor", "200", "status", r["status"])
	apply := func(id, amount string) map[string]any {
		return map[string]any{"application_id": id, "fund_record_id": "fund.p2", "target": map[string]any{"kind": "receivable",
			"statement_id": "stmt.sales.1", "stage_id": "P2"}, "amount": amount}
	}
	c.refused(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, apply("app.p2.full", "672.00"), http.StatusConflict,
		stlapi.CodeFundDifferenceUnexplained)
	r = c.cmd(brcCreditManager1, "stl", stlapi.Contract, stlapi.CommandResolveDifference, map[string]any{"fund_record_id": "fund.p2",
		"expected_version": 1, "resolution": "bank_charge", "amount": "2.00", "evidence_ids": []string{"bank.p2"},
		"reason": "中间行扣费 2.00 USD，经批准由我方承担（合成）"})
	if r["status"] != "difference_resolved" {
		c.t.Fatalf("resolve %v", r)
	}
	c.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, apply("app.p2", "670.00"))
	c.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, apply("app.p2.charge", "2.00"))
	v := c.get(brcTreasury, "/api/brant/v1/stl/receivables/stmt.sales.1")
	if v["open_minor"] != "0" || v["applied_minor"] != "96000" {
		c.t.Fatalf("receivable after P2 %v", v)
	}
	c.fact("open_minor", v["open_minor"], "applied_minor", v["applied_minor"], "installments", installmentsView(v))
	f := c.get(brcTreasury, "/api/brant/v1/stl/fund_records/fund.p2")
	if f["resolved_minor"] != "200" || f["available_minor"] != "0" {
		c.t.Fatalf("fund record %v", f)
	}
	c.fact("fund_resolved_minor", f["resolved_minor"], "fund_available_minor", f["available_minor"])
	if e := c.exposure(brcCreditOfficer); e != "0" {
		c.t.Fatalf("exposure after the collection %s", e)
	}
	c.record.Amounts["receivable_applied"] = "288.00 + 670.00 + 2.00 USD"
	c.record.Amounts["receivable_open"] = brcMajor(c.t, "0") + " USD"
}

// payable is §5 step 21: the purchase statement of the verified delivery and our payment.
func (c *brcS5) payable() {
	c.step(21, "STL purchase statement, payable and payment")
	var sources []any
	for _, l := range brcLines {
		sources = append(sources, map[string]any{"delivery_id": "delivery.1", "line_id": l.id, "version": 1})
	}
	c.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmSettlement, map[string]any{"statement_id": "stmt.purchase.1",
		"kind": stlapi.StatementPurchase, "sources": sources, "evidence_ids": []string{"supplier.ack"}})
	v := c.get(brcTreasury, "/api/brant/v1/stl/payables/stmt.purchase.1")
	if v["amount_minor"] != "60000" || v["open_minor"] != "60000" {
		c.t.Fatalf("payable %v", v)
	}
	c.fact("amount_minor", v["amount_minor"], "open_minor", v["open_minor"])
	c.source(brcDocumentation, "bank.pay", "合成银行付款回单：向 supplier.A 付款 600.00 USD，TT-BRC-PAY-1。")
	c.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, map[string]any{"fund_record_id": "fund.pay.1", "kind": stlapi.FundPayment,
		"counterparty_id": "supplier.A", "currency": cfgRef("currency", "USD", 1), "bank_account": c.bank, "received_amount": "600.00",
		"value_date": now(), "bank_reference": "TT-BRC-PAY-1", "evidence_ids": []string{"bank.pay"}})
	c.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, map[string]any{"application_id": "app.pay.1", "fund_record_id": "fund.pay.1",
		"target": map[string]any{"kind": "payable", "statement_id": "stmt.purchase.1"}, "amount": "600.00"})
	v = c.get(brcTreasury, "/api/brant/v1/stl/payables/stmt.purchase.1")
	if v["open_minor"] != "0" || v["applied_minor"] != "60000" {
		c.t.Fatalf("payable after the payment %v", v)
	}
	c.fact("open_minor", v["open_minor"], "applied_minor", v["applied_minor"])
	c.record.Amounts["payable"] = brcMajor(c.t, "60000") + " USD"
	c.record.Amounts["payable_open"] = brcMajor(c.t, "0") + " USD"
}

// invoiceFacts are supplier.A's synthetic VAT special invoice: one line of 120 PCS, 600.00 USD net at
// 13 %, buyer the company profile's legal name and registration number.
func invoiceFacts() map[string]any {
	return map[string]any{"kind": "special_vat", "colour": "blue", "number": "33002611001", "issued_on": today(),
		"supplier": map[string]any{"party_id": "supplier.A", "name": "supplier.A", "tax_id": "91330200SYNTHA0001"},
		"buyer":    map[string]any{"name": "合成企业", "tax_id": "SYNTHETIC-NOT-REAL"}, "currency": map[string]any{"id": "USD", "revision": 1},
		"lines": []any{map[string]any{"line_no": 1, "description": "棉制针织T恤衫 ST01", "quantity": "120", "unit": "PCS", "net_amount": "600.00",
			"tax_rate": "0.13", "tax_amount": "78.00"}}, "net_total": "600.00", "tax_total": "78.00", "gross_total": "678.00"}
}

// invoice is §5 step 22 (CNX-INV-005, CN-AT18: a human verifies the candidate).
func (c *brcS5) invoice() {
	c.step(22, "CNX purchase invoice")
	c.fileOf(brcPurchaser, "invoice.scan", "vat-special-33002611001.txt", "合成增值税专用发票扫描件：supplier.A，No. 33002611001，ST01 120 PCS，"+
		"金额 600.00，税率 13%，税额 78.00，价税合计 678.00（合成，非真实发票）。")
	c.source(brcPurchaser, "invoice.received", "合成收票记录：收到 supplier.A 增值税专用发票 No. 33002611001 原件（ST01 120 PCS）。")
	r := c.cmd(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandImportInvoice, map[string]any{"file_id": "invoice.scan", "candidate": invoiceFacts()})
	c.invoiceID = brcStr(r["object_id"])
	var matches []any
	for _, l := range brcLines {
		matches = append(matches, map[string]any{"line_no": 1, "commitment_id": "commitment.1", "order_line_id": l.id, "quantity": l.qty})
	}
	r = c.cmd(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandVerifyInvoice, map[string]any{"purchase_invoice_id": c.invoiceID,
		"expected_version": r["row_version"], "decision": "verify", "facts": invoiceFacts(), "matches": matches})
	if r["status"] != "verified" {
		c.t.Fatalf("verify %v", r)
	}
	v := c.get(brcTaxOfficer, "/api/brant/v1/cnx/purchase_invoices/"+c.invoiceID)
	var matched int64
	for _, m := range brcList(v["matches"]) {
		matched += kernel.MustDecimal(brcStr(brcDig(m, "quantity"))).Floor().Int64()
	}
	if v["status"] != "verified" || matched != 120 {
		c.t.Fatalf("invoice %v", v)
	}
	c.fact("purchase_invoice_id", c.invoiceID, "status", v["status"], "matched", matched, "differences", len(brcList(v["differences"])))
}

// taxCase is §5 step 23 (CNX-INV-004, CN-AT10, CN-AT12).
func (c *brcS5) taxCase() {
	c.step(23, "CNX tax refund case")
	var allocations []any
	for i, l := range brcLines {
		allocations = append(allocations, map[string]any{"allocation_id": fmt.Sprintf("alloc.E%d", i+1), "export_line_id": fmt.Sprintf("E%d", i+1),
			"purchase_invoice_id": c.invoiceID, "line_no": 1, "purpose": "export_refund", "quantity": l.qty})
	}
	r := c.cmd(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandAllocateTaxEvidence, map[string]any{"export_lot_id": "lot.1",
		"policy_id": "export.tax", "allocations": allocations})
	c.taxCaseID = brcStr(r["object_id"])
	v := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+c.taxCaseID)
	// No refund amount is invented: the allocation states none (CN-E01).
	if v["status"] != "expected" || len(brcList(v["allocations"])) != len(brcLines) || len(brcMap(v["amounts"])) != 0 {
		c.t.Fatalf("tax case %v", v)
	}
	c.fact("case_id", c.taxCaseID, "status", v["status"], "allocations", len(brcList(v["allocations"])), "amounts", v["amounts"],
		"policy_revision", brcDig(v, "policy", "revision"))
	r = c.cmd(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxResult, map[string]any{"case_id": c.taxCaseID,
		"expected_version": r["row_version"], "stage": "expected", "currency": c.usdRef(), "amount": "78.00",
		"reason": "预计可退（抵）税额以已核实专用发票所列进项税额 78.00 为上限登记，非退税计算结果（合成）"})
	if r["status"] != "expected" {
		c.t.Fatalf("record_result %v", r)
	}
	v = c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+c.taxCaseID)
	if v["status"] != "expected" || brcDig(v, "amounts", "expected_minor") != "7800" || brcDig(v, "amounts", "approved_minor") != nil ||
		brcDig(v, "amounts", "received_minor") != nil {
		c.t.Fatalf("tax case after the expected result %v", v)
	}
	c.fact("status", v["status"], "amounts", v["amounts"])
}

// bundle is §5 step 24 (CNX-INV-008, CN-AT14: presence is not validity).
func (c *brcS5) bundle() {
	c.step(24, "CNX document bundle and external reconciliation")
	due := time.Now().UTC().AddDate(0, 0, 30).Format(time.DateOnly)
	req := func(id, kind string, evidence []string, extra map[string]any) map[string]any {
		r := map[string]any{"requirement_id": id, "kind": kind, "version": "1", "status": "verified", "owner": map[string]any{"kind": "role", "id": "documentation"},
			"due_on": due, "evidence_ids": evidence, "basis": "单证核对原件一致（合成）"}
		for k, v := range extra {
			r[k] = v
		}
		return r
	}
	c.source(brcDocumentation, "docs.originals", "合成单证核对记录：商业发票 CI-2026-00001、装箱单 PL-2026-00001、提单 SYNSTAR026E0001 "+
		"与 FUL 已签发的单证一致。")
	r := c.cmd(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandCheckBundle, map[string]any{"export_lot_id": "lot.1", "expected_version": 0,
		"requirements": []any{
			req("ci", "commercial_invoice", []string{"docs.originals"}, nil),
			req("pl", "packing_list", []string{"docs.originals"}, nil),
			req("bl", "bill_of_lading", []string{"docs.originals", "bl.release"}, nil),
			req("declaration", "customs_declaration", []string{"customs.receipt"}, map[string]any{"declaration_id": c.declarationID}),
			req("invoice", "purchase_invoice", []string{"invoice.received"}, nil)},
		"expect_complete": true})
	c.bundleID = brcStr(r["object_id"])
	if r["status"] != "complete" {
		c.t.Fatalf("bundle %v", r)
	}
	r = c.cmd(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandResolveReconciliation, map[string]any{"bundle_id": c.bundleID,
		"expected_version": r["row_version"], "reconciliation_id": "recon.value", "subject": map[string]any{"kind": "declaration", "id": c.declarationID},
		"measure": "amount", "currency": c.usdRef(), "external": map[string]any{"value": "960.00", "source": "报关单申报总价（海关回执）",
			"evidence_ids": []string{"customs.receipt"}}, "internal": map[string]any{"value": "960.00", "source": "商业发票 CI 金额"},
		"responsibility": map[string]any{"kind": "none", "note": "申报价与发票价一致（FCA）"}, "adjustment": map[string]any{"kind": "none"},
		"reason": "申报价值与商业发票金额核对一致（合成）"})
	if r["status"] != "complete" {
		c.t.Fatalf("external_reconciliation.resolve %v", r)
	}
	v := c.get(brcDocumentation, "/api/brant/v1/cnx/document_bundles/"+c.bundleID)
	if v["status"] != "complete" || len(brcList(v["reconciliations"])) != 1 {
		c.t.Fatalf("bundle view %v", v)
	}
	c.fact("bundle_id", c.bundleID, "status", v["status"], "reconciliation_difference", brcDig(v, "reconciliations", 0, "difference"))
}

// closeOrder is §5 step 25 (TRD-INV-019, D-19: shipped 120 = ordered 120).
func (c *brcS5) closeOrder() {
	c.step(25, "TRD order close")
	o := c.get(brcMerchandiser, "/api/brant/v1/trd/orders/order.1")
	r := c.cmd(brcMerchandiser, "trd", trdapi.Contract, "order.close", map[string]any{"order_id": "order.1", "expected_version": o["row_version"],
		"reason": "四行全部出运 120 PCS，单证放行，货款结清（合成）", "evidence": []any{map[string]any{"source_id": "bl.release", "locator": "放单记录"}}})
	if r["status"] != trdapi.OrderClosed {
		c.t.Fatalf("order.close %v", r)
	}
	o = c.get(brcMerchandiser, "/api/brant/v1/trd/orders/order.1")
	var shipped []any
	for i, l := range brcLines {
		line := brcMap(brcDig(o, "end", "lines", i))
		if line["line_id"] != l.id || line["ordered"] != l.qty || line["shipped"] != l.qty || line["remainder"] != "0" {
			c.t.Fatalf("order end %v", o["end"])
		}
		shipped = append(shipped, line["shipped"])
	}
	c.fact("status", o["status"], "row_version", o["row_version"], "end_command", brcDig(o, "end", "command"), "fulfilment_source",
		brcDig(o, "end", "fulfilment_source"), "shipped", shipped)
	// The commitment is delivered: every line verified in full (PUR's fulfilled).
	v := c.get(brcMerchandiser, "/api/brant/v1/pur/purchase_commitments/commitment.1")
	if v["fulfilled"] != true {
		c.t.Fatalf("commitment after the delivery %v", v)
	}
	c.fact("commitment_fulfilled", true)
}

// reservationReleased is §5 step 25's consumer half (PL-13): OrderClosed released res.order.1;
// the exposure is 0.
func (c *brcS5) reservationReleased() {
	c.step(25, "STL consumer of OrderClosed")
	v := c.reservation()
	if v["status"] != stlapi.ReservationReleased || brcDig(v, "released_by", "id") != "inbox.stl.order_credit" {
		c.t.Fatalf("reservation after the close %v", v)
	}
	c.fact("status", v["status"], "version", v["version"], "released_by", v["released_by"])
	if e := c.exposure(brcCreditOfficer); e != "0" {
		c.t.Fatalf("exposure after the release %s", e)
	}
	c.dispatchFact("reservation", v["status"])
}

// charge is §5 step 26 (STL-INV-010).
func (c *brcS5) charge() {
	c.step(26, "STL freight charge")
	r := c.cmd(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandRecordCharge, map[string]any{"charge_id": "freight.brc", "expected_version": 0,
		"status": "estimated", "source": map[string]any{"kind": "logistics_offer", "party_id": "provider.1", "reference": "log.1"},
		"beneficiaries": []any{map[string]any{"kind": "order", "order_id": "order.1"}}, "fee_category": c.freight, "currency": c.usd,
		"conversion": map[string]any{"kind": "same_currency"}, "amount": "60.00", "occurred_on": today(), "analysis_on": today(),
		"basis": "provider.1 海运报价 log.1：F1 60.00 USD"})
	if r["status"] != "estimated" {
		c.t.Fatalf("charge.record %v", r)
	}
	c.source(brcMerchandiser, "freight.bill", "合成货代账单：provider.1 海运费 60.00 USD，order.1 / shipment.1。")
	r = c.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmCharge, map[string]any{"charge_id": "freight.brc", "expected_version": 1,
		"evidence_ids": []string{"freight.bill"}})
	if r["status"] != "confirmed" {
		c.t.Fatalf("charge.confirm %v", r)
	}
	c.cmd(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandAllocateCharge, map[string]any{"charge_id": "freight.brc", "expected_version": 2,
		"basis": "amount", "data_basis": "单一受益订单 order.1，全额", "lines": []any{map[string]any{"line_id": "A1",
			"target": map[string]any{"kind": "order", "order_id": "order.1"}, "weight": "1"}}})
	v := c.get(brcTreasury, "/api/brant/v1/stl/charges/freight.brc")
	if v["status"] != "confirmed" || v["amount_minor"] != "6000" || brcDig(v, "allocations", 0, "lines", 0, "amount_minor") != "6000" {
		c.t.Fatalf("charge %v", v)
	}
	c.fact("status", v["status"], "amount_minor", v["amount_minor"], "estimate_minor", v["estimate_minor"], "allocation_seq", v["allocation_seq"])
	c.record.Amounts["freight_charge"] = brcMajor(c.t, "6000") + " USD"
}

// result is §5 step 27 (STL-INV-011, BT18; baseline 4.5).
func (c *brcS5) result() {
	c.step(27, "STL order result")
	r := c.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandReviewOrderResult, map[string]any{"order_id": "order.1", "expected_version": 0,
		"note": "四项来源核对：销售 960.00、采购 600.00、运费 60.00（合成）"})
	r = c.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandCloseOrderResult, map[string]any{"order_id": "order.1",
		"expected_version": r["row_version"], "reason": "BRC-001 订单经营结果结清"})
	if r["status"] != "settled" {
		c.t.Fatalf("order_result.close %v", r)
	}
	v := c.get(brcTreasury, "/api/brant/v1/stl/orders/order.1/result")
	confirmed := brcMap(brcDig(v, "live", "confirmed"))
	snapshot := brcMap(brcDig(v, "snapshots", 0, "figures", "confirmed"))
	var claims any
	for _, cond := range brcList(v["conditions"]) {
		if brcDig(cond, "dimension") == "claims" {
			claims = brcDig(cond, "status")
		}
	}
	if v["status"] != "settled" || confirmed["result_minor"] != "30000" || confirmed["margin"] != "31.25" || snapshot["result_minor"] != "30000" ||
		confirmed["net_sales_minor"] != "96000" || claims != "not_enabled" {
		c.t.Fatalf("order result %v", v)
	}
	c.fact("status", v["status"], "net_sales_minor", confirmed["net_sales_minor"], "cost_of_sales_minor", brcDig(confirmed, "batches", 0, "cost_of_sales_minor"),
		"direct_charges_minor", confirmed["direct_charges_minor"], "result_minor", confirmed["result_minor"], "margin", confirmed["margin"],
		"forecast_result_minor", brcDig(v, "live", "forecast", "result_minor"), "claims", claims, "snapshot_sha256", brcDig(v, "snapshots", 0, "content_sha256"))
	// Equal to the quote's calculation: profit 300.00 on 960.00 (S3 chain, quote.freeze).
	if got := brcMajor(c.t, brcStr(confirmed["result_minor"])) + " USD"; got != c.record.Amounts["quote_profit"] {
		c.t.Fatalf("confirmed result %s, the quote's profit %s", got, c.record.Amounts["quote_profit"])
	}
	c.record.Amounts["order_result"] = brcMajor(c.t, "30000") + " USD (31.25 %)"
}

// finalAssertions are §5's final assertions, read again at the end of the chain.
func (c *brcS5) finalAssertions() {
	c.phase = "§5 final assertions"
	o := c.get(brcSales, "/api/brant/v1/trd/orders/order.1")
	var snapshot map[string]any
	c.w.must(json.Unmarshal([]byte(brcStr(o["snapshot"])), &snapshot))
	if o["status"] != trdapi.OrderClosed || brcDig(snapshot, "calculation", "payable_minor") != "96000" {
		c.t.Fatalf("order %v", o["status"])
	}
	for i, commit := range brcList(o["commits"]) {
		if u := brcList(brcDig(commit, "unchecked_guards")); len(u) != 0 {
			c.t.Fatalf("order commit %d has unchecked guards %v", i, u)
		}
	}
	c.fact("order_status", o["status"], "order_payable_minor", "96000", "commits", len(brcList(o["commits"])))
	v := c.reservation()
	if v["status"] != stlapi.ReservationReleased || v["portion_minor"] != "67200" || v["version"] != float64(3) {
		c.t.Fatalf("reservation %v", v)
	}
	c.fact("reservation", "reserved → consumed → released", "portion_minor", v["portion_minor"])
	s := c.get(brcMerchandiser, "/api/brant/v1/ful/shipments/"+c.shipmentID)
	if u := brcList(brcDig(s, "confirmation", "unchecked_guards")); len(u) != 0 {
		c.t.Fatalf("shipment confirmation unchecked guards %v", u)
	}
	rec := c.get(brcTreasury, "/api/brant/v1/stl/receivables/stmt.sales.1")
	pay := c.get(brcTreasury, "/api/brant/v1/stl/payables/stmt.purchase.1")
	if rec["amount_minor"] != "96000" || rec["open_minor"] != "0" || pay["amount_minor"] != "60000" || pay["open_minor"] != "0" {
		c.t.Fatalf("receivable %v payable %v", rec, pay)
	}
	c.fact("receivable", "96000 → 0", "payable", "60000 → 0")
	res := c.get(brcTreasury, "/api/brant/v1/stl/orders/order.1/result")
	if res["status"] != "settled" || brcDig(res, "live", "confirmed", "result_minor") != "30000" {
		c.t.Fatalf("order result %v", res)
	}
	c.fact("order_result", "settled 30000 (31.25 %)", "events_delivered", len(c.delivered))
	// No unchecked guard in any commit evidence: no event names one (TRD's OrderActivated and
	// OrderAmended, FUL's ShipmentConfirmed carry unchecked_guards), and no evidence entry the
	// reads show is not_wired.
	var types []string
	withMember := 0
	for _, e := range c.delivered {
		types = append(types, string(e.Type))
		var payload map[string]any
		c.w.must(json.Unmarshal(e.Payload, &payload))
		if u, ok := payload["unchecked_guards"]; ok {
			withMember++
			if brcStr(u) != "" || len(brcList(u)) != 0 {
				c.t.Fatalf("%s names unchecked guards %v", e.Type, u)
			}
		}
		if strings.Contains(string(e.Payload), trdapi.NotWired) {
			c.t.Fatalf("%s carries %s: %s", e.Type, trdapi.NotWired, e.Payload)
		}
	}
	for _, want := range []string{trdapi.EventOrderActivated, trdapi.EventOrderClosed, fulapi.EventShipmentConfirmed} {
		if !slices.Contains(types, want) {
			c.t.Fatalf("no %s among the delivered events %v", want, types)
		}
	}
	entries := 0
	for _, commit := range brcList(o["commits"]) {
		for name, value := range brcMap(brcDig(commit, "entries")) {
			entries++
			if value == trdapi.NotWired {
				c.t.Fatalf("order commit entry %s is %v", name, value)
			}
		}
	}
	for _, g := range brcList(brcDig(s, "confirmation", "guards")) {
		entries++
		if brcDig(g, "value") == trdapi.NotWired {
			c.t.Fatalf("shipment confirmation guard %v", g)
		}
	}
	c.fact("events_with_unchecked_guards_member", withMember, "unchecked_guards", 0, "commit_entries_checked", entries, "not_wired", 0)
	// The facts §5 names as each step's outcome, each published as often as the chain caused it.
	count := map[string]int{}
	for _, e := range c.delivered {
		count[string(e.Type)]++
		if e.Type == stlapi.EventOrderResultClosed {
			var payload map[string]any
			c.w.must(json.Unmarshal(e.Payload, &payload))
			if payload["claims"] != "not_enabled" || payload["order_id"] != "order.1" {
				c.t.Fatalf("OrderResultClosed %s", e.Payload)
			}
		}
	}
	outcomes := []struct {
		step  int
		event string
		n     int
	}{{2, "idp-brant.stl.credit_limit_published.v1", 1}, {3, "idp-brant.stl.credit_reserved.v1", 1},
		{4, "idp-brant.cat.declaration_profile_verified.v1", 4}, {5, "idp-brant.cnx.trade_compliance_decided.v1", 1},
		{6, trdapi.EventOrderActivated, 1}, {6, stlapi.EventCreditReservationConsumed, 1}, {7, "idp-brant.stl.applied.v1", 4},
		{8, "idp-brant.pur.purchase_commitment_approved.v1", 1}, {8, "idp-brant.pur.purchase_commitment_issued.v1", 1},
		{8, "idp-brant.pur.supplier_confirmation_recorded.v1", 1}, {9, "idp-brant.qlt.quality_released.v1", 4},
		{10, "idp-brant.ful.milestone_actual_recorded.v1", 1}, {11, "idp-brant.pur.supplier_delivery_verified.v1", 1},
		{13, "idp-brant.ful.commercial_release_granted.v1", 2}, {14, fulapi.EventShipmentConfirmed, 1},
		{15, "idp-brant.ful.shipment_document_issued.v1", 1}, {16, "idp-brant.ful.documents_released.v1", 1},
		{17, "idp-brant.ful.export_lot_registered.v1", 1}, {18, "idp-brant.cnx.declaration_receipt_recorded.v1", 1},
		{19, "idp-brant.stl.settlement_confirmed.v1", 2}, {20, "idp-brant.stl.fund_difference_resolved.v1", 1},
		{22, "idp-brant.cnx.purchase_invoice_verified.v1", 1}, {23, "idp-brant.cnx.tax_evidence_allocated.v1", 1},
		{24, "idp-brant.cnx.document_bundle_checked.v1", 1}, {25, trdapi.EventOrderClosed, 1},
		{25, stlapi.EventCreditReservationReleased, 1}, {26, "idp-brant.stl.charge_allocated.v1", 1}, {27, stlapi.EventOrderResultClosed, 1}}
	var seen []string
	for _, o := range outcomes {
		if count[o.event] != o.n {
			c.t.Fatalf("§5 step %d: %d × %s, want %d", o.step, count[o.event], o.event, o.n)
		}
		seen = append(seen, fmt.Sprintf("%d: %d × %s", o.step, o.n, o.event))
	}
	c.fact("outcome_events", seen)
}
