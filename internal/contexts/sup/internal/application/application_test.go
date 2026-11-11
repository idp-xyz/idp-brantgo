package application

import (
	"maps"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/domain/pricing"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/domain/revision"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The domain decides with the codes package api declares: same code, same category.
func TestDomainCodesAreTheContractsCodes(t *testing.T) {
	domain := maps.Clone(revision.Categories)
	maps.Copy(domain, pricing.Categories)
	for code, category := range domain {
		if got, ok := api.Category(code); !ok || got != category {
			t.Errorf("%s: api says %q (declared %v), domain says %q", code, got, ok, category)
		}
	}
}

// 05 stage humanOnlyCommands for SUP, exactly: the verifications, publications, withdrawals,
// retirements and the supplier RFQ's issue and close are human only; drafts, revisions and discards
// are open to AI agents with the grant (SM-SUP-OFFER: discard_draft human|ai); system actors issue
// no SUP command.
func TestRulesKeepVerificationHumanOnly(t *testing.T) {
	human := []string{api.CommandIssueSupplierRfq, api.CommandCloseSupplierRfq, api.CommandVerifySupplierOffer,
		api.CommandWithdrawSupplierOffer, api.CommandPublishPurchaseTerms, api.CommandRetirePurchaseTerms,
		api.CommandVerifyLogisticsOffer, api.CommandWithdrawLogisticsOffer}
	agent := []string{api.CommandSaveSupplierRfq, api.CommandSaveSupplierOffer, api.CommandReviseSupplierOffer,
		api.CommandDiscardSupplierOffer, api.CommandSavePurchaseTerms, api.CommandRevisePurchaseTerms, api.CommandDiscardPurchaseTerms,
		api.CommandSaveLogisticsOffer, api.CommandReviseLogisticsOffer, api.CommandDiscardLogisticsOffer}
	seen := map[string]bool{}
	for _, r := range Rules() {
		seen[r.Command] = true
		switch {
		case slices.Contains(human, r.Command):
			if !slices.Equal(r.ActorKinds, []kernel.ActorKind{kernel.ActorHuman}) || r.Permission != r.Command {
				t.Errorf("%s must be human only with its own permission: %+v", r.Command, r)
			}
		case slices.Contains(agent, r.Command):
			if !slices.Equal(r.ActorKinds, humanOrAgent) || r.Permission != r.Command {
				t.Errorf("%s is for humans and AI agents with its own permission: %+v", r.Command, r)
			}
		default:
			if r.Permission != PermissionRead && r.Permission != PermissionAudit {
				t.Errorf("%s is a read of sup.read or sup.audit: %+v", r.Command, r)
			}
		}
	}
	for _, c := range append(human, agent...) {
		if !seen[c] {
			t.Errorf("%s has no rule", c)
		}
	}
	for _, kind := range []string{api.KindSupplierOffer, api.KindPurchaseTerms, api.KindLogisticsOffer} {
		if !seen[CommandGet(kind)] {
			t.Errorf("%s has no read", kind)
		}
	}
}

// Equal content hashes equally whatever the timestamps' lexical form, and the header carries what
// the guard reads.
func TestContentIsCanonical(t *testing.T) {
	a := document{kind: api.KindPurchaseTerms, terms: &api.PurchaseTerms{ValidFrom: "2026-10-22T00:00:00Z", ValidUntil: "2026-10-29T00:00:00.5Z",
		ValidityMode: api.ValidityDependent, SourceOfferRef: &api.SupplyConditionRef{Kind: api.KindSupplierOffer, ID: "offer.1", Revision: 2}}}
	b := document{kind: api.KindPurchaseTerms, terms: &api.PurchaseTerms{ValidFrom: "2026-10-22T00:00:00.000000Z", ValidUntil: "2026-10-29T00:00:00.500Z",
		ValidityMode: api.ValidityDependent, SourceOfferRef: &api.SupplyConditionRef{Kind: api.KindSupplierOffer, ID: "offer.1", Revision: 2}}}
	ca, err := a.encode()
	if err != nil {
		t.Fatal(err)
	}
	cb, err := b.encode()
	if err != nil {
		t.Fatal(err)
	}
	h := ca.Header
	if ca.Hash != cb.Hash || len(ca.Hash) != 64 || !h.Dependent() || h.SourceID != "offer.1" || h.SourceRevision != 2 ||
		h.ValidUntil.Sub(h.ValidFrom).Hours() != 168.0+0.5/3600 {
		t.Fatalf("%+v %+v", ca, cb)
	}
	other := document{kind: api.KindSupplierOffer, offer: &api.SupplierOffer{ValidFrom: "2026-10-22T00:00:00Z", ValidUntil: "2026-10-29T00:00:00.5Z"}}
	co, err := other.encode()
	if err != nil || co.Hash == ca.Hash {
		t.Fatalf("the kind is part of the hash: %v", err)
	}
	back, err := decode(api.KindPurchaseTerms, ca.Data)
	if err != nil || back.terms.ValidFrom != "2026-10-22T00:00:00.000000Z" || *back.terms.SourceOfferRef != *a.terms.SourceOfferRef {
		t.Fatalf("decoded %+v %v", back.terms, err)
	}
}

// Lookups are split into batches of distinct references the upstream contexts accept.
func TestBatches(t *testing.T) {
	ids := make([]string, 0, 450)
	for i := range 450 {
		ids = append(ids, string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	ids = append(ids, ids[:10]...)
	b := batches(ids)
	if len(b) != 3 || len(b[0]) != lookupBatch || len(b[2]) != 50 {
		t.Fatalf("%d batches", len(b))
	}
	if batches([]string{}) != nil {
		t.Fatal("nothing to ask is no batch")
	}
}
