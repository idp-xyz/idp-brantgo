package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// How CNX follows order and regulation changes (S5 unit C4b-CNX, WBS S5-09b; PL-14; WF-06 last
// step; SM-CNX-COMPLIANCE clear → reopen → pending; CNX-INV-001 "订单变更改动目的国、交易方或商品，
// 或法规版本更新时，结论对新修订不适用、须重新核查"; S5 D-9, D-18; MD-47). The Inbox consumer
// cnx.trade_compliance calls these in its transaction on CNX's own transaction owner; every effect
// is a trade_compliance.reopen run through CNX's command ledger as the consumer's system actor, so
// the reopening, its action result, audit entry and TradeComplianceReopened commit with the inbox
// record. Upstream state is read before the transaction (PrepareAmendedOrder, PrepareRegulation)
// or through reference data's list function; the reopen handler's own Prepare is hidden
// (prepared), so nothing reads another context's transaction owner inside CNX's transaction.
//
// Lock order (tactical lockOrder): CNX's own check roots FOR UPDATE in check ID order first, then
// the ledger's authority guard FOR SHARE, then POL's ConfigGuard v1 or reference data's list rows
// FOR SHARE inside each reopening.

// What an event did.
const (
	// EffectCheckOpened: a pending check is opened now for the amended revision.
	EffectCheckOpened = "check_opened"
	// EffectCheckExists: the revision has its check already (opened by compliance, or before a lost
	// answer); nothing is opened twice.
	EffectCheckExists = "check_exists"
	// EffectOrderEnded: TRD's order is no longer draft or active; nothing is opened for it.
	EffectOrderEnded = "order_ended"
	// EffectOrderAmendedAgain: TRD's order is at a later revision; that revision's OrderAmended
	// opens its check.
	EffectOrderAmendedAgain = "order_amended_again"
	// EffectNotScreeningRegulation: POL published a configuration of another kind.
	EffectNotScreeningRegulation = "not_screening_regulation"
	// EffectRegulationNotInForce: the published revision is no longer (or not) in force; a later
	// revision's publication reopens what follows the earlier ones.
	EffectRegulationNotInForce = "regulation_not_in_force"
	// EffectReopened: the clear conclusions named were reopened (none is a valid outcome).
	EffectReopened = "reopened"
)

// Failure codes of events CNX refuses for good.
const (
	// FailureOrderNotCheckable: TRD shows no such order to CNX, its sealed revision is not intact,
	// or it contradicts the event's binding.
	FailureOrderNotCheckable = "cnx.order_not_checkable"
	// FailureReopenRefused: trade_compliance.reopen refused in a way no retry changes.
	FailureReopenRefused = "cnx.reopen_refused"
)

// FollowMismatch is an event CNX cannot apply for good: the consumer refuses it and the relay
// dead-letters it with an alert (ADR-BR-015). Nothing is changed.
type FollowMismatch struct {
	Code   string
	Detail string
}

func (m *FollowMismatch) Error() string { return "cnx: " + m.Code + ": " + m.Detail }

// AmendedOrder is TRD's OrderAmended as CNX reads it: the event and the order revision it produced.
type AmendedOrder struct {
	EventID   string
	EventType string
	Binding   tcc.Binding
}

// OrderBasis is TRD's order as read before the consumer's transaction (nil: TRD shows none).
type OrderBasis struct {
	Order *Order
}

// PrepareAmendedOrder reads, outside any transaction, TRD's order now (SalesOrderRef v1, R46). A
// failure is retried with the event.
func (s Service) PrepareAmendedOrder(ctx context.Context, e AmendedOrder) (OrderBasis, error) {
	orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{e.Binding.OrderID})
	if err != nil {
		return OrderBasis{}, err
	}
	if o, ok := orders[e.Binding.OrderID]; ok {
		return OrderBasis{Order: &o}, nil
	}
	return OrderBasis{}, nil
}

