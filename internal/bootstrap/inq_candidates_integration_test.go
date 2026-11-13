//go:build integration

package bootstrap_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/gateway"
	"go.idp.xyz/idp-brant/internal/ai/ingest/ingesttest"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S4 B4 (WBS S4-04b): RFQ line candidates generated from a file extract, located, and confirmed by
// a human line by line with every correction recorded (BC-AT27, AN01, BC-AT05/CT03, CT16, SD-07).

// inquiryWorkbook is a synthetic customer inquiry (decision E-6): four lines of 24, 36, 12 and 48
// pieces — 120 in all — under a total cell that says 118 (BC-AT05); style ST02 has no unit (AN01).
func inquiryWorkbook() []byte {
	return ingesttest.Workbook{Sheets: []ingesttest.Sheet{{Name: "询价单", Cells: []ingesttest.Cell{
		{Ref: "A1", Text: "款号"}, {Ref: "B1", Text: "品名"}, {Ref: "C1", Text: "数量"}, {Ref: "D1", Text: "单位"},
		{Ref: "A2", Text: "ST01-R-S"}, {Ref: "B2", Text: "针织T恤 红 S"}, {Ref: "C2", Number: "24"}, {Ref: "D2", Text: "PCS"},
		{Ref: "A3", Text: "ST01-R-M"}, {Ref: "B3", Text: "针织T恤 红 M"}, {Ref: "C3", Number: "36"}, {Ref: "D3", Text: "PCS"},
		{Ref: "A4", Text: "ST02"}, {Ref: "B4", Text: "卫衣"}, {Ref: "C4", Number: "12"},
		{Ref: "A5", Text: "ST01-B-S"}, {Ref: "B5", Text: "针织T恤 蓝 S"}, {Ref: "C5", Number: "48"}, {Ref: "D5", Text: "PCS"},
		{Ref: "A6", Text: "合计"}, {Ref: "C6", Number: "118"},
	}}}}.Bytes()
}

func at(field, locator, quote string) map[string]string {
	return map[string]string{"Field": field, "Locator": locator, "Quote": quote}
}

// candidateLines are the lines the scripted model proposes from the inquiry, with its claims about
// where each value stands; one claim is wrong (L2's quantity) and one place is ambiguous (L4's unit).
func candidateLines() []map[string]any {
	return []map[string]any{
		{"LineID": "L1", "SkuID": "ST01.red.S", "Quantity": "24", "UOM": "PCS", "RequestedItem": "针织T恤 红 S", "SourceID": "src.inq",
			"Locator": "询价单 第2行", "Locators": []map[string]string{at("sku_id", "询价单!A2", "ST01-R-S"),
				at("requested_item", "询价单!B2", "针织T恤 红 S"), at("quantity", "询价单!C2", "24"), at("uom", "询价单!D2", "PCS")}},
		{"LineID": "L2", "SkuID": "ST01.red.M", "Quantity": "36", "UOM": "PCS", "RequestedItem": "针织T恤 红 M", "SourceID": "src.inq",
			"Locator": "询价单 第3行", "Locators": []map[string]string{at("sku_id", "询价单!A3", "ST01-R-M"),
				at("requested_item", "询价单!B3", "针织T恤 红 M"), at("quantity", "询价单!C9", "36"), at("uom", "询价单!D3", "PCS")}},
		{"LineID": "L3", "Quantity": "12", "RequestedItem": "卫衣", "SourceID": "src.inq", "Locator": "询价单 第4行",
			"Locators": []map[string]string{at("sku_id", "询价单!A4", "ST02"), at("requested_item", "询价单!B4", "卫衣"), at("quantity", "询价单!C4", "12")},
			"Pending": []map[string]any{{"Field": "sku_id", "Reason": "ambiguous", "Candidates": []string{"ST02.a", "ST02.b"}},
				{"Field": "uom", "Reason": "missing"}}},
		{"LineID": "L4", "SkuID": "ST01.blue.S", "Quantity": "48", "UOM": "PCS", "RequestedItem": "针织T恤 蓝 S", "SourceID": "src.inq",
			"Locator": "询价单 第5行", "Locators": []map[string]string{at("sku_id", "询价单!A5", "ST01-B-S"),
				at("requested_item", "询价单!B5", "针织T恤 蓝 S"), at("quantity", "询价单!C5", "48"), at("uom", "询价单!D9", "PCS")},
			"Pending": []map[string]any{{"Field": "quantity", "Reason": "conflict", "Candidates": []string{"48", "46"}}}},
	}
}

