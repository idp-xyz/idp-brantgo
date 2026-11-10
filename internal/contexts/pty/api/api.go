// Package api is the contract of the PTY bounded context (trading parties and their contacts) for
// transports, the AI layer and the contexts downstream of PTY. It exposes no PTY domain type.
package api

import (
	"context"
	"slices"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// Contract names the request and response shapes of PTY commands.
	Contract = "brant.pty/0.1.0"
	// Source identifies PTY as the producer of integration events.
	Source = "idp-brant.pty"

	EventPartyConfirmed   = "idp-brant.pty.party_confirmed.v1"
	EventPartyRevised     = "idp-brant.pty.party_revised.v1"
	EventPartyDeactivated = "idp-brant.pty.party_deactivated.v1"
	EventContactConfirmed = "idp-brant.pty.contact_confirmed.v1"

	CommandSaveParty       = "party.save_draft"
	CommandConfirmParty    = "party.confirm"
	CommandReviseParty     = "party.revise"
	CommandDeactivateParty = "party.deactivate"
	CommandSaveContact     = "contact.save_draft"
	CommandConfirmContact  = "contact.confirm"

	RoleCustomer = "customer"
	RoleSupplier = "supplier"

	StatusDraft     = "draft"
	StatusConfirmed = "confirmed"
	StatusInactive  = "inactive"

	// MaxEvidence bounds the sources a confirmation cites.
	MaxEvidence = 200
)

// SaveParty creates a draft party at ExpectedVersion 0 or replaces the whole draft at its current
// version. An omitted name clears it. Roles say what the party is to the company; one party may be
// both customer and supplier (Python master.kind=customer/supplier).
type SaveParty struct {
	IdempotencyKey  string
	PartyID         string
	ExpectedVersion int64
	Name            string
	Roles           []string
}

// ConfirmParty verifies a draft party against sources the confirming human may read. Only a human
// confirms (PTY-INV-001); the party then counts as current for downstream references.
type ConfirmParty struct {
	IdempotencyKey  string
	PartyID         string
	ExpectedVersion int64
	EvidenceIDs     []string
}

// ReviseParty reopens a confirmed party as a draft for a recorded reason. The confirmed revision
// stays as it was; until the next confirmation the party is not current.
type ReviseParty struct {
	IdempotencyKey  string
	PartyID         string
	ExpectedVersion int64
	Reason          string
}

// DeactivateParty stops a confirmed party for new business; its history stays readable.
type DeactivateParty struct {
	IdempotencyKey  string
	PartyID         string
	ExpectedVersion int64
	Reason          string
}

// SaveContact creates or replaces a draft contact of a customer party.
type SaveContact struct {
	IdempotencyKey  string
	ContactID       string
	ExpectedVersion int64
	Name            string
	PartyID         string // the customer the contact belongs to
}

// ConfirmContact verifies a draft contact of a confirmed customer against readable sources.
type ConfirmContact struct {
	IdempotencyKey  string
	ContactID       string
	ExpectedVersion int64
	EvidenceIDs     []string
}

