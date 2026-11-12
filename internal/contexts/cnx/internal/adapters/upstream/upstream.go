// Package upstream implements CNX's ports (application.Orders, Countries, Profiles, Programmes) on
// the upstream contexts' api packages. None of them calls another context's transaction owner:
// each lookup runs before CNX's transaction in the upstream's own.
//
// TRD's order snapshots (OrderCommercialSnapshot v1 / SalesOrderRef v1, S5 A2), PTY's address
// countries (AddressRoleRef v1 with the country, S5 A3b) and reference data's country programme
// lists (S5 A3b) are wired. CAT's declaration profiles (S5 A5, catapi ResolveDeclarationProfiles)
// are not in this build: NotWired answers DEPENDENCY_NOT_SUPPORTED for them, never an empty answer
// that would read as "none".
package upstream

import (
	"context"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/application"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// TRD answers application.Orders from TRD's OrderSnapshotLookups (R46; an active grant, TRD's data
// scope): the order at its current revision, whatever its state.
type TRD struct {
	Snapshots trdapi.OrderSnapshotLookups
}

var _ application.Orders = TRD{}

// ResolveOrders asks TRD before CNX's transaction.
func (t TRD) ResolveOrders(ctx context.Context, ids []string) (map[string]application.Order, error) {
	out := map[string]application.Order{}
	if err := trdapi.ValidateOrderIDs(ids); err != nil || len(ids) == 0 {
		return out, err
	}
	facts, err := t.Snapshots.ResolveOrderSnapshots(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, f := range facts {
		o := application.Order{Binding: tcc.Binding{OrderID: f.Binding.OrderID, OrderRevision: f.Binding.OrderRevision, OrderHash: f.Binding.OrderHash},
			Status: f.Status, Code: f.Code, CustomerID: f.CustomerRef.PartyID}
		for _, a := range f.Addresses {
			o.Addresses = append(o.Addresses, application.OrderAddress{Role: a.Role, PartyID: a.PartyID, PartyRevision: a.PartyRevision, AddressID: a.AddressID})
		}
		for _, l := range f.Lines {
			o.Lines = append(o.Lines, application.OrderLine{LineID: l.LineID, SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision})
		}
		out[id] = o
	}
	return out, nil
}

// AddressRoles is the part of PTY's ProfileQueries the countries need.
type AddressRoles interface {
	ResolveAddressRoles(ctx context.Context, refs []ptyapi.AddressRoleRef) (map[ptyapi.AddressRoleRef]ptyapi.ResolvedAddressRole, error)
}

// PTY answers application.Countries from PTY's AddressRoleRef v1 lookups (R77; an active grant,
// PTY's data scope): the role and the country the bound party revision keeps for the address,
// whether or not the party was revised since — the order binds the revision. A revision confirmed
// before PTY recorded countries has none (S5 D-8): the evaluation asks for it.
type PTY struct {
	Roles AddressRoles
}

var _ application.Countries = PTY{}

// ResolveAddressCountries asks PTY before CNX's transaction.
func (p PTY) ResolveAddressCountries(ctx context.Context, refs []application.AddressRef) (map[application.AddressRef]application.AddressCountry, error) {
	out := map[application.AddressRef]application.AddressCountry{}
	if len(refs) == 0 {
		return out, nil
	}
	asked := make([]ptyapi.AddressRoleRef, len(refs))
	for i, r := range refs {
		asked[i] = ptyapi.AddressRoleRef{PartyID: r.PartyID, PartyRevision: r.PartyRevision, AddressID: r.AddressID}
	}
	got, err := p.Roles.ResolveAddressRoles(ctx, asked)
	if err != nil {
		return nil, err
	}
	for ref, a := range got {
		out[application.AddressRef{PartyID: ref.PartyID, PartyRevision: ref.PartyRevision, AddressID: ref.AddressID}] =
			application.AddressCountry{Role: a.Role, Country: a.Country}
	}
	return out, nil
}

// Programmes is the part of reference data's ProgrammeQueries the check needs.
type Programmes interface {
	ResolveCountryProgrammes(ctx context.Context, q refapi.ResolveProgrammes) (refapi.ResolvedProgrammes, error)
}

// REF answers application.Programmes from reference data's ResolveCountryProgrammes (the current
// version of each list, an active grant).
type REF struct {
	Programmes Programmes
}

var _ application.Programmes = REF{}

// ResolveProgrammes asks reference data before CNX's transaction.
func (r REF) ResolveProgrammes(ctx context.Context, lists []string, countries []string) (application.ProgrammeAnswer, error) {
	out := application.ProgrammeAnswer{Versions: map[string]tcc.ListVersion{}}
	if len(lists) == 0 {
		return out, nil
	}
	q := refapi.ResolveProgrammes{Countries: countries}
	for _, code := range lists {
		q.Lists = append(q.Lists, refapi.ListVersionRef{ListCode: code})
	}
	got, err := r.Programmes.ResolveCountryProgrammes(ctx, q)
	if err != nil {
		return application.ProgrammeAnswer{}, err
	}
	for code, v := range got.Versions {
		out.Versions[code] = tcc.ListVersion{ListCode: v.ListCode, Jurisdiction: v.Jurisdiction, Version: v.Version, ContentHash: v.ContentHash}
	}
	for _, h := range got.Hits {
		out.Hits = append(out.Hits, tcc.ProgrammeHit{ListCode: h.ListCode, Jurisdiction: h.Jurisdiction, Version: h.Version, Country: h.Country,
			ProgrammeID: h.Programme.ProgrammeID, Name: h.Programme.Name, Measure: h.Programme.Measure, Scope: h.Programme.Scope,
			ContentHash: h.Programme.ContentHash})
	}
	return out, nil
}

// NotWired answers the port whose upstream contract is not in this build (CAT's declaration
// profiles, S5 A5) with DEPENDENCY_NOT_SUPPORTED, naming the contract.
type NotWired struct{}

var _ application.Profiles = NotWired{}

// ResolveProfiles answers that CAT's DeclarationProfileRef v1 is not wired.
func (NotWired) ResolveProfiles(context.Context, []application.SpecRef) (map[application.SpecRef]tcc.Profile, error) {
	return nil, api.Reject(api.CodeDependencyNotSupported, "/", "CAT DeclarationProfileRef v1 is not wired in this build")
}
