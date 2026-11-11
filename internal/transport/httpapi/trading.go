package httpapi

import (
	"context"
	"net/http"
	"reflect"
	"strconv"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TRD is what the transport needs from the trading context.
type TRD interface {
	trdapi.Commands
	trdapi.Queries
	trdapi.Reconciler
	trdapi.Auditor
}

// receiptOf returns the closed receipt a TRD command answers: the receipt itself, or the one its
// extended receipt embeds. The extensions (business hash, number, document hash, effectivity) are
// read through the quote, formal document, response and acceptance routes.
func receiptOf[C, R any](call func(context.Context, C) (R, error)) func(context.Context, C) (trdapi.Receipt, error) {
	return func(ctx context.Context, cmd C) (trdapi.Receipt, error) {
		r, err := call(ctx, cmd)
		v := reflect.ValueOf(r)
		if v.Type() == reflect.TypeFor[trdapi.Receipt]() {
			return v.Interface().(trdapi.Receipt), err
		}
		return v.FieldByName("Receipt").Interface().(trdapi.Receipt), err
	}
}

func trdCommands(c TRD) *commandSet {
	// TRD's commands name their objects as Python's T03/T04 contracts do (quote_id, response_id,
	// acceptance_id), so no member is renamed to id.
	return &commandSet{bc: "trd", contract: trdapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile), commands: map[string]commandFunc{
		trdapi.CommandSaveDraft:            reflected("", "", receiptOf(c.SaveDraft)),
		trdapi.CommandRevise:               reflected("", "", receiptOf(c.Revise)),
		trdapi.CommandDiscardDraft:         reflected("", "", receiptOf(c.DiscardDraft)),
		trdapi.CommandFreeze:               reflected("", "", receiptOf(c.Freeze)),
		trdapi.CommandUpdateMetadata:       reflected("", "", receiptOf(c.UpdateMetadata)),
		trdapi.CommandBindControl:          reflected("", "", receiptOf(c.BindControl)),
		trdapi.CommandStopControl:          reflected("", "", receiptOf(c.StopControl)),
		trdapi.CommandReleaseControl:       reflected("", "", receiptOf(c.ReleaseControl)),
		trdapi.CommandSubmitReview:         reflected("", "", receiptOf(c.SubmitReview)),
		trdapi.CommandDecideReview:         reflected("", "", receiptOf(c.DecideReview)),
		trdapi.CommandRevokeApproval:       reflected("", "", receiptOf(c.RevokeApproval)),
		trdapi.CommandActivate:             reflected("", "", receiptOf(c.Activate)),
		trdapi.CommandReplace:              reflected("", "", receiptOf(c.Replace)),
		trdapi.CommandWithdraw:             reflected("", "", receiptOf(c.Withdraw)),
		trdapi.CommandCancel:               reflected("", "", receiptOf(c.Cancel)),
		trdapi.CommandRecordResponse:       reflected("", "", receiptOf(c.RecordResponse)),
		trdapi.CommandVerifyResponse:       reflected("", "", receiptOf(c.VerifyResponse)),
		trdapi.CommandInvalidateAcceptance: reflected("", "", receiptOf(c.InvalidateAcceptance)),
		trdapi.CommandDisputeAcceptance:    reflected("", "", receiptOf(c.DisputeAcceptance)),
	}}
}

// approvalCheck answers GET /api/brant/v1/trd/quotes/<id>/approvals/<approval_id>/check.
func (s *server) approvalCheck(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.TRD.CheckApproval(r.Context(), r.PathValue("id"), r.PathValue("approval_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, okView(v))
}

// formalDocument answers GET /api/brant/v1/trd/quotes/<id>/revisions/<revision>/document: the
// binding and the document bytes (Base64), verified against their hashes on every read.
func (s *server) formalDocument(w http.ResponseWriter, r *http.Request) {
	rev, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
	if err != nil || rev < 1 {
		s.reject(w, r, kernel.Reject(kernel.CodeSchemaInvalid, "/revision", "positive integer"))
		return
	}
	v, err := s.cfg.TRD.GetFormalDocument(r.Context(), r.PathValue("id"), rev)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, okView(v))
}
