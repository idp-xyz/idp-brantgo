// Package supplierrfq is the SupplierRfq aggregate (tactical model SUP-AGG-SupplierRfq, T02 §13.1,
// SM-SUP-SRFQ): the record that the company asked a supplier for prices on lines of a ready
// customer RFQ.
//
//	draft  → issued  supplier_rfq.record_issued: a human records the sending, on evidence; the
//	                 content is sealed and the row version after the issue is its issued version
//	issued → closed  supplier_rfq.close: a human closes it; the issued content stays
//
// Issued only records a human fact: SUP sends nothing. A draft is saved whole; an issued request is
// never edited, and a correction is a new supplier RFQ that refers to the old one. Offers that
// answer the request name it at its issued version.
package supplierrfq

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/domain/revision"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Status of a supplier RFQ.
type Status string

const (
	Draft  Status = "draft"
	Issued Status = "issued"
	Closed Status = "closed"
)

// SupplierRfq is the aggregate root. Data is its canonical content and Hash its SHA-256.
type SupplierRfq struct {
	ID            string
	Status        Status
	RowVersion    int64
	OwnerActor    string
	OwnerTeam     string
	Data          []byte
	Hash          string
	SavedBy       kernel.ActorRef
	SavedEpoch    int64
	SavedAt       time.Time
	IssuedVersion int64 // 0 before the issue
	Issue         *revision.Act
	Closing       *revision.Act
}

// New returns an unsaved draft the actor will own.
func New(id, ownerActor, ownerTeam string) *SupplierRfq {
	return &SupplierRfq{ID: id, Status: Draft, OwnerActor: ownerActor, OwnerTeam: ownerTeam}
}

// CheckSave admits a whole-draft save: a missing request only at expected version 0, an existing
// one only as a visible draft at its current version.
func CheckSave(r *SupplierRfq, visible bool, expected int64) error {
	if r == nil {
		if expected != 0 {
			return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
		}
		return nil
	}
	return Check(r, visible, expected, Draft)
}

// Check admits a command on an existing, visible request at its current version in an allowed
// state.
func Check(r *SupplierRfq, visible bool, expected int64, allowed ...Status) error {
	switch {
	case r == nil || !visible:
		return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case r.RowVersion != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case !slices.Contains(allowed, r.Status):
		return kernel.Reject(kernel.CodeInvalidState, "/args/id", "")
	}
	return nil
}

// Replace replaces the whole draft content, edited by by.
func (r *SupplierRfq) Replace(data []byte, hash string, by kernel.ActorRef, epoch int64, at time.Time) {
	r.Data, r.Hash, r.SavedBy, r.SavedEpoch, r.SavedAt = data, hash, by, epoch, at
	r.RowVersion++
}

// RecordIssued seals the draft as sent by the human by, on evidence; the issued version is the row
// version after the issue.
func (r *SupplierRfq) RecordIssued(by kernel.ActorRef, epoch int64, reason string, evidence []revision.Evidence, at time.Time) {
	r.Status = Issued
	r.RowVersion++
	r.IssuedVersion = r.RowVersion
	r.Issue = &revision.Act{By: by, Epoch: epoch, At: at, Reason: reason, Evidence: slices.Clone(evidence)}
}

// Close closes an issued request; its issued content and version stay.
func (r *SupplierRfq) Close(by kernel.ActorRef, epoch int64, reason string, evidence []revision.Evidence, at time.Time) {
	r.Status = Closed
	r.RowVersion++
	r.Closing = &revision.Act{By: by, Epoch: epoch, At: at, Reason: reason, Evidence: slices.Clone(evidence)}
}

// Answerable reports whether an offer may answer the request at version: it was issued at that
// version and is issued or closed (closing does not undo the sending).
func (r *SupplierRfq) Answerable(version int64) bool {
	return r != nil && r.IssuedVersion == version && (r.Status == Issued || r.Status == Closed)
}
