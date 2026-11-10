// Package party is the Party aggregate: a trading party with the roles it plays for the company.
// One party may be customer and supplier at once (Python kept one master.kind).
package party

import (
	"slices"

	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/lifecycle"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Role a party plays.
type Role string

const (
	Customer Role = "customer"
	Supplier Role = "supplier"
)

// Party is the aggregate root.
type Party struct {
	lifecycle.Record
	Name  string
	Roles []Role
}

// ReplaceDraft replaces the whole draft; an empty name is unknown.
func (p *Party) ReplaceDraft(name string, roles []Role) {
	p.Name, p.Roles = name, slices.Clone(roles)
	slices.Sort(p.Roles)
	p.Move(lifecycle.Draft)
}

// Has reports whether the party plays role.
func (p *Party) Has(role Role) bool { return slices.Contains(p.Roles, role) }

// ReadyToConfirm reports the first part a confirmation still lacks (Python _confirm_master).
func (p *Party) ReadyToConfirm() error {
	if p.Name == "" {
		return kernel.Reject(kernel.CodeDraftNotReady, "/args/name", "")
	}
	return nil
}
