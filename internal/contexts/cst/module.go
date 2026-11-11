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
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
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

	_ api.ScenarioCommands  = (*Module)(nil)
	_ api.ScenarioQueries   = (*Module)(nil)
	_ api.EvaluationLookups = (*Module)(nil)
)

// New wires CST to db, a transaction owner that belongs to CST alone, and to the lookups of its
// upstream contexts EVD (SourceRef v1, R64: the sources cited evidence names), INQ (RfqRef v1,
// R08), CAT (SkuRef v1, R51), POL (ConfigRef v1, R10) and SUP (SupplyConditionRef v1, R09).
func New(db *bentopg.DB, sources evdapi.Queries, rfqs inqapi.RfqLookups, skus catapi.Queries, configs polapi.Queries, supply supapi.Queries) (*Module, error) {
	if sources == nil || rfqs == nil || skus == nil || configs == nil || supply == nil {
		return nil, errors.New("cst: module needs the lookups of EVD, INQ, CAT, POL and SUP")
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
		Upstream: application.Upstream{Sources: sources, Rfqs: rfqs, Skus: skus, Configs: configs, Supply: supply},
		Ledger: &action.Ledger{
			DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
			Outbox: outbox, Clock: clock, ReplayRead: application.CommandGetContext, Visible: application.Visible,
		}}}, nil
}
