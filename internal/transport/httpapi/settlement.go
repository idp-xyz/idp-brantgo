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
	stlapi.SettlementCommands
	stlapi.SettlementQueries
	stlapi.CreditNoteCommands
	stlapi.CreditNoteQueries
	stlapi.ResultCommands
	stlapi.ResultQueries
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
			stlapi.CommandConfirmSettlement: reflected("", "", receiptOf[stlapi.Receipt](c.ConfirmSettlement)),
			stlapi.CommandRequestCreditNote: reflected("", "", receiptOf[stlapi.Receipt](c.RequestCreditNote)),
			stlapi.CommandConfirmCredit:     reflected("", "", receiptOf[stlapi.Receipt](c.ConfirmCredit)),
			stlapi.CommandReverseSettlement: reflected("", "", receiptOf[stlapi.Receipt](c.ReverseSettlement)),
			stlapi.CommandRecordCharge:      reflected("", "", receiptOf[stlapi.Receipt](c.RecordCharge)),
			stlapi.CommandConfirmCharge:     reflected("", "", receiptOf[stlapi.Receipt](c.ConfirmCharge)),
			stlapi.CommandAllocateCharge:    reflected("", "", receiptOf[stlapi.Receipt](c.AllocateCharge)),
			stlapi.CommandReviewOrderResult: reflected("", "", receiptOf[stlapi.Receipt](c.ReviewOrderResult)),
			stlapi.CommandCloseOrderResult:  reflected("", "", receiptOf[stlapi.Receipt](c.CloseOrderResult)),
			stlapi.CommandReopenOrderResult: reflected("", "", receiptOf[stlapi.Receipt](c.ReopenOrderResult)),
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
	mux.Handle("GET /api/brant/v1/stl/settlement_statements/{id}", s.authenticated(read(s, s.cfg.STL.GetStatement, okView[stlapi.StatementView])))
	mux.Handle("GET /api/brant/v1/stl/receivables/{id}", s.authenticated(read(s, s.cfg.STL.GetReceivable, okView[stlapi.BalanceView])))
	mux.Handle("GET /api/brant/v1/stl/payables/{id}", s.authenticated(read(s, s.cfg.STL.GetPayable, okView[stlapi.BalanceView])))
	mux.Handle("GET /api/brant/v1/stl/orders/{id}/settlement", s.authenticated(read(s, s.cfg.STL.GetOrderSettlement,
		okView[stlapi.OrderSettlementView])))
	mux.Handle("GET /api/brant/v1/stl/credit_notes/{id}", s.authenticated(read(s, s.cfg.STL.GetCreditNote, okView[stlapi.CreditNoteView])))
	mux.Handle("GET /api/brant/v1/stl/charges/{id}", s.authenticated(read(s, s.cfg.STL.GetCharge, okView[stlapi.ChargeView])))
	mux.Handle("GET /api/brant/v1/stl/orders/{id}/result", s.authenticated(read(s, s.cfg.STL.GetOrderResult, okView[stlapi.OrderResultView])))
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
	{"/api/brant/v1/stl/settlement_statements/{id}", "stl.settlement_statement.get", "stl",
		"A settlement statement (sales: confirmed FUL shipment lines of one order; purchase: verified PUR supplier delivery lines of one supplier): its lines with the source each settles, its installments with what is applied and open, and the applications on it", []string{"id"}, false},
	{"/api/brant/v1/stl/receivables/{id}", "stl.receivable.get", "stl",
		"The business receivable of a sales statement: amount, applied and open, the installments by payment stage with their due instants and overdue days, and the applications on it (moved prepayments name the stage application they replace)", []string{"id"}, false},
	{"/api/brant/v1/stl/payables/{id}", "stl.payable.get", "stl",
		"The business payable of a purchase statement: amount, applied and open, its installment and the payments applied to it", []string{"id"}, false},
	{"/api/brant/v1/stl/orders/{id}/settlement", "stl.order_settlement.get", "stl",
		"An order's settlement position: each payment stage of the order account's revision with what stands prepaid, settled and paid on it, the order's receivables (settled, applied, open, overdue; a reversed statement counts nothing), net sales less effective credit notes, the purchase statement lines of the order with their payables, and its cross-currency applications with their business settlement FX differences", []string{"id"}, false},
	{"/api/brant/v1/stl/credit_notes/{id}", "stl.credit_note.get", "stl",
		"A credit note (CreditNoteRef v1) of a sales statement's receivable: kind (price allowance, return, claim), state (requested, confirmed, rejected), amount, what stands applied from it and its rest, whether it counts against the statement's sales, basis, evidence, requester and decider; it changes no shipped quantity, refunds nothing and adjusts no tax-refund material", []string{"id"}, false},
	{"/api/brant/v1/stl/charges/{id}", "stl.charge.get", "stl",
		"A charge: its source, the orders or shipments it benefits, POL fee category, currency and conversion basis, its estimate and amount, its state (estimated, pending, confirmed) with who recorded, billed and confirmed it, and every allocation (basis, data basis, weights and shares by the largest remainder) with the one that counts", []string{"id"}, false},
	{"/api/brant/v1/stl/orders/{id}/result", "stl.order_result.get", "stl",
		"An order's operating result in its analysis currency, derived now from its sources: forecast, executing and confirmed result (by shipment batch) with their sources and gaps, the business settlement FX difference and the net cash; the close conditions (fulfilment, receivables, payables, purchase costs, direct charges; claims not_enabled); the result's state, every immutable closing snapshot, review and reopening. Not legal profit or FX accounting", []string{"id"}, false},
}
