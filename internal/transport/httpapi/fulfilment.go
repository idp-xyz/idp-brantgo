package httpapi

import (
	"net/http"
	"strconv"

	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// FUL is what the transport needs from the fulfilment context.
type FUL interface {
	fulapi.Commands
	fulapi.Queries
	fulapi.ReleaseCommands
	fulapi.ReleaseQueries
	fulapi.Reconciler
	fulapi.Auditor
	fulapi.MilestoneCommands
	fulapi.MilestoneQueries
	fulapi.ExportLotCommands
	fulapi.ExportLotQueries
	// S5-08: bookings and the shipment document set.
	fulapi.BookingCommands
	fulapi.BookingQueries
	fulapi.DocumentCommands
	fulapi.DocumentQueries
	// S5-16: the CIF/CIP insurance policy record.
	fulapi.InsuranceCommands
	fulapi.InsuranceQueries
	// S5-09b: shipment reviews after an order change.
	fulapi.ReviewCommands
	fulapi.ReviewQueries
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
			fulapi.CommandEvaluateRelease:       reflected("", "", receiptOf[fulapi.Receipt](c.EvaluateRelease)),
			fulapi.CommandGrantReleaseException: reflected("", "", receiptOf[fulapi.Receipt](c.GrantReleaseException)),
			fulapi.CommandRecordDocumentRelease: reflected("", "", receiptOf[fulapi.Receipt](c.RecordDocumentRelease)),
			// S5-08
			fulapi.CommandRecordBooking:                reflected("", "", c.RecordBooking),
			fulapi.CommandPrepareShipmentDocument:      reflected("", "", receiptOf[fulapi.Receipt](c.PrepareShipmentDocument)),
			fulapi.CommandIssueShipmentDocuments:       reflected("", "", receiptOf[fulapi.Receipt](c.IssueShipmentDocuments)),
			fulapi.CommandRecordSignedShipmentDocument: reflected("", "", c.RecordSignedShipmentDocument),
			// S5-16
			fulapi.CommandRecordInsurance: reflected("", "", receiptOf[fulapi.Receipt](c.RecordInsurance)),
			// S5-09b: a review names its object review_id; shipment_review.open is the Inbox consumer's
			// and is not routed.
			fulapi.CommandSettleShipmentReview: reflected("", "", c.SettleShipmentReview),
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
	{"/api/brant/v1/ful/commercial_releases/{id}", "ful.commercial_release.get", "ful",
		"A commercial release of a shipment and scope (shipment or documents): its state, every evaluation with its outcome, the first unmet guard and code when blocked, each guard's answer, its SHA-256 and what each guard bound (trade compliance check, licence allocation, screenings, supplier delivery lines, quality releases, payment condition, order), the payment-condition exceptions, and what consumed it", []string{"id"}, false},
	{"/api/brant/v1/ful/shipments/{id}/commercial_releases", "ful.commercial_release.list", "ful",
		"The commercial releases of a shipment, shipment scope first", []string{"id"}, false},
	{"/api/brant/v1/ful/shipments/{id}/bookings", "ful.booking.get", "ful",
		"A shipment's booking: the current version and every version with the forwarder's or carrier's service role, booking number, vessel and voyage, ETD, cut-off, containers, ports, evidence and the reason of a correction", []string{"id"}, false},
	{"/api/brant/v1/ful/document_sets/{id}", "ful.shipment_document_set.get", "ful",
		"A shipment document set: every version of its commercial invoice, packing list, bill of lading and certificate of origin records with state, number, POL revisions, sources, renditions, commit evidence and signed copies, its issues with the differences they accepted, and the item-by-item reconciliation now (differences with their paths)", []string{"id"}, false},
	{"/api/brant/v1/ful/document_sets/{id}/documents/{kind}/versions/{version}/{language}", "ful.shipment_document.get", "ful",
		"One language of a rendered commercial invoice or packing list version with its bytes in Base64, verified against the stored SHA-256 on every read", []string{"id", "kind", "version", "language"}, false},
	{"/api/brant/v1/ful/document_sets/{id}/insurance", "ful.insurance.get", "ful",
		"A document set's insurance: the Incoterm of the commercial invoice it stands on and whether that needs a policy (CIF, CIP), the current policy record and FUL-INV-005 applied to it now (invoice total, percentage, amount required HALF_UP, amount insured, sufficient), and every version with insurer, policy number, clauses, amounts, file, the cover and the premium's comparison with the costing's insurance component (equal, different, not_comparable, no_component, unavailable) found when it was recorded", []string{"id"}, false},
	{"/api/brant/v1/ful/shipment_reviews/{id}", "ful.shipment_review.get", "ful",
		"A shipment review after an order change: the order revision, change and TRD event it follows, its state (open, settled or superseded) and responsible role, every item — a prepared shipment that can no longer be confirmed with its lines set against the amended revision, its current packing plan and actual packing, booking and draft documents, a granted release nothing can consume — with what FUL knew of it then, and each item's disposition with the replacement shipment and counterpart it names; the caller's access to the order is re-checked", []string{"id"}, false},
	{"/api/brant/v1/ful/orders/{id}/shipment_reviews", "ful.shipment_review.list", "ful",
		"The shipment reviews of a sales order, by order version", []string{"id"}, false},
}

// fulRoutes registers FUL's reads.
func (s *server) fulRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/brant/v1/ful/shipments/{id}", s.authenticated(read(s, s.cfg.FUL.GetShipment, okView[fulapi.ShipmentView])))
	mux.Handle("GET /api/brant/v1/ful/orders/{id}/fulfilment", s.authenticated(read(s, s.cfg.FUL.GetFulfilment, okView[fulapi.FulfilmentView])))
	mux.Handle("GET /api/brant/v1/ful/milestone_plans/{id}", s.authenticated(read(s, s.cfg.FUL.GetMilestonePlan, okView[fulapi.MilestonePlanView])))
	mux.Handle("GET /api/brant/v1/ful/export_lots/{id}", s.authenticated(read(s, s.cfg.FUL.GetExportLot, okView[fulapi.ExportLotView])))
	mux.Handle("GET /api/brant/v1/ful/commercial_releases/{id}", s.authenticated(read(s, s.cfg.FUL.GetRelease, okView[fulapi.ReleaseView])))
	mux.Handle("GET /api/brant/v1/ful/shipments/{id}/commercial_releases", s.authenticated(read(s, s.cfg.FUL.ShipmentReleases,
		okView[fulapi.ShipmentReleases])))
	mux.Handle("GET /api/brant/v1/ful/shipments/{id}/bookings", s.authenticated(read(s, s.cfg.FUL.GetBookings, okView[fulapi.ShipmentBookingsView])))
	mux.Handle("GET /api/brant/v1/ful/document_sets/{id}", s.authenticated(read(s, s.cfg.FUL.GetDocumentSet, okView[fulapi.DocumentSetView])))
	mux.Handle("GET /api/brant/v1/ful/document_sets/{id}/documents/{kind}/versions/{version}/{language}", s.authenticated(s.shipmentDocument))
	mux.Handle("GET /api/brant/v1/ful/document_sets/{id}/insurance", s.authenticated(read(s, s.cfg.FUL.GetInsurance, okView[fulapi.InsuranceView])))
	mux.Handle("GET /api/brant/v1/ful/shipment_reviews/{id}", s.authenticated(read(s, s.cfg.FUL.GetShipmentReview, okView[fulapi.ShipmentReviewView])))
	mux.Handle("GET /api/brant/v1/ful/orders/{id}/shipment_reviews", s.authenticated(read(s, s.cfg.FUL.ListShipmentReviews,
		okView[fulapi.OrderShipmentReviews])))
}

// shipmentDocument answers GET /api/brant/v1/ful/document_sets/<id>/documents/<kind>/versions/<version>/<language>:
// one language of a rendered version with its bytes (Base64), verified on every read.
func (s *server) shipmentDocument(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil || version < 1 {
		s.fail(w, r, kernel.Reject(kernel.CodeSchemaInvalid, "/version", "positive integer"))
		return
	}
	v, err := s.cfg.FUL.GetShipmentDocument(r.Context(), r.PathValue("id"), r.PathValue("kind"), version, r.PathValue("language"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, okView(v))
}
