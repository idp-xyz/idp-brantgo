package api

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func pathOf(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " " + rej.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

func line() Line {
	return Line{LineID: "L1", RfqLineID: "L1", Sku: SkuRef{SkuID: "sku.1", SpecRevision: 1}, Quantity: "120.000000",
		UnitRef: ConfigRef{Kind: "unit", ID: "PCS", Revision: 1}, UnitPrice: "8.0000", PriceBasisQuantity: "1", DiscountRatio: "0.00",
		TaxApplicationRef: ConfigRef{Kind: "tax_application", ID: "tax.sale", Revision: 1},
		Evidence:          []Evidence{{SourceID: "source.1", Locator: "合成核实依据"}}}
}

// The closed contract of quote.save_draft (Python T03_write-commands.schema.json quote_draft):
// every member optional, a present member well formed.
func TestSaveDraftContract(t *testing.T) {
	ok := SaveDraft{IdempotencyKey: "k1", QuoteID: "quote.1", Data: QuoteData{Lines: []Line{line()}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (SaveDraft{IdempotencyKey: "k1", QuoteID: "quote.1"}).Validate(); err != nil {
		t.Fatalf("an empty draft is a draft: %v", err)
	}
	notes := strings.Repeat("字", 5001)
	for want, edit := range map[string]func(c *SaveDraft){
		"SCHEMA_INVALID /idempotency_key":                  func(c *SaveDraft) { c.IdempotencyKey = "" },
		"SCHEMA_INVALID /args/quote_id":                    func(c *SaveDraft) { c.QuoteID = "bad id" },
		"SCHEMA_INVALID /args/expected_version":            func(c *SaveDraft) { c.ExpectedVersion = -1 },
		"SCHEMA_INVALID /args/data/lines/0/quantity":       func(c *SaveDraft) { c.Data.Lines[0].Quantity = "0" },
		"SCHEMA_INVALID /args/data/lines/0/unit_price":     func(c *SaveDraft) { c.Data.Lines[0].UnitPrice = "1e3" },
		"SCHEMA_INVALID /args/data/lines/0/discount_ratio": func(c *SaveDraft) { c.Data.Lines[0].DiscountRatio = "1" },
		"SCHEMA_INVALID /args/data/lines/0/evidence":       func(c *SaveDraft) { c.Data.Lines[0].Evidence = nil },
		"SCHEMA_INVALID /args/data/lines":                  func(c *SaveDraft) { c.Data.Lines = []Line{} },
		"SCHEMA_INVALID /args/data/customer_notes":         func(c *SaveDraft) { c.Data.CustomerNotes = &notes },
		"SCHEMA_INVALID /args/data/expires_at":             func(c *SaveDraft) { c.Data.ExpiresAt = "2026-10-28 08:00:00" },
		"SCHEMA_INVALID /args/data/cost_snapshot_ref/result_hash": func(c *SaveDraft) {
			c.Data.CostSnapshotRef = &CostSnapshotRef{SnapshotID: "s", ResultHash: "ABC"}
		},
		"SCHEMA_INVALID /args/data/charges/0/included_line_id": func(c *SaveDraft) {
			c.Data.Charges = []Charge{{ChargeID: "C1", FeeCategoryRef: ConfigRef{Kind: "fee_category", ID: "f", Revision: 1}, ObligationKey: "o",
				Amount: "1", Inclusion: InclusionIncludedInLine, TaxApplicationRef: ConfigRef{Kind: "tax_application", ID: "t", Revision: 1},
				Evidence: []Evidence{{SourceID: "s", Locator: "l"}}}}
		},
		"SCHEMA_INVALID /args/data/customer_address_refs/0/party_revision": func(c *SaveDraft) {
			c.Data.CustomerAddressRefs = []AddressRoleRef{{PartyID: "p", AddressID: "a"}}
		},
	} {
		c := ok
		c.Data.Lines = []Line{line()}
		edit(&c)
		if got := pathOf(c.Validate()); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

func TestOtherCommandContracts(t *testing.T) {
	proof := []Evidence{{SourceID: "s", Locator: "l"}}
	for _, c := range []struct {
		err  error
		want string
	}{
		{Freeze{IdempotencyKey: "k", QuoteID: "q", ExpectedVersion: 1, WorkingRevision: 1}.Validate(), "OK"},
		{Freeze{IdempotencyKey: "k", QuoteID: "q", ExpectedVersion: 1}.Validate(), "SCHEMA_INVALID /args/working_revision"},
		{Revise{IdempotencyKey: "k", QuoteID: "q", BaseRevision: 1}.Validate(), "SCHEMA_INVALID /args/expected_version"},
		{DiscardDraft{IdempotencyKey: "k", QuoteID: "q", ExpectedVersion: 1, WorkingRevision: 1, Evidence: proof}.Validate(), "SCHEMA_INVALID /args/reason"},
		{DiscardDraft{IdempotencyKey: "k", QuoteID: "q", ExpectedVersion: 1, WorkingRevision: 1, Reason: "r"}.Validate(), "SCHEMA_INVALID /args/evidence"},
		{UpdateMetadata{IdempotencyKey: "k", QuoteID: "q", ExpectedVersion: 1, Labels: []string{"a", "a"}}.Validate(), "SCHEMA_INVALID /args/labels/1"},
		{BindControl{IdempotencyKey: "k", QuoteID: "q"}.Validate(), "SCHEMA_INVALID /args/task_id"},
		{BindControl{IdempotencyKey: "k", QuoteID: "q", TaskID: "task.1"}.Validate(), "OK"},
		{StopControl{IdempotencyKey: "k", QuoteID: "q", Reason: "r"}.Validate(), "SCHEMA_INVALID /args/expected_control_epoch"},
		{ReleaseControl{IdempotencyKey: "k", QuoteID: "q", ExpectedControlEpoch: 1}.Validate(), "SCHEMA_INVALID /args/reason"},
	} {
		if got := pathOf(c.err); got != c.want {
			t.Errorf("got %s, want %s", got, c.want)
		}
	}
}

// Python test_t03_a24_write_control.py test_task_identity_cannot_be_supplied_through_public_json
// (PYT-0195): no command a transport decodes has a member that names a task or a control epoch,
// except object_control.bind's own task; the claim comes from the trusted adapter's context only.
func TestTaskIdentityIsNotACommandMember(t *testing.T) {
	for _, c := range []any{SaveDraft{}, Revise{}, DiscardDraft{}, Freeze{}, UpdateMetadata{}, StopControl{}, ReleaseControl{}, QuoteData{}, Line{}, Charge{}} {
		ty := reflect.TypeOf(c)
		for i := range ty.NumField() {
			name := ty.Field(i).Name
			if strings.Contains(name, "Task") || (strings.Contains(name, "ControlEpoch") && ty.Name() != "StopControl" && ty.Name() != "ReleaseControl") {
				t.Errorf("%s has %s", ty.Name(), name)
			}
		}
	}
	ctx := context.Background()
	if _, ok := ControlFromContext(ctx); ok {
		t.Fatal("no claim without the adapter")
	}
	if _, err := WithControl(ctx, ControlClaim{TaskID: "task.1"}); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("a claim without an epoch: %v", err)
	}
	ctx, err := WithControl(ctx, ControlClaim{TaskID: "task.1", ControlEpoch: 3})
	if c, ok := ControlFromContext(ctx); err != nil || !ok || c.TaskID != "task.1" || c.ControlEpoch != 3 {
		t.Fatalf("claim %+v %v", c, err)
	}
}

// Error catalog V0.1: the QuoteRoot rows with their categories (OBJECT_CONTROL_CONFLICT and
// CONTROL_EPOCH_STALE 409, REFERENCE_STALE 409, the rest 422). Category and Reject answer for
// QuoteRoot's codes and the calculation's alike; the two maps share no code.
func TestCodesHaveCatalogCategories(t *testing.T) {
	want := map[string]kernel.Category{
		CodeObjectControlConflict: kernel.CategoryConflict, CodeControlEpochStale: kernel.CategoryConflict,
		CodeQuoteExpired: kernel.CategoryInvalid, CodeQuoteValidityInvalid: kernel.CategoryInvalid,
		CodeCostSnapshotMissing: kernel.CategoryInvalid, CodeBusinessHashMismatch: kernel.CategoryInvalid,
		CodeCustomerReferenceMiss: kernel.CategoryInvalid, CodeContactReferenceMiss: kernel.CategoryInvalid,
		CodeReferenceStale: kernel.CategoryConflict,
	}
	if len(codes) != len(want) {
		t.Fatalf("%d QuoteRoot codes, want %d", len(codes), len(want))
	}
	for code, category := range want {
		if got, ok := Category(code); !ok || got != category {
			t.Errorf("%s: %v", code, got)
		}
		if r := Reject(code, "/args/quote_id"); r.Code != code || r.Category != category {
			t.Errorf("Reject(%s) = %+v", code, r)
		}
	}
	for code := range calcCodes {
		if _, clash := codes[code]; clash {
			t.Errorf("%s is declared twice", code)
		}
		if got, ok := Category(code); !ok || got != kernel.CategoryInvalid {
			t.Errorf("calculation code %s: %v", code, got)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("an undeclared code must panic")
		}
	}()
	_ = Reject(kernel.CodeSchemaInvalid, "/")
}
