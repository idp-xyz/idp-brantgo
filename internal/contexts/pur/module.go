// Package pur assembles the PUR bounded context (procurement and supply allocation: purchase
// drafts and the demand allocation of active order lines; commitments and supplier deliveries are
// S5). Code outside PUR uses it through package api; everything under internal/ is private to PUR.
package pur

import (
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/application"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is PUR wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands   = (*Module)(nil)
	_ api.Queries    = (*Module)(nil)
	_ api.Auditor    = (*Module)(nil)
	_ api.Reconciler = (*Module)(nil)
)

// Upstream are the lookups of PUR's upstream contexts: TRD's orders through PUR's port Orders (R19),
// SUP's supply conditions (R20), CST's cost snapshots (R21), PTY's parties (R22), POL's
// configurations (R48) and CAT's specifications (R52).
type Upstream = application.Upstream

// The port to TRD (ActiveOrderRef v1 and OrderGuard v1) and what passes through it.
type (
	Orders        = application.Orders
	Order         = application.Order
	OrderBinding  = application.OrderBinding
	OrderLine     = application.OrderLine
	OrderUnit     = application.OrderUnit
	OrderRfq      = application.OrderRfq
	OrderSnapshot = application.OrderSnapshot
	OrderDelivery = application.OrderDelivery
	OrderVerdict  = application.OrderVerdict
)

// Order codes TRD answers, which PUR passes on.
const (
	OrderNotActive        = application.OrderNotActive
	OrderApprovalRequired = application.OrderApprovalRequired
	OrderApprovalRevoked  = application.OrderApprovalRevoked
	OrderStale            = application.OrderStale
)

// New wires PUR to db, a transaction owner that belongs to PUR alone, and to its upstreams.
func New(db *bentopg.DB, upstream Upstream) (*Module, error) {
	if !upstream.Complete() {
		return nil, errors.New("pur: module needs TRD's orders and the lookups of SUP, CST, PTY, POL and CAT")
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
		Outbox: outbox, Clock: clock, ReplayRead: api.CommandGetDraft, Visible: application.Visible,
	}}}, nil
}
