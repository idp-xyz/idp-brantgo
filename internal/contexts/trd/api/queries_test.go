package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Python test_t03_query_contract.py test_query_contract_rejects_bad_shape_and_keeps_unimplemented_kinds_explicit
// (PYT-0436) and test_quote_history_requires_a_specific_revision_and_current_dependencies (PYT-0429,
// the shape half), test_t04_a45_response_query_recovery.py test_closed_request_and_response_schemas
// (PYT-0534, the request half for TRD's queries): the queries are closed — an identifier that is not
// one, a negative position, a page beyond MaxPage and a dispatch without its revision are
// SCHEMA_INVALID at their member; an empty limit is the default page.
func TestQueryContracts(t *testing.T) {
	path := func(err error) string {
		rej, ok := kernel.AsRejection(err)
		if !ok || rej.Code != kernel.CodeSchemaInvalid {
			return "not SCHEMA_INVALID"
		}
		return rej.Fields[0].Path
	}
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"history: a blank quote":       {HistoryQuery{QuoteID: " "}.Validate(), "/quote_id"},
		"history: an over-long quote":  {HistoryQuery{QuoteID: string(make([]byte, 65))}.Validate(), "/quote_id"},
		"history: a negative position": {HistoryQuery{QuoteID: "quote.1", Page: Page{After: -1}}.Validate(), "/after"},
		"history: a position too far":  {HistoryQuery{QuoteID: "quote.1", Page: Page{After: 1 << 53}}.Validate(), "/after"},
		"history: a negative limit":    {HistoryQuery{QuoteID: "quote.1", Page: Page{Limit: -1}}.Validate(), "/limit"},
		"history: a page too large":    {HistoryQuery{QuoteID: "quote.1", Page: Page{Limit: MaxPage + 1}}.Validate(), "/limit"},
		"ready: a bad quote":           {ReadyQuery{QuoteID: "回复 A45"}.Validate(), "/quote_id"},
		"ready: a negative revision":   {ReadyQuery{QuoteID: "quote.1", BusinessRevision: -1}.Validate(), "/business_revision"},
		"dispatch: no revision":        {DispatchQuery{QuoteID: "quote.1"}.Validate(), "/business_revision"},
		"dispatch: a bad quote":        {DispatchQuery{QuoteID: "", BusinessRevision: 1}.Validate(), "/quote_id"},
	} {
		if got := path(c.err); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	for _, err := range []error{HistoryQuery{QuoteID: "quote.1"}.Validate(), HistoryQuery{QuoteID: "quote.1", Page: Page{After: 3, Limit: MaxPage}}.Validate(),
		ReadyQuery{QuoteID: "quote.1"}.Validate(), ReadyQuery{QuoteID: "quote.1", BusinessRevision: 2}.Validate(),
		DispatchQuery{QuoteID: "quote.1", BusinessRevision: 1}.Validate()} {
		if err != nil {
			t.Errorf("a valid query: %v", err)
		}
	}
	if (Page{}).Size() != DefaultPage || (Page{Limit: 7}).Size() != 7 {
		t.Error("page size")
	}
}

// Error catalog V0.1: COMMIT_EVIDENCE_MISSING, APPROVAL_MISSING, RECIPIENT_SNAPSHOT_INTEGRITY_FAILURE
// and EXTERNAL_CHANNEL_NOT_CONFIGURED list TRD and answer 422. TRD declares each once: none is
// QuoteRoot's, the calculation's or the formal document's, and Category answers for them.
func TestQueryCodesFollowTheErrorCatalog(t *testing.T) {
	rows := []string{"COMMIT_EVIDENCE_MISSING", "APPROVAL_MISSING", "RECIPIENT_SNAPSHOT_INTEGRITY_FAILURE", "EXTERNAL_CHANNEL_NOT_CONFIGURED"}
	if len(queryCodes) != len(rows) {
		t.Fatalf("%d codes declared, the catalog has %d rows", len(queryCodes), len(rows))
	}
	for _, code := range rows {
		if c, ok := Category(code); !ok || c != kernel.CategoryInvalid {
			t.Errorf("%s: %q (declared %v)", code, c, ok)
		}
		_, inCodes := codes[code]
		_, inCalc := calcCodes[code]
		_, inDocument := documentCodes[code]
		if inCodes || inCalc || inDocument {
			t.Errorf("%s is declared twice", code)
		}
		if r := Reject(code, "/channel"); r.Code != code || r.Category != kernel.CategoryInvalid {
			t.Errorf("Reject(%s) = %+v", code, r)
		}
	}
}
