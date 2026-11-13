//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SupplierDeliveryGuard v1 (Context Map R59 PUR→FUL, guardLock; ADR-BR-004 option B, ADR-BR-011;
// FUL-INV-003; TD-AT14) as FUL will use it: a throwaway copy of what FUL will own — shipment lines,
// each sourced from one supplier delivery line at a version, FUL keeping its own count of what it
// shipped of a delivery line across versions — with its own Bento transaction owner. Confirming a
// shipment line locks its row FOR UPDATE first, then calls PUR's guard function with the
// downstream's own executor, then checks FUL's own count against the verified quantity and confirms.

const directShipmentDDL = `
CREATE TABLE {{schema}}.shipment_line (
    tenant_id   text          NOT NULL,
    company_id  text          NOT NULL,
    shipment_id text          NOT NULL,
    status      text          NOT NULL CHECK (status IN ('prepared', 'confirmed')),
    delivery_id text          NOT NULL,
    line_id     text          NOT NULL,
    version     bigint        NOT NULL,
    quantity    numeric(18,6) NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (tenant_id, company_id, shipment_id)
);
ALTER TABLE {{schema}}.shipment_line ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.shipment_line FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_line_scope ON {{schema}}.shipment_line
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
GRANT SELECT, INSERT, UPDATE ON {{schema}}.shipment_line TO {{app}};
`

// errDeliveryInsufficient is FUL's SUPPLIER_DELIVERY_INSUFFICIENT (FUL-INV-003): the shipments of a
// delivery line would exceed its verified quantity.
var errDeliveryInsufficient = errors.New("downstream: SUPPLIER_DELIVERY_INSUFFICIENT")

type directShipments struct {
	*downstream
	table string
}

func newDirectShipments(w *world) *directShipments {
	w.t.Helper()
	d := newDownstream(w)
	schema := pgx.Identifier{pgtest.Schema(w.t, pgtest.MigratorPool(w.t), d.app, "brant_t_ful_", directShipmentDDL)}.Sanitize()
	return &directShipments{downstream: d, table: schema + ".shipment_line"}
}

func (s *directShipments) prepare(id string, ref purapi.DeliveryLineRef, quantity string) {
	s.t.Helper()
	if err := s.in(s.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, "INSERT INTO "+s.table+" (tenant_id, company_id, shipment_id, status, delivery_id, line_id, version, quantity) "+
			"VALUES ($1, $2, $3, 'prepared', $4, $5, $6, $7::numeric)",
			string(s.scope.Tenant), string(s.scope.Company), id, ref.DeliveryID, ref.LineID, ref.Version, quantity)
		return err
	}); err != nil {
		s.t.Fatal(err)
	}
}

// deliveryGuard calls SupplierDeliveryGuard v1 for ref in a transaction of scope.
func (s *directShipments) deliveryGuard(scope kernel.Scope, ref purapi.DeliveryLineRef) purapi.SupplierDeliveryVerdict {
	s.t.Helper()
	var v purapi.SupplierDeliveryVerdict
	if err := s.in(scope, func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = purapi.ScanSupplierDeliveryGuard(ref, exec.QueryRow(ctx, purapi.SupplierDeliveryGuardSQL, purapi.SupplierDeliveryGuardArgs(ref)...).Scan)
		return err
	}); err != nil {
		s.t.Fatal(err)
	}
	return v
}

