package declaration

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Evidence is an EVD source an entry rests on (SourceRef v1) and its content hash.
type Evidence struct {
	SourceID      string `json:"source_id"`
	ContentSHA256 string `json:"content_sha256"`
}

// Confirmation is the confirmation of a version (human only).
type Confirmation struct {
	DeclarationID string
	Version       int
	Evidence      []Evidence
	Reason        string
	By            kernel.ActorRef
	Epoch         int64
	At            time.Time
}

// Submission is a manual submission of the confirmed version: the business operation's identifier,
// the broker (PTY party, and since S5R-D36 the verified service role of kind customs_broker it rests on) or our own
// clerk, when it was submitted and its evidence.
type Submission struct {
	DeclarationID       string
	No                  int
	Version             int
	OperationReference  string
	SubmitterKind       string
	SubmitterPartyID    string
	ServiceRoleID       string // "" for our own clerk, and for a broker's submission recorded before S5R
	ServiceRoleRevision int64
	ServiceRoleKind     string
	SubmitterName       string
	SubmittedAt         time.Time
	Evidence            []Evidence
	Note                string
	By                  kernel.ActorRef
	Epoch               int64
	At                  time.Time
}

// Receipt is a recorded external result, or an unknown one (no sequence, text, mapping or number).
type Receipt struct {
	ID                string
	DeclarationID     string
	No                int
	Outcome           string
	From              string
	Sequence          int64
	StatusCode        string
	OriginalText      string
	MappingVersion    string
	ReceivedAt        time.Time
	DeclarationNumber string
	ExportedAt        time.Time
	Version           int // the amendment version it made; 0 none
	Evidence          []Evidence
	Reason            string
	By                kernel.ActorRef
	Epoch             int64
	At                time.Time
}
