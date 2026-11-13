//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5 A3b, owner decision D-8 (tactical PTY-AGG-Party value object AddressCountry; Context Map R77
// PTY->CNX "AddressRoleRef v1（含国家代码）"): an address states the ISO 3166-1 alpha-2 code of its
// country, the confirmation keeps it with the revision for good, and AddressRoleRef v1 resolves
// it. An address without one stays unknown, also in the revisions confirmed before countries
// existed; a country arrives with the next confirmed revision only. Python V0.50 has no address
// country; the vectors come from the design.
func TestAddressCountryIsKeptPerConfirmedRevision(t *testing.T) {
	w := newWorld(t)
	editor, _ := w.polActors()
	buyer := ptyapi.Address{AddressID: "hq", Role: ptyapi.AddressBuyer, Address: "合成买方地址\n汉堡"}
	consignee := ptyapi.Address{AddressID: "dock", Role: ptyapi.AddressConsignee, Address: "合成收货地址", Country: "DE"}
	save := ptyapi.SaveParty{IdempotencyKey: "save.customer.1", PartyID: "customer", Name: "customer", Roles: []string{ptyapi.RoleCustomer},
		Addresses: []ptyapi.Address{buyer, consignee}}
	saved, err := w.c.PTY.SaveParty(editor, save)
	w.must(err)
	// The same request replays; the same key with a country added is another request.
	replay, err := w.c.PTY.SaveParty(editor, save)
	w.must(err)
	if !replay.Replayed || replay.ActionID != saved.ActionID {
		t.Fatalf("replay %+v", replay)
	}
	changed := save
	changed.Addresses = []ptyapi.Address{{AddressID: "hq", Role: ptyapi.AddressBuyer, Address: buyer.Address, Country: "DE"}, consignee}
	_, err = w.c.PTY.SaveParty(editor, changed)
	w.want(err, kernel.CodeIdempotencyPayloadConflict)
	misspelt := ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: "other", Name: "other", Roles: []string{ptyapi.RoleCustomer},
		Addresses: []ptyapi.Address{{AddressID: "dock", Role: ptyapi.AddressConsignee, Address: "x", Country: "UK"}}}
	_, err = w.c.PTY.SaveParty(editor, misspelt)
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/addresses/0/country" {
		t.Fatalf("an unassigned code is refused: %v", err)
	}
	_, err = w.c.PTY.ConfirmParty(editor, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "customer", ExpectedVersion: 1, EvidenceIDs: []string{"evidence"}})
	w.must(err)

	hq1 := ptyapi.AddressRoleRef{PartyID: "customer", PartyRevision: 1, AddressID: "hq"}
	dock1 := ptyapi.AddressRoleRef{PartyID: "customer", PartyRevision: 1, AddressID: "dock"}
	got, err := w.c.PTY.ResolveAddressRoles(editor, []ptyapi.AddressRoleRef{hq1, dock1})
	w.must(err)
	if got[hq1].Country != "" || got[dock1].Country != "DE" || got[dock1].Role != ptyapi.AddressConsignee || got[dock1].Code != "" {
		t.Fatalf("resolved %+v", got)
	}
	v, err := w.c.PTY.GetParty(editor, "customer")
	w.must(err)
	if !slices.Equal(v.Revisions[0].Addresses, []ptyapi.Address{consignee, buyer}) || !slices.Equal(v.Addresses, []ptyapi.Address{consignee, buyer}) {
		t.Fatalf("party %+v", v)
	}
	if err = w.exec("company.A", `UPDATE brant_pty.party_revision_address SET country = 'CN' WHERE country IS NULL`); err == nil {
		t.Fatal("a confirmed revision never gains a country")
	}

	// The buyer's country arrives with the next confirmed revision; revision 1 keeps none.
	_, err = w.c.PTY.ReviseParty(editor, ptyapi.ReviseParty{IdempotencyKey: w.key(), PartyID: "customer", ExpectedVersion: 2, Reason: "补录地址所在国"})
	w.must(err)
	located := ptyapi.Address{AddressID: "hq", Role: ptyapi.AddressBuyer, Address: buyer.Address, Country: "DE"}
	_, err = w.c.PTY.SaveParty(editor, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: "customer", ExpectedVersion: 3, Name: "customer",
		Roles: []string{ptyapi.RoleCustomer}, Addresses: []ptyapi.Address{located, consignee}})
	w.must(err)
	_, err = w.c.PTY.ConfirmParty(editor, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "customer", ExpectedVersion: 4, EvidenceIDs: []string{"evidence"}})
	w.must(err)
	hq2 := ptyapi.AddressRoleRef{PartyID: "customer", PartyRevision: 2, AddressID: "hq"}
	got, err = w.c.PTY.ResolveAddressRoles(editor, []ptyapi.AddressRoleRef{hq1, hq2})
	w.must(err)
	if got[hq1].Country != "" || got[hq1].Stale != ptyapi.StalePartyRevised || got[hq2].Country != "DE" || got[hq2].Code != "" {
		t.Fatalf("revisions %+v", got)
	}
	v, err = w.c.PTY.GetParty(editor, "customer")
	w.must(err)
	if len(v.Revisions) != 2 || v.Revisions[0].Addresses[1].Country != "" || v.Revisions[1].Addresses[1] != located {
		t.Fatalf("revisions %+v", v.Revisions)
	}

	// The database refuses a malformed code even from the schema owner.
	migrator := pgtest.MigratorPool(t)
	tx, err := migrator.Begin(context.Background())
	w.must(err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`, w.tenant)
	w.must(err)
	_, err = tx.Exec(context.Background(), `INSERT INTO brant_pty.party_revision_address (tenant_id, company_id, party_id, revision, address_id, role,
		address, country) VALUES ($1, 'company.A', 'customer', 2, 'extra', 'payer', 'x', 'de')`, w.tenant)
	if err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("a malformed country must be refused: %v", err)
	}
}
