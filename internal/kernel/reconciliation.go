package kernel

// ReconcileState is what became of a command whose caller lost the answer, for instance after a
// COMMIT_UNCERTAIN transport error or a broken connection (contract 06, 动作对账). A reconciliation
// observes the command ledger and never executes the command again.
type ReconcileState string

const (
	// ReconcileCommitted: the command committed with this request; the stored result is returned
	// and a replay of the same idempotency key returns it too.
	ReconcileCommitted ReconcileState = "committed"
	// ReconcilePayloadConflict: the idempotency key committed a different request.
	ReconcilePayloadConflict ReconcileState = "payload_conflict"
	// ReconcileNotCommitted: nothing committed under the key in the caller's scope; the caller may
	// send the command again with the same key.
	ReconcileNotCommitted ReconcileState = "not_committed"
)
