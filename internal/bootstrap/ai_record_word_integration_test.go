//go:build integration

package bootstrap_test

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
)

// A place written with the located form's record word (lead decision 2026-11-19, S4 G8). In the
// live trial bin/ai-eval/live-20261119T082915Z normal-1 the model copied the word "segment" from
// the located form it reads into every place it wrote, the fields' and the lines' own
// ("segment 5:43"). That is no place of the text notation, so the gateway dropped the hints, the
// unit "PCS" stayed ambiguous on all four lines, a person named four places, and INQ stored the
// line's own place as the model wrote it ("segment 5:1").
//
// g8Inquiry is that trial's inquiry (inquiry.txt of the trial). g8Draft is its save_draft call as
// the model sent it (trace 01-rfq, turn 1): every place with the word, the model's columns as they
// were — the item and quantity of lines 3 and 4 one and two columns early, the unit of lines 3 and
// 4 where it does not stand (7:43 for 7:45, 8:43 for 8:45).
var g8Inquiry = []string{
	"SYNTHETIC INQUIRY - AI evaluation data, not a real customer",
	"From: customer.1 (contact.1), Hamburg",
	"Please quote FCA, delivery 2027-01-31, payment 30/70.",
	"Code | Item | Qty | Unit",
	"ST01.red.S | knitted T-shirt red S | 24 | PCS",
	"ST01.red.M | knitted T-shirt red M | 36 | PCS",
	"ST01.blue.S | knitted T-shirt blue S | 12 | PCS",
	"ST01.blue.M | knitted T-shirt blue M | 48 | PCS",
}

type g8Line struct {
	id, sku, item, qty string
	own                string    // the line's own place as the model wrote it
	at                 [4]string // sku_id, requested_item, quantity, uom as the model wrote them
}

var g8Lines = []g8Line{
	{"L1", "ST01.red.S", "knitted T-shirt red S", "24", "segment 5:1", [4]string{"segment 5:1", "segment 5:14", "segment 5:38", "segment 5:43"}},
	{"L2", "ST01.red.M", "knitted T-shirt red M", "36", "segment 6:1", [4]string{"segment 6:1", "segment 6:14", "segment 6:38", "segment 6:43"}},
	{"L3", "ST01.blue.S", "knitted T-shirt blue S", "12", "segment 7:1", [4]string{"segment 7:1", "segment 7:14", "segment 7:38", "segment 7:43"}},
	{"L4", "ST01.blue.M", "knitted T-shirt blue M", "48", "segment 8:1", [4]string{"segment 8:1", "segment 8:14", "segment 8:38", "segment 8:43"}},
}

// g8Draft has the agent save lines as one drafting turn and returns the draft as INQ holds it after
// the gateway's locating. tweak may change a line before it is sent. The SKU the lines state is the
// test world's own, sku.1; the words the model quotes for it are the inquiry's codes, as in the trial.
func (w *agentWorld) g8Draft(id string, tweak func(i int, l *g8Line)) inqapi.RfqView {
	w.t.Helper()
	source, file := "src."+id, "file."+id
	var lines []map[string]any
	for i, l := range g8Lines {
		if tweak != nil {
			tweak(i, &l)
		}
		lines = append(lines, map[string]any{"LineID": l.id, "Locator": l.own, "SourceID": source, "RequestedItem": l.item, "SkuID": "sku.1",
			"Quantity": l.qty, "UOM": "PCS", "Locators": []map[string]string{at("sku_id", l.at[0], l.sku),
				at("requested_item", l.at[1], l.item), at("quantity", l.at[2], l.qty), at("uom", l.at[3], "PCS")}})
	}
	w.rawFile(w.clerk, file, "inquiry.txt", evdapi.MimeText, []byte(strings.Join(g8Inquiry, "\n")+"\n"))
	task, rfqID := "task."+id+".rfq", "rfq."+id
	m := model.NewScripted("scripted-q01-v1", calls(registerExtractCall("c1", source, file)),
		calls(tc("c2", inqapi.CommandSaveRfq, map[string]any{"RfqID": rfqID, "ExpectedVersion": 0, "CustomerID": "customer.1", "ContactID": "contact.1",
			"SourceIDs": []string{source}, "Lines": lines})), done("RFQ 草稿已保存。"))
	r := w.runner(m)
	w.sourcedTask(task, tskapi.Target{RfqID: rfqID, CustomerID: "customer.1"}, tskapi.SourceRef{Kind: tskapi.SourceFile, ID: file},
		bundleRef(r), evdapi.CommandRegisterSource, inqapi.CommandSaveRfq)
	if res := w.turnOn(r, task); len(res.Receipts) != 2 || len(res.Refusals) != 0 || m.Remaining() != 0 {
		w.t.Fatalf("the drafting turn %+v", res)
	}
	rfq, err := w.c.INQ.GetRfq(w.clerk, rfqID)
	w.must(err)
	return rfq
}

