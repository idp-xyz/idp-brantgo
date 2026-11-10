package application

import (
	"maps"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/budget"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/delivery"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/rules"
)

// The domain decides with the codes package api declares: same code, same category. The domain
// may not import api (package rules), so the comparison lives here.
func TestRuleCodesAreTheContractsCodes(t *testing.T) {
	domain := maps.Clone(rules.Categories)
	maps.Copy(domain, delivery.Categories)
	maps.Copy(domain, budget.Categories)
	for code, category := range domain {
		if got, ok := api.Category(code); !ok || got != category {
			t.Errorf("%s: api says %q (declared %v), domain says %q", code, got, ok, category)
		}
	}
}
