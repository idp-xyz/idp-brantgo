package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/profile"
	"go.idp.xyz/idp-brant/internal/foundation/action"
)

// S5R-D21: trading_profile.save_draft hashes the canonical content, which omits permitted payers
// when there are none, so a stored save replays after the upgrade. The hash was computed by the code
// of 90925f3 (the S5R base, before R2) and is pinned here.
func TestS5RTradingProfileSaveKeepsItsRequestHash(t *testing.T) {
	c := profile.Content{PartyRevision: 3, PaymentTerms: profile.TermsRef{Kind: "payment_terms", ConfigID: "payment.cad", Revision: 2},
		TradeTerms: profile.TermsRef{Kind: "trade_terms", ConfigID: "trade.cif", Revision: 1}, Owner: profile.Owner{ActorID: "sales.1", TeamID: "trade"},
		Contacts: []profile.ContactRef{{ContactID: "contact.b", RowVersion: 2}, {ContactID: "contact.a", RowVersion: 1}}}
	hash := func(c profile.Content) string {
		h, err := action.RequestHash(struct {
			ID      string
			Content profile.Content
		}{"customer.1", c.Canonical()})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if got := hash(c); got != "f329cbc45d83e137abce670d4f8ee3670c11900b01b5bbc6594f63119bd49900" {
		t.Fatalf("a save without payers hashes as %s", got)
	}
	c.PermittedPayers = []string{"payer.1"}
	if hash(c) == "f329cbc45d83e137abce670d4f8ee3670c11900b01b5bbc6594f63119bd49900" {
		t.Fatal("permitted payers are part of the request")
	}
}