// g8Places is where the words of line i (0-based) stand in the inquiry: each field at its own place,
// in the order the model listed the fields.
func g8Places(i int) []inqapi.FieldLocator {
	l, row := g8Lines[i], g8Inquiry[4+i]
	place := func(col int) string { return strconv.Itoa(5+i) + ":" + strconv.Itoa(col+1) }
	return []inqapi.FieldLocator{{Field: "sku_id", Locator: place(strings.Index(row, l.sku)), Quote: l.sku},
		{Field: "requested_item", Locator: place(strings.Index(row, l.item)), Quote: l.item},
		{Field: "quantity", Locator: place(strings.Index(row, " | "+l.qty+" | ") + 3), Quote: l.qty},
		{Field: "uom", Locator: place(strings.LastIndex(row, "PCS")), Quote: "PCS"}}
}

// The live trial's draft as the model sent it is confirmed by the owner as it stands, with no
// correction (the normal family's assertion "INQ corrections 0"), and INQ holds no place with the
// word. The fields of lines 1 and 2 stand where the model said, so they are verified there; the
// columns of lines 3 and 4 are wrong, and the words that stand once are moved, the unit, which
// stands in every row, takes the place in the row the model named (G7a's rule). Each line has its
// own place, the model's read as a place.
func TestTheLiveDraftWithTheRecordWordIsConfirmedAsItStands(t *testing.T) {
	w := newAgentWorld(t, false)
	rfq := w.g8Draft("g8.live", nil)
	if len(rfq.Lines) != len(g8Lines) {
		t.Fatalf("lines %+v", rfq.Lines)
	}
	var verified []inqapi.LineVerification
	for i, l := range rfq.Lines {
		if want := strconv.Itoa(5+i) + ":1"; l.Locator != want || len(l.Pending) != 0 || !slices.Equal(l.Locators, g8Places(i)) {
			t.Fatalf("line %d after the locating: place %q (want %q), pending %+v, locators %+v (want %+v)", i+1, l.Locator, want, l.Pending, l.Locators, g8Places(i))
		}
		verified = append(verified, inqapi.LineVerification{LineID: l.LineID})
	}
	_, err := w.c.INQ.ConfirmRfqScope(w.clerk, inqapi.ConfirmRfqScope{IdempotencyKey: w.key(), RfqID: rfq.RfqID, ExpectedVersion: rfq.RowVersion,
		EvidenceIDs: []string{"src.g8.live"}, VerifiedLines: verified})
	w.must(err)
	rfq, err = w.c.INQ.GetRfq(w.clerk, rfq.RfqID)
	w.must(err)
	if rfq.Status != inqapi.StatusReady || len(rfq.Corrections) != 0 {
		t.Fatalf("after the confirmation: %s, corrections %+v", rfq.Status, rfq.Corrections)
	}
	// INQ holds no place with the word, in the lines, the fields or the corrections.
	held, err := json.Marshal(rfq)
	w.must(err)
	if strings.Contains(string(held), "segment") {
		t.Fatalf("INQ holds a place with the record word: %s", held)
	}
}

