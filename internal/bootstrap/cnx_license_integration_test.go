//go:build integration

package bootstrap_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	"go.idp.xyz/idp-brant/internal/contexts/cnx"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CNX's export licences and their usage ledger (S5 B5, WBS S5-14; tactical model
// CNX-AGG-ExportLicense, CNX-INV-002; MD-44; TD-AT13; ES-12 step 3; S5 decisions D-3, E-4). Python
// V0.50 has no CNX ("—（Python 未实现）"), so no Python test is ported; the vectors come from the
// design. Every licence is synthetic (E-4: apparel needs none in reality).
//
// FUL is not in this tree (S5 B1a builds shipments, B1b export lots): the suite's CNX is wired to
// the real POL, EVD, TRD, PTY and reference data and to an in-memory FUL port (fakeExports) and,
// for a licence by classification, a fixed CAT port; the composed process's CNX answers
// DEPENDENCY_NOT_SUPPORTED for an allocation (shown over HTTP). The downstream side of
// ExportLicenseGuard v1 is a transaction of another transaction owner in the company's scope, as
// FUL's commercial release will run it.

// fakeExports is the in-memory FUL port: shipments and export lots as FUL would answer them.
type fakeExports struct {
	mu      sync.Mutex
	exports map[cnx.ExportRef]cnx.Export
}

func (f *fakeExports) ResolveExports(_ context.Context, refs []cnx.ExportRef) (map[cnx.ExportRef]cnx.Export, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[cnx.ExportRef]cnx.Export{}
	for _, r := range refs {
		if x, ok := f.exports[r]; ok {
			out[r] = x
		}
	}
	return out, nil
}

func (f *fakeExports) put(x cnx.Export) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exports[x.Ref] = x
}

// fixedProfiles is a CAT port that answers sku.1 r1 under CN-2026 as verified with commodity code
// 6109100021.
type fixedProfiles struct{}

func (fixedProfiles) ResolveProfiles(_ context.Context, refs []cnx.ProfileRef) (map[cnx.ProfileRef]cnx.Profile, error) {
	out := map[cnx.ProfileRef]cnx.Profile{}
	for _, r := range refs {
		if r.SkuID == "sku.1" && r.SpecRevision == 1 && r.CodeTableVersion == codeTable {
			out[r] = cnx.Profile{ProfileID: "decl.sku.1", Revision: 1, CodeTableVersion: codeTable, Verified: true, CommodityCode: "6109100021",
				ExportControl: cnx.NotControlled, ContentHash: strings.Repeat("c", 64)}
		}
	}
	return out, nil
}

type licenseWorld struct {
	*world
	cnx                   *cnx.Module
	ful                   *fakeExports
	app                   *bentopg.DB
	pool                  *pgxpool.Pool
	doc1, doc2, agent     context.Context
	outside               context.Context
	usd                   polapi.ConfigRef
	licenseFile, evidence string
}

func newLicenseWorld(t *testing.T) *licenseWorld {
	w := &licenseWorld{world: newWorld(t), ful: &fakeExports{exports: map[cnx.ExportRef]cnx.Export{}}}
	editor, reviewer := w.polActors()
	w.usd = w.currency(editor, reviewer, "USD", newWindow())
	w.doc1, w.doc2 = w.assign("doc.1", "documentation"), w.assign("doc.2", "documentation")
	w.agent = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.doc"},
		Permissions: []string{"export_license.register", "export_license.allocate", "export_license.release", "cnx.read", "evd.read"},
		DataScope:   "company"})
	w.outside = w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "doc.b"},
		Permissions: []string{"export_license.register", "export_license.allocate", "export_license.release", "cnx.read", "evd.read"},
		DataScope:   "company"})
	_, err := w.file(w.doc1, "licence.1", "licence.txt", evdapi.MimeText, "合成出口许可证（测试数据）", "")
	w.must(err)
	w.licenseFile = "licence.1"
	w.evidence = w.source(w.doc1, "receipt.1", "合成报关回执：改单／撤销（测试数据）")
	w.pool = pgtest.AppPool(t)
	db, err := bentopg.NewDB(w.pool)
	w.must(err)
	w.app = db
	cdb, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	w.cnx, err = cnx.New(cdb, cnx.Upstream{Orders: cnx.TRDOrders(w.c.TRD), Countries: cnx.PTYCountries(w.c.PTY), Profiles: fixedProfiles{},
		Programmes: cnx.REFProgrammes(w.c.REF), Configs: w.c.POL, Sources: w.c.EVD, Exports: w.ful, Purchases: cnx.PURLines(w.c.PUR),
		Funds: cnx.STLFunds(w.c.STL)})
	w.must(err)
	return w
}

// registerCmd is a synthetic licence: 1,000 PCS of sku.1 r1 for USD 10,000.00, destination DE,
// end user customer.1, valid from an hour ago for 30 days.
func (w *licenseWorld) registerCmd(number string) cnxapi.RegisterLicense {
	now := time.Now().UTC()
	return cnxapi.RegisterLicense{IdempotencyKey: w.key(), LicenseNumber: number, Issuer: "合成发证机关（测试）", Holder: "合成出口商（测试）",
		Goods: cnxapi.LicensedGoods{SkuSpecs: []cnxapi.SpecRef{{SkuID: "sku.1", SpecRevision: 1}}}, LicensedQuantity: "1000", Unit: "PCS",
		Currency: cnxapi.CurrencyRef{ID: w.usd.ConfigID, Revision: w.usd.Revision}, LicensedValue: "10000.00", Destination: "DE",
		EndUser: cnxapi.EndUser{PartyID: "customer.1", Name: "合成最终用户"}, ValidFrom: kernel.FormatTimestamp(now.Add(-time.Hour)),
		ValidUntil: kernel.FormatTimestamp(now.Add(30 * 24 * time.Hour)), FileID: w.licenseFile}
}

