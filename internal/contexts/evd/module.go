// Package evd assembles the EVD bounded context (evidence: sources and business files). Code
// outside EVD uses it through package api; everything under internal/ is private to EVD.
package evd

import (
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/adapters/events"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/application"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
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
	_ api.FormalFiles      = (*Module)(nil)
)

// Option configures the module.
type Option func(*application.Service)

// WithExtractor gives EVD the reader of file_extract sources: the AI layer's deterministic extractor
// (internal/ai/ingest), which bootstrap wires; EVD itself holds no parsing code (Context Map EVD
// boundary; ADR-BR-010).
func WithExtractor(x api.Extractor) Option {
	return func(s *application.Service) { s.Extractor = x }
}

// FormalDocuments is TRD's FormalDocument read (Context Map R65 TRD→EVD): the formal documents of
// quote revisions and the S/C and PI of order revisions with their bytes; trd.Module implements it.
type FormalDocuments = application.FormalDocuments

// WithFormalDocuments gives EVD TRD's FormalDocument read, with which its Inbox consumer
// evd.formal_files files TRD's formal documents as business files (WBS S4-06c; R65; IG-08). EVD is
// composed before TRD, which reads EVD's sources and files (R16), so bootstrap binds TRD by a
// function EVD calls when an event arrives (S5 decision D-2). Without it EVD declares no consumer.
func WithFormalDocuments(docs func() FormalDocuments) Option {
	return func(s *application.Service) { s.Formal = docs }
}

// Subscriptions are EVD's Inbox consumers on its own transaction owner (repository structure §07):
// evd.formal_files, which files the formal document TRD's QuoteActivated, QuoteReplaced and
// OrderDocumentGenerated announce (Context Map R65; WBS S4-06c), when EVD was given TRD's read.
func (m *Module) Subscriptions() []messaging.Subscription {
	if m.Formal == nil {
		return nil
	}
	return events.Subscriptions(m.Ledger.DB, m.Service)
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
