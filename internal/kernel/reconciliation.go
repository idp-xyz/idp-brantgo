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

// ActionRequest is a command as its context's action ledger records it: the command name, the
// idempotency key and the request hash (the SHA-256 of the business arguments the context hashes,
// without the key and the expected version). ActionReconciliation v1 (Context Map R18, R66–R69)
// reconciles by it: a caller that persisted it before sending the command (TSK's ActionExecution,
// TSK-INV-003) learns later what became of the command without holding the command itself.
type ActionRequest struct {
	Command        string
	IdempotencyKey string
	RequestHash    string
}

// ValidRequestHash reports whether h has the form of a request hash: 64 lower-case hexadecimal
// digits.
func ValidRequestHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		if c := h[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
