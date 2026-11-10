//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"testing"

	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
)

// assign gives a human actor of company.A roles with company data scope and returns its context.
func (w *world) assign(id string, roles ...string) context.Context {
	w.t.Helper()
	actor := kernel.ActorRef{Kind: kernel.ActorHuman, ID: id}
	if _, err := w.platform.AssignRoles(context.Background(), w.scope("company.A"), actor, roles, authorization.DataScopeCompany, ""); err != nil {
		w.t.Fatal(err)
	}
	ctx, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: actor, Scope: w.scope("company.A")})
	w.must(err)
	return ctx
}

// BC-D02 end to end: the front of BRC-001 with role templates instead of hand-picked permissions.
// The purchaser drafts SKUs, the sales clerk drafts the customer and runs the RFQ, the
// administrator confirms master data; each is refused what its job does not include.
func TestBRC001FrontWithRoleTemplates(t *testing.T) {
	w := newWorld(t)
	sales, purchaser, admin := w.assign("sales.1", "sales_clerk"), w.assign("purchaser.1", "purchaser"), w.assign("admin.1", "administrator")
	w.source(sales, "source.1", "合成样例：ST01四个SKU，24、36、12、48件，合计120 PCS。")

	_, err := w.c.PTY.SaveParty(sales, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: "customer.1", Name: "customer.1", Roles: []string{ptyapi.RoleCustomer}})
	w.must(err)
	_, err = w.c.PTY.ConfirmParty(sales, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: 1, EvidenceIDs: []string{"source.1"}})
	w.want(err, kernel.CodePermissionDenied) // master data is confirmed by the administrator
	_, err = w.c.PTY.ConfirmParty(admin, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: 1, EvidenceIDs: []string{"source.1"}})
	w.must(err)
	_, err = w.c.PTY.SaveContact(sales, ptyapi.SaveContact{IdempotencyKey: w.key(), ContactID: "contact.1", Name: "contact.1", PartyID: "customer.1"})
	w.must(err)
	_, err = w.c.PTY.ConfirmContact(admin, ptyapi.ConfirmContact{IdempotencyKey: w.key(), ContactID: "contact.1", ExpectedVersion: 1, EvidenceIDs: []string{"source.1"}})
	w.must(err)

	_, err = w.c.CAT.SaveSku(sales, catapi.SaveSku{IdempotencyKey: w.key(), SkuID: "ST01.red.S", Name: "x", BaseUnit: catapi.UnitPCS})
	w.want(err, kernel.CodePermissionDenied) // SKUs are the purchaser's drafts
	cmd := inqapi.SaveRfq{RfqID: "rfq.1", CustomerID: "customer.1", ContactID: "contact.1", SourceIDs: []string{"source.1"}}
	for i, s := range []string{"ST01.red.S", "ST01.red.M", "ST01.blue.S", "ST01.blue.M"} {
		_, err = w.c.CAT.SaveSku(purchaser, catapi.SaveSku{IdempotencyKey: w.key(), SkuID: s, Name: s, BaseUnit: catapi.UnitPCS})
		w.must(err)
		_, err = w.c.CAT.ConfirmSku(admin, catapi.ConfirmSku{IdempotencyKey: w.key(), SkuID: s, ExpectedVersion: 1, EvidenceIDs: []string{"source.1"}})
		w.must(err)
		n := string(rune('1' + i))
		cmd.Lines = append(cmd.Lines, inqapi.RfqLine{LineID: "L" + n, SkuID: s, Quantity: []string{"24", "36", "12", "48"}[i], UOM: "PCS",
			SourceID: "source.1", Locator: "第1段，第" + n + "项"})
	}
	_, err = w.save(sales, cmd)
	w.must(err)
	_, err = w.confirm(admin, "rfq.1", 1, "source.1")
	w.want(err, kernel.CodePermissionDenied) // the administrator does not run sales work
	r, err := w.confirm(sales, "rfq.1", 1, "source.1")
	w.must(err)
	if r.Status != inqapi.StatusReady {
		t.Fatalf("confirm %+v", r)
	}
}

// Separation of duties through the operator path: sales may not hold a credit decision role.
func TestSalesCannotHoldCreditDecisions(t *testing.T) {
	w := newWorld(t)
	_, err := w.platform.AssignRoles(context.Background(), w.scope("company.A"), kernel.ActorRef{Kind: kernel.ActorHuman, ID: "sales.1"},
		[]string{"sales_clerk", "credit_manager"}, "", "")
	if !errors.Is(err, authorization.ErrSeparationOfDuties) {
		t.Fatalf("want ErrSeparationOfDuties, got %v", err)
	}
	if _, err = w.platform.DefineRole(context.Background(), w.scope("company.A"), "sales.and.review", "销售兼审核", []string{"sales_clerk", "order_reviewer"}); !errors.Is(err, authorization.ErrSeparationOfDuties) {
		t.Fatalf("a tenant role merging sales and order review: %v", err)
	}
}
