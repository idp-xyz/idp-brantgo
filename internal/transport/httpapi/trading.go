package httpapi

import (
	"context"
	"net/http"
	"reflect"
	"slices"
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
	rev, err := revisionParam(r)
	if err == nil {
		var v trdapi.FormalDocument
		if v, err = s.cfg.TRD.GetFormalDocument(r.Context(), r.PathValue("id"), rev); err == nil {
			writeJSON(w, http.StatusOK, okView(v))
			return
		}
	}
	s.fail(w, r, err)
}

// revisionParam is the positive business revision in the path.
func revisionParam(r *http.Request) (int64, error) {
	rev, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
	if err != nil || rev < 1 {
		return 0, kernel.Reject(kernel.CodeSchemaInvalid, "/revision", "positive integer")
	}
	return rev, nil
}

// queryInts reads the query parameters names as non-negative integers (0 when absent); any other
// parameter, a repeated one or a malformed value is SCHEMA_INVALID.
func queryInts(r *http.Request, names ...string) (map[string]int64, error) {
	q := r.URL.Query()
	for k := range q {
		if !slices.Contains(names, k) {
			return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/"+k, "unknown query parameter")
		}
	}
	out := make(map[string]int64, len(names))
	for _, n := range names {
		if !q.Has(n) {
			continue
		}
		v, err := strconv.ParseInt(q.Get(n), 10, 64)
		if err != nil || v < 0 || len(q[n]) != 1 {
			return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/"+n, "non-negative integer")
		}
		out[n] = v
	}
	return out, nil
}

// quoteReady answers GET /api/brant/v1/trd/quotes/<id>/ready[?business_revision=N]: whether the
// revision (the active one by default) serves the next step now; a failed check is a reason in the
// answer, not an error.
func (s *server) quoteReady(w http.ResponseWriter, r *http.Request) {
	q, err := queryInts(r, "business_revision")
	if err == nil {
		var v trdapi.QuoteReadiness
		if v, err = s.cfg.TRD.QuoteReady(r.Context(), trdapi.ReadyQuery{QuoteID: r.PathValue("id"), BusinessRevision: q["business_revision"]}); err == nil {
			writeJSON(w, http.StatusOK, okView(v))
			return
		}
	}
	s.fail(w, r, err)
}

// quoteHistory answers a paged history of a quote: ?after= (exclusive, 0 from the start) and
// ?limit= (0 for the default page).
func quoteHistory[P any](s *server, get func(context.Context, trdapi.HistoryQuery) (P, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, err := queryInts(r, "after", "limit")
		if err == nil && q["limit"] > trdapi.MaxPage {
			err = kernel.Reject(kernel.CodeSchemaInvalid, "/limit", "at most "+strconv.Itoa(trdapi.MaxPage))
		}
		if err == nil {
			var v P
			if v, err = get(r.Context(), trdapi.HistoryQuery{QuoteID: r.PathValue("id"), Page: trdapi.Page{After: q["after"], Limit: int(q["limit"])}}); err == nil {
				writeJSON(w, http.StatusOK, okView(v))
				return
			}
		}
		s.fail(w, r, err)
	}
}

// dispatchRead answers a dispatch read of one revision of a quote.
func dispatchRead[V any](s *server, get func(context.Context, trdapi.DispatchQuery) (V, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rev, err := revisionParam(r)
		if err == nil {
			var v V
			if v, err = get(r.Context(), trdapi.DispatchQuery{QuoteID: r.PathValue("id"), BusinessRevision: rev}); err == nil {
				writeJSON(w, http.StatusOK, okView(v))
				return
			}
		}
		s.fail(w, r, err)
	}
}
