//go:build integration

package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// PUR supplier deliveries on the direct delivery route (WBS S5-15; tactical model
// PUR-AGG-SupplierDelivery, SM-PUR-SUPPLIER-DELIVERY, PUR-INV-008; UL-INV-32; fact authority MD-45;
// event storming ES-12; TD-AT14) on PostgreSQL 16, on the PUR worlds of S3 and S5-01: newPurWorld
// (order.1 through PUR's port to an in-memory TRD) and TRD's real orders through the shared order
// world. Python V0.50 has no supplier delivery and no test to port: the tests cite the design IDs.
// The fixture's commitment buys line L1, 120 PCS of sku.1 from supplier.1 at 5.00 USD, 600.00 USD.

var (
	purHandover   = []purapi.Evidence{{SourceID: "evidence", Locator: "合成货代收货单"}}
	purCorrection = "合成：货代收货单更正件数"
)

// purDeliver drives the supplier deliveries of the module m returns, in world w, against commitments
// it has issued through p.
type purDeliver struct {
	w *scenWorld
	m func() *pur.Module
	p *purCommit
}

func newPurDeliver(w *scenWorld, m func() *pur.Module) *purDeliver {
	return &purDeliver{w: w, m: m, p: newPurCommit(w, m)}
}

// issued requests commitment id of draft d as the author, has the reviewer approve it and the author
// issue it: the commitment is issued at row version 3.
func (x *purDeliver) issued(id string, d purapi.DraftReceipt, acks ...purapi.Acknowledgement) {
	x.w.t.Helper()
	_, err := x.p.request(x.w.author, id, d.ObjectID, d.DraftHash)
	x.w.must(err)
	_, err = x.p.approve(x.w.reviewer, id, 1, d.DraftHash, acks...)
	x.w.must(err)
	_, err = x.p.issue(x.w.author, id, 2)
	x.w.must(err)
}

// claim is a record of a new delivery id against commitment: quantity of line L1 handed over at
// Ningbo a minute ago, on the forwarder's cargo receipt.
func (x *purDeliver) claim(id, commitment, quantity string) purapi.RecordSupplierDelivery {
	return purapi.RecordSupplierDelivery{IdempotencyKey: x.w.key(), DeliveryID: id, CommitmentID: commitment,
		Lines: []purapi.DeliveryClaim{{OrderLineID: "L1", Quantity: quantity}}, HandoverPlace: "CNNGB",
		HandedOverAt: kernel.FormatTimestamp(time.Now().Add(-time.Minute)), Evidence: purHandover, SupplierReference: "DN-2026-17"}
}

func (x *purDeliver) record(ctx context.Context, id, commitment, quantity string) (purapi.DeliveryReceipt, error) {
	return x.m().RecordSupplierDelivery(ctx, x.claim(id, commitment, quantity))
}

// correct records the next version of delivery id, at row version expected.
func (x *purDeliver) correct(ctx context.Context, id, commitment, quantity string, expected int64) (purapi.DeliveryReceipt, error) {
	cmd := x.claim(id, commitment, quantity)
	cmd.ExpectedVersion, cmd.Reason = expected, purCorrection
	return x.m().RecordSupplierDelivery(ctx, cmd)
}

func (x *purDeliver) verifyCmd(id string, expected int64, quantity string) purapi.VerifySupplierDelivery {
	return purapi.VerifySupplierDelivery{IdempotencyKey: x.w.key(), DeliveryID: id, ExpectedVersion: expected, Decision: purapi.DecisionVerify,
		Lines: []purapi.DeliveryVerifiedLine{{OrderLineID: "L1", VerifiedQuantity: quantity}}}
}

func (x *purDeliver) verify(ctx context.Context, id string, expected int64, quantity string) (purapi.DeliveryReceipt, error) {
	return x.m().VerifySupplierDelivery(ctx, x.verifyCmd(id, expected, quantity))
}

func (x *purDeliver) reject(ctx context.Context, id string, expected int64) (purapi.DeliveryReceipt, error) {
	return x.m().VerifySupplierDelivery(ctx, purapi.VerifySupplierDelivery{IdempotencyKey: x.w.key(), DeliveryID: id, ExpectedVersion: expected,
		Decision: purapi.DecisionReject, Reason: "合成：收货单与上报不符，证据不足"})
}

func (x *purDeliver) view(ctx context.Context, id string) purapi.DeliveryView {
	x.w.t.Helper()
	v, err := x.m().GetDelivery(ctx, id)
	x.w.must(err)
	return v
}

// standing returns what the deliveries of commitment add up to for its line L1: purchased, verified
// and remaining, as supplier_delivery.list shows them.
func (x *purDeliver) standing(ctx context.Context, commitment string) string {
	x.w.t.Helper()
	v, err := x.m().GetCommitmentDeliveries(ctx, commitment)
	x.w.must(err)
	if len(v.Lines) != 1 {
		x.w.t.Fatalf("lines %+v", v.Lines)
	}
	l := v.Lines[0]
	return l.PurchaseQuantity + " " + l.VerifiedQuantity + " " + l.RemainingQuantity
}

func deliveryRef(id string, version int64) purapi.DeliveryLineRef {
	return purapi.DeliveryLineRef{DeliveryID: id, LineID: "L1", Version: version}
}

// guard runs SupplierDeliveryGuard v1 (R59) for ref as the application role in company's scope.
func (x *purDeliver) guard(company string, ref purapi.DeliveryLineRef) purapi.SupplierDeliveryVerdict {
	x.w.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(x.w.t))
	x.w.must(err)
	var v purapi.SupplierDeliveryVerdict
	x.w.must(bento.ScopeTransactor{DB: db, Scope: x.w.scope(company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		v, err = purapi.ScanSupplierDeliveryGuard(ref, exec.QueryRow(ctx, purapi.SupplierDeliveryGuardSQL, purapi.SupplierDeliveryGuardArgs(ref)...).Scan)
		return err
	}))
	return v
}

// verifiedEvents returns the payloads of the SupplierDeliveryVerified events in company.A's Outbox.
func (x *purDeliver) verifiedEvents() []map[string]any {
	x.w.t.Helper()
	var out []map[string]any
	for _, e := range x.w.events("company.A", purapi.Source) {
		if e.Type == purapi.EventSupplierDeliveryVerified {
			out = append(out, e.Payload)
		}
	}
	return out
}