func (w *licenseWorld) register(number string) cnxapi.LicenseReceipt {
	w.t.Helper()
	r, err := w.cnx.RegisterLicense(w.doc1, w.registerCmd(number))
	w.must(err)
	return r
}

// shipment puts a prepared shipment of quantity PCS of sku.1 r1 at USD 10.00 each into FUL.
func (w *licenseWorld) shipment(id, quantity string) []cnxapi.ShipmentLine {
	w.t.Helper()
	q := kernel.MustDecimal(quantity)
	value, err := kernel.RoundMoney(kernel.MustCurrency("USD", 2), q.Mul(kernel.MustDecimal("10")))
	w.must(err)
	w.ful.put(cnx.Export{Ref: cnx.ExportRef{Kind: cnxapi.TargetShipment, ID: id}, Status: cnx.ShipmentPrepared, Version: 1, OrderID: "order.1",
		OrderRevision: 1, Destination: "DE", EndUserPartyID: "customer.1", Value: value,
		Lines: []cnx.ExportLine{{LineID: id + ".l1", SkuID: "sku.1", SpecRevision: 1, Quantity: q, Unit: "PCS"}}})
	return []cnxapi.ShipmentLine{{LineID: id + ".l1", SkuID: "sku.1", SpecRevision: 1, Quantity: quantity, Unit: "PCS"}}
}

func (w *licenseWorld) allocate(ctx context.Context, license, allocation, shipment string) (cnxapi.LicenseReceipt, error) {
	return w.cnx.AllocateLicense(ctx, cnxapi.AllocateLicense{IdempotencyKey: w.key(), LicenseID: license, AllocationID: allocation,
		Target: cnxapi.ExportTarget{Kind: cnxapi.TargetShipment, ID: shipment}})
}

func (w *licenseWorld) releaseCmd(license string, version int64, allocation, release, kind string) cnxapi.ReleaseLicense {
	cmd := cnxapi.ReleaseLicense{IdempotencyKey: w.key(), LicenseID: license, ExpectedVersion: version, AllocationID: allocation, ReleaseID: release,
		ExternalResult: &cnxapi.ExternalResult{Kind: kind, DeclarationNumber: "SYN-DEC-0001", ReceiptText: "海关回执原文（合成）",
			ReceivedAt: kernel.FormatTimestamp(time.Now().UTC().Add(-time.Minute)), EvidenceIDs: []string{w.evidence}},
		Reason: "按外部回执释放用量（合成）"}
	if kind == cnxapi.ExternalAmended {
		cmd.Quantity, cmd.Value = "100", "1000.00"
	}
	return cmd
}

// inApp runs fn in a transaction of another transaction owner (FUL's, once merged) in company's
// scope.
func (w *licenseWorld) inApp(company string, fn func(ctx context.Context, exec bentopg.Executor) error) error {
	return bento.ScopeTransactor{DB: w.app, Scope: w.scope(company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := w.app.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, exec)
	})
}

// licenseGuard calls ExportLicenseGuard v1 for shipment at at in a downstream transaction; pid
// receives its backend pid and hold runs while the transaction holds the guard's locks.
func (w *licenseWorld) licenseGuard(company, shipment string, at time.Time, pid chan<- int32, hold func()) cnxapi.ExportLicenseVerdict {
	var v cnxapi.ExportLicenseVerdict
	err := w.inApp(company, func(ctx context.Context, exec bentopg.Executor) error {
		if pid != nil {
			var p int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&p); err != nil {
				return err
			}
			pid <- p
		}
		var err error
		v, err = cnxapi.ScanExportLicenseGuard(shipment, exec.QueryRow(ctx, cnxapi.ExportLicenseGuardSQL, cnxapi.ExportLicenseGuardArgs(shipment, at)...).Scan)
		if err == nil && hold != nil {
			hold()
		}
		return err
	})
	if err != nil {
		w.t.Errorf("guard: %v", err)
	}
	return v
}

func (w *licenseWorld) wantLicenseVerdict(shipment, status, reason string) cnxapi.ExportLicenseVerdict {
	w.t.Helper()
	v := w.licenseGuard("company.A", shipment, time.Now(), nil, nil)
	if v.Status != status || v.Reason != reason || v.Covered != (status == cnxapi.LicenseStatusAllocated) ||
		(v.Covered != (v.Code == "")) || (!v.Covered && v.Code != cnxapi.CodeExportLicenseRequired) {
		w.t.Fatalf("guard for %s: %+v, want %s/%s", shipment, v, status, reason)
	}
	return v
}

