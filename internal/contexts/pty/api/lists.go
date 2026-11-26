package api

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The list queries (S7-06 read models): a page of the parties or the contacts the caller sees,
// most recently changed first (kernel.ListCursor), audited under pty.read. A party or a contact
// is seen as party.get and contact.get decide it; the data scope is applied in the query.

// Statuses a PartyQuery or a ContactQuery may name.
var ListStatuses = []string{"draft", "confirmed", "inactive"}

// PartyQuery asks for a page of parties. Status and Role narrow; Search keeps the parties whose
// name or ID contains the text, whatever its case, which is also how a person looks for a party
// that may already exist before adding one. After is the Next of the previous page, empty for
// the first. Limit is 0 (kernel.DefaultListPage) to kernel.MaxListPage.
type PartyQuery struct {
	Status string
	Role   string
	Search string
	After  string
	Limit  int
}

// Validate checks the closed contract.
func (q PartyQuery) Validate() error {
	switch {
	case q.Status != "" && !slices.Contains(ListStatuses, q.Status):
		return schema("/status")
	case q.Role != "" && q.Role != RoleCustomer && q.Role != RoleSupplier:
		return schema("/role")
	case q.Search != "" && !kernel.ValidSearchText(q.Search):
		return schema("/q")
	}
	if _, field := kernel.ListPosition(q.After, q.Limit); field != "" {
		return schema("/" + field)
	}
	return nil
}

// PartySummary is one party of a list.
type PartySummary struct {
	PartyID      string
	Name         string
	Status       string
	Roles        []string
	Revision     int64
	RowVersion   int64
	OwnerActorID string
	UpdatedAt    time.Time
}

// PartyPage is a page of a PartyQuery. Next is the After of the following page, empty on the last.
type PartyPage struct {
	Parties []PartySummary
	Next    string
}

// ContactQuery asks for a page of contacts. PartyID narrows to one party's contacts; Status and
// Search are PartyQuery's.
type ContactQuery struct {
	PartyID string
	Status  string
	Search  string
	After   string
	Limit   int
}

// Validate checks the closed contract.
func (q ContactQuery) Validate() error {
	switch {
	case q.PartyID != "" && !kernel.ValidIdentifier(q.PartyID):
		return schema("/party_id")
	case q.Status != "" && !slices.Contains(ListStatuses, q.Status):
		return schema("/status")
	case q.Search != "" && !kernel.ValidSearchText(q.Search):
		return schema("/q")
	}
	if _, field := kernel.ListPosition(q.After, q.Limit); field != "" {
		return schema("/" + field)
	}
	return nil
}

// ContactSummary is one contact of a list.
type ContactSummary struct {
	ContactID    string
	PartyID      string
	Name         string
	Status       string
	Revision     int64
	RowVersion   int64
	OwnerActorID string
	UpdatedAt    time.Time
}

// ContactPage is a page of a ContactQuery. Next is the After of the following page, empty on the last.
type ContactPage struct {
	Contacts []ContactSummary
	Next     string
}