type candidateWorld struct {
	*world
	clerk, agent context.Context
	agentRef     kernel.ActorRef
}

// candidateSetup provisions a sales clerk (team.sales) and the Q01 agent (team.ai), both seeing the
// company's records, the masters the RFQ needs, and the inquiry file the clerk uploaded.
func candidateSetup(t *testing.T) candidateWorld {
	w := newWorld(t)
	scope := w.scope("company.A")
	admin := w.actor("admin", allPermissions...)
	w.source(admin, "src.1", "合成主数据核实资料")
	w.party(admin, "customer.1", true, "src.1")
	w.contact(admin, "contact.1", "customer.1", true, "src.1")
	for _, sku := range []string{"ST01.red.S", "ST01.red.M", "ST01.blue.S", "ST02.a", "ST02.b"} {
		w.sku(admin, sku, true, "src.1")
	}
	cw := candidateWorld{world: w, agentRef: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.q01"}}
	cw.clerk = w.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk.1"},
		Permissions: templatePermissions(t, "sales_clerk"), DataScope: authorization.DataScopeCompany, Team: "team.sales"})
	cw.agent = w.provision(bootstrap.Grant{Scope: scope, Actor: cw.agentRef, Permissions: templatePermissions(t, "ai_q01_agent"),
		DataScope: authorization.DataScopeCompany, Team: "team.ai"})
	w.rawFile(cw.clerk, "file.inq", "inquiry.xlsx", evdapi.MimeXLSX, inquiryWorkbook())
	return cw
}

