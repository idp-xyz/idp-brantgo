package authorization

import (
	"slices"
	"strings"
	"testing"
)

// S5R role template decisions (unit R1, WBS S5R-01; simulated sign-off E0 of 2026-11-13), on the
// catalog roles_gen.go generates from the archive's role templates: what each decision gives a
// template, takes from it, or leaves where it was. The commands' own checks (FUL, STL, POL, PUR)
// prove the refusals and acceptances against PostgreSQL in internal/bootstrap
// (s5r_roles_integration_test.go); this test pins the templates those checks read.
func TestS5RTemplateDecisions(t *testing.T) {
	for _, c := range []struct {
		decision, template, permission string
		held                           bool
	}{
		// S5R-D01: the shipment confirmation (the logistics fact "goods handed to the carrier") stays
		// the merchandiser's; documentation evaluates the commercial release, issues and releases the
		// documents. A tenant merges the two templates when one person does both.
		{"S5R-D01", "merchandiser", "shipment.confirm", true},
		{"S5R-D01", "documentation", "shipment.confirm", false},
		{"S5R-D01", "documentation", "commercial_release.evaluate", true},
		{"S5R-D01", "documentation", "shipment_document.issue", true},
		{"S5R-D01", "documentation", "document_release.record", true},
		{"S5R-D01", "merchandiser", "commercial_release.evaluate", false},
		// S5R-D02: order_account.open is a permission of its own, treasury's (re-creating an account)
		// besides the system consumer; no sales template and no AI identity holds it.
		{"S5R-D02", "treasury", "order_account.open", true},
		{"S5R-D02", "treasury", "payment_plan_review.open", true},
		{"S5R-D02", "sales_clerk", "order_account.open", false},
		{"S5R-D02", "sales_clerk", "payment_plan_review.open", false},
		{"S5R-D02", "ai_q01_agent", "order_account.open", false},
		// S5R-D25: documentation numbers the commercial invoice and the packing list itself (S5-D34's
		// rule for the purchaser); the Q01 AI identity issues no number.
		{"S5R-D25", "documentation", "number.issue", true},
		{"S5R-D25", "documentation", "shipment_document.prepare", true},
		{"S5R-D25", "ai_q01_agent", "number.issue", false},
		// S5R-D26: who decides on bank advices, bills, invoices and tax papers registers them.
		{"S5R-D26", "treasury", "source.register", true},
		{"S5R-D26", "treasury", "file.register", true},
		{"S5R-D26", "credit_manager", "source.register", true},
		{"S5R-D26", "credit_manager", "file.register", true},
		{"S5R-D26", "credit_officer", "source.register", true},
		{"S5R-D26", "credit_officer", "file.register", true},
		{"S5R-D26", "tax_specialist", "source.register", true},
		{"S5R-D26", "tax_specialist", "file.register", true},
		{"S5R-D26", "compliance", "source.register", true},
		{"S5R-D26", "compliance", "file.register", true},
		{"S5R-D26", "order_reviewer", "source.register", false},
		{"S5R-D26", "quote_reviewer", "source.register", false},
		// S5R-D27: order.activate stays the sales clerk's; the order reviewer decides, never executes.
		{"S5R-D27", "sales_clerk", "order.activate", true},
		{"S5R-D27", "order_reviewer", "order.activate", false},
		{"S5R-D27", "order_reviewer", "order.review.decide", true},
		// S5R-D28: the exception to a commercial release stays credit_manager's alone (its
		// independence from the evaluator is FUL's check, unit R6); documentation evaluates.
		{"S5R-D28", "credit_manager", "commercial_release.grant_exception", true},
		{"S5R-D28", "documentation", "commercial_release.grant_exception", false},
		// S5R-D29: the review openings are the system consumers'; the humans settle the reviews, and
		// treasury keeps payment_plan_review.open (re-creation, as S5R-D02).
		{"S5R-D29", "merchandiser", "shipment_review.open", false},
		{"S5R-D29", "merchandiser", "shipment_review.settle", true},
		{"S5R-D29", "purchaser", "allocation_review.open", false},
		{"S5R-D29", "purchaser", "allocation_review.settle", true},
		// S5R-D30: publication by kind; the specialist holds config.publish with its kind capability,
		// the administrator publishes the other kinds (and retires).
		{"S5R-D30", "credit_manager", "config.publish.credit_policy", true},
		{"S5R-D30", "credit_manager", "config.publish", true},
		{"S5R-D30", "tax_specialist", "config.publish.export_tax_policy", true},
		{"S5R-D30", "tax_specialist", "config.publish", true},
		{"S5R-D30", "compliance", "config.publish.screening_regulation", true},
		{"S5R-D30", "compliance", "config.publish", true},
		{"S5R-D30", "administrator", "config.publish", true},
		{"S5R-D30", "administrator", "config.publish.credit_policy", false},
		{"S5R-D30", "administrator", "config.publish.export_tax_policy", false},
		{"S5R-D30", "administrator", "config.publish.screening_regulation", false},
		// S5R-D32: the Q01 AI template is not widened for FUL; a future documentation or merchandiser
		// AI template gets the reads its task needs.
		{"S5R-D32", "ai_q01_agent", "ful.read", false},
	} {
		tpl, ok := TemplateByCode(c.template)
		if !ok {
			t.Fatalf("%s: no template %s", c.decision, c.template)
		}
		if slices.Contains(tpl.Permissions, c.permission) != c.held {
			t.Errorf("%s: %s holds %s = %v, want %v", c.decision, c.template, c.permission, !c.held, c.held)
		}
	}
}

