// Package upstream implements CNX's ports (application.Orders, Countries, Profiles, Programmes) on
// the upstream contexts' api packages. None of them calls another context's transaction owner:
// each lookup runs before CNX's transaction in the upstream's own.
//
// TRD's order snapshots (OrderCommercialSnapshot v1 / SalesOrderRef v1, S5 A2), PTY's address
// countries (AddressRoleRef v1 with the country, S5 A3b), reference data's country programme lists
// (S5 A3b) and CAT's declaration profiles (DeclarationProfileRef v1, S5 A5) are wired.
package upstream

import (
	"context"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
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

// CAT answers application.Profiles from CAT's DeclarationProfileLookups (R39; an active grant;
// profiles belong to the company). Each key is asked with revision 0: the revision verified now.
//
//   - A verified answer is the profile at that revision: its commodity code, export control
//     classification (not_controlled, or controlled with its list reference) and content hash.
//   - DECLARATION_PROFILE_UNVERIFIED for a profile that has only a draft is the profile unverified,
//     at its draft revision and without content (CAT never answers a candidate's content): the line
//     is "unverified" and a clear conclusion is refused with DECLARATION_PROFILE_UNVERIFIED.
//   - DECLARATION_PROFILE_UNVERIFIED because no profile has the key is absent: the line is "unknown"
//     and a clear conclusion is refused the same way.
//
// REFERENCE_STALE and REFERENCE_INVALID answer a stated revision only and cannot occur here. R39 has
// no guard (application.Profiles says what that means for an evaluation).
type CAT struct {
	Profiles catapi.DeclarationProfileLookups
}

var _ application.Profiles = CAT{}

// ResolveProfiles asks CAT before CNX's transaction, at most catapi.MaxLookup keys per lookup.
func (c CAT) ResolveProfiles(ctx context.Context, refs []application.ProfileRef) (map[application.ProfileRef]tcc.Profile, error) {
	out := map[application.ProfileRef]tcc.Profile{}
	for start := 0; start < len(refs); start += catapi.MaxLookup {
		chunk := refs[start:min(start+catapi.MaxLookup, len(refs))]
		asked := make([]catapi.DeclarationProfileRef, len(chunk))
		for i, r := range chunk {
			asked[i] = catapi.DeclarationProfileRef{SkuID: r.SkuID, SpecRevision: r.SpecRevision, CodeTableVersion: r.CodeTableVersion}
		}
		got, err := c.Profiles.ResolveDeclarationProfiles(ctx, asked)
		if err != nil {
			return nil, err
		}
		for ref, r := range got {
			key := application.ProfileRef{SkuID: ref.SkuID, SpecRevision: ref.SpecRevision, CodeTableVersion: ref.CodeTableVersion}
			switch {
			case r.Verified:
				x := r.Profile.ExportControl
				out[key] = tcc.Profile{ProfileID: r.ProfileID, Revision: r.Revision, CodeTableVersion: ref.CodeTableVersion, Verified: true,
					CommodityCode: r.Profile.Commodity.Code, ExportControl: x.Status, ListReference: x.ListReference, ContentHash: r.ContentHash}
			case r.ProfileID != "":
				out[key] = tcc.Profile{ProfileID: r.ProfileID, Revision: r.DraftRevision, CodeTableVersion: ref.CodeTableVersion}
			}
		}
	}
	return out, nil
}
