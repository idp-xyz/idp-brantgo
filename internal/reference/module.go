// Package reference assembles Brant's reference data (screening lists, S1-11). It is not a bounded
// context: contexts use it through package api, and it depends on the kernel and the foundation
// only. It has a transaction owner of its own and writes only schema brant_ref.
package reference

import (
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/reference/api"
	"go.idp.xyz/idp-brant/internal/reference/internal/application"
	"go.idp.xyz/idp-brant/internal/reference/internal/postgres"
)

// Module is reference data wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands = (*Module)(nil)
	_ api.Queries  = (*Module)(nil)
	_ api.Auditor  = (*Module)(nil)
)

// New wires reference data to db, a transaction owner that belongs to it alone.
func New(db *bentopg.DB) (*Module, error) {
	policy, err := authorization.NewPolicy(application.Rules()...)
	if err != nil {
		return nil, err
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	clock := bento.SystemClock{}
	return &Module{application.Service{Clock: clock, Ledger: &action.Ledger{
		DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
		Outbox: outbox, Clock: clock, ReplayRead: application.CommandGetList, Visible: application.Visible,
	}}}, nil
}
