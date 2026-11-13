package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The reservations of one order, as the order event consumer reads them (unit B6, PL-12, PL-13).

func collectReservations(rows pgx.Rows, err error) ([]cc.Reservation, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (cc.Reservation, error) {
		r, err := scanReservation(row)
		if err != nil {
			return cc.Reservation{}, err
		}
		return *r, nil
	})
}

// LoadHeldReservations returns the reservations of order orderID that are reserved or consumed, of
// every order revision, in the order of their IDs (index credit_reservation_live). A reserved one
// may have expired: expiry is derived, the row stays reserved until a command releases it.
func LoadHeldReservations(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string) ([]cc.Reservation, error) {
	t, c := ts(sc)
	return collectReservations(exec.Query(ctx, `SELECT `+reservationColumns+` FROM brant_stl.credit_reservation WHERE tenant_id = $1
		AND company_id = $2 AND order_id = $3 AND status IN ('reserved', 'consumed') ORDER BY reservation_id`, t, c, orderID))
}

// LoadReleasedReservations returns the released reservations of one order revision, the latest
// release first. No index serves it (credit_reservation_live holds reserved and consumed rows
// only): the consumer asks only when the revision has no held reservation.
func LoadReleasedReservations(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, revision int64) ([]cc.Reservation, error) {
	t, c := ts(sc)
	return collectReservations(exec.Query(ctx, `SELECT `+reservationColumns+` FROM brant_stl.credit_reservation WHERE tenant_id = $1
		AND company_id = $2 AND order_id = $3 AND order_revision = $4 AND status = 'released' ORDER BY released_at DESC, reservation_id`,
		t, c, orderID, revision))
}