// confirm is FUL's shipment.confirm for one line: lock the shipment line FOR UPDATE, guard the
// delivery line it is sourced from, check that what FUL confirmed of that delivery line — at any
// version — and this line stay within the verified quantity (FUL-INV-003), then confirm, in one
// transaction.
func (s *directShipments) confirm(id string, st stage) (purapi.SupplierDeliveryVerdict, error) {
	var v purapi.SupplierDeliveryVerdict
	err := s.in(s.scope, func(ctx context.Context, exec bentopg.Executor) error {
		if st.pid != nil {
			var pid int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			st.pid <- pid
		}
		t, c := string(s.scope.Tenant), string(s.scope.Company)
		var ref purapi.DeliveryLineRef
		var quantity string
		if err := exec.QueryRow(ctx, "SELECT delivery_id, line_id, version, quantity::text FROM "+s.table+
			" WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3 FOR UPDATE", t, c, id).Scan(&ref.DeliveryID, &ref.LineID, &ref.Version, &quantity); err != nil {
			return err
		}
		var err error
		if v, err = purapi.ScanSupplierDeliveryGuard(ref, exec.QueryRow(ctx, purapi.SupplierDeliveryGuardSQL, purapi.SupplierDeliveryGuardArgs(ref)...).Scan); err != nil {
			return err
		}
		if err = v.Check("/args/lines/0/supplier_delivery"); err != nil {
			return errors.Join(errReferenceRefused, err)
		}
		var within bool
		if err = exec.QueryRow(ctx, "SELECT coalesce(sum(quantity), 0) + $5::numeric <= $6::numeric FROM "+s.table+
			" WHERE tenant_id = $1 AND company_id = $2 AND delivery_id = $3 AND line_id = $4 AND status = 'confirmed'",
			t, c, ref.DeliveryID, ref.LineID, quantity, v.VerifiedQuantity).Scan(&within); err != nil {
			return err
		}
		if !within {
			return errDeliveryInsufficient
		}
		if st.hold != nil {
			st.hold()
		}
		_, err = exec.Exec(ctx, "UPDATE "+s.table+" SET status = 'confirmed' WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3", t, c, id)
		return err
	})
	return v, err
}

func (s *directShipments) status(id string) string {
	s.t.Helper()
	var status string
	if err := s.in(s.scope, func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, "SELECT status FROM "+s.table+" WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3",
			string(s.scope.Tenant), string(s.scope.Company), id).Scan(&status)
	}); err != nil {
		s.t.Fatal(err)
	}
	return status
}

// deliveryWorld is the PUR world with commitment.1 issued for 120 PCS of line L1, a merchandiser
// who records deliveries and the throwaway FUL.
type deliveryWorld struct {
	*purWorld
	x     *purDeliver
	s     *directShipments
	merch context.Context
}

