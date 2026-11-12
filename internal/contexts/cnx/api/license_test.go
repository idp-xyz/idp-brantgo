package api

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func validRegister() RegisterLicense {
	return RegisterLicense{IdempotencyKey: "k1", LicenseNumber: "SYN-EL-1", Issuer: "合成发证机关", Holder: "合成出口商",
		Goods: LicensedGoods{SkuSpecs: []SpecRef{{SkuID: "sku.1", SpecRevision: 1}}}, LicensedQuantity: "1000", Unit: "PCS",
		Currency: CurrencyRef{ID: "USD", Revision: 1}, LicensedValue: "10000.00", Destination: "DE", EndUser: EndUser{PartyID: "customer.1", Name: "合成"},
		ValidFrom: "2026-11-01T00:00:00Z", ValidUntil: "2027-05-01T00:00:00Z", FileID: "licence.1"}
}

// export_license.register's closed contract: goods are SKU specifications or a classification,
// exactly one; positive quantity and value; a country; an interval that ends after it starts.
func TestRegisterLicenseContract(t *testing.T) {
	if err := validRegister().Validate(); err != nil {
		t.Fatal(err)
	}
	for want, edit := range map[string]func(*RegisterLicense){
		"/args/license_number": func(c *RegisterLicense) { c.LicenseNumber = "" },
		"/args/goods": func(c *RegisterLicense) {
			c.Goods.Classification = &Classification{CodeTableVersion: "CN-2026", CommodityCodes: []string{"1"}}
		},
		"/args/goods/sku_specs/1/spec_revision": func(c *RegisterLicense) { c.Goods.SkuSpecs = append(c.Goods.SkuSpecs, c.Goods.SkuSpecs[0]) },
		"/args/goods/classification/commodity_codes": func(c *RegisterLicense) {
			c.Goods = LicensedGoods{Classification: &Classification{CodeTableVersion: "CN-2026"}}
		},
		"/args/licensed_quantity": func(c *RegisterLicense) { c.LicensedQuantity = "0" },
		"/args/unit":              func(c *RegisterLicense) { c.Unit = "pcs" },
		"/args/currency/revision": func(c *RegisterLicense) { c.Currency.Revision = 0 },
		"/args/licensed_value":    func(c *RegisterLicense) { c.LicensedValue = "-1" },
		"/args/destination":       func(c *RegisterLicense) { c.Destination = "XX" },
		"/args/end_user/party_id": func(c *RegisterLicense) { c.EndUser.PartyID = "" },
		"/args/valid_until":       func(c *RegisterLicense) { c.ValidUntil = c.ValidFrom },
		"/args/file_id":           func(c *RegisterLicense) { c.FileID = "" },
	} {
		c := validRegister()
		edit(&c)
		if got := verdict(c.Validate()); got != "SCHEMA_INVALID "+want {
			t.Errorf("%s: %s", want, got)
		}
	}
	byCode := validRegister()
	byCode.Goods = LicensedGoods{Classification: &Classification{CodeTableVersion: "CN-2026", CommodityCodes: []string{"6109100021"}}}
	if err := byCode.Validate(); err != nil {
		t.Fatal(err)
	}
}

