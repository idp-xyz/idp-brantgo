package application

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/declaration"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/mapping"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/sku"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The domain decides with the codes package api declares: same code, same category.
func TestDomainCodesAreTheContractsCodes(t *testing.T) {
	draft := &declaration.Root{LastRevision: 1, Draft: &declaration.Revision{Number: 1, SavedBy: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "doc.1"}}}
	_, independence := draft.CheckVerify(1, kernel.ActorRef{Kind: kernel.ActorHuman, ID: "doc.1"})
	for code, err := range map[string]error{
		mapping.CodeBusinessKeyConflict:                        mapping.CheckKey(true),
		sku.CodeReferenceStale:                                 sku.CheckSpec(&sku.Sku{Status: sku.Confirmed, Revision: 2}, true, 1),
		declaration.CodeBusinessKeyConflict + " (declaration)": declaration.CheckKey(true),
		declaration.CodeIndependentReviewRequired:              independence,
		declaration.CodeDeclarationProfileUnverified:           declaration.Reject(declaration.CodeDeclarationProfileUnverified, "/x", ""),
		declaration.CodeReferenceStale + " (declaration)":      declaration.Reject(declaration.CodeReferenceStale, "/x", ""),
	} {
		code, _, _ = strings.Cut(code, " ")
		rej, ok := kernel.AsRejection(err)
		if !ok || rej.Code != code {
			t.Fatalf("%s: %v", code, err)
		}
		if got, declared := api.Category(code); !declared || got != rej.Category {
			t.Errorf("%s: api says %q (declared %v), domain says %q", code, got, declared, rej.Category)
		}
	}
}

// A DeclarationProfileRef v1 answer passes CAT's verdict on with the contract's code and category.
func TestDeclarationAnswersCheckWithTheContractsCodes(t *testing.T) {
	for _, c := range []string{declaration.CodeDeclarationProfileUnverified, declaration.CodeReferenceStale, kernel.CodeReferenceInvalid} {
		rej, ok := kernel.AsRejection(api.ResolvedDeclarationProfile{Code: c}.Check("/lines/0"))
		want, declared := api.Category(c)
		if c == kernel.CodeReferenceInvalid {
			want, declared = kernel.CategoryInvalid, true
		}
		if !ok || !declared || rej.Code != c || rej.Category != want || rej.Fields[0].Path != "/lines/0" {
			t.Errorf("%s: %+v", c, rej)
		}
	}
}

func TestRulesKeepConfirmationAndPublicationHumanOnly(t *testing.T) {
	for _, r := range Rules() {
		human := len(r.ActorKinds) == 1 && r.ActorKinds[0] == kernel.ActorHuman
		switch r.Command {
		case api.CommandConfirmSku, api.CommandDeactivateSku, api.CommandPublishItemCodeMapping, api.CommandVerifyDeclarationProfile:
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
