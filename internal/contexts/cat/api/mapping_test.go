package api

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return "OK"
}

func TestItemCodeMappingContract(t *testing.T) {
	for _, c := range []struct {
		edit func(*SaveItemCodeMapping)
		want string
	}{
		{func(*SaveItemCodeMapping) {}, "OK"},
		{func(c *SaveItemCodeMapping) { c.ExternalCode = "ST-01/红色 XL" }, "OK"},
		{func(c *SaveItemCodeMapping) { c.ExpectedVersion = -1 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *SaveItemCodeMapping) { c.PartyID = "" }, "SCHEMA_INVALID /args/party_id"},
		{func(c *SaveItemCodeMapping) { c.PartyRole = "carrier" }, "SCHEMA_INVALID /args/party_role"},
		{func(c *SaveItemCodeMapping) { c.ExternalCode = "" }, "SCHEMA_INVALID /args/external_code"},
		{func(c *SaveItemCodeMapping) { c.ExternalCode = " \t" }, "SCHEMA_INVALID /args/external_code"},
		{func(c *SaveItemCodeMapping) { c.ExternalCode = "SUP-01 " }, "SCHEMA_INVALID /args/external_code"},
		{func(c *SaveItemCodeMapping) { c.ExternalCode = "　SUP-01" }, "SCHEMA_INVALID /args/external_code"},
		{func(c *SaveItemCodeMapping) { c.ExternalCode = "SUP\n01" }, "SCHEMA_INVALID /args/external_code"},
		{func(c *SaveItemCodeMapping) { c.ExternalCode = "SUP\xff01" }, "SCHEMA_INVALID /args/external_code"},
		{func(c *SaveItemCodeMapping) { c.ExternalCode = strings.Repeat("码", MaxExternalCodeLength) }, "OK"},
		{func(c *SaveItemCodeMapping) { c.ExternalCode = strings.Repeat("码", MaxExternalCodeLength+1) }, "SCHEMA_INVALID /args/external_code"},
		{func(c *SaveItemCodeMapping) { c.SkuID = "" }, "SCHEMA_INVALID /args/sku_id"},
		{func(c *SaveItemCodeMapping) { c.SpecRevision = 0 }, "SCHEMA_INVALID /args/spec_revision"},
	} {
		cmd := SaveItemCodeMapping{IdempotencyKey: "k1", MappingID: "mapping1", PartyID: "supplier", PartyRole: RoleSupplier,
			ExternalCode: "SUP-01", SkuID: "sku", SpecRevision: 1}
		c.edit(&cmd)
		if got := code(cmd.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", cmd, got, c.want)
		}
	}
	for _, c := range []struct {
		edit func(*PublishItemCodeMapping)
		want string
	}{
		{func(*PublishItemCodeMapping) {}, "OK"},
		{func(c *PublishItemCodeMapping) { c.ExpectedVersion = 0 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *PublishItemCodeMapping) { c.Revision = 0 }, "SCHEMA_INVALID /args/revision"},
		{func(c *PublishItemCodeMapping) { c.EvidenceIDs = nil }, "SCHEMA_INVALID /args/evidence_ids"},
		{func(c *PublishItemCodeMapping) { c.EvidenceIDs = []string{"e", "e"} }, "SCHEMA_INVALID /args/evidence_ids"},
		{func(c *PublishItemCodeMapping) { c.Reason = "" }, "SCHEMA_INVALID /args/reason"},
	} {
		cmd := PublishItemCodeMapping{IdempotencyKey: "k1", MappingID: "mapping1", ExpectedVersion: 1, Revision: 1, EvidenceIDs: []string{"e"},
			Reason: "核实"}
		c.edit(&cmd)
		if got := code(cmd.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", cmd, got, c.want)
		}
	}
}

func TestLookupsAndVerdicts(t *testing.T) {
	ok := ItemCode{PartyID: "supplier", PartyRole: RoleSupplier, ExternalCode: "SUP-01"}
	if got := code(ValidateItemCodes([]ItemCode{ok, ok, {PartyID: "c", PartyRole: "buyer", ExternalCode: "x"}})); got != "SCHEMA_INVALID /codes/2/party_role" {
		t.Fatal(got)
	}
	if got := code(ValidateItemCodes(make([]ItemCode, MaxLookup+1))); got != "SCHEMA_INVALID /codes" {
		t.Fatal(got)
	}
	if got := code(ValidateSpecRefs([]SpecRef{{SkuID: "sku", SpecRevision: 1}, {SkuID: "sku"}})); got != "SCHEMA_INVALID /refs/1/spec_revision" {
		t.Fatal(got)
	}
	if (ResolvedItemCode{}).Mapped() || !(ResolvedItemCode{Status: ItemCodeMapped}).Mapped() {
		t.Fatal("only a mapped answer is mapped")
	}
	for _, c := range []struct {
		s    SpecStanding
		want string
	}{
		{SpecStanding{Current: true}, "OK"},
		{SpecStanding{}, "REFERENCE_INVALID /lines/0/sku"}, // a standing CAT did not give is never current
		{SpecStanding{Code: CodeReferenceStale}, "REFERENCE_STALE /lines/0/sku"},
		{SpecStanding{Code: kernel.CodeReferenceInvalid}, "REFERENCE_INVALID /lines/0/sku"},
	} {
		if got := code(c.s.Check("/lines/0/sku")); got != c.want {
			t.Errorf("%+v: %s, want %s", c.s, got, c.want)
		}
	}
	if r := Reject(CodeBusinessKeyConflict, "/x"); r.Category != kernel.CategoryConflict {
		t.Fatalf("%+v", r)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an undeclared code panics")
		}
	}()
	_ = Reject("NOT_A_CAT_CODE", "/")
}
