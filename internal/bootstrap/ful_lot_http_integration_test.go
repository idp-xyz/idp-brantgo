//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5-03b over HTTP with OIDC access tokens, on the composed process: the merchandiser plans the
// order's milestones, records the customs release as an internal verification with evidence and
// reconciles it, registers an export lot of a shipment confirmed on the FUL world, and reads the
// plan and the lot back in their wire form; an unknown lot is 404, a malformed request 422.
func TestMilestonesAndExportLotsOverHTTP(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "100")
	d := w.delivered("commitment.1", "100")
	_, err := w.prepare(w.merch, "shipment.1", "40", d, rel)
	w.must(err)
	_, err = w.confirm(w.merch, "shipment.1", 1)
	w.must(err)
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	merch := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "merch.1", "human")}

	p := merch.command("ful", fulapi.Contract, fulapi.CommandPlanMilestones, map[string]any{"plan_id": "plan.http", "expected_version": 0,
		"subject": map[string]any{"kind": fulapi.SubjectOrder, "id": w.order.ObjectID}, "nodes": []any{
			map[string]any{"node_id": "release", "node_type": fulapi.NodeCustomsRelease, "planned_on": day(5),
				"responsible": map[string]any{"kind": fulapi.ResponsibleParty, "id": "broker.1"}},
			map[string]any{"node_id": "departure", "node_type": fulapi.NodeActualDeparture, "planned_on": day(6), "predecessors": []any{"release"}}}})
	if p["status"] != fulapi.PlanActive || p["row_version"] != float64(1) {
		t.Fatalf("plan %v", p)
	}
	record := map[string]any{"contract": fulapi.Contract, "command": fulapi.CommandRecordMilestoneActual, "idempotency_key": "ful.http.release",
		"args": map[string]any{"plan_id": "plan.http", "expected_version": 1, "node_id": "release", "evidence_level": fulapi.LevelInternalVerified,
			"occurred_at": kernel.FormatTimestamp(time.Now().Add(-time.Hour)), "evidence_ids": []any{"evidence"}, "note": "报关行回执：海关已放行"}}
	status, a := merch.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandRecordMilestoneActual, record)
	if status != http.StatusOK || a["row_version"] != float64(2) {
		t.Fatalf("actual %d %v", status, a)
	}
	status, rec := merch.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandRecordMilestoneActual+"/reconcile", record)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != a["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	status, out := merch.try("ful", fulapi.Contract, fulapi.CommandRecordMilestoneActual, map[string]any{"plan_id": "plan.http",
		"expected_version": 2, "node_id": "departure", "evidence_level": "rumoured", "occurred_at": kernel.FormatTimestamp(time.Now())})
	if status != http.StatusUnprocessableEntity || out["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("an unknown evidence level %d %v", status, out)
	}

	l := merch.command("ful", fulapi.Contract, fulapi.CommandRegisterExportLot, map[string]any{"export_lot_id": "lot.http", "shipment_id": "shipment.1",
		"lines": []any{map[string]any{"line_id": "E1", "shipment_line_id": "S1", "quantity": "40"}}})
	if l["status"] != fulapi.LotRegistered || l["object_id"] != "lot.http" {
		t.Fatalf("lot %v", l)
	}

	status, v := merch.do("GET", "/api/brant/v1/ful/milestone_plans/plan.http", nil)
	nodes, _ := v["nodes"].([]any)
	if status != http.StatusOK || len(nodes) != 2 {
		t.Fatalf("plan read %d %v", status, v)
	}
	release := nodes[0].(map[string]any)
	verified, _ := release["verified"].(map[string]any)
	if release["highest_level"] != fulapi.LevelInternalVerified || verified == nil || verified["note"] != "报关行回执：海关已放行" ||
		release["responsible"].(map[string]any)["id"] != "broker.1" || nodes[1].(map[string]any)["highest_level"] != nil {
		t.Fatalf("plan wire form %v", v)
	}
	status, v = merch.do("GET", "/api/brant/v1/ful/export_lots/lot.http", nil)
	lines, _ := v["lines"].([]any)
	if status != http.StatusOK || v["reconciliation"] != fulapi.LotMatched || len(lines) != 1 ||
		v["totals"].(map[string]any)["net_minor"] != shareOf(t, w.line.NetMinor, 40, 120) || lines[0].(map[string]any)["confirmed_quantity"] != "40" {
		t.Fatalf("lot wire form %d %v", status, v)
	}
	if status, out = merch.do("GET", "/api/brant/v1/ful/export_lots/lot.none", nil); status != http.StatusNotFound {
		t.Fatalf("unknown lot %d %v", status, out)
	}
}