// CNX-INV-002 positive and TD-AT13 (licence 1,000, allocated 600, allocating 500 →
// EXPORT_LICENSE_EXCEEDED; 400 fits), ES-12 step 3: a documentation clerk registers a licence with
// its EVD file; an AI agent allocates it to a prepared shipment on the same command path;
// ExportLicenseGuard v1 then covers exactly that shipment's lines. Registration is human only and
// unique per issuer and number; replays, reads, events, the audit trail, reconciliation and FORCE
// RLS behave as for every context.
func TestTDAT13ExportLicenseRegisterAllocateAndGuard(t *testing.T) {
	w := newLicenseWorld(t)
	cmd := w.registerCmd("SYN-EL-2026-0001")
	_, err := w.cnx.RegisterLicense(w.agent, cmd)
	w.want(err, kernel.CodePermissionDenied)
	reg, err := w.cnx.RegisterLicense(w.doc1, cmd)
	w.must(err)
	if reg.Status != cnxapi.LicenseStatusRegistered || reg.RowVersion != 1 || reg.LicenseID == "" || reg.Usage.AvailableQuantity != "1000" ||
		reg.Usage.LicensedValueMinor != "1000000" || reg.Usage.Currency != "USD" || reg.Usage.AllocatedQuantity != "0" {
		t.Fatalf("register %+v", reg)
	}
	if replay, err := w.cnx.RegisterLicense(w.doc1, cmd); err != nil || !replay.Replayed || replay.ActionID != reg.ActionID {
		t.Fatalf("replay %+v %v", replay, err)
	}
	again := w.registerCmd("SYN-EL-2026-0001")
	_, err = w.cnx.RegisterLicense(w.doc2, again)
	w.want(err, cnxapi.CodeBusinessKeyConflict)
	bad := w.registerCmd("SYN-EL-2026-0002")
	bad.LicensedValue = "10000.001"
	_, err = w.cnx.RegisterLicense(w.doc1, bad)
	w.want(err, kernel.CodeInvalidKnownValue)
	bad = w.registerCmd("SYN-EL-2026-0002")
	bad.Currency.Revision = 99
	_, err = w.cnx.RegisterLicense(w.doc1, bad)
	w.want(err, kernel.CodeReferenceInvalid)
	bad = w.registerCmd("SYN-EL-2026-0002")
	bad.FileID = "no.such.file"
	_, err = w.cnx.RegisterLicense(w.doc1, bad)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/file_id" {
		t.Fatalf("missing file: %s %s", code(err), path(err))
	}

	lines1 := w.shipment("ship.1", "600")
	w.wantLicenseVerdict("ship.1", cnxapi.LicenseStatusRequired, cnxapi.LicenseReasonNotAllocated)
	a1, err := w.allocate(w.agent, reg.LicenseID, "alloc.1", "ship.1")
	w.must(err)
	if a1.RowVersion != 2 || a1.AllocationID != "alloc.1" || a1.Usage.AllocatedQuantity != "600" || a1.Usage.AvailableQuantity != "400" ||
		a1.Usage.AvailableValueMinor != "400000" {
		t.Fatalf("allocate %+v", a1)
	}
	v := w.wantLicenseVerdict("ship.1", cnxapi.LicenseStatusAllocated, cnxapi.LicenseReasonAllocated)
	if v.LicenseID != reg.LicenseID || v.AllocationID != "alloc.1" || v.Quantity != "600" || v.Unit != "PCS" || v.ValueMinor != "600000" ||
		v.Currency != "USD" || v.ValidUntil.IsZero() || v.Evidence() != "allocated/allocated/"+reg.LicenseID+"/alloc.1/"+v.LinesHash {
		t.Fatalf("verdict %+v", v)
	}
	if err := v.Check("/args/shipment_id", lines1); err != nil {
		t.Fatalf("the allocation covers the shipment's lines: %v", err)
	}
	changed := []cnxapi.ShipmentLine{lines1[0]}
	changed[0].Quantity = "601"
	if err := v.Check("/args/shipment_id", changed); code(err) != cnxapi.CodeExportLicenseRequired {
		t.Fatalf("changed lines: %v", err)
	}

	// TD-AT13: 600 allocated, 500 more exceed the 1,000 licensed; 400 fit; then 1 more exceeds.
	w.shipment("ship.2", "500")
	_, err = w.allocate(w.doc1, reg.LicenseID, "alloc.2", "ship.2")
	if code(err) != cnxapi.CodeExportLicenseExceeded || path(err) != "/args/target/id" {
		t.Fatalf("600 + 500: %s %s", code(err), path(err))
	}
	w.shipment("ship.2", "400")
	a2, err := w.allocate(w.doc1, reg.LicenseID, "alloc.2", "ship.2")
	w.must(err)
	if a2.Usage.AvailableQuantity != "0" || a2.Usage.AvailableValueMinor != "0" {
		t.Fatalf("600 + 400: %+v", a2.Usage)
	}
	w.shipment("ship.3", "1")
	_, err = w.allocate(w.doc1, reg.LicenseID, "alloc.3", "ship.3")
	w.want(err, cnxapi.CodeExportLicenseExceeded)

	// The refusals before the domain's: allocation ID taken, a target FUL does not know, one not
	// prepared, one that holds an allocation, a licence the company lacks.
	_, err = w.allocate(w.doc1, reg.LicenseID, "alloc.1", "ship.3")
	w.want(err, cnxapi.CodeBusinessKeyConflict)
	_, err = w.allocate(w.doc1, reg.LicenseID, "alloc.4", "ship.unknown")
	w.want(err, kernel.CodeReferenceInvalid)
	w.ful.put(cnx.Export{Ref: cnx.ExportRef{Kind: cnxapi.TargetShipment, ID: "ship.4"}, Status: "cancelled", Version: 2})
	_, err = w.allocate(w.doc1, reg.LicenseID, "alloc.4", "ship.4")
	w.want(err, kernel.CodeInvalidState)
	_, err = w.allocate(w.doc1, reg.LicenseID, "alloc.4", "ship.1")
	w.want(err, kernel.CodeInvalidState)
	_, err = w.allocate(w.doc1, "elc.none", "alloc.4", "ship.3")
	w.want(err, kernel.CodeNotFound)

	view, err := w.cnx.GetLicense(w.doc2, reg.LicenseID)
	w.must(err)
	if view.State != cnxapi.LicenseValid || view.RowVersion != 3 || len(view.Allocations) != 2 || view.Usage.AllocatedQuantity != "1000" ||
		view.LicensedValue != "10000.00" || view.Currency.Code != "USD" || view.File.FileID != w.licenseFile || len(view.File.ContentSHA256) != 64 ||
		view.RegisteredBy.ID != "doc.1" || view.Goods.SkuSpecs[0].SkuID != "sku.1" {
		t.Fatalf("view %+v", view)
	}
	if a := view.Allocations[0]; a.AllocationID != "alloc.1" || a.Status != cnxapi.AllocationAllocated || a.OutstandingQuantity != "600" ||
		a.AllocatedBy.Kind != kernel.ActorAI || a.Attempt != 1 || a.LinesHash != v.LinesHash || a.Lines[0].Quantity != "600" {
		t.Fatalf("allocation %+v", a)
	}
	list, err := w.cnx.Licenses(w.doc2)
	if err != nil || len(list) != 1 || list[0].Usage.AvailableQuantity != "0" {
		t.Fatalf("list %+v %v", list, err)
	}
	read, err := w.cnx.ShipmentLicense(w.doc2, "ship.2")
	if err != nil || !read.Covered || read.AllocationID != "alloc.2" || read.CheckedAt.IsZero() {
		t.Fatalf("shipment read %+v %v", read, err)
	}

	events := w.events("company.A", cnxapi.Source)
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	if strings.Join(types, " ") != cnxapi.EventLicenseRegistered+" "+cnxapi.EventLicenseAllocated+" "+cnxapi.EventLicenseAllocated ||
		events[1].Payload["allocation_id"] != "alloc.1" || events[1].Payload["quantity"] != "600" || events[1].Payload["lines_hash"] != v.LinesHash {
		t.Fatalf("events %+v", events)
	}
	if _, err := w.cnx.Audit(w.doc1); err == nil {
		t.Fatal("documentation holds no cnx.audit")
	}
	audit, err := w.cnx.Audit(w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "auditor"},
		Permissions: []string{"cnx.audit"}}))
	w.must(err)
	var trail []string
	for _, e := range audit {
		trail = append(trail, e.Command+"/"+e.Outcome+"/"+e.Code)
	}
	for _, want := range []string{"export_license.register/rejected/PERMISSION_DENIED", "export_license.register/succeeded/",
		"export_license.register/replayed/", "export_license.register/rejected/BUSINESS_KEY_CONFLICT",
		"export_license.allocate/rejected/EXPORT_LICENSE_EXCEEDED", "export_license.allocate/succeeded/", "export_license.get/read/"} {
		if !strings.Contains(strings.Join(trail, " "), want) {
			t.Fatalf("audit lacks %s: %v", want, trail)
		}
	}
	rec, err := w.cnx.Reconcile(w.doc2, cmd)
	if err != nil || rec.State != kernel.ReconcileCommitted || rec.Receipt == nil || rec.Receipt.ObjectID != reg.LicenseID {
		t.Fatalf("reconcile %+v %v", rec, err)
	}

	// FORCE RLS: company.B sees neither the licence nor an allocation for the same shipment.
	_, err = w.cnx.GetLicense(w.outside, reg.LicenseID)
	w.want(err, kernel.CodeNotFound)
	if v := w.licenseGuard("company.B", "ship.1", time.Now(), nil, nil); v.Covered || v.Reason != cnxapi.LicenseReasonNotAllocated {
		t.Fatalf("company.B guard %+v", v)
	}
	if n := w.count("company.B", "brant_cnx.export_license_allocation"); n != 0 {
		t.Fatalf("company.B sees %d allocations", n)
	}

	// The composed process reads FUL (S5 C3a): FUL has no shipment ship.1, so the target is
	// REFERENCE_INVALID.
	_, err = w.c.CNX.AllocateLicense(w.doc1, cnxapi.AllocateLicense{IdempotencyKey: w.key(), LicenseID: reg.LicenseID, AllocationID: "alloc.9",
		Target: cnxapi.ExportTarget{Kind: cnxapi.TargetShipment, ID: "ship.1"}})
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/target/id" {
		t.Fatalf("composed allocation of a shipment FUL does not have: %s %s", code(err), path(err))
	}
}

