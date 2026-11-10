// Package source holds the four-layer identity of a text source (T01 §18.2): SourceContent is the
// text, deduplicated by SHA-256 within a scope; SourceOrigin is where the text came from, an email
// identified by provider, account and message ID; SourceImport records each accepted registration;
// what a business object does with a source belongs to that object. All four are immutable
// (EVD-INV-001).
package source

import (
	"crypto/sha256"
	"encoding/hex"
)

// Kind of source.
type Kind string

const (
	ManualText Kind = "manual_text"
	EmailText  Kind = "email_text"
)

// Channel is the identity of an email: the message is the one with ExternalMessageID in Account
// of Provider. AuthorClaim is who the message says it comes from; a hash proves no author.
type Channel struct {
	Provider          string
	AccountRef        string
	ExternalMessageID string
	AuthorClaim       string
}

// Registration is a request to register text under an origin ID.
type Registration struct {
	OriginID string
	Kind     Kind
	Channel  Channel // zero for a manual text
	Text     string
}

// ContentHash is the SHA-256 of the text encoded as UTF-8, without normalizing line breaks or
// white space: different bodies never deduplicate.
func (r Registration) ContentHash() string {
	sum := sha256.Sum256([]byte(r.Text))
	return hex.EncodeToString(sum[:])
}

// Origin is a registered origin with the hash of its content.
type Origin struct {
	ID          string
	ContentID   string
	ContentHash string
	Kind        Kind
	Channel     Channel
	OwnerActor  string
	OwnerTeam   string
}

// Conflict is returned when a registration names a known origin, or a known email, with other
// content or another identity.
type Conflict struct{}

func (Conflict) Error() string { return "source: identity conflict" }

// Match decides which known origin a registration resolves to. byID is the origin with the
// registration's ID, byChannel the origin of the same email; either may be nil. It returns the
// origin to record a new import for, nil when a new origin must be created, or Conflict.
//
// The same email under a new ID resolves to the email's first origin; the same text from another
// email or under another manual ID is a new origin that shares the content.
func Match(r Registration, byID, byChannel *Origin) (*Origin, error) {
	if byID != nil && byChannel != nil && byID.ID != byChannel.ID {
		return nil, Conflict{}
	}
	existing := byChannel
	if existing == nil {
		existing = byID
	}
	if existing == nil {
		return nil, nil
	}
	if existing.ContentHash != r.ContentHash() || existing.Kind != r.Kind || existing.Channel != r.Channel {
		return nil, Conflict{}
	}
	return existing, nil
}
