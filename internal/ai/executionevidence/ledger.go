package executionevidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai/codes"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Kinds of a ledger row.
const (
	KindModel = "model"
	KindTool  = "tool"
)

// States of a ledger row.
const (
	StateReserved    = "reserved"
	StateSucceeded   = "succeeded"
	StateFailed      = "failed"
	StateUnavailable = "unavailable"
	StateUnknown     = "unknown"
)

// Reservation asks for one model or tool call of a task before it is made. Retry marks a call that
// repeats an unavailable one (it counts against MaxRetries). A model call names its profile and the
// SHA-256 of the prompt template; a tool call the command the model asked for ("invalid" when the
// name is not shaped like a command).
type Reservation struct {
	TaskID      string
	TaskVersion int64
	Kind        string
	Retry       bool
	Bundle      Ref
	Profile     *ModelProfile // model calls
	PromptSHA   string        // model calls
	ToolCommand string        // tool calls
}

// Outcome settles a reserved call. A model call reports what the provider returned; Cost is the
// actual cost (nil: unknown, then State must be unknown). Code is the refusal or failure code
// (failed, unavailable).
type Outcome struct {
	State         string
	ModelReturned string
	InputSHA256   string
	OutputSHA256  string
	InputTokens   *int64
	OutputTokens  *int64
	Latency       time.Duration
	FinishReason  string
	ActionID      string
	Code          string
	Cost          *kernel.Money
}

// Call is one row of the ledger.
type Call struct {
	CallID         string
	TaskID         string
	Seq            int
	Kind           string
	Retry          bool
	State          string
	ActorID        string
	TaskVersion    int64
	Bundle         Ref
	ProfileID      string
	Provider       string
	ModelRequested string
	PromptSHA256   string
	ToolCommand    string
	Reserved       kernel.Money
	ReservedAt     time.Time
	ModelReturned  string
	InputSHA256    string
	OutputSHA256   string
	InputTokens    *int64
	OutputTokens   *int64
	LatencyMS      *int64
	FinishReason   string
	ActionID       string
	Code           string
	Cost           *kernel.Money
	SettledAt      time.Time
}

// Spend is what a task has used of its budget: its limits, the model calls, tool calls and retries
// reserved so far (whatever their outcome), the settled cost and the cost still reserved by calls
// not yet settled or of unknown outcome.
type Spend struct {
	Limits     Limits
	ModelCalls int
	ToolCalls  int
	Retries    int
	Settled    kernel.Money
	Open       kernel.Money
	Tokens     int64
}

// Total returns the cost counted against the budget: settled plus open.
func (s Spend) Total() kernel.Money {
	t, _ := s.Settled.Add(s.Open)
	return t
}

// Ledger records bundles, budgets and calls in brant_ai, on the AI layer's transaction owner, in the
// scope of the Principal of each call.
type Ledger struct {
	DB    *bentopg.DB
	Clock application.Clock
}

func (l Ledger) within(ctx context.Context, fn func(context.Context, bentopg.Executor, identity.Principal) error) error {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return err
	}
	return policy.WithinScope(ctx, l.DB, p.Scope, func(ctx context.Context, exec bentopg.Executor) error {
		return fn(ctx, exec, p)
	})
}

// PutBundle records b in the caller's scope once and returns its reference; recording the same
// bundle again changes nothing.
func (l Ledger) PutBundle(ctx context.Context, b Bundle) (Ref, error) {
	raw, err := b.Canonical()
	if err != nil {
		return Ref{}, err
	}
	ref, err := b.Ref()
	if err != nil {
		return Ref{}, err
	}
	err = l.within(ctx, func(ctx context.Context, exec bentopg.Executor, p identity.Principal) error {
		_, err := exec.Exec(ctx, `INSERT INTO brant_ai.execution_bundle (tenant_id, company_id, bundle_id, sha256, content, recorded_by_id,
			recorded_at) VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`, string(p.Scope.Tenant), string(p.Scope.Company),
			ref.BundleID, ref.SHA256, string(raw), p.Actor.ID, kernel.Instant(l.Clock.Now()))
		return err
	})
	return ref, err
}

