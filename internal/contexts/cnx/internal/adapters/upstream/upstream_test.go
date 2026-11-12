package upstream

import (
	"context"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/application"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
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

// CAT's declaration profiles (S5 A5) are refused as DEPENDENCY_NOT_SUPPORTED, never answered empty.
func TestProfilesNotWiredRefuses(t *testing.T) {
	_, err := NotWired{}.ResolveProfiles(context.Background(), nil)
	if r, ok := kernel.AsRejection(err); !ok || r.Code != "DEPENDENCY_NOT_SUPPORTED" || r.Category != kernel.CategoryInvalid {
		t.Errorf("%v", err)
	}
}
