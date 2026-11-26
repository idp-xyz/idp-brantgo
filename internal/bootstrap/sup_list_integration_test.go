//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// supplier_rfq.list and supplier_offer.list (S7-06 read models): a page of the supplier RFQs and
// the supplier offers the caller sees, most recently changed first, narrowed by the customer RFQ
// and the supplier their content names. An offer is listed with its latest revision's status and
// the revision in force. The data scope is part of the query; a refused query is audited.
func TestSourcingListsNarrowByRfqAndFollowTheDataScope(t *testing.T) {
	s := newSupWorld(t)
	request := supapi.SupplierRfq{Supplier: s.offer().Supplier, Rfq: s.offer().Rfq, RfqLineIDs: []string{"L1"}, Evidence: evidenceOf("line 1")}
	for _, id := range []string{"request.1", "request.2"} {
		_, err := s.c.SUP.SaveSupplierRfq(s.author, supapi.SaveSupplierRfq{IdempotencyKey: s.key(), SupplierRfqID: id, Data: request})
		s.must(err)
	}
	_, err := s.c.SUP.RecordSupplierRfqIssued(s.author, supapi.RecordSupplierRfqIssued{IdempotencyKey: s.key(), SupplierRfqID: "request.1",
		ExpectedVersion: 1, Reason: "已发出", Evidence: evidenceOf("发送记录")})
	s.must(err)

	s.verified(supapi.KindSupplierOffer, "offer.1", s.offer())
	_, err = s.save(s.author, supapi.KindSupplierOffer, "offer.2", 0, s.offer())
	s.must(err)
	s.verified(supapi.KindLogisticsOffer, "log.1", s.logistics()) // another kind, never listed

	requests := func(ctx context.Context, q supapi.SupplierRfqQuery) []string {
		t.Helper()
		p, err := s.c.SUP.ListSupplierRfqs(ctx, q)
		s.must(err)
		ids := []string{}
		for _, r := range p.SupplierRfqs {
			ids = append(ids, r.SupplierRfqID)
		}
		return ids
	}
	offers := func(ctx context.Context, q supapi.OfferQuery) []string {
		t.Helper()
		p, err := s.c.SUP.ListOffers(ctx, q)
		s.must(err)
		ids := []string{}
		for _, r := range p.Offers {
			ids = append(ids, r.OfferID)
		}
		return ids
	}

	if got := requests(s.author, supapi.SupplierRfqQuery{}); !slices.Equal(got, []string{"request.1", "request.2"}) {
		t.Fatalf("supplier RFQs %v", got)
	}
	if got := requests(s.author, supapi.SupplierRfqQuery{RfqID: "rfq.1", SupplierID: "supplier.1", Status: supapi.StatusIssued}); !slices.Equal(got, []string{"request.1"}) {
		t.Fatalf("issued supplier RFQs of rfq.1 %v", got)
	}
	if got := requests(s.author, supapi.SupplierRfqQuery{RfqID: "rfq.none"}); len(got) != 0 {
		t.Fatalf("supplier RFQs of another RFQ %v", got)
	}
	if got := requests(s.author, supapi.SupplierRfqQuery{SupplierID: "provider.1"}); len(got) != 0 {
		t.Fatalf("supplier RFQs of another supplier %v", got)
	}
	page, err := s.c.SUP.ListSupplierRfqs(s.author, supapi.SupplierRfqQuery{Limit: 1})
	s.must(err)
	if r := page.SupplierRfqs[0]; page.Next == "" || r.RfqID != "rfq.1" || r.SupplierID != "supplier.1" || r.Lines != 1 || r.Status != supapi.StatusIssued || r.RowVersion != 2 {
		t.Fatalf("supplier RFQ page %+v", page)
	}
	if got := requests(s.author, supapi.SupplierRfqQuery{Limit: 1, After: page.Next}); !slices.Equal(got, []string{"request.2"}) {
		t.Fatalf("second page %v", got)
	}

	if got := offers(s.author, supapi.OfferQuery{}); !slices.Equal(got, []string{"offer.2", "offer.1"}) {
		t.Fatalf("offers %v", got)
	}
	if got := offers(s.author, supapi.OfferQuery{RfqID: "rfq.1", SupplierID: "supplier.1", Status: supapi.StatusActive}); !slices.Equal(got, []string{"offer.1"}) {
		t.Fatalf("active offers of rfq.1 %v", got)
	}
	if got := offers(s.author, supapi.OfferQuery{Status: supapi.StatusDraft}); !slices.Equal(got, []string{"offer.2"}) {
		t.Fatalf("draft offers %v", got)
	}
	if got := offers(s.author, supapi.OfferQuery{RfqID: "rfq.none"}); len(got) != 0 {
		t.Fatalf("offers of another RFQ %v", got)
	}
	op, err := s.c.SUP.ListOffers(s.author, supapi.OfferQuery{Status: supapi.StatusActive})
	s.must(err)
	if o := op.Offers[0]; o.ActiveRevision != 1 || o.LastRevision != 1 || o.RootVersion != 2 || o.Lines != 1 || o.SupplierRfqID != "" ||
		!o.ValidFrom.Equal(s.win.from) || !o.ValidUntil.Equal(s.win.until) || op.Next != "" {
		t.Fatalf("active offer %+v", op)
	}
	// A revision of the active offer: the list shows the draft, and still names the revision in force.
	_, err = s.revise(s.author, supapi.KindSupplierOffer, "offer.1", 2, 1)
	s.must(err)
	op, err = s.c.SUP.ListOffers(s.author, supapi.OfferQuery{Limit: 1})
	s.must(err)
	if o := op.Offers[0]; o.OfferID != "offer.1" || o.Status != supapi.StatusDraft || o.LastRevision != 2 || o.ActiveRevision != 1 || op.Next == "" {
		t.Fatalf("revised offer %+v", op)
	}

	// Data scope: an actor of the company who sees only their own, and another company.
	own := s.provision(bootstrap.Grant{Scope: s.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "sup.own"},
		Permissions: allPermissions, DataScope: "own", Team: "west"})
	outside := s.provision(bootstrap.Grant{Scope: s.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "sup.outside"},
		Permissions: allPermissions, DataScope: "company"})
	for _, ctx := range []context.Context{own, outside} {
		if got := requests(ctx, supapi.SupplierRfqQuery{}); len(got) != 0 {
			t.Fatalf("an actor without sight lists supplier RFQs %v", got)
		}
		if got := offers(ctx, supapi.OfferQuery{}); len(got) != 0 {
			t.Fatalf("an actor without sight lists offers %v", got)
		}
	}

	// Refusals.
	for _, bad := range []supapi.OfferQuery{{Status: "retired"}, {RfqID: "has space"}, {SupplierID: "a b"}, {After: "x"}, {Limit: kernel.MaxListPage + 1}} {
		_, err = s.c.SUP.ListOffers(s.author, bad)
		s.want(err, kernel.CodeSchemaInvalid)
	}
	_, err = s.c.SUP.ListSupplierRfqs(s.author, supapi.SupplierRfqQuery{Status: supapi.StatusActive})
	s.want(err, kernel.CodeSchemaInvalid)
	none := s.actor("sup.none")
	_, err = s.c.SUP.ListOffers(none, supapi.OfferQuery{})
	s.want(err, kernel.CodePermissionDenied)
	_, err = s.c.SUP.ListSupplierRfqs(none, supapi.SupplierRfqQuery{})
	s.want(err, kernel.CodePermissionDenied)
	trail, err := s.c.SUP.Audit(s.author)
	s.must(err)
	refused := 0
	for _, e := range trail {
		if e.Command == "supplier_offer.list" && e.Code == kernel.CodeSchemaInvalid {
			refused++
		}
	}
	if refused != 5 {
		t.Fatalf("%d refused supplier_offer.list entries in the audit trail", refused)
	}
}
