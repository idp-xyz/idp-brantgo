package httpapi

import (
	"net/http"

	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
)

// QLT is what the transport needs from the quality context.
type QLT interface {
	qltapi.Commands
	qltapi.Queries
	qltapi.Reconciler
	qltapi.Auditor
}

func qltCommands(c QLT) *commandSet {
	// QLT's commands name their objects by kind (inspection_id, release_id), as PUR's do, so no
	// member is renamed to id.
	return &commandSet{bc: "qlt", contract: qltapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile),
		commands: map[string]commandFunc{
			qltapi.CommandRecordInspection:  reflected("", "", receiptOf[qltapi.Receipt](c.RecordInspection)),
			qltapi.CommandDecideDisposition: reflected("", "", receiptOf[qltapi.Receipt](c.DecideDisposition)),
			qltapi.CommandGrantRelease:      reflected("", "", receiptOf[qltapi.Receipt](c.GrantRelease)),
			qltapi.CommandRevokeRelease:     reflected("", "", receiptOf[qltapi.Receipt](c.RevokeRelease)),
		}}
}

// qualitySubject answers GET /api/brant/v1/qlt/subjects/<kind>/<id>/lines/<line_id>: the subject's
// quality standing now — what its valid inspections cover and find conforming, what was released
// and what may still be released, with its inspections and releases.
func (s *server) qualitySubject(w http.ResponseWriter, r *http.Request) {
	ref := qltapi.SubjectRef{Kind: r.PathValue("kind"), ID: r.PathValue("id"), LineID: r.PathValue("line_id")}
	v, err := s.cfg.QLT.GetSubject(r.Context(), ref)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, okView(v))
}
