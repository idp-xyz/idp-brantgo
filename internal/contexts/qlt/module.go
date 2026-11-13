// Package qlt assembles the QLT bounded context (quality inspection and release: inspections with
// their dispositions, quality releases with their revocations, QualityReleaseGuard v1 for FUL;
// WBS S5-02). Code outside QLT uses it through package api; everything under internal/ is private
// to QLT.
package qlt

import (
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/adapters/subjects"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/application"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// Module is QLT wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands       = (*Module)(nil)
	_ api.Queries        = (*Module)(nil)
	_ api.Auditor        = (*Module)(nil)
	_ api.Reconciler     = (*Module)(nil)
	_ api.ReleaseLookups = (*Module)(nil)
)

// Upstream are the lookups of QLT's upstream contexts: PUR's inspection subjects through QLT's port
// Subjects (R24), EVD's sources and files (R72) and PTY's service roles (R73).
type Upstream = application.Upstream

// The port to PUR (InspectionSubject v1) and what passes through it.
type (
	Subjects = application.Subjects
	Subject  = application.Subject
)

// SubjectsNotWired is the port to PUR while PUR publishes no InspectionSubject v1 (S5 unit A4):
// every command and read that needs a subject answers DEPENDENCY_NOT_SUPPORTED.
func SubjectsNotWired() Subjects { return application.NotWired{} }

// PURSubjects is the port to PUR over its InspectionSubject v1 lookup (R24).
func PURSubjects(lines purapi.InspectionSubjects) Subjects { return subjects.PUR{Lines: lines} }

// New wires QLT to db, a transaction owner that belongs to QLT alone, and to its upstreams.
func New(db *bentopg.DB, upstream Upstream) (*Module, error) {
	if !upstream.Complete() {
		return nil, errors.New("qlt: module needs PUR's subjects and the lookups of EVD and PTY")
	}
	policy, err := authorization.NewPolicy(application.Rules()...)
	if err != nil {
		return nil, err
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	clock := bento.SystemClock{}
	return &Module{application.Service{Clock: clock, Upstream: upstream, Ledger: &action.Ledger{
		DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
		Outbox: outbox, Clock: clock, ReplayRead: api.CommandGetInspection, Visible: application.Visible,
	}}}, nil
}
