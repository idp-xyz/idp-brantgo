// Package pol assembles the POL bounded context (policy and configuration: independently published
// configuration revisions, foundation snapshots and document numbering). Code outside POL uses it
// through package api; everything under internal/ is private to POL.
package pol

import (
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/application"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is POL wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands = (*Module)(nil)
	_ api.Queries  = (*Module)(nil)
	_ api.Auditor  = (*Module)(nil)
)

// New wires POL to db, a transaction owner that belongs to POL alone. POL has no upstream context.
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
		Outbox: outbox, Clock: clock, ReplayRead: application.CommandGetConfig, Visible: application.Visible,
	}}}, nil
}