// Validate checks the closed contract.
func (c SaveParty) Validate() error {
	if err := head(c.IdempotencyKey, c.PartyID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	if c.Name != "" && !Text(c.Name, 200) {
		return schema("/args/name")
	}
	if len(c.Roles) < 1 || len(c.Roles) > 2 {
		return schema("/args/roles")
	}
	for i, r := range c.Roles {
		if (r != RoleCustomer && r != RoleSupplier) || slices.Index(c.Roles, r) != i {
			return schema("/args/roles")
		}
	}
	return nil
}

func (c ConfirmParty) Validate() error {
	if err := head(c.IdempotencyKey, c.PartyID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	return evidence(c.EvidenceIDs)
}

func (c ReviseParty) Validate() error {
	if err := head(c.IdempotencyKey, c.PartyID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if !Text(c.Reason, 1000) {
		return schema("/args/reason")
	}
	return nil
}

func (c DeactivateParty) Validate() error {
	if err := head(c.IdempotencyKey, c.PartyID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if !Text(c.Reason, 1000) {
		return schema("/args/reason")
	}
	return nil
}

func (c SaveContact) Validate() error {
	if err := head(c.IdempotencyKey, c.ContactID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	if c.Name != "" && !Text(c.Name, 200) {
		return schema("/args/name")
	}
	if c.PartyID != "" && !kernel.ValidIdentifier(c.PartyID) {
		return schema("/args/party_id")
	}
	return nil
}

func (c ConfirmContact) Validate() error {
	if err := head(c.IdempotencyKey, c.ContactID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	return evidence(c.EvidenceIDs)
}

// Auditor records a request an inbound adapter refused before it became a command, such as a body
// that is not valid JSON, in the context's audit trail. The principal in ctx is the requester.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Receipt is the closed success answer of a command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Commands is PTY's command entry point. Errors are *kernel.Rejection for refusals.
type Commands interface {
	SaveParty(ctx context.Context, cmd SaveParty) (Receipt, error)
	ConfirmParty(ctx context.Context, cmd ConfirmParty) (Receipt, error)
	ReviseParty(ctx context.Context, cmd ReviseParty) (Receipt, error)
	DeactivateParty(ctx context.Context, cmd DeactivateParty) (Receipt, error)
	SaveContact(ctx context.Context, cmd SaveContact) (Receipt, error)
	ConfirmContact(ctx context.Context, cmd ConfirmContact) (Receipt, error)
}

// PartyRef is PartyRef v1: a party at a row version, with the status and roles it had then.
type PartyRef struct {
	PartyID    string
	RowVersion int64
	Status     string
	Roles      []string
}

// ContactRef is a contact at a row version and the party it belongs to.
type ContactRef struct {
	ContactID  string
	PartyID    string
	RowVersion int64
	Status     string
}

// Resolve asks for the current state of parties and contacts.
type Resolve struct {
	PartyIDs   []string
	ContactIDs []string
}

// Resolved holds the parties and contacts that exist and that the caller sees; others are absent.
type Resolved struct {
	Parties  map[string]PartyRef
	Contacts map[string]ContactRef
}

// PartyRevision is a confirmed revision of a party, kept unchanged after the party is revised.
type PartyRevision struct {
	Revision    int64
	Name        string
	Roles       []string
	EvidenceIDs []string
	ConfirmedBy kernel.ActorRef
}

// PartyView is a party and its confirmed revisions.
type PartyView struct {
	PartyID    string
	Status     string
	RowVersion int64
	Name       string
	Roles      []string
	Revisions  []PartyRevision
}

// ContactView is a contact.
type ContactView struct {
	ContactID   string
	PartyID     string
	Status      string
	RowVersion  int64
	Name        string
	EvidenceIDs []string // of the last confirmation
}

// Queries is PTY's read side.
type Queries interface {
	GetParty(ctx context.Context, partyID string) (PartyView, error)
	GetContact(ctx context.Context, contactID string) (ContactView, error)
	// ResolveParties answers reference lookups of downstream contexts. It needs no permission
	// beyond an active grant; records outside the caller's data scope are absent (Python
	// _master_ref). Ask before opening your own transaction.
	ResolveParties(ctx context.Context, q Resolve) (Resolved, error)
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

func head(key, id string, expected, minVersion int64) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(id):
		return schema("/args/id")
	case expected < minVersion || expected > 2147483647:
		return schema("/args/expected_version")
	}
	return nil
}

func evidence(ids []string) error {
	if len(ids) < 1 || len(ids) > MaxEvidence {
		return schema("/args/evidence_ids")
	}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) || slices.Index(ids, id) != i {
			return schema("/args/evidence_ids")
		}
	}
	return nil
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

// Text reports whether s is valid UTF-8 of 1 to max characters.
func Text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s)
}