func newDeliveryWorld(t *testing.T) *deliveryWorld {
	w := newPurWorld(t)
	x := newPurDeliver(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	x.issued("commitment.1", d)
	return &deliveryWorld{purWorld: w, x: x, s: newDirectShipments(w.world), merch: w.other("merchandiser", kernel.ActorHuman, allPermissions...)}
}

// verified records delivery id for claimed PCS as the merchandiser and has the purchaser verify
// quantity of them: version 1 is the verified one, at row version 2.
func (w *deliveryWorld) verified(id, claimed, quantity string) {
	w.t.Helper()
	_, err := w.x.record(w.merch, id, "commitment.1", claimed)
	w.must(err)
	_, err = w.x.verify(w.author, id, 1, quantity)
	w.must(err)
}

// The guard's verdicts for every state of a delivery version and in scopes, the lookup that answers
// the same before FUL's transaction, and how the database publishes the function.
func TestSupplierDeliveryGuardVerdicts(t *testing.T) {
	w := newDeliveryWorld(t)
	x, s := w.x, w.s
	// delivery.1: version 1 verified at 30 and superseded by version 2 at 20, version 3 a pending
	// correction; delivery.2: a rejected claim.
	w.verified("delivery.1", "30", "30")
	_, err := x.correct(w.merch, "delivery.1", "commitment.1", "20", 2)
	w.must(err)
	_, err = x.verify(w.author, "delivery.1", 3, "20")
	w.must(err)
	_, err = x.correct(w.merch, "delivery.1", "commitment.1", "25", 4)
	w.must(err)
	_, err = x.record(w.merch, "delivery.2", "commitment.1", "5")
	w.must(err)
	_, err = x.reject(w.author, "delivery.2", 1)
	w.must(err)

	scope := w.scope("company.A")
	ref := func(id, line string, version int64) purapi.DeliveryLineRef {
		return purapi.DeliveryLineRef{DeliveryID: id, LineID: line, Version: version}
	}
	cases := []struct {
		ref      purapi.DeliveryLineRef
		scope    kernel.Scope
		known    bool
		status   string
		code     string
		quantity string
		verified int64
	}{
		{ref("delivery.1", "L1", 1), scope, true, purapi.DeliverySuperseded, purapi.CodeReferenceStale, "", 2},
		{ref("delivery.1", "L1", 2), scope, true, purapi.DeliveryVerified, "", "20", 2},
		{ref("delivery.1", "L1", 3), scope, true, purapi.DeliveryRecorded, purapi.CodeSupplierDeliveryUnverified, "", 2},
		{ref("delivery.2", "L1", 1), scope, true, purapi.DeliveryRejected, purapi.CodeSupplierDeliveryUnverified, "", 0},
		{ref("delivery.1", "L9", 2), scope, false, "", purapi.CodeSupplierDeliveryUnverified, "", 0},
		{ref("delivery.1", "L1", 9), scope, false, "", purapi.CodeSupplierDeliveryUnverified, "", 0},
		{ref("delivery.none", "L1", 1), scope, false, "", purapi.CodeSupplierDeliveryUnverified, "", 0},
		{ref("delivery.1", "L1", 2), w.scope("company.B"), false, "", purapi.CodeSupplierDeliveryUnverified, "", 0}, // another company never sees it
	}
	var refs []purapi.DeliveryLineRef
	for _, c := range cases {
		v := s.deliveryGuard(c.scope, c.ref)
		if v.Known != c.known || v.Status != c.status || v.Code != c.code || v.VerifiedQuantity != c.quantity || v.VerifiedVersion != c.verified ||
			v.Usable() != (c.code == "") || code(v.Check("/x")) != map[bool]string{true: "OK", false: c.code}[c.code == ""] {
			t.Errorf("%+v in %s: %+v", c.ref, c.scope, v)
		}
		if c.known && (v.CommitmentID != "commitment.1" || v.OrderID != "order.1" || v.OrderLineID != "L1" || v.SupplierID != "supplier.1" ||
			v.SkuID != "sku.1" || v.SpecRevision != w.spec || v.UnitID != "PCS" || v.UnitRevision != w.pcs.Revision) {
			t.Errorf("%+v: %+v", c.ref, v)
		}
		if c.scope == scope {
			refs = append(refs, c.ref)
		}
	}
	// The lookup before FUL's transaction answers the same for the lines the caller sees.
	got, err := w.pur.ResolveDeliveryLines(w.reviewer, refs)
	w.must(err)
	if len(got) != 4 {
		t.Fatalf("lookup %+v", got)
	}
	for _, c := range cases[:4] {
		l := got[c.ref]
		if l.Ref != c.ref || l.Status != c.status || l.Code != c.code || l.VerifiedVersion != c.verified || l.Line.Ref.CommitmentID != "commitment.1" ||
			l.Line.Ref.OrderLineID != "L1" || l.Line.PurchaseQuantity != "120" || l.Line.Check("/x") != nil || l.HandoverPlace != "CNNGB" ||
			code(l.Check("/x")) != map[bool]string{true: "OK", false: c.code}[c.code == ""] {
			t.Errorf("lookup %+v: %+v", c.ref, l)
		}
	}
	if l := got[ref("delivery.1", "L1", 2)]; l.VerifiedQuantity != "20" || l.ClaimedQuantity != "20" || l.VerifiedBy.ID != "editor" || l.VerifiedAt.IsZero() ||
		l.RowVersion != 5 {
		t.Fatalf("the verified line %+v", l)
	}
	if l := got[ref("delivery.1", "L1", 1)]; l.VerifiedQuantity != "30" {
		t.Fatalf("a superseded line keeps what it verified: %+v", l)
	}
	own := w.provision(bootstrapGrant(w.supWorld, "delivery.own", "own", allPermissions...))
	if got, err = w.pur.ResolveDeliveryLines(own, refs); err != nil || len(got) != 0 {
		t.Fatalf("outside the data scope %+v, %v", got, err)
	}
	if _, err = w.pur.ResolveDeliveryLines(w.reviewer, []purapi.DeliveryLineRef{ref("delivery 1", "L1", 1)}); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("refs: %v", err)
	}
	if _, err = w.pur.ResolveDeliveryLines(w.reviewer, []purapi.DeliveryLineRef{ref("delivery.1", "L1", 0)}); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("version: %v", err)
	}
	for _, args := range [][]any{{nil, "L1", int64(1)}, {"delivery.1", nil, int64(1)}, {"delivery.1", "L1", nil}} {
		err = s.in(scope, func(ctx context.Context, exec bentopg.Executor) error {
			_, err := exec.Exec(ctx, purapi.SupplierDeliveryGuardSQL, args...)
			return err
		})
		if sqlCode(err) != "22004" {
			t.Fatalf("the guard needs %v: %s", args, sqlCode(err))
		}
	}
	s.guardAttributes("brant_pur.guard_supplier_delivery_v1(text, text, bigint)")
}

