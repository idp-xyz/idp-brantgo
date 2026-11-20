// Package mfg assembles the MFG bounded context (collaboration plans, subcontract orders and
// material handover associations; WBS S6-02). Code outside MFG uses it through package api.
package mfg

import (
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/api"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/internal/application"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is MFG wired to PostgreSQL through its own transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands   = (*Module)(nil)
	_ api.Queries    = (*Module)(nil)
	_ api.Auditor    = (*Module)(nil)
	_ api.Reconciler = (*Module)(nil)
)

// New wires MFG to db, a transaction owner that belongs to MFG alone. demands is PUR's
// SubcontractDemand v1 (R23); nil refuses an issue that cites a commitment. handovers is the
// composition root's port to STK (R42); nil refuses a material handover. MFG does not import STK.
func New(db *bentopg.DB, demands purapi.SubcontractDemands, handovers api.Handovers) (*Module, error) {
	policy, err := authorization.NewPolicy(application.Rules()...)
	if err != nil {
		return nil, err
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	clock := bento.SystemClock{}
	return &Module{application.Service{Clock: clock, Demands: demands, Handovers: handovers, Ledger: &action.Ledger{
		DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
		Outbox: outbox, Clock: clock, ReplayRead: api.CommandGetPlan, Visible: application.Visible,
	}}}, nil
}
