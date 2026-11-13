// Package evd assembles the EVD bounded context (evidence: sources and business files). Code
// outside EVD uses it through package api; everything under internal/ is private to EVD.
package evd

import (
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/application"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is EVD wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands         = (*Module)(nil)
	_ api.Queries          = (*Module)(nil)
	_ api.ExtractQueries   = (*Module)(nil)
	_ api.Auditor          = (*Module)(nil)
	_ api.Reconciler       = (*Module)(nil)
	_ api.ActionReconciler = (*Module)(nil)
)

// Option configures the module.
type Option func(*application.Service)

// WithExtractor gives EVD the reader of file_extract sources: the AI layer's deterministic extractor
// (internal/ai/ingest), which bootstrap wires; EVD itself holds no parsing code (Context Map EVD
// boundary; ADR-BR-010).
func WithExtractor(x api.Extractor) Option {
	return func(s *application.Service) { s.Extractor = x }
}

// New wires EVD to db, a transaction owner that belongs to EVD alone.
func New(db *bentopg.DB, opts ...Option) (*Module, error) {
	policy, err := authorization.NewPolicy(application.Rules()...)
	if err != nil {
		return nil, err
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	clock := bento.SystemClock{}
	m := &Module{application.Service{Clock: clock, Ledger: &action.Ledger{
		DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
		Outbox: outbox, Clock: clock, ReplayRead: application.CommandGetSource, Visible: application.Visible,
	}}}
	for _, o := range opts {
		o(&m.Service)
	}
	return m, nil
}