// OpenOnAmendment applies TRD's OrderAmended e with b, read before the transaction: it opens a
// pending check for the amended revision through trade_compliance.reopen (cause order_amended, the
// event ID as idempotency key), linked to the check of the order's previous revision, which stays
// as it is. Nothing is opened when the revision has a check already, when TRD's order ended or
// moved to a later revision since. An order TRD does not show intact at e's binding is a
// *FollowMismatch.
func (s Service) OpenOnAmendment(ctx context.Context, exec bentopg.Executor, e AmendedOrder, b OrderBasis) (string, error) {
	c, err := postgres.LoadCheckOfRevision(ctx, exec, scope(ctx), e.Binding.OrderID, e.Binding.OrderRevision, postgres.ForUpdate)
	if err != nil {
		return "", err
	}
	if c != nil {
		return EffectCheckExists, nil
	}
	o := b.Order
	switch {
	case o == nil:
		return "", &FollowMismatch{Code: FailureOrderNotCheckable, Detail: fmt.Sprintf("TRD shows no order %s", e.Binding.OrderID)}
	case o.Code != "":
		return "", &FollowMismatch{Code: FailureOrderNotCheckable, Detail: fmt.Sprintf("TRD's order %s revision %d is not intact (%s)",
			e.Binding.OrderID, o.Binding.OrderRevision, o.Code)}
	case o.Status != OrderDraft && o.Status != OrderActive:
		return EffectOrderEnded, nil
	case o.Binding.OrderRevision > e.Binding.OrderRevision:
		return EffectOrderAmendedAgain, nil
	case o.Binding != e.Binding:
		return "", &FollowMismatch{Code: FailureOrderNotCheckable, Detail: fmt.Sprintf("TRD's order %s is at revision %d hash %s, the event says %d %s",
			e.Binding.OrderID, o.Binding.OrderRevision, o.Binding.OrderHash, e.Binding.OrderRevision, e.Binding.OrderHash)}
	}
	cmd := api.Reopen{IdempotencyKey: followKey(e.EventID, ""), Cause: api.CauseOrderAmended, OrderID: e.Binding.OrderID,
		OrderRevision: e.Binding.OrderRevision, Reason: fmt.Sprintf("TRD amended order %s to revision %d (%s %s): the new revision needs its own "+
			"trade compliance conclusion (PL-14)", e.Binding.OrderID, e.Binding.OrderRevision, e.EventType, e.EventID)}
	if err = s.followReopen(ctx, cmd, &reopenCommand{s: s, cmd: cmd, order: o}); err != nil {
		return "", err
	}
	return EffectCheckOpened, nil
}

// PublishedRegulation is POL's ConfigPublished as CNX reads it.
type PublishedRegulation struct {
	EventID   string
	EventType string
	Kind      string
	Ref       polapi.ConfigRef
}

// RegulationBasis is the published screening_regulation revision as POL resolved it before the
// consumer's transaction (nil: another kind, or POL resolves none).
type RegulationBasis struct {
	Regulation *polapi.ResolvedConfig
}

// PrepareRegulation reads, outside any transaction, the published revision of a
// screening_regulation as POL resolves it now (ConfigRef v1, R50); another kind needs nothing. A
// failure is retried with the event.
func (s Service) PrepareRegulation(ctx context.Context, e PublishedRegulation) (RegulationBasis, error) {
	if e.Kind != polapi.KindScreeningRegulation {
		return RegulationBasis{}, nil
	}
	resolved, err := s.Upstream.Configs.ResolveConfigs(ctx, []polapi.ConfigRef{e.Ref}, s.Clock.Now())
	if err != nil {
		return RegulationBasis{}, err
	}
	if r, ok := resolved[e.Ref]; ok {
		return RegulationBasis{Regulation: &r}, nil
	}
	return RegulationBasis{}, nil
}

