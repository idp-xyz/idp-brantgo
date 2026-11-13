// Package inq assembles the INQ bounded context (customer RFQs). Code outside INQ uses it through
// package api; everything under internal/ is private to INQ.
package inq

import (
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/contexts/inq/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/inq/internal/application"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is INQ wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands         = (*Module)(nil)
	_ api.Queries          = (*Module)(nil)
	_ api.Auditor          = (*Module)(nil)
	_ api.Reconciler       = (*Module)(nil)
	_ api.ActionReconciler = (*Module)(nil)
	_ api.RfqLookups       = (*Module)(nil)
)

// New wires INQ to db, a transaction owner that belongs to INQ alone, and to the queries of its
// upstream contexts EVD, PTY and CAT.
func New(db *bentopg.DB, sources evdapi.Queries, parties ptyapi.Queries, skus catapi.Queries) (*Module, error) {
	if sources == nil || parties == nil || skus == nil {
		return nil, errors.New("inq: module needs the queries of EVD, PTY and CAT")
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
	return &Module{application.Service{Clock: clock,
		Upstream: application.Upstream{Sources: sources, Parties: parties, Skus: skus},
		Ledger: &action.Ledger{
			DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
			Outbox: outbox, Clock: clock, ReplayRead: application.CommandGetRfq, Visible: application.Visible,
		}}}, nil
}