// GetBundle returns the bundle ref names as recorded in the caller's scope, or nil.
func (l Ledger) GetBundle(ctx context.Context, ref Ref) (*Bundle, error) {
	var out *Bundle
	err := l.within(ctx, func(ctx context.Context, exec bentopg.Executor, p identity.Principal) error {
		var err error
		out, err = LoadBundle(ctx, exec, p.Scope, ref)
		return err
	})
	return out, err
}

// LoadBundle reads a recorded bundle through exec; nil when there is none. The content's SHA-256 is
// checked against its reference.
func LoadBundle(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ref Ref) (*Bundle, error) {
	var raw string
	err := exec.QueryRow(ctx, `SELECT content::text FROM brant_ai.execution_bundle WHERE tenant_id = $1 AND company_id = $2
		AND bundle_id = $3 AND sha256 = $4`, string(scope.Tenant), string(scope.Company), ref.BundleID, ref.SHA256).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b Bundle
	if err = json.Unmarshal([]byte(raw), &b); err != nil {
		return nil, err
	}
	got, err := b.Ref()
	if err != nil {
		return nil, fmt.Errorf("executionevidence: bundle %s/%s: %w", ref.BundleID, ref.SHA256, err)
	}
	if got != ref {
		return nil, fmt.Errorf("executionevidence: bundle %s/%s does not hash to its reference", ref.BundleID, ref.SHA256)
	}
	return &b, nil
}

// Reserve records a call of a task before it is made, under the task's budget (AI-INV-003; baseline
// 17.4): the budget row is created with limits on the task's first reservation and locked FOR UPDATE
// for every later one, so concurrent reservations of a task are counted one after the other and
// never exceed it. Refusals, BUDGET_EXHAUSTED at the limit that is used up:
//
//   - a model call beyond max_model_calls (/budget/max_model_calls);
//   - a retry beyond max_retries (/budget/max_retries);
//   - a reservation that would take the settled and open cost beyond the cost cap
//     (/budget/max_cost), or a model whose prices are in another currency (/budget/max_cost/currency).
//
// The caller's actor is recorded as the call's actor.
func (l Ledger) Reserve(ctx context.Context, limits Limits, r Reservation) (Call, error) {
	if err := r.check(); err != nil {
		return Call{}, err
	}
	reserve := zero(limits.Currency)
	if r.Kind == KindModel {
		var err error
		if reserve, err = r.Profile.Reservation(limits.Currency); err != nil {
			return Call{}, codes.Reject(codes.BudgetExhausted, "/budget/max_cost/currency")
		}
	}
	var out Call
	err := l.within(ctx, func(ctx context.Context, exec bentopg.Executor, p identity.Principal) error {
		stored, err := lockBudget(ctx, exec, p.Scope, r.TaskID, limits, l.Clock.Now())
		if err != nil {
			return err
		}
		spend, seq, err := spendOf(ctx, exec, p.Scope, r.TaskID, stored)
		if err != nil {
			return err
		}
		switch {
		case r.Kind == KindModel && spend.ModelCalls+1 > stored.MaxModelCalls:
			return codes.Reject(codes.BudgetExhausted, "/budget/max_model_calls")
		case r.Retry && spend.Retries+1 > stored.MaxRetries:
			return codes.Reject(codes.BudgetExhausted, "/budget/max_retries")
		}
		if stored.MaxCost != nil {
			after, err := spend.Total().Add(reserve)
			if err != nil {
				return err
			}
			if c, err := after.Cmp(*stored.MaxCost); err != nil || c > 0 {
				return codes.Reject(codes.BudgetExhausted, "/budget/max_cost")
			}
		}
		out = Call{CallID: CallID(r.TaskID, seq+1), TaskID: r.TaskID, Seq: seq + 1, Kind: r.Kind, Retry: r.Retry, State: StateReserved,
			ActorID: p.Actor.ID, TaskVersion: r.TaskVersion, Bundle: r.Bundle, ToolCommand: r.ToolCommand, Reserved: reserve,
			ReservedAt: kernel.Instant(l.Clock.Now())}
		if r.Kind == KindModel {
			out.ProfileID, out.Provider, out.ModelRequested, out.PromptSHA256 = r.Profile.ProfileID, r.Profile.Provider, r.Profile.Model, r.PromptSHA
		}
		_, err = exec.Exec(ctx, `INSERT INTO brant_ai.model_call (tenant_id, company_id, call_id, task_id, seq, kind, retry, state, actor_id,
			task_version, bundle_id, bundle_sha256, profile_id, provider, model_requested, prompt_sha256, tool_command, currency, minor_units,
			reserved_minor, reserved_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, nullif($13, ''), nullif($14, ''),
			nullif($15, ''), nullif($16, ''), nullif($17, ''), $18, $19, $20::numeric, $21)`,
			string(p.Scope.Tenant), string(p.Scope.Company), out.CallID, out.TaskID, out.Seq, out.Kind, out.Retry, out.State, out.ActorID,
			out.TaskVersion, out.Bundle.BundleID, out.Bundle.SHA256, out.ProfileID, out.Provider, out.ModelRequested, out.PromptSHA256,
			out.ToolCommand, reserve.Currency().Code(), reserve.Currency().MinorUnits(), reserve.MinorString(), out.ReservedAt)
		return err
	})
	return out, err
}