// TD-AT14 ("直发：承诺 120 件，核实交付 100 件，另有未核实上报 20 件 … 出运 120 件返回
// SUPPLIER_DELIVERY_INSUFFICIENT；未核实上报不计入") and ES-12 step 4 ("出运 100 件，不超核实交付量") through the
// guard, with the throwaway FUL keeping its own count: 120 cannot ship on the delivery verified at
// 100; the claim of 20 serves no shipment at all; 100 ship, and then not a piece more.
func TestSupplierDeliveryServesShipmentsOnlyAsVerifiedTDAT14(t *testing.T) {
	w := newDeliveryWorld(t)
	s := w.s
	w.verified("delivery.1", "120", "100")
	_, err := w.x.record(w.merch, "delivery.2", "commitment.1", "20")
	w.must(err)
	s.prepare("ship.120", deliveryRef("delivery.1", 1), "120")
	if v, err := s.confirm("ship.120", stage{}); !errors.Is(err, errDeliveryInsufficient) || v.VerifiedQuantity != "100" {
		t.Fatalf("120 on the 100 verified: %v, %+v", err, v)
	}
	s.prepare("ship.claim", deliveryRef("delivery.2", 1), "20")
	if v, err := s.confirm("ship.claim", stage{}); !errors.Is(err, errReferenceRefused) || code(err) != purapi.CodeSupplierDeliveryUnverified ||
		v.Status != purapi.DeliveryRecorded {
		t.Fatalf("a shipment on the claim: %v, %+v", err, v)
	}
	s.prepare("ship.100", deliveryRef("delivery.1", 1), "100")
	if _, err := s.confirm("ship.100", stage{}); err != nil {
		t.Fatalf("100 on the 100 verified: %v", err)
	}
	s.prepare("ship.more", deliveryRef("delivery.1", 1), "0.000001")
	if _, err := s.confirm("ship.more", stage{}); !errors.Is(err, errDeliveryInsufficient) {
		t.Fatalf("FUL's own count: %v", err)
	}
	if s.status("ship.100") != "confirmed" || s.status("ship.120") != "prepared" || s.status("ship.claim") != "prepared" || s.status("ship.more") != "prepared" {
		t.Fatal("only the 100 shipped")
	}
}

