package documentbundle

import "testing"

// S5R-D12 (CNX-INV-008 "一份报关单对应一份原产地证书或原产地声明"): a check names at most one certificate of origin or
// preferential certificate (or origin declaration) for the lot its declaration declares — two covering the lot
// together, in one requirement or two, are INVALID_KNOWN_VALUE at the second; the same certificate named twice, and
// other kinds beside it, are not counted.
func TestS5ROneOriginCertificatePerDeclarationD12(t *testing.T) {
	owner := Owner{Kind: "actor", ID: "doc.1"}
	req := func(id, kind string, certs ...CertificateRef) Requirement {
		return Requirement{RequirementID: id, Kind: kind, Version: "1", Status: Verified, Owner: owner, Evidence: []Evidence{{SourceID: id + ".scan"}},
			Basis: "结果已登记", Certificates: certs}
	}
	co1 := CertificateRef{CertificateID: "crt.1", Kind: "certificate_of_origin", Covered: []string{"E1"}}
	co2 := CertificateRef{CertificateID: "crt.2", Kind: "certificate_of_origin", Covered: []string{"E2"}}
	rcep := CertificateRef{CertificateID: "crt.3", Kind: "preferential_certificate_of_origin", Covered: []string{"E1", "E2"}}
	insp := CertificateRef{CertificateID: "crt.4", Kind: "inspection_certificate", Covered: []string{"E1", "E2"}}
	lines := []string{"E1", "E2"}
	for _, x := range []struct {
		name string
		reqs []Requirement
		want string
	}{
		{"two certificates of origin together", []Requirement{req("co", KindCertificateOfOrigin, co1, co2)},
			"INVALID_KNOWN_VALUE /args/requirements/0/certificate_ids/1"},
		{"a certificate of origin and a preferential one", []Requirement{req("co", KindCertificateOfOrigin, rcep), req("co2", KindCertificateOfOrigin, co1)},
			"INVALID_KNOWN_VALUE /args/requirements/1/certificate_ids/0"},
		{"one preferential certificate covering the lot", []Requirement{req("co", KindCertificateOfOrigin, rcep), req("insp", KindInspectionCertificate, insp)},
			"OK"},
		{"the same certificate twice", []Requirement{req("co", KindCertificateOfOrigin, rcep), req("co2", KindCertificateOfOrigin, rcep)}, "OK"},
	} {
		if _, err := Check(CheckInput{Requirements: x.reqs, Human: true, Lines: lines}); verdict(err) != x.want {
			t.Errorf("%s: %s, want %s", x.name, verdict(err), x.want)
		}
	}
}