// TD-AT13 "另一许可证已过期" and the catalog's EXPORT_LICENSE_EXPIRED ("许可证已过期或目的国、最终用户不一致"):
// an expired licence, another destination or end user, goods the licence does not cover; a licence
// by classification covers a line through its verified declaration profile (R39). The guard says
// expired at an instant after the licence's validity.
func TestTDAT13ExpiredLicenceAndMismatchesAreRefused(t *testing.T) {
	w := newLicenseWorld(t)
	expired := w.registerCmd("SYN-EL-EXPIRED")
	expired.ValidFrom = kernel.FormatTimestamp(time.Now().UTC().Add(-60 * 24 * time.Hour))
	expired.ValidUntil = kernel.FormatTimestamp(time.Now().UTC().Add(-24 * time.Hour))
	old, err := w.cnx.RegisterLicense(w.doc1, expired)
	w.must(err)
	w.shipment("ship.1", "10")
	_, err = w.allocate(w.doc1, old.LicenseID, "alloc.1", "ship.1")
	if code(err) != cnxapi.CodeExportLicenseExpired || path(err) != "/args/license_id" {
		t.Fatalf("expired: %s %s", code(err), path(err))
	}
	if v, err := w.cnx.GetLicense(w.doc1, old.LicenseID); err != nil || v.State != cnxapi.LicenseExpired {
		t.Fatalf("view %+v %v", v, err)
	}

	l := w.register("SYN-EL-0002")
	for name, edit := range map[string]func(*cnx.Export){
		"destination": func(x *cnx.Export) { x.Destination = "FR" },
		"end user":    func(x *cnx.Export) { x.EndUserPartyID = "customer.2" },
		"goods":       func(x *cnx.Export) { x.Lines[0].SpecRevision = 2 },
	} {
		w.shipment("ship.2", "10")
		x := w.ful.exports[cnx.ExportRef{Kind: cnxapi.TargetShipment, ID: "ship.2"}]
		edit(&x)
		w.ful.put(x)
		if _, err := w.allocate(w.doc1, l.LicenseID, "alloc.2", "ship.2"); code(err) != cnxapi.CodeExportLicenseExpired || path(err) != "/args/target/id" {
			t.Errorf("%s: %s %s", name, code(err), path(err))
		}
	}

	byCode := w.registerCmd("SYN-EL-0003")
	byCode.Goods = cnxapi.LicensedGoods{Classification: &cnxapi.Classification{CodeTableVersion: codeTable, CommodityCodes: []string{"6109100021"}}}
	c, err := w.cnx.RegisterLicense(w.doc1, byCode)
	w.must(err)
	w.shipment("ship.3", "10")
	if _, err := w.allocate(w.doc1, c.LicenseID, "alloc.3", "ship.3"); err != nil {
		t.Fatalf("classified line: %v", err)
	}
	x := w.ful.exports[cnx.ExportRef{Kind: cnxapi.TargetShipment, ID: "ship.2"}]
	x.Destination, x.EndUserPartyID, x.Lines[0].SpecRevision = "DE", "customer.1", 2
	w.ful.put(x)
	if _, err := w.allocate(w.doc1, c.LicenseID, "alloc.4", "ship.2"); code(err) != cnxapi.CodeExportLicenseExpired {
		t.Fatalf("unclassified line: %s", code(err))
	}

	v := w.licenseGuard("company.A", "ship.3", time.Now().Add(31*24*time.Hour), nil, nil)
	if v.Covered || v.Status != cnxapi.LicenseStatusExpired || v.Reason != cnxapi.LicenseReasonExpired || v.Code != cnxapi.CodeExportLicenseRequired {
		t.Fatalf("after the validity: %+v", v)
	}
	if err := v.Check("/args/shipment_id", nil); code(err) != cnxapi.CodeExportLicenseRequired {
		t.Fatalf("check %v", err)
	}
}