// ADR-BR-004 option B against PUR's supplier deliveries: whichever transaction locks the delivery
// root first decides (FUL-INV-003 "交付更正与出运确认并发按锁序裁决").
func TestSupplierDeliveryGuardDecidesEveryCommitOrder(t *testing.T) {
	w := newDeliveryWorld(t)
	x, s := w.x, w.s
	// corrected leaves delivery id verified at 30 in version 1, with the correction to 20 — version 2,
	// recorded by the merchandiser — awaiting verification at row version 3.
	corrected := func(id string) {
		t.Helper()
		w.verified(id, "30", "30")
		_, err := x.correct(w.merch, id, "commitment.1", "20", 2)
		w.must(err)
	}

	// The correction is verified first: the shipment's guard then answers REFERENCE_STALE for the
	// superseded version and nothing ships on it.
	t.Run("PUR commits first", func(t *testing.T) {
		corrected("delivery.a")
		s.prepare("ship.a", deliveryRef("delivery.a", 1), "30")
		_, err := x.verify(w.author, "delivery.a", 3, "20")
		w.must(err)
		v, err := s.confirm("ship.a", stage{})
		if !errors.Is(err, errReferenceRefused) || code(err) != purapi.CodeReferenceStale || v.Status != purapi.DeliverySuperseded || v.VerifiedVersion != 2 {
			t.Fatalf("confirmation %v, verdict %+v; want refused REFERENCE_STALE", err, v)
		}
		if s.status("ship.a") != "prepared" {
			t.Fatal("shipped on a superseded delivery version")
		}
	})

	// The shipment's guard passes — a correction that only awaits verification changes nothing — and
	// holds the delivery root FOR SHARE; the correction's verification waits until the shipment
	// commits, then supersedes the version: what shipped stays shipped.
	t.Run("downstream holds the guard first", func(t *testing.T) {
		corrected("delivery.b")
		s.prepare("ship.b", deliveryRef("delivery.b", 1), "30")
		type outcome struct {
			at  time.Time
			err error
		}
		verified := make(chan outcome, 1)
		pids := make(chan int32, 1)
		var released time.Time
		v, err := s.confirm("ship.b", stage{pid: pids, hold: func() {
			pid := <-pids
			go func() {
				_, err := x.verify(w.author, "delivery.b", 3, "20")
				verified <- outcome{time.Now(), err}
			}()
			await(t, "the correction's verification waits on the guard", func() bool { return s.blocks(pid) }, verified)
			released = time.Now() // the shipment confirms and commits after this
		}})
		if err != nil || !v.Usable() || v.VerifiedQuantity != "30" {
			t.Fatalf("confirmation %v, verdict %+v", err, v)
		}
		r := <-verified
		if r.err != nil || !r.at.After(released) {
			t.Fatalf("verification %v, returned after the shipment moved on: %v", r.err, r.at.After(released))
		}
		if g := s.deliveryGuard(s.scope, deliveryRef("delivery.b", 1)); s.status("ship.b") != "confirmed" || g.Status != purapi.DeliverySuperseded ||
			g.Code != purapi.CodeReferenceStale {
			t.Fatalf("after the correction: shipment %s, version 1 %+v", s.status("ship.b"), g)
		}
	})

	// Recording a correction waits for the guard too: it moves the delivery root.
	t.Run("downstream holds the guard while a correction is recorded", func(t *testing.T) {
		w.verified("delivery.r", "30", "30")
		s.prepare("ship.r", deliveryRef("delivery.r", 1), "30")
		type outcome struct {
			at  time.Time
			err error
		}
		recorded := make(chan outcome, 1)
		pids := make(chan int32, 1)
		var released time.Time
		v, err := s.confirm("ship.r", stage{pid: pids, hold: func() {
			pid := <-pids
			go func() {
				_, err := x.correct(w.merch, "delivery.r", "commitment.1", "20", 2)
				recorded <- outcome{time.Now(), err}
			}()
			await(t, "the correction's record waits on the guard", func() bool { return s.blocks(pid) }, recorded)
			released = time.Now()
		}})
		if err != nil || !v.Usable() {
			t.Fatalf("confirmation %v, verdict %+v", err, v)
		}
		r := <-recorded
		if r.err != nil || !r.at.After(released) {
			t.Fatalf("record %v, returned after the shipment moved on: %v", r.err, r.at.After(released))
		}
		if g := s.deliveryGuard(s.scope, deliveryRef("delivery.r", 1)); !g.Usable() || g.VerifiedVersion != 1 {
			t.Fatalf("a pending correction leaves version 1 verified: %+v", g)
		}
	})

	// The correction's verification writes the delivery and holds its root (a transaction holding its
	// idempotency key keeps it from committing); the shipment's guard waits on the root and, when the
	// verification commits, reads the delivery as it left it and refuses.
	t.Run("PUR holds the row first", func(t *testing.T) {
		corrected("delivery.c")
		s.prepare("ship.c", deliveryRef("delivery.c", 1), "30")
		cmd := x.verifyCmd("delivery.c", 3, "20")
		blockerPID, release := s.keyBlocker(t, "brant_pur", purapi.CommandVerifySupplierDelivery, cmd.IdempotencyKey)
		verified := make(chan error, 1)
		go func() {
			_, err := w.pur.VerifySupplierDelivery(w.author, cmd)
			verified <- err
		}()
		if !awaitInHold(t, "the verification holds the delivery root and waits on the blocker", func() bool { return s.blocks(blockerPID) }, verified) {
			release()
			return
		}
		type outcome struct {
			v   purapi.SupplierDeliveryVerdict
			err error
		}
		confirmed := make(chan outcome, 1)
		pids := make(chan int32, 1)
		go func() {
			v, err := s.confirm("ship.c", stage{pid: pids})
			confirmed <- outcome{v, err}
		}()
		downstreamPID := <-pids
		ok := awaitInHold(t, "the guard waits on the verification", func() bool { return s.waitsForLock(downstreamPID) }, confirmed)
		release()
		if !ok {
			return
		}
		if err := <-verified; err != nil {
			t.Fatalf("verification: %v", err)
		}
		c := <-confirmed
		if !errors.Is(c.err, errReferenceRefused) || code(c.err) != purapi.CodeReferenceStale || c.v.Status != purapi.DeliverySuperseded || c.v.VerifiedVersion != 2 {
			t.Fatalf("confirmation %v, verdict %+v; want refused REFERENCE_STALE after the correction", c.err, c.v)
		}
		if s.status("ship.c") != "prepared" {
			t.Fatal("shipped on a superseded delivery version")
		}
	})
}
