//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// rfq.list (S7-06 read models): a page of the RFQs the caller sees, most recently changed first.
// The data scope is part of the query: own sees its own drafts, team its team's, company all of
// the company's, and nobody another company's. Filters narrow, pages do not overlap and together
// are the whole list, and the read is audited.

func rfqIDs(p inqapi.RfqPage) []string {
	ids := []string{}
	for _, r := range p.Rfqs {
		ids = append(ids, r.RfqID)
	}
	return ids
}

func TestRfqListFollowsTheDataScope(t *testing.T) {
	w := newWorld(t)
	human := func(company, id, dataScope, team string) context.Context {
		return w.provision(bootstrap.Grant{Scope: w.scope(company), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id},
			Permissions: allPermissions, DataScope: authorization.DataScope(dataScope), Team: team})
	}
	lead := human("company.A", "lead", "company", "")
	ann := human("company.A", "ann", "team", "east")
	bob := human("company.A", "bob", "team", "east")
	cat := human("company.A", "cat", "own", "west")
	other := human("company.B", "dan", "company", "")

	draft := func(ctx context.Context, id string) {
		t.Helper()
		_, err := w.save(ctx, inqapi.SaveRfq{RfqID: id, Lines: []inqapi.RfqLine{{LineID: "L1", RequestedItem: "待核实商品"}}})
		w.must(err)
	}
	draft(ann, "rfq.ann")
	draft(bob, "rfq.bob")
	draft(cat, "rfq.cat")
	draft(other, "rfq.dan")

	list := func(ctx context.Context, q inqapi.RfqQuery) inqapi.RfqPage {
		t.Helper()
		p, err := w.c.INQ.ListRfqs(ctx, q)
		w.must(err)
		return p
	}
	// Most recently changed first.
	if got := rfqIDs(list(lead, inqapi.RfqQuery{})); !slices.Equal(got, []string{"rfq.cat", "rfq.bob", "rfq.ann"}) {
		t.Fatalf("company scope sees %v", got)
	}
	if got := rfqIDs(list(ann, inqapi.RfqQuery{})); !slices.Equal(got, []string{"rfq.bob", "rfq.ann"}) {
		t.Fatalf("team scope sees %v", got)
	}
	if got := rfqIDs(list(cat, inqapi.RfqQuery{})); !slices.Equal(got, []string{"rfq.cat"}) {
		t.Fatalf("own scope sees %v", got)
	}
	if got := rfqIDs(list(other, inqapi.RfqQuery{})); !slices.Equal(got, []string{"rfq.dan"}) {
		t.Fatalf("the other company sees %v", got)
	}

	first := list(lead, inqapi.RfqQuery{})
	if r := first.Rfqs[0]; r.Status != inqapi.StatusDraft || r.RowVersion != 1 || r.OwnerActorID != "cat" || r.UpdatedAt.IsZero() || first.Next != "" {
		t.Fatalf("summary %+v next %q", r, first.Next)
	}

	// A write moves the RFQ to the front.
	_, err := w.save(ann, inqapi.SaveRfq{RfqID: "rfq.ann", ExpectedVersion: 1, Lines: []inqapi.RfqLine{{LineID: "L1", RequestedItem: "已改"}}})
	w.must(err)
	if got := rfqIDs(list(lead, inqapi.RfqQuery{})); !slices.Equal(got, []string{"rfq.ann", "rfq.cat", "rfq.bob"}) {
		t.Fatalf("after a write %v", got)
	}

	// Pages of one: no overlap, nothing missing, and the last has no next.
	var paged []string
	q := inqapi.RfqQuery{Limit: 1}
	for range 4 {
		p := list(lead, q)
		paged = append(paged, rfqIDs(p)...)
		if q.After = p.Next; q.After == "" {
			break
		}
	}
	if !slices.Equal(paged, []string{"rfq.ann", "rfq.cat", "rfq.bob"}) {
		t.Fatalf("paged %v", paged)
	}
	// A cursor is a position, not a grant: the own-scope caller pages only through what it sees.
	if got := rfqIDs(list(cat, inqapi.RfqQuery{After: list(lead, inqapi.RfqQuery{Limit: 1}).Next})); !slices.Equal(got, []string{"rfq.cat"}) {
		t.Fatalf("own scope after the lead's cursor %v", got)
	}

	// Filters.
	ready := w.readyArgs(lead)
	saved, err := w.save(lead, ready)
	w.must(err)
	_, err = w.confirm(lead, "rfq.1", saved.RowVersion, "src.1")
	w.must(err)
	if got := rfqIDs(list(lead, inqapi.RfqQuery{Status: inqapi.StatusReady})); !slices.Equal(got, []string{"rfq.1"}) {
		t.Fatalf("status filter %v", got)
	}
	if got := rfqIDs(list(lead, inqapi.RfqQuery{CustomerID: "customer.1"})); !slices.Equal(got, []string{"rfq.1"}) {
		t.Fatalf("customer filter %v", got)
	}
	if got := rfqIDs(list(lead, inqapi.RfqQuery{Status: inqapi.StatusDraft, CustomerID: "customer.1"})); len(got) != 0 {
		t.Fatalf("both filters %v", got)
	}

	// Refusals are audited like any other read's.
	for _, bad := range []inqapi.RfqQuery{{Status: "open"}, {After: "not-a-cursor"}, {Limit: kernel.MaxListPage + 1}, {CustomerID: "has space"}} {
		_, err = w.c.INQ.ListRfqs(lead, bad)
		w.want(err, kernel.CodeSchemaInvalid)
	}
	reader := w.actor("no.read")
	_, err = w.c.INQ.ListRfqs(reader, inqapi.RfqQuery{})
	w.want(err, kernel.CodePermissionDenied)
	trail, err := w.c.INQ.Audit(lead)
	w.must(err)
	var lists, refused int
	for _, e := range trail {
		if e.Command == "rfq.list" {
			lists++
			if e.Code == kernel.CodeSchemaInvalid {
				refused++
			}
		}
	}
	if lists == 0 || refused != 4 {
		t.Fatalf("audit: %d rfq.list entries, %d refused", lists, refused)
	}
}
