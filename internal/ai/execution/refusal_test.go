package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	"go.idp.xyz/idp-brant/internal/ai/gateway"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func refusalTurn(t *testing.T) *turn {
	t.Helper()
	reg, err := capability.Q01Registry(capability.Q01Targets{})
	if err != nil {
		t.Fatal(err)
	}
	return &turn{r: &Runner{Gateway: gateway.Gateway{Registry: reg}}, agent: context.Background(),
		scope: kernel.Scope{Tenant: "tenant.1", Company: "company.A"}}
}

// What the model is told of a refused call (S4 G3, unit item 2), on the refusals of the live trial of
// 2026-11-18 (bin/ai-eval/live-20261118T041505Z, phase 02-quote): before, {"ok":false,"code","path"}.
func TestARefusedCallSaysWhatItsMemberNeeds(t *testing.T) {
	tn := refusalTurn(t)
	// The first quote draft of the live model: no UnitPrice, a delivery of kind "date".
	args := json.RawMessage(`{"QuoteID":"quote.q01","ExpectedVersion":0,"Data":{"Lines":[{"LineID":"L1","Quantity":"24"}],
		"Delivery":{"Kind":"date","Date":"2027-01-31","Milestone":"delivery"}}}`)
	call := model.ToolCall{ID: "c1", Name: "quote.save_draft", Arguments: args}
	for _, c := range []struct {
		err               error
		want              string
		keptPath, keptWhy string
	}{
		// A member left out: the context states no reason; the schema's words say what it needs.
		{kernel.Reject(kernel.CodeSchemaInvalid, "/args/data/lines/0/unit_price", ""),
			`{"ok":false,"code":"SCHEMA_INVALID","path":"/args/data/lines/0/unit_price","member":"Data.Lines[0].UnitPrice","missing":true,` +
				`"needs":"required; decimal, up to 8 decimals; for PriceBasisQuantity units, before discount"}`,
			"/args/data/lines/0/unit_price", ""},
		// A value outside an enumeration: the values allowed are told.
		{kernel.Reject(kernel.CodeSchemaInvalid, "/args/data/delivery/milestone", ""),
			`{"ok":false,"code":"SCHEMA_INVALID","path":"/args/data/delivery/milestone","member":"Data.Delivery.Milestone",` +
				`"needs":"required; one of the allowed values; what is due at PlaceRef","allowed":["goods_ready","handover","arrival"]}`,
			"/args/data/delivery/milestone", ""},
		{kernel.Reject(kernel.CodeSchemaInvalid, "/args/data/delivery/kind", ""),
			`{"ok":false,"code":"SCHEMA_INVALID","path":"/args/data/delivery/kind","member":"Data.Delivery.Kind",` +
				`"needs":"required; one of the allowed values; fixed: on Date; lead_time: Days after the event AnchorRef names",` +
				`"allowed":["fixed","lead_time"]}`,
			"/args/data/delivery/kind", ""},
		{kernel.Reject(kernel.CodeSchemaInvalid, "/args/data/delivery/time_zone", ""),
			`{"ok":false,"code":"SCHEMA_INVALID","path":"/args/data/delivery/time_zone","member":"Data.Delivery.TimeZone","missing":true,` +
				`"needs":"required; IANA zone, such as Asia/Shanghai"}`,
			"/args/data/delivery/time_zone", ""},
		// The context's own reason, a static text of its source, is passed with the kind the member names.
		{kernel.Reject(kernel.CodeReferenceInvalid, "/args/data/currency_ref/kind", "The reference is not of the kind its field takes"),
			`{"ok":false,"code":"REFERENCE_INVALID","path":"/args/data/currency_ref/kind","member":"Data.CurrencyRef.Kind","missing":true,` +
				`"reason":"The reference is not of the kind its field takes","needs":"required; one of the allowed values","allowed":["currency"]}`,
			"/args/data/currency_ref/kind", "The reference is not of the kind its field takes"},
		// A reason built at run time is withheld, here one that names another party.
		{kernel.Reject(kernel.CodeInvalidState, "/args/quote_id", "quote.q01 is held by task.77 of customer.9"),
			`{"ok":false,"code":"INVALID_STATE","path":"/args/quote_id","member":"QuoteID","needs":"required; ` +
				`an identifier: 1 to 64 letters, digits and . _ : -, starting with a letter or digit"}`,
			"/args/quote_id", ""},
		// A path that carries a name the model made up is told to the model and not kept.
		{kernel.Reject(kernel.CodeSchemaInvalid, `/args/Data/Unit Price "x"`, ""),
			`{"ok":false,"code":"SCHEMA_INVALID","path":"/args/Data/Unit Price \"x\""}`, "", ""},
		// A refusal that is not about an argument names no member.
		{kernel.Reject(kernel.CodePermissionDenied, "/command", ""), `{"ok":false,"code":"PERMISSION_DENIED","path":"/command"}`, "/command", ""},
	} {
		answer, path, reason := tn.refused(call, c.err)
		if !strings.HasPrefix(answer, c.want) || (strings.HasSuffix(c.want, "}") && answer != c.want) || path != c.keptPath || reason != c.keptWhy {
			t.Errorf("refused(%v)\n got %s (kept %q, %q)\nwant %s (kept %q, %q)", c.err, answer, path, reason, c.want, c.keptPath, c.keptWhy)
		}
		if !json.Valid([]byte(answer)) {
			t.Errorf("the answer is not JSON: %s", answer)
		}
	}
	// A tool that is not registered, and a read: the same shape, by the same rules.
	if answer, _, _ := tn.refused(model.ToolCall{Name: "sql.exec", Arguments: json.RawMessage(`{}`)},
		kernel.Reject(kernel.CodeUnsupportedContract, "/command", "")); answer != `{"ok":false,"code":"UNSUPPORTED_CONTRACT","path":"/command"}` {
		t.Errorf("an unregistered tool: %s", answer)
	}
	answer, _, _ := tn.refused(model.ToolCall{Name: "config.lookup", Arguments: json.RawMessage(`{"Kind":"price_list"}`)},
		kernel.Reject(kernel.CodeSchemaInvalid, "/args/Kind", ""))
	if !strings.Contains(answer, `"member":"Kind"`) || !strings.Contains(answer, `"allowed":["company_profile","currency","unit"`) ||
		strings.Contains(answer, "bank_account") || strings.Contains(answer, `"missing"`) {
		t.Errorf("config.lookup of an unknown kind: %s", answer)
	}
}

