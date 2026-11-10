package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/mapping"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/sku"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The domain decides with the codes package api declares: same code, same category.
func TestDomainCodesAreTheContractsCodes(t *testing.T) {
	for code, err := range map[string]error{
		mapping.CodeBusinessKeyConflict: mapping.CheckKey(true),
		sku.CodeReferenceStale:          sku.CheckSpec(&sku.Sku{Status: sku.Confirmed, Revision: 2}, true, 1),
	} {
		rej, ok := kernel.AsRejection(err)
		if !ok || rej.Code != code {
			t.Fatalf("%s: %v", code, err)
		}
		if got, declared := api.Category(code); !declared || got != rej.Category {
			t.Errorf("%s: api says %q (declared %v), domain says %q", code, got, declared, rej.Category)
		}
	}
}

func TestRulesKeepConfirmationAndPublicationHumanOnly(t *testing.T) {
	for _, r := range Rules() {
		human := len(r.ActorKinds) == 1 && r.ActorKinds[0] == kernel.ActorHuman
		switch r.Command {
		case api.CommandConfirmSku, api.CommandDeactivateSku, api.CommandPublishItemCodeMapping:
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
