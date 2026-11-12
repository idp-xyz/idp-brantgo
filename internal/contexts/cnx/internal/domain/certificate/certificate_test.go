package certificate

import (
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	t0    = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	human = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "doc.1"}
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func path(err error) string {
	if r, ok := kernel.AsRejection(err); ok && len(r.Fields) > 0 {
		return r.Fields[0].Path
	}
	return ""
}

// origin is a certificate of origin of shipment.1's two lines (sku.1 r1, sku.2 r1) for DE, valid
// for 30 days from t0.
func origin() Content {
	return Content{Kind: KindOrigin, Number: "CO-2026-001", Issuer: "合成贸促会（测试）", IssuedOn: "2026-12-01",
		Object: Object{Kind: ObjectShipment, ID: "shipment.1", Version: 2, OrderID: "order.1", OrderRevision: 1, Lines: []Line{
			{ShipmentID: "shipment.1", LineID: "S1", SkuID: "sku.1", SpecRevision: 1, Quantity: "60", Unit: "PCS"},
			{ShipmentID: "shipment.1", LineID: "S2", SkuID: "sku.2", SpecRevision: 1, Quantity: "60", Unit: "PCS"}}},
		Market: "DE", ValidFrom: kernel.FormatTimestamp(t0), ValidUntil: kernel.FormatTimestamp(t0.Add(30 * 24 * time.Hour)),
		File: Evidence{SourceID: "file.co", ContentSHA256: repeated('a')}, Basis: "原件与签发机构核验平台一致（合成）", Evidence: []Evidence{}}
}

