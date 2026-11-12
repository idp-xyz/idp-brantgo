package httpapi

import (
	"net/http"

	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
)

// PUR is what the transport needs from the purchasing context.
type PUR interface {
	purapi.Commands
	purapi.Queries
	purapi.Reconciler
	purapi.Auditor
}

func purCommands(c PUR) *commandSet {
	// PUR's commands name their objects as Python's T04 A46 contract does (draft_id), so no member
	// is renamed to id.
	return &commandSet{bc: "pur", contract: purapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile),
		commands: map[string]commandFunc{
			purapi.CommandPrepareDraft: reflected("", "", receiptOf[purapi.Receipt](c.PrepareDraft)),
			purapi.CommandVoidDraft:    reflected("", "", receiptOf[purapi.Receipt](c.VoidDraft)),
			// Purchase commitments (S5-01); they name their object commitment_id.
			purapi.CommandRequestCommitment:          reflected("", "", receiptOf[purapi.Receipt](c.RequestCommitment)),
			purapi.CommandDecideCommitment:           reflected("", "", receiptOf[purapi.Receipt](c.DecideCommitment)),
			purapi.CommandRevokeCommitment:           reflected("", "", receiptOf[purapi.Receipt](c.RevokeCommitment)),
			purapi.CommandIssueCommitment:            reflected("", "", receiptOf[purapi.Receipt](c.IssueCommitment)),
			purapi.CommandRecordSupplierConfirmation: reflected("", "", receiptOf[purapi.Receipt](c.RecordSupplierConfirmation)),
			// Supplier deliveries (S5-15); they name their object delivery_id.
			purapi.CommandRecordSupplierDelivery: reflected("", "", receiptOf[purapi.Receipt](c.RecordSupplierDelivery)),
			purapi.CommandVerifySupplierDelivery: reflected("", "", receiptOf[purapi.Receipt](c.VerifySupplierDelivery)),
			// Commitment changes and cancellation (S5-19); a change names its object change_id.
			purapi.CommandPrepareCommitmentChange: reflected("", "", receiptOf[purapi.Receipt](c.PrepareCommitmentChange)),
			purapi.CommandDecideCommitmentChange:  reflected("", "", receiptOf[purapi.Receipt](c.DecideCommitmentChange)),
			purapi.CommandApplyCommitmentChange:   reflected("", "", receiptOf[purapi.Receipt](c.ApplyCommitmentChange)),
			purapi.CommandCancelCommitment:        reflected("", "", receiptOf[purapi.Receipt](c.CancelCommitment)),
			// Allocation reviews after an order change (S5-09b); a review names its object review_id.
			// allocation_review.open is the Inbox consumer's and is not routed.
			purapi.CommandSettleAllocationReview: reflected("", "", receiptOf[purapi.Receipt](c.SettleAllocationReview)),
		}}
}

// orderAllocationReviews answers GET /api/brant/v1/pur/orders/<id>/allocation_reviews: the allocation
// reviews of the order, by order version.
func (s *server) orderAllocationReviews(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.PUR.GetAllocationReviews(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v == nil {
		v = []purapi.AllocationReviewView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "order_id": r.PathValue("id"), "allocation_reviews": wire(v)})
}

// commitmentChanges answers GET /api/brant/v1/pur/purchase_commitments/<id>/changes: the changes of
// the commitment the caller sees, by change ID.
func (s *server) commitmentChanges(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.PUR.GetCommitmentChanges(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v == nil {
		v = []purapi.ChangeView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "commitment_id": r.PathValue("id"), "changes": wire(v)})
}

// orderCommitments answers GET /api/brant/v1/pur/orders/<id>/commitments: the purchase commitments
// of the order the caller sees, by commitment ID.
func (s *server) orderCommitments(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.PUR.GetOrderCommitments(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v == nil {
		v = []purapi.CommitmentView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "order_id": r.PathValue("id"), "commitments": wire(v)})
}

// allocations answers GET /api/brant/v1/pur/orders/<id>/allocations: the demand allocation of every
// drafted line of the order, ordered by line.
func (s *server) allocations(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.PUR.GetAllocations(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if v == nil {
		v = []purapi.AllocationView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "order_id": r.PathValue("id"), "allocations": wire(v)})
}
