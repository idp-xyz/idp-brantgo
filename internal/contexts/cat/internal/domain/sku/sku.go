// Package sku is the Sku aggregate: a tradeable specification. A draft is saved whole; a human
// confirms it and so fixes a new specification revision; revising reopens the draft and leaves
// every confirmed revision as it was (CAT-INV-001: SKU identity and specification revision are
// referenced separately). Checks run in the Python order: existence and visibility, version, state.
package sku

import "go.idp.xyz/idp-brant/internal/kernel"

// Status of a SKU.
type Status string

const (
	Draft     Status = "draft"
	Confirmed Status = "confirmed"
	Inactive  Status = "inactive"
)

// UnitPCS is the only base unit in S1.
const UnitPCS = "PCS"

// Sku is the aggregate root. Revision is the current specification revision, the number of
// confirmations so far; RowVersion 0 means not stored yet.
type Sku struct {
	ID         string
	Status     Status
	RowVersion int64
	Revision   int64
	Name       string
	BaseUnit   string
	OwnerActor string
	OwnerTeam  string
}

// New returns an unsaved draft the actor will own.
func New(id, ownerActor, ownerTeam string) *Sku {
	return &Sku{ID: id, Status: Draft, OwnerActor: ownerActor, OwnerTeam: ownerTeam}
}

// CheckSave admits a whole-draft save.
func CheckSave(s *Sku, visible bool, expected int64) error {
	if s == nil {
		if expected != 0 {
			return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
		}
		return nil
	}
	return Check(s, visible, expected, Draft)
}

// Check admits a command on an existing, visible SKU at its current version in an allowed state.
func Check(s *Sku, visible bool, expected int64, allowed ...Status) error {
	switch {
	case s == nil || !visible:
		return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case s.RowVersion != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	for _, a := range allowed {
		if s.Status == a {
			return nil
		}
	}
	return kernel.Reject(kernel.CodeInvalidState, "/args/id", "")
}

// ReplaceDraft replaces the whole draft; empty fields are unknown.
func (s *Sku) ReplaceDraft(name, baseUnit string) {
	s.Name, s.BaseUnit = name, baseUnit
	s.move(Draft)
}

// Confirm fixes a new specification revision; it refuses a draft without a name or the PCS base
// unit (Python _confirm_master for kind sku).
func (s *Sku) Confirm() error {
	switch {
	case s.Name == "":
		return kernel.Reject(kernel.CodeDraftNotReady, "/args/name", "")
	case s.BaseUnit != UnitPCS:
		return kernel.Reject(kernel.CodeDraftNotReady, "/args/base_uom", "")
	}
	s.Revision++
	s.move(Confirmed)
	return nil
}

// Reopen turns a confirmed SKU back into a draft.
func (s *Sku) Reopen() { s.move(Draft) }

// Deactivate stops the SKU for new business.
func (s *Sku) Deactivate() { s.move(Inactive) }

func (s *Sku) move(status Status) {
	s.Status = status
	s.RowVersion++
}
