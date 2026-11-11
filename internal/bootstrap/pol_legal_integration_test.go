//go:build integration

package bootstrap_test

import (
	"testing"
	"time"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S3-17 (G0 F-02): legal terms are a POL configuration kind like the others — saved whole,
// published by a second human citing evidence, read back as saved with their content hash, and
// resolved for downstream contexts at an instant — and POL refuses a formation rule the CISG or PRC
// law contradicts. The tenant's default (默认 CISG 与中国法，由租户配置) is the legal terms revision
// the company profile names: refused while that revision is not in force (still a working draft),
// published once it is, and read back with the profile.
func TestLegalTermsKindAndTheTenantDefault(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.currency(editor, reviewer, "CNY", win)
	_, err := w.saveConfig(editor, polapi.KindLegalTerms, "legal.wrong", 0, win.from, time.Time{},
		termsData("CISG, dispatch", "DE", polapi.CISGApplies, polapi.FormationDispatch, hamburgArbitration))
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/data/formation_rule" {
		t.Fatalf("the CISG forms by arrival: %s at %s", code(err), path(err))
	}
	data := termsData("CISG with PRC law", polapi.GoverningLawCN, polapi.CISGApplies, polapi.FormationArrival, cietac)
	saved, err := w.saveConfig(editor, polapi.KindLegalTerms, "legal.default", 0, win.from, time.Time{}, data)
	w.must(err)
	def := polapi.ConfigRef{Kind: polapi.KindLegalTerms, ConfigID: "legal.default", Revision: 1}
	profile := companyData("CNY")
	profile.CompanyProfile.DefaultLegalTerms = &def
	_, err = w.saveConfig(editor, polapi.KindCompanyProfile, "company.A", 0, win.from, time.Time{}, profile)
	if code(err) != polapi.CodeConfigNotActive || path(err) != "/args/data/default_legal_terms" {
		t.Fatalf("a default still in draft: %s at %s", code(err), path(err))
	}
	_, err = w.publish(editor, polapi.KindLegalTerms, "legal.default", saved.RowVersion, 1)
	w.want(err, polapi.CodeIndependentReviewRequired)
	_, err = w.publish(reviewer, polapi.KindLegalTerms, "legal.default", saved.RowVersion, 1)
	w.must(err)
	company := w.configure(editor, reviewer, polapi.KindCompanyProfile, "company.A", 0, win.from, time.Time{}, profile, true)

	v, err := w.c.POL.GetConfig(editor, polapi.KindCompanyProfile, company.ConfigID)
	w.must(err)
	if got := v.Revisions[0].Data.CompanyProfile.DefaultLegalTerms; got == nil || *got != def || v.ActiveRevision != 1 {
		t.Fatalf("the profile %+v", v.Revisions[0].Data.CompanyProfile)
	}
	l, err := w.c.POL.GetConfig(editor, polapi.KindLegalTerms, "legal.default")
	w.must(err)
	if r := l.Revisions[0]; r.Status != polapi.StatusActive || *r.Data.LegalTerms != *data.LegalTerms || len(r.ContentHash) != 64 ||
		r.PublishedBy.ID != "reviewer" {
		t.Fatalf("the legal terms %+v", r)
	}
	resolved, err := w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{def}, time.Now())
	w.must(err)
	if r := resolved[def]; r.Code != "" || r.Data.LegalTerms == nil || r.Data.LegalTerms.GoverningLaw != polapi.GoverningLawCN ||
		r.ContentHash != l.Revisions[0].ContentHash {
		t.Fatalf("resolved %+v", r)
	}
}