// The refusals a turn's facts carry (unit item 3): the task's refused tool calls after the tool's
// last success, the same refusal once with its count, the last eight, oldest first.
func TestRefusalFactsAreWhatANewTurnWouldRepeat(t *testing.T) {
	tn := refusalTurn(t)
	failed := func(tool, code, path, reason string) executionevidence.Call {
		return executionevidence.Call{Kind: executionevidence.KindTool, State: executionevidence.StateFailed, ToolCommand: tool, Code: code,
			RefusalPath: path, RefusalReason: reason}
	}
	ok := func(tool string) executionevidence.Call {
		return executionevidence.Call{Kind: executionevidence.KindTool, State: executionevidence.StateSucceeded, ToolCommand: tool}
	}
	calls := []executionevidence.Call{
		{Kind: executionevidence.KindModel, State: executionevidence.StateSucceeded},
		failed("rfq.get", "NOT_FOUND", "/args/RfqID", ""), // before the RFQ existed
		failed("rfq.save_draft", "SCHEMA_INVALID", "/args/lines/0/uom", ""),
		ok("rfq.save_draft"), // corrected: its refusals are history
		ok("rfq.get"),
		failed("quote.save_draft", "SCHEMA_INVALID", "/args/data/lines/0/unit_price", ""),
		failed("quote.save_draft", "SCHEMA_INVALID", "/args/data/delivery/milestone", ""),
		failed("config.lookup", "SCHEMA_INVALID", "/args/Kind", ""),
		failed("quote.get", "NOT_FOUND", "/args/QuoteID", ""), // a read that found nothing is state, not a lesson
		{Kind: executionevidence.KindTool, State: executionevidence.StateWaiting, ToolCommand: "order.prepare", Code: executionevidence.CodeWaitingApproval},
		failed("quote.save_draft", "SCHEMA_INVALID", "/args/data/lines/0/unit_price", ""), // the next turn repeated it
		failed("quote.save_draft", "REFERENCE_INVALID", "/args/data/currency_ref/kind", "The reference is not of the kind its field takes"),
	}
	got := tn.refusalFacts(calls)
	raw, _ := json.Marshal(got)
	want := `[{"tool":"quote.save_draft","code":"SCHEMA_INVALID","path":"/args/data/delivery/milestone","member":"Data.Delivery.Milestone",` +
		`"needs":"required; one of the allowed values; what is due at PlaceRef","allowed":["goods_ready","handover","arrival"],"times":1},` +
		`{"tool":"config.lookup","code":"SCHEMA_INVALID","path":"/args/Kind","member":"Kind","needs":"required; one of the allowed values",` +
		`"allowed":["company_profile","currency","unit","unit_conversion","fx_rate","calendar","trade_terms","payment_terms","tax_profile",` +
		`"tax_application","fee_category","pricing_policy","number_sequence","document_template","quote_document_template",` +
		`"screening_regulation","credit_policy","legal_terms","order_document_template","shipment_document_template","export_tax_policy"],"times":1},` +
		`{"tool":"quote.save_draft","code":"SCHEMA_INVALID","path":"/args/data/lines/0/unit_price","member":"Data.Lines[0].UnitPrice",` +
		`"needs":"required; decimal, up to 8 decimals; for PriceBasisQuantity units, before discount","times":2},` +
		`{"tool":"quote.save_draft","code":"REFERENCE_INVALID","path":"/args/data/currency_ref/kind","member":"Data.CurrencyRef.Kind",` +
		`"reason":"The reference is not of the kind its field takes","needs":"required; one of the allowed values","allowed":["currency"],"times":1}]`
	if string(raw) != want {
		t.Errorf("refusal facts\n got %s\nwant %s", raw, want)
	}
	// Bounded: of twelve distinct refusals the last eight are told.
	var many []executionevidence.Call
	for i := range 12 {
		many = append(many, failed("quote.save_draft", "SCHEMA_INVALID", fmt.Sprintf("/args/data/lines/%d/unit_price", i), ""))
	}
	got = tn.refusalFacts(many)
	if len(got) != maxRefusalFacts || got[0].Path != "/args/data/lines/4/unit_price" || got[7].Path != "/args/data/lines/11/unit_price" {
		t.Errorf("the bound: %d refusals, %+v", len(got), got)
	}
	if facts := tn.refusalFacts(nil); len(facts) != 0 {
		t.Errorf("no calls, %d refusals", len(facts))
	}
}
