package httpapi

import (
	"context"
	"net/http"
	"testing"

	"go.idp.xyz/idp-bento-go/application"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
)

// fakeLicenses records the licence commands it receives on top of fakeCNX.
type fakeLicenses struct {
	fakeCNX
	registered []cnxapi.RegisterLicense
	allocated  []cnxapi.AllocateLicense
	released   []cnxapi.ReleaseLicense
}

func (f *fakeLicenses) RegisterLicense(_ context.Context, cmd cnxapi.RegisterLicense) (cnxapi.LicenseReceipt, error) {
	f.registered = append(f.registered, cmd)
	return cnxapi.LicenseReceipt{Receipt: cnxapi.Receipt{ActionID: "act-1", ObjectID: "elc-1", RowVersion: 1, Status: "registered"}}, nil
}

func (f *fakeLicenses) AllocateLicense(_ context.Context, cmd cnxapi.AllocateLicense) (cnxapi.LicenseReceipt, error) {
	f.allocated = append(f.allocated, cmd)
	return cnxapi.LicenseReceipt{}, cnxapi.Reject(cnxapi.CodeExportLicenseExceeded, "/args/target/id", "")
}

func (f *fakeLicenses) ReleaseLicense(_ context.Context, cmd cnxapi.ReleaseLicense) (cnxapi.LicenseReceipt, error) {
	f.released = append(f.released, cmd)
	return cnxapi.LicenseReceipt{Receipt: cnxapi.Receipt{ActionID: "act-2", ObjectID: "elc-1", RowVersion: 3, Status: "registered"}}, nil
}

func (f *fakeLicenses) Licenses(context.Context) ([]cnxapi.LicenseSummary, error) { return nil, nil }

// The licence commands decode by the reflective mapping: nested goods, currency, end user, target
// and external result; allocate has no expected_version; a release without an external result
// never reaches CNX's validation as anything but absent.
func TestCNXLicenseCommandsDecodeTheirMembers(t *testing.T) {
	cnx := &fakeLicenses{}
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{}, POL: struct{ POL }{}, REF: struct{ REF }{},
		SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: struct{ TRD }{}, PUR: struct{ PUR }{}, CNX: cnx, Verifier: tokens{"Bearer good": alice},
		Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	envelope := func(command, args string) string {
		return `{"contract":"` + cnxapi.Contract + `","command":"` + command + `","idempotency_key":"k1","args":` + args + `}`
	}
	a := call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/export_license.register", "Bearer good", envelope("export_license.register",
		`{"license_number":"SYN-1","issuer":"合成","holder":"合成","goods":{"classification":{"code_table_version":"CN-2026","commodity_codes":["6109100021"]}},`+
			`"licensed_quantity":"1000","unit":"PCS","currency":{"id":"USD","revision":1},"licensed_value":"10000.00","destination":"DE",`+
			`"end_user":{"party_id":"customer.1","name":"合成"},"valid_from":"2026-11-01T00:00:00Z","valid_until":"2027-05-01T00:00:00Z","file_id":"f1"}`))
	if a.status != http.StatusOK || a.body["object_id"] != "elc-1" || len(cnx.registered) != 1 {
		t.Fatalf("%d %v", a.status, a.body)
	}
	r := cnx.registered[0]
	if r.Goods.Classification == nil || r.Goods.Classification.CommodityCodes[0] != "6109100021" || r.Currency.ID != "USD" || r.Currency.Revision != 1 ||
		r.EndUser.PartyID != "customer.1" || r.LicensedValue != "10000.00" || r.IdempotencyKey != "k1" {
		t.Fatalf("%+v", r)
	}

	a = call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/export_license.allocate", "Bearer good", envelope("export_license.allocate",
		`{"license_id":"elc-1","allocation_id":"a1","target":{"kind":"shipment","id":"s1"}}`))
	if a.status != http.StatusConflict || a.body["code"] != cnxapi.CodeExportLicenseExceeded || cnx.allocated[0].Target.ID != "s1" {
		t.Fatalf("%d %v", a.status, a.body)
	}

	a = call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/export_license.release", "Bearer good", envelope("export_license.release",
		`{"license_id":"elc-1","expected_version":2,"allocation_id":"a1","release_id":"r1","external_result":{"kind":"withdrawn",`+
			`"declaration_number":"D1","receipt_text":"撤销回执","received_at":"2026-11-12T00:00:00Z","evidence_ids":["e1"]},"reason":"撤销"}`))
	if a.status != http.StatusOK || cnx.released[0].ExternalResult == nil || cnx.released[0].ExternalResult.Kind != "withdrawn" ||
		cnx.released[0].ExpectedVersion != 2 {
		t.Fatalf("%d %v %+v", a.status, a.body, cnx.released)
	}
	a = call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/export_license.release", "Bearer good", envelope("export_license.release",
		`{"license_id":"elc-1","expected_version":2,"allocation_id":"a1","release_id":"r2","reason":"本地取消"}`))
	if a.status != http.StatusOK || cnx.released[1].ExternalResult != nil {
		t.Fatalf("absent external_result decodes as absent: %d %v", a.status, a.body)
	}

	a = call(t, h, http.MethodGet, "/api/brant/v1/cnx/export_licenses", "Bearer good", "")
	if list, ok := a.body["licenses"].([]any); a.status != http.StatusOK || !ok || len(list) != 0 {
		t.Fatalf("%d %v", a.status, a.body)
	}
}
