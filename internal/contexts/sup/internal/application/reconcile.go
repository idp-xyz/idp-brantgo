package application

import (
	"context"

	"go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// request returns the name, the idempotency key and the business arguments of cmd, which its
// request hash covers: everything but the key and the expected version, so a replay with another
// CAS value still matches (Python test_replay_ignores_changed_cas_but_not_changed_business_fields).
// Execution and reconciliation hash through it alike. The name is "" for a supply condition kind
// SUP does not know.
func request(cmd api.CommandRequest) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.SaveSupplierRfq:
		return api.CommandSaveSupplierRfq, c.IdempotencyKey, struct {
			ID   string
			Data api.SupplierRfq
		}{c.SupplierRfqID, c.Data}
	case api.RecordSupplierRfqIssued:
		return api.CommandIssueSupplierRfq, c.IdempotencyKey, struct {
			ID       string
			Evidence []api.Evidence
			Reason   string
		}{c.SupplierRfqID, c.Evidence, c.Reason}
	case api.CloseSupplierRfq:
		return api.CommandCloseSupplierRfq, c.IdempotencyKey, struct {
			ID       string
			Evidence []api.Evidence
			Reason   string
		}{c.SupplierRfqID, c.Evidence, c.Reason}
	case api.SaveSupplierOffer:
		return api.CommandSaveSupplierOffer, c.IdempotencyKey, saveArgs(api.KindSupplierOffer, c.OfferID, &c.Data)
	case api.SavePurchaseTerms:
		return api.CommandSavePurchaseTerms, c.IdempotencyKey, saveArgs(api.KindPurchaseTerms, c.TermsID, &c.Data)
	case api.SaveLogisticsOffer:
		return api.CommandSaveLogisticsOffer, c.IdempotencyKey, saveArgs(api.KindLogisticsOffer, c.OfferID, &c.Data)
	case api.Revise:
		return kindCommand(c.Kind, api.ActionRevise), c.IdempotencyKey, struct {
			Kind, ID     string
			BaseRevision int64
			Reason       string
		}{c.Kind, c.ID, c.BaseRevision, c.Reason}
	case api.DiscardDraft:
		return kindCommand(c.Kind, api.ActionDiscardDraft), c.IdempotencyKey, struct {
			Kind, ID        string
			WorkingRevision int64
			Reason          string
		}{c.Kind, c.ID, c.WorkingRevision, c.Reason}
	case api.Seal:
		return kindCommand(c.Kind, api.SealAction(c.Kind)), c.IdempotencyKey, struct {
			Kind, ID           string
			WorkingRevision    int64
			SupersedesRevision int64
			Evidence           []api.Evidence
		}{c.Kind, c.ID, c.WorkingRevision, c.SupersedesRevision, c.Evidence}
	case api.End:
		return kindCommand(c.Kind, api.EndAction(c.Kind)), c.IdempotencyKey, struct {
			Kind, ID       string
			TargetRevision int64
			Reason         string
			Evidence       []api.Evidence
		}{c.Kind, c.ID, c.TargetRevision, c.Reason, c.Evidence}
	}
	return "", "", nil
}

// saveArgs are the business arguments of a supply condition's save_draft: its kind, its ID and its
// content.
func saveArgs(kind, id string, data any) any {
	return struct {
		Kind string
		ID   string
		Data any
	}{kind, id, data}
}

// kindCommand is the command name of action on a supply condition kind SUP knows, "" otherwise.
func kindCommand(kind, action string) string {
	if !api.KindKnown(kind) {
		return ""
	}
	return api.Command(kind, action)
}

// RequestOf returns the ledger identity of cmd, hashed as executing it hashes it
// (api.ActionReconciler; ActionReconciliation v1, Context Map R68).
func (s Service) RequestOf(cmd api.CommandRequest) (kernel.ActionRequest, error) {
	name, key, args := request(cmd)
	if name == "" {
		return kernel.ActionRequest{}, kernel.Reject(kernel.CodeSchemaInvalid, "/command", "")
	}
	hash, err := action.RequestHash(args)
	if err != nil {
		return kernel.ActionRequest{}, err
	}
	return kernel.ActionRequest{Command: name, IdempotencyKey: key, RequestHash: hash}, nil
}

// Reconcile reports what became of cmd, sent earlier with its idempotency key, without executing
// it (api.Reconciler; contract 06, 动作对账).
func (s Service) Reconcile(ctx context.Context, cmd api.CommandRequest) (api.Reconciliation, error) {
	r, err := s.RequestOf(cmd)
	if err != nil {
		return api.Reconciliation{}, err
	}
	return s.ReconcileHash(ctx, r.Command, r.IdempotencyKey, r.RequestHash)
}

// ReconcileHash reports what became of the command the ledger identity names, without executing
// anything (api.ActionReconciler; ActionReconciliation v1, Context Map R68): the ledger re-checks
// sup.read and the visibility of the supplier RFQ or supply condition the stored result names now.
func (s Service) ReconcileHash(ctx context.Context, command, idempotencyKey, requestHash string) (api.Reconciliation, error) {
	r, err := s.Ledger.Reconcile(ctx, command, idempotencyKey, requestHash)
	if err != nil {
		return api.Reconciliation{}, err
	}
	out := api.Reconciliation{State: r.State, ActionID: r.ActionID, ObjectID: r.ObjectID}
	if r.Result != nil {
		receipt := api.Receipt(*r.Result)
		out.Receipt = &receipt
	}
	return out, nil
}
