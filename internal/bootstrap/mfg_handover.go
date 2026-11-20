package bootstrap

import (
	"context"

	mfgapi "go.idp.xyz/idp-brant/internal/contexts/mfg/api"
	"go.idp.xyz/idp-brant/internal/contexts/stk"
	stkapi "go.idp.xyz/idp-brant/internal/contexts/stk/api"
)

// stkHandover is MFG's port to STK's MaterialHandover v1 (R42). The call is STK's own transaction,
// made before MFG records the association. MFG does not import STK: the Context Map edge lets STK
// import MFG, not the reverse.
type stkHandover struct{ stock *stk.Module }

func (h stkHandover) Enabled(ctx context.Context) (bool, error) {
	if h.stock == nil {
		return false, nil
	}
	st, err := h.stock.GetSetting(ctx)
	if err != nil {
		return false, err
	}
	return st.Enabled, nil
}

func (h stkHandover) Record(ctx context.Context, req mfgapi.HandoverRequest) (mfgapi.HandoverResult, error) {
	rec, err := h.stock.RecordHandover(ctx, stkapi.RecordHandover{IdempotencyKey: req.IdempotencyKey, MovementID: req.MovementID,
		CounterMovementID: req.CounterMovementID, Kind: req.Kind, SkuID: req.SkuID, LocationID: req.LocationID, BatchID: req.BatchID,
		CustodyKind: req.CustodyKind, ToLocationID: req.ToLocationID, ToBatchID: req.ToBatchID, ToCustodyKind: req.ToCustodyKind,
		Quantity: req.Quantity, UnitID: req.UnitID, UnitRevision: req.UnitRevision, Reason: req.Reason})
	if err != nil {
		return mfgapi.HandoverResult{}, err
	}
	return mfgapi.HandoverResult{MovementID: rec.ObjectID, Replayed: rec.Replayed}, nil
}
