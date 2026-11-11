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
		}}
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
