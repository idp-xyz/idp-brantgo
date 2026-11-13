package httpapi

import (
	"context"
	"testing"

	"go.idp.xyz/idp-bento-go/application"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
)

type fakeSUP struct {
	SUP
	seal  supapi.Seal
	offer supapi.SaveSupplierOffer
}

func (f *fakeSUP) Seal(_ context.Context, cmd supapi.Seal) (supapi.Receipt, error) {
	f.seal = cmd
	return supapi.Receipt{ActionID: "act-1", ObjectID: cmd.ID, RowVersion: 2, Status: "active"}, nil
}

func (f *fakeSUP) SaveSupplierOffer(_ context.Context, cmd supapi.SaveSupplierOffer) (supapi.Receipt, error) {
	f.offer = cmd
	return supapi.Receipt{ActionID: "act-2", ObjectID: cmd.OfferID, RowVersion: 1, Status: "draft"}, nil
}

func (f *fakeSUP) AuditRejected(context.Context, string, string, string) error { return nil }

type fakeCST struct {
	CST
	context     cstapi.SaveContext
	measurement cstapi.RecordMeasurement
}

func (f *fakeCST) SaveContext(_ context.Context, cmd cstapi.SaveContext) (cstapi.Receipt, error) {
	f.context = cmd
	return cstapi.Receipt{ActionID: "act-3", ObjectID: cmd.ContextID, RowVersion: 1, Status: "working"}, nil
}

func (f *fakeCST) RecordMeasurement(_ context.Context, cmd cstapi.RecordMeasurement) (cstapi.Receipt, error) {
	f.measurement = cmd
	return cstapi.Receipt{ActionID: "act-4", ObjectID: cmd.MeasurementID, RowVersion: 1, Status: "active"}, nil
}

func (f *fakeCST) AuditRejected(context.Context, string, string, string) error { return nil }

// SUP and CST commands decode by reflection: the command's ID member is "id", expected_version is
// required, SUP's kind comes from the route, and explicit JSON names of an api package win.
func TestReflectedCommandsDecodeTheApiStructures(t *testing.T) {
	sup, cst := &fakeSUP{}, &fakeCST{}
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{}, POL: struct{ POL }{},
		REF: struct{ REF }{}, SUP: sup, CST: cst, TRD: struct{ TRD }{}, PUR: struct{ PUR }{}, QLT: struct{ QLT }{}, CNX: struct{ CNX }{}, Verifier: tokens{"Bearer good": alice}, Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	post := func(bc, contract, command, args string) answer {
		return call(t, h, "POST", "/api/brant/v1/"+bc+"/commands/"+command, "Bearer good",
			`{"contract":"`+contract+`","command":"`+command+`","idempotency_key":"k1","args":`+args+`}`)
	}
	a := post("sup", supapi.Contract, "purchase_terms.publish",
		`{"id":"terms.1","expected_version":1,"working_revision":1,"evidence":[{"source_id":"src.1","locator":"p1"}]}`)
	if a.status != 200 || sup.seal.Kind != supapi.KindPurchaseTerms || sup.seal.ID != "terms.1" || sup.seal.ExpectedVersion != 1 ||
		sup.seal.WorkingRevision != 1 || sup.seal.IdempotencyKey != "k1" || len(sup.seal.Evidence) != 1 || sup.seal.Evidence[0].Locator != "p1" {
		t.Fatalf("%d %v %+v", a.status, a.body, sup.seal)
	}
	a = post("sup", supapi.Contract, "supplier_offer.save_draft",
		`{"id":"offer.1","expected_version":0,"data":{"supplier":{"party_id":"supplier","row_version":2},"rfq_line_ids":["L1"]}}`)
	if a.status != 200 || sup.offer.OfferID != "offer.1" || sup.offer.Data.Supplier.PartyID != "supplier" || sup.offer.Data.RfqLineIDs[0] != "L1" {
		t.Fatalf("%d %v %+v", a.status, a.body, sup.offer)
	}
	if a = post("sup", supapi.Contract, "supplier_offer.verify", `{"id":"offer.1","working_revision":1}`); a.status != 422 ||
		a.body["field_errors"].([]any)[0].(map[string]any)["path"] != "/args/expected_version" {
		t.Fatalf("missing expected_version: %d %v", a.status, a.body)
	}
	a = post("cst", cstapi.Contract, "costing_context.save_draft",
		`{"id":"ctx.1","expected_version":0,"data":{"rfq_ref":{"id":"rfq.1","row_version":3},"context_kind":"actual",`+
			`"assumptions":[{"id":"a1","path":"/context/lines/0","value_text":"x","reason":"r"}]}}`)
	if a.status != 200 || cst.context.ContextID != "ctx.1" || cst.context.Data.RfqRef.RfqID != "rfq.1" ||
		cst.context.Data.RfqRef.RowVersion != 3 || cst.context.Data.Assumptions[0].AssumptionID != "a1" {
		t.Fatalf("%d %v %+v", a.status, a.body, cst.context)
	}
	a = post("cst", cstapi.Contract, "measurement.record_verified",
		`{"id":"m.1","expected_version":0,"data":{"kind":"measured","packages":[{"id":"p1","count":2,"length_m":"0.5","line_ids":["L1"]}]}}`)
	if a.status != 200 || cst.measurement.MeasurementID != "m.1" || cst.measurement.Data.Packages[0].PackageID != "p1" ||
		cst.measurement.Data.Packages[0].Count != 2 {
		t.Fatalf("%d %v %+v", a.status, a.body, cst.measurement)
	}
	if a = post("cst", cstapi.Contract, "costing_context.save_draft", `{"id":"ctx.1","expected_version":0,"data":{"rfq_ref":{"rfq_id":"x"}}}`); a.status != 422 {
		t.Fatalf("Go field name accepted: %d %v", a.status, a.body)
	}
}
