package api

import "context"

// OrderRequestChecks is what TRD publishes for a caller that needs to know, before a person is
// asked to approve an AI agent's order command, whether TRD would refuse the request (S4 G7b).
//
// Each check judges the request as the command itself judges it, on the stored records as they are
// now, in a read transaction: it takes no lock, writes nothing and uses no idempotency key (the cmd's
// IdempotencyKey may be empty and is ignored) and no approval grant. It returns nil when the command would not be refused for anything it can judge
// that way, and otherwise the *kernel.Rejection the command answers first (the same code and the
// same path). The check is audited in TRD under the caller's Principal, as order.prepare.check and
// order.review.request.check, and a caller needs the permission of the command it checks.
//
// What a check cannot judge stays with the command, which judges it inside its own transaction on
// the locked records: a change between the check and the command (a conversion slot taken, a
// version moved, a revision withdrawn, an address revised), the object control fence of the quote
// root (the task's claim exists only on the send), an idempotent replay, and the caller's authority
// when the command runs. A request the check accepts can therefore still be refused by the command;
// one it refuses is refused by the command too, unless the records changed in between.
type OrderRequestChecks interface {
	// CheckPrepareOrder judges order.prepare: the request's own validation; the quote and its
	// formal revision (NOT_FOUND for a quote the caller does not see; the revision usable at the
	// hash), the order ID (BUSINESS_KEY_CONFLICT for another quote's order), the acceptance of the
	// revision, the facts the order seals (trade terms, customer countries and the notify party:
	// REFERENCE_INVALID, REFERENCE_STALE, INVALID_KNOWN_VALUE for an address of another role at
	// /args/notify_party_ref) and the conversion slot (CONVERSION_SLOT_CONFLICT).
	CheckPrepareOrder(ctx context.Context, cmd PrepareOrder) error
	// CheckRequestOrderReview judges order.review.request: the request's own validation; the
	// order (NOT_FOUND for an order of a quote the caller does not see), its row version
	// (VERSION_CONFLICT), that it is a draft (ORDER_NOT_DRAFT), that it has no review of that ID
	// (ORDER_REVIEW_IMMUTABLE) and no open review (INVALID_STATE).
	CheckRequestOrderReview(ctx context.Context, cmd RequestOrderReview) error
}
