package shipmentdocumentset

import (
	"strings"
	"testing"
)

// S5 C5 (FUL-INV-004 原产地证引用; CNX-INV-007; MD-40; R56 CertificateResultRef v1): a certificate of
// origin record that names a certificate result is prepared only when CNX holds the revision as current
// and it applies to the shipment, and its number is the certificate's; the version records CNX's answer
// among its sources. A certificate CNX does not hold (or no answer) is REFERENCE_INVALID, a superseded
// revision REFERENCE_STALE, an inapplicable one CERTIFICATE_NOT_APPLICABLE — refused, never a
// difference to acknowledge at issue. A record without a reference is recorded as stated.
func TestCertificateOfOriginRecordNamesAnApplicableCertificate(t *testing.T) {
	file := &File{ID: "file.co", SHA256: strings.Repeat("c", 64), MimeType: "application/pdf"}
	co := func(id string) Request {
		return Request{Kind: KindCertificateOfOrigin, FileID: "file.co", Certificate: &Certificate{Number: "CO-1", Issuer: "合成贸促会",
			IssuedOn: "2026-12-01", CertificateID: id, CertificateVersion: 1}}
	}
	applicable := &CertificateResult{Number: "CO-1", Reason: "applicable", Binding: "crt-1/v1/" + strings.Repeat("a", 64) + "/applicable/DE/x"}
	for _, x := range []struct {
		name string
		req  Request
		up   *CertificateResult
		want string
	}{
		{"no answer", co("crt-1"), nil, "REFERENCE_INVALID /args/certificate/certificate_ref"},
		{"unknown", co("crt-1"), &CertificateResult{Code: "REFERENCE_INVALID", Reason: "unknown"}, "REFERENCE_INVALID /args/certificate/certificate_ref"},
		{"superseded", co("crt-1"), &CertificateResult{Code: CodeReferenceStale, Reason: "superseded", Number: "CO-1"},
			"REFERENCE_STALE /args/certificate/certificate_ref"},
		{"another market", co("crt-1"), &CertificateResult{Code: CodeCertificateNotApplicable, Reason: "market", Number: "CO-1"},
			"CERTIFICATE_NOT_APPLICABLE /args/certificate/certificate_ref"},
		{"another number", co("crt-1"), &CertificateResult{Number: "CO-2", Reason: "applicable"}, "INVALID_KNOWN_VALUE /args/certificate/number"},
		{"applicable", co("crt-1"), applicable, "OK"},
		{"no reference", co(""), nil, "OK"},
	} {
		d, err := Prepare(NewSet("docs.1", fixtureShipment(), "", act()), x.req, fixtureFacts(), Upstream{File: file, Certificate: x.up}, act())
		if outcome(err) != x.want {
			t.Errorf("%s: %s", x.name, outcome(err))
			continue
		}
		if err != nil {
			continue
		}
		var bound string
		for _, s := range d.Sources {
			if s.Name == CertificateSource {
				bound = s.Value
			}
		}
		if (x.up != nil) != (bound != "") || (x.up != nil && bound != x.up.Binding) || d.Number != "CO-1" || d.Record.Certificate == nil {
			t.Errorf("%s: %+v", x.name, d.Sources)
		}
	}
	if Codes[CodeCertificateNotApplicable] != "conflict" {
		t.Fatal(Codes)
	}
}
