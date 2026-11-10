// Package cst assembles the CST bounded context (costing: costing contexts, measurement snapshots
// and the calculation rules; cost scenarios and snapshots follow). Code outside CST uses it through
// package api; everything under internal/ is private to CST.
package cst

import (
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/application"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is CST wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands   = (*Module)(nil)
	_ api.Queries    = (*Module)(nil)
	_ api.Lookups    = (*Module)(nil)
	_ api.Auditor    = (*Module)(nil)
	_ api.Reconciler = (*Module)(nil)
)

// New wires CST to db, a transaction owner that belongs to CST alone, and to the lookups of its
// upstream contexts INQ (RfqRef v1, R08), CAT (SkuRef v1, R51) and POL (ConfigRef v1, R10).
func New(db *bentopg.DB, rfqs inqapi.RfqLookups, skus catapi.Queries, configs polapi.Queries) (*Module, error) {
	if rfqs == nil || skus == nil || configs == nil {
		return nil, errors.New("cst: module needs the lookups of INQ, CAT and POL")
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
		Upstream: application.Upstream{Rfqs: rfqs, Skus: skus, Configs: configs},
		Ledger: &action.Ledger{
			DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
			Outbox: outbox, Clock: clock, ReplayRead: application.CommandGetContext, Visible: application.Visible,
		}}}, nil
}