func repeated(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

// CNX-INV-007, tactical "只追加；更正形成新版本": a registration is revision 1 of a new certificate; a
// correction is the next revision with its reason, superseding the last, the root moving by one row
// version to the corrected key; the revision registered first is not rewritten. A correction of
// another row version, another kind, a certificate the company lacks or stating nothing new is
// refused; only a human registers (CN-R14 核实人).
func TestRegistrationsAndCorrectionsKeepHistory(t *testing.T) {
	c, r1, err := Register(nil, nil, "crt.1", 0, Registration{Content: origin(), By: human, Epoch: 3, At: t0})
	if err != nil || c.CurrentRevision != 1 || c.RowVersion != 1 || r1.Revision != 1 || r1.Supersedes != 0 || r1.VerifiedBy.ID != "doc.1" ||
		r1.Reason != "" || r1.CertificateID != "crt.1" || r1.Contract != DocumentContract {
		t.Fatalf("registered %+v %+v %v", c, r1, err)
	}
	if _, _, err = Register(nil, nil, "crt.x", 0, Registration{Content: origin(), By: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent"}, At: t0}); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("an AI agent registered: %v", err)
	}
	fixed := origin()
	fixed.Number, fixed.Market = "CO-2026-001A", "FR"
	for _, x := range []struct {
		root     *Certificate
		expected int64
		content  Content
		want     string
		path     string
	}{
		{nil, 1, fixed, kernel.CodeNotFound, "/args/certificate_id"},
		{c, 2, fixed, kernel.CodeVersionConflict, "/args/expected_version"},
		{c, 1, func() Content { k := fixed; k.Kind = KindInspection; return k }(), kernel.CodeInvalidKnownValue, "/args/certificate_kind"},
		{c, 1, origin(), kernel.CodeInvalidKnownValue, "/args/certificate_id"},
	} {
		_, _, err := Register(x.root, r1, "crt.1", x.expected, Registration{Content: x.content, Reason: "更正", By: human, At: t0})
		if code(err) != x.want || path(err) != x.path {
			t.Errorf("want %s %s, got %v", x.want, x.path, err)
		}
	}
	c2, r2, err := Register(c, r1, "crt.1", 1, Registration{Content: fixed, Reason: "签发机构更正证书号与进口国", By: human, Epoch: 4, At: t0.Add(time.Hour)})
	if err != nil || c2.CurrentRevision != 2 || c2.RowVersion != 2 || c2.Number != "CO-2026-001A" || c2.RegisteredAt != c.RegisteredAt ||
		r2.Revision != 2 || r2.Supersedes != 1 || r2.Reason == "" || r2.Market != "FR" {
		t.Fatalf("corrected %+v %+v %v", c2, r2, err)
	}
	if r1.Market != "DE" || r1.Number != "CO-2026-001" || r1.SHA256 == r2.SHA256 {
		t.Fatal("revision 1 was rewritten")
	}
}

// A revision is sealed as canonical JSON with its SHA-256 and read back only when intact.
func TestSealedRevisionsAreReadBackIntact(t *testing.T) {
	_, r, err := Register(nil, nil, "crt.1", 0, Registration{Content: origin(), By: human, Epoch: 1, At: t0})
	if err != nil {
		t.Fatal(err)
	}
	back, ok, err := Unseal(r.Bytes, r.SHA256)
	if err != nil || !ok || back.Number != r.Number || !back.Validity.From.Equal(t0) || len(back.Object.Lines) != 2 {
		t.Fatalf("unsealed %+v %v %v", back, ok, err)
	}
	tampered := slices.Clone(r.Bytes)
	tampered[len(tampered)-2] = ' '
	if _, ok, _ := Unseal(tampered, r.SHA256); ok {
		t.Fatal("a tampered revision read back")
	}
	bad := origin()
	bad.ValidUntil = bad.ValidFrom
	if _, _, err := Register(nil, nil, "crt.2", 0, Registration{Content: bad, By: human, At: t0}); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("an empty validity: %v", err)
	}
}

// CNX-INV-007, MD-40, CN-AT14 certificate half: a certificate applies to a shipment or export lot only
// when it is of the kind the use needs, for its destination market, valid at the instant, and covers
// every line; another specification, shipment, market or an expired certificate is
// CERTIFICATE_NOT_APPLICABLE with the reason; a superseded revision is REFERENCE_STALE.
func TestApplicabilityCNAT14(t *testing.T) {
	_, byShipment, _ := Register(nil, nil, "crt.1", 0, Registration{Content: origin(), By: human, At: t0})
	spec := origin()
	spec.Object = Object{Kind: ObjectSkuSpec, Specs: []Spec{{SkuID: "sku.1", SpecRevision: 1}, {SkuID: "sku.2", SpecRevision: 1}}}
	_, bySpec, _ := Register(nil, nil, "crt.2", 0, Registration{Content: spec, By: human, At: t0})
	lot := origin()
	lot.Object = Object{Kind: ObjectExportLot, ID: "lot.1", Lines: []Line{{ShipmentID: "shipment.1", LineID: "E1", SkuID: "sku.1", SpecRevision: 1}}}
	_, byLot, _ := Register(nil, nil, "crt.3", 0, Registration{Content: lot, By: human, At: t0})
	shipment := Target{Kind: ObjectShipment, ID: "shipment.1", Destination: "DE", Lines: []Line{{ShipmentID: "shipment.1", LineID: "S1", SkuID: "sku.1",
		SpecRevision: 1}, {ShipmentID: "shipment.1", LineID: "S2", SkuID: "sku.2", SpecRevision: 1}}}
	at := t0.Add(time.Hour)
	with := func(f func(*Target)) Target {
		x := shipment
		x.Lines = slices.Clone(shipment.Lines)
		f(&x)
		return x
	}
	for _, x := range []struct {
		name    string
		r       *Revision
		current int
		kind    string
		target  Target
		at      time.Time
		code    string
		reason  string
		covered int
	}{
		{"shipment certificate", byShipment, 1, KindOrigin, shipment, at, "", ReasonApplicable, 2},
		{"specification certificate", bySpec, 1, KindOrigin, shipment, at, "", ReasonApplicable, 2},
		{"another kind", byShipment, 1, KindInspection, shipment, at, CodeCertificateNotApplicable, ReasonKind, 2},
		{"destination unknown", byShipment, 1, KindOrigin, with(func(t *Target) { t.Destination = "" }), at, CodeCertificateNotApplicable,
			ReasonMarketUnknown, 2},
		{"another market", byShipment, 1, KindOrigin, with(func(t *Target) { t.Destination = "US" }), at, CodeCertificateNotApplicable, ReasonMarket, 2},
		{"before its validity", byShipment, 1, KindOrigin, shipment, t0.Add(-time.Second), CodeCertificateNotApplicable, ReasonNotYetValid, 2},
		{"at its end", byShipment, 1, KindOrigin, shipment, t0.Add(30 * 24 * time.Hour), CodeCertificateNotApplicable, ReasonExpired, 2},
		{"another specification revision", bySpec, 1, KindOrigin, with(func(t *Target) { t.Lines[1].SpecRevision = 2 }), at,
			CodeCertificateNotApplicable, ReasonGoods, 1},
		{"another shipment", byShipment, 1, KindOrigin, with(func(t *Target) {
			for i := range t.Lines {
				t.Lines[i].ShipmentID = "shipment.2"
			}
		}), at, CodeCertificateNotApplicable, ReasonGoods, 0},
		{"a lot certificate on part of the shipment", byLot, 1, KindOrigin, shipment, at, CodeCertificateNotApplicable, ReasonGoods, 1},
		{"no lines", byShipment, 1, KindOrigin, with(func(t *Target) { t.Lines = nil }), at, CodeCertificateNotApplicable, ReasonGoods, 0},
		{"superseded", byShipment, 2, KindOrigin, shipment, at, CodeReferenceStale, ReasonApplicable, 2},
	} {
		v := Judge(x.r, x.current, Use{Kind: x.kind, Target: x.target, At: x.at})
		if v.Code() != x.code || v.Reason != x.reason || len(v.Covered) != x.covered || v.Applicable != (x.reason == ReasonApplicable) {
			t.Errorf("%s: %+v (code %q)", x.name, v, v.Code())
		}
		if x.code == CodeCertificateNotApplicable && Explain(x.r, Use{Kind: x.kind, Target: x.target, At: x.at}, v) == "" {
			t.Errorf("%s: no explanation", x.name)
		}
	}
	if r := NotApplicable("/x", "y"); r.Code != CodeCertificateNotApplicable || r.Category != kernel.CategoryConflict {
		t.Fatal(r)
	}
}
