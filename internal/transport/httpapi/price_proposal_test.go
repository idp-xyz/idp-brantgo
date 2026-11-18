package httpapi

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// fakeTRD answers the price proposal and records the query it received.
type fakeTRD struct {
	TRD
	asked []trdapi.PriceProposalQuery
}

func (f *fakeTRD) PriceProposal(_ context.Context, q trdapi.PriceProposalQuery) (trdapi.PriceProposal, error) {
	f.asked = append(f.asked, q)
	if err := q.Validate(); err != nil {
		return trdapi.PriceProposal{}, err
	}
	if q.RfqID != "rfq.1" {
		return trdapi.PriceProposal{}, kernel.Reject(kernel.CodeNotFound, "/rfq_id", "")
	}
	out := trdapi.PriceProposal{RfqID: q.RfqID, RfqRowVersion: 2, CustomerID: "customer.1", Currency: q.Currency, PricePrecision: 2,
		PricingPolicyRef: q.PricingPolicyRef, MinimumMargin: "0.25", TargetMargin: "0.3125",
		CheckedAt: time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC),
		Lines: []trdapi.ProposedLine{
			{RfqLineID: "L1", SkuID: "ST01.red.S", Quantity: "24", Unit: "PCS", Source: trdapi.PriceSourceFormula, UnitPrice: "8.00",
				PriceBasisQuantity: "1", UnitCost: "5.50", TargetMargin: "0.3125", NetMinor: "19200", CostMinor: "13200", Margin: "0.3125"},
			{RfqLineID: "L2", SkuID: "ST01.red.M", Quantity: "36", Unit: "PCS", Source: trdapi.PriceSourceList, UnitPrice: "7.00",
				PriceBasisQuantity: "1", PriceListRef: &trdapi.ConfigRef{Kind: "sales_price", ID: "price.red.M", Revision: 2},
				PriceListScope: trdapi.PriceListCustomer, NetMinor: "25200", CostMinor: "19800", Margin: "0.214285714286", UnderFloor: true},
			{RfqLineID: "L3", SkuID: "ST01.blue.S", Quantity: "12", Unit: "PCS", Source: trdapi.PriceSourceNone, Reason: trdapi.ProposalNoTargetMargin},
		}}
	if r := q.CostSnapshotRef; r != nil {
		out.CostSnapshot = &trdapi.CostQualification{SnapshotID: r.SnapshotID, ResultHash: r.ResultHash, EvidenceHash: "e", Status: "adoptable", Adoptable: true}
	}
	return out, nil
}

// The sales price and the pricing policy's target margin travel over the wire through the reflective
// mapping like every POL kind and member (S4 unit G4a; owner decision 2026-11-18): they decode, read
// back as the same request, stay absent when not stated, and the kind is in the OpenAPI document.
func TestG4AConfigDataOverHTTP(t *testing.T) {
	for kind, in := range map[string]string{
		"sales_price": `{"sku_id":"ST01.red.S","party_id":"customer.1","currency":"USD","unit":"PCS","unit_price":"7.90","price_basis_quantity":"1"}`,
		"pricing_policy": `{"name":"n","calculation_currency":"USD","minimum_margin":"0.25","quote_valid_days":7,"max_fx_age_seconds":3600,` +
			`"tax_profile":{"kind":"tax_profile","id":"tax.zero","revision":1},"target_margin":"0.3125"}`,
	} {
		d, err := configData(kind, json.RawMessage(in))
		if err != nil {
			t.Fatal(kind, err)
		}
		var a, b any
		if err = json.Unmarshal(dataJSON(d), &a); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal([]byte(in), &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s reads back as %s", kind, dataJSON(d))
		}
	}
	d, err := configData("sales_price", json.RawMessage(`{"sku_id":"ST01.red.S","currency":"USD","unit":"PCS","unit_price":"8.40","price_basis_quantity":"1"}`))
	if err != nil || d.SalesPrice == nil || d.SalesPrice.PartyID != "" || strings.Contains(string(dataJSON(d)), "party_id") {
		t.Fatalf("a general price: %s %v", dataJSON(d), err)
	}
	d, err = configData("pricing_policy", json.RawMessage(`{"name":"n","calculation_currency":"USD","minimum_margin":"0.25","quote_valid_days":7,`+
		`"max_fx_age_seconds":3600,"tax_profile":{"kind":"tax_profile","id":"tax.zero","revision":1}}`))
	if err != nil || d.PricingPolicy.TargetMargin != "" || strings.Contains(string(dataJSON(d)), "target_margin") {
		t.Fatalf("a policy without a target margin: %s %v", dataJSON(d), err)
	}
	// A float is never a price.
	if _, err = configData("sales_price", json.RawMessage(`{"sku_id":"s","currency":"USD","unit":"PCS","unit_price":8.4,"price_basis_quantity":"1"}`)); err == nil {
		t.Fatal("a number was taken for a unit price")
	}
	doc, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err = json.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Components.Schemas["pol.sales_price"].Properties["unit_price"] == nil ||
		parsed.Components.Schemas["pol.pricing_policy"].Properties["target_margin"] == nil {
		t.Fatal("the OpenAPI document lacks pol.sales_price or the target margin")
	}
}

