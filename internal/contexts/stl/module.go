// Package stl assembles the STL bounded context (business settlement). S5 unit A6 brings its first
// aggregate, CustomerCredit — credit limits, reservations and decisions — and CreditGuard v1 for
// TRD; settlements, funds, applications and order accounts follow. Code outside STL uses it
// through package api; everything under internal/ is private to STL.
package stl

import (
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/orders"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/application"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
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
)

// Order states TRD reports.
const (
	OrderDraft  = application.OrderDraft
	OrderActive = application.OrderActive
)

// TRDOrders is STL's port to TRD on TRD's api: OrderSnapshotLookups (OrderCommercialSnapshot v1,
// R32) before STL's transaction.
func TRDOrders(lookups trdapi.OrderSnapshotLookups) Orders { return orders.TRD{Lookups: lookups} }

// New wires STL to db, a transaction owner that belongs to STL alone, and to its upstreams.
func New(db *bentopg.DB, upstream Upstream) (*Module, error) {
	if !upstream.Complete() {
		return nil, errors.New("stl: module needs TRD's orders and the lookups of POL, PTY and EVD")
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
