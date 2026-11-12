// Package httpapi is Brant's HTTP transport: one command route per context
// (POST /api/brant/v1/<bc>/commands/<command>), audited reads, and the ingress limits of the
// engineering contracts. It authenticates every /api request from a bearer token, so actor, tenant
// and company never come from a request body; it maps a rejection's category, never its cause, to
// the status code (contract C5). It reaches contexts only through their api packages.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// Verifier turns a bearer token into the verified principal of the request.
type Verifier interface {
	Verify(ctx context.Context, token string) (identity.Principal, error)
}

// EVD, PTY, CAT and INQ are what the transport needs from each context; REF what it needs from
// reference data.
type (
	EVD interface {
		evdapi.Commands
		evdapi.Queries
		evdapi.Auditor
	}
	PTY interface {
		ptyapi.Commands
		ptyapi.Queries
		ptyapi.ScreeningCommands
		ptyapi.ScreeningQueries
		ptyapi.ProfileCommands
		GetTradingProfile(ctx context.Context, partyID string) (ptyapi.TradingProfileView, error)
		ptyapi.Auditor
	}
	CAT interface {
		catapi.Commands
		catapi.Queries
		catapi.DeclarationCommands
		catapi.DeclarationQueries
		catapi.Auditor
	}
	INQ interface {
		inqapi.Commands
		inqapi.Queries
		inqapi.Reconciler
		inqapi.Auditor
	}
	REF interface {
		refapi.Commands
		refapi.Auditor
		REFProgrammes
		GetList(ctx context.Context, listCode string) (refapi.ListView, error)
		GetListVersion(ctx context.Context, listCode, version string) (refapi.ListVersionView, error)
		Audit(ctx context.Context) ([]kernel.AuditEntry, error)
	}
)

// DeadLetterReplayer republishes a dead letter by event ID in the scope of the Principal in ctx.
type DeadLetterReplayer interface {
	Replay(ctx context.Context, source, eventID string) (messaging.ReplayResult, error)
}

// Config wires the transport.
type Config struct {
	EVD EVD
	PTY PTY
	CAT CAT
	INQ INQ
	POL POL
	REF REF
	SUP SUP
	CST CST
	TRD TRD
	PUR PUR
	// The S5 contexts are optional: nil leaves a context's commands and routes out, so a transport
	// that serves or tests other contexts needs no stand-in for them. The composed process sets all.
	QLT      QLT
	STL      STL
	CNX      CNX
	FUL      FUL
	Verifier Verifier
	// Replayer replays Outbox dead letters for an authorized human (ADR-BR-015); nil leaves the
	// route out.
	Replayer DeadLetterReplayer
	Clock    application.Clock
	Logger   *slog.Logger
	// MaxBodyBytes bounds a request body (default 1 MiB, the contracts' limit).
	MaxBodyBytes int64
}

// MaxBody is the default request body limit; MaxFileBody the limit of file.register.
const (
	MaxBody     = 1 << 20
	MaxFileBody = 15 << 20
)

