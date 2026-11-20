package postgres

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
)

// CommitmentShipped reads FUL's CommitmentShipped v1 (Context Map R82, PUR-INV-013) of commitment
// commitmentID with exec, which is PUR's own executor inside PUR's transaction. Call it after the
// commitment root is locked FOR UPDATE. The function takes no lock on a FUL row, and this read does
// not call FUL's transaction owner. The quantities are parsed; what they mean against a disposition
// is the domain's (commitment.ShippedOf).
func CommitmentShipped(ctx context.Context, exec bentopg.Executor, commitmentID string) ([]commitment.LineShipped, error) {
	rows, err := exec.Query(ctx, fulapi.CommitmentShippedSQL, fulapi.CommitmentShippedArgs(commitmentID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []commitment.LineShipped
	for rows.Next() {
		line, err := fulapi.ScanCommitmentShipped(rows.Scan)
		if err != nil {
			return nil, err
		}
		q, err := quantity(line.Quantity)
		if err != nil {
			return nil, err
		}
		out = append(out, commitment.LineShipped{OrderLineID: line.OrderLineID, UnitID: line.UnitID, UnitRevision: line.UnitRevision,
			Quantity: q, Shipments: line.Shipments})
	}
	return out, rows.Err()
}