func (r Reservation) check() error {
	bad := func(what string) error { return fmt.Errorf("%w: reservation: %s", ErrInvalid, what) }
	switch {
	case !kernel.ValidIdentifier(r.TaskID) || r.TaskVersion < 1:
		return bad("task")
	case !kernel.ValidIdentifier(r.Bundle.BundleID) || !hex64(r.Bundle.SHA256):
		return bad("bundle")
	case r.Kind == KindModel && (r.Profile == nil || !hex64(r.PromptSHA) || r.ToolCommand != ""):
		return bad("a model call names its profile and prompt template")
	case r.Kind == KindTool && (r.Profile != nil || r.ToolCommand == ""):
		return bad("a tool call names its command")
	case r.Kind != KindModel && r.Kind != KindTool:
		return bad("kind")
	}
	if r.Profile != nil {
		return r.Profile.Validate()
	}
	return nil
}

// CallID returns the ID of the seq-th call of task.
func CallID(task string, seq int) string {
	sum := sha256.Sum256([]byte("brant.ai.call\x1f" + task + "\x1f" + strconv.Itoa(seq)))
	return "aic." + hex.EncodeToString(sum[:20])
}

// Settle records the outcome of a reserved call (or of one whose outcome was unknown) once.
func (l Ledger) Settle(ctx context.Context, callID string, o Outcome) (Call, error) {
	if err := o.check(); err != nil {
		return Call{}, err
	}
	var out Call
	err := l.within(ctx, func(ctx context.Context, exec bentopg.Executor, p identity.Principal) error {
		calls, err := loadCalls(ctx, exec, p.Scope, `call_id = $3 FOR UPDATE`, callID)
		if err != nil {
			return err
		}
		if len(calls) == 0 {
			return fmt.Errorf("executionevidence: no call %s", callID)
		}
		c := calls[0]
		if c.State != StateReserved && c.State != StateUnknown {
			return fmt.Errorf("executionevidence: call %s is already %s", callID, c.State)
		}
		cost := ""
		if o.Cost != nil {
			if o.Cost.Currency() != c.Reserved.Currency() {
				return ErrCurrency
			}
			cost = o.Cost.MinorString()
		}
		var latency *int64
		if o.Latency > 0 || o.State == StateSucceeded {
			ms := o.Latency.Milliseconds()
			latency = &ms
		}
		now := kernel.Instant(l.Clock.Now())
		_, err = exec.Exec(ctx, `UPDATE brant_ai.model_call SET state = $4, model_returned = nullif($5, ''), input_sha256 = nullif($6, ''),
			output_sha256 = nullif($7, ''), input_tokens = $8, output_tokens = $9, latency_ms = $10, finish_reason = nullif($11, ''),
			action_id = nullif($12, ''), code = nullif($13, ''), cost_minor = nullif($14, '')::numeric, settled_at = $15
			WHERE tenant_id = $1 AND company_id = $2 AND call_id = $3`, string(p.Scope.Tenant), string(p.Scope.Company), callID,
			o.State, o.ModelReturned, o.InputSHA256, o.OutputSHA256, o.InputTokens, o.OutputTokens, latency, o.FinishReason, o.ActionID,
			o.Code, cost, now)
		if err != nil {
			return err
		}
		calls, err = loadCalls(ctx, exec, p.Scope, `call_id = $3`, callID)
		if err == nil {
			out = calls[0]
		}
		return err
	})
	return out, err
}

