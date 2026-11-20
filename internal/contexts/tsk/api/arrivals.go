package api

// Arrivals of TRD (WBS S4-06b; Context Map R18 "TRD 集成事件 v1"; SM-TSK-TASK waiting_* → running
// by task.resume on what arrived). TSK's Inbox consumer tsk.task_progress reads TRD's integration
// events and resumes the waiting tasks whose wait (task.wait awaiting {kind, id, version}) names an
// object an event answers, at the awaited version or a later one (version 0 takes any), recording
// that object as the arrival; and it starts the verification of the tasks whose goal an event makes
// verifiable (PL-04, PL-06). The kinds below are the objects TRD's events answer; a task waiting on
// TRD names one of them.
//
//	event                      answers (kind: id @ version)
//	QuoteReviewDecided         quote_review: review_request_id @ business_revision; quote: quote_id @ business_revision
//	QuoteActivated             quote: quote_id @ business_revision — makes quote_ready verifiable
//	QuoteReplaced              quote: quote_id @ business_revision — makes quote_ready verifiable
//	QuoteApprovalRevoked       quote_approval: approval_id @ effectivity_version; quote — wakes the quote's waiting tasks
//	CustomerResponseRecorded   customer_response: response_id; quote
//	AcceptanceVerified         acceptance: acceptance_id @ effectivity_version; customer_response: response_id; quote
//	OrderReviewDecided         order_review: review_id @ order_revision; order: order_id @ order_revision; quote
//	OrderActivated             order: order_id @ order_revision; quote — makes order_active verifiable
//	OrderApprovalRevoked       order_review: review_id @ order_revision; order; quote — wakes the quote's waiting tasks
const (
	ArrivalQuote            = "quote"
	ArrivalQuoteReview      = "quote_review"
	ArrivalQuoteApproval    = "quote_approval"
	ArrivalCustomerResponse = "customer_response"
	ArrivalAcceptance       = "acceptance"
	ArrivalOrder            = "order"
	ArrivalOrderReview      = "order_review"
)