// draftByAgent has the scripted agent register the inquiry's extract and save the RFQ draft of its
// candidate lines through the gateway, under a delegation of the clerk. It returns the calls that
// ran and the refusals.
func (cw candidateWorld) draftByAgent(t *testing.T) ([]gateway.Call, []string) {
	layer, _ := aiLayer(t, cw.c)
	if _, err := layer.GrantDelegation(cw.clerk, policy.GrantDelegation{IdempotencyKey: cw.key(), DelegationID: "del.b4", AgentID: cw.agentRef.ID,
		TaskID: "task.b4", Capabilities: []string{evdapi.CommandRegisterSource, inqapi.CommandSaveRfq}, Autonomy: "draft",
		ExpiresAt: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	d, err := layer.GetDelegation(cw.agent, "del.b4")
	cw.must(err)
	tools, err := layer.Gateway.Tools(cw.agent, d)
	cw.must(err)
	m := model.NewScripted("scripted-b4-v1",
		model.Step{Reply: model.Message{ToolCalls: []model.ToolCall{
			scriptedCall("c1", evdapi.CommandRegisterSource, map[string]any{"SourceID": "src.inq", "SourceKind": evdapi.KindFileExtract, "FileID": "file.inq"}),
			// AI-INV-004: the model may not supply an extract's text.
			scriptedCall("c2", evdapi.CommandRegisterSource, map[string]any{"SourceID": "src.forged", "SourceKind": evdapi.KindFileExtract,
				"FileID": "file.inq", "ContentText": "ST01-R-S 数量 9999"}),
		}}},
		model.Step{Reply: model.Message{ToolCalls: []model.ToolCall{scriptedCall("c3", inqapi.CommandSaveRfq, map[string]any{
			"RfqID": "rfq.b4", "SourceIDs": []string{"src.inq"}, "UnresolvedFields": []string{"customer", "contact"}, "Lines": candidateLines()})}}},
	)
	b := gateway.Binding{TaskID: "task.b4", DelegationID: "del.b4"}
	var calls []gateway.Call
	var refused []string
	for n, prompt := range []string{"register the inquiry's extract", "draft the RFQ from it"} {
		for i, tc := range turn(t, cw.agent, m, tools, prompt).Message.ToolCalls {
			c, err := layer.Gateway.Decode(cw.agent, b, tc)
			if err == nil {
				var a gateway.Admitted
				if a, err = layer.Gateway.Admit(cw.agent, b, c, fmt.Sprintf("intent.b4.%d.%d", n, i)); err == nil {
					_, err = a.Run(cw.agent)
				}
			}
			if err != nil {
				refused = append(refused, tc.Name+" "+code(err)+" "+path(err))
				continue
			}
			calls = append(calls, c)
		}
	}
	return calls, refused
}

func linesOf(v inqapi.RfqView) []inqapi.RfqLine {
	out := make([]inqapi.RfqLine, len(v.Lines))
	for i, l := range v.Lines {
		out[i] = l.RfqLine
	}
	return out
}

func describeCorrections(cs []inqapi.Correction) string {
	var out []string
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%s %s.%s %s %q>%q @%s %s%v", c.Command, c.LineID, c.Field, c.Kind, c.From, c.To, c.Locator, c.Reason, c.Candidates))
	}
	return strings.Join(out, "\n")
}

// BC-AT27 on a synthetic XLSX (with AN01 and BC-AT05/CT03): the agent registers the inquiry's
// extract — EVD reads the file, a forged text is refused — and saves its candidate lines; the
// gateway verifies every field locator against the extract before the call is hashed: L2's wrong
// quantity locator is relocated to the one place 36 stands, L4's unit stands in three places and is
// pending ambiguous; the model's own pending fields (two SKUs and no unit for ST02, the quantity
// that contradicts the total) stay. INQ records the agent as proposer, and the clerk's save of the
// customer keeps it. The clerk confirms line by line: without verifying every line DRAFT_NOT_READY,
// with ST02's SKU still open SOURCE_AMBIGUOUS; verified, the RFQ is ready with each correction
// recorded against the candidate (原始需求值与核实后结构化值分别保存); nothing is smoothed over: 48
// stays 48 and the total's conflict is recorded as verified, not resolved by editing a line.
func TestAgentCandidatesAreLocatedAndConfirmedLineByLine(t *testing.T) {
	cw := candidateSetup(t)
	calls, refused := cw.draftByAgent(t)
	if fmt.Sprint(refused) != "[source.register SCHEMA_INVALID /args/content_text]" || len(calls) != 2 {
		t.Fatalf("refused %v, ran %d", refused, len(calls))
	}
	if _, err := cw.c.EVD.GetSource(cw.clerk, "src.forged"); code(err) != kernel.CodeNotFound {
		t.Fatalf("the forged extract exists: %v", err)
	}
	// The hash the gateway computed after locating is the one INQ's ledger records (A5-D05).
	_, db := aiLayer(t, cw.c)
	if got := storedHash(t, db, cw.scope("company.A"), "brant_inq", inqapi.CommandSaveRfq, "intent.b4.1.0"); got != calls[1].RequestHash {
		t.Fatalf("INQ recorded %s, the gateway computed %s", got, calls[1].RequestHash)
	}
	draft, err := cw.c.INQ.GetRfq(cw.clerk, "rfq.b4")
	cw.must(err)
	var located []string
	for _, l := range draft.Lines {
		if l.ProposedBy != cw.agentRef || l.QuotesWithheld {
			t.Fatalf("%s: proposed by %v, withheld %v", l.LineID, l.ProposedBy, l.QuotesWithheld)
		}
		var places []string
		for _, x := range l.Locators {
			places = append(places, x.Field+"@"+x.Locator)
		}
		var pending []string
		for _, p := range l.Pending {
			pending = append(pending, p.Field+":"+p.Reason+fmt.Sprint(p.Candidates))
		}
		located = append(located, l.LineID+" "+strings.Join(places, ",")+" "+strings.Join(pending, ","))
	}
	want := []string{
		"L1 sku_id@询价单!A2,requested_item@询价单!B2,quantity@询价单!C2,uom@询价单!D2 ",
		"L2 sku_id@询价单!A3,requested_item@询价单!B3,quantity@询价单!C3,uom@询价单!D3 ",
		"L3 sku_id@询价单!A4,requested_item@询价单!B4,quantity@询价单!C4 sku_id:ambiguous[ST02.a ST02.b],uom:missing[]",
		"L4 sku_id@询价单!A5,requested_item@询价单!B5,quantity@询价单!C5,uom@询价单!D2,uom@询价单!D3,uom@询价单!D5 quantity:conflict[48 46],uom:ambiguous[]",
	}
	if !reflect.DeepEqual(located, want) {
		t.Fatalf("located\n%s\nwant\n%s", strings.Join(located, "\n"), strings.Join(want, "\n"))
	}
	// The clerk completes the customer; the candidate lines are saved as they are and stay the agent's.
	_, err = cw.save(cw.clerk, inqapi.SaveRfq{RfqID: "rfq.b4", ExpectedVersion: 1, CustomerID: "customer.1", ContactID: "contact.1",
		SourceIDs: []string{"src.inq"}, UnresolvedFields: []string{}, Lines: linesOf(draft)})
	cw.must(err)
	confirm := func(v ...inqapi.LineVerification) error {
		_, err := cw.c.INQ.ConfirmRfqScope(cw.clerk, inqapi.ConfirmRfqScope{IdempotencyKey: cw.key(), RfqID: "rfq.b4", ExpectedVersion: 2,
			EvidenceIDs: []string{"src.inq"}, VerifiedLines: v})
		return err
	}
	f := func(field, value, locator string) inqapi.VerifiedField {
		return inqapi.VerifiedField{Field: field, Value: value, Locator: locator}
	}
	l4 := inqapi.LineVerification{LineID: "L4", Fields: []inqapi.VerifiedField{f("uom", "PCS", "询价单!D5"), f("quantity", "48", "")}}
	if err = confirm(); code(err) != kernel.CodeDraftNotReady || path(err) != "/args/verified_lines" {
		t.Fatalf("no verification: %v", err)
	}
	if err = confirm(inqapi.LineVerification{LineID: "L1"}, inqapi.LineVerification{LineID: "L2"},
		inqapi.LineVerification{LineID: "L3", Fields: []inqapi.VerifiedField{f("uom", "PCS", "")}}, l4); code(err) != inqapi.CodeSourceAmbiguous ||
		path(err) != "/args/lines/2/sku_id" {
		t.Fatalf("ST02 still open: %v", err)
	}
	// An AI agent never confirms (X-INV-002), even with every line verified.
	_, err = cw.c.INQ.ConfirmRfqScope(cw.agent, inqapi.ConfirmRfqScope{IdempotencyKey: cw.key(), RfqID: "rfq.b4", ExpectedVersion: 2,
		EvidenceIDs: []string{"src.inq"}})
	cw.want(err, kernel.CodePermissionDenied)
	cw.must(confirm(inqapi.LineVerification{LineID: "L1"}, inqapi.LineVerification{LineID: "L2", Fields: []inqapi.VerifiedField{f("quantity", "36", "")}},
		inqapi.LineVerification{LineID: "L3", Fields: []inqapi.VerifiedField{f("sku_id", "ST02.a", ""), f("uom", "PCS", "")}}, l4))
	ready, err := cw.c.INQ.GetRfq(cw.clerk, "rfq.b4")
	cw.must(err)
	wantCorrections := `rfq.confirm_scope L2.quantity verified "36">"36" @ []
rfq.confirm_scope L3.sku_id changed "">"ST02.a" @ ambiguous[ST02.a ST02.b]
rfq.confirm_scope L3.uom changed "">"PCS" @ missing[]
rfq.confirm_scope L4.uom verified "PCS">"PCS" @询价单!D5 ambiguous[]
rfq.confirm_scope L4.quantity verified "48">"48" @ conflict[48 46]`
	if ready.Status != inqapi.StatusReady || describeCorrections(ready.Corrections) != wantCorrections {
		t.Fatalf("%s\ncorrections\n%s", ready.Status, describeCorrections(ready.Corrections))
	}
	for _, c := range ready.Corrections {
		if c.ProposedBy != cw.agentRef || c.CorrectedBy.ID != "clerk.1" || c.RowVersion != 3 || c.RecordedAt.IsZero() {
			t.Fatalf("correction %+v", c)
		}
	}
	l3, l4v := ready.Lines[2], ready.Lines[3]
	if l3.SkuID != "ST02.a" || l3.UOM != "PCS" || len(l3.Pending) != 0 || l4v.Quantity != "48" || len(l4v.Locators) != 4 ||
		l4v.Locators[3] != (inqapi.FieldLocator{Field: "uom", Locator: "询价单!D5", Quote: "PCS"}) {
		t.Fatalf("confirmed lines %+v %+v", l3, l4v)
	}
	// Corrections are history: never changed or removed.
	if err = cw.exec("company.A", `UPDATE brant_inq.rfq_correction SET to_value = 'x'`); err == nil {
		t.Fatal("corrections must be immutable")
	}
}

// A human who changes a line the agent proposed records the change at the save, whichever command
// carries it (BC-AT27 逐行修正); the line is the human's from then on.
func TestHumanEditsOfCandidatesAreRecorded(t *testing.T) {
	cw := candidateSetup(t)
	if _, refused := cw.draftByAgent(t); len(refused) != 1 {
		t.Fatalf("refused %v", refused)
	}
	draft, err := cw.c.INQ.GetRfq(cw.clerk, "rfq.b4")
	cw.must(err)
	// The clerk drops the agent's L4 and adds a line of their own: the removal is a correction, the
	// addition is not (it corrects no candidate).
	lines := linesOf(draft)[:3]
	lines = append(lines, inqapi.RfqLine{LineID: "L5", Quantity: "1", SourceID: "src.inq", Locator: "电话补充"})
	_, err = cw.save(cw.clerk, inqapi.SaveRfq{RfqID: "rfq.b4", ExpectedVersion: 1, SourceIDs: []string{"src.inq"}, Lines: lines})
	cw.must(err)
	got, err := cw.c.INQ.GetRfq(cw.clerk, "rfq.b4")
	cw.must(err)
	if describeCorrections(got.Corrections) != `rfq.save_draft L4. removed "">"" @ []` || got.Lines[3].ProposedBy.ID != "clerk.1" {
		t.Fatalf("%s", describeCorrections(got.Corrections))
	}
	// Then the clerk restores L4 with the total's 46 (the clerk's line now) and changes L2's quantity:
	// only the change of the agent's L2 is a correction.
	lines = linesOf(got)
	lines = append(lines, inqapi.RfqLine{LineID: "L4", SkuID: "ST01.blue.S", Quantity: "46", UOM: "PCS", SourceID: "src.inq", Locator: "询价单 第5行"})
	lines[1].Quantity = "35"
	_, err = cw.save(cw.clerk, inqapi.SaveRfq{RfqID: "rfq.b4", ExpectedVersion: 2, SourceIDs: []string{"src.inq"}, Lines: lines})
	cw.must(err)
	got, err = cw.c.INQ.GetRfq(cw.clerk, "rfq.b4")
	cw.must(err)
	if describeCorrections(got.Corrections) != "rfq.save_draft L4. removed \"\">\"\" @ []\nrfq.save_draft L2.quantity changed \"36\">\"35\" @询价单!C3 []" ||
		got.Corrections[1].RowVersion != 3 || got.Corrections[1].Seq != 2 {
		t.Fatalf("%s", describeCorrections(got.Corrections))
	}
}

// CT16 for candidates: a reader who sees the RFQ but not the inquiry file — another team's reader,
// or one without evd.read — reads the lines without the file's words (QuotesWithheld); a human who
// cannot read the file cannot confirm against it (REFERENCE_INVALID: 不可访问时不能假装核实成功).
func TestCandidateQuotesFollowTheFilesAccess(t *testing.T) {
	cw := candidateSetup(t)
	if _, refused := cw.draftByAgent(t); len(refused) != 1 {
		t.Fatalf("refused %v", refused)
	}
	scope := cw.scope("company.A")
	teamReader := cw.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "ai.reviewer"},
		Permissions: []string{"inq.read", "evd.read", "rfq.save_draft", "rfq.confirm_scope", "pty.read", "cat.read"},
		DataScope:   authorization.DataScopeTeam, Team: "team.ai"})
	noEvd := cw.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "inq.only"},
		Permissions: []string{"inq.read"}, DataScope: authorization.DataScopeCompany})
	for _, reader := range []context.Context{teamReader, noEvd} {
		v, err := cw.c.INQ.GetRfq(reader, "rfq.b4")
		cw.must(err)
		for _, l := range v.Lines {
			if !l.QuotesWithheld || len(l.Locators) == 0 {
				t.Fatalf("%s: %+v", l.LineID, l)
			}
			for _, x := range l.Locators {
				if x.Quote != "" {
					t.Fatalf("%s: the file's words %q reached a reader without access", l.LineID, x.Quote)
				}
			}
		}
	}
	_, err := cw.c.INQ.ConfirmRfqScope(teamReader, inqapi.ConfirmRfqScope{IdempotencyKey: cw.key(), RfqID: "rfq.b4", ExpectedVersion: 1,
		EvidenceIDs: []string{"src.inq"}})
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence_ids/0" {
		t.Fatalf("confirmation without access to the file: %v", err)
	}
	// The clerk who sees the file reads the words.
	v, err := cw.c.INQ.GetRfq(cw.clerk, "rfq.b4")
	cw.must(err)
	if v.Lines[0].QuotesWithheld || v.Lines[0].Locators[0].Quote != "ST01-R-S" {
		t.Fatalf("%+v", v.Lines[0])
	}
}
