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
}

func fulCommands(c FUL) *commandSet {
	// FUL's commands name their objects (shipment_id), so no member is renamed to id; the members
	// follow the reflective mapping (S3-D149).
	return &commandSet{bc: "ful", contract: fulapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile),
		commands: map[string]commandFunc{
			fulapi.CommandPrepareShipment: reflected("", "", receiptOf[fulapi.Receipt](c.PrepareShipment)),
			fulapi.CommandPlanPacking:     reflected("", "", receiptOf[fulapi.Receipt](c.PlanPacking)),
			fulapi.CommandRecordPacking:   reflected("", "", receiptOf[fulapi.Receipt](c.RecordPacking)),
			fulapi.CommandCancelShipment:  reflected("", "", receiptOf[fulapi.Receipt](c.CancelShipment)),
			fulapi.CommandConfirmShipment: reflected("", "", receiptOf[fulapi.Receipt](c.ConfirmShipment)),
		}}
}

// fulReads are FUL's read routes in the OpenAPI document.
var fulReads = []readRoute{
	{"/api/brant/v1/ful/shipments/{id}", "ful.shipment.get", "ful",
		"A shipment: the order revision it binds, its lines with their supplier delivery lines and quality releases, the parties its confirmation screens, its current packing plan and actual packing, and its confirmation with every guard's answer or its cancellation", []string{"id"}, false},
	{"/api/brant/v1/ful/orders/{id}/fulfilment", "ful.order_fulfilment.get", "ful",
		"An order's fulfilment: its lines at the current revision with what confirmed shipments shipped of each over every revision, what remains and what prepared shipments plan, and its shipments (those outside the caller's data scope without their IDs)", []string{"id"}, false},
}

// fulRoutes registers FUL's reads.
func (s *server) fulRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/brant/v1/ful/shipments/{id}", s.authenticated(read(s, s.cfg.FUL.GetShipment, okView[fulapi.ShipmentView])))
	mux.Handle("GET /api/brant/v1/ful/orders/{id}/fulfilment", s.authenticated(read(s, s.cfg.FUL.GetFulfilment, okView[fulapi.FulfilmentView])))
}
