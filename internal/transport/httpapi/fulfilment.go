package httpapi

import (
	"net/http"

	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
)

// FUL is what the transport needs from the fulfilment context.
type FUL interface {
	fulapi.Commands
	fulapi.Queries
	fulapi.Reconciler
	fulapi.Auditor
	fulapi.MilestoneCommands
	fulapi.MilestoneQueries
	fulapi.ExportLotCommands
	fulapi.ExportLotQueries
}

func fulCommands(c FUL) *commandSet {
	// FUL's commands name their objects (shipment_id, plan_id, export_lot_id), so no member is
	// renamed to id; the members follow the reflective mapping (S3-D149).
	return &commandSet{bc: "ful", contract: fulapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile),
		commands: map[string]commandFunc{
			fulapi.CommandPrepareShipment:       reflected("", "", receiptOf[fulapi.Receipt](c.PrepareShipment)),
			fulapi.CommandPlanPacking:           reflected("", "", receiptOf[fulapi.Receipt](c.PlanPacking)),
			fulapi.CommandRecordPacking:         reflected("", "", receiptOf[fulapi.Receipt](c.RecordPacking)),
			fulapi.CommandCancelShipment:        reflected("", "", receiptOf[fulapi.Receipt](c.CancelShipment)),
			fulapi.CommandConfirmShipment:       reflected("", "", receiptOf[fulapi.Receipt](c.ConfirmShipment)),
			fulapi.CommandPlanMilestones:        reflected("", "", c.PlanMilestones),
			fulapi.CommandRecordMilestoneActual: reflected("", "", c.RecordMilestoneActual),
			fulapi.CommandRegisterExportLot:     reflected("", "", c.RegisterExportLot),
		}}
}

// fulReads are FUL's read routes in the OpenAPI document.
var fulReads = []readRoute{
	{"/api/brant/v1/ful/shipments/{id}", "ful.shipment.get", "ful",
		"A shipment: the order revision it binds, its lines with their supplier delivery lines and quality releases, the parties its confirmation screens, its current packing plan and actual packing, and its confirmation with every guard's answer or its cancellation", []string{"id"}, false},
	{"/api/brant/v1/ful/orders/{id}/fulfilment", "ful.order_fulfilment.get", "ful",
		"An order's fulfilment: its lines at the current revision with what confirmed shipments shipped of each over every revision, what remains and what prepared shipments plan, and its shipments (those outside the caller's data scope without their IDs)", []string{"id"}, false},
	{"/api/brant/v1/ful/milestone_plans/{id}", "ful.milestone_plan.get", "ful",
		"A milestone plan: the order or shipment it follows, the nodes of its current revision with the latest supplier claim, internal verification and accepted fact of each and whether it is overdue now, and every actual in the order recorded", []string{"id"}, false},
	{"/api/brant/v1/ful/export_lots/{id}", "ful.export_lot.get", "ful",
		"An export lot (ExportLotRef v1): the shipment and order it is of, its lines with quantities and amounts at the order's prices, its totals, and how it stands against its shipment now (awaiting_confirmation, matched or review_required with the differences)", []string{"id"}, false},
}

// fulRoutes registers FUL's reads.
func (s *server) fulRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/brant/v1/ful/shipments/{id}", s.authenticated(read(s, s.cfg.FUL.GetShipment, okView[fulapi.ShipmentView])))
	mux.Handle("GET /api/brant/v1/ful/orders/{id}/fulfilment", s.authenticated(read(s, s.cfg.FUL.GetFulfilment, okView[fulapi.FulfilmentView])))
	mux.Handle("GET /api/brant/v1/ful/milestone_plans/{id}", s.authenticated(read(s, s.cfg.FUL.GetMilestonePlan, okView[fulapi.MilestonePlanView])))
	mux.Handle("GET /api/brant/v1/ful/export_lots/{id}", s.authenticated(read(s, s.cfg.FUL.GetExportLot, okView[fulapi.ExportLotView])))
}