// The permissions S5R gives to one template or to the system consumers only: S5R-D02
// (order_account.open: treasury; the consumer system:inbox.stl.order_account is granted by
// brant-admin inbox, never by a template), S5R-D29 (the review openings: no template), S5R-D30 (the
// kind capabilities). Every holder of a kind capability holds config.publish too, which admits the
// command that checks the capability (role templates principle 6).
func TestS5RPermissionsHeldByOneTemplateOrNone(t *testing.T) {
	for permission, want := range map[string][]string{
		"order_account.open":                  {"treasury"},
		"shipment_review.open":                nil,
		"allocation_review.open":              nil,
		"config.publish.credit_policy":        {"credit_manager"},
		"config.publish.export_tax_policy":    {"tax_specialist"},
		"config.publish.screening_regulation": {"compliance"},
	} {
		var got []string
		for _, tpl := range Templates() {
			if slices.Contains(tpl.Permissions, permission) {
				got = append(got, tpl.Code)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s is held by %v, want %v", permission, got, want)
		}
	}
	for _, tpl := range Templates() {
		for _, p := range tpl.Permissions {
			if strings.HasPrefix(p, "config.publish.") && !slices.Contains(tpl.Permissions, "config.publish") {
				t.Errorf("%s holds %s without config.publish", tpl.Code, p)
			}
		}
	}
	// The two exclusive kind capabilities are registered as such (export_tax_policy is not: a
	// tenant may give it to a second tax template).
	for _, p := range []string{"config.publish.credit_policy", "config.publish.screening_regulation"} {
		if !slices.ContainsFunc(ExclusivePermissions(), func(e ExclusivePermission) bool { return e.Permission == p }) {
			t.Errorf("%s is not an exclusive permission", p)
		}
	}
}

// The duties texts carry the S5R decisions that only move a duty between templates (S5R-D01) or
// add one (S5R-D02, D25, D26), so a tenant reading the template sees them.
func TestS5RDutiesNameTheDecisions(t *testing.T) {
	for template, decisions := range map[string][]string{
		"merchandiser":  {"S5R-D01"},
		"treasury":      {"S5R-D02", "S5R-D26"},
		"documentation": {"S5R-D25"},
	} {
		tpl, _ := TemplateByCode(template)
		for _, d := range decisions {
			if !strings.Contains(tpl.Duties, d) {
				t.Errorf("%s duties lack %s: %s", template, d, tpl.Duties)
			}
		}
	}
}
