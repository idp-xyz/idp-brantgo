package party

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/lifecycle"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var at = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return "OK"
}

// AddressRole: addresses are draft content, kept in ID order; a confirmation does not touch them.
func TestAddressesArePartOfTheDraft(t *testing.T) {
	p := &Party{Record: lifecycle.New("customer", "editor", "")}
	in := []Address{{ID: "hq", Role: AddressBuyer, Text: "a"}, {ID: "ap", Role: AddressBilling, Text: "b"}}
	p.ReplaceDraft("customer", []Role{Customer}, in)
	if p.Addresses[0].ID != "ap" || p.Addresses[1].ID != "hq" || in[0].ID != "hq" || p.RowVersion != 1 {
		t.Fatalf("addresses %+v (input %+v)", p.Addresses, in)
	}
	p.Move(lifecycle.Confirmed)
	if len(p.Addresses) != 2 || p.Revision != 1 {
		t.Fatalf("confirmed %+v", p)
	}
	p.ReplaceDraft("customer", []Role{Customer}, nil)
	if len(p.Addresses) != 0 {
		t.Fatalf("omitted addresses are none: %+v", p.Addresses)
	}
}

// ServicePartyRole (T02 spec 13.1): only a confirmed supplier holds a service role; a validity that
// has ended verifies nothing; each verification is the next revision of the role, bound to the
// party's confirmed revision. Touch counts the change on the party root without a new revision.
func TestServiceRoleBelongsToAConfirmedSupplier(t *testing.T) {
	supplier := &Party{Record: lifecycle.Record{ID: "forwarder", Status: lifecycle.Confirmed, RowVersion: 4, Revision: 2}, Roles: []Role{Supplier}}
	customer := &Party{Record: lifecycle.Record{ID: "customer", Status: lifecycle.Confirmed, RowVersion: 2, Revision: 1}, Roles: []Role{Customer}}
	draft := &Party{Record: lifecycle.Record{ID: "draft", Status: lifecycle.Draft, RowVersion: 3, Revision: 1}, Roles: []Role{Supplier}}
	role := ServiceRole{ID: "sea", Kind: ServiceForwarder, ServiceCode: "sea.fcl", Origin: "CNSHA", Destination: "DEHAM",
		Validity: kernel.ValidityInterval{From: at.Add(-time.Hour), Until: at.Add(24 * time.Hour)}, EvidenceIDs: []string{"evidence"}}
	ended := role
	ended.Validity = kernel.ValidityInterval{From: at.Add(-48 * time.Hour), Until: at}
	inverted := role
	inverted.Validity = kernel.ValidityInterval{From: at, Until: at}
	for _, c := range []struct {
		name  string
		party *Party
		role  ServiceRole
		want  string
	}{
		{"supplier", supplier, role, "OK"},
		{"customer", customer, role, "INVALID_STATE /args/id"},
		{"draft", draft, role, "INVALID_STATE /args/id"},
		{"validity ended at the instant", supplier, ended, "INVALID_KNOWN_VALUE /args/valid_until"},
		{"empty validity", supplier, inverted, "INVALID_KNOWN_VALUE /args/valid_until"},
	} {
		_, err := c.party.VerifyServiceRole(c.role, 0, at)
		if got := code(err); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	verified, err := supplier.VerifyServiceRole(role, 3, at)
	if err != nil || verified.Revision != 4 || verified.PartyRevision != 2 {
		t.Fatalf("verified %+v %v", verified, err)
	}
	supplier.Touch()
	if supplier.RowVersion != 5 || supplier.Revision != 2 || supplier.Status != lifecycle.Confirmed {
		t.Fatalf("touch %+v", supplier.Record)
	}
}