// ReopenOnRegulation applies POL's ConfigPublished e with b: when e published a screening_regulation
// revision that is in force, every clear conclusion that follows an earlier revision of that
// regulation is reopened (trade_compliance.reopen, cause regulation_revised, key event ID and
// check). A revision no longer in force reopens nothing: the revision that replaced it was
// published after it, and its event reopens what follows the earlier ones.
func (s Service) ReopenOnRegulation(ctx context.Context, exec bentopg.Executor, e PublishedRegulation, b RegulationBasis) (string, []string, error) {
	if e.Kind != polapi.KindScreeningRegulation {
		return EffectNotScreeningRegulation, nil, nil
	}
	if r := b.Regulation; r == nil || r.Ref != e.Ref || r.Data.ScreeningRegulation == nil || r.Code != "" {
		return EffectRegulationNotInForce, nil, nil
	}
	rows, err := postgres.LockClearBeforeRevision(ctx, exec, scope(ctx), e.Ref.ConfigID, e.Ref.Revision)
	if err != nil {
		return "", nil, err
	}
	var reopened []string
	for _, row := range rows {
		c := row.Check
		cmd := api.Reopen{IdempotencyKey: followKey(e.EventID, c.ID), Cause: api.CauseRegulationRevised, CheckID: c.ID, ExpectedVersion: c.RowVersion,
			RegulationID: e.Ref.ConfigID, RegulationRevision: e.Ref.Revision, Reason: fmt.Sprintf("POL published screening_regulation %s "+
				"revision %d (%s %s); the conclusion follows revision %d", e.Ref.ConfigID, e.Ref.Revision, e.EventType, e.EventID,
				row.Regulation.Revision)}
		if err = s.followReopen(ctx, cmd, &reopenCommand{s: s, cmd: cmd, regulation: b.Regulation}); err != nil {
			return "", nil, err
		}
		reopened = append(reopened, c.ID)
	}
	return EffectReopened, reopened, nil
}

// ImportedList is reference data's CountryProgrammesImported as CNX reads it.
type ImportedList struct {
	EventID   string
	EventType string
	ListCode  string
	Version   string
}

// ReopenOnProgrammes applies reference data's CountryProgrammesImported e: every clear conclusion
// whose evaluation used list e.ListCode at a version that is not the list's current one is
// reopened (trade_compliance.reopen, cause programme_updated, key event ID and check), whichever
// import made it outdated — an import delivered after a later one reopens what the later one left.
func (s Service) ReopenOnProgrammes(ctx context.Context, exec bentopg.Executor, e ImportedList) ([]string, error) {
	rows, err := postgres.LockClearOnOutdatedList(ctx, exec, scope(ctx), e.ListCode)
	if err != nil {
		return nil, err
	}
	var reopened []string
	for _, row := range rows {
		c := row.Check
		cmd := api.Reopen{IdempotencyKey: followKey(e.EventID, c.ID), Cause: api.CauseProgrammeUpdated, CheckID: c.ID, ExpectedVersion: c.RowVersion,
			Reason: fmt.Sprintf("Reference data imported version %s of country programme list %s (%s %s); the conclusion used another version",
				e.Version, e.ListCode, e.EventType, e.EventID)}
		if err = s.followReopen(ctx, cmd, &reopenCommand{s: s, cmd: cmd}); err != nil {
			return nil, err
		}
		reopened = append(reopened, c.ID)
	}
	return reopened, nil
}

// followReopen runs reopen h, whose upstream reads were made before the consumer's transaction, in
// that transaction. A refusal a later delivery can pass — the consumer's actor not provisioned
// yet, a concurrent change of the check, a POL revision superseded or changed since it was read —
// is returned to be retried; any other refusal is a *FollowMismatch.
func (s Service) followReopen(ctx context.Context, cmd api.Reopen, h *reopenCommand) error {
	object := cmd.CheckID
	if object == "" {
		object = cmd.OrderID
	}
	_, err := s.run(ctx, action.Command{Name: api.CommandReopen, IdempotencyKey: cmd.IdempotencyKey, ObjectID: object, Reason: cmd.Reason},
		cmd, cmd.Validate(), prepared{h})
	if rej, ok := kernel.AsRejection(err); ok && !retried(rej.Code) {
		return &FollowMismatch{Code: FailureReopenRefused, Detail: fmt.Sprintf("trade_compliance.reopen %s of %s: %s", cmd.Cause, object, rej.Error())}
	}
	return err
}

// retried reports whether a refusal of trade_compliance.reopen can pass on a later delivery.
func retried(code string) bool {
	switch code {
	case kernel.CodePermissionDenied, kernel.CodeVersionConflict, api.CodeReferenceStale, polapi.CodeConfigNotActive, polapi.CodeConfigExpired:
		return true
	}
	return false
}

