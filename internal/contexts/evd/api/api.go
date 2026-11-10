// Package api is the contract of the EVD bounded context (evidence: sources and business files)
// for transports, the AI layer and the contexts downstream of EVD. It exposes no EVD domain type.
package api

import (
	"context"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// Contract names the request and response shapes of EVD commands.
	Contract = "brant.evd/0.1.0"
	// Source identifies EVD as the producer of integration events.
	Source = "idp-brant.evd"
	// EventSourceRegistered is published when a new source origin is registered.
	EventSourceRegistered = "idp-brant.evd.source_registered.v1"

	CommandRegisterSource = "source.register"

	// CodeSourceIdentityConflict: the source ID or the channel message is known with other content
	// or another channel identity (EVD-INV-001).
	CodeSourceIdentityConflict = "SOURCE_IDENTITY_CONFLICT"

	KindManualText = "manual_text"
	KindEmailText  = "email_text"

	// MaxContentCharacters bounds a text source (Python T01: 50,000 characters).
	MaxContentCharacters = 50000
)

// RegisterSource registers a text source. The same content in one scope is stored once; an email
// is identified by provider, account and message ID, and the same message with another body is
// refused. Every accepted request records an import, except a replay of the same idempotency key.
type RegisterSource struct {
	IdempotencyKey    string
	SourceID          string // args.id
	ContentText       string
	SourceKind        string // manual_text or email_text
	Provider          string // email only
	AccountRef        string // email only
	ExternalMessageID string // email only
	AuthorClaim       string // email only: who the message claims to come from, not verified
}

// Validate checks the closed contract (Python BC_T01_commands.schema.json, source.register).
func (c RegisterSource) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.SourceID):
		return schema("/args/id")
	case !text(c.ContentText, MaxContentCharacters):
		return schema("/args/content_text")
	}
	channel := c.Provider != "" || c.AccountRef != "" || c.ExternalMessageID != "" || c.AuthorClaim != ""
	switch c.SourceKind {
	case KindManualText:
		if channel {
			return schema("/args/source_kind") // a manual text has no channel identity
		}
	case KindEmailText:
		switch {
		case !kernel.ValidIdentifier(c.Provider):
			return schema("/args/provider")
		case !kernel.ValidIdentifier(c.AccountRef):
			return schema("/args/account_ref")
		case !text(c.ExternalMessageID, 200):
			return schema("/args/external_message_id")
		case !text(c.AuthorClaim, 200):
			return schema("/args/author_claim")
		}
	default:
		return schema("/args/source_kind")
	}
	return nil
}

// Receipt is the closed success answer of a command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Commands is EVD's command entry point. Errors are *kernel.Rejection for refusals; anything else
// is an infrastructure failure.
type Commands interface {
	RegisterSource(ctx context.Context, cmd RegisterSource) (Receipt, error)
}

// SourceRef is SourceRef v1: a source origin and the hash of its content.
type SourceRef struct {
	SourceID      string
	ContentSHA256 string
}

// SourceImport is one accepted registration of a source.
type SourceImport struct {
	ImportID   string
	Actor      kernel.ActorRef
	ImportedAt time.Time
}

// SourceView is a source with its content and imports, for a reader allowed to see it.
type SourceView struct {
	SourceID          string
	SourceKind        string
	Provider          string
	AccountRef        string
	ExternalMessageID string
	AuthorClaim       string
	ContentSHA256     string
	ContentText       string
	RegisteredAt      time.Time
	Imports           []SourceImport
}

// Queries is EVD's read side.
type Queries interface {
	// GetSource returns a source the caller sees; the read is audited. NOT_FOUND hides sources of
	// other owners as well as missing ones.
	GetSource(ctx context.Context, sourceID string) (SourceView, error)
	// ResolveSources returns, among ids, the sources the caller may read. It needs the EVD read
	// permission (PERMISSION_DENIED otherwise; Python _source_ref). Ask before opening your own
	// transaction: EVD answers in a transaction of its own.
	ResolveSources(ctx context.Context, ids []string) (map[string]SourceRef, error)
	// Audit returns EVD's audit trail of the caller's scope.
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

// text reports whether s has 1 to max characters.
func text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s)
}