// waitBlocked returns once n backends wait for locks behind pid: the first waits for pid's row lock,
// the next ones queue behind it on the same row.
func waitBlocked(t *testing.T, pool *pgxpool.Pool, pid int32, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var got int
		if err := pool.QueryRow(context.Background(), `WITH RECURSIVE behind(pid) AS (
				SELECT a.pid FROM pg_stat_activity a WHERE $1 = ANY (pg_blocking_pids(a.pid))
				UNION SELECT a.pid FROM pg_stat_activity a JOIN behind b ON b.pid = ANY (pg_blocking_pids(a.pid)))
			SELECT count(*) FROM behind`, pid).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d backends wait for %d", got, n, pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// CNX-INV-002 concurrency "两笔并发分配合计超额时只有一笔成功" (TD-AT13, WBS S5-14 acceptance), on
// PG16: two allocations of 600 each to two shipments start together while the licence row is held;
// both wait on the licence root; when it is let go, one commits and the other, deriving the usage
// under the lock, is EXPORT_LICENSE_EXCEEDED. Σ allocated stays 600.
func TestTDAT13ConcurrentAllocationsOneWins(t *testing.T) {
	w := newLicenseWorld(t)
	l := w.register("SYN-EL-CONC")
	w.shipment("ship.a", "600")
	w.shipment("ship.b", "600")
	pid, held, release := make(chan int32, 1), make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- w.inApp("company.A", func(ctx context.Context, exec bentopg.Executor) error {
			var p int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&p); err != nil {
				return err
			}
			if _, err := exec.Exec(ctx, "SELECT 1 FROM brant_cnx.export_license WHERE license_id = $1 FOR UPDATE", l.LicenseID); err != nil {
				return err
			}
			pid <- p
			close(held)
			<-release
			return nil
		})
	}()
	p := <-pid
	<-held
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	defer letGo()
	results := make(chan error, 2)
	for _, s := range []string{"a", "b"} {
		go func() {
			_, err := w.allocate(w.doc1, l.LicenseID, "alloc."+s, "ship."+s)
			results <- err
		}()
	}
	waitBlocked(t, w.pool, p, 2)
	letGo()
	w.must(<-holder)
	var won, exceeded int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			won++
		case code(err) == cnxapi.CodeExportLicenseExceeded:
			exceeded++
		default:
			t.Fatalf("unexpected %v", err)
		}
	}
	if won != 1 || exceeded != 1 {
		t.Fatalf("won %d, exceeded %d", won, exceeded)
	}
	view, err := w.cnx.GetLicense(w.doc1, l.LicenseID)
	if err != nil || view.Usage.AllocatedQuantity != "600" || len(view.Allocations) != 1 || view.RowVersion != 2 {
		t.Fatalf("view %+v %v", view, err)
	}
}

