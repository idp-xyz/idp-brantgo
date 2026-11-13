package httpapi

import (
	"context"
	"net/http"
	"net/url"
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
	trdapi.ActionReconciler
	trdapi.GoalQueries
	trdapi.Auditor
}

// receiptOf returns the closed receipt of type Rec a command answers: the receipt itself, or the one
// its extended receipt embeds. The extensions (hashes, numbers, document hashes, effectivity) are
// read through the context's read routes.
func receiptOf[Rec, C, R any](call func(context.Context, C) (R, error)) func(context.Context, C) (Rec, error) {
	return func(ctx context.Context, cmd C) (Rec, error) {
		r, err := call(ctx, cmd)
		v := reflect.ValueOf(r)
		if v.Type() == reflect.TypeFor[Rec]() {
			return v.Interface().(Rec), err
		}
		return v.FieldByName("Receipt").Interface().(Rec), err
	}
}

func trdCommands(c TRD) *commandSet {
	// TRD's commands name their objects as Python's T03/T04 contracts do (quote_id, response_id,
	// acceptance_id, order_id), so no member is renamed to id.
	return &commandSet{bc: "trd", contract: trdapi.Contract, auditor: c, audit: c.Audit, reconcile: hashReconciler(c.Reconcile, c.ReconcileHash), commands: map[string]commandFunc{
		trdapi.CommandSaveDraft:                 reflected("", "", receiptOf[trdapi.Receipt](c.SaveDraft)),
		trdapi.CommandRevise:                    reflected("", "", receiptOf[trdapi.Receipt](c.Revise)),
		trdapi.CommandDiscardDraft:              reflected("", "", receiptOf[trdapi.Receipt](c.DiscardDraft)),
		trdapi.CommandFreeze:                    reflected("", "", receiptOf[trdapi.Receipt](c.Freeze)),
		trdapi.CommandUpdateMetadata:            reflected("", "", receiptOf[trdapi.Receipt](c.UpdateMetadata)),
		trdapi.CommandBindControl:               reflected("", "", receiptOf[trdapi.Receipt](c.BindControl)),
		trdapi.CommandStopControl:               reflected("", "", receiptOf[trdapi.Receipt](c.StopControl)),
		trdapi.CommandEnterManualControl:        reflected("", "", receiptOf[trdapi.Receipt](c.EnterManualControl)),
		trdapi.CommandHandoffControl:            reflected("", "", receiptOf[trdapi.Receipt](c.HandoffControl)),
		trdapi.CommandReleaseControl:            reflected("", "", receiptOf[trdapi.Receipt](c.ReleaseControl)),
		trdapi.CommandSubmitReview:              reflected("", "", receiptOf[trdapi.Receipt](c.SubmitReview)),
		trdapi.CommandDecideReview:              reflected("", "", receiptOf[trdapi.Receipt](c.DecideReview)),
		trdapi.CommandRevokeApproval:            reflected("", "", receiptOf[trdapi.Receipt](c.RevokeApproval)),
		trdapi.CommandActivate:                  reflected("", "", receiptOf[trdapi.Receipt](c.Activate)),
		trdapi.CommandReplace:                   reflected("", "", receiptOf[trdapi.Receipt](c.Replace)),
		trdapi.CommandWithdraw:                  reflected("", "", receiptOf[trdapi.Receipt](c.Withdraw)),
		trdapi.CommandCancel:                    reflected("", "", receiptOf[trdapi.Receipt](c.Cancel)),
		trdapi.CommandRecordResponse:            reflected("", "", receiptOf[trdapi.Receipt](c.RecordResponse)),
		trdapi.CommandVerifyResponse:            reflected("", "", receiptOf[trdapi.Receipt](c.VerifyResponse)),
		trdapi.CommandInvalidateAcceptance:      reflected("", "", receiptOf[trdapi.Receipt](c.InvalidateAcceptance)),
		trdapi.CommandDisputeAcceptance:         reflected("", "", receiptOf[trdapi.Receipt](c.DisputeAcceptance)),
		trdapi.CommandPrepareOrder:              reflected("", "", receiptOf[trdapi.Receipt](c.PrepareOrder)),
		trdapi.CommandRequestOrderReview:        reflected("", "", receiptOf[trdapi.Receipt](c.RequestOrderReview)),
		trdapi.CommandDecideOrderReview:         reflected("", "", receiptOf[trdapi.Receipt](c.DecideOrderReview)),
		trdapi.CommandActivateOrder:             reflected("", "", receiptOf[trdapi.Receipt](c.ActivateOrder)),
		trdapi.CommandRevokeOrderApproval:       reflected("", "", receiptOf[trdapi.Receipt](c.RevokeOrderApproval)),
		trdapi.CommandCancelOrder:               reflected("", "", receiptOf[trdapi.Receipt](c.CancelOrder)),
		trdapi.CommandCloseOrderUnfulfilled:     reflected("", "", receiptOf[trdapi.Receipt](c.CloseOrderUnfulfilled)),
		trdapi.CommandPrepareAmendment:          reflected("", "", receiptOf[trdapi.Receipt](c.PrepareAmendment)),
		trdapi.CommandSubmitAmendment:           reflected("", "", receiptOf[trdapi.Receipt](c.SubmitAmendment)),
		trdapi.CommandDecideAmendment:           reflected("", "", receiptOf[trdapi.Receipt](c.DecideAmendment)),
		trdapi.CommandApplyAmendment:            reflected("", "", receiptOf[trdapi.Receipt](c.ApplyAmendment)),
		trdapi.CommandWithdrawAmendment:         reflected("", "", receiptOf[trdapi.Receipt](c.WithdrawAmendment)),
		trdapi.CommandCloseOrderRemainder:       reflected("", "", receiptOf[trdapi.Receipt](c.CloseOrderRemainder)),
		trdapi.CommandCloseOrder:                reflected("", "", receiptOf[trdapi.Receipt](c.CloseOrder)),
		trdapi.CommandGenerateOrderDocument:     reflected("", "", receiptOf[trdapi.Receipt](c.GenerateOrderDocument)),
		trdapi.CommandRecordSignedOrderDocument: reflected("", "", receiptOf[trdapi.Receipt](c.RecordSignedOrderDocument)),
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
	return queryValues(r.URL.Query(), names...)
}

// queryValues is queryInts on the parameters q.
func queryValues(q url.Values, names ...string) (map[string]int64, error) {
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

// goalReadiness answers GET /api/brant/v1/trd/quotes/<id>/goal_readiness?goal=quote_ready|order_active
// [&goal_version=1][&business_revision=N][&customer_id=][&rfq_id=] (GoalReadiness v1, Context Map
// R18; baseline 15.4): every assertion the goal requires with its answer, checks and evidence, and
// the outcome. goal_version defaults to 1. The order_active form is the HTTP read of the root's one
// full_v1 order (its ID, revision and hash) and whether it is active. An unsatisfied assertion is
// the answer, not an error.
func (s *server) goalReadiness(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	text := []string{"goal", "customer_id", "rfq_id"}
	ints := url.Values{}
	for k, v := range q {
		if len(v) != 1 {
			s.reject(w, r, kernel.Reject(kernel.CodeSchemaInvalid, "/"+k, "repeated query parameter"))
			return
		}
		if !slices.Contains(text, k) {
			ints[k] = v
		}
	}
	nums, err := queryValues(ints, "goal_version", "business_revision")
	if err == nil {
		gq := trdapi.GoalQuery{Goal: q.Get("goal"), GoalVersion: trdapi.GoalVersion, QuoteID: r.PathValue("id"),
			BusinessRevision: nums["business_revision"], CustomerID: q.Get("customer_id"), RfqID: q.Get("rfq_id")}
		if q.Has("goal_version") {
			gq.GoalVersion = nums["goal_version"]
		}
		var v trdapi.GoalAssessment
		if v, err = s.cfg.TRD.GoalReadiness(r.Context(), gq); err == nil {
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

// orderCheck answers GET /api/brant/v1/trd/orders/<id>/check[?review_id=]: the checks
// order.activate would make now on that review (none: the source and upstream risks only); a
// refusal is the answer, not an error.
func (s *server) orderCheck(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for k := range q {
		if k != "review_id" || len(q[k]) != 1 {
			s.reject(w, r, kernel.Reject(kernel.CodeSchemaInvalid, "/"+k, "unknown or repeated query parameter"))
			return
		}
	}
	v, err := s.cfg.TRD.CheckOrder(r.Context(), r.PathValue("id"), q.Get("review_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, okView(v))
}

// orderDocument answers GET /api/brant/v1/trd/orders/<id>/revisions/<revision>/documents/<kind>:
// the S/C or PI of that order revision with its binding and bytes (Base64), verified on every read.
func (s *server) orderDocument(w http.ResponseWriter, r *http.Request) {
	rev, err := revisionParam(r)
	if err == nil {
		var v trdapi.OrderDocument
		if v, err = s.cfg.TRD.GetOrderDocument(r.Context(), r.PathValue("id"), rev, r.PathValue("kind")); err == nil {
			writeJSON(w, http.StatusOK, okView(v))
			return
		}
	}
	s.fail(w, r, err)
}
