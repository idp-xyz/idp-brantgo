package upstream

import (
	"context"
	"strings"
	"testing"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/application"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

type roles map[ptyapi.AddressRoleRef]ptyapi.ResolvedAddressRole

func (r roles) ResolveAddressRoles(_ context.Context, refs []ptyapi.AddressRoleRef) (map[ptyapi.AddressRoleRef]ptyapi.ResolvedAddressRole, error) {
	out := map[ptyapi.AddressRoleRef]ptyapi.ResolvedAddressRole{}
	for _, ref := range refs {
		if a, ok := r[ref]; ok {
			out[ref] = a
		}
	}
	return out, nil
}

// R77 (S5 A3b): PTY answers the role and country of the bound party revision's address, a stale
// one too (the order binds the revision); an address confirmed before countries has none; one PTY
// does not resolve is absent.
func TestPTYCountries(t *testing.T) {
	hq := ptyapi.AddressRoleRef{PartyID: "p1", PartyRevision: 1, AddressID: "hq"}
	old := ptyapi.AddressRoleRef{PartyID: "p1", PartyRevision: 1, AddressID: "old"}
	p := PTY{Roles: roles{hq: {Ref: hq, Role: ptyapi.AddressConsignee, Country: "DE", Code: ptyapi.CodeReferenceStale},
		old: {Ref: old, Role: ptyapi.AddressBuyer}}}
	got, err := p.ResolveAddressCountries(context.Background(), []application.AddressRef{{PartyID: "p1", PartyRevision: 1, AddressID: "hq"},
		{PartyID: "p1", PartyRevision: 1, AddressID: "old"}, {PartyID: "p1", PartyRevision: 1, AddressID: "gone"}})
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if a := got[application.AddressRef{PartyID: "p1", PartyRevision: 1, AddressID: "hq"}]; a.Role != ptyapi.AddressConsignee || a.Country != "DE" {
		t.Fatalf("%+v", a)
	}
	if a := got[application.AddressRef{PartyID: "p1", PartyRevision: 1, AddressID: "old"}]; a.Country != "" {
		t.Fatalf("%+v", a)
	}
	if got, err := p.ResolveAddressCountries(context.Background(), nil); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}

type snapshots map[string]trdapi.OrderSnapshotFact

func (s snapshots) ResolveOrderSnapshots(_ context.Context, ids []string) (map[string]trdapi.OrderSnapshotFact, error) {
	out := map[string]trdapi.OrderSnapshotFact{}
	for _, id := range ids {
		if f, ok := s[id]; ok {
			out[id] = f
		}
	}
	return out, nil
}

// R46: TRD's order snapshot as CNX's port carries it — binding, state, sealed-revision verdict,
// customer, address roles and line specifications.
func TestTRDOrders(t *testing.T) {
	hash := strings.Repeat("a", 64)
	f := trdapi.OrderSnapshotFact{Binding: trdapi.OrderBinding{OrderID: "o1", OrderRevision: 2, OrderHash: hash}, Status: trdapi.OrderActive,
		CustomerRef: trdapi.PartyRef{PartyID: "cust"},
		OrderTerms: trdapi.OrderTerms{Addresses: []trdapi.OrderAddress{{Role: ptyapi.AddressConsignee, PartyID: "cust", PartyRevision: 3, AddressID: "dock",
			Address: "合成地址"}}},
		Lines: []trdapi.OrderLine{{LineID: "L1", Sku: trdapi.SkuRef{SkuID: "sku.1", SpecRevision: 2}}}}
	got, err := TRD{Snapshots: snapshots{"o1": f}}.ResolveOrders(context.Background(), []string{"o1", "o2"})
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	o := got["o1"]
	if o.Binding.OrderHash != hash || o.Binding.OrderRevision != 2 || o.Status != application.OrderActive || o.CustomerID != "cust" ||
		o.Addresses[0] != (application.OrderAddress{Role: ptyapi.AddressConsignee, PartyID: "cust", PartyRevision: 3, AddressID: "dock"}) ||
		o.Lines[0] != (application.OrderLine{LineID: "L1", SkuID: "sku.1", SpecRevision: 2}) {
		t.Fatalf("%+v", o)
	}
	if _, err := (TRD{Snapshots: snapshots{}}).ResolveOrders(context.Background(), []string{"bad id"}); err == nil {
		t.Fatal("an invalid order ID is refused")
	}
}

type programmes struct {
	asked refapi.ResolveProgrammes
	out   refapi.ResolvedProgrammes
}

func (p *programmes) ResolveCountryProgrammes(_ context.Context, q refapi.ResolveProgrammes) (refapi.ResolvedProgrammes, error) {
	p.asked = q
	return p.out, nil
}

// Reference data's country programmes (S5 A3b): the current version of each list asked (Version
// ""), the hits for the countries; a list never imported is absent.
func TestREFProgrammes(t *testing.T) {
	p := &programmes{out: refapi.ResolvedProgrammes{
		Versions: map[string]refapi.ProgrammeListVersion{"cn.embargo": {ListCode: "cn.embargo", Jurisdiction: "CN", Version: "v1", ContentHash: "h"}},
		Hits: []refapi.CountryProgrammeHit{{ListCode: "cn.embargo", Jurisdiction: "CN", Version: "v1", Country: "KP",
			Programme: refapi.Programme{ProgrammeID: "P1", Name: "合成禁运", Measure: refapi.MeasureEmbargo, Scope: "all", ContentHash: "ph"}}}}}
	got, err := REF{Programmes: p}.ResolveProgrammes(context.Background(), []string{"cn.embargo", "un.sc"}, []string{"DE", "KP"})
	if err != nil || len(p.asked.Lists) != 2 || p.asked.Lists[1] != (refapi.ListVersionRef{ListCode: "un.sc"}) || len(p.asked.Countries) != 2 {
		t.Fatalf("%+v %v", p.asked, err)
	}
	if _, ok := got.Versions["un.sc"]; ok || got.Versions["cn.embargo"].Version != "v1" || len(got.Hits) != 1 {
		t.Fatalf("%+v", got)
	}
	if h := got.Hits[0]; h.ProgrammeID != "P1" || h.Measure != refapi.MeasureEmbargo || h.Country != "KP" || h.ContentHash != "ph" {
		t.Fatalf("%+v", h)
	}
}

type profiles struct {
	asked []catapi.DeclarationProfileRef
	out   map[catapi.DeclarationProfileRef]catapi.ResolvedDeclarationProfile
}

func (p *profiles) ResolveDeclarationProfiles(_ context.Context, refs []catapi.DeclarationProfileRef) (map[catapi.DeclarationProfileRef]catapi.ResolvedDeclarationProfile, error) {
	p.asked = append(p.asked, refs...)
	out := map[catapi.DeclarationProfileRef]catapi.ResolvedDeclarationProfile{}
	for _, ref := range refs {
		r, ok := p.out[ref]
		if !ok {
			// CAT answers every reference: no profile has the key.
			r = catapi.ResolvedDeclarationProfile{Ref: ref, Code: catapi.CodeDeclarationProfileUnverified}
		}
		out[ref] = r
	}
	return out, nil
}

// R39 (S5 A5, CAT-INV-002): each line's (SKU, specification revision, code table version) is asked
// with revision 0, the revision verified now. A verified answer is the profile with its commodity
// code, export control classification and content hash; a profile with only a draft is unverified,
// without content; a key no profile has is absent. CAT's classifications are the check's.
func TestCATProfiles(t *testing.T) {
	if tcc.NotControlled != catapi.ExportNotControlled || tcc.Controlled != catapi.ExportControlled {
		t.Fatal("CAT's export control classifications are the check's")
	}
	key := func(sku string) catapi.DeclarationProfileRef {
		return catapi.DeclarationProfileRef{SkuID: sku, SpecRevision: 2, CodeTableVersion: "CN-2026"}
	}
	hash := strings.Repeat("c", 64)
	p := &profiles{out: map[catapi.DeclarationProfileRef]catapi.ResolvedDeclarationProfile{
		key("sku.plain"): {Ref: key("sku.plain"), Verified: true, ProfileID: "decl.plain", Revision: 3, Status: catapi.StatusVerified, CurrentRevision: 3,
			DraftRevision: 4, ContentHash: hash, Profile: catapi.DeclarationProfile{Commodity: catapi.CommodityClassification{Code: "6109100021"},
				ExportControl: catapi.ExportControlClassification{Status: catapi.ExportNotControlled}}},
		key("sku.dual"): {Ref: key("sku.dual"), Verified: true, ProfileID: "decl.dual", Revision: 1, ContentHash: hash,
			Profile: catapi.DeclarationProfile{Commodity: catapi.CommodityClassification{Code: "8401100000"},
				ExportControl: catapi.ExportControlClassification{Status: catapi.ExportControlled, ListReference: "合成两用物项 1A001"}}},
		key("sku.draft"): {Ref: key("sku.draft"), Code: catapi.CodeDeclarationProfileUnverified, ProfileID: "decl.draft", DraftRevision: 1},
	}}
	ref := func(sku string) application.ProfileRef {
		return application.ProfileRef{SkuID: sku, SpecRevision: 2, CodeTableVersion: "CN-2026"}
	}
	got, err := CAT{Profiles: p}.ResolveProfiles(context.Background(), []application.ProfileRef{ref("sku.plain"), ref("sku.dual"), ref("sku.draft"),
		ref("sku.none")})
	if err != nil || len(got) != 3 || len(p.asked) != 4 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, a := range p.asked {
		if a.Revision != 0 || a.CodeTableVersion != "CN-2026" {
			t.Fatalf("asked %+v", a)
		}
	}
	if want := (tcc.Profile{ProfileID: "decl.plain", Revision: 3, CodeTableVersion: "CN-2026", Verified: true, CommodityCode: "6109100021",
		ExportControl: tcc.NotControlled, ContentHash: hash}); got[ref("sku.plain")] != want {
		t.Fatalf("verified %+v", got[ref("sku.plain")])
	}
	if d := got[ref("sku.dual")]; !d.Verified || d.ExportControl != tcc.Controlled || d.ListReference != "合成两用物项 1A001" {
		t.Fatalf("controlled %+v", d)
	}
	if d := got[ref("sku.draft")]; d.Verified || d.ProfileID != "decl.draft" || d.Revision != 1 || d.CommodityCode != "" || d.ContentHash != "" {
		t.Fatalf("draft only %+v", d)
	}
	if _, ok := got[ref("sku.none")]; ok {
		t.Fatal("a key without a profile is absent")
	}
	if got, err := (CAT{Profiles: p}).ResolveProfiles(context.Background(), nil); err != nil || len(got) != 0 || len(p.asked) != 4 {
		t.Fatal("no keys, no lookup")
	}
}
