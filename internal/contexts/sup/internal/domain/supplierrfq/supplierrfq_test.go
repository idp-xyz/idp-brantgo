package supplierrfq

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/domain/revision"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func rejected(t *testing.T, err error, code string) {
	t.Helper()
	if r, ok := kernel.AsRejection(err); !ok || r.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

// SM-SUP-SRFQ (T02 §13.1, §13.2): a draft is saved whole; recording the issue seals it at its issued
// version; closing keeps the issued version, and an offer may answer only that version.
func TestSupplierRfqStates(t *testing.T) {
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	human := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "buyer"}
	evidence := []revision.Evidence{{SourceID: "email.1", Locator: "sent"}}
	rejected(t, CheckSave(nil, false, 1), kernel.CodeNotFound)
	if err := CheckSave(nil, false, 0); err != nil {
		t.Fatal(err)
	}
	r := New("request.1", "buyer", "trade")
	r.Replace([]byte(`{}`), "h1", human, 1, at)
	r.Replace([]byte(`{"rfq_line_ids":["L1"]}`), "h2", human, 1, at)
	if r.RowVersion != 2 || r.Hash != "h2" || r.Answerable(2) {
		t.Fatalf("draft %+v", r)
	}
	rejected(t, Check(r, true, 2, Issued), kernel.CodeInvalidState)
	rejected(t, Check(r, false, 2, Draft), kernel.CodeNotFound)
	rejected(t, Check(r, true, 1, Draft), kernel.CodeVersionConflict)
	r.RecordIssued(human, 3, "人工发送", evidence, at)
	if r.Status != Issued || r.RowVersion != 3 || r.IssuedVersion != 3 || r.Issue.Epoch != 3 || !r.Answerable(3) || r.Answerable(2) {
		t.Fatalf("issued %+v", r)
	}
	rejected(t, CheckSave(r, true, 3), kernel.CodeInvalidState)
	r.Close(human, 3, "已收到报价", evidence, at)
	if r.Status != Closed || r.RowVersion != 4 || r.IssuedVersion != 3 || r.Closing == nil || !r.Answerable(3) {
		t.Fatalf("closed %+v", r)
	}
	rejected(t, Check(r, true, 4, Issued), kernel.CodeInvalidState)
	var none *SupplierRfq
	if none.Answerable(3) {
		t.Fatal("no request is answerable")
	}
}
