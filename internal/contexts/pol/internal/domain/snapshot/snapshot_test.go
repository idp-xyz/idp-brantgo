package snapshot

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func TestCaptureIsCanonicalAndAdoptsEachRevisionOnce(t *testing.T) {
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	by := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "editor"}
	hash := "0000000000000000000000000000000000000000000000000000000000000000"
	usd := Entry{Kind: "currency", ConfigID: "USD", Revision: 1, ContentHash: hash}
	fx := Entry{Kind: "fx_rate", ConfigID: "usd.cny", Revision: 2, ContentHash: hash}
	cny := Entry{Kind: "currency", ConfigID: "CNY", Revision: 1, ContentHash: hash}
	a, err := Capture("s", "cost_estimate", at, by, 1, []Entry{fx, usd, cny, usd})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Capture("s", "cost_estimate", at, by, 1, []Entry{cny, usd, fx})
	quote, _ := Capture("s", "quotation", at, by, 1, []Entry{cny, usd, fx})
	if len(a.Entries) != 3 || a.Entries[0] != cny || a.Entries[2] != fx || a.Hash != b.Hash || a.Hash == quote.Hash || len(a.Hash) != 64 {
		t.Fatalf("%+v %s %s", a.Entries, a.Hash, quote.Hash)
	}
	if r, ok := kernel.AsRejection(CheckNew(true)); !ok || r.Code != CodeBusinessKeyConflict || r.Category != kernel.CategoryConflict {
		t.Fatal("a taken snapshot ID is a business key conflict")
	}
}
