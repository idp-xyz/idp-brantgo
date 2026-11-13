// Package stl assembles the STL bounded context (business settlement). S5 unit A6 brings its first
// aggregate, CustomerCredit — credit limits, reservations and decisions — and CreditGuard v1 for
// TRD; settlements, funds, applications and order accounts follow. Code outside STL uses it
// through package api; everything under internal/ is private to STL.
package stl

import (
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/events"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/orders"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/sources"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/application"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
)

// Module is STL wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands   = (*Module)(nil)
	_ api.Queries    = (*Module)(nil)
	_ api.Auditor    = (*Module)(nil)
	_ api.Reconciler = (*Module)(nil)

	_ api.FundCommands      = (*Module)(nil)
	_ api.FundQueries       = (*Module)(nil)
	_ api.FundRecordLookups = (*Module)(nil)

	_ api.ReviewCommands = (*Module)(nil)
	_ api.ReviewQueries  = (*Module)(nil)

	_ api.SettlementCommands = (*Module)(nil)
	_ api.SettlementQueries  = (*Module)(nil)

	_ api.CreditNoteCommands = (*Module)(nil)
	_ api.CreditNoteQueries  = (*Module)(nil)
	_ api.CreditNoteLookups  = (*Module)(nil)

	_ api.ResultCommands = (*Module)(nil)
	_ api.ResultQueries  = (*Module)(nil)
)

// Upstream are the lookups of STL's upstream contexts: TRD's orders through STL's port Orders (R32),
// POL's configurations (R33), PTY's parties (R43) and EVD's sources (R75).
type Upstream = application.Upstream

// The port to TRD (OrderCommercialSnapshot v1) and what passes through it.
type (
	Orders       = application.Orders
	Order        = application.Order
	OrderBinding = application.OrderBinding
	OrderStage   = application.OrderStage
	CurrencyRef  = application.CurrencyRef
	// OrderAnalysis is the order's analysis rate (TRD AnalysisFX) a cross-currency application compares with (C8).
	OrderAnalysis = application.OrderAnalysis
)

// Order states TRD reports.
const (
	OrderDraft  = application.OrderDraft
	OrderActive = application.OrderActive
)

// The order-level halves of STL's ports to FUL and PUR (C9, the order result): FULShipmentLines and
// PURSettlementSources implement them.
type (
	OrderShipments    = application.OrderShipments
	OrderShipmentLine = application.OrderShipmentLine
	OrderPurchases    = application.OrderPurchases
	OrderPurchaseLine = application.OrderPurchaseLine
)

// TRDOrders is STL's port to TRD on TRD's api: OrderSnapshotLookups (OrderCommercialSnapshot v1,
// R32) before STL's transaction.
func TRDOrders(lookups trdapi.OrderSnapshotLookups) Orders { return orders.TRD{Lookups: lookups} }

// The ports to the sources of settlement statements and what passes through them (S5-04b).
type (
	Shipments      = application.Shipments
	Deliveries     = application.Deliveries
	ShipmentSource = application.ShipmentSource
	DeliverySource = application.DeliverySource
	ShipmentLine   = settlement.ShipmentLine
	DeliveryLine   = settlement.DeliveryLine
)

// FULShipmentLines is STL's port to FUL on FUL's api: ShipmentLineLookups (ShipmentLineRef v1, R30)
// before STL's transaction. FUL is composed after STL, so lookups returns FUL's lookups once FUL
// exists (S5 decision D-2; nil: FUL shows no line).
func FULShipmentLines(lookups func() fulapi.ShipmentLineLookups) Shipments {
	return sources.FUL{Lookups: lookups}
}

// PURSettlementSources is STL's port to PUR on PUR's api: DeliveryLookups.ResolveSettlementSources
// (PurchaseSettlementSource v1, R31) before STL's transaction.
func PURSettlementSources(lookups purapi.DeliveryLookups) Deliveries {
	return sources.PUR{Lookups: lookups}
}

// New wires STL to db, a transaction owner that belongs to STL alone, and to its upstreams.
func New(db *bentopg.DB, upstream Upstream) (*Module, error) {
	if !upstream.Complete() {
		return nil, errors.New("stl: module needs TRD's orders, the lookups of POL, PTY and EVD, and the settlement sources of FUL and PUR")
	}
	policy, err := authorization.NewPolicy(application.Rules()...)
	if err != nil {
		return nil, err
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	clock := bento.SystemClock{}
	return &Module{application.Service{Clock: clock, Upstream: upstream, Ledger: &action.Ledger{
		DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
		Outbox: outbox, Clock: clock, ReplayRead: api.CommandGetReservation, Visible: application.Visible,
	}}}, nil
}

// Subscriptions are STL's Inbox consumers on its own transaction owner (repository structure §07):
// stl.order_credit, which has TRD's order events consume and release the order's credit reservations
// (Context Map R32; PL-12, PL-13) and moves them to an amended revision, and stl.order_account, which
// opens an activated order's account and opens the payment plan review of an amended one (D-4, D-18,
// PL-09).
func (m *Module) Subscriptions() []messaging.Subscription {
	return events.Subscriptions(m.Ledger.DB, m.Service)
}
