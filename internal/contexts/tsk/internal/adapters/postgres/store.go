// Package postgres stores TSK's business tasks and their history in schema brant_tsk, on the
// executor of TSK's own transaction.
package postgres

import (
	"context"
	"errors"
	"math/big"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is TSK's schema.
const Schema = "brant_tsk"

// ErrConflict means a compare-and-set update found another task version.
var ErrConflict = errors.New("tsk: task version changed")

// Lock is how a read locks the rows it returns.
type Lock string

const (
	NoLock    Lock = ""
	ForUpdate Lock = " FOR UPDATE"
)

func scope2(s kernel.Scope) (string, string) { return string(s.Tenant), string(s.Company) }

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = kernel.Instant(t)
	return &t
}

func nullItems(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	return items
}

const taskColumns = `task_id, goal_code, goal_version, lifecycle, control_mode, control_epoch, task_version, initiator_kind, initiator_id,
	owner_id, coalesce(owner_team_id, ''), coalesce(agent_id, ''), coalesce(target_rfq_id, ''), coalesce(target_quote_id, ''),
	coalesce(target_customer_id, ''), source_kind, source_id, deadline, budget_max_model_calls, budget_max_retries, budget_cost_currency,
	budget_cost_minor_units, budget_cost_minor::text, bundle_id, bundle_sha256, binding_quote_id, binding_state, binding_epoch, bound_at,
	released_at, wait_kind, wait_reason, wait_responsible_id, wait_items, awaiting_kind, awaiting_id, awaiting_version, wait_due_at,
	wait_since, pause_cause, pause_reason, pause_responsible_id, resume_condition, paused_at, coalesce(paused_from, ''), stop_purpose,
	stop_from, stop_reason,
	stop_by_kind, stop_by_id, stop_at, scope_settled_at, coalesce(completion_actor, ''), end_cause, end_reason, end_responsible_id,
	ended_by_kind, ended_by_id, ended_at, request_hash, created_at, updated_at`

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func at(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.UTC()
}

