package shipmentdocumentset

import "go.idp.xyz/idp-brant/internal/kernel"

// The certificate of origin record's reference to CNX (S5 C5; FUL-INV-004 "原产地证引用逐版本保存";
// CNX-INV-007; MD-40; Context Map R56 CertificateResultRef v1). A record that names a certificate result
// (certificate_ref) is prepared only when CNX holds that revision as current and it applies to the
// shipment now: a certificate of origin, for the shipment's destination market, valid at the instant
// of the prepare, covering every shipment line's SKU specification. Otherwise the prepare is refused —
// not shown as a difference for issue to acknowledge: CNX-INV-007 answers the reference of a certificate
// that does not apply with CERTIFICATE_NOT_APPLICABLE and MD-40 forbids reusing a certificate of another
// specification or market, which an acknowledged difference would allow. A record without
// certificate_ref stays a record of the external document as stated (S5 C2).

// CodeCertificateNotApplicable (409): the certificate's object, market or validity is not the
// shipment's (CNX's code, which FUL passes on; error catalog contexts CNX／FUL).
const CodeCertificateNotApplicable = "CERTIFICATE_NOT_APPLICABLE"

// CertificateSource is the name of a certificate of origin version's source entry that records what
// CNX's CertificateResultRef v1 answered.
const CertificateSource = "cnx.certificate_result_ref.v1"

// CertificateResult is what CNX's CertificateResultRef v1 answered, before FUL's transaction, for the
// certificate a certificate of origin record names: Code "" (applicable), REFERENCE_INVALID,
// REFERENCE_STALE or CERTIFICATE_NOT_APPLICABLE with Reason; the certificate's Number; Binding, the
// value the version records (certificate, revision, content SHA-256, reason, market, validity end).
type CertificateResult struct {
	Code    string
	Reason  string
	Number  string
	Binding string
}

// certificateRef decides the reference of certificate of origin record c on what CNX answered (r; nil
// when it was not asked), in this order: no answer or an unknown certificate (REFERENCE_INVALID), a
// superseded revision (REFERENCE_STALE), a certificate that does not apply to the shipment
// (CERTIFICATE_NOT_APPLICABLE), all at /args/certificate/certificate_ref; another number than the
// certificate's (INVALID_KNOWN_VALUE at /args/certificate/number). It records the answer in d's sources.
func certificateRef(d *Document, c *Certificate, r *CertificateResult) error {
	if c == nil || c.CertificateID == "" {
		return nil
	}
	const at = "/args/certificate/certificate_ref"
	switch {
	case r == nil || r.Code == kernel.CodeReferenceInvalid:
		reason := "CNX holds no such certificate result"
		if r != nil {
			reason += " (" + r.Reason + ")"
		}
		return Reject(kernel.CodeReferenceInvalid, at, reason)
	case r.Code == CodeReferenceStale:
		return Reject(CodeReferenceStale, at, "A correction superseded the certificate's revision")
	case r.Code != "":
		return Reject(CodeCertificateNotApplicable, at, "The certificate does not apply to the shipment: "+r.Reason)
	case r.Number != c.Number:
		return Reject(kernel.CodeInvalidKnownValue, "/args/certificate/number", "The certificate's number is "+r.Number)
	}
	d.Sources = append(d.Sources, Entry{Name: CertificateSource, Value: r.Binding})
	return nil
}
