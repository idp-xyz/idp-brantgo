package httpapi

import (
	"net/http"

	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
)

// CNX is what the transport needs from the China export context.
type CNX interface {
	cnxapi.Commands
	cnxapi.Queries
	cnxapi.Reconciler
	cnxapi.Auditor
}

func cnxCommands(c CNX) *commandSet {
	// CNX's commands name their objects (check_id, order_id), so no member is renamed to id; the
	// members follow the reflective mapping (S3-D149).
	return &commandSet{bc: "cnx", contract: cnxapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile),
		commands: map[string]commandFunc{
			cnxapi.CommandEvaluate: reflected("", "", receiptOf[cnxapi.Receipt](c.Evaluate)),
			cnxapi.CommandDecide:   reflected("", "", receiptOf[cnxapi.Receipt](c.Decide)),
			cnxapi.CommandReopen:   reflected("", "", receiptOf[cnxapi.Receipt](c.Reopen)),
		}}
}

// cnxReads are CNX's read routes in the OpenAPI document.
var cnxReads = []readRoute{
	{"/api/brant/v1/cnx/trade_compliance_checks/{id}", "cnx.trade_compliance.get", "cnx",
		"A trade compliance check of an order revision: its state now as TradeComplianceGuard v1 decides it, every evaluation with its facts, findings and gaps, every decision and reopening", []string{"id"}, false},
	{"/api/brant/v1/cnx/orders/{id}/trade_compliance", "cnx.trade_compliance.order_checks", "cnx",
		"The trade compliance checks of a sales order's revisions, newest first, each with its state now", []string{"id"}, false},
}

// cnxRoutes registers CNX's reads.
func (s *server) cnxRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/brant/v1/cnx/trade_compliance_checks/{id}", s.authenticated(read(s, s.cfg.CNX.GetCheck, okView[cnxapi.CheckView])))
	mux.Handle("GET /api/brant/v1/cnx/orders/{id}/trade_compliance", s.authenticated(s.orderComplianceChecks))
}

// orderComplianceChecks answers GET /api/brant/v1/cnx/orders/<id>/trade_compliance.
func (s *server) orderComplianceChecks(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.CNX.OrderChecks(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v == nil {
		v = []cnxapi.CheckSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "order_id": r.PathValue("id"), "checks": wire(v)})
}