// New returns the transport's handler.
func New(cfg Config) (http.Handler, error) {
	if cfg.EVD == nil || cfg.PTY == nil || cfg.CAT == nil || cfg.INQ == nil || cfg.POL == nil || cfg.REF == nil || cfg.SUP == nil || cfg.CST == nil || cfg.TRD == nil || cfg.PUR == nil || cfg.Verifier == nil ||
		cfg.Clock == nil {
		return nil, errors.New("httpapi: contexts, verifier and clock are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = MaxBody
	}
	s := &server{cfg: cfg, contexts: map[string]*commandSet{}}
	for _, set := range []*commandSet{evdCommands(cfg.EVD), ptyCommands(cfg.PTY), catCommands(cfg.CAT), inqCommands(cfg.INQ), polCommands(cfg.POL),
		refCommands(cfg.REF), supCommands(cfg.SUP), cstCommands(cfg.CST), trdCommands(cfg.TRD), purCommands(cfg.PUR)} {
		s.contexts[set.bc] = set
	}
	if cfg.QLT != nil {
		s.contexts["qlt"] = qltCommands(cfg.QLT)
	}
	if cfg.STL != nil {
		s.contexts["stl"] = stlCommands(cfg.STL)
	}
	if cfg.CNX != nil {
		s.contexts["cnx"] = cnxCommands(cfg.CNX)
	}
	if cfg.FUL != nil {
		s.contexts["ful"] = fulCommands(cfg.FUL)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.reject(w, r, kernel.Reject(kernel.CodeNotFound, "/", "no such route"))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.Handle("POST /api/brant/v1/{bc}/commands/{command}", s.authenticated(s.command))
	mux.Handle("POST /api/brant/v1/{bc}/commands/{command}/reconcile", s.authenticated(s.reconcile))
	mux.Handle("GET /api/brant/v1/{bc}/audit", s.authenticated(s.audit))
	if cfg.Replayer != nil {
		mux.Handle("POST /api/brant/v1/msg/dead_letters/{source}/{event_id}/replay", s.authenticated(s.replay))
	}
	mux.Handle("GET /api/brant/v1/evd/sources/{id}", s.authenticated(read(s, cfg.EVD.GetSource, sourceView)))
	mux.Handle("GET /api/brant/v1/evd/files/{id}", s.authenticated(read(s, cfg.EVD.GetFile, fileView)))
	mux.Handle("GET /api/brant/v1/pty/parties/{id}", s.authenticated(read(s, cfg.PTY.GetParty, partyView)))
	mux.Handle("GET /api/brant/v1/pty/parties/{id}/screening", s.authenticated(read(s, cfg.PTY.GetScreening, okView[ptyapi.ScreeningView])))
	mux.Handle("GET /api/brant/v1/pty/screenings/due", s.authenticated(s.screeningsDue))
	mux.Handle("GET /api/brant/v1/pty/parties/{id}/trading_profile", s.authenticated(read(s, cfg.PTY.GetTradingProfile, okView[ptyapi.TradingProfileView])))
	mux.Handle("GET /api/brant/v1/pty/contacts/{id}", s.authenticated(read(s, cfg.PTY.GetContact, contactView)))
	mux.Handle("GET /api/brant/v1/cat/skus/{id}", s.authenticated(read(s, cfg.CAT.GetSku, skuView)))
	mux.Handle("GET /api/brant/v1/cat/item_code_mappings/{id}", s.authenticated(read(s, cfg.CAT.GetItemCodeMapping, okView[catapi.ItemCodeMappingView])))
	mux.Handle("GET /api/brant/v1/cat/declaration_profiles/{id}", s.authenticated(read(s, cfg.CAT.GetDeclarationProfile, okView[catapi.DeclarationProfileView])))
	mux.Handle("GET /api/brant/v1/cat/skus/{id}/declaration_profiles", s.authenticated(read(s, cfg.CAT.ListDeclarationProfiles, okView[catapi.DeclarationProfileList])))
	mux.Handle("GET /api/brant/v1/inq/rfqs/{id}", s.authenticated(read(s, cfg.INQ.GetRfq, rfqView)))
	mux.Handle("GET /api/brant/v1/pol/configs/{kind}/{id}", s.authenticated(s.configRead))
	mux.Handle("GET /api/brant/v1/pol/snapshots/{id}", s.authenticated(read(s, cfg.POL.GetSnapshot, snapshotView)))
	mux.Handle("GET /api/brant/v1/sup/supplier_rfqs/{id}", s.authenticated(read(s, cfg.SUP.GetSupplierRfq, okView[supapi.SupplierRfqView])))
	for _, k := range supConditionKinds {
		get := func(ctx context.Context, id string) (supapi.ConditionView, error) {
			return cfg.SUP.GetCondition(ctx, k.kind, id)
		}
		mux.Handle("GET /api/brant/v1/sup/"+k.route+"/{id}", s.authenticated(read(s, get, okView[supapi.ConditionView])))
	}
	mux.Handle("GET /api/brant/v1/cst/costing_contexts/{id}", s.authenticated(read(s, cfg.CST.GetContext, okView[cstapi.ContextView])))
	mux.Handle("GET /api/brant/v1/cst/measurements/{id}", s.authenticated(read(s, cfg.CST.GetMeasurement, okView[cstapi.MeasurementView])))
	mux.Handle("GET /api/brant/v1/cst/cost_scenarios/{id}", s.authenticated(read(s, cfg.CST.GetScenario, okView[cstapi.ScenarioView])))
	mux.Handle("GET /api/brant/v1/cst/cost_evaluations/{id}", s.authenticated(read(s, cfg.CST.GetEvaluation, okView[cstapi.EvaluationView])))
	mux.Handle("GET /api/brant/v1/cst/cost_snapshots/{id}", s.authenticated(read(s, cfg.CST.GetSnapshot, okView[cstapi.SnapshotView])))
	mux.Handle("GET /api/brant/v1/cst/cost_snapshots/{id}/adoptability", s.authenticated(read(s, cfg.CST.QuerySnapshot, okView[cstapi.SnapshotQuery])))
	mux.Handle("GET /api/brant/v1/cst/source_attestations/{id}", s.authenticated(read(s, cfg.CST.GetAttestation, okView[cstapi.AttestationView])))
	mux.Handle("GET /api/brant/v1/trd/quotes/{id}", s.authenticated(read(s, cfg.TRD.GetQuote, okView[trdapi.QuoteView])))
	mux.Handle("GET /api/brant/v1/trd/quotes/{id}/active", s.authenticated(read(s, cfg.TRD.GetActiveRevision, okView[trdapi.FormalRevision])))
	mux.Handle("GET /api/brant/v1/trd/quotes/{id}/approvals/{approval_id}/check", s.authenticated(s.approvalCheck))
	mux.Handle("GET /api/brant/v1/trd/quotes/{id}/revisions/{revision}/document", s.authenticated(s.formalDocument))
	mux.Handle("GET /api/brant/v1/trd/quotes/{id}/ready", s.authenticated(s.quoteReady))
	mux.Handle("GET /api/brant/v1/trd/quotes/{id}/history", s.authenticated(quoteHistory(s, cfg.TRD.QuoteHistory)))
	mux.Handle("GET /api/brant/v1/trd/quotes/{id}/approvals", s.authenticated(quoteHistory(s, cfg.TRD.ApprovalHistory)))
	mux.Handle("GET /api/brant/v1/trd/quotes/{id}/documents", s.authenticated(quoteHistory(s, cfg.TRD.DocumentHistory)))
	mux.Handle("GET /api/brant/v1/trd/quotes/{id}/commits", s.authenticated(quoteHistory(s, cfg.TRD.CommitHistory)))
	mux.Handle("GET /api/brant/v1/trd/quotes/{id}/outbox", s.authenticated(quoteHistory(s, cfg.TRD.OutboxHistory)))
	mux.Handle("GET /api/brant/v1/trd/quotes/{id}/revisions/{revision}/dispatch_preflight", s.authenticated(dispatchRead(s, cfg.TRD.DispatchPreflight)))
	mux.Handle("GET /api/brant/v1/trd/quotes/{id}/revisions/{revision}/dispatch_target", s.authenticated(dispatchRead(s, cfg.TRD.DispatchTarget)))
	mux.Handle("GET /api/brant/v1/trd/orders/{id}", s.authenticated(read(s, cfg.TRD.GetOrder, okView[trdapi.OrderView])))
	mux.Handle("GET /api/brant/v1/trd/orders/{id}/check", s.authenticated(s.orderCheck))
	mux.Handle("GET /api/brant/v1/trd/orders/{id}/snapshot", s.authenticated(read(s, cfg.TRD.GetOrderSnapshot, okView[trdapi.OrderSnapshotFact])))
	mux.Handle("GET /api/brant/v1/trd/orders/{id}/revisions/{revision}/documents/{kind}", s.authenticated(s.orderDocument))
	mux.Handle("GET /api/brant/v1/trd/customer_responses/{id}", s.authenticated(read(s, cfg.TRD.GetCustomerResponse, okView[trdapi.CustomerResponseView])))
	mux.Handle("GET /api/brant/v1/trd/acceptances/{id}", s.authenticated(read(s, cfg.TRD.GetAcceptance, okView[trdapi.AcceptanceView])))
	mux.Handle("GET /api/brant/v1/pur/purchase_drafts/{id}", s.authenticated(read(s, cfg.PUR.GetDraft, okView[purapi.DraftView])))
	mux.Handle("GET /api/brant/v1/pur/orders/{id}/allocations", s.authenticated(s.allocations))
	mux.Handle("GET /api/brant/v1/pur/purchase_commitments/{id}", s.authenticated(read(s, cfg.PUR.GetCommitment, okView[purapi.CommitmentView])))
	mux.Handle("GET /api/brant/v1/pur/orders/{id}/commitments", s.authenticated(s.orderCommitments))
	mux.Handle("GET /api/brant/v1/pur/supplier_deliveries/{id}", s.authenticated(read(s, cfg.PUR.GetDelivery, okView[purapi.DeliveryView])))
	mux.Handle("GET /api/brant/v1/pur/purchase_commitments/{id}/deliveries", s.authenticated(read(s, cfg.PUR.GetCommitmentDeliveries, okView[purapi.CommitmentDeliveriesView])))
	mux.Handle("GET /api/brant/v1/pur/purchase_commitment_changes/{id}", s.authenticated(read(s, cfg.PUR.GetCommitmentChange, okView[purapi.ChangeView])))
	mux.Handle("GET /api/brant/v1/pur/purchase_commitments/{id}/changes", s.authenticated(s.commitmentChanges))
	mux.Handle("GET /api/brant/v1/pur/allocation_reviews/{id}", s.authenticated(read(s, cfg.PUR.GetAllocationReview, okView[purapi.AllocationReviewView])))
	mux.Handle("GET /api/brant/v1/pur/orders/{id}/allocation_reviews", s.authenticated(s.orderAllocationReviews))
	if cfg.QLT != nil {
		mux.Handle("GET /api/brant/v1/qlt/inspections/{id}", s.authenticated(read(s, cfg.QLT.GetInspection, okView[qltapi.InspectionView])))
		mux.Handle("GET /api/brant/v1/qlt/quality_releases/{id}", s.authenticated(read(s, cfg.QLT.GetRelease, okView[qltapi.ReleaseView])))
		mux.Handle("GET /api/brant/v1/qlt/subjects/{kind}/{id}/lines/{line_id}", s.authenticated(s.qualitySubject))
	}
	if cfg.STL != nil {
		s.stlRoutes(mux)
	}
	mux.Handle("GET /api/brant/v1/ref/screening_lists/{id}", s.authenticated(read(s, cfg.REF.GetList, okView[refapi.ListView])))
	mux.Handle("GET /api/brant/v1/ref/screening_lists/{id}/versions/{version}", s.authenticated(s.listVersion))
	mux.Handle("GET /api/brant/v1/ref/country_programme_lists/{id}", s.authenticated(read(s, cfg.REF.GetProgrammeList, okView[refapi.ProgrammeListView])))
	mux.Handle("GET /api/brant/v1/ref/country_programme_lists/{id}/versions/{version}", s.authenticated(s.programmeListVersion))
	if cfg.CNX != nil {
		s.cnxRoutes(mux)
	}
	if cfg.FUL != nil {
		s.fulRoutes(mux)
	}
	return s.correlated(mux), nil
}

type server struct {
	cfg      Config
	contexts map[string]*commandSet
}

// correlated gives every request a correlation ID, returned in X-Correlation-ID and in every
// rejection, and recorded with every audit entry and event the request causes.
func (s *server) correlated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := action.NewID("corr", s.cfg.Clock.Now())
		w.Header().Set("X-Correlation-ID", id)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		defer func() {
			if p := recover(); p != nil {
				s.cfg.Logger.Error("request panicked", "correlation_id", id, "panic", p)
				writeJSON(w, http.StatusInternalServerError, transportError{Error: "INTERNAL", CorrelationID: id})
			}
		}()
		next.ServeHTTP(w, r.WithContext(action.WithCorrelation(r.Context(), id)))
	})
}

