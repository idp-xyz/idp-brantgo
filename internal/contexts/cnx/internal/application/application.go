// Package application runs CNX's commands and queries on the command ledger. The trade compliance
// rules live in domain/tradecompliancecheck; SQL lives in adapters/postgres.
//
// CNX's upstreams for the trade compliance check are TRD (Context Map R46, SalesOrderRef v1,
// through the port Orders), PTY (R77, the countries of addresses, port Countries), CAT (R39,
// DeclarationProfileRef v1, port Profiles), reference data (country programmes, port Programmes),
// POL (R50, ConfigRef v1 and ConfigGuard v1 of screening_regulation, guardLock) and EVD (R54,
// SourceRef v1 of the evidence). A command reads them before CNX's transaction begins (the ledger's
// Preparer step) and the domain judges what they said; inside the transaction, after CNX locked
// its own check FOR UPDATE and the ledger the authority guard FOR SHARE, POL's ConfigGuard v1
// re-checks the regulation revision a conclusion relies on (ADR-BR-004 option B, ADR-BR-011). R46,
// R77, R39 and R54 carry no guard lock: an evaluation binds what it read (the order revision and
// hash, party revisions, profile revisions and hashes, programme versions, source hashes) and a
// later change makes a new evaluation necessary, not a stale conclusion. CNX never calls another
// context's transaction owner.
package application

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var anyActor = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}

// Rules is CNX's authorization policy. The tactical model's humanOnlyCommands name
// trade_compliance.decide (compliance-exclusive in the role templates). SM-CNX-COMPLIANCE gives
// evaluate to human|ai (the S5 plan lets a system evaluate too: candidates only) and reopen to the
// system; compliance holds reopen as well, so humans and system actors reopen, AI agents never.
// Reads need cnx.read, the audit trail cnx.audit.
//
// Export licences (S5-14): humanOnlyCommands name export_license.register and .release; allocate
// (ES-12 step 3, 单证员) admits humans and AI agents on the same command path, no system actor (no
// consumer allocates). The documentation template holds the three permissions.
func Rules() []authorization.Rule {
	return append([]authorization.Rule{
		{Command: api.CommandEvaluate, Permission: api.CommandEvaluate, ActorKinds: anyActor},
		authorization.HumanOnly(api.CommandDecide, api.CommandDecide),
		{Command: api.CommandReopen, Permission: api.CommandReopen, ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorSystem}},
		{Command: api.CommandGetCheck, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandOrderChecks, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandChecksByRegulation, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandChecksByList, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandAudit, Permission: api.PermissionAudit, ActorKinds: anyActor},
		authorization.HumanOnly(api.CommandRegisterLicense, api.CommandRegisterLicense),
		{Command: api.CommandAllocateLicense, Permission: api.CommandAllocateLicense, ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		authorization.HumanOnly(api.CommandReleaseLicense, api.CommandReleaseLicense),
		{Command: api.CommandGetLicense, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandLicenses, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandShipmentLicense, Permission: api.PermissionRead, ActorKinds: anyActor},
		// Purchase invoices and tax refund cases (S5-05b): humanOnlyCommands name
		// purchase_invoice.verify and tax_refund_case.record_result; an import and an allocation are
		// candidates' paths for humans and AI agents alike (CN-AT18), no system actor.
		{Command: api.CommandImportInvoice, Permission: api.CommandImportInvoice, ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		authorization.HumanOnly(api.CommandVerifyInvoice, api.CommandVerifyInvoice),
		{Command: api.CommandAllocateTaxEvidence, Permission: api.CommandAllocateTaxEvidence, ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		authorization.HumanOnly(api.CommandRecordTaxResult, api.CommandRecordTaxResult),
		{Command: api.CommandGetInvoice, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandInvoices, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetTaxCase, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandTaxCases, Permission: api.PermissionRead, ActorKinds: anyActor},
	}, slices.Concat(declarationRules(), bundleRules(), certificateRules(), openingRules())...)
}

// Upstream are the contracts CNX reads.
type Upstream struct {
	Orders     Orders         // TRD SalesOrderRef v1 (R46), CNX's port
	Countries  Countries      // PTY address countries (R77), CNX's port
	Profiles   Profiles       // CAT DeclarationProfileRef v1 (R39), CNX's port
	Programmes Programmes     // reference data's country programmes, CNX's port
	Configs    polapi.Queries // POL ConfigRef v1 (R50)
	Sources    evdapi.Queries // EVD SourceRef v1 and the licence files (R54)
	Exports    Exports        // FUL shipments and export lots (R36), CNX's port
	Purchases  PurchaseLines  // PUR PurchaseLineRef v1 (R37), CNX's port
	Funds      Funds          // STL FundRecordRef v1 (R38), CNX's port
	// STL CreditNoteRef v1 (on R38; S5 C10, CN-AT11): the credit notes a reconciliation records the impact of.
	CreditNotes CreditNotes
}

// Complete reports whether every upstream is wired.
func (u Upstream) Complete() bool {
	return u.Orders != nil && u.Countries != nil && u.Profiles != nil && u.Programmes != nil && u.Configs != nil && u.Sources != nil &&
		u.Exports != nil && u.Purchases != nil && u.Funds != nil && u.CreditNotes != nil
}

// Service runs CNX's use cases.
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Upstream Upstream
}

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// Visible reports whether d's actor sees the object a stored result names: a check by its opener
// or by data scope; a licence, declaration, bundle or certificate of the company always (they are
// company-wide).
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command string, id string) (bool, error) {
	if strings.HasPrefix(command, "export_license.") {
		l, err := postgres.LoadLicense(ctx, exec, scope(ctx), id, postgres.NoLock)
		return l != nil, err
	}
	if strings.HasPrefix(command, "purchase_invoice.") {
		i, err := postgres.LoadInvoice(ctx, exec, scope(ctx), id, postgres.NoLock)
		return i != nil, err
	}
	if ok, visible, err := visibleDeclaration(ctx, exec, command, id); ok {
		return visible, err
	}
	if ok, visible, err := visibleCertificate(ctx, exec, command, id); ok {
		return visible, err
	}
	if ok, visible, err := visibleOpening(ctx, exec, command, id); ok {
		return visible, err
	}
	if strings.HasPrefix(command, "tax_refund_case.") {
		c, err := postgres.LoadCase(ctx, exec, scope(ctx), id, postgres.NoLock)
		return c != nil, err
	}
	c, err := postgres.LoadCheck(ctx, exec, scope(ctx), id, postgres.NoLock)
	if err != nil || c == nil {
		return false, err
	}
	return d.Sees(c.OwnerActor, c.OwnerTeam), nil
}

