//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// party.list, contact.list and sku.list (S7-06 read models): a page of the master data the caller
// sees, most recently changed first. The data scope is part of the query; search matches the
// name or the ID whatever its case and treats % and _ as text; a refused query is audited.
func TestMasterDataListsFollowTheDataScope(t *testing.T) {
	w := newWorld(t)
	human := func(company, id string, ds authorization.DataScope, team string) context.Context {
		return w.provision(bootstrap.Grant{Scope: w.scope(company), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id},
			Permissions: allPermissions, DataScope: ds, Team: team})
	}
	lead := human("company.A", "md.lead", authorization.DataScopeCompany, "")
	ann := human("company.A", "md.ann", authorization.DataScopeTeam, "east")
	bob := human("company.A", "md.bob", authorization.DataScopeTeam, "east")
	cat := human("company.A", "md.cat", "own", "west")
	other := human("company.B", "md.dan", authorization.DataScopeCompany, "")

	w.source(lead, "src.1", "合成主数据证据")
	party := func(ctx context.Context, id, name string, roles ...string) {
		t.Helper()
		_, err := w.c.PTY.SaveParty(ctx, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: id, Name: name, Roles: roles})
		w.must(err)
	}
	party(ann, "p.acme", "Acme Trading GmbH", ptyapi.RoleCustomer)
	party(bob, "p.fabric", "Hangzhou Fabric 50%_Co", ptyapi.RoleSupplier)
	party(cat, "p.both", "ACME Sourcing", ptyapi.RoleCustomer, ptyapi.RoleSupplier)
	party(other, "p.outside", "Acme of another company", ptyapi.RoleCustomer)
	w.contact(ann, "c.schmidt", "p.acme", false)
	w.contact(ann, "c.weber", "p.acme", false)
	w.contact(cat, "c.li", "p.both", false)
	w.sku(ann, "sku.tee.red", false)
	w.sku(cat, "sku.tee.blue", false)
	w.sku(other, "sku.outside", false)

	parties := func(ctx context.Context, q ptyapi.PartyQuery) []string {
		t.Helper()
		p, err := w.c.PTY.ListParties(ctx, q)
		w.must(err)
		ids := []string{}
		for _, r := range p.Parties {
			ids = append(ids, r.PartyID)
		}
		return ids
	}
	if got := parties(lead, ptyapi.PartyQuery{}); !slices.Equal(got, []string{"p.both", "p.fabric", "p.acme"}) {
		t.Fatalf("company scope sees %v", got)
	}
	if got := parties(ann, ptyapi.PartyQuery{}); !slices.Equal(got, []string{"p.fabric", "p.acme"}) {
		t.Fatalf("team scope sees %v", got)
	}
	if got := parties(cat, ptyapi.PartyQuery{}); !slices.Equal(got, []string{"p.both"}) {
		t.Fatalf("own scope sees %v", got)
	}
	if got := parties(other, ptyapi.PartyQuery{}); !slices.Equal(got, []string{"p.outside"}) {
		t.Fatalf("the other company sees %v", got)
	}
	// Search: any case, name or ID, and the caller's scope still applies.
	if got := parties(lead, ptyapi.PartyQuery{Search: "acme"}); !slices.Equal(got, []string{"p.both", "p.acme"}) {
		t.Fatalf("search by name %v", got)
	}
	if got := parties(lead, ptyapi.PartyQuery{Search: "P.FAB"}); !slices.Equal(got, []string{"p.fabric"}) {
		t.Fatalf("search by ID %v", got)
	}
	if got := parties(cat, ptyapi.PartyQuery{Search: "acme"}); !slices.Equal(got, []string{"p.both"}) {
		t.Fatalf("search inside the own scope %v", got)
	}
	// % and _ are text, not wildcards.
	if got := parties(lead, ptyapi.PartyQuery{Search: "50%_co"}); !slices.Equal(got, []string{"p.fabric"}) {
		t.Fatalf("literal %% and _ %v", got)
	}
	if got := parties(lead, ptyapi.PartyQuery{Search: "%"}); !slices.Equal(got, []string{"p.fabric"}) {
		t.Fatalf("a lone %% %v", got)
	}
	if got := parties(lead, ptyapi.PartyQuery{Search: "a_m"}); len(got) != 0 {
		t.Fatalf("_ matched a character: %v", got)
	}
	if got := parties(lead, ptyapi.PartyQuery{Role: ptyapi.RoleSupplier}); !slices.Equal(got, []string{"p.both", "p.fabric"}) {
		t.Fatalf("role filter %v", got)
	}
	if got := parties(lead, ptyapi.PartyQuery{Status: "confirmed"}); len(got) != 0 {
		t.Fatalf("status filter %v", got)
	}
	first, err := w.c.PTY.ListParties(lead, ptyapi.PartyQuery{Limit: 2})
	w.must(err)
	rest, err := w.c.PTY.ListParties(lead, ptyapi.PartyQuery{Limit: 2, After: first.Next})
	w.must(err)
	if first.Next == "" || rest.Next != "" || len(rest.Parties) != 1 || rest.Parties[0].PartyID != "p.acme" {
		t.Fatalf("pages %+v then %+v", first, rest)
	}
	if r := first.Parties[0]; r.Name != "ACME Sourcing" || r.Status != "draft" || !slices.Equal(r.Roles, []string{ptyapi.RoleCustomer, ptyapi.RoleSupplier}) ||
		r.OwnerActorID != "md.cat" || r.RowVersion != 1 || r.UpdatedAt.IsZero() {
		t.Fatalf("summary %+v", r)
	}

	contacts := func(ctx context.Context, q ptyapi.ContactQuery) []string {
		t.Helper()
		p, err := w.c.PTY.ListContacts(ctx, q)
		w.must(err)
		ids := []string{}
		for _, r := range p.Contacts {
			ids = append(ids, r.ContactID)
		}
		return ids
	}
	if got := contacts(lead, ptyapi.ContactQuery{}); !slices.Equal(got, []string{"c.li", "c.weber", "c.schmidt"}) {
		t.Fatalf("contacts %v", got)
	}
	if got := contacts(lead, ptyapi.ContactQuery{PartyID: "p.acme"}); !slices.Equal(got, []string{"c.weber", "c.schmidt"}) {
		t.Fatalf("contacts of one party %v", got)
	}
	if got := contacts(cat, ptyapi.ContactQuery{PartyID: "p.acme"}); len(got) != 0 {
		t.Fatalf("own scope sees another's contacts %v", got)
	}
	if got := contacts(lead, ptyapi.ContactQuery{Search: "WEB"}); !slices.Equal(got, []string{"c.weber"}) {
		t.Fatalf("contact search %v", got)
	}

	skus := func(ctx context.Context, q catapi.SkuQuery) []string {
		t.Helper()
		p, err := w.c.CAT.ListSkus(ctx, q)
		w.must(err)
		ids := []string{}
		for _, r := range p.Skus {
			ids = append(ids, r.SkuID)
		}
		return ids
	}
	if got := skus(lead, catapi.SkuQuery{}); !slices.Equal(got, []string{"sku.tee.blue", "sku.tee.red"}) {
		t.Fatalf("skus %v", got)
	}
	if got := skus(cat, catapi.SkuQuery{}); !slices.Equal(got, []string{"sku.tee.blue"}) {
		t.Fatalf("own scope skus %v", got)
	}
	if got := skus(other, catapi.SkuQuery{}); !slices.Equal(got, []string{"sku.outside"}) {
		t.Fatalf("the other company's skus %v", got)
	}
	if got := skus(lead, catapi.SkuQuery{Search: "RED", Status: "draft"}); !slices.Equal(got, []string{"sku.tee.red"}) {
		t.Fatalf("sku search %v", got)
	}
	page, err := w.c.CAT.ListSkus(lead, catapi.SkuQuery{Limit: 1})
	w.must(err)
	if page.Next == "" || page.Skus[0].BaseUnit != catapi.UnitPCS || page.Skus[0].OwnerActorID != "md.cat" {
		t.Fatalf("sku page %+v", page)
	}

	// Refusals.
	for _, bad := range []ptyapi.PartyQuery{{Status: "open"}, {Role: "agent"}, {Search: " "}, {Search: "a\nb"}, {After: "x"}, {Limit: kernel.MaxListPage + 1}} {
		_, err = w.c.PTY.ListParties(lead, bad)
		w.want(err, kernel.CodeSchemaInvalid)
	}
	_, err = w.c.PTY.ListContacts(lead, ptyapi.ContactQuery{PartyID: "has space"})
	w.want(err, kernel.CodeSchemaInvalid)
	_, err = w.c.CAT.ListSkus(lead, catapi.SkuQuery{Status: "retired"})
	w.want(err, kernel.CodeSchemaInvalid)
	none := w.actor("md.none")
	_, err = w.c.PTY.ListParties(none, ptyapi.PartyQuery{})
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.c.CAT.ListSkus(none, catapi.SkuQuery{})
	w.want(err, kernel.CodePermissionDenied)
	trail, err := w.c.PTY.Audit(lead)
	w.must(err)
	refused := 0
	for _, e := range trail {
		if e.Command == "party.list" && e.Code == kernel.CodeSchemaInvalid {
			refused++
		}
	}
	if refused != 6 {
		t.Fatalf("%d refused party.list entries in the audit trail", refused)
	}
}
