package documentbundle

import (
	"strings"
	"testing"
)

// CN-AT14 certificate half (已上传文件不直接满足要求（CERTIFICATE_NOT_APPLICABLE）), CNX-INV-007/008, S5 C5:
// a certificate of origin or inspection certificate requirement stated verified on an attachment alone
// is DOCUMENT_REQUIREMENT_UNVERIFIED; on certificate results that leave a lot line uncovered it is
// CERTIFICATE_NOT_APPLICABLE; certificates that cover every line together let a human state it
// verified. A present requirement may name a certificate covering part of the lot; an export_license
// requirement rests on CNX's licence and needs no licence copy. Checks without certificates are sealed
// as before (no "certificates" member).
func TestCertificateRequirementsRestOnApplicableCertificates(t *testing.T) {
	owner := Owner{Kind: "actor", ID: "doc.1"}
	co := func(status string, certs ...CertificateRef) Requirement {
		r := Requirement{RequirementID: "co", Kind: KindCertificateOfOrigin, Version: "1", Status: status, Owner: owner,
			Evidence: []Evidence{{SourceID: "co.scan"}}, Certificates: certs}
		if status == Verified {
			r.Basis = "原产地证结果已登记"
		} else {
			r.DueOn = "2026-12-10"
		}
		return r
	}
	e1 := CertificateRef{CertificateID: "crt.1", Revision: 1, Covered: []string{"E1"}}
	e2 := CertificateRef{CertificateID: "crt.2", Revision: 1, Covered: []string{"E2"}}
	lines := []string{"E1", "E2"}
	for _, x := range []struct {
		name string
		r    Requirement
		want string
	}{
		{"verified on an attachment", co(Verified), "DOCUMENT_REQUIREMENT_UNVERIFIED /args/requirements/0/status"},
		{"inspection on an attachment", func() Requirement { r := co(Verified); r.Kind = KindInspectionCertificate; return r }(),
			"DOCUMENT_REQUIREMENT_UNVERIFIED /args/requirements/0/status"},
		{"one line uncovered", co(Verified, e1), "CERTIFICATE_NOT_APPLICABLE /args/requirements/0/certificate_ids"},
		{"covered together", co(Verified, e1, e2), "OK"},
		{"present on part of the lot", co(Present, e1), "OK"},
		{"export licence without a copy", func() Requirement { r := co(Verified); r.Kind = KindExportLicense; return r }(), "OK"},
		{"export licence copy on part of the lot", func() Requirement { r := co(Verified, e2); r.Kind = KindExportLicense; return r }(),
			"CERTIFICATE_NOT_APPLICABLE /args/requirements/0/certificate_ids"},
	} {
		_, err := Check(CheckInput{Requirements: []Requirement{x.r}, Human: true, Lines: lines})
		if verdict(err) != x.want {
			t.Errorf("%s: %s", x.name, verdict(err))
		}
	}
	if CertificateKind(KindExportLicense) != "license_copy" || CertificateKind(KindCertificateOfOrigin) != "certificate_of_origin" ||
		CertificateKind(KindPackingList) != "" {
		t.Fatal("certificate kinds")
	}
	plain, err := Seal(CheckDocument{BundleID: "b", CheckNo: 1, Requirements: checklist()})
	if err != nil || strings.Contains(string(plain.Bytes), "certificates") {
		t.Fatalf("a check without certificates changed: %s %v", plain.Bytes, err)
	}
	named, err := Seal(CheckDocument{BundleID: "b", CheckNo: 1, Requirements: []Requirement{co(Verified, e1, e2)}})
	if err != nil || !strings.Contains(string(named.Bytes), `"certificates":[{"certificate_id":"crt.1"`) {
		t.Fatalf("certificates not sealed: %s %v", named.Bytes, err)
	}
}
