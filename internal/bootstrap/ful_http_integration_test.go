//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5-03a over HTTP with OIDC access tokens, on the composed process (bootstrap's Contexts wires FUL
// to PUR's real supplier deliveries): a shipment prepared and confirmed on the FUL world's in-memory
// delivery line reads back in its wire form; the merchandiser plans its sibling's packing and
// cancels it over HTTP, and the plan reconciles; on that in-memory delivery line, which PUR does not
// have, the composed shipment.prepare is 422 REFERENCE_INVALID and shipment.confirm 409
// SUPPLIER_DELIVERY_UNVERIFIED (SupplierDeliveryGuard v1's answer); the order's fulfilment and FUL's
// audit trail read back. TestShipmentOnDeliveryVerifiedOverHTTP runs on a real delivery.
func TestShipmentsOverHTTP(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "100")
	d := w.delivered("commitment.1", "100")
	_, err := w.prepare(w.merch, "shipment.1", "40", d, rel)
	w.must(err)
	w.granted("shipment.1")
	_, err = w.confirm(w.merch, "shipment.1", 1)
	w.must(err)
	_, err = w.prepare(w.merch, "shipment.2", "40", d, rel)
	w.must(err)
	w.assign("admin.1", "administrator")
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	merch := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "merch.1", "human")}
	admin := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "admin.1", "human"), n: 300}

	status, v := merch.do("GET", "/api/brant/v1/ful/shipments/shipment.1", nil)
	lines, _ := v["lines"].([]any)
	if status != http.StatusOK || v["status"] != fulapi.StatusConfirmed || len(lines) != 1 {
		t.Fatalf("shipment %d %v", status, v)
	}
	line := lines[0].(map[string]any)
	delivery := line["supplier_delivery"].(map[string]any)
	confirmation := v["confirmation"].(map[string]any)
	if line["quantity"] != "40" || line["quality_release_id"] != rel || delivery["delivery_id"] != d.DeliveryID || delivery["version"] != float64(1) ||
		confirmation["unchecked_guards"] != nil || confirmation["confirmed_by"].(map[string]any)["id"] != "merch.1" {
		t.Fatalf("wire form %v", v)
	}

	_, err = w.prepare(w.merch, "shipment.3", "10", d, rel)
	w.must(err)
	status, out := merch.try("ful", fulapi.Contract, fulapi.CommandPrepareShipment, map[string]any{"shipment_id": "shipment.http",
		"order_id": w.order.ObjectID, "route": fulapi.RouteSupplierDirect, "lines": []any{map[string]any{"line_id": "S1", "order_line_id": "L1",
			"quantity": "10", "supplier_delivery": map[string]any{"delivery_id": d.DeliveryID, "line_id": d.LineID, "version": d.Version},
			"quality_release_id": rel}}})
	if status != http.StatusUnprocessableEntity || out["code"] != kernel.CodeReferenceInvalid {
		t.Fatalf("prepare on a delivery line PUR does not have %d %v", status, out)
	}
	status, out = merch.try("ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": "shipment.3",
		"expected_version": 1})
	if status != http.StatusConflict || out["code"] != fulapi.CodeSupplierDeliveryUnverified {
		t.Fatalf("confirm on a delivery line SupplierDeliveryGuard v1 does not know %d %v", status, out)
	}

	plan := map[string]any{"contract": fulapi.Contract, "command": fulapi.CommandPlanPacking, "idempotency_key": "ful.http.plan",
		"args": map[string]any{"shipment_id": "shipment.2", "expected_version": 1, "packages": []any{map[string]any{"cartons": 4,
			"contents": []any{map[string]any{"line_id": "S1", "quantity_per_carton": "10"}}}}}}
	status, p := merch.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandPlanPacking, plan)
	if status != http.StatusOK || p["row_version"] != float64(2) || p["status"] != fulapi.StatusPrepared {
		t.Fatalf("plan %d %v", status, p)
	}
	status, rec := merch.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandPlanPacking+"/reconcile", plan)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != p["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	c := merch.command("ful", fulapi.Contract, fulapi.CommandCancelShipment, map[string]any{"shipment_id": "shipment.2", "expected_version": 2,
		"reason": "客户推迟出运"})
	if c["status"] != fulapi.StatusCancelled || c["row_version"] != float64(3) {
		t.Fatalf("cancel %v", c)
	}

	status, f := merch.do("GET", "/api/brant/v1/ful/orders/"+w.order.ObjectID+"/fulfilment", nil)
	fl, _ := f["lines"].([]any)
	if status != http.StatusOK || len(fl) != 1 || fl[0].(map[string]any)["shipped"] != "40" || fl[0].(map[string]any)["prepared"] != "10" ||
		len(f["shipments"].([]any)) != 3 {
		t.Fatalf("fulfilment %d %v", status, f)
	}
	if status, out = merch.do("GET", "/api/brant/v1/ful/shipments/shipment.none", nil); status != http.StatusNotFound {
		t.Fatalf("unknown shipment %d %v", status, out)
	}
	status, audit := admin.do("GET", "/api/brant/v1/ful/audit", nil)
	if status != http.StatusOK || len(audit["entries"].([]any)) == 0 {
		t.Fatalf("audit %d %v", status, audit)
	}
}
