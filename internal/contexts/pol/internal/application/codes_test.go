package application

import (
	"maps"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/config"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/numbering"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/snapshot"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The domain decides with the codes package api declares: same code, same category.
func TestDomainCodesAreTheContractsCodes(t *testing.T) {
	domain := maps.Clone(config.Categories)
	maps.Copy(domain, numbering.Categories)
	domain[snapshot.CodeBusinessKeyConflict] = kernel.CategoryConflict
	for code, category := range domain {
		if got, ok := api.Category(code); !ok || got != category {
			t.Errorf("%s: api says %q (declared %v), domain says %q", code, got, ok, category)
		}
	}
}

func TestRulesKeepPublicationHumanOnly(t *testing.T) {
	for _, r := range Rules() {
		human := len(r.ActorKinds) == 1 && r.ActorKinds[0] == kernel.ActorHuman
		switch r.Command {
		case api.CommandPublish, api.CommandRetire, api.CommandDiscardDraft:
			if !human {
				t.Errorf("%s must be human only", r.Command)
			}
		default:
			if human {
				t.Errorf("%s need not be human only", r.Command)
			}
		}
	}
}
