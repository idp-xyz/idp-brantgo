package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	dcl "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/declaration"
	el "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/exportlicense"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// evaluatedDestination is the destination country the current evaluation of the trade compliance
// check of an order revision recorded, "" when there is none (S5 D-8: the consignee address's
// country PTY records, or the one compliance entered with a reason and evidence). An export licence
// allocation compares it with the licence's when FUL's shipment binds no consignee address with a
// country (port Exports leaves Destination ""): compliance and the licence then rest on the same
// destination. It reads CNX's own check inside CNX's transaction, without a lock: an evaluation is
// never changed, and a new one replaces it only through CNX's own commands.
func evaluatedDestination(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, revision int64) (string, error) {
	c, err := postgres.LoadCheckOfRevision(ctx, exec, sc, orderID, revision, postgres.NoLock)
	if err != nil || c == nil || c.Evaluation == "" {
		return "", err
	}
	evs, err := postgres.Evaluations(ctx, exec, sc, c.ID, c.Evaluation)
	if err != nil || len(evs) != 1 {
		return "", err
	}
	return evs[0].Destination.Country, nil
}

// declarationResult puts the declaration receipt an export licence release names into in, or checks
// that a stated result may stand (S5 B5's announced widening, CNX-INV-002 "报关改单或撤销按外部结果释放
// 用量"). With receipt_id: a receipt CNX recorded (REFERENCE_INVALID otherwise), of the release's kind
// (amended or withdrawn; INVALID_KNOWN_VALUE otherwise), of a declaration covering the allocation's
// target — its export lot, or a lot of its shipment (REFERENCE_INVALID otherwise); the declaration
// number, original text, receipt instant and evidence are the receipt's. Without it: only while no
// declaration in CNX covers the target (INVALID_STATE: release on its receipt); the human's
// statement stands for what was declared outside CNX.
func declarationResult(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, a *el.Allocation, r *api.ExternalResult,
	in *el.ReleaseInput) error {
	const path = "/args/external_result/receipt_id"
	if r.ReceiptID == "" {
		var covering []*dcl.Case
		if a.Target.Kind == api.TargetExportLot {
			c, err := postgres.DeclarationOfLot(ctx, exec, sc, a.Target.ID, postgres.NoLock)
			if err != nil {
				return err
			}
			if c != nil {
				covering = append(covering, c)
			}
		} else {
			var err error
			if covering, err = postgres.DeclarationsOfShipment(ctx, exec, sc, a.Target.ID); err != nil {
				return err
			}
		}
		if len(covering) > 0 {
			return kernel.Reject(kernel.CodeInvalidState, path, "Declaration "+covering[0].ID+" covers the "+a.Target.Kind+
				": release on its receipt")
		}
		return nil
	}
	rec, err := postgres.LoadReceipt(ctx, exec, sc, r.ReceiptID)
	if err != nil {
		return err
	}
	if rec == nil {
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "CNX recorded no such declaration receipt")
	}
	if rec.Outcome != r.Kind {
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/external_result/kind", "The receipt is "+rec.Outcome)
	}
	c, err := postgres.LoadDeclaration(ctx, exec, sc, rec.DeclarationID, postgres.NoLock)
	if err != nil {
		return err
	}
	if c == nil || (a.Target.Kind == api.TargetExportLot && c.ExportLotID != a.Target.ID) ||
		(a.Target.Kind == api.TargetShipment && c.ShipmentID != a.Target.ID) {
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "The receipt's declaration does not cover the allocation's "+a.Target.Kind)
	}
	in.ReceiptID, in.DeclarationNumber, in.ReceiptText, in.ReceivedAt = rec.ID, rec.DeclarationNumber, rec.OriginalText, rec.ReceivedAt
	in.Evidence = []el.Evidence{}
	for _, e := range rec.Evidence {
		in.Evidence = append(in.Evidence, el.Evidence{SourceID: e.SourceID, ContentSHA256: e.ContentSHA256})
	}
	return nil
}