// TD-AT14, PUR half ("直发：承诺 120 件，核实交付 100 件，另有未核实上报 20 件 … 未核实上报不计入"), ES-12 steps 1
// and 2, PUR-INV-008 positive ("承诺 120 件，核实交付 100 件") and UL-INV-32 ("供应商上报的交付只是声明"): an AI
// agent records the supplier's claim of 120 PCS handed to the forwarder at Ningbo, with the cargo
// receipt as an EVD file — a claim, which the guard refuses and nothing counts; the purchaser
// verifies the 100 PCS the receipt shows, which become the fact FUL may ship and STL may settle; a
// second claim of 20 stays a claim. The record is replayed by its key and answered from the stored
// delivery under another; SupplierDeliveryVerified is in the Outbox, a record publishes nothing; the
// database keeps the verified version as it is.
func TestSupplierDeliveryCountsOnlyWhatAHumanVerifiedTDAT14(t *testing.T) {
	w := newPurWorld(t)
	x := newPurDeliver(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	x.issued("commitment.1", d)
	file, err := w.file(w.author, "receipt.1", "cargo-receipt.txt", evdapi.MimeText, "合成货代收货单：100 件", "")
	w.must(err)
	agent := w.other("delivery.agent", kernel.ActorAI, allPermissions...)
	cmd := x.claim("delivery.1", "commitment.1", "120")
	cmd.FileIDs = []string{file.ObjectID}
	r, err := w.pur.RecordSupplierDelivery(agent, cmd)
	w.must(err)
	if r.Status != purapi.DeliveryRecorded || r.RowVersion != 1 || r.Replayed || r.ObjectID != "delivery.1" || r.CommitmentID != "commitment.1" ||
		r.OrderID != "order.1" || r.SupplierID != "supplier.1" || r.LatestVersion != 1 || r.VerifiedVersion != 0 {
		t.Fatalf("record %+v", r)
	}
	again, err := w.pur.RecordSupplierDelivery(agent, cmd)
	if err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("replay %+v, %v", again, err)
	}
	cmd.IdempotencyKey = w.key()
	if answered, err := w.pur.RecordSupplierDelivery(agent, cmd); err != nil || !answered.Replayed || answered.ActionID == r.ActionID ||
		answered.Status != purapi.DeliveryRecorded {
		t.Fatalf("answered from the stored delivery %+v, %v", answered, err)
	}
	other := cmd
	other.IdempotencyKey, other.Lines = w.key(), []purapi.DeliveryClaim{{OrderLineID: "L1", Quantity: "121"}}
	_, err = w.pur.RecordSupplierDelivery(agent, other)
	w.want(err, kernel.CodeVersionConflict)
	cmd.IdempotencyKey = w.key()
	_, err = w.pur.RecordSupplierDelivery(w.author, cmd)
	w.want(err, kernel.CodeVersionConflict)

	// The claim never counts (UL-INV-32).
	if v := x.guard("company.A", deliveryRef("delivery.1", 1)); !v.Known || v.Usable() || v.Status != purapi.DeliveryRecorded ||
		v.Code != purapi.CodeSupplierDeliveryUnverified || v.VerifiedQuantity != "" || v.VerifiedVersion != 0 {
		t.Fatalf("guard on a claim %+v", v)
	}
	if got := x.standing(w.author, "commitment.1"); got != "120 0 120" {
		t.Fatalf("standing with a claim only: %s", got)
	}
	if e := x.verifiedEvents(); len(e) != 0 {
		t.Fatalf("a claim is no fact: %v", e)
	}

	// The purchaser verifies what the forwarder's receipt shows: 100 of the 120 claimed (ES-12 step 2).
	v, err := x.verify(w.author, "delivery.1", 1, "100")
	w.must(err)
	if v.Status != purapi.DeliveryVerified || v.RowVersion != 2 || v.LatestVersion != 1 || v.VerifiedVersion != 1 {
		t.Fatalf("verify %+v", v)
	}
	merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
	_, err = x.record(merch, "delivery.2", "commitment.1", "20")
	w.must(err)

	// Committed 120, verified 100, an unverified claim of 20.
	g := x.guard("company.A", deliveryRef("delivery.1", 1))
	if !g.Usable() || g.Status != purapi.DeliveryVerified || g.VerifiedQuantity != "100" || g.VerifiedVersion != 1 || g.CommitmentID != "commitment.1" ||
		g.OrderID != "order.1" || g.OrderLineID != "L1" || g.SupplierID != "supplier.1" || g.SkuID != "sku.1" || g.SpecRevision != w.spec ||
		g.UnitID != "PCS" || g.UnitRevision != w.pcs.Revision || g.Check("/x") != nil {
		t.Fatalf("guard on the verified delivery %+v", g)
	}
	if c := x.guard("company.A", deliveryRef("delivery.2", 1)); c.Usable() || c.Code != purapi.CodeSupplierDeliveryUnverified ||
		code(c.Check("/args/lines/0/supplier_delivery")) != purapi.CodeSupplierDeliveryUnverified || path(c.Check("/args/lines/0/supplier_delivery")) != "/args/lines/0/supplier_delivery" {
		t.Fatalf("guard on the second claim %+v", c)
	}
	if got := x.standing(w.author, "commitment.1"); got != "120 100 20" {
		t.Fatalf("standing: %s", got)
	}
	dv := x.view(w.reviewer, "delivery.1")
	if dv.Status != purapi.DeliveryVerified || dv.RowVersion != 2 || dv.VerifiedVersion != 1 || dv.PendingVersion != 0 || len(dv.Versions) != 1 ||
		!strings.HasPrefix(dv.PONumber, "PO-") || dv.CommitmentID != "commitment.1" || dv.OrderID != "order.1" {
		t.Fatalf("view %+v", dv)
	}
	ver := dv.Versions[0]
	if ver.Status != purapi.DeliveryVerified || ver.HandoverPlace != "CNNGB" || ver.SupplierReference != "DN-2026-17" || ver.CorrectionReason != "" ||
		ver.RecordedBy != actorRef("ai", "delivery.agent") || ver.DecidedBy != actorRef("human", "editor") || ver.Decision != purapi.DecisionVerify ||
		len(ver.Evidence) != 1 || len(ver.Files) != 1 || ver.Files[0].FileID != "receipt.1" || len(ver.Files[0].SHA256) != 64 ||
		len(ver.Lines) != 1 || ver.Lines[0].OrderLineID != "L1" || ver.Lines[0].ClaimedQuantity != "120" || ver.Lines[0].VerifiedQuantity != "100" ||
		ver.Lines[0].SkuID != "sku.1" || ver.Lines[0].UnitID != "PCS" || ver.DecidedAt.Before(ver.RecordedAt) || ver.HandedOverAt.After(ver.RecordedAt) {
		t.Fatalf("version %+v", ver)
	}
	if claim := x.view(w.reviewer, "delivery.2"); claim.Status != purapi.DeliveryRecorded || claim.PendingVersion != 1 || claim.VerifiedVersion != 0 ||
		claim.Versions[0].Lines[0].VerifiedQuantity != "" || claim.Versions[0].RecordedBy.ID != "merchandiser" {
		t.Fatalf("the claim %+v", claim)
	}
	events := x.verifiedEvents()
	if len(events) != 1 || events[0]["delivery_id"] != "delivery.1" || events[0]["delivery_version"] != float64(1) ||
		events[0]["superseded_version"] != float64(0) || events[0]["commitment_id"] != "commitment.1" || events[0]["order_id"] != "order.1" ||
		events[0]["supplier_id"] != "supplier.1" || events[0]["handover_place"] != "CNNGB" || events[0]["verified_by"] != "editor" ||
		events[0]["aggregate_type"] != "SupplierDelivery" || events[0]["aggregate_version"] != float64(2) {
		t.Fatalf("events %v", events)
	}
	if lines, _ := events[0]["lines"].([]any); len(lines) != 1 || lines[0].(map[string]any)["order_line_id"] != "L1" ||
		lines[0].(map[string]any)["verified_quantity"] != "100" {
		t.Fatalf("event lines %v", events[0]["lines"])
	}
	audit, err := w.pur.Audit(w.reviewer)
	w.must(err)
	var trail []string
	for _, e := range audit {
		if strings.HasPrefix(e.Command, "supplier_delivery.") {
			trail = append(trail, e.Command+" "+e.ObjectID+" "+e.Outcome+" "+e.Code)
		}
	}
	if want := []string{"supplier_delivery.record delivery.1 succeeded ", "supplier_delivery.record delivery.1 replayed ",
		"supplier_delivery.record delivery.1 succeeded ", "supplier_delivery.record delivery.1 rejected VERSION_CONFLICT",
		"supplier_delivery.record delivery.1 rejected VERSION_CONFLICT", "supplier_delivery.list commitment.1 read ",
		"supplier_delivery.verify delivery.1 succeeded ", "supplier_delivery.record delivery.2 succeeded ",
		"supplier_delivery.list commitment.1 read ", "supplier_delivery.get delivery.1 read ", "supplier_delivery.get delivery.2 read "}; !slices.Equal(trail, want) {
		t.Fatalf("audit %q", trail)
	}
	// Nothing verified is edited or deleted: the application role has no such grant, and the triggers
	// refuse the schema owner too.
	for _, c := range []struct{ sql, want string }{
		{"UPDATE brant_pur.supplier_delivery_line SET verified_quantity = 120 WHERE delivery_id = 'delivery.1'", "23000"},
		{"UPDATE brant_pur.supplier_delivery_line SET claimed_quantity = 100", "42501"},
		{"UPDATE brant_pur.supplier_delivery_version SET status = 'recorded' WHERE delivery_id = 'delivery.1'", "23000"},
		{"UPDATE brant_pur.supplier_delivery_version SET handover_place = 'CNSHA'", "42501"},
		{"UPDATE brant_pur.supplier_delivery SET verified_version = NULL, row_version = row_version + 1 WHERE delivery_id = 'delivery.1'", "23000"},
		{"UPDATE brant_pur.supplier_delivery SET commitment_id = 'x'", "42501"},
		{"DELETE FROM brant_pur.supplier_delivery_line", "42501"},
		{"DELETE FROM brant_pur.supplier_delivery_version", "42501"},
		{"DELETE FROM brant_pur.supplier_delivery", "42501"},
	} {
		if got := sqlCode(w.exec("company.A", c.sql)); got != c.want {
			t.Errorf("%s: %s, want %s", c.sql, got, c.want)
		}
	}
	for _, sql := range []string{
		"UPDATE brant_pur.supplier_delivery_line SET claimed_quantity = 100 WHERE delivery_id = 'delivery.1'",
		"UPDATE brant_pur.supplier_delivery_version SET handover_place = 'CNSHA' WHERE delivery_id = 'delivery.1'",
		"UPDATE brant_pur.supplier_delivery SET commitment_id = 'x' WHERE delivery_id = 'delivery.1'",
		"DELETE FROM brant_pur.supplier_delivery_line WHERE delivery_id = 'delivery.1'",
		"DELETE FROM brant_pur.supplier_delivery_version WHERE delivery_id = 'delivery.1'",
		"DELETE FROM brant_pur.supplier_delivery WHERE delivery_id = 'delivery.1'",
	} {
		if got := sqlCode(w.purOwner(sql)); got != "23000" && got != "23503" {
			t.Errorf("owner: %s: %s", sql, got)
		}
	}
}

// refusedAt fails the test unless err is the rejection wantCode at wantPath.
func refusedAt(t *testing.T, what string, err error, wantCode, wantPath string) {
	t.Helper()
	if code(err) != wantCode || path(err) != wantPath {
		t.Fatalf("%s: %s at %s, want %s at %s (%v)", what, code(err), path(err), wantCode, wantPath, err)
	}
}