// authenticated admits a request only with a verified bearer token; the principal it yields is the
// only source of actor, tenant and company.
func (s *server) authenticated(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		p, err := s.cfg.Verifier.Verify(ctx, r.Header.Get("Authorization"))
		if err == nil {
			ctx, err = identity.WithPrincipal(ctx, p)
		}
		if err != nil {
			s.cfg.Logger.Info("request not authenticated", "correlation_id", action.Correlation(ctx), "reason", err)
			w.Header().Set("WWW-Authenticate", `Bearer`)
			s.reject(w, r.WithContext(ctx), kernel.Reject(kernel.CodeUnauthenticated, "/", ""))
			return
		}
		next(w, r.WithContext(ctx))
	})
}

// status is the HTTP status of each rejection category (error catalog V0.1).
var status = map[kernel.Category]int{
	kernel.CategoryUnsupported:     http.StatusBadRequest,
	kernel.CategoryUnauthenticated: http.StatusUnauthorized,
	kernel.CategoryForbidden:       http.StatusForbidden,
	kernel.CategoryNotFound:        http.StatusNotFound,
	kernel.CategoryConflict:        http.StatusConflict,
	kernel.CategoryTooLarge:        http.StatusRequestEntityTooLarge,
	kernel.CategoryInvalid:         http.StatusUnprocessableEntity,
}