// What is not a place stays not a place, with the word in front or not (the lead's list: "line 5",
// "row 5:43", "segment" alone, "segment 5:43 PCS", "note 5:43", "segment segment 5:43"). The unit
// written so is no hint: it stays at every place of the extract, pending ambiguous, and INQ holds it
// until a person names the place. A line's own place that is not a place even with the word read off
// is stored as the model wrote it, as before this unit, and INQ takes it: it has no check there.
func TestAFormThatIsNoPlaceStaysNoPlace(t *testing.T) {
	w := newAgentWorld(t, false)
	refused := []string{"segment 5:43 PCS", "segment segment 6:43", "note 7:45", "row 8:45"}
	own := []string{"row 5", "segment row 6", "segment 7:1 PCS", "segment segment 8:1"}
	rfq := w.g8Draft("g8.refused", func(i int, l *g8Line) {
		l.at[3], l.own = refused[i], own[i]
	})
	every := []string{"5:43", "6:43", "7:45", "8:45"}
	for i, l := range rfq.Lines {
		var held []string
		for _, x := range l.Locators {
			if x.Field == inqapi.FieldUOM {
				held = append(held, x.Locator)
			}
		}
		if l.Locator != own[i] || !slices.Equal(held, every) || len(l.Pending) != 1 || l.Pending[0].Field != inqapi.FieldUOM ||
			l.Pending[0].Reason != inqapi.PendingAmbiguous || !slices.Equal(l.Locators[:3], g8Places(i)[:3]) {
			t.Fatalf("line %d after the locating: place %q (the model wrote %q), unit places %v, pending %+v", i+1, l.Locator, own[i], held, l.Pending)
		}
	}
	// INQ is as before: the unit is held until a person names its place; the line's own place is no
	// obstacle.
	_, err := w.c.INQ.ConfirmRfqScope(w.clerk, inqapi.ConfirmRfqScope{IdempotencyKey: w.key(), RfqID: rfq.RfqID, ExpectedVersion: rfq.RowVersion,
		EvidenceIDs: []string{"src.g8.refused"}, VerifiedLines: []inqapi.LineVerification{{LineID: "L1"}, {LineID: "L2"}, {LineID: "L3"}, {LineID: "L4"}}})
	if code(err) != inqapi.CodeSourceAmbiguous || path(err) != "/args/lines/0/uom" {
		t.Fatalf("a confirmation that settles nothing: %v", err)
	}
	var fields []inqapi.LineVerification
	for i, id := range []string{"L1", "L2", "L3", "L4"} {
		fields = append(fields, inqapi.LineVerification{LineID: id, Fields: []inqapi.VerifiedField{{Field: inqapi.FieldUOM, Value: "PCS", Locator: every[i]}}})
	}
	w.must(func() error {
		_, err := w.c.INQ.ConfirmRfqScope(w.clerk, inqapi.ConfirmRfqScope{IdempotencyKey: w.key(), RfqID: rfq.RfqID, ExpectedVersion: rfq.RowVersion,
			EvidenceIDs: []string{"src.g8.refused"}, VerifiedLines: fields})
		return err
	}())
	rfq, err = w.c.INQ.GetRfq(w.clerk, rfq.RfqID)
	w.must(err)
	if rfq.Status != inqapi.StatusReady || len(rfq.Corrections) != len(g8Lines) {
		t.Fatalf("after the owner named the four places: %s, corrections %d: %+v", rfq.Status, len(rfq.Corrections), rfq.Corrections)
	}
	for i, c := range rfq.Corrections {
		if c.Field != inqapi.FieldUOM || c.Locator != every[i] || c.Reason != inqapi.PendingAmbiguous {
			t.Fatalf("correction %d: %+v", i, c)
		}
	}
}
