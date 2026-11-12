package httpapi

import (
	"net/http"

	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
)

// STL is what the transport needs from the settlement context (S5: customer credit first).
type STL interface {
	stlapi.Commands
	stlapi.Queries
	stlapi.Reconciler
	stlapi.Auditor
}

func stlCommands(c STL) *commandSet {
	// STL's commands name their objects (request_id, reservation_id, decision_id), so no member is
	// renamed to id.
	return &commandSet{bc: "stl", contract: stlapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile),
		commands: map[string]commandFunc{
			stlapi.CommandRequestLimit:    reflected("", "", receiptOf[stlapi.Receipt](c.RequestCreditLimit)),
			stlapi.CommandPublishLimit:    reflected("", "", receiptOf[stlapi.Receipt](c.PublishCreditLimit)),
			stlapi.CommandReserve:         reflected("", "", receiptOf[stlapi.Receipt](c.ReserveCredit)),
			stlapi.CommandDecideException: reflected("", "", receiptOf[stlapi.Receipt](c.DecideCreditException)),
			stlapi.CommandRelease:         reflected("", "", receiptOf[stlapi.Receipt](c.ReleaseCredit)),
			stlapi.CommandConsume:         reflected("", "", receiptOf[stlapi.Receipt](c.ConsumeCredit)),
		}}
}

// stlRoutes registers STL's audited reads.
func (s *server) stlRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/brant/v1/stl/customers/{id}/credit", s.authenticated(read(s, s.cfg.STL.GetCustomerCredit, okView[stlapi.CustomerCreditView])))
	mux.Handle("GET /api/brant/v1/stl/credit_reservations/{id}", s.authenticated(read(s, s.cfg.STL.GetReservation, okView[stlapi.ReservationView])))
	mux.Handle("GET /api/brant/v1/stl/credit_decisions/{id}", s.authenticated(read(s, s.cfg.STL.GetDecision, okView[stlapi.DecisionView])))
}

// stlReads are STL's reads in the OpenAPI document.
var stlReads = []readRoute{
	{"/api/brant/v1/stl/customers/{id}/credit", "stl.customer_credit.get", "stl",
		"A customer's credit: the limit version in force, every version, the limit requests, reservations and credit decisions the caller sees, and the exposure of the live reservations now", []string{"id"}, false},
	{"/api/brant/v1/stl/credit_reservations/{id}", "stl.credit_reservation.get", "stl",
		"A credit reservation of an order revision: its state now (reserved, expired, consumed or released), amounts, basis and history", []string{"id"}, false},
	{"/api/brant/v1/stl/credit_decisions/{id}", "stl.credit_decision.get", "stl",
		"A credit decision: its state now (open, released, rejected or expired), reasons, amounts, approvals and the reservation it allowed", []string{"id"}, false},
}