func (o Outcome) check() error {
	bad := func(what string) error { return fmt.Errorf("%w: outcome: %s", ErrInvalid, what) }
	switch o.State {
	case StateSucceeded, StateFailed, StateUnavailable:
		if o.Cost == nil {
			return bad("a known outcome has a known cost")
		}
	case StateUnknown:
		if o.Cost != nil {
			return bad("an unknown outcome has no cost")
		}
	default:
		return bad("state " + o.State)
	}
	if (o.State == StateFailed || o.State == StateUnavailable) && o.Code == "" {
		return bad("a failed or unavailable call has a code")
	}
	if o.Cost != nil && o.Cost.Sign() < 0 {
		return bad("negative cost")
	}
	return nil
}

// Calls returns the calls of a task in order.
func (l Ledger) Calls(ctx context.Context, taskID string) ([]Call, error) {
	var out []Call
	err := l.within(ctx, func(ctx context.Context, exec bentopg.Executor, p identity.Principal) error {
		var err error
		out, err = LoadCalls(ctx, exec, p.Scope, taskID)
		return err
	})
	return out, err
}

// LoadCalls reads the calls of a task through exec, in order.
func LoadCalls(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, taskID string) ([]Call, error) {
	return loadCalls(ctx, exec, scope, `task_id = $3 ORDER BY seq`, taskID)
}

// Spend returns what a task has used of its budget; ok is false before its first reservation.
func (l Ledger) Spend(ctx context.Context, taskID string) (Spend, bool, error) {
	var out Spend
	var ok bool
	err := l.within(ctx, func(ctx context.Context, exec bentopg.Executor, p identity.Principal) error {
		limits, found, err := loadBudget(ctx, exec, p.Scope, taskID, "")
		if err != nil || !found {
			return err
		}
		ok = true
		out, _, err = spendOf(ctx, exec, p.Scope, taskID, limits)
		return err
	})
	return out, ok, err
}

func zero(c kernel.Currency) kernel.Money {
	m, _ := kernel.NewMoney(c, 0)
	return m
}

// lockBudget creates the task's budget with limits when it has none and locks it FOR UPDATE; it
// returns the stored limits (the first reservation's).
func lockBudget(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, task string, limits Limits, now time.Time) (Limits, error) {
	if limits.MaxModelCalls < 1 || limits.MaxRetries < 0 || limits.Currency.IsZero() || (limits.Source != "task" && limits.Source != "runner") {
		return Limits{}, fmt.Errorf("%w: limits %+v", ErrInvalid, limits)
	}
	maxCost := ""
	if limits.MaxCost != nil {
		if limits.MaxCost.Currency() != limits.Currency {
			return Limits{}, fmt.Errorf("%w: the cost cap is not in the budget's currency", ErrInvalid)
		}
		maxCost = limits.MaxCost.MinorString()
	}
	if _, err := exec.Exec(ctx, `INSERT INTO brant_ai.task_budget (tenant_id, company_id, task_id, max_model_calls, max_retries, currency,
		minor_units, max_cost_minor, source, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, '')::numeric, $9, $10)
		ON CONFLICT DO NOTHING`, string(scope.Tenant), string(scope.Company), task, limits.MaxModelCalls, limits.MaxRetries,
		limits.Currency.Code(), limits.Currency.MinorUnits(), maxCost, limits.Source, kernel.Instant(now)); err != nil {
		return Limits{}, err
	}
	stored, _, err := loadBudget(ctx, exec, scope, task, " FOR UPDATE")
	return stored, err
}

