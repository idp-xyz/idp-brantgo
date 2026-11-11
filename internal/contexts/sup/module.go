// Package sup assembles the SUP bounded context (supply sourcing and supply conditions: supplier
// RFQs, supplier offers, purchase terms and logistics offers). Code outside SUP uses it through
// package api; everything under internal/ is private to SUP.
package sup

import (
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/application"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is SUP wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands = (*Module)(nil)
	_ api.Queries  = (*Module)(nil)
	_ api.Auditor  = (*Module)(nil)
)

// Upstream are the queries of SUP's upstream contexts: EVD's sources (R53), PTY's parties and
// service roles (R05), CAT's SKU specifications and item code mappings (R06), INQ's RFQs (R04) and
// POL's configurations (R07).
type Upstream = application.Upstream

// New wires SUP to db, a transaction owner that belongs to SUP alone, and to the queries of its
// upstream contexts.
func New(db *bentopg.DB, upstream Upstream) (*Module, error) {
	u := upstream
	if u.Sources == nil || u.Parties == nil || u.Skus == nil || u.Rfqs == nil || u.Configs == nil {
		return nil, errors.New("sup: module needs the queries of EVD, PTY, CAT, INQ and POL")
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
	return &Module{application.Service{Clock: clock, Upstream: u, Ledger: &action.Ledger{
		DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
		Outbox: outbox, Clock: clock, ReplayRead: application.CommandGet(api.KindSupplierOffer), Visible: application.Visible,
	}}}, nil
}
