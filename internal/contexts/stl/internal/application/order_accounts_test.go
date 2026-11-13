package application

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Unit B6b (S5 plan D-4, D-18; STL-AGG-OrderAccount): an activation opens the account only while
// TRD's order is active, intact and at exactly the activated binding; an order TRD reports ended or
// at a later revision opens nothing (the payment plan review owns later revisions).
func TestActivationOpensOnlyTheActivatedRevision(t *testing.T) {
	e := activation(time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC))
	active := Order{Binding: OrderBinding{OrderID: "order.1", OrderRevision: 1, OrderHash: hashA}, Status: OrderActive}
	if !(AccountBasis{Order: &active}).opens(e) {
		t.Fatal("the active order at the activated binding opens")
	}
	if (AccountBasis{}).opens(e) {
		t.Fatal("an order TRD does not show opens nothing")
	}
	for name, change := range map[string]func(o *Order){
		"ended":      func(o *Order) { o.Status = "cancelled" },
		"amended":    func(o *Order) { o.Binding.OrderRevision, o.Binding.OrderHash = 2, hashB },
		"other hash": func(o *Order) { o.Binding.OrderHash = hashB },
		"not intact": func(o *Order) { o.Code = kernel.CodeReferenceInvalid },
	} {
		o := active
		change(&o)
		if (AccountBasis{Order: &o}).opens(e) {
			t.Errorf("%s: opens", name)
		}
	}
}

// order_account.open's refusals that a later delivery can pass are retried (the actor provisioned
// later, a concurrent change, a POL revision read before the transaction that changed since);
// every other refusal is for good.
func TestAccountOpenRefusalsRetriedOrForGood(t *testing.T) {
	for _, code := range []string{kernel.CodePermissionDenied, kernel.CodeVersionConflict, api.CodeReferenceStale} {
		if !retried(code) {
			t.Errorf("%s is retried", code)
		}
	}
	for _, code := range []string{kernel.CodeReferenceInvalid, kernel.CodeNotFound, api.CodeOrderNotActive, kernel.CodeInvalidState} {
		if retried(code) {
			t.Errorf("%s is for good", code)
		}
	}
}