func loadBudget(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, task, lock string) (Limits, bool, error) {
	var l Limits
	var code string
	var units int
	var maxCost *string
	err := exec.QueryRow(ctx, `SELECT max_model_calls, max_retries, currency, minor_units, max_cost_minor::text, source
		FROM brant_ai.task_budget WHERE tenant_id = $1 AND company_id = $2 AND task_id = $3`+lock,
		string(scope.Tenant), string(scope.Company), task).Scan(&l.MaxModelCalls, &l.MaxRetries, &code, &units, &maxCost, &l.Source)
	if errors.Is(err, pgx.ErrNoRows) {
		return Limits{}, false, nil
	}
	if err != nil {
		return Limits{}, false, err
	}
	if l.Currency, err = kernel.NewCurrency(code, units); err != nil {
		return Limits{}, false, err
	}
	if maxCost != nil {
		m, err := minor(l.Currency, *maxCost)
		if err != nil {
			return Limits{}, false, err
		}
		l.MaxCost = &m
	}
	return l, true, nil
}

// spendOf sums a task's calls under limits and returns the highest sequence number.
func spendOf(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, task string, limits Limits) (Spend, int, error) {
	calls, err := LoadCalls(ctx, exec, scope, task)
	if err != nil {
		return Spend{}, 0, err
	}
	s := Spend{Limits: limits, Settled: zero(limits.Currency), Open: zero(limits.Currency)}
	seq := 0
	for _, c := range calls {
		seq = max(seq, c.Seq)
		switch c.Kind {
		case KindModel:
			s.ModelCalls++
		case KindTool:
			s.ToolCalls++
		}
		if c.Retry {
			s.Retries++
		}
		if c.InputTokens != nil {
			s.Tokens += *c.InputTokens
		}
		if c.OutputTokens != nil {
			s.Tokens += *c.OutputTokens
		}
		if c.Cost != nil {
			if s.Settled, err = s.Settled.Add(*c.Cost); err != nil {
				return Spend{}, 0, err
			}
		} else if s.Open, err = s.Open.Add(c.Reserved); err != nil {
			return Spend{}, 0, err
		}
	}
	return s, seq, nil
}

const selectCall = `SELECT call_id, task_id, seq, kind, retry, state, actor_id, task_version, bundle_id, bundle_sha256,
	coalesce(profile_id, ''), coalesce(provider, ''), coalesce(model_requested, ''), coalesce(prompt_sha256, ''), coalesce(tool_command, ''),
	currency, minor_units, reserved_minor::text, reserved_at, coalesce(model_returned, ''), coalesce(input_sha256, ''),
	coalesce(output_sha256, ''), input_tokens, output_tokens, latency_ms, coalesce(finish_reason, ''), coalesce(action_id, ''),
	coalesce(code, ''), cost_minor::text, settled_at
	FROM brant_ai.model_call WHERE tenant_id = $1 AND company_id = $2 AND `

func loadCalls(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, where string, arg string) ([]Call, error) {
	rows, err := exec.Query(ctx, selectCall+where, string(scope.Tenant), string(scope.Company), arg)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Call, error) {
		var c Call
		var code, reserved string
		var units int
		var cost *string
		var settled *time.Time
		err := row.Scan(&c.CallID, &c.TaskID, &c.Seq, &c.Kind, &c.Retry, &c.State, &c.ActorID, &c.TaskVersion, &c.Bundle.BundleID,
			&c.Bundle.SHA256, &c.ProfileID, &c.Provider, &c.ModelRequested, &c.PromptSHA256, &c.ToolCommand, &code, &units, &reserved,
			&c.ReservedAt, &c.ModelReturned, &c.InputSHA256, &c.OutputSHA256, &c.InputTokens, &c.OutputTokens, &c.LatencyMS,
			&c.FinishReason, &c.ActionID, &c.Code, &cost, &settled)
		if err != nil {
			return Call{}, err
		}
		cur, err := kernel.NewCurrency(code, units)
		if err != nil {
			return Call{}, err
		}
		if c.Reserved, err = minor(cur, reserved); err != nil {
			return Call{}, err
		}
		if cost != nil {
			m, err := minor(cur, *cost)
			if err != nil {
				return Call{}, err
			}
			c.Cost = &m
		}
		c.ReservedAt = c.ReservedAt.UTC()
		if settled != nil {
			c.SettledAt = settled.UTC()
		}
		return c, nil
	})
}

func minor(c kernel.Currency, s string) (kernel.Money, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return kernel.Money{}, fmt.Errorf("executionevidence: minor units %q", s)
	}
	return kernel.MoneyFromMinor(c, v)
}
