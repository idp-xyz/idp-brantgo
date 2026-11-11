package httpapi

import (
	"context"
	"reflect"
	"testing"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
)

// Every TRD command answers the closed receipt or a receipt that embeds it, so receiptOf never
// fails on a command's answer.
func TestEveryTRDCommandAnswersAReceipt(t *testing.T) {
	commands := reflect.TypeFor[trdapi.Commands]()
	receipt := reflect.TypeFor[trdapi.Receipt]()
	for i := range commands.NumMethod() {
		m := commands.Method(i)
		r := m.Type.Out(0)
		if r == receipt {
			continue
		}
		f, ok := r.FieldByName("Receipt")
		if !ok || !f.Anonymous || f.Type != receipt {
			t.Errorf("%s answers %s, which does not embed trdapi.Receipt", m.Name, r)
		}
	}
	if n := len(trdCommands(struct{ TRD }{}).commands); n != commands.NumMethod() {
		t.Errorf("%d TRD commands are routed, trd/api has %d", n, commands.NumMethod())
	}
}

func TestReceiptOfUnwrapsTheEmbeddedReceipt(t *testing.T) {
	freeze := func(_ context.Context, c trdapi.Freeze) (trdapi.FreezeReceipt, error) {
		return trdapi.FreezeReceipt{Receipt: trdapi.Receipt{ObjectID: c.QuoteID, RowVersion: 2, Status: trdapi.StatusFrozen}, BusinessHash: "h"}, nil
	}
	r, err := receiptOf(freeze)(context.Background(), trdapi.Freeze{QuoteID: "quote.1"})
	if err != nil || r != (trdapi.Receipt{ObjectID: "quote.1", RowVersion: 2, Status: trdapi.StatusFrozen}) {
		t.Fatalf("%+v, %v", r, err)
	}
}