type fieldError struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// rejection is the closed error branch of a response (Python BC_T01_response.schema.json).
type rejection struct {
	OK            bool         `json:"ok"`
	Code          string       `json:"code"`
	FieldErrors   []fieldError `json:"field_errors"`
	CorrelationID string       `json:"correlation_id"`
	Retryable     bool         `json:"retryable"`
}

// transportError answers a failure that is not a business decision: the outcome is unknown or the
// service is unavailable, and the client reconciles by replaying the same idempotency key.
type transportError struct {
	Error         string `json:"transport_error"`
	CorrelationID string `json:"correlation_id"`
}

// fail answers err: a rejection by its category, anything else as a transport error that reveals
// nothing about the cause.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if rej, ok := kernel.AsRejection(err); ok {
		s.reject(w, r, rej)
		return
	}
	id := action.Correlation(r.Context())
	if errors.Is(err, application.ErrCommitUncertain) {
		s.cfg.Logger.Error("commit outcome uncertain", "correlation_id", id, "error", err)
		writeJSON(w, http.StatusServiceUnavailable, transportError{Error: "COMMIT_UNCERTAIN", CorrelationID: id})
		return
	}
	s.cfg.Logger.Error("request failed", "correlation_id", id, "error", err)
	writeJSON(w, http.StatusServiceUnavailable, transportError{Error: "UNAVAILABLE", CorrelationID: id})
}

