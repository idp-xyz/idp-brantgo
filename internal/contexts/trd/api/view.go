package api

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// QuoteView is a quote root with all its revisions, oldest first.
type QuoteView struct {
	QuoteID         string
	RootVersion     int64
	WorkingRevision int64 // 0: none
	ActiveRevision  int64 // 0: none
	LastRevision    int64
	Labels          []string
	InternalNote    string
	Control         ControlView
	Revisions       []RevisionView
}

// ControlView is the root's object control.
type ControlView struct {
	State        string // unbound, bound or stopping
	ControlEpoch int64
	TaskID       string // bound and stopping
	ChangedBy    kernel.ActorRef
	ChangedAt    time.Time
}

// RevisionView is one business revision. Data is the draft as saved (for a frozen revision, as it
// was frozen; ExpiresAt in kernel.FormatTimestamp form). A frozen revision has its business hash,
// its issued_at and expires_at, who froze it, and Commercial: the normalized canonical commercial
// document, byte for byte what the business hash covers (Python protocol.business_hash). A
// discarded revision has who ended it, why and on which evidence.
type RevisionView struct {
	Revision       int64
	Status         string
	BaseRevision   int64 // 0: none
	ContentHash    string
	Data           QuoteData
	Contributors   []string
	SavedBy        kernel.ActorRef
	SavedAt        time.Time
	BusinessHash   string
	IssuedAt       time.Time
	ExpiresAt      time.Time
	FrozenBy       kernel.ActorRef
	FreezeEpoch    int64
	FrozenAt       time.Time
	Commercial     string
	DiscardedBy    kernel.ActorRef
	DiscardedAt    time.Time
	DiscardReason  string
	DiscardProofs  []Evidence
	AuthorityEpoch int64 // of the last save
}