func scanTask(row pgx.Row) (*task.Task, error) {
	var t task.Task
	var state, mode, initiatorKind, pausedFrom string
	var sourceKind, sourceID, costCurrency, costMinor, bundleID, bundleSHA, bQuote, bState *string
	var deadline, boundAt, releasedAt *time.Time
	var maxCalls, maxRetries *int
	var minorUnits *int16
	var bEpoch *int64
	var waitKind, wReason, wResponsible, aKind, aID *string
	var items []string
	var aVersion *int64
	var dueAt, since *time.Time
	var pauseCause, pReason, pResponsible, resume *string
	var pausedAt *time.Time
	var stopPurpose, stopFrom, stopReason, stopKind, stopID *string
	var stopAt, settled *time.Time
	var endCause, endReason, endResponsible, endKind, endID *string
	var endAt *time.Time
	err := row.Scan(&t.ID, &t.Goal, &t.GoalVersion, &state, &mode, &t.ControlEpoch, &t.Version, &initiatorKind, &t.Initiator.ID, &t.Owner,
		&t.OwnerTeam, &t.Agent, &t.Target.RfqID, &t.Target.QuoteID, &t.Target.CustomerID, &sourceKind, &sourceID, &deadline, &maxCalls,
		&maxRetries, &costCurrency, &minorUnits, &costMinor, &bundleID, &bundleSHA, &bQuote, &bState, &bEpoch, &boundAt, &releasedAt,
		&waitKind, &wReason, &wResponsible, &items, &aKind, &aID, &aVersion, &dueAt, &since, &pauseCause, &pReason, &pResponsible, &resume,
		&pausedAt, &pausedFrom,
		&stopPurpose, &stopFrom, &stopReason, &stopKind, &stopID, &stopAt, &settled, &t.CompletionActor, &endCause, &endReason,
		&endResponsible, &endKind, &endID, &endAt, &t.RequestHash, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	t.State, t.Mode, t.PausedFrom = task.State(state), task.Mode(mode), task.State(pausedFrom)
	t.Initiator.Kind = kernel.ActorKind(initiatorKind)
	t.Deadline, t.ScopeSettledAt = at(deadline), at(settled)
	t.CreatedAt, t.UpdatedAt = t.CreatedAt.UTC(), t.UpdatedAt.UTC()
	if sourceKind != nil {
		t.Source = &task.Source{Kind: *sourceKind, ID: str(sourceID)}
	}
	if maxCalls != nil {
		t.Budget = &task.Budget{MaxModelCalls: *maxCalls, MaxRetries: *maxRetries}
		if costCurrency != nil {
			cur, err := kernel.NewCurrency(*costCurrency, int(*minorUnits))
			if err != nil {
				return nil, err
			}
			minor, ok := new(big.Int).SetString(*costMinor, 10)
			if !ok {
				return nil, errors.New("tsk: stored cost cap " + *costMinor)
			}
			m, err := kernel.MoneyFromMinor(cur, minor)
			if err != nil {
				return nil, err
			}
			t.Budget.MaxCost = &m
		}
	}
	if bundleID != nil {
		t.Bundle = &task.Bundle{ID: *bundleID, SHA256: str(bundleSHA)}
	}
	if bQuote != nil {
		t.Binding = &task.Binding{QuoteID: *bQuote, State: str(bState), Epoch: *bEpoch, BoundAt: at(boundAt), ReleasedAt: at(releasedAt)}
	}
	if waitKind != nil {
		c := &task.WaitCondition{Kind: *waitKind, Reason: str(wReason), Responsible: str(wResponsible), Items: items, DueAt: at(dueAt),
			Since: at(since)}
		if aKind != nil {
			c.Awaiting = &task.Ref{Kind: *aKind, ID: str(aID), Version: *aVersion}
		}
		t.WaitCond = c
	}
	if pauseCause != nil {
		t.PauseCond = &task.PauseCondition{Cause: *pauseCause, Reason: str(pReason), Responsible: str(pResponsible), ResumeCondition: str(resume),
			Since: at(pausedAt)}
	}
	if stopPurpose != nil {
		t.Stop = &task.Stop{Purpose: *stopPurpose, From: task.State(str(stopFrom)), Reason: str(stopReason),
			By: kernel.ActorRef{Kind: kernel.ActorKind(str(stopKind)), ID: str(stopID)}, At: at(stopAt)}
	}
	if endCause != nil {
		t.Termination = &task.Termination{Cause: *endCause, Reason: str(endReason), Responsible: str(endResponsible),
			By: kernel.ActorRef{Kind: kernel.ActorKind(str(endKind)), ID: str(endID)}, At: at(endAt)}
	}
	return &t, nil
}

// LoadTask returns task id of scope, or nil.
func LoadTask(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*task.Task, error) {
	tenant, company := scope2(scope)
	t, err := scanTask(exec.QueryRow(ctx, `SELECT `+taskColumns+` FROM brant_tsk.business_task
		WHERE tenant_id = $1 AND company_id = $2 AND task_id = $3`+string(lock), tenant, company, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// OpenTaskFor returns the ID of the open (non-terminal) task that serves goal for source, or "".
func OpenTaskFor(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, goal string, source task.Source) (string, error) {
	tenant, company := scope2(scope)
	var id string
	err := exec.QueryRow(ctx, `SELECT task_id FROM brant_tsk.business_task WHERE tenant_id = $1 AND company_id = $2 AND goal_code = $3
		AND source_kind = $4 AND source_id = $5 AND lifecycle NOT IN ('completed', 'closed_unfulfilled', 'cancelled', 'failed')`,
		tenant, company, goal, source.Kind, source.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// row is the mutable part of a task as stored.
func row(t *task.Task) []any {
	var bQuote, bState *string
	var bEpoch *int64
	var boundAt, releasedAt *time.Time
	if b := t.Binding; b != nil {
		bQuote, bState, bEpoch, boundAt, releasedAt = &b.QuoteID, &b.State, &b.Epoch, nullTime(b.BoundAt), nullTime(b.ReleasedAt)
	}
	var waitKind, wReason, wResponsible, aKind, aID *string
	var items []string
	var aVersion *int64
	var dueAt, since *time.Time
	if c := t.WaitCond; c != nil {
		waitKind, wReason, wResponsible = &c.Kind, &c.Reason, &c.Responsible
		items, dueAt, since = nullItems(c.Items), nullTime(c.DueAt), nullTime(c.Since)
		if a := c.Awaiting; a != nil {
			aKind, aID, aVersion = &a.Kind, &a.ID, &a.Version
		}
	}
	var pauseCause, pReason, pResponsible, resume *string
	var pausedAt *time.Time
	if p := t.PauseCond; p != nil {
		pauseCause, pReason, pResponsible, resume, pausedAt = &p.Cause, &p.Reason, &p.Responsible, &p.ResumeCondition, nullTime(p.Since)
	}
	var stopPurpose, stopFrom, stopReason, stopKind, stopID *string
	var stopAt *time.Time
	if s := t.Stop; s != nil {
		from, kind := string(s.From), string(s.By.Kind)
		stopPurpose, stopFrom, stopReason, stopKind, stopID, stopAt = &s.Purpose, &from, &s.Reason, &kind, &s.By.ID, nullTime(s.At)
	}
	var endCause, endReason, endResponsible, endKind, endID *string
	var endAt *time.Time
	if e := t.Termination; e != nil {
		kind := string(e.By.Kind)
		endCause, endReason, endResponsible, endKind, endID, endAt = &e.Cause, &e.Reason, nullable(e.Responsible), &kind, &e.By.ID, nullTime(e.At)
	}
	return []any{string(t.State), string(t.Mode), t.ControlEpoch, t.Version, nullable(t.Target.QuoteID), bQuote, bState, bEpoch, boundAt,
		releasedAt, waitKind, wReason, wResponsible, items, aKind, aID, aVersion, dueAt, since, pauseCause, pReason, pResponsible, resume,
		pausedAt, nullable(string(t.PausedFrom)), stopPurpose, stopFrom, stopReason, stopKind, stopID, stopAt, nullTime(t.ScopeSettledAt),
		nullable(t.CompletionActor), endCause, endReason, endResponsible, endKind, endID, endAt, kernel.Instant(t.UpdatedAt)}
}

const mutableColumns = `lifecycle, control_mode, control_epoch, task_version, target_quote_id, binding_quote_id, binding_state,
	binding_epoch, bound_at, released_at, wait_kind, wait_reason, wait_responsible_id, wait_items, awaiting_kind, awaiting_id,
	awaiting_version, wait_due_at, wait_since, pause_cause, pause_reason, pause_responsible_id, resume_condition, paused_at, paused_from,
	stop_purpose, stop_from, stop_reason, stop_by_kind, stop_by_id, stop_at, scope_settled_at, completion_actor, end_cause, end_reason,
	end_responsible_id, ended_by_kind, ended_by_id, ended_at, updated_at`

// mutableCount is the number of mutable columns, which row returns in their order.
var mutableCount = len(splitColumns(mutableColumns))

// InsertTask writes a new task and its first history row.
func InsertTask(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, t *task.Task, ch task.Change) error {
	tenant, company := scope2(scope)
	var sourceKind, sourceID *string
	if s := t.Source; s != nil {
		sourceKind, sourceID = &s.Kind, &s.ID
	}
	var maxCalls, maxRetries *int
	var costCurrency, costMinor *string
	var minorUnits *int
	if b := t.Budget; b != nil {
		maxCalls, maxRetries = &b.MaxModelCalls, &b.MaxRetries
		if m := b.MaxCost; m != nil {
			code, units, minor := m.Currency().Code(), m.Currency().MinorUnits(), m.MinorString()
			costCurrency, minorUnits, costMinor = &code, &units, &minor
		}
	}
	var bundleID, bundleSHA *string
	if b := t.Bundle; b != nil {
		bundleID, bundleSHA = &b.ID, &b.SHA256
	}
	args := append([]any{tenant, company, t.ID, t.Goal, t.GoalVersion, string(t.Initiator.Kind), t.Initiator.ID, t.Owner, nullable(t.OwnerTeam),
		nullable(t.Agent), nullable(t.Target.RfqID), nullable(t.Target.CustomerID), sourceKind, sourceID, nullTime(t.Deadline), maxCalls,
		maxRetries, costCurrency, minorUnits, costMinor, bundleID, bundleSHA, t.RequestHash, kernel.Instant(t.CreatedAt)}, row(t)...)
	_, err := exec.Exec(ctx, `INSERT INTO brant_tsk.business_task (tenant_id, company_id, task_id, goal_code, goal_version, initiator_kind,
		initiator_id, owner_id, owner_team_id, agent_id, target_rfq_id, target_customer_id, source_kind, source_id, deadline,
		budget_max_model_calls, budget_max_retries, budget_cost_currency, budget_cost_minor_units, budget_cost_minor, bundle_id, bundle_sha256,
		request_hash, created_at, `+mutableColumns+`) VALUES (`+placeholders(1, 24+mutableCount)+`)`, args...)
	if err != nil {
		return err
	}
	return insertChange(ctx, exec, scope, t.ID, ch)
}

// SaveTask writes t's changes if the stored task is still at version from (ErrConflict otherwise)
// and appends ch to its history.
func SaveTask(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, t *task.Task, from int64, ch task.Change) error {
	tenant, company := scope2(scope)
	set := ""
	for i, c := range splitColumns(mutableColumns) {
		if i > 0 {
			set += ", "
		}
		set += c + " = $" + strconv.Itoa(i+4)
	}
	args := append([]any{tenant, company, t.ID}, row(t)...)
	args = append(args, from)
	tag, err := exec.Exec(ctx, `UPDATE brant_tsk.business_task SET `+set+` WHERE tenant_id = $1 AND company_id = $2 AND task_id = $3
		AND task_version = $`+strconv.Itoa(4+mutableCount), args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return insertChange(ctx, exec, scope, t.ID, ch)
}

func insertChange(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, ch task.Change) error {
	tenant, company := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_tsk.task_transition (tenant_id, company_id, task_id, task_version, command, from_state,
		to_state, control_mode, control_epoch, control_change, actor_kind, actor_id, authority_epoch, recorded_at, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		tenant, company, id, ch.Version, string(ch.Command), nullable(string(ch.From)), string(ch.To), string(ch.Mode), ch.ControlEpoch,
		nullable(ch.ControlChange), string(ch.By.Kind), ch.By.ID, ch.Epoch, kernel.Instant(ch.At), nullable(ch.Reason))
	return err
}

// LoadHistory returns task id's history, oldest first.
func LoadHistory(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]task.Change, error) {
	tenant, company := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT task_version, command, coalesce(from_state, ''), to_state, control_mode, control_epoch,
		coalesce(control_change, ''), actor_kind, actor_id, authority_epoch, recorded_at, coalesce(reason, '')
		FROM brant_tsk.task_transition WHERE tenant_id = $1 AND company_id = $2 AND task_id = $3 ORDER BY task_version`, tenant, company, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (task.Change, error) {
		var ch task.Change
		var command, from, to, mode, kind string
		err := r.Scan(&ch.Version, &command, &from, &to, &mode, &ch.ControlEpoch, &ch.ControlChange, &kind, &ch.By.ID, &ch.Epoch, &ch.At,
			&ch.Reason)
		ch.Command, ch.From, ch.To, ch.Mode, ch.By.Kind, ch.At = task.Command(command), task.State(from), task.State(to), task.Mode(mode),
			kernel.ActorKind(kind), ch.At.UTC()
		return ch, err
	})
}

func placeholders(from, n int) string {
	s := ""
	for i := range n {
		if i > 0 {
			s += ", "
		}
		s += "$" + strconv.Itoa(from+i)
	}
	return s
}

func splitColumns(cols string) []string {
	var out []string
	cur := ""
	for _, r := range cols {
		switch r {
		case ',':
			out = append(out, cur)
			cur = ""
		case ' ', '\n', '\t':
		default:
			cur += string(r)
		}
	}
	return append(out, cur)
}
