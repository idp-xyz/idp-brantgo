package api

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// The S5 contract additions (owner decision 2026-11-12; s5-plan A3a): exchange rates for credit
// (STL-INV-004) and settlement (STL-INV-007); release control documents on a payment stage (S5 D-5,
// G0 F-04); the credit policy's secured documents, validities and two approvers above the highest
// tier (S5 D-6, STL-INV-005); shipment document templates (WBS S5-08, D-12); the export tax policy
// (CN-AT12); the packing list's numbers.
func TestS5Contracts(t *testing.T) {
	pl := func(d *ConfigData) {
		d.ShipmentDocumentTemplate.DocumentKind = ShipmentDocumentPackingList
		d.ShipmentDocumentTemplate.Sections = []string{"seller", "buyer", "identity", "shipment", "packages", "totals", "marks", "notes"}
		d.ShipmentDocumentTemplate.Clauses = nil
	}
	documents := func(d *ConfigData) {
		d.PaymentTerms.Stages[1] = PaymentStage{StageID: "balance", Percent: "70", Trigger: TriggerAfterShipment,
			ReleaseControl: ReleaseControlDocuments}
	}
	for _, c := range []struct {
		kind string
		edit func(*ConfigData)
		want string
	}{
		{KindFXRate, func(d *ConfigData) { d.FXRate.Purpose = PurposeCredit }, "OK"},
		{KindFXRate, func(d *ConfigData) { d.FXRate.Purpose = PurposeSettlement }, "OK"},
		{KindFXRate, func(d *ConfigData) { d.FXRate.Purpose = "receivable" }, "SCHEMA_INVALID /args/data/purpose"},
		{KindPaymentTerms, documents, "OK"},
		{KindPaymentTerms, func(d *ConfigData) { documents(d); d.PaymentTerms.Stages[1].ReleaseControl = "telex_release" },
			"SCHEMA_INVALID /args/data/stages/1/release_control"},
		{KindCreditPolicy, func(d *ConfigData) {
			p := d.CreditPolicy
			p.DocumentsSecured, p.TwoApproversAboveTop, p.ReservationValidityDays, p.DecisionValidityDays = true, true, 3650, 1
		}, "OK"},
		{KindCreditPolicy, func(d *ConfigData) { d.CreditPolicy.ReservationValidityDays = 0 }, "SCHEMA_INVALID /args/data/reservation_validity_days"},
		{KindCreditPolicy, func(d *ConfigData) { d.CreditPolicy.ReservationValidityDays = 3651 },
			"SCHEMA_INVALID /args/data/reservation_validity_days"},
		{KindCreditPolicy, func(d *ConfigData) { d.CreditPolicy.DecisionValidityDays = 0 }, "SCHEMA_INVALID /args/data/decision_validity_days"},
		{KindShipmentDocumentTemplate, func(*ConfigData) {}, "OK"},
		{KindShipmentDocumentTemplate, pl, "OK"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) {
			pl(d)
			d.ShipmentDocumentTemplate.Language = LanguageZhCN
			d.ShipmentDocumentTemplate.Sections = append(d.ShipmentDocumentTemplate.Sections, "consignee", "notify", "signatures")
		}, "OK"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) { slices.Reverse(d.ShipmentDocumentTemplate.Sections) }, "OK"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) {
			pl(d)
			d.ShipmentDocumentTemplate.Sections = slices.DeleteFunc(d.ShipmentDocumentTemplate.Sections, func(s string) bool { return s == "marks" })
		}, "SCHEMA_INVALID /args/data/sections"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) { d.ShipmentDocumentTemplate.Sections[5] = "packages" },
			"SCHEMA_INVALID /args/data/sections/5"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) { pl(d); d.ShipmentDocumentTemplate.Sections[4] = "lines" },
			"SCHEMA_INVALID /args/data/sections/4"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) { d.ShipmentDocumentTemplate.Sections[1] = "seller" },
			"SCHEMA_INVALID /args/data/sections/1"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) { d.ShipmentDocumentTemplate.DocumentKind = OrderDocumentProformaInvoice },
			"SCHEMA_INVALID /args/data/document_kind"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) { d.ShipmentDocumentTemplate.RendererVersion = OrderRendererVersion },
			"SCHEMA_INVALID /args/data/renderer_version"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) { d.ShipmentDocumentTemplate.Language = "fr" }, "SCHEMA_INVALID /args/data/language"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) { d.ShipmentDocumentTemplate.Title = " " }, "SCHEMA_INVALID /args/data/title"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) { d.ShipmentDocumentTemplate.Clauses = nil }, "SCHEMA_INVALID /args/data/clauses"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) { pl(d); d.ShipmentDocumentTemplate.Clauses = []string{"c"} },
			"SCHEMA_INVALID /args/data/clauses"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) { d.ShipmentDocumentTemplate.Clauses = []string{strings.Repeat("条", 2001)} },
			"SCHEMA_INVALID /args/data/clauses/0"},
		{KindShipmentDocumentTemplate, func(d *ConfigData) { d.ShipmentDocumentTemplate.FooterNote = strings.Repeat("注", 5001) },
			"SCHEMA_INVALID /args/data/footer_note"},
		{KindExportTaxPolicy, func(*ConfigData) {}, "OK"},
		{KindExportTaxPolicy, func(d *ConfigData) { d.ExportTaxPolicy.RefundMethod = RefundExemptCreditRefund }, "OK"},
		{KindExportTaxPolicy, func(d *ConfigData) { d.ExportTaxPolicy.RefundMethod = RefundNotApplicable }, "OK"},
		{KindExportTaxPolicy, func(d *ConfigData) { d.ExportTaxPolicy.RefundMethod = "exempt" }, "SCHEMA_INVALID /args/data/refund_method"},
		{KindExportTaxPolicy, func(d *ConfigData) { d.ExportTaxPolicy.PolicyBasis = "" }, "SCHEMA_INVALID /args/data/policy_basis"},
		{KindExportTaxPolicy, func(d *ConfigData) { d.ExportTaxPolicy.ProfessionalReviewEvidenceID = "" },
			"SCHEMA_INVALID /args/data/professional_review_evidence_id"},
		{KindExportTaxPolicy, func(d *ConfigData) { d.ExportTaxPolicy.Name = "" }, "SCHEMA_INVALID /args/data/name"},
		{KindNumberSequence, func(d *ConfigData) { d.NumberSequence.DocumentType = DocumentTypePackingList }, "OK"},
		{KindDocumentTemplate, func(d *ConfigData) { d.DocumentTemplate.DocumentType = DocumentTypePackingList }, "OK"},
	} {
		d := valid()[c.kind]
		c.edit(&d)
		if got := code(save(c.kind, d).Validate()); got != c.want {
			t.Errorf("%s: %s, want %s", c.kind, got, c.want)
		}
	}
	if len(kinds) != 22 || kinds[20] != KindShipmentDocumentTemplate || kinds[21] != KindExportTaxPolicy ||
		!KindImplemented(KindShipmentDocumentTemplate) || !KindImplemented(KindExportTaxPolicy) {
		t.Fatal("shipment_document_template and export_tax_policy are the twenty-first and twenty-second kinds")
	}
	need, may := ShipmentDocumentSections(ShipmentDocumentPackingList)
	need[0] = "changed"
	if again, _ := ShipmentDocumentSections(ShipmentDocumentPackingList); again[0] != "seller" || len(again) != 8 || len(may) != 4 {
		t.Fatal("the section lists are copies")
	}
	if need, may = ShipmentDocumentSections("bill_of_lading"); need != nil || may != nil {
		t.Fatal("an unknown kind has no sections")
	}
}

// Credit and settlement rates are looked up like the others; a FoundationSnapshot is captured for
// a calculation only (cost_estimate or quotation), as the database requires.
func TestS5ExchangeRatePurposes(t *testing.T) {
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	for _, purpose := range []string{PurposeCostEstimate, PurposeQuotation, PurposeCredit, PurposeSettlement} {
		if got := code(FXQuery{FromCurrency: "CNY", ToCurrency: "USD", Purpose: purpose, At: at}.Validate()); got != "OK" {
			t.Errorf("%s: %s", purpose, got)
		}
	}
	snapshot := CaptureSnapshot{IdempotencyKey: "k1", SnapshotID: "s1", Purpose: PurposeQuotation,
		Refs: []ConfigRef{{Kind: KindFXRate, ConfigID: "usd.cny", Revision: 1}}}
	if got := code(snapshot.Validate()); got != "OK" {
		t.Fatal(got)
	}
	for _, purpose := range []string{PurposeCredit, PurposeSettlement} {
		snapshot.Purpose = purpose
		if got := code(snapshot.Validate()); got != "SCHEMA_INVALID /args/purpose" {
			t.Errorf("a %s snapshot: %s", purpose, got)
		}
	}
}
