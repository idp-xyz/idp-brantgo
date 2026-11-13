// Package application runs QLT's commands and queries on the command ledger. The inspection rules
// live in domain/inspection, the release rules in domain/release; SQL lives in adapters/postgres.
//
// QLT's upstream contexts are PUR (Context Map R24, InspectionSubject v1, through QLT's own port
// Subjects), EVD (R72, SourceRef v1 for standards and evidence, FileRef v1 for reports), PTY (R73,
// ServiceRoleRef v1 for external inspection agencies, through QLT's own port Agencies; S5R-D36) and
// FUL (R79, ReleaseConsumption v1, through the port ReleaseConsumption; S5R-D18). None of these
// relationships is a guard edge. PUR, EVD and PTY are read through their api packages before QLT's
// transaction begins (the ledger's Preparer step) and the domain judges what they said inside it;
// FUL's ReleaseConsumption v1 is the reverse read of R26 (S5-D33): quality_release.revoke runs it
// with QLT's own executor inside its transaction, after it locked the release row, without a lock.
// QLT never calls another context's transaction owner, and it publishes QualityReleaseGuard v1 to
// FUL (R26).
//
// Lock order inside QLT's transactions (ADR-BR-011, tactical lockOrder "QLT (release root; subject
// inspections)"): the command's own root FOR UPDATE — the inspection or release its ID names, when
// it exists — then, for inspection.record, quality_release.grant and quality_release.revoke, every
// inspection of the subject FOR UPDATE in inspection ID order, then the company authority guard FOR
// SHARE. Every command that changes what a subject may release locks the subject's inspections
// (disposition.decide locks the one inspection it disposes, which the others lock too), so they
// commit one at a time; two first inspections of a subject collide on the subject's inspection
// number and the second runs again.
package application

import (
	"context"
	"errors"
	"strings"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var anyActor = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}

// Rules is QLT's authorization policy. The tactical model's humanOnlyCommands name
// disposition.decide, quality_release.grant and quality_release.revoke: only our authorized human
// QC releases (F-06), an AI or system actor is PERMISSION_DENIED. inspection.record is human or AI
// (SM-QLT-INSPECTION, owner decision D-17: an AI may transcribe a report). Reads need qlt.read, the
// audit trail qlt.audit.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandRecordInspection, Permission: api.CommandRecordInspection, ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		authorization.HumanOnly(api.CommandDecideDisposition, api.CommandDecideDisposition),
		authorization.HumanOnly(api.CommandGrantRelease, api.CommandGrantRelease),
		authorization.HumanOnly(api.CommandRevokeRelease, api.CommandRevokeRelease),
		{Command: api.CommandGetInspection, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetRelease, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetSubject, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandAudit, Permission: api.PermissionAudit, ActorKinds: anyActor},
	}
}

// Upstream are the contexts QLT reads references from.
type Upstream struct {
	Subjects    Subjects           // PUR InspectionSubject v1 (R24), QLT's port
	Sources     evdapi.Queries     // EVD SourceRef v1 and FileRef v1 (R72)
	Agencies    Agencies           // PTY ServiceRoleRef v1 of inspection agencies (R73, S5R-D36), QLT's port
	Consumption ReleaseConsumption // FUL ReleaseConsumption v1 (R79, S5R-D18)
}

// Complete reports whether every upstream is wired.
func (u Upstream) Complete() bool {
	return u.Subjects != nil && u.Sources != nil && u.Agencies != nil && u.Consumption != nil
}

// Service runs QLT's use cases.
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Upstream Upstream
}

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// Visible reports whether d's actor sees the inspection or release a stored result names.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	sc := scope(ctx)
	if strings.HasPrefix(command, "quality_release.") {
		r, err := postgres.LoadRelease(ctx, exec, sc, id, postgres.NoLock)
		return err == nil && r != nil && d.Sees(r.OwnerActor, r.OwnerTeam), err
	}
	i, err := postgres.LoadInspection(ctx, exec, sc, id, postgres.NoLock)
	return err == nil && i != nil && d.Sees(i.OwnerActor, i.OwnerTeam), err
}

func (s Service) run(ctx context.Context, cmd action.Command, req api.Command, validation error, h action.Handler) (api.Receipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return api.Receipt{}, errors.Join(validation, err)
			}
		}
		return api.Receipt{}, validation
	}
	_, _, args := request(req)
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Receipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	return api.Receipt(res), err
}

// request returns the name, the idempotency key and the business arguments of cmd, which its
// request hash covers: everything but the key and the expected version.
func request(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.RecordInspection:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandRecordInspection, key, c
	case api.DecideDisposition:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandDecideDisposition, key, c
	case api.GrantRelease:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandGrantRelease, key, c
	case api.RevokeRelease:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandRevokeRelease, key, c
	}
	return "", "", nil
}

// requestHash is the hash the ledger records for cmd; a taken inspection or release ID is answered
// from the stored record only for the same hash.
func requestHash(cmd api.Command) string {
	_, _, args := request(cmd)
	h, _ := action.RequestHash(args)
	return h
}

// Reconcile reports what became of cmd, sent earlier with its idempotency key, without executing
// it (api.Reconciler; contract 06, 动作对账).
func (s Service) Reconcile(ctx context.Context, cmd api.Command) (api.Reconciliation, error) {
	name, key, args := request(cmd)
	if name == "" {
		return api.Reconciliation{}, kernel.Reject(kernel.CodeSchemaInvalid, "/command", "")
	}
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Reconciliation{}, err
	}
	r, err := s.Ledger.Reconcile(ctx, name, key, hash)
	if err != nil {
		return api.Reconciliation{}, err
	}
	out := api.Reconciliation{State: r.State, ActionID: r.ActionID, ObjectID: r.ObjectID}
	if r.Result != nil {
		receipt := api.Receipt(*r.Result)
		out.Receipt = &receipt
	}
	return out, nil
}

// Audit returns QLT's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, api.CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}

// subjects asks PUR about refs through the port. ErrSubjectsNotWired is DEPENDENCY_NOT_SUPPORTED at
// path.
func (s Service) subjects(ctx context.Context, refs []api.SubjectRef, path string) (map[api.SubjectRef]Subject, error) {
	if len(refs) == 0 {
		return map[api.SubjectRef]Subject{}, nil
	}
	got, err := s.Upstream.Subjects.ResolveSubjects(ctx, refs)
	if errors.Is(err, ErrSubjectsNotWired) {
		return nil, api.Reject(api.CodeDependencyNotSupported, path, "PUR's InspectionSubject v1 is not wired")
	}
	return got, err
}

// sources asks EVD which of ids the caller reads, with their content hashes. A command that cites
// no source does not ask and needs no EVD permission.
func (s Service) sources(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	refs, err := s.Upstream.Sources.ResolveSources(ctx, ids)
	for id, r := range refs {
		out[id] = r.ContentSHA256
	}
	return out, err
}

// factErr passes on an upstream's refusal of a looked-up reference at path: NOT_FOUND is
// REFERENCE_INVALID there, PERMISSION_DENIED stays itself.
func factErr(err error, path string) error {
	rej, ok := kernel.AsRejection(err)
	switch {
	case !ok:
		return err
	case rej.Code == kernel.CodeNotFound:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "File is not accessible")
	case rej.Code == kernel.CodePermissionDenied:
		return err
	}
	return kernel.RejectAs(rej.Code, rej.Category, path, "")
}