func (s Service) run(ctx context.Context, cmd action.Command, req api.Command, validation error, h action.Handler) (api.CheckReceipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return api.CheckReceipt{}, errors.Join(validation, err)
			}
		}
		return api.CheckReceipt{}, validation
	}
	_, _, args := request(req)
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.CheckReceipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	if err != nil {
		return api.CheckReceipt{}, err
	}
	return s.receipt(ctx, api.Receipt(res))
}

// receipt rebuilds the answer of a command from the stored check.
func (s Service) receipt(ctx context.Context, res api.Receipt) (api.CheckReceipt, error) {
	out := api.CheckReceipt{Receipt: res, CheckID: res.ObjectID}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		c, err := postgres.LoadCheck(ctx, exec, sc, res.ObjectID, postgres.NoLock)
		if err != nil || c == nil {
			return err
		}
		out.OrderID, out.OrderRevision, out.OrderHash = c.Binding.OrderID, c.Binding.OrderRevision, c.Binding.OrderHash
		out.EvaluationID, out.DecisionID = c.Evaluation, c.Decision
		if c.Evaluation != "" {
			evs, err := postgres.Evaluations(ctx, exec, sc, c.ID, c.Evaluation)
			if err != nil {
				return err
			}
			if len(evs) == 1 {
				out.Outcome = string(evs[0].Outcome)
			}
		}
		if c.Decision != "" {
			ds, err := postgres.Decisions(ctx, exec, sc, c.ID, c.Decision)
			if err != nil {
				return err
			}
			if len(ds) == 1 {
				out.LicenseRequired, out.ValidUntil = ds[0].LicenseRequired, ds[0].ValidUntil
			}
		}
		return nil
	})
	return out, err
}

// request returns the name, the idempotency key and the business arguments of cmd, which its
// request hash covers: everything but the key and the expected version.
func request(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.Evaluate:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandEvaluate, key, c
	case api.Decide:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandDecide, key, c
	case api.Reopen:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandReopen, key, c
	case api.RegisterLicense:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandRegisterLicense, key, c
	case api.AllocateLicense:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandAllocateLicense, key, c
	case api.ReleaseLicense:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandReleaseLicense, key, c
	case api.ImportInvoice:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandImportInvoice, key, c
	case api.VerifyInvoice:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandVerifyInvoice, key, c
	case api.AllocateTaxEvidence:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandAllocateTaxEvidence, key, c
	case api.RecordTaxResult:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRecordTaxResult, key, c
	}
	return declarationRequest(cmd)
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

// Audit returns CNX's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, api.CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}

// sources resolves the EVD sources of ids before CNX's transaction (R54; EVD's read permission).
func (s Service) sources(ctx context.Context, ids []string) (map[string]evdapi.SourceRef, error) {
	if len(ids) == 0 {
		return map[string]evdapi.SourceRef{}, nil
	}
	return s.Upstream.Sources.ResolveSources(ctx, ids)
}

// regulationGuard re-checks a screening_regulation revision inside CNX's transaction with POL's
// ConfigGuard v1 (R50, guardLock), holding it FOR SHARE until CNX commits: in force at the instant,
// at the content hash CNX read (REFERENCE_STALE otherwise).
func regulationGuard(ctx context.Context, exec bentopg.Executor, ref polapi.ConfigRef, hash string, at time.Time, path string) error {
	v, err := polapi.ScanConfigGuard(ref, exec.QueryRow(ctx, polapi.ConfigGuardSQL, polapi.ConfigGuardArgs(ref, at)...).Scan)
	if err != nil {
		return err
	}
	if err = v.Check(path); err != nil {
		return err
	}
	if v.ContentHash != hash {
		return api.Reject(api.CodeReferenceStale, path, "The regulation revision changed")
	}
	return nil
}

// regulationOf judges a screening_regulation revision as POL resolved it before the transaction:
// it must exist as one in the company (REFERENCE_INVALID) and be in force (POL's verdict).
func regulationOf(r *polapi.ResolvedConfig, path string) (*polapi.ScreeningRegulation, error) {
	if r == nil || r.Ref.Kind != polapi.KindScreeningRegulation || r.Data.ScreeningRegulation == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	if err := r.Check(path); err != nil {
		return nil, err
	}
	return r.Data.ScreeningRegulation, nil
}

// usedLists returns the programme lists an evaluation named and the versions it used.
func usedLists(ev *tcc.Evaluation) ([]string, map[string]tcc.ListVersion) {
	versions := map[string]tcc.ListVersion{}
	for _, p := range ev.Programmes {
		versions[p.Code] = tcc.ListVersion{ListCode: p.Code, Jurisdiction: p.Jurisdiction, Version: p.Version, ContentHash: p.ContentHash}
	}
	return ev.Lists, versions
}
