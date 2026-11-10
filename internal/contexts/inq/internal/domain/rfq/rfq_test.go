package rfq

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return "OK"
}

var facts = Facts{
	Parties:  map[string]Party{"customer.1": {Customer: true, Confirmed: true, RowVersion: 2}, "supplier.1": {Confirmed: true}},
	Contacts: map[string]Contact{"contact.1": {PartyID: "customer.1", Confirmed: true, RowVersion: 2}},
	Skus:     map[string]Sku{"sku.1": {Confirmed: true, SpecRevision: 1}, "sku.draft": {}},
	Sources:  map[string]bool{"src.1": true, "src.2": true},
}

func ready() *Rfq {
	r := New("rfq.1", "alice", "")
	r.ReplaceDraft(Content{CustomerID: "customer.1", ContactID: "contact.1", SourceIDs: []string{"src.1"},
		Lines: []Line{{ID: "L1", SkuID: "sku.1", Quantity: "120", UOM: "PCS", SourceID: "src.1", Locator: "第1段"}}})
	return r
}

// Python test_duplicate_line_and_zero_quantity_have_exact_codes and
// test_unknown_source_points_to_actual_request_field.
func TestDraftChecksHaveExactCodesAndPaths(t *testing.T) {
	for _, c := range []struct {
		d    Content
		want string
	}{
		{Content{Lines: []Line{{ID: "L1"}, {ID: "L1"}}}, "DUPLICATE_LINE_ID /args/lines"},
		{Content{Lines: []Line{{ID: "L1", Quantity: "0.000"}}}, "INVALID_KNOWN_VALUE /args/lines/0/quantity"},
		{Content{CustomerID: "sku.1"}, "REFERENCE_INVALID /args/customer_id"},
		{Content{CustomerID: "supplier.1"}, "REFERENCE_INVALID /args/customer_id"},
		{Content{CustomerID: "customer.1", ContactID: "contact.x"}, "REFERENCE_INVALID /args/contact_id"},
		{Content{SourceIDs: []string{"src.1", "missing"}}, "REFERENCE_INVALID /args/source_ids/1"},
		{Content{Lines: []Line{{ID: "L1", SourceID: "missing"}}}, "REFERENCE_INVALID /args/lines/0/source_id"},
		{Content{Lines: []Line{{ID: "L1", SkuID: "sku.unknown"}}}, "REFERENCE_INVALID /args/lines/0/sku_id"},
		{Content{Lines: []Line{{ID: "L1", SkuID: "sku.draft", Quantity: "0.5"}}}, "OK"},
	} {
		if got := code(CheckDraft(c.d, facts)); got != c.want {
			t.Errorf("%+v: %s, want %s", c.d, got, c.want)
		}
	}
}

// Python test_confirmation_requires_all_sources_and_no_unresolved_fields and
// test_supplemental_confirmation_evidence_is_retained.
func TestConfirmationNeedsCompleteCurrentContentAndCoveringEvidence(t *testing.T) {
	r := ready()
	r.Sources = append(r.Sources, "src.2")
	if got := code(r.Confirm([]string{"src.1"}, facts, kernel.ActorRef{}, 1)); got != "DRAFT_NOT_READY /args/evidence_ids" {
		t.Fatal(got)
	}
	r = ready()
	r.Unresolved = []string{"交期尚未澄清"}
	if got := code(r.Confirm([]string{"src.1"}, facts, kernel.ActorRef{}, 1)); got != "DRAFT_NOT_READY /args/unresolved_fields" {
		t.Fatal(got)
	}
	r = ready()
	r.Lines[0].Locator = ""
	if got := code(r.Confirm([]string{"src.1"}, facts, kernel.ActorRef{}, 1)); got != "DRAFT_NOT_READY /args/lines/0/locator" {
		t.Fatal(got)
	}
	r = ready()
	r.Lines[0].SkuID = "sku.draft"
	if got := code(r.Confirm([]string{"src.1"}, facts, kernel.ActorRef{}, 1)); got != "REFERENCE_INVALID /args/lines/0/sku_id" {
		t.Fatal(got)
	}
	r = ready()
	if got := code(r.Confirm([]string{"src.1", "src.9"}, facts, kernel.ActorRef{}, 1)); got != "REFERENCE_INVALID /args/evidence_ids/1" {
		t.Fatal(got)
	}
	r = ready()
	human := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "alice"}
	if err := r.Confirm([]string{"src.2", "src.1"}, facts, human, 7); err != nil {
		t.Fatal(err)
	}
	if r.Status != Ready || r.RowVersion != 2 || r.Lines[0].SkuSpecRevision != 1 || len(r.Sources) != 2 ||
		r.Confirmation.CustomerVersion != 2 || r.Confirmation.Epoch != 7 {
		t.Fatalf("%+v %+v", r, r.Confirmation)
	}
}

func TestChecksRunInContractOrder(t *testing.T) {
	r := ready()
	r.Cancel()
	for _, c := range []struct {
		err  error
		want string
	}{
		{CheckSave(nil, false, 1), "NOT_FOUND /args/id"},
		{CheckSave(r, true, 1), "VERSION_CONFLICT /args/expected_version"},
		{CheckSave(r, true, 2), "INVALID_STATE /args/id"},
		{Check(r, false, 2, Draft), "NOT_FOUND /args/id"},
	} {
		if got := code(c.err); got != c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
}
