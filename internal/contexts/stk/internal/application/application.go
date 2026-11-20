// Package application runs STK's commands on the command ledger (WBS S6-01, S6-D02).
//
// Lock order inside STK: the stock balance FOR UPDATE (several balances in key order), then the
// reservation FOR UPDATE when the command names one, then the company authority guard FOR SHARE
// (the ledger), then cost layers FOR UPDATE in FIFO order (received_at, layer_id). STK locks no
// row of FUL. PUR's ReceiptRequest v1 (R28) is read in Prepare, before the transaction.
package application

import (
	"context"
	"errors"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/stk/api"
	"go.idp.xyz/idp-brant/internal/contexts/stk/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stk/internal/domain/ledger"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	anyActor    = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}
	humanAI     = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}
	humanSystem = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorSystem}
)

// Rules is STK's authorization policy. stk.module.configure, stock.receive, stock.issue and
// stock.adjust are human only (S6-R0). reservation.place and reservation.release are human or AI.
// material.handover is in process for MFG, on the purchaser's stock.issue permission.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		authorization.HumanOnly(api.CommandConfigure, api.CommandConfigure),
		authorization.HumanOnly(api.CommandReceive, api.CommandReceive),
		authorization.HumanOnly(api.CommandIssue, api.CommandIssue),
		authorization.HumanOnly(api.CommandAdjust, api.CommandAdjust),
		{Command: api.CommandPlace, Permission: api.CommandPlace, ActorKinds: humanAI},
		{Command: api.CommandRelease, Permission: api.CommandRelease, ActorKinds: humanAI},
		{Command: api.CommandHandover, Permission: api.CommandIssue, ActorKinds: humanSystem},
		{Command: api.CommandGetSetting, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetBalance, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetReservation, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandListMovements, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandAudit, Permission: api.PermissionAudit, ActorKinds: anyActor},
	}
}

// Service runs STK's use cases. Receipts is PUR's ReceiptRequest v1 (R28); nil refuses a receipt
// that cites PUR.
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Receipts purapi.ReceiptRequests
}

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// Visible reports whether d's actor sees the object a stored result names.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	sc := scope(ctx)
	switch command {
	case api.CommandConfigure:
		st, err := postgres.LoadSetting(ctx, exec, sc, postgres.NoLock)
		return err == nil && st != nil && d.Sees(st.ActorID, ""), err
	case api.CommandPlace, api.CommandRelease:
		r, err := postgres.LoadReservation(ctx, exec, sc, id, postgres.NoLock)
		return err == nil && r != nil && d.Sees(r.OwnerActor, r.OwnerTeam), err
	default:
		m, err := postgres.LoadMovement(ctx, exec, sc, id)
		return err == nil && m != nil && d.Sees(m.OwnerActor, m.OwnerTeam), err
	}
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

// request returns the name, the idempotency key and the business arguments. The key and the
// expected version are not part of the request hash.
func request(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.ConfigureModule:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandConfigure, key, c
	case api.ReceiveStock:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandReceive, key, c
	case api.IssueStock:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandIssue, key, c
	case api.AdjustStock:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandAdjust, key, c
	case api.PlaceReservation:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandPlace, key, c
	case api.ReleaseReservation:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRelease, key, c
	case api.RecordHandover:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandHandover, key, c
	}
	return "", "", nil
}

// Reconcile reports what became of cmd without executing it.
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

// Audit returns STK's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, api.CommandAudit)
}

// AuditRejected records a refusal an adapter made before the command ran.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}

func (s Service) requireEnabled(ctx context.Context, exec bentopg.Executor) error {
	on, err := enabled(ctx, exec)
	if err != nil || on {
		return err
	}
	return api.Reject(api.CodeDependencyNotSupported, "/", "")
}

// moduleEnabled reads the company switch before a write. stock.receive calls it before PUR's
// ReceiptRequest v1, so a company that has not enabled STK is refused DEPENDENCY_NOT_SUPPORTED
// and the upstream lookup does not run (STK-INV-004). The read commits on its own; Apply checks
// the switch again inside the stock transaction.
func (s Service) moduleEnabled(ctx context.Context) (bool, error) {
	var on bool
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		on, err = enabled(ctx, exec)
		return err
	})
	return on, err
}

func enabled(ctx context.Context, exec bentopg.Executor) (bool, error) {
	st, err := postgres.LoadSetting(ctx, exec, scope(ctx), postgres.NoLock)
	if err != nil || st == nil {
		return false, err
	}
	return st.Enabled, nil
}

func stockErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ledger.ErrShort), errors.Is(err, ledger.ErrLayers):
		return api.Reject(api.CodeStockInsufficient, "/args/quantity", "")
	case errors.Is(err, ledger.ErrDirect):
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/source_kind", "")
	case errors.Is(err, ledger.ErrShipment):
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/source_kind", "")
	case errors.Is(err, ledger.ErrHold):
		return kernel.Reject(kernel.CodeInvalidState, "/args/source_kind", "")
	case errors.Is(err, ledger.ErrOrderHold):
		return api.Reject(api.CodeStockReservationConflict, "/args/reservation_id", "")
	case errors.Is(err, ledger.ErrSource):
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/source_kind", "")
	case errors.Is(err, ledger.ErrUnit), errors.Is(err, ledger.ErrCurrency):
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/unit_id", "")
	case errors.Is(err, postgres.ErrConflict):
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	default:
		return err
	}
}

func qtyOf(s string) (kernel.Decimal, error) {
	return kernel.ParseDecimal(s, kernel.QuantityFormat)
}

func minorOf(currency string, units int, amount string) (int64, error) {
	cur, err := kernel.NewCurrency(currency, units)
	if err != nil {
		return 0, err
	}
	m, err := kernel.ParseMinor(cur, amount)
	if err != nil {
		return 0, err
	}
	if m.Sign() < 0 || !m.Minor().IsInt64() {
		return 0, kernel.ErrAmountOutOfRange
	}
	return m.Minor().Int64(), nil
}

func instant(clock application.Clock, s string) (time.Time, error) {
	if s == "" {
		return clock.Now(), nil
	}
	t, err := kernel.ParseTimestamp(s)
	if err != nil {
		return time.Time{}, err
	}
	return kernel.Instant(t), nil
}

func textDec(d kernel.Decimal) string {
	s, ok := d.Exact()
	if !ok {
		return d.String()
	}
	return s
}

func actorOf(d authorization.Decision) (kind, id, team string, epoch int64) {
	return string(d.Actor.Kind), d.Actor.ID, d.Team, d.Epoch
}
