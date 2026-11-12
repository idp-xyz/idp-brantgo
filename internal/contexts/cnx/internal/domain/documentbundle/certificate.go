package documentbundle

import (
	"slices"
	"strconv"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Official certificates in a bundle (S5 C5; CNX-INV-007, CNX-INV-008, CN-AT14 "已上传文件不直接满足要求
// （CERTIFICATE_NOT_APPLICABLE）"): a certificate_of_origin, inspection_certificate or export_license
// requirement may name certificate results CNX registered (certificate.register: an authorized human
// verified the original). The application judges each named certificate on the export lot before the
// check (CertificateResultRef v1's rules: kind, market, validity, goods) and refuses one that applies to
// none of the lot's lines; the check records what each certificate's revision states and which lot
// lines it covers.
//
// Verified does not follow from a certificate: a requirement's status remains what the checker, a
// human, states. But a certificate_of_origin or inspection_certificate requirement is verified only on
// registered certificates (an attachment and a basis are not an official result:
// DOCUMENT_REQUIREMENT_UNVERIFIED), and only when they cover every line of the lot together
// (CERTIFICATE_NOT_APPLICABLE otherwise). An export_license requirement rests on the licence CNX
// registers itself; a licence copy may be named, and is held to the same coverage when it is.

// CodeCertificateNotApplicable is the certificate domain's code, which a check gives for certificates
// that do not cover the lot (error catalog V0.1, 409).
const CodeCertificateNotApplicable = "CERTIFICATE_NOT_APPLICABLE"

// CertificateKind returns the certificate kind a requirement of kind names, "" when it names none.
func CertificateKind(kind string) string {
	switch kind {
	case KindCertificateOfOrigin:
		return "certificate_of_origin"
	case KindInspectionCertificate:
		return "inspection_certificate"
	case KindExportLicense:
		return "license_copy"
	}
	return ""
}

// CertificateRef is a certificate result a requirement names, as CNX held it at the check: the revision
// and its document's SHA-256, what it states, its state at the check and the lot lines it covers.
type CertificateRef struct {
	CertificateID  string   `json:"certificate_id"`
	Revision       int      `json:"revision"`
	DocumentSHA256 string   `json:"document_sha256"`
	Kind           string   `json:"kind"`
	Number         string   `json:"number"`
	Issuer         string   `json:"issuer"`
	Market         string   `json:"market"`
	ValidFrom      string   `json:"valid_from"`
	ValidUntil     string   `json:"valid_until"`
	State          string   `json:"state"`
	Covered        []string `json:"covered"`
}

// checkCertificates decides requirement r at index i of a check of a lot with lines (CN-AT14): a
// verified certificate_of_origin or inspection_certificate requirement without certificates
// (DOCUMENT_REQUIREMENT_UNVERIFIED), a verified requirement whose certificates leave a lot line
// uncovered (CERTIFICATE_NOT_APPLICABLE).
func checkCertificates(r Requirement, i int, lines []string) error {
	path := "/args/requirements/" + strconv.Itoa(i)
	if r.Status != Verified {
		return nil
	}
	if (r.Kind == KindCertificateOfOrigin || r.Kind == KindInspectionCertificate) && len(r.Certificates) == 0 {
		return reject(CodeDocumentRequirementUnverified, path+"/status", "A "+r.Kind+" is verified on a registered certificate result, not on an attachment")
	}
	if len(r.Certificates) == 0 {
		return nil
	}
	for _, l := range lines {
		if !slices.ContainsFunc(r.Certificates, func(c CertificateRef) bool { return slices.Contains(c.Covered, l) }) {
			return kernel.RejectAs(CodeCertificateNotApplicable, kernel.CategoryConflict, path+"/certificate_ids",
				"No certificate named covers lot line "+l)
		}
	}
	return nil
}
