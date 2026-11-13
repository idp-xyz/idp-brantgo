package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func rejection(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return "OK"
}

// Structured locators and pending fields have closed shapes (WBS S4-04b): a known field, a locator
// and the source's words of 1 to 200 characters, a locator only with the line's source, no
// repeated place or pending field, a known reason, at most MaxCandidates distinct candidates.
func TestSaveRfqLineStructure(t *testing.T) {
	ok := RfqLine{LineID: "L1", SourceID: "src.1", Locators: []FieldLocator{{Field: FieldQuantity, Locator: "Sheet1!B2", Quote: "24"}},
		Pending: []PendingField{{Field: FieldSkuID, Reason: PendingAmbiguous, Candidates: []string{"sku.1", "sku.2"}}}}
	many := make([]string, MaxCandidates+1)
	for i := range many {
		many[i] = strings.Repeat("x", i+1)
	}
	for _, c := range []struct {
		edit func(*RfqLine)
		want string
	}{
		{func(*RfqLine) {}, "OK"},
		{func(l *RfqLine) { l.Locators[0].Field = "price" }, "SCHEMA_INVALID /args/lines/0/locators/0/field"},
		{func(l *RfqLine) { l.Locators[0].Quote = "" }, "SCHEMA_INVALID /args/lines/0/locators/0/quote"},
		{func(l *RfqLine) { l.Locators[0].Locator = strings.Repeat("A", 201) }, "SCHEMA_INVALID /args/lines/0/locators/0/locator"},
		{func(l *RfqLine) { l.SourceID = "" }, "SCHEMA_INVALID /args/lines/0/source_id"},
		{func(l *RfqLine) { l.Locators = append(l.Locators, l.Locators[0]) }, "SCHEMA_INVALID /args/lines/0/locators/1/locator"},
		{func(l *RfqLine) { l.Locators = make([]FieldLocator, MaxLineLocators+1) }, "SCHEMA_INVALID /args/lines/0/locators"},
		{func(l *RfqLine) { l.Pending = append(l.Pending, l.Pending[0]) }, "SCHEMA_INVALID /args/lines/0/pending/1/field"},
		{func(l *RfqLine) { l.Pending[0].Reason = "unsure" }, "SCHEMA_INVALID /args/lines/0/pending/0/reason"},
		{func(l *RfqLine) { l.Pending[0].Candidates = []string{"a", "a"} }, "SCHEMA_INVALID /args/lines/0/pending/0/candidates/1"},
		{func(l *RfqLine) { l.Pending[0].Candidates = many }, "SCHEMA_INVALID /args/lines/0/pending/0/candidates"},
	} {
		l := ok
		l.Locators = append([]FieldLocator(nil), ok.Locators...)
		l.Pending = append([]PendingField(nil), ok.Pending...)
		c.edit(&l)
		if got := rejection(SaveRfq{IdempotencyKey: "k1", RfqID: "rfq.1", Lines: []RfqLine{l}}.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", l, got, c.want)
		}
	}
}

// A line-by-line verification names each line once, each known field once, with a value of the
// field's shape and a locator of at most 200 characters.
func TestConfirmVerifiedLines(t *testing.T) {
	base := func(v ...LineVerification) ConfirmRfqScope {
		return ConfirmRfqScope{IdempotencyKey: "k1", RfqID: "rfq.1", ExpectedVersion: 1, EvidenceIDs: []string{"src.1"}, VerifiedLines: v}
	}
	f := func(field, value string) VerifiedField { return VerifiedField{Field: field, Value: value} }
	for _, c := range []struct {
		cmd  ConfirmRfqScope
		want string
	}{
		{base(), "OK"},
		{base(LineVerification{LineID: "L1"}, LineVerification{LineID: "L2", Fields: []VerifiedField{f(FieldQuantity, "24"), f(FieldUOM, "PCS"),
			f(FieldSkuID, "sku.1"), f(FieldRequestedItem, "红 S")}}), "OK"},
		{base(LineVerification{LineID: "L1"}, LineVerification{LineID: "L1"}), "SCHEMA_INVALID /args/verified_lines/1/line_id"},
		{base(LineVerification{LineID: ""}), "SCHEMA_INVALID /args/verified_lines/0/line_id"},
		{base(LineVerification{LineID: "L1", Fields: []VerifiedField{f(FieldUOM, "DOZEN")}}), "SCHEMA_INVALID /args/verified_lines/0/fields/0/value"},
		{base(LineVerification{LineID: "L1", Fields: []VerifiedField{f(FieldQuantity, "-1")}}), "SCHEMA_INVALID /args/verified_lines/0/fields/0/value"},
		{base(LineVerification{LineID: "L1", Fields: []VerifiedField{f(FieldQuantity, "1"), f(FieldQuantity, "2")}}),
			"SCHEMA_INVALID /args/verified_lines/0/fields/1/field"},
		{base(LineVerification{LineID: "L1", Fields: []VerifiedField{f("source_id", "src.2")}}), "SCHEMA_INVALID /args/verified_lines/0/fields/0/field"},
		{base(LineVerification{LineID: "L1", Fields: []VerifiedField{{Field: FieldQuantity, Value: "1", Locator: strings.Repeat("A", 201)}}}),
			"SCHEMA_INVALID /args/verified_lines/0/fields/0/locator"},
	} {
		if got := rejection(c.cmd.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", c.cmd.VerifiedLines, got, c.want)
		}
	}
}

// The members B4 added are left out of a request's JSON while empty, so the request hash of every
// command sent before them is unchanged (X-INV-001: a replay across the upgrade).
func TestNewMembersLeaveEarlierRequestsUnchanged(t *testing.T) {
	for _, v := range []any{SaveRfq{RfqID: "rfq.1", Lines: []RfqLine{{LineID: "L1"}}}, ConfirmRfqScope{RfqID: "rfq.1"}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range []string{"Locators", "Pending", "VerifiedLines"} {
			if strings.Contains(string(b), member) {
				t.Errorf("%s holds %s", b, member)
			}
		}
	}
}

// SOURCE_AMBIGUOUS is in the error catalog with INQ among its contexts, status 422 (invalid).
func TestSourceAmbiguousIsInTheCatalog(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "00_START_HERE", "tools", "data", "error_catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Codes [][]any `json:"codes"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, r := range doc.Codes {
		if r[0] == CodeSourceAmbiguous {
			if r[2] != float64(422) || !strings.Contains(r[4].(string), "INQ") {
				t.Fatalf("catalog row %v", r)
			}
			return
		}
	}
	t.Fatal("SOURCE_AMBIGUOUS is not in the catalog")
}