func (s *server) reject(w http.ResponseWriter, r *http.Request, rej *kernel.Rejection) {
	code, ok := status[rej.Category]
	if !ok {
		code = http.StatusUnprocessableEntity
	}
	body := rejection{Code: rej.Code, FieldErrors: []fieldError{}, CorrelationID: action.Correlation(r.Context())}
	for _, f := range rej.Fields {
		body.FieldErrors = append(body.FieldErrors, fieldError{Path: f.Path, Reason: f.Reason})
	}
	writeJSON(w, code, body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // the status is sent; a broken connection is the client's to retry
}

// audit answers GET /api/brant/v1/<bc>/audit.
func (s *server) audit(w http.ResponseWriter, r *http.Request) {
	set, ok := s.contexts[r.PathValue("bc")]
	if !ok {
		s.reject(w, r, kernel.Reject(kernel.CodeNotFound, "/", ""))
		return
	}
	entries, err := set.audit(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]auditView, len(entries))
	for i, e := range entries {
		out[i] = auditView{Seq: e.Seq, Actor: actorView{string(e.Actor.Kind), e.Actor.ID}, Command: e.Command, ObjectID: e.ObjectID,
			Outcome: e.Outcome, Code: e.Code, CorrelationID: e.CorrelationID, RecordedAt: timestamp(e.RecordedAt), Reason: e.Reason,
			AuthorityEpoch: e.AuthorityEpoch}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "entries": out})
}

