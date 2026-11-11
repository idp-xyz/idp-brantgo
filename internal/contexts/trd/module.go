// Package trd assembles the TRD bounded context (quotes, customer acceptance and sales orders). S3
// starts with QuoteRoot: working drafts, the freeze with its server calculation, commercial document
// and business hash, internal metadata, the root's object control, the review of a frozen revision
// with its approval and margin exception, and the formalization that makes an approved revision the
// root's one active revision with its formal document, number and commit evidence (activation,
// replacement), its withdrawal and cancellation, risk holds and the conversion slot; then the
// customer's responses to an active formal revision, their verification into acceptances that form
// the contract when they arrived in time, and the acceptances' effectivity ledger; then the sales
// order an accepted revision converts into, its review and activation (ActiveOrderRef v1 for PUR).
// Code outside TRD uses it through package api; everything under internal/ is private to TRD.
package trd

import (
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/application"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is TRD wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands            = (*Module)(nil)
	_ api.Queries             = (*Module)(nil)
	_ api.Lookups             = (*Module)(nil)
	_ api.AcceptanceLookups   = (*Module)(nil)
	_ api.ActiveOrderLookups  = (*Module)(nil)
	_ api.ActiveOrderCommands = (*Module)(nil)
	_ api.Auditor             = (*Module)(nil)
	_ api.Reconciler          = (*Module)(nil)
)

// Upstream are the lookups of TRD's upstream contexts: EVD (SourceRef v1, R16), PTY (PartyRef,
// TradingProfileRef and AddressRoleRef v1, R13), CAT (SkuRef v1, R14), POL (ConfigRef v1 and
// NumberIssue v1, R15), INQ (RfqRef v1, R12) and CST (CostSnapshotRef v1 and the costing context it
// froze, R11).
type Upstream struct {
	Sources   evdapi.Queries
	Parties   ptyapi.Queries
	Profiles  ptyapi.ProfileQueries
	Skus      catapi.Queries
	Configs   polapi.Queries
	Numbers   polapi.Commands // number.issue for the formal documents, as the acting human
	Rfqs      inqapi.RfqLookups
	Snapshots cstapi.SnapshotLookups
	Contexts  cstapi.Lookups
}

// New wires TRD to db, a transaction owner that belongs to TRD alone, and to the lookups of its
// upstream contexts. The freeze calls the guard functions of CAT, CST, INQ, POL and PTY itself and
// computes the quote on the server (application.ServerCalculator, WBS S3-02).
func New(db *bentopg.DB, up Upstream) (*Module, error) {
	if up.Sources == nil || up.Parties == nil || up.Profiles == nil || up.Skus == nil || up.Configs == nil || up.Numbers == nil ||
		up.Rfqs == nil || up.Snapshots == nil || up.Contexts == nil {
		return nil, errors.New("trd: module needs the lookups of EVD, PTY, CAT, POL, INQ and CST and POL's number.issue")
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
	return &Module{application.Service{Clock: clock, Calculator: application.ServerCalculator{},
		Upstream: application.Upstream{Sources: up.Sources, Parties: up.Parties, Profiles: up.Profiles, Skus: up.Skus, Configs: up.Configs,
			Numbers: up.Numbers, Rfqs: up.Rfqs, Snapshots: up.Snapshots, Contexts: up.Contexts},
		Ledger: &action.Ledger{
			DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
			Outbox: outbox, Clock: clock, ReplayRead: application.CommandGetQuote, Visible: application.Visible,
		}}}, nil
}
