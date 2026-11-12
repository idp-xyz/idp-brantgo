package api

import (
	"strings"
	"testing"
	"time"
)

func validCertificate() RegisterCertificate {
	return RegisterCertificate{IdempotencyKey: "k", CertificateKind: CertificateOfOrigin, Number: "CO-2026-001", Issuer: "合成贸促会（测试）",
		IssuedOn: "2026-12-01", Object: CertificateObject{Kind: ObjectShipment, ID: "shipment.1"}, Market: "DE",
		ValidFrom: "2026-12-01T00:00:00Z", ValidUntil: "2027-12-01T00:00:00Z", FileID: "file.co", Basis: "原件与签发机构核验平台一致",
		EvidenceIDs: []string{"verify.page"}}
}

// The closed contract of certificate.register (CNX-AGG-CertificateResult, CNX-INV-007): a new
// certificate names no certificate_id, expected version 0 and no reason; a correction names the
// certificate, its row version and a reason; the object is SKU specification revisions or an FUL
// export lot or shipment; the market is an ISO country; the validity ends after it begins; only a
// licence copy names a licence; certificate_ids of a bundle requirement only for certificate kinds.
func TestCertificateContract(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  func(*RegisterCertificate)
		want string
	}{
		{"new", func(*RegisterCertificate) {}, "OK"},
		{"correction", func(c *RegisterCertificate) { c.CertificateID, c.ExpectedVersion, c.Reason = "crt-1", 1, "更正" }, "OK"},
		{"new with a version", func(c *RegisterCertificate) { c.ExpectedVersion = 1 }, "SCHEMA_INVALID /args/expected_version"},
		{"new with a reason", func(c *RegisterCertificate) { c.Reason = "x" }, "SCHEMA_INVALID /args/reason"},
		{"correction without a reason", func(c *RegisterCertificate) { c.CertificateID, c.ExpectedVersion = "crt-1", 1 }, "SCHEMA_INVALID /args/reason"},
		{"correction without a version", func(c *RegisterCertificate) { c.CertificateID, c.Reason = "crt-1", "x" }, "SCHEMA_INVALID /args/expected_version"},
		{"kind", func(c *RegisterCertificate) { c.CertificateKind = "origin" }, "SCHEMA_INVALID /args/certificate_kind"},
		{"number", func(c *RegisterCertificate) { c.Number = " " }, "SCHEMA_INVALID /args/number"},
		{"issuer", func(c *RegisterCertificate) { c.Issuer = strings.Repeat("x", 201) }, "SCHEMA_INVALID /args/issuer"},
		{"issued on", func(c *RegisterCertificate) { c.IssuedOn = "2026-13-01" }, "SCHEMA_INVALID /args/issued_on"},
		{"object kind", func(c *RegisterCertificate) { c.Object.Kind = "order" }, "SCHEMA_INVALID /args/object/kind"},
		{"shipment without ID", func(c *RegisterCertificate) { c.Object.ID = "" }, "SCHEMA_INVALID /args/object/id"},
		{"shipment with specs", func(c *RegisterCertificate) { c.Object.SkuSpecs = []SpecRef{{SkuID: "sku.1", SpecRevision: 1}} },
			"SCHEMA_INVALID /args/object/sku_specs"},
		{"specs", func(c *RegisterCertificate) {
			c.Object = CertificateObject{Kind: ObjectSkuSpec, SkuSpecs: []SpecRef{{SkuID: "sku.1", SpecRevision: 1}}}
		}, "OK"},
		{"specs repeated", func(c *RegisterCertificate) {
			c.Object = CertificateObject{Kind: ObjectSkuSpec, SkuSpecs: []SpecRef{{SkuID: "sku.1", SpecRevision: 1}, {SkuID: "sku.1", SpecRevision: 1}}}
		}, "SCHEMA_INVALID /args/object/sku_specs/1/spec_revision"},
		{"specs with an ID", func(c *RegisterCertificate) {
			c.Object = CertificateObject{Kind: ObjectSkuSpec, ID: "x", SkuSpecs: []SpecRef{{SkuID: "sku.1", SpecRevision: 1}}}
		}, "SCHEMA_INVALID /args/object/id"},
		{"no specs", func(c *RegisterCertificate) { c.Object = CertificateObject{Kind: ObjectSkuSpec} }, "SCHEMA_INVALID /args/object/sku_specs"},
		{"market", func(c *RegisterCertificate) { c.Market = "XX" }, "SCHEMA_INVALID /args/market"},
		{"validity", func(c *RegisterCertificate) { c.ValidUntil = c.ValidFrom }, "SCHEMA_INVALID /args/valid_until"},
		{"valid from", func(c *RegisterCertificate) { c.ValidFrom = "2026-12-01" }, "SCHEMA_INVALID /args/valid_from"},
		{"file", func(c *RegisterCertificate) { c.FileID = "" }, "SCHEMA_INVALID /args/file_id"},
		{"licence on a CO", func(c *RegisterCertificate) { c.LicenseID = "elc-1" }, "SCHEMA_INVALID /args/license_id"},
		{"licence copy", func(c *RegisterCertificate) { c.CertificateKind, c.LicenseID = CertificateLicenseCopy, "elc-1" }, "OK"},
		{"basis", func(c *RegisterCertificate) { c.Basis = "" }, "SCHEMA_INVALID /args/basis"},
		{"evidence", func(c *RegisterCertificate) { c.EvidenceIDs = []string{"a", "a"} }, "SCHEMA_INVALID /args/evidence_ids/1"},
	} {
		cmd := validCertificate()
		c.cmd(&cmd)
		if got := verdict(cmd.Validate()); got != c.want {
			t.Errorf("%s: %s", c.name, got)
		}
	}
	req := func(kind, status string, ids ...string) CheckBundle {
		r := Requirement{RequirementID: "r", Kind: kind, Version: "1", Status: status, Owner: RequirementOwner{Kind: OwnerActor, ID: "doc.1"},
			DueOn: "2026-12-10", CertificateIDs: ids}
		if status != RequirementMissing {
			r.EvidenceIDs = []string{"scan"}
		}
		return CheckBundle{IdempotencyKey: "k", ExportLotID: "lot.1", Requirements: []Requirement{r}}
	}
	for _, c := range []struct {
		cmd  CheckBundle
		want string
	}{
		{req(RequirementCertificateOfOrigin, RequirementPresent, "crt-1"), "OK"},
		{req(RequirementInspectionCertificate, RequirementPresent, "crt-1", "crt-2"), "OK"},
		{req(RequirementExportLicense, RequirementPresent, "crt-1"), "OK"},
		{req(RequirementPackingList, RequirementPresent, "crt-1"), "SCHEMA_INVALID /args/requirements/0/certificate_ids"},
		{req(RequirementCertificateOfOrigin, RequirementMissing, "crt-1"), "SCHEMA_INVALID /args/requirements/0/certificate_ids"},
		{req(RequirementCertificateOfOrigin, RequirementPresent, "crt-1", "crt-1"), "SCHEMA_INVALID /args/requirements/0/certificate_ids/1"},
	} {
		if got := verdict(c.cmd.Validate()); got != c.want {
			t.Errorf("%+v: %s", c.cmd.Requirements[0], got)
		}
	}
}

