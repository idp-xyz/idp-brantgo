package authorization

import (
	"errors"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// BC-D02 (simulated review SR-32): fifteen default templates, and the Q01 AI execution identity the
// owner added for S4 (decision D-7, 2026-11-12).
func TestTheDefaultTemplatesAreTheReviewedOnes(t *testing.T) {
	var names []string
	for _, tpl := range Templates() {
		names = append(names, tpl.Name)
		for _, p := range tpl.Permissions {
			if !kernel.ValidIdentifier(p) {
				t.Errorf("%s holds an invalid permission %q", tpl.Code, p)
			}
		}
	}
	want := []string{"销售经办", "跟单", "采购", "核价", "报价审核人", "订单审核人", "毛利审批人", "信用专员", "信用经理", "质检", "单证", "资金", "合规", "税务专员", "管理员", "Q01 AI 执行身份"}
	if !slices.Equal(names, want) {
		t.Fatalf("templates %v", names)
	}
}

// No template holds both sides of a forbidden combination, and an exclusive permission is held by
// its template only: "合规结论只由合规岗位确定", "销售不能放行信用决定".
func TestTheCatalogRespectsItsOwnRules(t *testing.T) {
	for _, tpl := range Templates() {
		if err := CheckSeparation([]string{tpl.Code}); err != nil {
			t.Errorf("template %s alone: %v", tpl.Code, err)
		}
	}
	for _, e := range ExclusivePermissions() {
		for _, tpl := range Templates() {
			if slices.Contains(tpl.Permissions, e.Permission) != (tpl.Code == e.Template) {
				t.Errorf("%s: held by %s (%s)", e.Permission, tpl.Code, e.Rule)
			}
		}
	}
}

func TestForbiddenCombinationsAreRefused(t *testing.T) {
	for _, c := range []struct {
		codes []string
		ok    bool
	}{
		{[]string{"sales_clerk", "merchandiser"}, true},
		{[]string{"sales_clerk", "quote_reviewer"}, false},
		{[]string{"merchandiser", "credit_manager", "sales_clerk"}, false},
		{[]string{"purchaser", "order_reviewer"}, false},
		{[]string{"compliance", "administrator"}, true},
	} {
		err := CheckSeparation(c.codes)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrSeparationOfDuties)) {
			t.Errorf("%v: %v", c.codes, err)
		}
	}
}

func TestPermissionsOfMergesTemplates(t *testing.T) {
	p, err := permissionsOf([]string{"sales_clerk", "merchandiser"})
	if err != nil || !slices.Contains(p, "rfq.confirm_scope") || !slices.Contains(p, "shipment.confirm") || slices.Contains(p, "party.confirm") {
		t.Fatalf("%v %v", len(p), err)
	}
	if _, err = permissionsOf([]string{"nobody"}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("unknown template: %v", err)
	}
}
