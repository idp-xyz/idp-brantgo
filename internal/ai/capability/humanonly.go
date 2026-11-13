package capability

import "slices"

// humanOnly is the tactical model's humanOnlyCommands (docs/05_TACTICAL_DDD/02_IDP_Brant_Tactical_DDD_
// Candidate_V0.1.json, 111 entries, sorted), the single source of the commands only a human may issue
// (UL-INV-17, X-INV-002). TestHumanOnlyListIsTheTacticalModels keeps it in step with the archive.
var humanOnly = []string{
	"acceptance.invalidate", "acceptance.mark_disputed", "ai.approval_grant.issue", "ai.approval_grant.revoke",
	"ai.delegation.grant", "ai.delegation.revoke", "allocation_review.settle", "application.apply",
	"application.unapply", "certificate.register", "charge.confirm", "commercial_release.grant_exception",
	"config.discard_draft", "config.publish", "config.retire", "contact.confirm", "cost_snapshot.freeze",
	"costing.attest_source", "costing_context.verify", "costing_context.withdraw", "credit.confirm",
	"credit_exception.decide", "credit_limit.publish", "declaration_case.confirm", "declaration_case.record_receipt",
	"declaration_case.record_submission", "declaration_profile.verify", "disposition.decide", "document_release.record",
	"export_license.register", "export_license.release", "external_reconciliation.resolve", "fund.record",
	"fund.resolve_difference", "item_code_mapping.publish", "logistics_offer.verify", "logistics_offer.withdraw",
	"measurement.record_verified", "measurement.supersede", "measurement.withdraw", "object_control.release",
	"object_control.stop", "opening_balance.record", "order.activate", "order.amendment.apply",
	"order.amendment.decide", "order.amendment.submit", "order.amendment.withdraw", "order.approval.revoke",
	"order.cancel", "order.close", "order.close_remainder", "order.close_unfulfilled", "order.document.record_signed",
	"order.review.decide", "order_result.close", "order_result.reopen", "party.confirm", "party.deactivate",
	"party_screening.block", "party_screening.decide", "payment_plan_review.settle", "purchase.commitment.cancel",
	"purchase.commitment.change.apply", "purchase.commitment.change.decide", "purchase.commitment.decide",
	"purchase.commitment.issue", "purchase.commitment.revoke", "purchase.draft.prepare", "purchase.draft.void",
	"purchase_invoice.verify", "purchase_terms.publish", "purchase_terms.retire", "quality_release.grant",
	"quality_release.revoke", "quote.activate", "quote.approval.revoke", "quote.cancel", "quote.freeze",
	"quote.replace", "quote.response.verify", "quote.review.decide", "quote.submit_review", "quote.withdraw",
	"rfq.cancel", "rfq.close", "rfq.confirm_scope", "sample.approve", "service_role.verify", "settlement.confirm",
	"settlement.reverse", "shipment.confirm", "shipment_document.issue", "shipment_document.record_signed",
	"shipment_review.settle", "sku.confirm", "sku.deactivate", "supplier_delivery.verify", "supplier_offer.verify",
	"supplier_offer.withdraw", "supplier_rfq.close", "supplier_rfq.record_issued", "task.cancel", "task.fail",
	"task.handoff", "task.release", "task.take_control", "tax_refund_case.record_opening",
	"tax_refund_case.record_result", "trade_compliance.decide", "trading_profile.publish",
}

// IsHumanOnly reports whether command is one of the tactical model's humanOnlyCommands.
func IsHumanOnly(command string) bool {
	_, found := slices.BinarySearch(humanOnly, command)
	return found
}

// HumanOnlyCommands returns the tactical model's humanOnlyCommands in order.
func HumanOnlyCommands() []string { return slices.Clone(humanOnly) }
