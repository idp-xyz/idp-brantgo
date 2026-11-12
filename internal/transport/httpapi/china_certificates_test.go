package httpapi

import (
	"context"
	"net/http"
	"testing"

	"go.idp.xyz/idp-bento-go/application"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// fakeCertificates records the certificate commands and reads it receives on top of fakeCNX.
type fakeCertificates struct {
	fakeCNX
	registered []cnxapi.RegisterCertificate
	targets    []cnxapi.ExportTarget
}

func (f *fakeCertificates) RegisterCertificate(_ context.Context, cmd cnxapi.RegisterCertificate) (cnxapi.CertificateReceipt, error) {
	f.registered = append(f.registered, cmd)
	if cmd.CertificateID != "" {
		return cnxapi.CertificateReceipt{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return cnxapi.CertificateReceipt{Receipt: cnxapi.Receipt{ActionID: "act-1", ObjectID: "crt-1", RowVersion: 1, Status: "registered"}}, nil
}

func (f *fakeCertificates) Certificates(context.Context) ([]cnxapi.CertificateSummary, error) {
	return nil, nil
}

func (f *fakeCertificates) CertificateApplicability(_ context.Context, id string, target cnxapi.ExportTarget) (cnxapi.CertificateApplicabilityView, error) {
	f.targets = append(f.targets, target)
	return cnxapi.CertificateApplicabilityView{CertificateResult: cnxapi.CertificateResult{CertificateID: id,
		Code: cnxapi.CodeCertificateNotApplicable, Reason: cnxapi.CertificateMarket, Covered: []string{"S1"}, Uncovered: []string{}}}, nil
}

// certificate.register decodes by the reflective mapping — the object with its SKU specifications,
// the market, the validity, file, basis, evidence and a correction's reason — and keeps the catalog's
// categories; the list answers empty and the applicability read takes exactly target_kind and
// target_id (S5 C5).
func TestCNXCertificateCommandDecodesItsMembers(t *testing.T) {
	cnx := &fakeCertificates{}
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{}, POL: struct{ POL }{}, REF: struct{ REF }{},
		SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: struct{ TRD }{}, PUR: struct{ PUR }{}, CNX: cnx, Verifier: tokens{"Bearer good": alice},
		Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	post := func(args string) answer {
		return call(t, h, http.MethodPost, "/api/brant/v1/cnx/commands/certificate.register", "Bearer good",
			`{"contract":"`+cnxapi.Contract+`","command":"certificate.register","idempotency_key":"k1","args":`+args+`}`)
	}
	body := `"certificate_kind":"certificate_of_origin","number":"CO-1","issuer":"合成贸促会","issued_on":"2026-12-01",` +
		`"object":{"kind":"sku_spec","sku_specs":[{"sku_id":"sku.1","spec_revision":2}]},"market":"DE","valid_from":"2026-12-01T00:00:00Z",` +
		`"valid_until":"2027-12-01T00:00:00Z","file_id":"file.co","basis":"核验","evidence_ids":["e1"]`
	a := post(`{"expected_version":0,` + body + `}`)
	if r := cnx.registered; a.status != http.StatusOK || a.body["object_id"] != "crt-1" || len(r) != 1 || r[0].Object.SkuSpecs[0].SpecRevision != 2 ||
		r[0].Market != "DE" || r[0].EvidenceIDs[0] != "e1" || r[0].Basis != "核验" {
		t.Fatalf("%d %v %+v", a.status, a.body, r)
	}
	a = post(`{"certificate_id":"crt-1","expected_version":1,"reason":"更正",` + body + `}`)
	if r := cnx.registered; a.status != http.StatusConflict || a.body["code"] != kernel.CodeVersionConflict || r[1].Reason != "更正" || r[1].ExpectedVersion != 1 {
		t.Fatalf("%d %v", a.status, a.body)
	}
	a = post(`{"surprise":1,` + body + `}`)
	if a.status != http.StatusUnprocessableEntity || a.body["code"] != kernel.CodeSchemaInvalid || len(cnx.registered) != 2 {
		t.Fatalf("unknown member: %d %v", a.status, a.body)
	}
	a = call(t, h, http.MethodGet, "/api/brant/v1/cnx/certificates", "Bearer good", "")
	if list, ok := a.body["certificates"].([]any); a.status != http.StatusOK || !ok || len(list) != 0 {
		t.Fatalf("list %d %v", a.status, a.body)
	}
	a = call(t, h, http.MethodGet, "/api/brant/v1/cnx/certificates/crt-1/applicability?target_kind=shipment&target_id=shipment.1", "Bearer good", "")
	if a.status != http.StatusOK || a.body["code"] != cnxapi.CodeCertificateNotApplicable || a.body["reason"] != "market" ||
		cnx.targets[0] != (cnxapi.ExportTarget{Kind: "shipment", ID: "shipment.1"}) {
		t.Fatalf("applicability %d %v", a.status, a.body)
	}
	a = call(t, h, http.MethodGet, "/api/brant/v1/cnx/certificates/crt-1/applicability?target_kind=shipment&target_id=s&x=1", "Bearer good", "")
	if a.status != http.StatusUnprocessableEntity || a.body["code"] != kernel.CodeSchemaInvalid || len(cnx.targets) != 1 {
		t.Fatalf("an unknown parameter %d %v", a.status, a.body)
	}
}
