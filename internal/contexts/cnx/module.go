// Package cnx assembles the CNX bounded context (China export professional collaboration, CN-M01).
// S5 A7 delivers its first aggregate, the trade compliance check of a sales order revision, and
// TradeComplianceGuard v1; declarations, purchase invoices, tax refund cases, document bundles,
// export licences and certificates follow in later S5 units. Code outside CNX uses it through
// package api; everything under internal/ is private to CNX.
package cnx

import (
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/upstream"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/application"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is CNX wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands   = (*Module)(nil)
	_ api.Queries    = (*Module)(nil)
	_ api.Auditor    = (*Module)(nil)
	_ api.Reconciler = (*Module)(nil)
)

// Upstream are the contracts of CNX's upstreams: TRD's orders (R46), PTY's address countries
// (R77), CAT's declaration profiles (R39) and reference data's country programmes through CNX's
// ports, POL's configurations (R50) and EVD's sources (R54).
type Upstream = application.Upstream

// CNX's ports and what passes through them.
type (
	Orders          = application.Orders
	Order           = application.Order
	OrderAddress    = application.OrderAddress
	OrderLine       = application.OrderLine
	OrderBinding    = tcc.Binding
	Countries       = application.Countries
	AddressRef      = application.AddressRef
	AddressCountry  = application.AddressCountry
	Profiles        = application.Profiles
	ProfileRef      = application.ProfileRef
	Profile         = tcc.Profile
	Programmes      = application.Programmes
	ProgrammeAnswer = application.ProgrammeAnswer
	ListVersion     = tcc.ListVersion
	ProgrammeHit    = tcc.ProgrammeHit
)

// TRD's order states CNX evaluates, and the classifications of the profiles CAT answers.
const (
	OrderDraft    = application.OrderDraft
	OrderActive   = application.OrderActive
	NotControlled = tcc.NotControlled
	Controlled    = tcc.Controlled
)

// TRDOrders is CNX's port to TRD on TRD's OrderSnapshotLookups (SalesOrderRef v1, R46).
func TRDOrders(snapshots trdapi.OrderSnapshotLookups) Orders {
	return upstream.TRD{Snapshots: snapshots}
}

// PTYCountries is CNX's port to PTY on PTY's AddressRoleRef v1 lookups with the country (R77).
func PTYCountries(roles upstream.AddressRoles) Countries { return upstream.PTY{Roles: roles} }

// REFProgrammes is CNX's port to reference data's country programme lists.
func REFProgrammes(programmes upstream.Programmes) Programmes {
	return upstream.REF{Programmes: programmes}
}

// CATProfiles is CNX's port to CAT on CAT's DeclarationProfileLookups (DeclarationProfileRef v1,
// R39): the revision verified now of each line's profile under the evaluation's code table version.
func CATProfiles(profiles catapi.DeclarationProfileLookups) Profiles {
	return upstream.CAT{Profiles: profiles}
}

// New wires CNX to db, a transaction owner that belongs to CNX alone, and to its upstreams.
func New(db *bentopg.DB, u Upstream) (*Module, error) {
	if !u.Complete() {
		return nil, errors.New("cnx: module needs the ports to TRD, PTY, CAT and reference data and the lookups of POL and EVD")
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
		Outbox: outbox, Clock: clock, ReplayRead: api.CommandGetCheck, Visible: application.Visible,
	}}}, nil
}
