package api

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The closed contract of declaration_profile.save_draft and declaration_profile.verify (CAT-INV-002,
// D-22: classification parts kept separately, each with its basis). Python V0.50 has no declaration
// profile; the vectors come from the design.

func validProfile() SaveDeclarationProfile {
	return SaveDeclarationProfile{IdempotencyKey: "k1", ProfileID: "profile.1", SkuID: "ST01.red.S", SpecRevision: 1, CodeTableVersion: "CN-2026",
		Commodity:       CommodityClassification{Code: "6109100021", Basis: "合成归类依据", SourceIDs: []string{"spec.1"}},
		DeclarationName: "棉制针织T恤衫",
		Elements:        []DeclarationElement{{Name: "品牌类型", Value: "无品牌"}, {Name: "成分含量", Value: "100%棉"}},
		LegalUnits:      []LegalUnitConversion{{Unit: "件", PerBaseUnit: "1", Basis: "一件即一 PCS"}, {Unit: "千克", PerBaseUnit: "0.25", Basis: "合成：实测净重"}},
		Origin:          OriginBasis{Country: "CN", Criterion: OriginWhollyObtained, Basis: "合成：境内生产"},
		ExportControl:   ExportControlClassification{Status: ExportNotControlled, Basis: "合成：不属于管制清单"},
	}
}

func TestDeclarationProfileContract(t *testing.T) {
	many := make([]DeclarationElement, MaxDeclarationElements+1)
	for i := range many {
		many[i] = DeclarationElement{Name: "要素" + strings.Repeat("一", i+1), Value: "x"}
	}
	for _, c := range []struct {
		edit func(*SaveDeclarationProfile)
		want string
	}{
		{func(*SaveDeclarationProfile) {}, "OK"},
		{func(c *SaveDeclarationProfile) { c.ExpectedVersion = -1 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *SaveDeclarationProfile) { c.ProfileID = "" }, "SCHEMA_INVALID /args/id"},
		{func(c *SaveDeclarationProfile) { c.SkuID = "ST01 red" }, "SCHEMA_INVALID /args/sku_id"},
		{func(c *SaveDeclarationProfile) { c.SpecRevision = 0 }, "SCHEMA_INVALID /args/spec_revision"},
		{func(c *SaveDeclarationProfile) { c.CodeTableVersion = "" }, "SCHEMA_INVALID /args/code_table_version"},
		{func(c *SaveDeclarationProfile) { c.Commodity.Code = "6109.10" }, "SCHEMA_INVALID /args/commodity/code"},
		{func(c *SaveDeclarationProfile) { c.Commodity.Code = "61091" }, "SCHEMA_INVALID /args/commodity/code"},
		{func(c *SaveDeclarationProfile) { c.Commodity.Code = "6109100021001" }, "SCHEMA_INVALID /args/commodity/code"},
		{func(c *SaveDeclarationProfile) { c.Commodity.Basis = " \n" }, "SCHEMA_INVALID /args/commodity/basis"},
		{func(c *SaveDeclarationProfile) { c.Commodity.Basis = "依据\x00" }, "SCHEMA_INVALID /args/commodity/basis"},
		{func(c *SaveDeclarationProfile) { c.Commodity.Basis = "第一行\n第二行\t附注" }, "OK"},
		{func(c *SaveDeclarationProfile) { c.Commodity.SourceIDs = []string{"s", "s"} }, "SCHEMA_INVALID /args/commodity/source_ids"},
		{func(c *SaveDeclarationProfile) { c.Commodity.SourceIDs = nil }, "OK"},
		{func(c *SaveDeclarationProfile) { c.DeclarationName = " T恤" }, "SCHEMA_INVALID /args/declaration_name"},
		{func(c *SaveDeclarationProfile) { c.Elements = nil }, "SCHEMA_INVALID /args/elements"},
		{func(c *SaveDeclarationProfile) { c.Elements = many }, "SCHEMA_INVALID /args/elements"},
		{func(c *SaveDeclarationProfile) { c.Elements[1].Name = "品牌类型" }, "SCHEMA_INVALID /args/elements/1/name"},
		{func(c *SaveDeclarationProfile) { c.Elements[0].Value = "无\n品牌" }, "SCHEMA_INVALID /args/elements/0/value"},
		{func(c *SaveDeclarationProfile) { c.LegalUnits = nil }, "SCHEMA_INVALID /args/legal_units"},
		{func(c *SaveDeclarationProfile) { c.LegalUnits = append(c.LegalUnits, c.LegalUnits[0]) }, "SCHEMA_INVALID /args/legal_units"},
		{func(c *SaveDeclarationProfile) { c.LegalUnits[1].Unit = "件" }, "SCHEMA_INVALID /args/legal_units/1/unit"},
		{func(c *SaveDeclarationProfile) { c.LegalUnits[1].PerBaseUnit = "0" }, "SCHEMA_INVALID /args/legal_units/1/per_base_unit"},
		{func(c *SaveDeclarationProfile) { c.LegalUnits[1].PerBaseUnit = "2.5e-1" }, "SCHEMA_INVALID /args/legal_units/1/per_base_unit"},
		{func(c *SaveDeclarationProfile) { c.LegalUnits[1].PerBaseUnit = "0.0000000000001" }, "SCHEMA_INVALID /args/legal_units/1/per_base_unit"},
		{func(c *SaveDeclarationProfile) { c.LegalUnits[1].PerBaseUnit = "0.000000000001" }, "OK"},
		{func(c *SaveDeclarationProfile) { c.LegalUnits[0].Basis = "" }, "SCHEMA_INVALID /args/legal_units/0/basis"},
		{func(c *SaveDeclarationProfile) { c.Origin.Country = "cn" }, "SCHEMA_INVALID /args/origin/country"},
		{func(c *SaveDeclarationProfile) { c.Origin.Country = "CHN" }, "SCHEMA_INVALID /args/origin/country"},
		{func(c *SaveDeclarationProfile) { c.Origin.Criterion = "preferential" }, "SCHEMA_INVALID /args/origin/criterion"},
		{func(c *SaveDeclarationProfile) { c.Origin.Criterion = OriginSubstantialTransformation }, "OK"},
		{func(c *SaveDeclarationProfile) { c.Origin.Basis = "" }, "SCHEMA_INVALID /args/origin/basis"},
		{func(c *SaveDeclarationProfile) { c.Origin.SourceIDs = make([]string, MaxPartSources+1) }, "SCHEMA_INVALID /args/origin/source_ids"},
		{func(c *SaveDeclarationProfile) { c.ExportControl.Status = "unknown" }, "SCHEMA_INVALID /args/export_control/status"},
		{func(c *SaveDeclarationProfile) { c.ExportControl.ListReference = "1C999" }, "SCHEMA_INVALID /args/export_control/list_reference"},
		{func(c *SaveDeclarationProfile) { c.ExportControl.Status = ExportControlled }, "SCHEMA_INVALID /args/export_control/list_reference"},
		{func(c *SaveDeclarationProfile) {
			c.ExportControl.Status, c.ExportControl.ListReference = ExportControlled, "合成管制清单 1C999"
		}, "OK"},
		{func(c *SaveDeclarationProfile) { c.ExportControl.Basis = "" }, "SCHEMA_INVALID /args/export_control/basis"},
		{func(c *SaveDeclarationProfile) { c.ExportControl.SourceIDs = []string{"bad id"} }, "SCHEMA_INVALID /args/export_control/source_ids"},
	} {
		cmd := validProfile()
		c.edit(&cmd)
		if got := code(cmd.Validate()); got != c.want {
			t.Errorf("%s, want %s: %+v", got, c.want, cmd)
		}
	}
	for _, c := range []struct {
		edit func(*VerifyDeclarationProfile)
		want string
	}{
		{func(*VerifyDeclarationProfile) {}, "OK"},
		{func(c *VerifyDeclarationProfile) { c.ExpectedVersion = 0 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *VerifyDeclarationProfile) { c.Revision = 0 }, "SCHEMA_INVALID /args/revision"},
		{func(c *VerifyDeclarationProfile) { c.EvidenceIDs = nil }, "SCHEMA_INVALID /args/evidence_ids"},
		{func(c *VerifyDeclarationProfile) { c.EvidenceIDs = []string{"e", "e"} }, "SCHEMA_INVALID /args/evidence_ids"},
		{func(c *VerifyDeclarationProfile) { c.Reason = "" }, "SCHEMA_INVALID /args/reason"},
	} {
		cmd := VerifyDeclarationProfile{IdempotencyKey: "k1", ProfileID: "profile.1", ExpectedVersion: 1, Revision: 1, EvidenceIDs: []string{"e"},
			Reason: "专业核实"}
		c.edit(&cmd)
		if got := code(cmd.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", cmd, got, c.want)
		}
	}
}

// DeclarationProfileRef v1 lookups: at most MaxLookup references, each with its key; revision 0 asks
// for the verified revision; Check passes CAT's verdict on and never lets a zero answer through.
func TestDeclarationProfileRefs(t *testing.T) {
	ok := DeclarationProfileRef{SkuID: "ST01.red.S", SpecRevision: 1, CodeTableVersion: "CN-2026"}
	if err := ValidateDeclarationProfileRefs([]DeclarationProfileRef{ok, {SkuID: "s", SpecRevision: 1, CodeTableVersion: "t", Revision: 3}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		refs []DeclarationProfileRef
		want string
	}{
		{make([]DeclarationProfileRef, MaxLookup+1), "SCHEMA_INVALID /refs"},
		{[]DeclarationProfileRef{ok, {SkuID: "s", CodeTableVersion: "t"}}, "SCHEMA_INVALID /refs/1/spec_revision"},
		{[]DeclarationProfileRef{{SkuID: "s", SpecRevision: 1}}, "SCHEMA_INVALID /refs/0/code_table_version"},
		{[]DeclarationProfileRef{{SkuID: "s", SpecRevision: 1, CodeTableVersion: "t", Revision: -1}}, "SCHEMA_INVALID /refs/0/revision"},
	} {
		if got := code(ValidateDeclarationProfileRefs(c.refs)); got != c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
	if ok.Key() != (DeclarationProfileKey{SkuID: "ST01.red.S", SpecRevision: 1, CodeTableVersion: "CN-2026"}) {
		t.Fatal(ok.Key())
	}
	answer := ResolvedDeclarationProfile{Ref: ok, Verified: true, Revision: 2}
	if adopted := answer.Adopted(); adopted.Revision != 2 || adopted.Key() != ok.Key() {
		t.Fatalf("adopted %+v", adopted)
	}
	for _, c := range []struct {
		r    ResolvedDeclarationProfile
		want string
	}{
		{answer, "OK"},
		{ResolvedDeclarationProfile{}, "DECLARATION_PROFILE_UNVERIFIED /lines/0/sku"},
		{ResolvedDeclarationProfile{Code: CodeDeclarationProfileUnverified}, "DECLARATION_PROFILE_UNVERIFIED /lines/0/sku"},
		{ResolvedDeclarationProfile{Code: CodeReferenceStale}, "REFERENCE_STALE /lines/0/sku"},
		{ResolvedDeclarationProfile{Code: kernel.CodeReferenceInvalid}, "REFERENCE_INVALID /lines/0/sku"},
	} {
		if got := code(c.r.Check("/lines/0/sku")); got != c.want {
			t.Errorf("%+v: %s, want %s", c.r, got, c.want)
		}
	}
	if c, _ := Category(CodeDeclarationProfileUnverified); c != kernel.CategoryInvalid {
		t.Fatalf("DECLARATION_PROFILE_UNVERIFIED is 422 in the error catalog: %s", c)
	}
}
