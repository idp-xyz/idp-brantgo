// Package clm assembles the CLM bounded context (after-sales claims: ClaimCase with its returns
// and remedy, CreditRequest v1 for STL; WBS S6-03). Code outside CLM uses it through package api;
// everything under internal/ is private to CLM.
package clm

import (
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/clm/api"
	"go.idp.xyz/idp-brant/internal/contexts/clm/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/clm/internal/adapters/shipments"
	"go.idp.xyz/idp-brant/internal/contexts/clm/internal/application"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is CLM wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands      = (*Module)(nil)
	_ api.Queries       = (*Module)(nil)
	_ api.Auditor       = (*Module)(nil)
	_ api.Reconciler    = (*Module)(nil)
	_ api.CreditLookups = (*Module)(nil)
)

// Upstream are the lookups CLM reads before its transaction: FUL's shipment lines (R34) and the
// ports to EVD sources and POL currency minor units, which the composition root implements.
type Upstream = application.Upstream

// Evidence and Currencies are those ports. CLM does not import EVD or POL: the Context Map gives
// CLM no edge to them.
type (
	Evidence   = application.Evidence
	Currencies = application.Currencies
)

// FULLines is the port to FUL over ShipmentLineRef v1 (R34).
func FULLines(lines fulapi.ShipmentLineLookups) application.ShipmentLines {
	return shipments.FUL{Lookups: lines}
}

// New wires CLM to db, a transaction owner that belongs to CLM alone, and to its upstreams.
func New(db *bentopg.DB, upstream Upstream) (*Module, error) {
	if !upstream.Complete() {
		return nil, errors.New("clm: module needs FUL's shipment lines, evidence and currency minor units")
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
	return &Module{application.Service{Clock: clock, Upstream: upstream, Ledger: &action.Ledger{
		DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
		Outbox: outbox, Clock: clock, ReplayRead: api.CommandGetClaim, Visible: application.Visible,
	}}}, nil
}
