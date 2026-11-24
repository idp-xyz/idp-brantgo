package plugin

import (
	"errors"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func TestApparelManifestValidatesAndHashesStably(t *testing.T) {
	m := ApparelManifest()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if !m.ProvidesCost() || m.Profile() != ProfileApparel {
		t.Fatalf("profile %s cost %v", m.Profile(), m.ProvidesCost())
	}
	if got := m.SHA256(); len(got) != 64 || got != m.SHA256() {
		t.Fatalf("sha %s", got)
	}
	if !Compatible(CoreVersion, m) {
		t.Fatal("apparel 0.1.0 must be a tested triple")
	}
	m.Version = "9.9.9"
	if Compatible(CoreVersion, m) || VersionIncompatible().Code != CodePluginVersionIncompatible {
		t.Fatal("an untested plugin version must be refused")
	}
	if err := (PluginManifest{ID: "not an id"}).Validate(); err == nil {
		t.Fatal("an invalid id must be refused")
	}
}

func TestMergeFindingsIsOrderIndependent(t *testing.T) {
	pass := Finding{RuleID: "a", Verdict: VerdictPass}
	approval := Finding{RuleID: "b", Verdict: VerdictRequiresApproval}
	reject := Finding{RuleID: "c", Verdict: VerdictReject}
	if MergeFindings(nil) != VerdictPass || MergeFindings([]Finding{pass, pass}) != VerdictPass {
		t.Fatal("pass")
	}
	left := []Finding{pass, approval, reject}
	right := []Finding{reject, pass, approval}
	if MergeFindings(left) != VerdictReject || MergeFindings(right) != VerdictReject {
		t.Fatal("reject dominates")
	}
	if MergeFindings([]Finding{approval, pass}) != VerdictRequiresApproval || MergeFindings([]Finding{pass, approval}) != VerdictRequiresApproval {
		t.Fatal("requires_approval dominates pass, in either order")
	}
	if len(left) != 3 {
		t.Fatal("merge must not drop findings")
	}
}

func TestRegistryRejectsDuplicatePluginsChecksAndCostProviders(t *testing.T) {
	r := NewRegistry()
	apparel := ApparelManifest()
	if err := r.Register(Registration{Manifest: apparel}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Registration{Manifest: apparel}); err == nil {
		t.Fatal("duplicate plugin id")
	}
	withCheck := apparel
	withCheck.ID = "industry.other"
	withCheck.Checks = []string{"APP-VAL-01"}
	withCheck.Activation.DocumentProfile = string(ProfileGeneric)
	withCheck.Capabilities = []string{"quote.validate.v1"}
	if err := r.Register(Registration{Manifest: withCheck}); err != nil {
		t.Fatal(err)
	}
	again := withCheck
	again.ID = "industry.third"
	if err := r.Register(Registration{Manifest: again}); err == nil {
		t.Fatal("duplicate check id")
	}
	cost := func(id string, profile DocumentProfile) Registration {
		m := apparel
		m.ID = id
		m.Activation.DocumentProfile = string(profile)
		m.Checks = nil
		return Registration{Manifest: m, Cost: fakeCost{id: id, profile: profile}}
	}
	if err := r.Register(cost("cost.one", ProfileApparel)); err != nil {
		t.Fatal(err)
	}
	err := r.Register(cost("cost.two", ProfileApparel))
	var rej *kernel.Rejection
	if !errors.As(err, &rej) || rej.Code != CodeCostProviderConflict {
		t.Fatalf("second cost provider: %v", err)
	}
	if err := r.Register(cost("cost.generic", ProfileGeneric)); err != nil {
		t.Fatal(err)
	}
}

type fakeCost struct {
	id      string
	profile DocumentProfile
}

func (f fakeCost) PluginID() string                 { return f.id }
func (f fakeCost) DocumentProfile() DocumentProfile { return f.profile }
func (f fakeCost) Estimate(CostEstimateInput) (CostEstimateOutput, error) {
	return CostEstimateOutput{}, nil
}

func TestUniformProfileRejectsAMix(t *testing.T) {
	got, err := Uniform([]string{"generic", "generic"}, "/args/order_id")
	if err != nil || got != ProfileGeneric {
		t.Fatal(got, err)
	}
	if _, err := Uniform([]string{"generic", "apparel"}, "/args/order_id"); err == nil || CodeOf(err) != CodeDocumentProfileMismatch {
		t.Fatalf("mix: %v", err)
	}
}

func TestCanonicalProfile(t *testing.T) {
	g, err := Canonical("", "/args/document_profile")
	if err != nil || g != ProfileGeneric {
		t.Fatal(g, err)
	}
	a, err := Canonical("apparel", "/args/document_profile")
	if err != nil || a != ProfileApparel {
		t.Fatal(a, err)
	}
	if _, err := Canonical("textile", "/args/document_profile"); err == nil {
		t.Fatal("unknown profile")
	}
}