// PUR-INV-008 ("直发路线的供应商交付按采购承诺行登记数量、交接地点与时间、证据") and SM-PUR-SUPPLIER-DELIVERY for the
// record: a delivery is recorded against a commitment the recorder sees, once it is issued (or
// confirmed), for lines the commitment has, each once, with a UN/LOCODE, a handover that is not in
// the future, and evidence and files the recorder reads; by a human or an AI holding the permission,
// never a system actor. A correction is the delivery's next version: with a reason, at the
// delivery's row version, against the same commitment, once no claim awaits verification. Nothing
// refused is stored.
func TestSupplierDeliveryIsRecordedAgainstIssuedCommitmentLines(t *testing.T) {
	w := newPurWorld(t)
	x := newPurDeliver(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
	_, err = x.record(merch, "delivery.1", "commitment.1", "120")
	refusedAt(t, "no such commitment", err, kernel.CodeNotFound, "/args/commitment_id")
	_, err = x.p.request(w.author, "commitment.1", d.ObjectID, d.DraftHash)
	w.must(err)
	_, err = x.record(merch, "delivery.1", "commitment.1", "120")
	refusedAt(t, "a requested commitment", err, kernel.CodeInvalidState, "/args/commitment_id")
	_, err = x.p.approve(w.reviewer, "commitment.1", 1, d.DraftHash)
	w.must(err)
	_, err = x.record(merch, "delivery.1", "commitment.1", "120")
	refusedAt(t, "an approved commitment", err, kernel.CodeInvalidState, "/args/commitment_id")
	_, err = x.p.issue(w.author, "commitment.1", 2)
	w.must(err)

	bad := func(ctx context.Context, edit func(c *purapi.RecordSupplierDelivery)) error {
		c := x.claim("delivery.1", "commitment.1", "120")
		edit(&c)
		_, err := w.pur.RecordSupplierDelivery(ctx, c)
		return err
	}
	for _, c := range []struct {
		what       string
		edit       func(c *purapi.RecordSupplierDelivery)
		code, path string
	}{
		{"a line the commitment does not have", func(c *purapi.RecordSupplierDelivery) { c.Lines[0].OrderLineID = "L9" }, kernel.CodeReferenceInvalid, "/args/lines/0/order_line_id"},
		{"a line twice", func(c *purapi.RecordSupplierDelivery) { c.Lines = append(c.Lines, c.Lines[0]) }, kernel.CodeDuplicateLineID, "/args/lines/1/order_line_id"},
		{"no quantity", func(c *purapi.RecordSupplierDelivery) { c.Lines[0].Quantity = "0" }, kernel.CodeSchemaInvalid, "/args/lines/0/quantity"},
		{"no line", func(c *purapi.RecordSupplierDelivery) { c.Lines = nil }, kernel.CodeSchemaInvalid, "/args/lines"},
		{"a place that is no UN/LOCODE", func(c *purapi.RecordSupplierDelivery) { c.HandoverPlace = "Ningbo" }, kernel.CodeSchemaInvalid, "/args/handover_place"},
		{"a handover in the future", func(c *purapi.RecordSupplierDelivery) {
			c.HandedOverAt = kernel.FormatTimestamp(time.Now().Add(time.Hour))
		},
			kernel.CodeInvalidKnownValue, "/args/handed_over_at"},
		{"no evidence", func(c *purapi.RecordSupplierDelivery) { c.Evidence = nil }, kernel.CodeSchemaInvalid, "/args/evidence"},
		{"evidence nobody registered", func(c *purapi.RecordSupplierDelivery) {
			c.Evidence = []purapi.Evidence{{SourceID: "missing.source", Locator: "x"}}
		},
			kernel.CodeReferenceInvalid, "/args/evidence/0/source_id"},
		{"a file nobody registered", func(c *purapi.RecordSupplierDelivery) { c.FileIDs = []string{"missing.file"} }, kernel.CodeReferenceInvalid, "/args/file_ids/0"},
		{"a file twice", func(c *purapi.RecordSupplierDelivery) { c.FileIDs = []string{"f.1", "f.1"} }, kernel.CodeSchemaInvalid, "/args/file_ids/1"},
		{"a reason on a new delivery", func(c *purapi.RecordSupplierDelivery) { c.Reason = purCorrection }, kernel.CodeSchemaInvalid, "/args/reason"},
		{"a correction without a reason", func(c *purapi.RecordSupplierDelivery) { c.ExpectedVersion = 1 }, kernel.CodeSchemaInvalid, "/args/reason"},
		{"a correction of no delivery", func(c *purapi.RecordSupplierDelivery) { c.ExpectedVersion, c.Reason = 1, purCorrection }, kernel.CodeNotFound, "/args/delivery_id"},
	} {
		refusedAt(t, c.what, bad(merch, c.edit), c.code, c.path)
	}
	keep := func(*purapi.RecordSupplierDelivery) {}
	w.want(bad(w.other("delivery.system", kernel.ActorSystem, allPermissions...), keep), kernel.CodePermissionDenied)
	w.want(bad(w.provision(bootstrapGrant(w.supWorld, "delivery.limited", "company", without(allPermissions, purapi.CommandRecordSupplierDelivery)...)), keep),
		kernel.CodePermissionDenied)
	outsider := w.provision(bootstrapGrant(w.supWorld, "delivery.own", "own", allPermissions...))
	refusedAt(t, "outside the commitment's data scope", bad(outsider, keep), kernel.CodeNotFound, "/args/commitment_id")
	if n := w.count("company.A", "brant_pur.supplier_delivery"); n != 0 {
		t.Fatalf("nothing refused is stored: %d", n)
	}

	r, err := x.record(merch, "delivery.1", "commitment.1", "120")
	w.must(err)
	if r.Status != purapi.DeliveryRecorded || r.RowVersion != 1 {
		t.Fatalf("record %+v", r)
	}
	_, err = x.correct(merch, "delivery.1", "commitment.1", "110", 1)
	refusedAt(t, "a correction while a claim awaits verification", err, kernel.CodeInvalidState, "/args/delivery_id")
	_, err = x.reject(w.author, "delivery.1", 1)
	w.must(err)
	_, err = x.correct(merch, "delivery.1", "commitment.1", "110", 1)
	refusedAt(t, "a stale row version", err, kernel.CodeVersionConflict, "/args/expected_version")
	_, err = x.correct(merch, "delivery.1", "commitment.x", "110", 2)
	refusedAt(t, "another commitment", err, kernel.CodeReferenceInvalid, "/args/commitment_id")
	_, err = x.correct(outsider, "delivery.1", "commitment.1", "110", 2)
	refusedAt(t, "outside the delivery's data scope", err, kernel.CodeNotFound, "/args/delivery_id")
	c, err := x.correct(merch, "delivery.1", "commitment.1", "110", 2)
	w.must(err)
	if c.Status != purapi.DeliveryRecorded || c.RowVersion != 3 || c.LatestVersion != 2 || c.VerifiedVersion != 0 {
		t.Fatalf("the next claim %+v", c)
	}
	v := x.view(w.author, "delivery.1")
	if v.Status != purapi.DeliveryRecorded || v.PendingVersion != 2 || len(v.Versions) != 2 || v.Versions[0].Status != purapi.DeliveryRejected ||
		v.Versions[0].DecisionReason == "" || v.Versions[1].CorrectionReason != purCorrection || v.Versions[1].Lines[0].ClaimedQuantity != "110" {
		t.Fatalf("view %+v", v)
	}
	// A commitment the supplier confirmed takes deliveries like an issued one.
	_, err = x.p.confirm(w.author, "commitment.1", 3, "")
	w.must(err)
	if r, err = x.record(merch, "delivery.2", "commitment.1", "10"); err != nil || r.Status != purapi.DeliveryRecorded {
		t.Fatalf("a confirmed commitment: %+v, %v", r, err)
	}
	if deliveries, versions := w.count("company.A", "brant_pur.supplier_delivery"), w.count("company.A", "brant_pur.supplier_delivery_version"); deliveries != 2 || versions != 3 {
		t.Fatalf("stored: %d deliveries, %d versions", deliveries, versions)
	}
}

// PUR-INV-008 ("经授权人工核实后才计入"), X-INV-002 and the role templates' independence row
// (supplier_delivery.verify independentOf 交付的登记人): the recorder of a claim does not verify it
// (INDEPENDENT_REVIEW_REQUIRED); an AI agent and a system actor with every permission never verify
// (PERMISSION_DENIED), nor does a human without the permission, nor one outside the data scope
// (NOT_FOUND). A verification states every line of the claim, nothing above the claimed quantity,
// and something above zero; the verifier reads its own evidence and every source the claim cites. A
// rejection needs a reason (SM-PUR-SUPPLIER-DELIVERY: 证据不足；理由必填), counts nothing and is final
// for the claim. Every refusal is audited.
func TestSupplierDeliveryVerificationIsIndependentAndHuman(t *testing.T) {
	w := newPurWorld(t)
	x := newPurDeliver(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	x.issued("commitment.1", d)
	merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
	_, err = x.record(merch, "delivery.1", "commitment.1", "120")
	w.must(err)

	_, err = x.verify(merch, "delivery.1", 1, "120")
	refusedAt(t, "the recorder", err, purapi.CodeIndependentReviewRequired, "/args/delivery_id")
	_, err = x.reject(merch, "delivery.1", 1)
	refusedAt(t, "the recorder rejecting", err, purapi.CodeIndependentReviewRequired, "/args/delivery_id")
	for _, kind := range []kernel.ActorKind{kernel.ActorAI, kernel.ActorSystem} {
		_, err = x.verify(w.other("verify."+string(kind), kind, allPermissions...), "delivery.1", 1, "120")
		w.want(err, kernel.CodePermissionDenied)
	}
	_, err = x.verify(w.provision(bootstrapGrant(w.supWorld, "verify.limited", "company", without(allPermissions, purapi.CommandVerifySupplierDelivery)...)), "delivery.1", 1, "120")
	w.want(err, kernel.CodePermissionDenied)
	_, err = x.verify(w.provision(bootstrapGrant(w.supWorld, "verify.own", "own", allPermissions...)), "delivery.1", 1, "120")
	refusedAt(t, "outside the data scope", err, kernel.CodeNotFound, "/args/delivery_id")
	_, err = x.verify(w.author, "delivery.none", 1, "120")
	refusedAt(t, "no such delivery", err, kernel.CodeNotFound, "/args/delivery_id")
	_, err = x.verify(w.author, "delivery.1", 2, "120")
	refusedAt(t, "a stale row version", err, kernel.CodeVersionConflict, "/args/expected_version")

	bad := func(edit func(c *purapi.VerifySupplierDelivery)) error {
		c := x.verifyCmd("delivery.1", 1, "100")
		edit(&c)
		_, err := w.pur.VerifySupplierDelivery(w.author, c)
		return err
	}
	for _, c := range []struct {
		what       string
		edit       func(c *purapi.VerifySupplierDelivery)
		code, path string
	}{
		{"more than was claimed", func(c *purapi.VerifySupplierDelivery) { c.Lines[0].VerifiedQuantity = "120.000001" }, kernel.CodeInvalidKnownValue,
			"/args/lines/0/verified_quantity"},
		{"a line the claim does not have", func(c *purapi.VerifySupplierDelivery) { c.Lines[0].OrderLineID = "L9" }, kernel.CodeReferenceInvalid,
			"/args/lines/0/order_line_id"},
		{"nothing verified", func(c *purapi.VerifySupplierDelivery) { c.Lines[0].VerifiedQuantity = "0" }, kernel.CodeInvalidKnownValue, "/args/lines"},
		{"a negative quantity", func(c *purapi.VerifySupplierDelivery) { c.Lines[0].VerifiedQuantity = "-1" }, kernel.CodeSchemaInvalid,
			"/args/lines/0/verified_quantity"},
		{"a line twice", func(c *purapi.VerifySupplierDelivery) { c.Lines = append(c.Lines, c.Lines[0]) }, kernel.CodeDuplicateLineID,
			"/args/lines/1/order_line_id"},
		{"a verification without lines", func(c *purapi.VerifySupplierDelivery) { c.Lines = nil }, kernel.CodeSchemaInvalid, "/args/lines"},
		{"an unknown decision", func(c *purapi.VerifySupplierDelivery) { c.Decision = "approve" }, kernel.CodeSchemaInvalid, "/args/decision"},
		{"a rejection without a reason", func(c *purapi.VerifySupplierDelivery) { c.Decision, c.Lines = purapi.DecisionReject, nil }, kernel.CodeSchemaInvalid,
			"/args/reason"},
		{"a rejection with lines", func(c *purapi.VerifySupplierDelivery) { c.Decision, c.Reason = purapi.DecisionReject, "x" }, kernel.CodeSchemaInvalid,
			"/args/lines"},
		{"evidence nobody registered", func(c *purapi.VerifySupplierDelivery) {
			c.Evidence = []purapi.Evidence{{SourceID: "missing.source", Locator: "x"}}
		},
			kernel.CodeReferenceInvalid, "/args/evidence/0/source_id"},
	} {
		refusedAt(t, c.what, bad(c.edit), c.code, c.path)
	}

	// What is verified is the evidence: a verifier who does not read a source the claim cites verifies
	// nothing, and may still reject the claim.
	desk := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "forwarder.desk"},
		Permissions: allPermissions, DataScope: "company", Team: "logistics"})
	w.source(desk, "handover.note", "合成交接记录：20 件")
	note := x.claim("delivery.2", "commitment.1", "20")
	note.Evidence = []purapi.Evidence{{SourceID: "handover.note", Locator: "第 1 行"}}
	_, err = w.pur.RecordSupplierDelivery(desk, note)
	w.must(err)
	teamBuyer := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "buyer.team"},
		Permissions: allPermissions, DataScope: "team", Team: "trade"})
	_, err = x.verify(teamBuyer, "delivery.2", 1, "20")
	refusedAt(t, "a source of the claim the verifier does not read", err, kernel.CodeReferenceInvalid, "/args/delivery_id")
	if r, err := x.reject(teamBuyer, "delivery.2", 1); err != nil || r.Status != purapi.DeliveryRejected || r.RowVersion != 2 {
		t.Fatalf("rejected by the same verifier: %+v, %v", r, err)
	}

	r, err := x.reject(w.author, "delivery.1", 1)
	w.must(err)
	if r.Status != purapi.DeliveryRejected || r.RowVersion != 2 || r.VerifiedVersion != 0 || r.LatestVersion != 1 {
		t.Fatalf("reject %+v", r)
	}
	_, err = x.verify(w.author, "delivery.1", 2, "120")
	refusedAt(t, "a claim is decided once", err, kernel.CodeInvalidState, "/args/delivery_id")
	v := x.view(w.author, "delivery.1")
	if v.Status != purapi.DeliveryRejected || v.PendingVersion != 0 || v.VerifiedVersion != 0 || v.Versions[0].Decision != purapi.DecisionReject ||
		v.Versions[0].DecisionReason == "" || v.Versions[0].DecidedBy.ID != "editor" || v.Versions[0].Lines[0].VerifiedQuantity != "" {
		t.Fatalf("rejected %+v", v)
	}
	if g := x.guard("company.A", deliveryRef("delivery.1", 1)); g.Usable() || g.Status != purapi.DeliveryRejected || g.Code != purapi.CodeSupplierDeliveryUnverified {
		t.Fatalf("guard on a rejected claim %+v", g)
	}
	if got := x.standing(w.author, "commitment.1"); got != "120 0 120" {
		t.Fatalf("a rejected claim counts nothing: %s", got)
	}
	if e := x.verifiedEvents(); len(e) != 0 {
		t.Fatalf("a rejection publishes nothing: %v", e)
	}
	audit, err := w.pur.Audit(w.reviewer)
	w.must(err)
	refused := 0
	for _, e := range audit {
		if e.Command == purapi.CommandVerifySupplierDelivery && e.Outcome == "rejected" {
			refused++
		}
	}
	if refused != 20 {
		t.Fatalf("every refused verification is audited: %d", refused)
	}
}

