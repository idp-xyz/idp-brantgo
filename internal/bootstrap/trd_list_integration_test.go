//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// quote.list and order.list (S7-06 read models): a page of what the caller sees, most recently
// changed first. A quote is seen through its root's owner and an order through its quote, as
// quote.get and order.get decide it. The status of a quote is its latest revision's.

func TestQuoteAndOrderListsFollowTheDataScope(t *testing.T) {
	q := newOrderWorld(t)
	reader := func(company, id string, ds authorization.DataScope, team string) context.Context {
		return q.provision(bootstrap.Grant{Scope: q.scope(company), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id},
			Permissions: allPermissions, DataScope: ds, Team: team})
	}
	lead := reader("company.A", "list.lead", authorization.DataScopeCompany, "")
	own := reader("company.A", "list.own", "own", "")
	team := reader("company.A", "list.team", authorization.DataScopeTeam, "no.such.team")
	other := reader("company.B", "list.other", authorization.DataScopeCompany, "")

	q.drafted("quote.draft")
	q.frozenQuote("quote.frozen")
	q.approved("quote.pending")
	q.activeOrder("sold")

	quotes := func(ctx context.Context, query trdapi.QuoteListQuery) trdapi.QuotePage {
		t.Helper()
		p, err := q.c.TRD.ListQuotes(ctx, query)
		q.must(err)
		return p
	}
	orders := func(ctx context.Context, query trdapi.OrderListQuery) trdapi.OrderPage {
		t.Helper()
		p, err := q.c.TRD.ListOrders(ctx, query)
		q.must(err)
		return p
	}
	quoteIDs := func(p trdapi.QuotePage) []string {
		ids := []string{}
		for _, r := range p.Quotes {
			ids = append(ids, r.QuoteID)
		}
		return ids
	}

	all := quotes(lead, trdapi.QuoteListQuery{})
	if got := quoteIDs(all); !slices.Equal(got, []string{"sold", "quote.pending", "quote.frozen", "quote.draft"}) || all.Next != "" {
		t.Fatalf("company scope sees %v next %q", got, all.Next)
	}
	byID := map[string]trdapi.QuoteSummary{}
	for _, r := range all.Quotes {
		byID[r.QuoteID] = r
	}
	if r := byID["quote.draft"]; r.Status != "working" || r.LastRevision != 1 || r.ActiveRevision != 0 || r.OpenReviewID != "" ||
		r.CustomerID != "customer.1" || r.RfqID != "rfq.1" || r.OwnerActorID == "" || r.UpdatedAt.IsZero() || r.RootVersion != 1 {
		t.Fatalf("draft %+v", r)
	}
	if r := byID["quote.frozen"]; r.Status != "frozen" {
		t.Fatalf("frozen %+v", r)
	}
	if r := byID["sold"]; r.Status != "active" || r.ActiveRevision != 1 {
		t.Fatalf("active %+v", r)
	}
	// The owner sees its quotes whatever its data scope; a caller that owns nothing sees none.
	if got := quoteIDs(quotes(q.author, trdapi.QuoteListQuery{})); len(got) != 4 {
		t.Fatalf("the owner sees %v", got)
	}
	for name, ctx := range map[string]context.Context{"own": own, "team": team, "other company": other} {
		if got := quoteIDs(quotes(ctx, trdapi.QuoteListQuery{})); len(got) != 0 {
			t.Fatalf("%s sees quotes %v", name, got)
		}
		if got := orders(ctx, trdapi.OrderListQuery{}).Orders; len(got) != 0 {
			t.Fatalf("%s sees orders %+v", name, got)
		}
	}

	// Filters are on the latest revision.
	if got := quoteIDs(quotes(lead, trdapi.QuoteListQuery{Status: "working"})); !slices.Equal(got, []string{"quote.draft"}) {
		t.Fatalf("status filter %v", got)
	}
	if got := quoteIDs(quotes(lead, trdapi.QuoteListQuery{CustomerID: "customer.1", RfqID: "rfq.1"})); len(got) != 4 {
		t.Fatalf("customer and rfq filter %v", got)
	}
	if got := quoteIDs(quotes(lead, trdapi.QuoteListQuery{CustomerID: "customer.none"})); len(got) != 0 {
		t.Fatalf("another customer %v", got)
	}

	// Pages of three, then the rest.
	first := quotes(lead, trdapi.QuoteListQuery{Limit: 3})
	rest := quotes(lead, trdapi.QuoteListQuery{Limit: 3, After: first.Next})
	if got := append(quoteIDs(first), quoteIDs(rest)...); first.Next == "" || rest.Next != "" ||
		!slices.Equal(got, []string{"sold", "quote.pending", "quote.frozen", "quote.draft"}) {
		t.Fatalf("pages %v, next %q then %q", got, first.Next, rest.Next)
	}

	sold := orders(lead, trdapi.OrderListQuery{})
	if len(sold.Orders) != 1 || sold.Next != "" {
		t.Fatalf("orders %+v", sold)
	}
	if o := sold.Orders[0]; o.OrderID != "order.sold" || o.QuoteID != "sold" || o.Status != "active" || o.OpenReviewID != "" ||
		o.RowVersion != q.orderVersion("order.sold") || o.UpdatedAt.IsZero() {
		t.Fatalf("order %+v", o)
	}
	if got := orders(lead, trdapi.OrderListQuery{Status: "draft"}).Orders; len(got) != 0 {
		t.Fatalf("draft orders %+v", got)
	}
	if got := orders(lead, trdapi.OrderListQuery{QuoteID: "sold", Status: "active"}).Orders; len(got) != 1 {
		t.Fatalf("orders of the quote %+v", got)
	}
	if got := orders(lead, trdapi.OrderListQuery{QuoteID: "quote.draft"}).Orders; len(got) != 0 {
		t.Fatalf("orders of a draft quote %+v", got)
	}

	for _, bad := range []trdapi.QuoteListQuery{{Status: "open"}, {After: "not-a-cursor"}, {Limit: kernel.MaxListPage + 1}, {RfqID: "has space"}} {
		_, err := q.c.TRD.ListQuotes(lead, bad)
		q.want(err, kernel.CodeSchemaInvalid)
	}
	_, err := q.c.TRD.ListOrders(lead, trdapi.OrderListQuery{Status: "frozen"})
	q.want(err, kernel.CodeSchemaInvalid)
	_, err = q.c.TRD.ListQuotes(q.actor("list.none"), trdapi.QuoteListQuery{})
	q.want(err, kernel.CodePermissionDenied)
	if !q.audited(trdapi.QueryQuoteList, "", "read", "") || !q.audited(trdapi.QueryOrderList, "", "rejected", kernel.CodeSchemaInvalid) {
		t.Fatal("the list reads are not in the audit trail")
	}
}
