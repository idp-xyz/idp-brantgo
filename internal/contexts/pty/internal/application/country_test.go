package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/party"
	"go.idp.xyz/idp-brant/internal/foundation/action"
)

// S5 A3b, D-8: an address country is optional, and adding it changes no hash of a party saved
// without one. party.save_draft's request hash is what a replay with the same idempotency key is
// compared against (contract C6); the two vectors below were computed on the code before the
// country existed (s5/business 87a55fa), so a request stored by S1–S3 still replays and is not
// refused as a different request. An address with a country hashes differently, as a different
// request must.
func TestAddressCountryKeepsTheSaveRequestHash(t *testing.T) {
	addresses := []party.Address{{ID: "hq", Role: party.AddressBuyer, Text: "1 Synthetic Road, Shanghai"},
		{ID: "dock", Role: party.AddressConsignee, Text: "Pier 9, Rotterdam"}}
	for _, c := range []struct {
		addresses []party.Address
		want      string
	}{
		{nil, "25b4714cfcdee5e457fdf16f92a93f8b62850aa11c71604e6eb56e4e1e4f0502"},
		{addresses, "cf0ac5c088ad04311f5cb263dff0db8c6b524a43c8cc0d9b3deca2f9c04f24b0"},
	} {
		got, err := action.RequestHash(saveArgs("customer.1", "Acme Trading", []string{"customer"}, c.addresses))
		if err != nil || got != c.want {
			t.Errorf("%d addresses: %s %v, want %s", len(c.addresses), got, err, c.want)
		}
	}
	with := []party.Address{addresses[0], addresses[1]}
	with[1].Country = "NL"
	got, err := action.RequestHash(saveArgs("customer.1", "Acme Trading", []string{"customer"}, with))
	if err != nil || got == "cf0ac5c088ad04311f5cb263dff0db8c6b524a43c8cc0d9b3deca2f9c04f24b0" {
		t.Fatalf("a stated country is part of the request: %s %v", got, err)
	}
}
