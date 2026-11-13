package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ActionReconciliation v1 (Context Map R68): every SUP command has a ledger identity — its name for
// its kind, its key, and the hash of what the command hashed before reconciliation existed (the
// business arguments without the key and the expected version) — and a kind SUP does not know has
// none.
func TestEveryCommandHasItsLedgerIdentity(t *testing.T) {
	s := Service{}
	ev := []api.Evidence{{SourceID: "src.1", Locator: "第1页"}}
	for _, c := range []struct {
		cmd  api.CommandRequest
		name string
		args any // the business arguments as the command hashed them
	}{
		{api.SaveSupplierRfq{IdempotencyKey: "k", SupplierRfqID: "srfq.1", ExpectedVersion: 3}, api.CommandSaveSupplierRfq,
			struct {
				ID   string
				Data api.SupplierRfq
			}{"srfq.1", api.SupplierRfq{}}},
		{api.RecordSupplierRfqIssued{IdempotencyKey: "k", SupplierRfqID: "srfq.1", ExpectedVersion: 2, Evidence: ev, Reason: "已发送"},
			api.CommandIssueSupplierRfq, struct {
				ID       string
				Evidence []api.Evidence
				Reason   string
			}{"srfq.1", ev, "已发送"}},
		{api.CloseSupplierRfq{IdempotencyKey: "k", SupplierRfqID: "srfq.1", ExpectedVersion: 2, Evidence: ev, Reason: "关闭"},
			api.CommandCloseSupplierRfq, struct {
				ID       string
				Evidence []api.Evidence
				Reason   string
			}{"srfq.1", ev, "关闭"}},
		{api.SaveSupplierOffer{IdempotencyKey: "k", OfferID: "offer.1", ExpectedVersion: 1}, api.CommandSaveSupplierOffer,
			struct {
				Kind string
				ID   string
				Data any
			}{api.KindSupplierOffer, "offer.1", &api.SupplierOffer{}}},
		{api.SavePurchaseTerms{IdempotencyKey: "k", TermsID: "terms.1"}, api.CommandSavePurchaseTerms,
			struct {
				Kind string
				ID   string
				Data any
			}{api.KindPurchaseTerms, "terms.1", &api.PurchaseTerms{}}},
		{api.SaveLogisticsOffer{IdempotencyKey: "k", OfferID: "log.1"}, api.CommandSaveLogisticsOffer,
			struct {
				Kind string
				ID   string
				Data any
			}{api.KindLogisticsOffer, "log.1", &api.LogisticsOffer{}}},
		{api.Revise{IdempotencyKey: "k", Kind: api.KindPurchaseTerms, ID: "terms.1", ExpectedVersion: 4, BaseRevision: 1, Reason: "改"},
			api.CommandRevisePurchaseTerms, struct {
				Kind, ID     string
				BaseRevision int64
				Reason       string
			}{api.KindPurchaseTerms, "terms.1", 1, "改"}},
		{api.DiscardDraft{IdempotencyKey: "k", Kind: api.KindLogisticsOffer, ID: "log.1", ExpectedVersion: 2, WorkingRevision: 1, Reason: "弃"},
			api.CommandDiscardLogisticsOffer, struct {
				Kind, ID        string
				WorkingRevision int64
				Reason          string
			}{api.KindLogisticsOffer, "log.1", 1, "弃"}},
		{api.Seal{IdempotencyKey: "k", Kind: api.KindPurchaseTerms, ID: "terms.1", ExpectedVersion: 2, WorkingRevision: 1, Evidence: ev},
			api.CommandPublishPurchaseTerms, struct {
				Kind, ID           string
				WorkingRevision    int64
				SupersedesRevision int64
				Evidence           []api.Evidence
			}{api.KindPurchaseTerms, "terms.1", 1, 0, ev}},
		{api.End{IdempotencyKey: "k", Kind: api.KindSupplierOffer, ID: "offer.1", ExpectedVersion: 3, TargetRevision: 1, Reason: "撤", Evidence: ev},
			api.CommandWithdrawSupplierOffer, struct {
				Kind, ID       string
				TargetRevision int64
				Reason         string
				Evidence       []api.Evidence
			}{api.KindSupplierOffer, "offer.1", 1, "撤", ev}},
	} {
		want, err := action.RequestHash(c.args)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.RequestOf(c.cmd)
		if err != nil || got != (kernel.ActionRequest{Command: c.name, IdempotencyKey: "k", RequestHash: want}) {
			t.Errorf("%T: %+v %v, want %s %s", c.cmd, got, err, c.name, want)
		}
	}
	for _, cmd := range []api.CommandRequest{api.Revise{Kind: "quote"}, api.DiscardDraft{Kind: ""}, api.Seal{Kind: "rfq"}, api.End{Kind: "x"}} {
		if _, err := s.RequestOf(cmd); err == nil {
			t.Errorf("%T of an unknown kind has a ledger identity", cmd)
		} else if rej, ok := kernel.AsRejection(err); !ok || rej.Code != kernel.CodeSchemaInvalid {
			t.Errorf("%T: %v", cmd, err)
		}
	}
}