// The price proposal over HTTP (S4 unit G4a; owner decision 2026-11-18): GET
// /api/brant/v1/trd/rfqs/<id>/price_proposal takes the members of trdapi.PriceProposalQuery as query
// parameters, answers the proposal with snake_case members and decimal strings, refuses unknown,
// repeated and malformed parameters as SCHEMA_INVALID, and is in the OpenAPI document.
func TestPriceProposalRoute(t *testing.T) {
	trd := &fakeTRD{}
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{}, POL: struct{ POL }{}, REF: struct{ REF }{},
		SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: trd, PUR: struct{ PUR }{}, Verifier: tokens{"Bearer good": alice},
		Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	const route = "/api/brant/v1/trd/rfqs/rfq.1/price_proposal"
	a := call(t, h, "GET", route+"?currency=USD&pricing_policy_id=policy.1&pricing_policy_revision=1&cost_snapshot_id=snapshot.1"+
		"&cost_snapshot_result_hash="+hash+"&customer_id=customer.1", "Bearer good", "")
	if a.status != 200 || a.body["ok"] != true {
		t.Fatalf("%d %v", a.status, a.body)
	}
	want := trdapi.PriceProposalQuery{RfqID: "rfq.1", CustomerID: "customer.1", Currency: "USD",
		CostSnapshotRef:  &trdapi.CostSnapshotRef{SnapshotID: "snapshot.1", ResultHash: hash},
		PricingPolicyRef: trdapi.ConfigRef{Kind: "pricing_policy", ID: "policy.1", Revision: 1}}
	if !reflect.DeepEqual(trd.asked[0], want) {
		t.Fatalf("asked %+v", trd.asked[0])
	}
	var wantBody map[string]any
	if err = json.Unmarshal([]byte(`{"ok":true,"rfq_id":"rfq.1","rfq_row_version":2,"customer_id":"customer.1","currency":"USD","price_precision":2,
		"pricing_policy_ref":{"kind":"pricing_policy","id":"policy.1","revision":1},"minimum_margin":"0.25","target_margin":"0.3125",
		"cost_snapshot":{"snapshot_id":"snapshot.1","result_hash":"`+hash+`","evidence_hash":"e","status":"adoptable","adoptable":true},
		"checked_at":"2026-11-18T06:00:00.000000Z","lines":[
		{"rfq_line_id":"L1","sku_id":"ST01.red.S","quantity":"24","unit":"PCS","source":"formula","unit_price":"8.00","price_basis_quantity":"1",
		 "unit_cost":"5.50","target_margin":"0.3125","net_minor":"19200","cost_minor":"13200","margin":"0.3125","under_floor":false},
		{"rfq_line_id":"L2","sku_id":"ST01.red.M","quantity":"36","unit":"PCS","source":"price_list","unit_price":"7.00","price_basis_quantity":"1",
		 "price_list_ref":{"kind":"sales_price","id":"price.red.M","revision":2},"price_list_scope":"customer","net_minor":"25200",
		 "cost_minor":"19800","margin":"0.214285714286","under_floor":true},
		{"rfq_line_id":"L3","sku_id":"ST01.blue.S","quantity":"12","unit":"PCS","source":"none","reason":"no_target_margin","under_floor":false}]}`),
		&wantBody); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.body, wantBody) {
		t.Fatalf("body %v", a.body)
	}

	// Without the optional members: no customer, no snapshot.
	a = call(t, h, "GET", route+"?currency=USD&pricing_policy_id=policy.1&pricing_policy_revision=3", "Bearer good", "")
	if q := trd.asked[1]; a.status != 200 || q.CustomerID != "" || q.CostSnapshotRef != nil || q.PricingPolicyRef.Revision != 3 ||
		a.body["cost_snapshot"] != nil {
		t.Fatalf("%d %v %+v", a.status, a.body, q)
	}
	for name, c := range map[string]struct {
		query  string
		status int
		code   string
		path   string
	}{
		"an unknown parameter":         {"?currency=USD&pricing_policy_id=policy.1&pricing_policy_revision=1&at=now", 422, "SCHEMA_INVALID", "/at"},
		"a repeated parameter":         {"?currency=USD&currency=EUR&pricing_policy_id=policy.1&pricing_policy_revision=1", 422, "SCHEMA_INVALID", "/currency"},
		"a revision that is no number": {"?currency=USD&pricing_policy_id=policy.1&pricing_policy_revision=one", 422, "SCHEMA_INVALID", "/pricing_policy_revision"},
		"no revision":                  {"?currency=USD&pricing_policy_id=policy.1", 422, "SCHEMA_INVALID", "/pricing_policy_ref/revision"},
		"no currency":                  {"?pricing_policy_id=policy.1&pricing_policy_revision=1", 422, "SCHEMA_INVALID", "/currency"},
		"a snapshot without its hash": {"?currency=USD&pricing_policy_id=policy.1&pricing_policy_revision=1&cost_snapshot_id=snapshot.1", 422,
			"SCHEMA_INVALID", "/cost_snapshot_ref/result_hash"},
	} {
		a = call(t, h, "GET", route+c.query, "Bearer good", "")
		if a.status != c.status || a.body["code"] != c.code || a.body["field_errors"].([]any)[0].(map[string]any)["path"] != c.path {
			t.Errorf("%s: %d %v", name, a.status, a.body)
		}
	}
	if a = call(t, h, "GET", "/api/brant/v1/trd/rfqs/rfq.2/price_proposal?currency=USD&pricing_policy_id=policy.1&pricing_policy_revision=1",
		"Bearer good", ""); a.status != 404 || a.body["code"] != "NOT_FOUND" {
		t.Fatalf("%d %v", a.status, a.body)
	}
	if a = call(t, h, "GET", route+"?currency=USD&pricing_policy_id=policy.1&pricing_policy_revision=1", "", ""); a.status != 401 {
		t.Fatalf("%d %v", a.status, a.body)
	}

	doc, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Parameters  []struct {
				Name     string `json:"name"`
				In       string `json:"in"`
				Required bool   `json:"required"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err = json.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	get := parsed.Paths["/api/brant/v1/trd/rfqs/{id}/price_proposal"]["get"]
	required := map[string]bool{}
	for _, p := range get.Parameters {
		required[p.In+":"+p.Name] = p.Required
	}
	if get.OperationID != "trd.quote.price_proposal" || !reflect.DeepEqual(required, map[string]bool{"path:id": true, "query:currency": true,
		"query:pricing_policy_id": true, "query:pricing_policy_revision": true, "query:cost_snapshot_id": false,
		"query:cost_snapshot_result_hash": false, "query:customer_id": false}) {
		t.Fatalf("%s %v", get.OperationID, required)
	}
}