// CNX-INV-002 "报关改单或撤销按外部结果释放用量，不凭本地取消恢复" and MD-44: a release needs an external
// result — without one, or with a local cause, it is refused at the contract — is human only, rests
// on evidence, releases what an amended receipt names and what a withdrawn one leaves; the released
// usage is available again and the shipment can be allocated anew; ExportLicenseGuard v1 then says
// released.
func TestExportLicenseReleaseOnlyOnAnExternalResult(t *testing.T) {
	w := newLicenseWorld(t)
	l := w.register("SYN-EL-REL")
	w.shipment("ship.1", "600")
	a, err := w.allocate(w.doc1, l.LicenseID, "alloc.1", "ship.1")
	w.must(err)

	none := w.releaseCmd(l.LicenseID, a.RowVersion, "alloc.1", "rel.1", cnxapi.ExternalWithdrawn)
	none.ExternalResult = nil
	_, err = w.cnx.ReleaseLicense(w.doc2, none)
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/external_result" {
		t.Fatalf("no external result: %s %s", code(err), path(err))
	}
	local := w.releaseCmd(l.LicenseID, a.RowVersion, "alloc.1", "rel.1", "cancelled")
	_, err = w.cnx.ReleaseLicense(w.doc2, local)
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/external_result/kind" {
		t.Fatalf("local cancellation: %s %s", code(err), path(err))
	}
	_, err = w.cnx.ReleaseLicense(w.agent, w.releaseCmd(l.LicenseID, a.RowVersion, "alloc.1", "rel.1", cnxapi.ExternalAmended))
	w.want(err, kernel.CodePermissionDenied)
	noEvidence := w.releaseCmd(l.LicenseID, a.RowVersion, "alloc.1", "rel.1", cnxapi.ExternalAmended)
	noEvidence.ExternalResult.EvidenceIDs = []string{"no.such.source"}
	_, err = w.cnx.ReleaseLicense(w.doc2, noEvidence)
	w.want(err, kernel.CodeReferenceInvalid)
	_, err = w.cnx.ReleaseLicense(w.doc2, w.releaseCmd(l.LicenseID, 1, "alloc.1", "rel.1", cnxapi.ExternalAmended))
	w.want(err, kernel.CodeVersionConflict)
	_, err = w.cnx.ReleaseLicense(w.doc2, w.releaseCmd(l.LicenseID, a.RowVersion, "alloc.none", "rel.1", cnxapi.ExternalAmended))
	w.want(err, kernel.CodeNotFound)
	tooMuch := w.releaseCmd(l.LicenseID, a.RowVersion, "alloc.1", "rel.1", cnxapi.ExternalAmended)
	tooMuch.Quantity = "601"
	_, err = w.cnx.ReleaseLicense(w.doc2, tooMuch)
	w.want(err, kernel.CodeInvalidState)

	amended, err := w.cnx.ReleaseLicense(w.doc2, w.releaseCmd(l.LicenseID, a.RowVersion, "alloc.1", "rel.1", cnxapi.ExternalAmended))
	w.must(err)
	if amended.ReleaseID != "rel.1" || amended.Usage.AllocatedQuantity != "500" || amended.Usage.AvailableValueMinor != "500000" {
		t.Fatalf("amended %+v", amended)
	}
	v := w.wantLicenseVerdict("ship.1", cnxapi.LicenseStatusAllocated, cnxapi.LicenseReasonAllocated)
	if v.Quantity != "500" {
		t.Fatalf("outstanding %+v", v)
	}
	if err := v.Check("/args/shipment_id", []cnxapi.ShipmentLine{{LineID: "ship.1.l1", SkuID: "sku.1", SpecRevision: 1, Quantity: "600", Unit: "PCS"}}); code(err) != cnxapi.CodeExportLicenseRequired {
		t.Fatalf("600 PCS on 500 outstanding: %v", err)
	}
	_, err = w.cnx.ReleaseLicense(w.doc2, w.releaseCmd(l.LicenseID, amended.RowVersion, "alloc.1", "rel.1", cnxapi.ExternalWithdrawn))
	w.want(err, cnxapi.CodeBusinessKeyConflict)
	withdrawn, err := w.cnx.ReleaseLicense(w.doc2, w.releaseCmd(l.LicenseID, amended.RowVersion, "alloc.1", "rel.2", cnxapi.ExternalWithdrawn))
	w.must(err)
	if withdrawn.Usage.AllocatedQuantity != "0" || withdrawn.Usage.AvailableQuantity != "1000" || withdrawn.RowVersion != 4 {
		t.Fatalf("withdrawn %+v", withdrawn)
	}
	w.wantLicenseVerdict("ship.1", cnxapi.LicenseStatusRequired, cnxapi.LicenseReasonReleased)
	_, err = w.cnx.ReleaseLicense(w.doc2, w.releaseCmd(l.LicenseID, withdrawn.RowVersion, "alloc.1", "rel.3", cnxapi.ExternalWithdrawn))
	w.want(err, kernel.CodeInvalidState)

	// The shipment's next attempt: allocated anew after the external result released the first.
	again, err := w.allocate(w.doc1, l.LicenseID, "alloc.2", "ship.1")
	w.must(err)
	if again.Usage.AllocatedQuantity != "600" {
		t.Fatalf("again %+v", again)
	}
	view, err := w.cnx.GetLicense(w.doc1, l.LicenseID)
	w.must(err)
	first := view.Allocations[0]
	if first.Status != cnxapi.AllocationReleased || len(first.Releases) != 2 || first.Releases[0].Kind != cnxapi.ExternalAmended ||
		first.Releases[0].Source != cnxapi.ResultStated || first.Releases[0].DeclarationNumber != "SYN-DEC-0001" ||
		len(first.Releases[0].Evidence) != 1 || first.Releases[1].Quantity != "500" || first.Releases[1].ReleasedBy.ID != "doc.2" ||
		view.Allocations[1].Attempt != 2 {
		t.Fatalf("ledger %+v", view.Allocations)
	}
	w.wantLicenseVerdict("ship.1", cnxapi.LicenseStatusAllocated, cnxapi.LicenseReasonAllocated)
}

