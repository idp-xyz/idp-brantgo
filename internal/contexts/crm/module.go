// Package crm assembles the CRM bounded context. Code outside CRM uses it through package api;
// everything under internal/ is private to CRM. The context is optional (S6-D04): bootstrap leaves
// it unconstructed when CRM is disabled, and INQ then holds no opportunity lookup.
package crm

import (
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/crm/api"
	"go.idp.xyz/idp-brant/internal/contexts/crm/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/crm/internal/application"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is CRM wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands           = (*Module)(nil)
	_ api.Queries            = (*Module)(nil)
	_ api.Auditor            = (*Module)(nil)
	_ api.Reconciler         = (*Module)(nil)
	_ api.OpportunityLookups = (*Module)(nil)
)

// New wires CRM to db, a transaction owner that belongs to CRM alone.
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
		Outbox: outbox, Clock: clock, ReplayRead: application.CommandGetLead, Visible: application.Visible,
	}}}, nil
}
