// Package contact is the Contact aggregate: a person at a customer party. A contact is confirmed
// only while its party is a confirmed customer the confirming human sees.
package contact

import (
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/lifecycle"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Contact is the aggregate root.
type Contact struct {
	lifecycle.Record
	Name    string
	PartyID string
}

// ReplaceDraft replaces the whole draft; empty fields are unknown.
func (c *Contact) ReplaceDraft(name, partyID string) {
	c.Name, c.PartyID = name, partyID
	c.Move(lifecycle.Draft)
}

// ReadyToConfirm reports the first part a confirmation still lacks.
func (c *Contact) ReadyToConfirm() error {
	switch {
	case c.Name == "":
		return kernel.Reject(kernel.CodeDraftNotReady, "/args/name", "")
	case c.PartyID == "":
		return kernel.Reject(kernel.CodeDraftNotReady, "/args/party_id", "")
	}
	return nil
}