// ExportLicenseGuard v1's commit orders on PG16 (R56 guardLock; the tactical lock order: the
// downstream's own roots FOR UPDATE, then CNX's guard rows FOR SHARE; CNX's release locks only its
// own licence root FOR UPDATE): a release that starts while FUL holds the guard waits until FUL
// commits, and FUL acted on the allocation it saw; a release that committed first is what FUL sees
// — the shipment is refused (EXPORT_LICENSE_REQUIRED); an allocation of another shipment of the
// same licence also waits for a holder.
func TestExportLicenseGuardDecidesEveryCommitOrder(t *testing.T) {
	w := newLicenseWorld(t)
	l := w.register("SYN-EL-ORDER")
	lines := w.shipment("ship.1", "600")
	a, err := w.allocate(w.doc1, l.LicenseID, "alloc.1", "ship.1")
	w.must(err)
	held := func(fn func() error, wantStatus string) {
		t.Helper()
		pid, held, release := make(chan int32, 1), make(chan struct{}), make(chan struct{})
		seen := make(chan cnxapi.ExportLicenseVerdict, 1)
		go func() {
			seen <- w.licenseGuard("company.A", "ship.1", time.Now(), pid, func() { close(held); <-release })
		}()
		p := <-pid
		<-held
		done := make(chan error, 1)
		go func() { done <- fn() }()
		waitBlockedBy(t, w.pool, p, done)
		close(release)
		if v := <-seen; v.Status != wantStatus {
			t.Fatalf("the downstream saw %+v, want %s", v, wantStatus)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// Downstream holding the guard → an allocation of another shipment waits; the downstream saw allocated.
	w.shipment("ship.2", "100")
	held(func() error {
		_, err := w.allocate(w.doc1, l.LicenseID, "alloc.2", "ship.2")
		return err
	}, cnxapi.LicenseStatusAllocated)
	// Downstream holding the guard → the release waits; the downstream saw allocated and covered.
	var rel cnxapi.LicenseReceipt
	held(func() error {
		var err error
		rel, err = w.cnx.ReleaseLicense(w.doc2, w.releaseCmd(l.LicenseID, a.RowVersion+1, "alloc.1", "rel.1", cnxapi.ExternalWithdrawn))
		return err
	}, cnxapi.LicenseStatusAllocated)
	if rel.Usage.AllocatedQuantity != "100" {
		t.Fatalf("release %+v", rel)
	}
	// The release committed first → the downstream is refused.
	v := w.wantLicenseVerdict("ship.1", cnxapi.LicenseStatusRequired, cnxapi.LicenseReasonReleased)
	if err := v.Check("/args/shipment_id", lines); code(err) != cnxapi.CodeExportLicenseRequired || path(err) != "/args/shipment_id" {
		t.Fatalf("after the release: %v", err)
	}
}

// CNX-INV-002 and D-3 in the database: brant_app cannot change a licence's terms, an allocation or
// a release, nor delete; the schema owner bypasses no trigger: a licence keeps its terms and moves
// by one version, entries never change, an allocation beyond the licensed quantity, one of a target
// that holds an allocation and a release of more than is outstanding are refused.
func TestExportLicenseRecordsAreAppendOnly(t *testing.T) {
	w := newLicenseWorld(t)
	l := w.register("SYN-EL-DB")
	w.shipment("ship.1", "600")
	_, err := w.allocate(w.doc1, l.LicenseID, "alloc.1", "ship.1")
	w.must(err)
	for _, sql := range []string{
		`UPDATE brant_cnx.export_license SET licensed_quantity = 5000`,
		`UPDATE brant_cnx.export_license_allocation SET quantity = 1`,
		`DELETE FROM brant_cnx.export_license_allocation`,
		`DELETE FROM brant_cnx.export_license`,
	} {
		if err := w.exec("company.A", sql); err == nil {
			t.Errorf("brant_app must not run %s", sql)
		}
	}
	owner := func(sql string) error {
		tx, err := pgtest.MigratorPool(w.t).Begin(context.Background())
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`,
			w.tenant); err != nil {
			return err
		}
		if _, err = tx.Exec(context.Background(), sql); err != nil {
			return err
		}
		return tx.Commit(context.Background())
	}
	for _, x := range []struct{ sql, want string }{
		{`UPDATE brant_cnx.export_license SET licensed_quantity = 5000, row_version = row_version + 1`, "keeps its terms"},
		{`UPDATE brant_cnx.export_license SET row_version = row_version + 2`, "keeps its terms"},
		{`DELETE FROM brant_cnx.export_license`, "never deleted"},
		{`UPDATE brant_cnx.export_license_allocation SET quantity = 1`, "never change"},
		{`DELETE FROM brant_cnx.export_license_allocation`, "never change"},
		// 600 more for another shipment: Σ 1,200 above the 1,000 licensed.
		{`INSERT INTO brant_cnx.export_license_allocation SELECT tenant_id, company_id, 'alloc.forged', license_id, target_kind, 'ship.2',
			target_version, 1, order_id, order_revision, destination, end_user_party_id, lines, lines_hash, quantity, value_minor, currency_code,
			allocated_by_kind, allocated_by_id, authority_epoch, allocated_at FROM brant_cnx.export_license_allocation`, "would be exceeded"},
		// A second attempt for ship.1 while its first allocation is outstanding.
		{`INSERT INTO brant_cnx.export_license_allocation SELECT tenant_id, company_id, 'alloc.forged', license_id, target_kind, target_id,
			target_version, 2, order_id, order_revision, destination, end_user_party_id, lines, lines_hash, 1, 0, currency_code,
			allocated_by_kind, allocated_by_id, authority_epoch, allocated_at FROM brant_cnx.export_license_allocation`, "holds an allocation"},
		{`INSERT INTO brant_cnx.export_license_release SELECT tenant_id, company_id, 'rel.forged', license_id, allocation_id, 'withdrawn',
			'stated', 'SYN', 'x', allocated_at, '[{"source_id":"x","content_sha256":"x"}]', quantity + 1, 0, 'x', 'human', 'doc.2',
			authority_epoch, allocated_at FROM brant_cnx.export_license_allocation`, "less outstanding"},
	} {
		if err := owner(x.sql); err == nil || !strings.Contains(err.Error(), x.want) {
			t.Errorf("must refuse %s: %v", x.sql, err)
		}
	}
}

// The composed process over HTTP with OIDC access tokens: a documentation clerk registers a
// licence through CNX's command route, reads it, the company's licences and a shipment's verdict,
// is refused an allocation of a shipment FUL does not have (REFERENCE_INVALID: the composed process
// reads FUL, S5 C3a) and a release without an external result (SCHEMA_INVALID), and reconciles a
// command never sent.
func TestExportLicensesOverHTTP(t *testing.T) {
	w := newLicenseWorld(t)
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	clerk := &client{t: t, base: api.URL, token: is.token(w.tenant, "company.A", "doc.1", "human"), n: 400}
	now := time.Now().UTC()
	args := map[string]any{"license_number": "SYN-EL-HTTP", "issuer": "合成发证机关（测试）", "holder": "合成出口商（测试）",
		"goods": map[string]any{"sku_specs": []any{map[string]any{"sku_id": "sku.1", "spec_revision": 1}}}, "licensed_quantity": "1000",
		"unit": "PCS", "currency": map[string]any{"id": w.usd.ConfigID, "revision": w.usd.Revision}, "licensed_value": "10000.00",
		"destination": "DE", "end_user": map[string]any{"party_id": "customer.1", "name": "合成最终用户"},
		"valid_from": kernel.FormatTimestamp(now.Add(-time.Hour)), "valid_until": kernel.FormatTimestamp(now.Add(24 * time.Hour)),
		"file_id": w.licenseFile}
	registered := clerk.command("cnx", cnxapi.Contract, cnxapi.CommandRegisterLicense, args)
	id, _ := registered["object_id"].(string)
	if registered["status"] != "registered" || registered["row_version"] != float64(1) || id == "" {
		t.Fatalf("register %v", registered)
	}
	status, view := clerk.do("GET", "/api/brant/v1/cnx/export_licenses/"+id, nil)
	usage, _ := view["usage"].(map[string]any)
	if status != 200 || view["state"] != "valid" || view["license_number"] != "SYN-EL-HTTP" || usage["available_quantity"] != "1000" ||
		usage["licensed_value_minor"] != "1000000" {
		t.Fatalf("licence %d %v", status, view)
	}
	if status, list := clerk.do("GET", "/api/brant/v1/cnx/export_licenses", nil); status != 200 || len(list["licenses"].([]any)) != 1 {
		t.Fatalf("list %d %v", status, list)
	}
	if status, v := clerk.do("GET", "/api/brant/v1/cnx/shipments/ship.1/export_license", nil); status != 200 || v["covered"] != false ||
		v["reason"] != "not_allocated" || v["code"] != cnxapi.CodeExportLicenseRequired {
		t.Fatalf("shipment %d %v", status, v)
	}
	status, out := clerk.try("cnx", cnxapi.Contract, cnxapi.CommandAllocateLicense, map[string]any{"license_id": id, "allocation_id": "alloc.1",
		"target": map[string]any{"kind": "shipment", "id": "ship.1"}})
	if status != 422 || out["code"] != kernel.CodeReferenceInvalid || errPath(out) != "/args/target/id" {
		t.Fatalf("allocate %d %v", status, out)
	}
	status, out = clerk.try("cnx", cnxapi.Contract, cnxapi.CommandReleaseLicense, map[string]any{"license_id": id, "expected_version": 1,
		"allocation_id": "alloc.1", "release_id": "rel.1", "reason": "本地取消"})
	if status != 422 || out["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("release without an external result %d %v", status, out)
	}
	status, rec := clerk.do("POST", "/api/brant/v1/cnx/commands/export_license.register/reconcile", map[string]any{"contract": cnxapi.Contract,
		"command": cnxapi.CommandRegisterLicense, "idempotency_key": "never.sent", "args": args})
	if status != 200 || rec["state"] != "not_committed" {
		t.Fatalf("reconcile %d %v", status, rec)
	}
}