// CertificateResultRef v1 as a consumer uses it: uses are validated; Check passes only an applicable
// current revision and refuses with the code at the caller's path otherwise; Evidence binds the
// revision, its hash, the reason, the market and the validity's end.
func TestCertificateResultRef(t *testing.T) {
	at := time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)
	use := CertificateUse{Ref: CertificateRef{CertificateID: "crt-1", Version: 1}, Kind: CertificateOfOrigin,
		Target: ExportTarget{Kind: TargetShipment, ID: "shipment.1"}, At: at}
	if err := use.Validate("/uses/0"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mod  func(*CertificateUse)
		want string
	}{
		{func(u *CertificateUse) { u.Ref.CertificateID = "" }, "SCHEMA_INVALID /uses/0/ref/certificate_id"},
		{func(u *CertificateUse) { u.Ref.Version = -1 }, "SCHEMA_INVALID /uses/0/ref/version"},
		{func(u *CertificateUse) { u.Kind = "x" }, "SCHEMA_INVALID /uses/0/kind"},
		{func(u *CertificateUse) { u.Target.Kind = "order" }, "SCHEMA_INVALID /uses/0/target/kind"},
		{func(u *CertificateUse) { u.At = time.Time{} }, "SCHEMA_INVALID /uses/0/at"},
	} {
		u := use
		c.mod(&u)
		if got := verdict(u.Validate("/uses/0")); got != c.want {
			t.Errorf("%s", got)
		}
	}
	ok := CertificateResult{Use: use, Reason: CertificateApplicable, Applicable: true, CertificateID: "crt-1", Revision: 1, CurrentRevision: 1,
		ContentSHA256: strings.Repeat("a", 64), Market: "DE", ValidUntil: at.Add(time.Hour)}
	if ok.Check("/p") != nil || ok.Evidence() != "crt-1/v1/"+strings.Repeat("a", 64)+"/applicable/DE/2026-12-02T01:00:00.000000Z" {
		t.Fatalf("applicable: %v %s", ok.Check("/p"), ok.Evidence())
	}
	for _, c := range []struct {
		code, want string
	}{
		{CodeCertificateNotApplicable, "CERTIFICATE_NOT_APPLICABLE /p"},
		{CodeReferenceStale, "REFERENCE_STALE /p"},
		{"REFERENCE_INVALID", "REFERENCE_INVALID /p"},
	} {
		r := ok
		r.Code = c.code
		if got := verdict(r.Check("/p")); got != c.want {
			t.Errorf("%s: %s", c.code, got)
		}
	}
}
