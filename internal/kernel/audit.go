package kernel

import "time"

// AuditEntry is one line of a context's audit trail: who did what to which object, with which
// outcome. It never holds request content (Python test_limits_unknown_contract_and_sensitive_audit).
type AuditEntry struct {
	Seq            int64
	Actor          ActorRef
	Command        string
	ObjectID       string
	Outcome        string // succeeded, replayed, rejected or read
	Code           string // the rejection code of a rejected outcome
	CorrelationID  string
	RecordedAt     time.Time
	Reason         string // the reason a cancelling or closing command gave
	AuthorityEpoch int64  // the authority epoch the check saw; 0 for a rejection
}