// export_license.allocate names a shipment or an export lot; export_license.release needs an
// external result (amended or withdrawn, with evidence) and no local cause exists; amended names a
// quantity and value, withdrawn neither.
func TestAllocateAndReleaseContracts(t *testing.T) {
	a := AllocateLicense{IdempotencyKey: "k1", LicenseID: "elc.1", AllocationID: "a1", Target: ExportTarget{Kind: TargetShipment, ID: "s1"}}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	a.Target.Kind = "order"
	if got := verdict(a.Validate()); got != "SCHEMA_INVALID /args/target/kind" {
		t.Fatal(got)
	}
	valid := func() ReleaseLicense {
		return ReleaseLicense{IdempotencyKey: "k1", LicenseID: "elc.1", ExpectedVersion: 2, AllocationID: "a1", ReleaseID: "r1",
			ExternalResult: &ExternalResult{Kind: ExternalAmended, DeclarationNumber: "D1", ReceiptText: "改单回执", ReceivedAt: "2026-11-12T00:00:00Z",
				EvidenceIDs: []string{"e1"}}, Quantity: "100", Value: "0", Reason: "改单"}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for want, edit := range map[string]func(*ReleaseLicense){
		"/args/external_result":              func(c *ReleaseLicense) { c.ExternalResult = nil },
		"/args/external_result/kind":         func(c *ReleaseLicense) { c.ExternalResult.Kind = "cancelled" },
		"/args/external_result/evidence_ids": func(c *ReleaseLicense) { c.ExternalResult.EvidenceIDs = nil },
		"/args/external_result/received_at":  func(c *ReleaseLicense) { c.ExternalResult.ReceivedAt = "today" },
		"/args/quantity":                     func(c *ReleaseLicense) { c.ExternalResult.Kind = ExternalWithdrawn },
		"/args/value":                        func(c *ReleaseLicense) { c.Value = "" },
		"/args/release_id":                   func(c *ReleaseLicense) { c.ReleaseID = "" },
		"/args/reason":                       func(c *ReleaseLicense) { c.Reason = " " },
	} {
		c := valid()
		edit(&c)
		if got := verdict(c.Validate()); got != "SCHEMA_INVALID "+want {
			t.Errorf("%s: %s", want, got)
		}
	}
	w := valid()
	w.ExternalResult.Kind, w.Quantity, w.Value = ExternalWithdrawn, "", ""
	if err := w.Validate(); err != nil {
		t.Fatal(err)
	}
}

func licenseRow(covered bool, status, reason string, quantity, hash *string, until *time.Time, code *string) func(dest ...any) error {
	return func(dest ...any) error {
		lic, alloc, unit, value, cur := "elc.1", "a1", "PCS", "600000", "USD"
		*dest[0].(*bool) = covered
		*dest[1].(**string) = &status
		*dest[2].(**string) = &reason
		*dest[3].(**string) = &lic
		*dest[4].(**string) = &alloc
		*dest[5].(**string) = quantity
		*dest[6].(**string) = &unit
		*dest[7].(**string) = &value
		*dest[8].(**string) = &cur
		*dest[9].(**string) = hash
		*dest[10].(**time.Time) = until
		*dest[11].(**string) = code
		return nil
	}
}

// ExportLicenseGuard v1's rows as FUL reads them: only a covered row without a code passes, and only
// for the lines the allocation was made for, within what is outstanding; every other row is
// EXPORT_LICENSE_REQUIRED (409) at the caller's path.
func TestExportLicenseGuardVerdicts(t *testing.T) {
	lines := []ShipmentLine{{LineID: "l1", SkuID: "sku.1", SpecRevision: 1, Quantity: "600", Unit: "PCS"}}
	hash, err := LinesHash(lines)
	if err != nil {
		t.Fatal(err)
	}
	q, until, required := "600", time.Date(2027, 5, 1, 0, 0, 0, 1500, time.UTC), CodeExportLicenseRequired
	less := "599"
	for _, c := range []struct {
		scan func(dest ...any) error
		want string
	}{
		{licenseRow(true, LicenseStatusAllocated, LicenseReasonAllocated, &q, &hash, &until, nil), "OK"},
		{licenseRow(false, LicenseStatusRequired, LicenseReasonNotAllocated, nil, nil, nil, &required), "EXPORT_LICENSE_REQUIRED /args/shipment_id"},
		{licenseRow(false, LicenseStatusRequired, LicenseReasonReleased, &q, &hash, &until, &required), "EXPORT_LICENSE_REQUIRED /args/shipment_id"},
		{licenseRow(false, LicenseStatusExpired, LicenseReasonExpired, &q, &hash, &until, &required), "EXPORT_LICENSE_REQUIRED /args/shipment_id"},
		{licenseRow(true, LicenseStatusAllocated, LicenseReasonAllocated, &q, &hash, &until, &required), "EXPORT_LICENSE_REQUIRED /args/shipment_id"},
		{licenseRow(true, LicenseStatusAllocated, LicenseReasonAllocated, &less, &hash, &until, nil), "EXPORT_LICENSE_REQUIRED /args/shipment_id"},
		{licenseRow(true, LicenseStatusAllocated, LicenseReasonAllocated, &q, &less, &until, nil), "EXPORT_LICENSE_REQUIRED /args/shipment_id"},
	} {
		v, err := ScanExportLicenseGuard("s1", c.scan)
		if err != nil {
			t.Fatal(err)
		}
		if got := verdict(v.Check("/args/shipment_id", lines)); got != c.want || v.ShipmentID != "s1" {
			t.Errorf("%+v: want %s, got %s", v, c.want, got)
		}
	}
	v, _ := ScanExportLicenseGuard("s1", licenseRow(true, LicenseStatusAllocated, LicenseReasonAllocated, &q, &hash, &until, nil))
	if v.LicenseID != "elc.1" || v.AllocationID != "a1" || v.ValueMinor != "600000" || !v.ValidUntil.Equal(kernel.Instant(until)) ||
		v.Evidence() != "allocated/allocated/elc.1/a1/"+hash {
		t.Fatalf("%+v %s", v, v.Evidence())
	}
	if args := ExportLicenseGuardArgs("s1", until); len(args) != 2 || args[1].(time.Time) != kernel.Instant(until) {
		t.Fatalf("%v", args)
	}
	other := []ShipmentLine{lines[0], {LineID: "l0", SkuID: "sku.2", SpecRevision: 1, Quantity: "1", Unit: "PCS"}}
	h2, _ := LinesHash([]ShipmentLine{other[1], other[0]})
	if h1, _ := LinesHash(other); h1 != h2 || h1 == hash || !strings.HasPrefix(ExportLicenseGuardSQL, "SELECT covered") {
		t.Fatal("lines hash")
	}
	if _, err := LinesHash([]ShipmentLine{{LineID: "l1", Quantity: "1.0000001"}}); err == nil {
		t.Fatal("a quantity with seven decimals")
	}
}
