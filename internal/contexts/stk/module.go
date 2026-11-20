// Package stk assembles the STK bounded context (inventory, FIFO cost layers, reservations and
// StockAvailability v1; WBS S6-01). Code outside STK uses it through package api.
package stk

import (
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/stk/api"
	"go.idp.xyz/idp-brant/internal/contexts/stk/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stk/internal/application"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is STK wired to PostgreSQL through its own transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands   = (*Module)(nil)
	_ api.Queries    = (*Module)(nil)
	_ api.Auditor    = (*Module)(nil)
	_ api.Reconciler = (*Module)(nil)
)

// New wires STK to db, a transaction owner that belongs to STK alone. receipts is PUR's
// ReceiptRequest v1 (R28); nil refuses a receipt that cites a commitment or a hold.
func New(db *bentopg.DB, receipts purapi.ReceiptRequests) (*Module, error) {
	policy, err := authorization.NewPolicy(application.Rules()...)
	if err != nil {
		return nil, err
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	clock := bento.SystemClock{}
	return &Module{application.Service{Clock: clock, Receipts: receipts, Ledger: &action.Ledger{
		DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
		Outbox: outbox, Clock: clock, ReplayRead: api.CommandGetBalance, Visible: application.Visible,
	}}}, nil
}
