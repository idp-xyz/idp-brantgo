package httpapi

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func TestSnakeNames(t *testing.T) {
	for in, want := range map[string]string{
		"LegalName": "legal_name", "FXRate": "fx_rate", "MaxFXAgeSeconds": "max_fx_age_seconds", "SkuID": "sku_id",
		"OwnerID": "owner_id", "RegistrationNo": "registration_no", "SpecRevision": "spec_revision", "Code": "code",
		"EvidenceIDs": "evidence_ids", "IDs": "ids", "URLsSeen": "urls_seen", "ContactRefs": "contact_refs",
	} {
		if got := snake(in); got != want {
			t.Errorf("snake(%s) = %s, want %s", in, got, want)
		}
	}
}

// Every kind POL defines decodes over HTTP; the transport holds no list of its own.
func TestEveryPOLKindHasAWireStructure(t *testing.T) {
	types := configDataTypes()
	for f := range reflect.TypeFor[polapi.ConfigData]().Fields() {
		if _, ok := types[kindOf(f.Name)]; !ok || !polapi.KindKnown(kindOf(f.Name)) {
			t.Errorf("ConfigData.%s: kind %q is not a POL kind or has no wire structure", f.Name, kindOf(f.Name))
		}
	}
}

func TestConfigDataDecodesNestedStructuresAndReadsBack(t *testing.T) {
	in := `{"name":"30% deposit","stages":[{"stage_id":"S1","percent":"30","trigger":"order_confirmed","days":0},` +
		`{"stage_id":"S2","percent":"70","trigger":"after_shipment","days":30}],"day_basis":"working",` +
		`"calendar":{"kind":"calendar","id":"cn","revision":2}}`
	d, err := configData("payment_terms", json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	want := polapi.PaymentTerms{Name: "30% deposit", DayBasis: "working", Calendar: &polapi.ConfigRef{Kind: "calendar", ConfigID: "cn", Revision: 2},
		Stages: []polapi.PaymentStage{{StageID: "S1", Percent: "30", Trigger: "order_confirmed"}, {StageID: "S2", Percent: "70", Trigger: "after_shipment", Days: 30}}}
	if d.PaymentTerms == nil || !reflect.DeepEqual(*d.PaymentTerms, want) {
		t.Fatalf("decoded %+v", d.PaymentTerms)
	}
	// The view reads back as the same request: zero numbers stay, absent strings stay absent.
	var a, b any
	if err := json.Unmarshal(dataJSON(d), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(in), &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("view %s", dataJSON(d))
	}
	jpy, err := configData("currency", json.RawMessage(`{"code":"JPY","name":"Yen","minor_units":0,"rounding":"HALF_UP"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(dataJSON(jpy)); got != `{"code":"JPY","name":"Yen","minor_units":0,"rounding":"HALF_UP"}` {
		t.Fatalf("currency view %s", got)
	}
}

func TestConfigDataRejectsWithPathsIntoTheData(t *testing.T) {
	for name, c := range map[string]struct{ kind, data, path string }{
		"unknown member":       {"payment_terms", `{"name":"n","stages":[{"stage_id":"S1","extra":1}]}`, "/args/data/stages/0/extra"},
		"Go field name":        {"currency", `{"Code":"USD"}`, "/args/data/Code"},
		"config id as Go name": {"payment_terms", `{"calendar":{"kind":"calendar","config_id":"cn"}}`, "/args/data/calendar/config_id"},
		"not an object":        {"currency", `[]`, "/args/data"},
	} {
		_, err := configData(c.kind, json.RawMessage(c.data))
		var se schemaError
		if !errors.As(err, &se) || se.rejection.Code != kernel.CodeSchemaInvalid || se.rejection.Fields[0].Path != c.path {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A kind POL does not define passes on empty, so POL answers with its own verdict.
	if d, err := configData("no_such_kind", json.RawMessage(`{"x":1}`)); err != nil || !reflect.DeepEqual(d, polapi.ConfigData{}) {
		t.Fatalf("%+v %v", d, err)
	}
}

// The S5 additions of POL (s5-plan A3a) reach the transport through the reflective mapping alone:
// a credit policy's S5 members, a payment stage's release control and the two new kinds decode,
// read back as the same request, and appear in the OpenAPI document.
func TestS5ConfigDataOverHTTP(t *testing.T) {
	for kind, in := range map[string]string{
		"credit_policy": `{"name":"E-2","currency":"USD","default_limit":"0","limit_sources":["internal_assessment"],` +
			`"approval_tiers":[{"level":"credit_officer","max_amount":"1000.00"},{"level":"credit_manager","max_amount":"5000.00"}],` +
			`"overdue_tolerance_days":30,"documents_secured":true,"reservation_validity_days":30,"decision_validity_days":7,` +
			`"two_approvers_above_top":true}`,
		"payment_terms": `{"name":"30/70 CAD","stages":[{"stage_id":"S1","percent":"30","trigger":"order_confirmed","days":0},` +
			`{"stage_id":"S2","percent":"70","trigger":"after_shipment","days":0,"release_control":"documents"}]}`,
		"shipment_document_template": `{"name":"pl","document_kind":"packing_list","language":"en","renderer_version":"shipment-text/0.1.0",` +
			`"title":"PACKING LIST","sections":["seller","buyer","identity","shipment","packages","totals","marks","notes"],"footer_note":"f"}`,
		"export_tax_policy": `{"name":"n","refund_method":"exempt_refund","policy_basis":"b","professional_review_evidence_id":"review"}`,
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
	doc, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err = json.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pol.shipment_document_template", "pol.export_tax_policy"} {
		if parsed.Components.Schemas[name] == nil {
			t.Errorf("the OpenAPI document has no %s", name)
		}
	}
}

// S5R-D06, D14, D38: the new members of POL's data travel over the wire like the others and stay
// absent when they are not stated, so an old view still reads back as the request it was.
func TestS5RConfigDataOverHTTP(t *testing.T) {
	for kind, in := range map[string]string{
		"trade_terms": `{"incoterm":"CIF","edition":"2020","named_place":"DEHAM","responsibility_note":"r",` +
			`"delivery":{"kind":"fixed_date","date":"2027-01-31","days":0},"insurance_percent":"120"}`,
		"screening_regulation": `{"lists":[{"list_code":"cn.uel","jurisdiction":"CN"}],"rescreen_interval_days":30,` +
			`"programmes":[{"list_code":"cn.country_programmes","jurisdiction":"CN"},{"list_code":"eu.p","jurisdiction":"foreign",` +
			`"legal_confirmation_evidence_id":"legal.eu"}]}`,
		"export_tax_policy": `{"name":"n","refund_method":"taxable","policy_basis":"b","professional_review_evidence_id":"review"}`,
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
	d, err := configData("trade_terms", json.RawMessage(`{"incoterm":"FCA","edition":"2020","named_place":"n","responsibility_note":"r",`+
		`"delivery":{"kind":"fixed_date","date":"2027-01-31"}}`))
	if err != nil || d.TradeTerms.InsurancePercent != "" || strings.Contains(string(dataJSON(d)), "insurance_percent") {
		t.Fatalf("terms without an insurance percentage: %s %v", dataJSON(d), err)
	}
}
