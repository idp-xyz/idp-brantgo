// Package ful assembles the FUL bounded context (fulfilment and shipment: shipments with their
// packing records and confirmations, the order fulfilment root, ShipmentFacts v1 for TRD; WBS
// S5-03a; milestone plans, export lots, ShipmentLineRef v1 for STL and ExportLotRef v1 for CNX; WBS
// S5-03b; the commercial release of a shipment and its documents; WBS S5-07; bookings and the
// shipment document set; WBS S5-08; the shipment reviews that follow an order change; WBS S5-09b).
// Code outside FUL uses it through package api; everything under internal/ is private to FUL.
package ful

import (
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/events"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/upstream"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/application"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
)

// Module is FUL wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands      = (*Module)(nil)
	_ api.Queries       = (*Module)(nil)
	_ api.Auditor       = (*Module)(nil)
	_ api.Reconciler    = (*Module)(nil)
	_ api.ShipmentFacts = (*Module)(nil)

	// S5-03b: milestone plans, export lots, ShipmentLineRef v1 (R30) and ExportLotRef v1 (R36).
	_ api.MilestoneCommands   = (*Module)(nil)
	_ api.MilestoneQueries    = (*Module)(nil)
	_ api.ExportLotCommands   = (*Module)(nil)
	_ api.ExportLotQueries    = (*Module)(nil)
	_ api.ShipmentLineLookups = (*Module)(nil)
	_ api.ExportLotLookups    = (*Module)(nil)

	// S5-07: the commercial release of a shipment and its documents.
	_ api.ReleaseCommands = (*Module)(nil)
	_ api.ReleaseQueries  = (*Module)(nil)

	// S5-08: bookings and the shipment document set.
	_ api.BookingCommands  = (*Module)(nil)
	_ api.BookingQueries   = (*Module)(nil)
	_ api.DocumentCommands = (*Module)(nil)
	_ api.DocumentQueries  = (*Module)(nil)

	// S5-16: the CIF/CIP insurance policy record.
	_ api.InsuranceCommands = (*Module)(nil)
	_ api.InsuranceQueries  = (*Module)(nil)

	// S5-09b: shipment reviews after an order change.
	_ api.ReviewCommands = (*Module)(nil)
	_ api.ReviewQueries  = (*Module)(nil)
)

// Upstream are the lookups and guards of FUL's upstream contexts: TRD's orders (R27), QLT's quality
// releases (R26), PUR's supplier deliveries through FUL's port Deliveries (R59), PTY's party
// screening (R44), CNX's trade compliance and export licences (R56), STL's payment condition (R45)
// and EVD's sources (R74).
type Upstream = application.Upstream

// FUL's ports and what passes through them.
type (
	Orders       = application.Orders
	Releases     = application.Releases
	Screening    = application.Screening
	Deliveries   = application.Deliveries
	DeliveryLine = application.DeliveryLine
	Compliance   = application.Compliance
	Payments     = application.Payments
	Costs        = application.Costs // CST's cost snapshot by category (R78, S5-16)
)

// TRDOrders is the port to TRD over its ActiveOrderRef v1 lookup and OrderGuard v1.
func TRDOrders(orders trdapi.ActiveOrderLookups) Orders { return upstream.TRD{Orders: orders} }

// QLTReleases is the port to QLT over its QualityReleaseRef v1 lookup and QualityReleaseGuard v1.
func QLTReleases(releases qltapi.ReleaseLookups) Releases { return upstream.QLT{Releases: releases} }

// PTYScreening is the port to PTY's PartyScreeningGuard v1.
func PTYScreening() Screening { return upstream.PTY{} }

// PURDeliveries is the port to PUR over its supplier delivery lookup (ResolveDeliveryLines) and
// SupplierDeliveryGuard v1 (R59).
func PURDeliveries(deliveries purapi.DeliveryLookups) Deliveries {
	return upstream.PUR{Deliveries: deliveries}
}

// CNXCompliance is the port to CNX's TradeComplianceGuard v1 and ExportLicenseGuard v1 (R56).
func CNXCompliance() Compliance { return upstream.CNX{} }

// STLPayments is the port to STL's PaymentConditionGuard v1 (R45).
func STLPayments() Payments { return upstream.STL{} }

// DocumentUpstream are the upstreams of bookings and shipment documents (S5-08).
type DocumentUpstream = application.DocumentUpstream

// DocumentPorts are the ports of bookings and shipment documents: POL's ConfigRef v1, ConfigGuard v1
// and NumberIssue v1 (R49), PTY's parties and ServiceRoleRef v1 (R44), TRD's
// OrderCommercialSnapshot v1 (R27).
func DocumentPorts(pol interface {
	polapi.Queries
	polapi.Commands
}, pty interface {
	ptyapi.Queries
	ptyapi.ProfileQueries
}, trd trdapi.OrderSnapshotLookups) DocumentUpstream {
	cfg := upstream.POLConfig{Queries: pol, Commands: pol}
	return DocumentUpstream{Configs: cfg, Numbers: cfg, Parties: upstream.PTYParties{Parties: pty, Profiles: pty}, Orders: trd}
}

// New wires FUL to db, a transaction owner that belongs to FUL alone, and to its upstreams.
func New(db *bentopg.DB, up Upstream) (*Module, error) {
	if !up.Complete() {
		return nil, errors.New("ful: module needs TRD's orders, QLT's releases, PUR's deliveries, PTY's screening, CNX's compliance, STL's payments and EVD's sources")
	}
	if !up.Documents.Complete() {
		return nil, errors.New("ful: module needs POL's configuration and numbers, PTY's parties and TRD's order snapshots for documents")
	}
	if up.Costs == nil {
		return nil, errors.New("ful: module needs CST's cost snapshots by category for insurance (R78)")
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
	return &Module{application.Service{Clock: clock, Upstream: up, Ledger: &action.Ledger{
		DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
		Outbox: outbox, Clock: clock, ReplayRead: api.CommandGetShipment, Visible: application.Visible,
	}}}, nil
}

// Subscriptions are FUL's Inbox consumers on its own transaction owner (repository structure §07):
// ful.shipment_review, which has TRD's OrderAmended, OrderCancelled and OrderRemainderClosed open a
// shipment review of what the change leaves stale (Context Map R27; PL-09; WF-06 step 5).
func (m *Module) Subscriptions() []messaging.Subscription {
	return events.Subscriptions(m.Ledger.DB, m.Service)
}