// read answers GET of one object through an audited query.
func read[V any, O any](s *server, get func(context.Context, string) (V, error), view func(V) O) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := get(r.Context(), r.PathValue("id"))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, view(v))
	}
}

// timestamp formats a stored instant as RFC 3339 UTC with microseconds.
func timestamp(t time.Time) string { return kernel.Instant(t).Format("2006-01-02T15:04:05.000000Z") }

// replay answers POST /api/brant/v1/msg/dead_letters/<source>/<event_id>/replay. The letter goes
// out again with its original event ID, so consumers deduplicate it.
func (s *server) replay(w http.ResponseWriter, r *http.Request) {
	res, err := s.cfg.Replayer.Replay(r.Context(), r.PathValue("source"), r.PathValue("event_id"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "source": res.Source, "event_id": string(res.EventID),
			"replayed_at": timestamp(res.ReplayedAt), "authority_epoch": res.AuthorityEpoch, "attempts": res.Attempts})
	case errors.Is(err, authorization.ErrForbidden), errors.Is(err, authorization.ErrActorKindDenied),
		errors.Is(err, authorization.ErrUnknownCommand), errors.Is(err, authorization.ErrScopeMismatch):
		s.reject(w, r, kernel.Reject(kernel.CodePermissionDenied, "/", ""))
	case errors.Is(err, messaging.ErrDeadLetterNotFound):
		s.reject(w, r, kernel.Reject(kernel.CodeNotFound, "/event_id", ""))
	case errors.Is(err, messaging.ErrAlreadyReplayed):
		s.reject(w, r, kernel.Reject(kernel.CodeInvalidState, "/event_id", "already replayed"))
	case errors.Is(err, messaging.ErrReplayFailed):
		s.cfg.Logger.Warn("dead letter replay failed", "correlation_id", action.Correlation(r.Context()), "error", err)
		writeJSON(w, http.StatusBadGateway, transportError{Error: "REPLAY_FAILED", CorrelationID: action.Correlation(r.Context())})
	default:
		s.fail(w, r, err)
	}
}

// screeningsDue answers GET /api/brant/v1/pty/screenings/due[?at=<UTC timestamp>]: the parties the
// caller sees whose screening is due at that instant, now by default.
func (s *server) screeningsDue(w http.ResponseWriter, r *http.Request) {
	at := s.cfg.Clock.Now()
	if q := r.URL.Query(); q.Has("at") {
		t, err := kernel.ParseTimestamp(q.Get("at"))
		if err != nil || len(q["at"]) != 1 {
			s.reject(w, r, kernel.Reject(kernel.CodeSchemaInvalid, "/at", "UTC timestamp"))
			return
		}
		at = t
	}
	due, err := s.cfg.PTY.ScreeningsDue(r.Context(), at)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if due == nil {
		due = []ptyapi.ScreeningDue{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "at": timestamp(at), "due": wire(due)})
}

// listVersion answers GET /api/brant/v1/ref/screening_lists/<code>/versions/<version>.
func (s *server) listVersion(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.REF.GetListVersion(r.Context(), r.PathValue("id"), r.PathValue("version"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, okView(v))
}