// followKey is the idempotency key of a consumer's reopening: the event ID, and for one of several
// checks a short hash of the check ID; a digest when that is no identifier.
func followKey(eventID, checkID string) string {
	key := eventID
	if checkID != "" {
		sum := sha256.Sum256([]byte(checkID))
		key += ":" + hex.EncodeToString(sum[:8])
	}
	if kernel.ValidIdentifier(key) {
		return key
	}
	sum := sha256.Sum256([]byte(eventID + "\x00" + checkID))
	return hex.EncodeToString(sum[:])
}

// prepared is a command handler whose upstream reads were made before the transaction it runs in:
// it hides the handler's Prepare from the ledger.
type prepared struct{ h action.Handler }

func (p prepared) Lock(ctx context.Context, exec bentopg.Executor) error { return p.h.Lock(ctx, exec) }

func (p prepared) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return p.h.Apply(ctx, exec, d)
}

// ChecksByRegulation returns the clear conclusions that follow screening_regulation regulationID
// (at revision when positive) the caller sees, with their state now (trade_compliance.by_regulation,
// cnx.read).
func (s Service) ChecksByRegulation(ctx context.Context, regulationID string, revision int64) ([]api.ClearCheck, error) {
	if len(regulationID) > api.MaxRegulationIDLen || !kernel.ValidIdentifier(regulationID) {
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	}
	if revision < 0 {
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/revision", "")
	}
	return s.clearChecks(ctx, api.CommandChecksByRegulation, regulationID, func(ctx context.Context, exec bentopg.Executor) ([]postgres.ClearRow, error) {
		return postgres.ClearByRegulation(ctx, exec, scope(ctx), regulationID, revision)
	})
}

// ChecksByList returns the clear conclusions whose evaluation used programme list listCode (at
// version when not empty) the caller sees, with their state now (trade_compliance.by_list,
// cnx.read).
func (s Service) ChecksByList(ctx context.Context, listCode, version string) ([]api.ClearCheck, error) {
	if len(listCode) > api.MaxListCodeLength || !kernel.ValidIdentifier(listCode) {
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	}
	if version != "" && !refapi.ValidVersion(version) {
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/version", "")
	}
	return s.clearChecks(ctx, api.CommandChecksByList, listCode, func(ctx context.Context, exec bentopg.Executor) ([]postgres.ClearRow, error) {
		return postgres.ClearByList(ctx, exec, scope(ctx), listCode, version)
	})
}

func (s Service) clearChecks(ctx context.Context, command, object string,
	find func(ctx context.Context, exec bentopg.Executor) ([]postgres.ClearRow, error)) ([]api.ClearCheck, error) {
	out := []api.ClearCheck{}
	err := s.Ledger.Read(ctx, command, object, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		rows, err := find(ctx, exec)
		if err != nil {
			return err
		}
		now := kernel.Instant(s.Clock.Now())
		for _, r := range rows {
			c := r.Check
			if !d.Sees(c.OwnerActor, c.OwnerTeam) {
				continue
			}
			state, err := postgres.StateOf(ctx, exec, c.Binding, now)
			if err != nil {
				return err
			}
			lists, err := postgres.ListsUsed(ctx, exec, scope(ctx), c.Evaluation)
			if err != nil {
				return err
			}
			v := api.ClearCheck{CheckID: c.ID, OrderID: c.Binding.OrderID, OrderRevision: c.Binding.OrderRevision, OrderHash: c.Binding.OrderHash,
				RowVersion: c.RowVersion, DecisionID: r.DecisionID, RegulationID: r.Regulation.ID, RegulationRevision: r.Regulation.Revision,
				Programmes: []api.ProgrammeView{}, LicenseRequired: r.LicenseRequired, ValidUntil: r.ValidUntil, State: state.Status,
				Reason: state.Reason, CheckedAt: now}
			for _, l := range lists {
				v.Programmes = append(v.Programmes, api.ProgrammeView{Code: l.ListCode, Jurisdiction: l.Jurisdiction, Version: l.Version,
					ContentHash: l.ContentHash})
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}
