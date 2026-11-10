// Package lifecycle is the life of a PTY master record: a draft is saved whole, a human confirms
// it, a confirmed record is revised back into a draft or deactivated, and every change compares
// and increments the row version (PTY-INV-001). The checks run in the order of the Python
// contracts: existence and visibility, version, state.
package lifecycle

import "go.idp.xyz/idp-brant/internal/kernel"

// Status of a master record.
type Status string

const (
	Draft     Status = "draft"
	Confirmed Status = "confirmed"
	Inactive  Status = "inactive"
)

// Record is the lifecycle part of a master record. RowVersion 0 means not stored yet; Revision
// counts confirmations.
type Record struct {
	ID         string
	Status     Status
	RowVersion int64
	Revision   int64
	OwnerActor string
	OwnerTeam  string
}

// New returns a draft that the actor will own.
func New(id, ownerActor, ownerTeam string) Record {
	return Record{ID: id, Status: Draft, OwnerActor: ownerActor, OwnerTeam: ownerTeam}
}

// CheckSave admits a whole-draft save: a missing record only at expected version 0, an existing
// one only as a visible draft at its current version.
func CheckSave(r *Record, visible bool, expected int64) error {
	if r == nil {
		if expected != 0 {
			return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
		}
		return nil
	}
	return Check(r, visible, expected, Draft)
}

// Check admits a command on an existing, visible record at its current version in one of the
// allowed states.
func Check(r *Record, visible bool, expected int64, allowed ...Status) error {
	switch {
	case r == nil || !visible:
		return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case r.RowVersion != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	for _, s := range allowed {
		if r.Status == s {
			return nil
		}
	}
	return kernel.Reject(kernel.CodeInvalidState, "/args/id", "")
}

// Move puts the record into status and increments its row version; a confirmation also starts a
// new revision.
func (r *Record) Move(status Status) {
	r.Status = status
	r.RowVersion++
	if status == Confirmed {
		r.Revision++
	}
}
