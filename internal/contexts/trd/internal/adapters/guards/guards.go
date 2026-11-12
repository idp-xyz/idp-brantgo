// Package guards implements the two upstream guards TRD's order.activate and order.amendment.apply
// call beside those of its quote upstreams (S5 unit B4; application.OrderGuards), on the contracts
// their owners publish:
//
//   - STL's CreditGuard v1 (Context Map R57 STL→TRD, guardLock; TRD-INV-022):
//     brant_stl.guard_credit_reservation_v1, which locks the order revision's live reservation row
//     FOR SHARE in TRD's transaction;
//   - CNX's TradeComplianceGuard v1 (R58 CNX→TRD, guardLock; TRD-INV-023):
//     brant_cnx.guard_trade_compliance_v1, which locks the order revision's check row (and the
//     programme lists a clear conclusion used) FOR SHARE in TRD's transaction.
//
// Both are SQL functions run with TRD's own executor (ADR-BR-004 option B): TRD never calls STL's or
// CNX's transaction owner, and needs neither module to be constructed (S5 decision D-2 — STL and
// CNX are composed after TRD, whose order snapshots they read). The verdicts and the refusals are
// the owners' (stlapi.CreditVerdict.Check, cnxapi.TradeComplianceVerdict.Check); nothing is judged
// here.
package guards

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/application"
)

var (
	_ application.CreditGuard          = STLCredit
	_ application.TradeComplianceGuard = CNXTradeCompliance
)

// STLCredit asks STL's CreditGuard v1 whether a live reservation of the order revision covers q's
// portion in q's currency at q's instant, for the order hash q names. A refusal is STL's
// CREDIT_RESERVATION_REQUIRED at q.Path.
func STLCredit(ctx context.Context, exec bentopg.Executor, q application.CreditQuery) (application.CreditAnswer, error) {
	sq := stlapi.CreditQuery{OrderID: q.Order.OrderID, OrderRevision: q.Order.OrderRevision, OrderHash: q.Order.OrderHash,
		PortionMinor: q.PortionMinor, Currency: q.Currency, At: q.At}
	v, err := stlapi.ScanCreditGuard(sq, exec.QueryRow(ctx, stlapi.CreditGuardSQL, stlapi.CreditGuardArgs(sq)...).Scan)
	if err != nil {
		return application.CreditAnswer{}, err
	}
	return application.CreditAnswer{Status: v.Status, ReservationID: v.ReservationID, ReservedMinor: v.ReservedMinor,
		SecuredMinor: v.SecuredMinor, Basis: v.Basis, DecisionID: v.DecisionID, Refusal: v.Check(q.Path)}, nil
}

// CNXTradeCompliance asks CNX's TradeComplianceGuard v1 whether a clear, valid trade compliance
// conclusion covers order revision o at at. A refusal is CNX's TRADE_COMPLIANCE_NOT_CLEAR at path.
func CNXTradeCompliance(ctx context.Context, exec bentopg.Executor, o application.OrderRevisionRef, at time.Time,
	path string) (application.ComplianceAnswer, error) {
	b := cnxapi.OrderBinding{OrderID: o.OrderID, OrderRevision: o.OrderRevision, OrderHash: o.OrderHash}
	v, err := cnxapi.ScanTradeComplianceGuard(b, exec.QueryRow(ctx, cnxapi.TradeComplianceGuardSQL, cnxapi.TradeComplianceGuardArgs(b, at)...).Scan)
	if err != nil {
		return application.ComplianceAnswer{}, err
	}
	return application.ComplianceAnswer{Evidence: v.Evidence(), RegulationID: v.RegulationID, RegulationRevision: v.RegulationRevision,
		ValidUntil: v.ValidUntil, Refusal: v.Check(path)}, nil
}
