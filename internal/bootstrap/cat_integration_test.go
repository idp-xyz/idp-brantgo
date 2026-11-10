//go:build integration

package bootstrap_test

import (
	"context"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// sku saves a PCS draft SKU named after its ID and, unless draft, confirms it on evidence.
func (w *world) sku(ctx context.Context, id string, confirm bool, evidence ...string) catapi.Receipt {
	w.t.Helper()
	r, err := w.c.CAT.SaveSku(ctx, catapi.SaveSku{IdempotencyKey: w.key(), SkuID: id, Name: id, BaseUnit: catapi.UnitPCS})
	w.must(err)
	if confirm {
		r, err = w.c.CAT.ConfirmSku(ctx, catapi.ConfirmSku{IdempotencyKey: w.key(), SkuID: id, ExpectedVersion: 1, EvidenceIDs: evidence})
		w.must(err)
	}
	return r
}

// CAT-INV-001 and Python master kind sku: a SKU is confirmed only with a name and the PCS base unit;
// each confirmation fixes a new specification revision; a revision is never edited.
func TestSkuSpecificationRevisions(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.source(ctx, "src.1", "text")
	_, err := w.c.CAT.SaveSku(ctx, catapi.SaveSku{IdempotencyKey: w.key(), SkuID: "sku.1", Name: "ST01 red S"})
	w.must(err)
	_, err = w.c.CAT.ConfirmSku(ctx, catapi.ConfirmSku{IdempotencyKey: w.key(), SkuID: "sku.1", ExpectedVersion: 1, EvidenceIDs: []string{"src.1"}})
	if code(err) != kernel.CodeDraftNotReady || path(err) != "/args/base_uom" {
		t.Fatalf("SKU without a base unit: %v", err)
	}
	_, err = w.c.CAT.SaveSku(ctx, catapi.SaveSku{IdempotencyKey: w.key(), SkuID: "sku.1", ExpectedVersion: 1, Name: "ST01 red S", BaseUnit: "DOZEN"})
	w.want(err, kernel.CodeSchemaInvalid)
	_, err = w.c.CAT.SaveSku(ctx, catapi.SaveSku{IdempotencyKey: w.key(), SkuID: "sku.1", ExpectedVersion: 1, Name: "ST01 red S", BaseUnit: catapi.UnitPCS})
	w.must(err)
	r, err := w.c.CAT.ConfirmSku(ctx, catapi.ConfirmSku{IdempotencyKey: w.key(), SkuID: "sku.1", ExpectedVersion: 2, EvidenceIDs: []string{"src.1"}})
	w.must(err)
	refs, err := w.c.CAT.ResolveSkus(ctx, []string{"sku.1", "sku.unknown"})
	w.must(err)
	if ref := refs["sku.1"]; len(refs) != 1 || ref.SpecRevision != 1 || ref.Status != catapi.StatusConfirmed || ref.RowVersion != r.RowVersion {
		t.Fatalf("refs %+v", refs)
	}
	_, err = w.c.CAT.ReviseSpec(ctx, catapi.ReviseSpec{IdempotencyKey: w.key(), SkuID: "sku.1", ExpectedVersion: 3, Reason: "面料变更"})
	w.must(err)
	_, err = w.c.CAT.SaveSku(ctx, catapi.SaveSku{IdempotencyKey: w.key(), SkuID: "sku.1", ExpectedVersion: 4, Name: "ST01 red S v2", BaseUnit: catapi.UnitPCS})
	w.must(err)
	_, err = w.c.CAT.ConfirmSku(ctx, catapi.ConfirmSku{IdempotencyKey: w.key(), SkuID: "sku.1", ExpectedVersion: 5, EvidenceIDs: []string{"src.1"}})
	w.must(err)
	v, err := w.c.CAT.GetSku(ctx, "sku.1")
	w.must(err)
	if len(v.Revisions) != 2 || v.Revisions[0].Name != "ST01 red S" || v.Revisions[1].Revision != 2 || v.Name != "ST01 red S v2" {
		t.Fatalf("SKU %+v", v)
	}
	if err = w.exec("company.A", `UPDATE brant_cat.sku SET name = 'edited' WHERE sku_id = 'sku.1'`); err == nil {
		t.Fatal("the trigger must refuse editing a confirmed SKU")
	}
	if got := w.events("company.A", catapi.Source); len(got) != 3 || got[1].Type != catapi.EventSpecificationRevised || got[2].Payload["spec_revision"] != float64(2) {
		t.Fatalf("events %+v", got)
	}
}

func TestSkuDeactivationIsHumanOnlyAndFinal(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "ai.1"}, Permissions: allPermissions,
		DataScope: "company"})
	w.source(ctx, "src.1", "text")
	w.sku(ctx, "sku.1", true, "src.1")
	_, err := w.c.CAT.DeactivateSku(ai, catapi.DeactivateSku{IdempotencyKey: w.key(), SkuID: "sku.1", ExpectedVersion: 2, Reason: "停用"})
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.c.CAT.DeactivateSku(ctx, catapi.DeactivateSku{IdempotencyKey: w.key(), SkuID: "sku.1", ExpectedVersion: 2, Reason: "停用"})
	w.must(err)
	_, err = w.c.CAT.ReviseSpec(ctx, catapi.ReviseSpec{IdempotencyKey: w.key(), SkuID: "sku.1", ExpectedVersion: 3, Reason: "x"})
	w.want(err, kernel.CodeInvalidState)
}
