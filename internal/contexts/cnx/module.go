// Package cnx assembles the CNX bounded context (China export professional collaboration, CN-M01).
// S5 A7 delivers its first aggregate, the trade compliance check of a sales order revision, and
// TradeComplianceGuard v1; S5 B5 the export licence with its usage ledger and ExportLicenseGuard
// v1; S5 C3b purchase invoices and tax refund cases; declarations, document bundles and
// certificates follow in later S5 units. Code outside CNX uses it through
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
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
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
	_ api.Commands        = (*Module)(nil)
	_ api.Queries         = (*Module)(nil)
	_ api.LicenseCommands = (*Module)(nil)
	_ api.LicenseQueries  = (*Module)(nil)
	_ api.InvoiceCommands = (*Module)(nil)
	_ api.InvoiceQueries  = (*Module)(nil)
	_ api.TaxCaseCommands = (*Module)(nil)
	_ api.TaxCaseQueries  = (*Module)(nil)
	_ api.Auditor         = (*Module)(nil)
	_ api.Reconciler      = (*Module)(nil)
)

// Upstream are the contracts of CNX's upstreams: TRD's orders (R46), PTY's address countries
// (R77), CAT's declaration profiles (R39), FUL's shipments and export lots (R36) and reference
// data's country programmes through CNX's ports, POL's configurations (R50) and EVD's sources and
// files (R54).
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
	Exports         = application.Exports
	ExportRef       = application.ExportRef
	Export          = application.Export
	ExportLine      = application.ExportLine
	PurchaseLines   = application.PurchaseLines
	PurchaseLineRef = application.PurchaseLineRef
	PurchaseLine    = application.PurchaseLine
	Funds           = application.Funds
	FundRecord      = application.FundRecord
)

// FUL's states an export licence allocation needs, and the one a tax refund allocation refuses.
const (
	ShipmentPrepared    = application.ShipmentPrepared
	ExportLotRegistered = application.ExportLotRegistered
	ExportLotCancelled  = application.ExportLotCancelled
)

// PURLines is CNX's port to PUR on PUR's DeliveryLookups (PurchaseLineRef v1, R37): the commitment
// lines purchase invoices are matched to.
func PURLines(lines purapi.DeliveryLookups) PurchaseLines { return upstream.PUR{Lines: lines} }

// STLFunds is CNX's port to STL on STL's FundRecordLookups (FundRecordRef v1, R38): the fund
// records a received tax refund rests on.
func STLFunds(funds stlapi.FundRecordLookups) Funds { return upstream.STL{Funds: funds} }

// ExportsNotWired is CNX's port to FUL's shipments and export lots (R36) until FUL is merged:
// export_license.allocate answers DEPENDENCY_NOT_SUPPORTED.
func ExportsNotWired() Exports { return upstream.FULNotWired{} }

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
		return nil, errors.New("cnx: module needs the ports to TRD, PTY, CAT, FUL, PUR, STL and reference data and the lookups of POL and EVD")
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
