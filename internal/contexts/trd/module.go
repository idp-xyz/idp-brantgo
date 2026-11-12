// Package trd assembles the TRD bounded context (quotes, customer acceptance and sales orders). S3
// starts with QuoteRoot: working drafts, the freeze with its server calculation, commercial document
// and business hash, internal metadata, the root's object control, the review of a frozen revision
// with its approval and margin exception, and the formalization that makes an approved revision the
// root's one active revision with its formal document, number and commit evidence (activation,
// replacement), its withdrawal and cancellation, risk holds and the conversion slot; then the
// customer's responses to an active formal revision, their verification into acceptances that form
// the contract when they arrived in time, and the acceptances' effectivity ledger; then the sales
// order an accepted revision converts into, its review and activation (ActiveOrderRef v1 for PUR),
// its amendments and end, and the S/C and PI of its revisions with their signed copies; and, for
// STL, CNX and FUL, OrderCommercialSnapshot v1 of drafts and active orders (OrderSnapshotLookups).
// Since S5 an order activates only on a clear trade compliance conclusion of CNX and, when its
// payment plan has a stage due after shipment or invoice, a credit reservation of STL; an amendment
// that asks for more credit applies only on a reservation for the revision it produces.
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
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/facts"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/guards"
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
	_ api.Commands              = (*Module)(nil)
	_ api.Queries               = (*Module)(nil)
	_ api.Lookups               = (*Module)(nil)
	_ api.AcceptanceLookups     = (*Module)(nil)
	_ api.ActiveOrderLookups    = (*Module)(nil)
	_ api.OrderSnapshotLookups  = (*Module)(nil)
	_ api.ActiveOrderCommands   = (*Module)(nil)
	_ api.OrderDocumentCommands = (*Module)(nil)
	_ api.Auditor               = (*Module)(nil)
	_ api.Reconciler            = (*Module)(nil)
)

// Upstream are the lookups of TRD's upstream contexts: EVD (SourceRef v1, R16), PTY (PartyRef,
// TradingProfileRef and AddressRoleRef v1, R13), CAT (SkuRef v1, R14), POL (ConfigRef v1 and
// NumberIssue v1, R15), INQ (RfqRef v1, R12) and CST (CostSnapshotRef v1 and the costing context it
// froze, R11); and the guards of STL (CreditGuard v1, R57) and CNX (TradeComplianceGuard v1, R58)
// that order.activate and order.amendment.apply call inside TRD's transaction.
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
	// Credit is STL's CreditGuard v1 (STLCreditGuard) and TradeCompliance CNX's
	// TradeComplianceGuard v1 (CNXTradeComplianceGuard). Both are guard functions run with TRD's
	// own executor, so TRD is composed before STL and CNX exist as modules (S5 decision D-2).
	Credit          CreditGuard
	TradeCompliance TradeComplianceGuard
	// Shipments is FUL's ShipmentFacts v1 (FULShipmentFacts, R70) and Commitments PUR's
	// DownstreamCommitments v1 (PURDownstreamCommitments, R71): the order end and the amendment
	// commands read them with TRD's executor inside TRD's transaction, without locks (S5 D-1).
	Shipments   ShipmentFacts
	Commitments DownstreamCommitments
}

// The reads of FUL's and PUR's facts as TRD runs them, and what they answer.
type (
	ShipmentFacts         = application.ShipmentFacts
	ShippedLine           = application.ShippedLine
	DownstreamCommitments = application.DownstreamCommitments
	DownstreamItem        = application.DownstreamItem
)

// FULShipmentFacts is FUL's ShipmentFacts v1 (Context Map R70, TRD-INV-018, TRD-INV-019) on FUL's
// published contract: brant_ful.order_shipped_v1, read with TRD's executor.
func FULShipmentFacts() ShipmentFacts { return facts.FULShipments }

// PURDownstreamCommitments is PUR's DownstreamCommitments v1 (R71, TRD-INV-019) on PUR's
// published contract: brant_pur.order_commitments_v1, read with TRD's executor.
func PURDownstreamCommitments() DownstreamCommitments { return facts.PURCommitments }

// The guards of STL and CNX as TRD calls them, and what passes through them. A composition (or a
// test) may wrap one, as it may any port.
type (
	CreditGuard          = application.CreditGuard
	CreditQuery          = application.CreditQuery
	CreditAnswer         = application.CreditAnswer
	TradeComplianceGuard = application.TradeComplianceGuard
	ComplianceAnswer     = application.ComplianceAnswer
	OrderRevisionRef     = application.OrderRevisionRef
)

// STLCreditGuard is STL's CreditGuard v1 (Context Map R57, TRD-INV-022) on STL's published
// contract: brant_stl.guard_credit_reservation_v1, called with TRD's executor.
func STLCreditGuard() CreditGuard { return guards.STLCredit }

// CNXTradeComplianceGuard is CNX's TradeComplianceGuard v1 (R58, TRD-INV-023) on CNX's published
// contract: brant_cnx.guard_trade_compliance_v1, called with TRD's executor.
func CNXTradeComplianceGuard() TradeComplianceGuard { return guards.CNXTradeCompliance }

// New wires TRD to db, a transaction owner that belongs to TRD alone, to the lookups of its
// upstream contexts and to the guards of STL and CNX. The freeze calls the guard functions of CAT,
// CST, INQ, POL and PTY itself and computes the quote on the server (application.ServerCalculator,
// WBS S3-02). A TRD without the credit or the trade compliance guard is refused: an order would
// activate with a recorded gap where TRD-INV-022 and TRD-INV-023 ask for a verdict.
func New(db *bentopg.DB, up Upstream) (*Module, error) {
	if up.Sources == nil || up.Parties == nil || up.Profiles == nil || up.Skus == nil || up.Configs == nil || up.Numbers == nil ||
		up.Rfqs == nil || up.Snapshots == nil || up.Contexts == nil {
		return nil, errors.New("trd: module needs the lookups of EVD, PTY, CAT, POL, INQ and CST and POL's number.issue")
	}
	if up.Credit == nil || up.TradeCompliance == nil {
		return nil, errors.New("trd: module needs STL's CreditGuard and CNX's TradeComplianceGuard")
	}
	if up.Shipments == nil || up.Commitments == nil {
		return nil, errors.New("trd: module needs FUL's ShipmentFacts and PUR's DownstreamCommitments")
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
		Orders: application.OrderGuards{Credit: up.Credit, TradeCompliance: up.TradeCompliance},
		Facts:  application.OrderFacts{Shipments: up.Shipments, Commitments: up.Commitments},
		Upstream: application.Upstream{Sources: up.Sources, Parties: up.Parties, Profiles: up.Profiles, Skus: up.Skus, Configs: up.Configs,
			Numbers: up.Numbers, Rfqs: up.Rfqs, Snapshots: up.Snapshots, Contexts: up.Contexts},
		Ledger: &action.Ledger{
			DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
			Outbox: outbox, Clock: clock, ReplayRead: application.CommandGetQuote, Visible: application.Visible,
		}}}, nil
}
