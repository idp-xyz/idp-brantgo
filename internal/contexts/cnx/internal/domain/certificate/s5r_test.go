package certificate

import (
	"testing"
	"time"
)

// S5R-D12 (CNX-INV-007, CNX-AGG-CertificateResult AgreementCode): a preferential certificate of origin — an RCEP
// certificate customs issued, or an approved exporter's origin declaration — registers as its own kind with its
// agreement, form and issuing body (members omitted for every other kind: earlier documents keep their hashes), and
// serves where a certificate of origin is needed; nothing serves where a preferential one is needed but one.
func TestS5RPreferentialCertificateOfOriginD12(t *testing.T) {
	plain, err := Seal(Document{CertificateID: "crt.1", Revision: 1, Content: origin(), VerifiedBy: Actor{Kind: "human", ID: "doc.1"}, AuthorityEpoch: 1,
		RegisteredAt: "2026-11-12T08:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"agreement_code", "origin_form", "issuing_body"} {
		if contains(string(plain.Bytes), f) {
			t.Fatalf("a certificate of origin's document names no %s: %s", f, plain.Bytes)
		}
	}
	pref := origin()
	pref.Kind, pref.AgreementCode, pref.OriginForm, pref.IssuingBody = KindPreferentialOrigin, "RCEP", FormCertificate, BodyCustoms
	rev, err := Seal(Document{CertificateID: "crt.2", Revision: 1, Content: pref, VerifiedBy: Actor{Kind: "human", ID: "doc.1"}, AuthorityEpoch: 1,
		RegisteredAt: "2026-11-12T08:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	back, ok, err := Unseal(rev.Bytes, rev.SHA256)
	if err != nil || !ok || back.AgreementCode != "RCEP" || back.OriginForm != FormCertificate || back.IssuingBody != BodyCustoms {
		t.Fatalf("%+v %v %v", back, ok, err)
	}
	if !Serves(KindPreferentialOrigin, KindOrigin) || !Serves(KindOrigin, KindOrigin) || Serves(KindOrigin, KindPreferentialOrigin) ||
		Serves(KindPreferentialOrigin, KindInspection) || !Origin(KindPreferentialOrigin) || !Origin(KindOrigin) || Origin(KindLicenseCopy) {
		t.Fatal("a preferential certificate of origin is a certificate of origin, not the other way round")
	}
	at := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	target := Target{Kind: ObjectShipment, ID: "ship.1", Destination: rev.Market, Lines: []Line{{ShipmentID: "ship.1", LineID: "S1", SkuID: "sku.1", SpecRevision: 1}}}
	if v := Judge(rev, 1, Use{Kind: KindOrigin, Target: target, At: at}); v.Reason == ReasonKind {
		t.Fatalf("a preferential certificate serves a certificate of origin use: %+v", v)
	}
	if v := Judge(plain, 1, Use{Kind: KindPreferentialOrigin, Target: target, At: at}); v.Reason != ReasonKind {
		t.Fatalf("a plain certificate of origin is no preferential one: %+v", v)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
