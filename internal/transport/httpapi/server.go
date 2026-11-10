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
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Verifier turns a bearer token into the verified principal of the request.
type Verifier interface {
	Verify(ctx context.Context, token string) (identity.Principal, error)
}

// EVD, PTY, CAT and INQ are what the transport needs from each context.
type (
	EVD interface {
		evdapi.Commands
		evdapi.Queries
		evdapi.Auditor
	}
	PTY interface {
		ptyapi.Commands
		ptyapi.Queries
		ptyapi.Auditor
	}
	CAT interface {
		catapi.Commands
		catapi.Queries
		catapi.Auditor
	}
	INQ interface {
		inqapi.Commands
		inqapi.Queries
		inqapi.Auditor
	}
)

// Config wires the transport.
type Config struct {
	EVD      EVD
	PTY      PTY
	CAT      CAT
	INQ      INQ
	Verifier Verifier
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
	if cfg.EVD == nil || cfg.PTY == nil || cfg.CAT == nil || cfg.INQ == nil || cfg.Verifier == nil || cfg.Clock == nil {
		return nil, errors.New("httpapi: contexts, verifier and clock are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = MaxBody
	}
	s := &server{cfg: cfg, contexts: map[string]*commandSet{}}
	for _, set := range []*commandSet{evdCommands(cfg.EVD), ptyCommands(cfg.PTY), catCommands(cfg.CAT), inqCommands(cfg.INQ)} {
		s.contexts[set.bc] = set
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.Handle("POST /api/brant/v1/{bc}/commands/{command}", s.authenticated(s.command))
	mux.Handle("GET /api/brant/v1/{bc}/audit", s.authenticated(s.audit))
	mux.Handle("GET /api/brant/v1/evd/sources/{id}", s.authenticated(read(s, cfg.EVD.GetSource, sourceView)))
	mux.Handle("GET /api/brant/v1/evd/files/{id}", s.authenticated(read(s, cfg.EVD.GetFile, fileView)))
	mux.Handle("GET /api/brant/v1/pty/parties/{id}", s.authenticated(read(s, cfg.PTY.GetParty, partyView)))
	mux.Handle("GET /api/brant/v1/pty/contacts/{id}", s.authenticated(read(s, cfg.PTY.GetContact, contactView)))
	mux.Handle("GET /api/brant/v1/cat/skus/{id}", s.authenticated(read(s, cfg.CAT.GetSku, skuView)))
	mux.Handle("GET /api/brant/v1/inq/rfqs/{id}", s.authenticated(read(s, cfg.INQ.GetRfq, rfqView)))
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
