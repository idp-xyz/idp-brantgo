package httpapi

import (
	"net/http"

	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
)

// STL is what the transport needs from the settlement context (S5: customer credit, then funds,
// applications and order accounts).
type STL interface {
	stlapi.Commands
	stlapi.FundCommands
	stlapi.Queries
	stlapi.FundQueries
	stlapi.ReviewCommands
	stlapi.ReviewQueries
	stlapi.Reconciler
	stlapi.Auditor
}

func stlCommands(c STL) *commandSet {
	// STL's commands name their objects (request_id, reservation_id, decision_id, fund_record_id,
	// order_id, application_id), so no member is renamed to id.
	return &commandSet{bc: "stl", contract: stlapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile),
		commands: map[string]commandFunc{
			stlapi.CommandRequestLimit:      reflected("", "", receiptOf[stlapi.Receipt](c.RequestCreditLimit)),
			stlapi.CommandPublishLimit:      reflected("", "", receiptOf[stlapi.Receipt](c.PublishCreditLimit)),
			stlapi.CommandReserve:           reflected("", "", receiptOf[stlapi.Receipt](c.ReserveCredit)),
			stlapi.CommandDecideException:   reflected("", "", receiptOf[stlapi.Receipt](c.DecideCreditException)),
			stlapi.CommandRelease:           reflected("", "", receiptOf[stlapi.Receipt](c.ReleaseCredit)),
			stlapi.CommandConsume:           reflected("", "", receiptOf[stlapi.Receipt](c.ConsumeCredit)),
			stlapi.CommandRecordFund:        reflected("", "", receiptOf[stlapi.Receipt](c.RecordFund)),
			stlapi.CommandResolveDifference: reflected("", "", receiptOf[stlapi.Receipt](c.ResolveFundDifference)),
			stlapi.CommandOpenOrderAccount:  reflected("", "", receiptOf[stlapi.Receipt](c.OpenOrderAccount)),
			stlapi.CommandApply:             reflected("", "", receiptOf[stlapi.Receipt](c.Apply)),
			stlapi.CommandUnapply:           reflected("", "", receiptOf[stlapi.Receipt](c.Unapply)),
			stlapi.CommandOpenReview:        reflected("", "", receiptOf[stlapi.Receipt](c.OpenPaymentPlanReview)),
			stlapi.CommandSettleReview:      reflected("", "", receiptOf[stlapi.Receipt](c.SettlePaymentPlanReview)),
		}}
}

// stlRoutes registers STL's audited reads.
func (s *server) stlRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/brant/v1/stl/customers/{id}/credit", s.authenticated(read(s, s.cfg.STL.GetCustomerCredit, okView[stlapi.CustomerCreditView])))
	mux.Handle("GET /api/brant/v1/stl/credit_reservations/{id}", s.authenticated(read(s, s.cfg.STL.GetReservation, okView[stlapi.ReservationView])))
	mux.Handle("GET /api/brant/v1/stl/credit_decisions/{id}", s.authenticated(read(s, s.cfg.STL.GetDecision, okView[stlapi.DecisionView])))
	mux.Handle("GET /api/brant/v1/stl/fund_records/{id}", s.authenticated(read(s, s.cfg.STL.GetFundRecord, okView[stlapi.FundRecordView])))
	mux.Handle("GET /api/brant/v1/stl/order_accounts/{id}", s.authenticated(read(s, s.cfg.STL.GetOrderAccount, okView[stlapi.OrderAccountView])))
	mux.Handle("GET /api/brant/v1/stl/applications/{id}", s.authenticated(read(s, s.cfg.STL.GetApplication, okView[stlapi.ApplicationView])))
	mux.Handle("GET /api/brant/v1/stl/order_accounts/{id}/payment_plan_reviews", s.authenticated(read(s, s.cfg.STL.ListPaymentPlanReviews,
		okView[stlapi.PaymentPlanReviewsView])))
}

// stlReads are STL's reads in the OpenAPI document.
var stlReads = []readRoute{
	{"/api/brant/v1/stl/customers/{id}/credit", "stl.customer_credit.get", "stl",
		"A customer's credit: the limit version in force, every version, the limit requests, reservations and credit decisions the caller sees, and the exposure of the live reservations now", []string{"id"}, false},
	{"/api/brant/v1/stl/credit_reservations/{id}", "stl.credit_reservation.get", "stl",
		"A credit reservation of an order revision: its state now (reserved, expired, consumed or released), amounts, basis and history", []string{"id"}, false},
	{"/api/brant/v1/stl/credit_decisions/{id}", "stl.credit_decision.get", "stl",
		"A credit decision: its state now (open, released, rejected or expired), reasons, amounts, approvals and the reservation it allowed", []string{"id"}, false},
	{"/api/brant/v1/stl/fund_records/{id}", "stl.fund_record.get", "stl",
		"A fund record (FundRecordRef v1): kind, bank account, reference and evidence, remitted, received, stated charges, the difference resolved and unexplained, what is applied, drawn and still available, and its commercial allocations (not 出口收汇核销)", []string{"id"}, false},
	{"/api/brant/v1/stl/order_accounts/{id}", "stl.order_account.get", "stl",
		"An order's account: the payment stages of the revision it holds with what is applied and open on each, the applications, and the payment condition per scope (shipment, documents) now", []string{"id"}, false},
	{"/api/brant/v1/stl/applications/{id}", "stl.application.get", "stl",
		"An application of a receipt to an order payment stage: amount, state (applied or unapplied) and who applied and unapplied it", []string{"id"}, false},
	{"/api/brant/v1/stl/order_accounts/{id}/payment_plan_reviews", "stl.payment_plan_review.list", "stl",
		"An order account's payment plan reviews, latest order revision first: the revision each moved the account from and to, its state (open, settled or superseded), responsible role, items (stage removed, stage over-applied, credit portion changed) and their dispositions", []string{"id"}, false},
}
