// Package application runs SUP's commands and queries on the command ledger. The revision state
// machine lives in domain/revision, the supplier RFQ in domain/supplierrfq, the pricing rules SUP
// shares with CST in domain/pricing; SQL lives in adapters/postgres.
//
// SUP's upstream contexts are INQ (RfqLineRef v1, R04), PTY (PartyRef v1 and ServiceRoleRef v1,
// R05), CAT (SkuRef v1 and ItemCodeMappingRef v1, R06), POL (ConfigRef v1, R07) and EVD (SourceRef
// v1, R53); none of these relationships carries a guard lock, so SUP reads them through their api
// packages before its own transaction begins (the ledger's Preparer), and judges what they said in
// the transaction (admission.go). SUP's downstream contexts re-check the revisions they adopt inside
// their own transactions through SupplyConditionGuard v1.
package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Read commands and permissions.
const (
	CommandGetSupplierRfq = "supplier_rfq.get"
	CommandAudit          = "sup.audit"
	PermissionRead        = "sup.read"
	PermissionAudit       = "sup.audit"
)

// CommandGet is the audited read of a supply condition of kind ("supplier_offer.get").
func CommandGet(kind string) string { return kind + ".get" }

var (
	anyActor     = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}
	humanOrAgent = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}
)

// Rules is SUP's authorization policy (05 stage humanOnlyCommands: supplier_rfq.record_issued,
// supplier_rfq.close, supplier_offer.verify, supplier_offer.withdraw, purchase_terms.publish,
// purchase_terms.retire, logistics_offer.verify, logistics_offer.withdraw). Drafts, revisions and
// discards are open to AI agents with the grant (SM-SUP-OFFER: discard_draft human|ai); no
// command is open to system actors.
func Rules() []authorization.Rule {
	rules := []authorization.Rule{
		{Command: api.CommandSaveSupplierRfq, Permission: api.CommandSaveSupplierRfq, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandIssueSupplierRfq, api.CommandIssueSupplierRfq),
		authorization.HumanOnly(api.CommandCloseSupplierRfq, api.CommandCloseSupplierRfq),
		{Command: CommandGetSupplierRfq, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandAudit, Permission: PermissionAudit, ActorKinds: anyActor},
	}
	for _, kind := range []string{api.KindSupplierOffer, api.KindPurchaseTerms, api.KindLogisticsOffer} {
		for _, a := range []string{api.ActionSaveDraft, api.ActionRevise, api.ActionDiscardDraft} {
			c := api.Command(kind, a)
			rules = append(rules, authorization.Rule{Command: c, Permission: c, ActorKinds: humanOrAgent})
		}
		for _, a := range []string{api.SealAction(kind), api.EndAction(kind)} {
			c := api.Command(kind, a)
			rules = append(rules, authorization.HumanOnly(c, c))
		}
		rules = append(rules, authorization.Rule{Command: CommandGet(kind), Permission: PermissionRead, ActorKinds: anyActor})
	}
	return rules
}

// Visible reports whether d's actor sees the object a stored result names: supplier RFQs and supply
// conditions are owned by their creator and filtered by data scope (Python record_access).
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	sc := scope(ctx)
	kind, _, _ := strings.Cut(command, ".")
	if kind == "supplier_rfq" {
		r, err := postgres.LoadSupplierRfq(ctx, exec, sc, id, false)
		return err == nil && r != nil && d.Sees(r.OwnerActor, r.OwnerTeam), err
	}
	if !api.KindKnown(kind) {
		return false, nil
	}
	root, err := postgres.LoadRoot(ctx, exec, sc, kind, id, 0, false)
	return err == nil && root != nil && d.Sees(root.OwnerActor, root.OwnerTeam), err
}

// Parties is what SUP reads from PTY: parties (PartyRef v1) and service roles (ServiceRoleRef v1).
type Parties interface {
	ResolveParties(ctx context.Context, q ptyapi.Resolve) (ptyapi.Resolved, error)
	ResolveServiceRoles(ctx context.Context, refs []ptyapi.ServiceRoleRef, at time.Time) (map[ptyapi.ServiceRoleRef]ptyapi.ResolvedServiceRole, error)
}

// Upstream are the contexts SUP reads references from, before its own transaction.
type Upstream struct {
	Sources evdapi.Queries
	Parties Parties
	Skus    catapi.Queries
	Rfqs    inqapi.RfqLookup
	Configs polapi.Queries
}

// Service runs SUP's use cases.
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Upstream Upstream
}

func (s Service) run(ctx context.Context, cmd action.Command, args any, validation error, h action.Handler) (api.Receipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return api.Receipt{}, errors.Join(validation, err)
			}
		}
		return api.Receipt{}, validation
	}
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Receipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	return api.Receipt(res), err
}

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// prepared gives a handler with a prepare step the ledger's Preparer: it reads upstream contexts
// after the command is authorized and found not to be a replay, before SUP's transaction.
type prepared struct {
	action.Handler
	prepare func(ctx context.Context) error
}

func (p prepared) Prepare(ctx context.Context) error { return p.prepare(ctx) }

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}

// Audit returns SUP's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, CommandAudit)
}