// PUR-INV-008 ("同一承诺行累计核实交付量不超过承诺量加获批容差": negative "累计核实超出承诺与容差返回
// SUPPLIER_DELIVERY_EXCEEDED"; no tolerance is configured in S5): the verified deliveries of a
// commitment line add up to what the line buys and never beyond — 100 of 120, then 30 more is
// refused and 20 fills it, then not one piece more; a claim is not capped, it never counts. What the
// line buys includes an overbuy the commitment decision approved (S5 decision D-14). The database
// checks the sum again at commit, for the schema owner too.
func TestSupplierDeliveryNeverExceedsWhatTheCommitmentLineBuys(t *testing.T) {
	w := newPurWorld(t)
	x := newPurDeliver(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	x.issued("commitment.1", d)
	merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
	_, err = x.record(merch, "delivery.1", "commitment.1", "100")
	w.must(err)
	_, err = x.verify(w.author, "delivery.1", 1, "100")
	w.must(err)
	// A claim beyond what is left is recorded: only a verification is measured.
	_, err = x.record(merch, "delivery.2", "commitment.1", "30")
	w.must(err)
	_, err = x.verify(w.author, "delivery.2", 1, "30")
	refusedAt(t, "100 + 30 of 120", err, purapi.CodeSupplierDeliveryExceeded, "/args/lines/0/verified_quantity")
	if r, _ := kernel.AsRejection(err); r == nil || r.Category != kernel.CategoryConflict || !strings.Contains(r.Fields[0].Reason, "120") {
		t.Fatalf("a conflict that names what the line buys: %v", err)
	}
	if v := x.view(w.author, "delivery.2"); v.Status != purapi.DeliveryRecorded || v.RowVersion != 1 {
		t.Fatalf("the refused claim stays a claim: %+v", v)
	}
	_, err = x.verify(w.author, "delivery.2", 1, "20")
	w.must(err)
	if got := x.standing(w.author, "commitment.1"); got != "120 120 0" {
		t.Fatalf("standing: %s", got)
	}
	_, err = x.record(merch, "delivery.3", "commitment.1", "0.000001")
	w.must(err)
	_, err = x.verify(w.author, "delivery.3", 1, "0.000001")
	w.want(err, purapi.CodeSupplierDeliveryExceeded)
	// The schema owner, with the commands out of the way, is refused at commit by the deferred check.
	err = w.purOwner(
		"UPDATE brant_pur.supplier_delivery SET row_version = row_version + 1, verified_version = 1, updated_at = now() WHERE delivery_id = 'delivery.3'",
		"UPDATE brant_pur.supplier_delivery_line SET verified_quantity = claimed_quantity WHERE delivery_id = 'delivery.3'",
		"UPDATE brant_pur.supplier_delivery_version SET status = 'verified', decided_by_kind = 'human', decided_by_id = 'intruder', decide_epoch = 1, "+
			"decided_at = now(), decision = 'verify', decision_evidence = '[]' WHERE delivery_id = 'delivery.3'")
	if got := sqlCode(err); got != "23000" || !strings.Contains(err.Error(), "PUR-INV-008") {
		t.Fatalf("the database's own check: %s (%v)", got, err)
	}
	if got := x.standing(w.author, "commitment.1"); got != "120 120 0" {
		t.Fatalf("standing after the refusals: %s", got)
	}

	// An approved overbuy is bought too: 80 allocated and 70 overbought under an MOQ of 150.
	o := newPurWorld(t)
	y := newPurDeliver(o.scenWorld, func() *pur.Module { return o.pur })
	moq := o.variant(func(l *supapi.ConditionLine) { l.MOQ.Quantity = "150" })
	line := purLine("80", purSource(moq))
	line.Overbuy = &purapi.Overbuy{Quantity: "70", Reason: "MOQ 150; the remainder goes to stock"}
	dm, err := o.pur.PrepareDraft(o.author, o.cmd("draft.moq", line))
	o.must(err)
	y.issued("commitment.moq", dm, purapi.Acknowledgement{OrderLineID: "L1", Kind: purapi.DeviationSourceDiffers, Reason: "合成：MOQ 150 的报价"},
		purapi.Acknowledgement{OrderLineID: "L1", Kind: purapi.DeviationOverbuy, Reason: "合成：余量入库，经采购经理批准"})
	buyer := o.other("merchandiser", kernel.ActorHuman, allPermissions...)
	_, err = y.record(buyer, "delivery.moq", "commitment.moq", "151")
	o.must(err)
	_, err = y.verify(o.author, "delivery.moq", 1, "151")
	o.want(err, purapi.CodeSupplierDeliveryExceeded)
	_, err = y.verify(o.author, "delivery.moq", 1, "150")
	o.must(err)
	if got := y.standing(o.author, "commitment.moq"); got != "150 150 0" {
		t.Fatalf("standing with an overbuy: %s", got)
	}
}

// SM-PUR-SUPPLIER-DELIVERY verified → superseded ("核实更正版本时原版本被取代") and the aggregate's rule
// 核实后不可变，更正形成新版本: a correction is the delivery's next version, a claim again, and changes
// nothing until an independent human verifies it — then the version verified before is superseded
// (REFERENCE_STALE at the guard) and the commitment line counts the corrected quantity. A rejected
// correction leaves the verified version as it is. A correction may bring a delivery down to
// nothing, which frees the line for another delivery, and never above what the line still takes.
func TestSupplierDeliveryCorrectionsSupersede(t *testing.T) {
	w := newPurWorld(t)
	x := newPurDeliver(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	x.issued("commitment.1", d)
	merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
	_, err = x.record(merch, "delivery.1", "commitment.1", "120")
	w.must(err)
	_, err = x.verify(w.author, "delivery.1", 1, "100")
	w.must(err)

	c, err := x.correct(w.author, "delivery.1", "commitment.1", "90", 2)
	w.must(err)
	if c.Status != purapi.DeliveryRecorded || c.RowVersion != 3 || c.LatestVersion != 2 || c.VerifiedVersion != 1 {
		t.Fatalf("correction %+v", c)
	}
	// Until it is verified the correction is a claim: version 1 stays the verified one.
	if g := x.guard("company.A", deliveryRef("delivery.1", 1)); !g.Usable() || g.VerifiedQuantity != "100" {
		t.Fatalf("version 1 while the correction is pending %+v", g)
	}
	if g := x.guard("company.A", deliveryRef("delivery.1", 2)); g.Usable() || g.Code != purapi.CodeSupplierDeliveryUnverified || g.VerifiedVersion != 1 {
		t.Fatalf("the pending correction %+v", g)
	}
	if v := x.view(w.author, "delivery.1"); v.Status != purapi.DeliveryVerified || v.PendingVersion != 2 || v.VerifiedVersion != 1 {
		t.Fatalf("view with a pending correction %+v", v)
	}
	if got := x.standing(w.author, "commitment.1"); got != "120 100 20" {
		t.Fatalf("standing with a pending correction: %s", got)
	}
	_, err = x.verify(w.author, "delivery.1", 3, "90")
	refusedAt(t, "the correction's recorder", err, purapi.CodeIndependentReviewRequired, "/args/delivery_id")
	s, err := x.verify(w.reviewer, "delivery.1", 3, "90")
	w.must(err)
	if s.Status != purapi.DeliveryVerified || s.RowVersion != 4 || s.LatestVersion != 2 || s.VerifiedVersion != 2 {
		t.Fatalf("the verified correction %+v", s)
	}
	old := x.guard("company.A", deliveryRef("delivery.1", 1))
	if old.Usable() || !old.Known || old.Status != purapi.DeliverySuperseded || old.Code != purapi.CodeReferenceStale || old.VerifiedVersion != 2 ||
		old.VerifiedQuantity != "" || code(old.Check("/args/lines/0/supplier_delivery")) != purapi.CodeReferenceStale {
		t.Fatalf("the superseded version %+v", old)
	}
	if g := x.guard("company.A", deliveryRef("delivery.1", 2)); !g.Usable() || g.VerifiedQuantity != "90" {
		t.Fatalf("the corrected version %+v", g)
	}
	if got := x.standing(w.author, "commitment.1"); got != "120 90 30" {
		t.Fatalf("standing after the correction: %s", got)
	}
	v := x.view(w.author, "delivery.1")
	if len(v.Versions) != 2 || v.Versions[0].Status != purapi.DeliverySuperseded || v.Versions[0].SupersededByVersion != 2 ||
		v.Versions[0].SupersededAt.IsZero() || v.Versions[0].Lines[0].VerifiedQuantity != "100" || v.Versions[1].Status != purapi.DeliveryVerified ||
		v.Versions[1].CorrectionReason != purCorrection || v.Versions[1].RecordedBy.ID != "editor" || v.Versions[1].DecidedBy.ID != "reviewer" {
		t.Fatalf("versions %+v", v.Versions)
	}
	events := x.verifiedEvents()
	if len(events) != 2 || events[1]["delivery_version"] != float64(2) || events[1]["superseded_version"] != float64(1) ||
		events[1]["aggregate_version"] != float64(4) {
		t.Fatalf("events %v", events)
	}
	if got := sqlCode(w.purOwner("UPDATE brant_pur.supplier_delivery_version SET status = 'verified', superseded_by_version = NULL, superseded_at = NULL " +
		"WHERE delivery_id = 'delivery.1' AND version = 1")); got != "23000" {
		t.Fatalf("a superseded version is final: %s", got)
	}

	// A rejected correction changes nothing.
	_, err = x.correct(merch, "delivery.1", "commitment.1", "60", 4)
	w.must(err)
	_, err = x.reject(w.author, "delivery.1", 5)
	w.must(err)
	if g := x.guard("company.A", deliveryRef("delivery.1", 2)); !g.Usable() || g.VerifiedQuantity != "90" || g.VerifiedVersion != 2 {
		t.Fatalf("after a rejected correction %+v", g)
	}
	if v = x.view(w.author, "delivery.1"); v.Status != purapi.DeliveryVerified || v.RowVersion != 6 || v.LatestVersion != 3 || v.PendingVersion != 0 ||
		v.Versions[2].Status != purapi.DeliveryRejected {
		t.Fatalf("view after a rejected correction %+v", v)
	}

	// A correction may bring the delivery down to nothing: the goods were another commitment's.
	_, err = x.correct(merch, "delivery.1", "commitment.1", "90", 6)
	w.must(err)
	z, err := x.verify(w.author, "delivery.1", 7, "0")
	w.must(err)
	if z.Status != purapi.DeliveryVerified || z.VerifiedVersion != 4 || z.RowVersion != 8 {
		t.Fatalf("verified at nothing %+v", z)
	}
	if g := x.guard("company.A", deliveryRef("delivery.1", 4)); !g.Usable() || g.VerifiedQuantity != "0" {
		t.Fatalf("a delivery corrected to nothing %+v", g)
	}
	if got := x.standing(w.author, "commitment.1"); got != "120 0 120" {
		t.Fatalf("standing after the withdrawal: %s", got)
	}
	_, err = x.record(merch, "delivery.2", "commitment.1", "120")
	w.must(err)
	_, err = x.verify(w.author, "delivery.2", 1, "120")
	w.must(err)
	// And never above what the line still takes: delivery.2 holds all 120 now.
	_, err = x.correct(merch, "delivery.1", "commitment.1", "10", 8)
	w.must(err)
	_, err = x.verify(w.author, "delivery.1", 9, "10")
	refusedAt(t, "a correction beyond the line", err, purapi.CodeSupplierDeliveryExceeded, "/args/lines/0/verified_quantity")
	if got := x.standing(w.author, "commitment.1"); got != "120 120 0" {
		t.Fatalf("standing at the end: %s", got)
	}
}

// PUR-INV-008 concurrency ("两次核实并发只有一个成功") on PostgreSQL 16. Two records of one new delivery
// commit once (the commitment root, then the delivery ID read again): the same claim under another
// key is answered from the stored delivery, another claim is VERSION_CONFLICT. Two decisions of one
// claim commit once (the delivery root). Two verifications of two deliveries that together exceed what
// the commitment line buys commit once (the commitment root): in every round one is verified and
// the other is SUPPLIER_DELIVERY_EXCEEDED; and, held in a known order, the second waits for the
// first and is then measured against what it verified.
func TestSupplierDeliveryVerificationsRaceOnce(t *testing.T) {
	w := newPurWorld(t)
	x := newPurDeliver(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.race", "120", "offer.1")
	w.must(err)
	x.issued("commitment.1", d)
	merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
	race := func(a, b func() error) []string {
		out := make([]string, 2)
		var wg sync.WaitGroup
		for i, f := range []func() error{a, b} {
			wg.Add(1)
			go func() { defer wg.Done(); out[i] = code(f()) }()
		}
		wg.Wait()
		return out
	}
	for round := range 3 {
		id := "new." + string(rune('a'+round))
		same := x.claim(id, "commitment.1", "1")
		again := same
		again.IdempotencyKey = w.key()
		receipts := make([]purapi.DeliveryReceipt, 2)
		got := race(func() (err error) { receipts[0], err = w.pur.RecordSupplierDelivery(merch, same); return err },
			func() (err error) { receipts[1], err = w.pur.RecordSupplierDelivery(merch, again); return err })
		if got[0] != "OK" || got[1] != "OK" || receipts[0].Replayed == receipts[1].Replayed || receipts[0].RowVersion != 1 || receipts[1].RowVersion != 1 {
			t.Fatalf("round %d, one claim recorded twice: %v %+v", round, got, receipts)
		}
		got = race(func() error { _, err := x.record(merch, id+".x", "commitment.1", "1"); return err },
			func() error { _, err := x.record(merch, id+".x", "commitment.1", "2"); return err })
		if !slices.Contains(got, "OK") || !slices.Contains(got, kernel.CodeVersionConflict) {
			t.Fatalf("round %d, two claims under one new delivery ID: %v", round, got)
		}
		if v := x.view(w.author, id+".x"); v.RowVersion != 1 || v.LatestVersion != 1 || len(v.Versions) != 1 {
			t.Fatalf("round %d: %+v", round, v)
		}
	}
	for round := range 3 {
		id := "decide." + string(rune('a'+round))
		_, err = x.record(merch, id, "commitment.1", "1")
		w.must(err)
		got := race(func() error { _, err := x.verify(w.author, id, 1, "1"); return err },
			func() error { _, err := x.reject(w.reviewer, id, 1); return err })
		if !slices.Contains(got, "OK") || !slices.Contains(got, kernel.CodeVersionConflict) {
			t.Fatalf("round %d, two decisions of one claim: %v", round, got)
		}
		if v := x.view(w.author, id); v.RowVersion != 2 || (v.Status == purapi.DeliveryVerified) != (got[0] == "OK") {
			t.Fatalf("round %d: %s at %d after %v", round, v.Status, v.RowVersion, got)
		}
	}
	verified := func() kernel.Decimal {
		t.Helper()
		v, err := w.pur.GetCommitmentDeliveries(w.author, "commitment.1")
		w.must(err)
		return kernel.MustDecimal(v.Lines[0].VerifiedQuantity)
	}
	// Each round two claims that each fit what is left and together exceed it.
	for round, each := range []string{"70", "30", "11"} {
		before := verified()
		a, b := "over."+string(rune('a'+round))+".1", "over."+string(rune('a'+round))+".2"
		for _, id := range []string{a, b} {
			_, err = x.record(merch, id, "commitment.1", each)
			w.must(err)
		}
		got := race(func() error { _, err := x.verify(w.author, a, 1, each); return err },
			func() error { _, err := x.verify(w.reviewer, b, 1, each); return err })
		if !slices.Contains(got, "OK") || !slices.Contains(got, purapi.CodeSupplierDeliveryExceeded) {
			t.Fatalf("round %d, two verifications of %s with %s verified: %v", round, each, before, got)
		}
		if after := verified(); !after.Equal(before.Add(kernel.MustDecimal(each))) {
			t.Fatalf("round %d: %s verified after %v, %s before", round, after, got, before)
		}
	}
	// In a known order: the first verification holds the commitment root, the second waits for it.
	before := verified()
	left := kernel.MustDecimal("120").Sub(before)
	each, _ := left.Exact()
	for _, id := range []string{"held.1", "held.2"} {
		_, err = x.record(merch, id, "commitment.1", each)
		w.must(err)
	}
	first := x.verifyCmd("held.1", 1, each)
	one, two := w.blockedBehind(t, purapi.CommandVerifySupplierDelivery, first.IdempotencyKey,
		func() error { _, err := w.pur.VerifySupplierDelivery(w.author, first); return err },
		func() error { _, err := x.verify(w.reviewer, "held.2", 1, each); return err })
	if one != nil || code(two) != purapi.CodeSupplierDeliveryExceeded {
		t.Fatalf("held: first %v, second %s", one, code(two))
	}
	if got := x.standing(w.author, "commitment.1"); got != "120 120 0" {
		t.Fatalf("standing: %s", got)
	}
}

// PurchaseSettlementSource v1 (Context Map R31 PUR→STL: 采购承诺与收货或直发交付证据（采购结算来源）; risk 无交付证据
// 形成应付) and PurchaseLineRef v1 (R37 PUR→CNX: 采购与收货明细（购进发票匹配）), with fact authority MD-45 ("只有
// 核实后的交付计入出运与采购结算"): the lookups show a verified delivery line with what its commitment line buys
// in money (BRC-001: 120 PCS, 600.00 USD), the evidence of the claim and of the verification, and
// what is verified for the commitment line; a claim is no settlement source and no receipt; a
// superseded version is REFERENCE_STALE. They need an active grant only and follow PUR's data scope.
// The reads (X-INV-003) re-check the grant, the data scope and the caller's access to the order.
func TestSupplierDeliverySettlementSourcesPurchaseLinesAndReads(t *testing.T) {
	w := newDeliveryWorld(t)
	x := w.x
	file, err := w.file(w.author, "receipt.1", "cargo-receipt.txt", evdapi.MimeText, "合成货代收货单：100 件", "")
	w.must(err)
	claim := x.claim("delivery.1", "commitment.1", "120")
	claim.FileIDs = []string{file.ObjectID}
	_, err = w.pur.RecordSupplierDelivery(w.merch, claim)
	w.must(err)
	verify := x.verifyCmd("delivery.1", 1, "100")
	verify.Evidence, verify.Reason = []purapi.Evidence{{SourceID: "evidence", Locator: "合成：收货单第 2 页"}}, "货代收货单只证实 100 件"
	_, err = w.pur.VerifySupplierDelivery(w.author, verify)
	w.must(err)
	_, err = x.record(w.merch, "delivery.2", "commitment.1", "20")
	w.must(err)

	d1, d2 := deliveryRef("delivery.1", 1), deliveryRef("delivery.2", 1)
	sources, err := w.pur.ResolveSettlementSources(w.reviewer, []purapi.DeliveryLineRef{d1, d2, deliveryRef("delivery.1", 9), deliveryRef("delivery.none", 1)})
	w.must(err)
	s1, s2 := sources[d1], sources[d2]
	if len(sources) != 2 || s1.Ref != d1 || s1.Code != "" || s1.Check("/x") != nil || s1.Status != purapi.DeliveryVerified || s1.VerifiedQuantity != "100" ||
		s1.ClaimedQuantity != "120" || s1.VerifiedVersion != 1 || s1.RowVersion != 2 || s1.VerifiedBy.ID != "editor" || s1.VerifiedAt.IsZero() ||
		s1.HandoverPlace != "CNNGB" || s1.HandedOverAt.IsZero() {
		t.Fatalf("the settlement source %+v", s1)
	}
	if l := s1.Line; l.Ref != (purapi.CommitmentLineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}) || l.Check("/x") != nil ||
		l.Status != purapi.CommitmentIssued || !strings.HasPrefix(l.PONumber, "PO-") || l.DraftID != "purchase.a46" || len(l.DraftHash) != 64 ||
		l.OrderID != "order.1" || l.OrderRevision != 1 || l.OrderHash != purOrderHash || l.SupplierID != "supplier.1" || l.SkuID != "sku.1" ||
		l.SpecRevision != w.spec || l.UnitID != "PCS" || l.PurchaseQuantity != "120" || l.AllocatedQuantity != "120" {
		t.Fatalf("its commitment line %+v", l)
	}
	if s1.Currency != "USD" || s1.MinorUnits != 2 || s1.LineNetMinor != "60000" || s1.LineTaxMinor != "0" || s1.LineGrossMinor != "60000" ||
		s1.LineVerifiedQuantity != "100" || len(s1.Evidence) != 1 || s1.Evidence[0].SourceID != "evidence" || len(s1.Files) != 1 ||
		s1.Files[0].FileID != "receipt.1" || len(s1.Files[0].SHA256) != 64 || len(s1.VerificationEvidence) != 1 {
		t.Fatalf("what it is worth and shown by %+v", s1)
	}
	if s2.Code != purapi.CodeSupplierDeliveryUnverified || s2.Status != purapi.DeliveryRecorded || s2.VerifiedQuantity != "" || s2.ClaimedQuantity != "20" ||
		s2.LineVerifiedQuantity != "100" || code(s2.Check("/args/sources/1")) != purapi.CodeSupplierDeliveryUnverified || len(s2.VerificationEvidence) != 0 {
		t.Fatalf("a claim is no settlement source %+v", s2)
	}
	if v := x.view(w.reviewer, "delivery.1"); v.Versions[0].DecisionReason != "货代收货单只证实 100 件" || len(v.Versions[0].DecisionEvidence) != 1 {
		t.Fatalf("the verification's reason and evidence %+v", v.Versions[0])
	}

	// A verified correction: the version settled before is stale, the new one is the source.
	_, err = x.correct(w.merch, "delivery.1", "commitment.1", "90", 2)
	w.must(err)
	_, err = x.verify(w.author, "delivery.1", 3, "90")
	w.must(err)
	d1v2 := deliveryRef("delivery.1", 2)
	sources, err = w.pur.ResolveSettlementSources(w.reviewer, []purapi.DeliveryLineRef{d1, d1v2})
	w.must(err)
	if old, now := sources[d1], sources[d1v2]; old.Code != purapi.CodeReferenceStale || code(old.Check("/x")) != purapi.CodeReferenceStale ||
		old.VerifiedVersion != 2 || old.VerifiedQuantity != "100" || now.Code != "" || now.VerifiedQuantity != "90" || now.LineVerifiedQuantity != "90" ||
		old.LineVerifiedQuantity != "90" {
		t.Fatalf("after the correction: %+v then %+v", old, now)
	}

	line := purapi.CommitmentLineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}
	asked := []purapi.CommitmentLineRef{line, {CommitmentID: "commitment.1", OrderLineID: "L9"}, {CommitmentID: "commitment.none", OrderLineID: "L1"}}
	lines, err := w.pur.ResolvePurchaseLines(w.reviewer, asked)
	w.must(err)
	pl := lines[line]
	if len(lines) != 1 || pl.Line.Ref != line || pl.Line.Check("/x") != nil || pl.Line.PurchaseQuantity != "120" || pl.Line.SupplierID != "supplier.1" ||
		pl.Currency != "USD" || pl.MinorUnits != 2 || pl.NetMinor != "60000" || pl.TaxMinor != "0" || pl.GrossMinor != "60000" || pl.VerifiedQuantity != "90" ||
		len(pl.Deliveries) != 1 || pl.Deliveries[0].Ref != d1v2 || pl.Deliveries[0].VerifiedQuantity != "90" || pl.Deliveries[0].HandoverPlace != "CNNGB" ||
		pl.Deliveries[0].VerifiedAt.IsZero() || pl.Deliveries[0].HandedOverAt.IsZero() {
		t.Fatalf("the purchase line %+v", pl)
	}
	_, err = x.verify(w.author, "delivery.2", 1, "20")
	w.must(err)
	lines, err = w.pur.ResolvePurchaseLines(w.reviewer, asked)
	w.must(err)
	if pl = lines[line]; pl.VerifiedQuantity != "110" || len(pl.Deliveries) != 2 || pl.Deliveries[0].Ref != d1v2 || pl.Deliveries[1].Ref != d2 ||
		pl.Deliveries[1].VerifiedQuantity != "20" {
		t.Fatalf("with the second delivery verified %+v", pl)
	}

	// An active grant is enough; the data scope and the company decide what is shown.
	own := w.provision(bootstrapGrant(w.supWorld, "delivery.own", "own", allPermissions...))
	bare := w.provision(bootstrapGrant(w.supWorld, "delivery.bare", "company"))
	elsewhere := w.provision(bootstrapGrantIn(w.supWorld, "company.B", "delivery.b", "company", allPermissions...))
	for who, want := range map[string]struct {
		ctx context.Context
		n   int
	}{"own data scope": {own, 0}, "no permission": {bare, 1}, "another company": {elsewhere, 0}} {
		src, err := w.pur.ResolveSettlementSources(want.ctx, []purapi.DeliveryLineRef{d1v2})
		got, err2 := w.pur.ResolvePurchaseLines(want.ctx, []purapi.CommitmentLineRef{line})
		dl, err3 := w.pur.ResolveDeliveryLines(want.ctx, []purapi.DeliveryLineRef{d1v2})
		if err != nil || err2 != nil || err3 != nil || len(src) != want.n || len(got) != want.n || len(dl) != want.n {
			t.Fatalf("%s: %d sources (%v), %d lines (%v), %d delivery lines (%v)", who, len(src), err, len(got), err2, len(dl), err3)
		}
	}
	many := make([]purapi.DeliveryLineRef, purapi.MaxDeliveryLineRefs+1)
	for i := range many {
		many[i] = d1
	}
	_, err = w.pur.ResolveSettlementSources(w.reviewer, many)
	w.want(err, kernel.CodeSchemaInvalid)
	_, err = w.pur.ResolveSettlementSources(w.reviewer, []purapi.DeliveryLineRef{{DeliveryID: "delivery 1", LineID: "L1", Version: 1}})
	w.want(err, kernel.CodeSchemaInvalid)
	_, err = w.pur.ResolvePurchaseLines(w.reviewer, make([]purapi.CommitmentLineRef, purapi.MaxCommitmentLineRefs+1))
	w.want(err, kernel.CodeSchemaInvalid)

	// The reads.
	list, err := w.pur.GetCommitmentDeliveries(w.reviewer, "commitment.1")
	w.must(err)
	if list.CommitmentID != "commitment.1" || list.Status != purapi.CommitmentIssued || !strings.HasPrefix(list.PONumber, "PO-") || list.OrderID != "order.1" ||
		list.SupplierID != "supplier.1" || len(list.Lines) != 1 || list.Lines[0].OrderLineID != "L1" || list.Lines[0].SkuID != "sku.1" ||
		list.Lines[0].UnitID != "PCS" || list.Lines[0].PurchaseQuantity != "120" || list.Lines[0].VerifiedQuantity != "110" ||
		list.Lines[0].RemainingQuantity != "10" || len(list.Deliveries) != 2 || list.Deliveries[0].DeliveryID != "delivery.1" ||
		list.Deliveries[0].VerifiedVersion != 2 || len(list.Deliveries[0].Versions) != 2 || list.Deliveries[1].DeliveryID != "delivery.2" ||
		list.Deliveries[1].Status != purapi.DeliveryVerified {
		t.Fatalf("the commitment's deliveries %+v", list)
	}
	_, err = w.pur.GetDelivery(own, "delivery.1")
	refusedAt(t, "a delivery outside the data scope", err, kernel.CodeNotFound, "/id")
	_, err = w.pur.GetCommitmentDeliveries(own, "commitment.1")
	refusedAt(t, "a commitment outside the data scope", err, kernel.CodeNotFound, "/commitment_id")
	_, err = w.pur.GetDelivery(elsewhere, "delivery.1")
	w.want(err, kernel.CodeNotFound)
	_, err = w.pur.GetDelivery(w.reviewer, "delivery.none")
	w.want(err, kernel.CodeNotFound)
	_, err = w.pur.GetCommitmentDeliveries(w.reviewer, "commitment.none")
	w.want(err, kernel.CodeNotFound)
	_, err = w.pur.GetDelivery(w.reviewer, "delivery 1")
	w.want(err, kernel.CodeSchemaInvalid)
	_, err = w.pur.GetCommitmentDeliveries(w.reviewer, "commitment 1")
	w.want(err, kernel.CodeSchemaInvalid)
	unread := w.provision(bootstrapGrant(w.supWorld, "delivery.unread", "company", without(allPermissions, purapi.PermissionRead)...))
	_, err = w.pur.GetDelivery(unread, "delivery.1")
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.pur.GetCommitmentDeliveries(unread, "commitment.1")
	w.want(err, kernel.CodePermissionDenied)
	// A reader TRD no longer shows the order to reads neither the delivery nor the list.
	w.orders.mu.Lock()
	w.orders.blind["delivery.blind"] = true
	w.orders.mu.Unlock()
	blind := w.other("delivery.blind", kernel.ActorHuman, allPermissions...)
	_, err = w.pur.GetDelivery(blind, "delivery.1")
	refusedAt(t, "a delivery whose order the caller does not read", err, kernel.CodeReferenceInvalid, "/id")
	_, err = w.pur.GetCommitmentDeliveries(blind, "commitment.1")
	refusedAt(t, "a commitment whose order the caller does not read", err, kernel.CodeReferenceInvalid, "/commitment_id")
}

// Tactical principle 9 ("一次成功写入原子提交业务变化、动作结果、提交证据、审计与 Outbox；失败不留下半份事实") and
// X-INV-001 for the verification: a failing Outbox write rolls the verification back whole — the
// claim stays a claim, nothing counts, no action result —; the same key verifies once the store
// accepts the write; a restarted process reads the same delivery and replays both keys.
func TestSupplierDeliveryFailureRollsBackAndReplaysAfterRestart(t *testing.T) {
	w := newDeliveryWorld(t)
	x := w.x
	record := x.claim("delivery.1", "commitment.1", "100")
	r, err := w.pur.RecordSupplierDelivery(w.merch, record)
	w.must(err)
	restore := w.failOutbox(purapi.EventSupplierDeliveryVerified)
	cmd := x.verifyCmd("delivery.1", 1, "100")
	_, err = w.pur.VerifySupplierDelivery(w.author, cmd)
	if _, ok := kernel.AsRejection(err); err == nil || ok || !strings.Contains(err.Error(), "test storage failure") {
		t.Fatalf("an infrastructure failure, no rejection: %v", err)
	}
	if v := x.view(w.author, "delivery.1"); v.Status != purapi.DeliveryRecorded || v.RowVersion != 1 || v.VerifiedVersion != 0 ||
		v.Versions[0].Decision != "" || v.Versions[0].Lines[0].VerifiedQuantity != "" {
		t.Fatalf("rolled back: %+v", v)
	}
	if g := x.guard("company.A", deliveryRef("delivery.1", 1)); g.Usable() || g.Status != purapi.DeliveryRecorded {
		t.Fatalf("the guard after the rollback %+v", g)
	}
	if got := x.standing(w.author, "commitment.1"); got != "120 0 120" {
		t.Fatalf("nothing counts after the rollback: %s", got)
	}
	restore()
	v, err := w.pur.VerifySupplierDelivery(w.author, cmd)
	if err != nil || v.Replayed || v.Status != purapi.DeliveryVerified || v.RowVersion != 2 || v.VerifiedVersion != 1 {
		t.Fatalf("retry %+v, %v", v, err)
	}
	m := w.module()
	view, err := m.GetDelivery(w.author, "delivery.1")
	if err != nil || view.RowVersion != 2 || view.VerifiedVersion != 1 || view.Versions[0].Lines[0].VerifiedQuantity != "100" {
		t.Fatalf("restart %+v, %v", view, err)
	}
	again, err := m.VerifySupplierDelivery(w.author, cmd)
	if err != nil || !again.Replayed || again.ActionID != v.ActionID || again.Status != purapi.DeliveryVerified || again.RowVersion != 2 {
		t.Fatalf("the verification replayed after restart %+v, %v", again, err)
	}
	back, err := m.RecordSupplierDelivery(w.merch, record)
	if err != nil || !back.Replayed || back.ActionID != r.ActionID || back.Status != purapi.DeliveryRecorded || back.RowVersion != 1 || back.VerifiedVersion != 1 {
		t.Fatalf("the record replayed after restart %+v, %v", back, err)
	}
	if e := x.verifiedEvents(); len(e) != 1 || e[0]["delivery_id"] != "delivery.1" {
		t.Fatalf("one event: %v", e)
	}
}

// On TRD's real orders, reached through the shared order world (newPurOrderWorld: BRC-001's active
// 960.00 USD order and its 600.00 USD purchase draft): SIM-01 step 12 (工厂装柜直送堆场，货代收货单证实) and the
// S5 chain's step "supplier_delivery.record 120 at CNNGB → supplier_delivery.verify 120". What a
// supplier handed over is a fact of the supplier's: once the commitment is issued a delivery needs
// nothing of the order any more — it is still corrected and verified after the order's activating
// approval was revoked (PUR-INV-008 names no order condition; no OrderGuard is called) — and it adds
// nothing to what DownstreamCommitments v1 (R71) shows TRD.
func TestSupplierDeliveryOnTRDOrdersIsAFactOfTheSupplier(t *testing.T) {
	w := newPurOrderWorld(t)
	x := newPurDeliver(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.brc001", "120", "offer.1")
	w.must(err)
	if d.NetMinor != "60000" {
		t.Fatalf("draft %+v", d)
	}
	x.issued("commitment.brc001", d)
	open := x.p.downstream("company.A", w.order.ObjectID)
	merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
	r, err := x.record(merch, "delivery.brc001", "commitment.brc001", "120")
	w.must(err)
	if r.OrderID != w.order.ObjectID || r.SupplierID != "supplier.1" {
		t.Fatalf("record %+v", r)
	}
	v, err := x.verify(w.author, "delivery.brc001", 1, "120")
	if err != nil || v.Status != purapi.DeliveryVerified || v.VerifiedVersion != 1 {
		t.Fatalf("verify %+v, %v", v, err)
	}
	ref := deliveryRef("delivery.brc001", 1)
	sources, err := w.pur.ResolveSettlementSources(w.reviewer, []purapi.DeliveryLineRef{ref})
	w.must(err)
	if s := sources[ref]; s.Code != "" || s.VerifiedQuantity != "120" || s.LineVerifiedQuantity != "120" || s.LineNetMinor != "60000" || s.Currency != "USD" ||
		s.Line.OrderID != w.order.ObjectID || s.Line.OrderRevision != w.order.OrderRevision || s.Line.OrderHash != w.order.OrderHash {
		t.Fatalf("120 PCS at 5.00 USD: %+v", s)
	}
	w.must(w.revoke())
	if _, err = x.correct(merch, "delivery.brc001", "commitment.brc001", "118", 2); err != nil {
		t.Fatalf("a correction after the order's approval was revoked: %v", err)
	}
	s, err := x.verify(w.author, "delivery.brc001", 3, "118")
	if err != nil || s.Status != purapi.DeliveryVerified || s.VerifiedVersion != 2 {
		t.Fatalf("its verification %+v, %v", s, err)
	}
	if got := x.standing(w.reviewer, "commitment.brc001"); got != "120 118 2" {
		t.Fatalf("standing: %s", got)
	}
	if view := x.view(w.reviewer, "delivery.brc001"); view.Status != purapi.DeliveryVerified || view.VerifiedVersion != 2 || len(view.Versions) != 2 {
		t.Fatalf("view %+v", view)
	}
	if got := x.p.downstream("company.A", w.order.ObjectID); !slices.Equal(got, open) || len(got) != 2 {
		t.Fatalf("deliveries change nothing of DownstreamCommitments v1: %q, before %q", got, open)
	}
}

// S5-15 over HTTP with OIDC access tokens, on TRD's real order: the reviewer records the supplier's
// claim, the editor verifies part of it, everything in its wire form; the delivery and the
// commitment's deliveries read back; the verification reconciles; a verification beyond what the
// commitment line buys is 409 SUPPLIER_DELIVERY_EXCEEDED and the recorder's own 422
// INDEPENDENT_REVIEW_REQUIRED.
func TestSupplierDeliveryOverHTTP(t *testing.T) {
	q := newOrderWorld(t)
	order := q.activeOrder("quote.1")
	x := newPurDeliver(q.scenWorld, func() *pur.Module { return q.c.PUR })
	d, err := q.c.PUR.PrepareDraft(q.author, purapi.PrepareDraft{IdempotencyKey: q.key(), DraftID: "purchase.http", OrderID: order.ObjectID,
		Lines: []purapi.DraftLine{purLine("120", purSource("offer.1"))}, SelectionReason: purReason})
	q.must(err)
	x.issued("commitment.http", d)
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ed := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "editor", "human")}
	rv := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "reviewer", "human"), n: 300}

	claim := func(id, quantity string) map[string]any {
		return map[string]any{"delivery_id": id, "expected_version": 0, "commitment_id": "commitment.http",
			"lines":          []any{map[string]any{"order_line_id": "L1", "quantity": quantity}},
			"handover_place": "CNNGB", "handed_over_at": kernel.FormatTimestamp(time.Now().Add(-time.Minute)),
			"evidence": []any{map[string]any{"source_id": "evidence", "locator": "合成货代收货单"}}, "supplier_reference": "DN-2026-17"}
	}
	r := rv.command("pur", purapi.Contract, purapi.CommandRecordSupplierDelivery, claim("delivery.http", "120"))
	if r["status"] != "recorded" || r["row_version"] != float64(1) || r["object_id"] != "delivery.http" {
		t.Fatalf("record %v", r)
	}
	lines := func(quantity string) []any {
		return []any{map[string]any{"order_line_id": "L1", "verified_quantity": quantity}}
	}
	status, out := rv.try("pur", purapi.Contract, purapi.CommandVerifySupplierDelivery, map[string]any{"delivery_id": "delivery.http",
		"expected_version": 1, "decision": "verify", "lines": lines("100")})
	if status != http.StatusUnprocessableEntity || out["code"] != purapi.CodeIndependentReviewRequired {
		t.Fatalf("the recorder verifying %d %v", status, out)
	}
	verify := map[string]any{"contract": purapi.Contract, "command": purapi.CommandVerifySupplierDelivery, "idempotency_key": "pur.http.verify",
		"args": map[string]any{"delivery_id": "delivery.http", "expected_version": 1, "decision": "verify", "lines": lines("100"),
			"reason": "货代收货单只证实 100 件"}}
	status, a := ed.do("POST", "/api/brant/v1/pur/commands/"+purapi.CommandVerifySupplierDelivery, verify)
	if status != http.StatusOK || a["status"] != "verified" || a["row_version"] != float64(2) {
		t.Fatalf("verify %d %v", status, a)
	}
	status, rec := ed.do("POST", "/api/brant/v1/pur/commands/"+purapi.CommandVerifySupplierDelivery+"/reconcile", verify)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != a["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	status, v := ed.do("GET", "/api/brant/v1/pur/supplier_deliveries/delivery.http", nil)
	if status != http.StatusOK || v["status"] != "verified" || v["verified_version"] != float64(1) || v["commitment_id"] != "commitment.http" ||
		v["order_id"] != order.ObjectID || !strings.HasPrefix(v["po_number"].(string), "PO-") {
		t.Fatalf("delivery %d %v", status, v)
	}
	version := v["versions"].([]any)[0].(map[string]any)
	line := version["lines"].([]any)[0].(map[string]any)
	if version["status"] != "verified" || version["handover_place"] != "CNNGB" || version["decision"] != "verify" ||
		version["recorded_by"].(map[string]any)["id"] != "reviewer" || version["decided_by"].(map[string]any)["id"] != "editor" ||
		line["order_line_id"] != "L1" || line["claimed_quantity"] != "120" || line["verified_quantity"] != "100" || line["unit_id"] != "PCS" {
		t.Fatalf("version %v", version)
	}
	rv.command("pur", purapi.Contract, purapi.CommandRecordSupplierDelivery, claim("delivery.more", "30"))
	status, out = ed.try("pur", purapi.Contract, purapi.CommandVerifySupplierDelivery, map[string]any{"delivery_id": "delivery.more",
		"expected_version": 1, "decision": "verify", "lines": lines("30")})
	if status != http.StatusConflict || out["code"] != purapi.CodeSupplierDeliveryExceeded {
		t.Fatalf("beyond the commitment line %d %v", status, out)
	}
	rejected := ed.command("pur", purapi.Contract, purapi.CommandVerifySupplierDelivery, map[string]any{"delivery_id": "delivery.more",
		"expected_version": 1, "decision": "reject", "reason": "合成：收货单只有 100 件"})
	if rejected["status"] != "rejected" || rejected["row_version"] != float64(2) {
		t.Fatalf("reject %v", rejected)
	}
	correction := claim("delivery.http", "100")
	correction["expected_version"], correction["reason"] = 2, purCorrection
	if c := rv.command("pur", purapi.Contract, purapi.CommandRecordSupplierDelivery, correction); c["status"] != "recorded" || c["row_version"] != float64(3) {
		t.Fatalf("correction %v", c)
	}
	status, list := ed.do("GET", "/api/brant/v1/pur/purchase_commitments/commitment.http/deliveries", nil)
	if status != http.StatusOK || list["commitment_id"] != "commitment.http" || len(list["deliveries"].([]any)) != 2 {
		t.Fatalf("the commitment's deliveries %d %v", status, list)
	}
	if l := list["lines"].([]any)[0].(map[string]any); l["purchase_quantity"] != "120" || l["verified_quantity"] != "100" || l["remaining_quantity"] != "20" {
		t.Fatalf("the line's standing %v", l)
	}
	if first := list["deliveries"].([]any)[0].(map[string]any); first["delivery_id"] != "delivery.http" || first["pending_version"] != float64(2) ||
		first["verified_version"] != float64(1) {
		t.Fatalf("the corrected delivery %v", first)
	}
	status, out = ed.do("GET", "/api/brant/v1/pur/supplier_deliveries/delivery.none", nil)
	if status != http.StatusNotFound || out["code"] != kernel.CodeNotFound {
		t.Fatalf("no such delivery %d %v", status, out)
	}
}
