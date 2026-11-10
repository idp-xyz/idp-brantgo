// Package cat assembles the CAT bounded context (catalog: SKUs and specification revisions). Code
// outside CAT uses it through package api; everything under internal/ is private to CAT.
package cat

import (
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/application"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is CAT wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands = (*Module)(nil)
	_ api.Queries  = (*Module)(nil)
	_ api.Auditor  = (*Module)(nil)
)

// New wires CAT to db, a transaction owner that belongs to CAT alone, and to EVD's queries for the
// evidence of confirmations.
func New(db *bentopg.DB, sources evdapi.Queries) (*Module, error) {
	if sources == nil {
		return nil, errors.New("cat: module needs EVD's queries")
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
	return &Module{application.Service{Clock: clock, Sources: sources, Ledger: &action.Ledger{
		DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
		Outbox: outbox, Clock: clock, ReplayRead: application.CommandGetSku, Visible: application.Visible,
	}}}, nil
}
