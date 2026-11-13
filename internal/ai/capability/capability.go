// Package capability is the registry of the context commands an AI execution identity may call
// (UL AI-Capability; AI-INV-001). A capability names one api command of one bounded context, its
// contract version, its Go api type as the input schema, its side-effect class, risk level and
// approval need. What is not registered is refused (default deny), and a human-only command of the
// tactical model's humanOnlyCommands can never be registered (X-INV-002, UL-INV-17).
//
// A capability never decides business rules: the owning context authorizes, validates and executes
// the command as it would for any caller.
package capability

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SideEffect is what a capability changes (plan S4-03, decision D-7). The classes are ordered: a
// delegation's autonomy is the highest class it allows.
type SideEffect string

const (
	Read   SideEffect = "read"   // reads and explains authoritative records
	Draft  SideEffect = "draft"  // registers material or saves internal drafts and previews
	Submit SideEffect = "submit" // starts a business step towards an effective quote or order
)

var sideEffectRank = map[SideEffect]int{Read: 1, Draft: 2, Submit: 3}

// Valid reports whether s is one of the three classes.
func (s SideEffect) Valid() bool { return sideEffectRank[s] > 0 }

// Within reports whether s is allowed under autonomy, the highest class a delegation allows.
func (s SideEffect) Within(autonomy SideEffect) bool {
	return s.Valid() && autonomy.Valid() && sideEffectRank[s] <= sideEffectRank[autonomy]
}

// Risk is a capability's risk level.
type Risk string

const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"
)

// Approval is what a call needs besides the delegation.
type Approval string

const (
	// ApprovalNone: the delegation suffices (D-7: reads and drafts).
	ApprovalNone Approval = "none"
	// ApprovalGrant: every call needs a single-use ApprovalGrant a human issued for exactly that
	// request (D-7: order.prepare, order.review.request; AI-INV-002).
	ApprovalGrant Approval = "grant"
)

// Spec describes a capability.
type Spec struct {
	Command    string // the context command, such as "rfq.save_draft"
	Context    string // the owning bounded context's code, lower case, such as "inq"
	Contract   string // the context's api contract, such as "brant.inq/0.1.0"
	Version    int    // the capability's version; a changed input type or meaning is a new version
	SideEffect SideEffect
	Risk       Risk
	Approval   Approval
	// Tenants limits the capability to these tenants; empty means every tenant (UL AI-Capability:
	// 适用租户).
	Tenants []kernel.TenantID
	// Summary describes the tool to the model.
	Summary string
}

// Permission is the permission the owning context's rule requires for the command; for every
// command an AI may call it is the command name.
func (s Spec) Permission() string { return s.Command }

// AppliesTo reports whether the capability is available to tenant.
func (s Spec) AppliesTo(tenant kernel.TenantID) bool {
	return len(s.Tenants) == 0 || slices.Contains(s.Tenants, tenant)
}

// ObjectRef names the object a call acts on and the version the caller expects of it; an approval
// grant binds both (AI-INV-002). Version 0 means "new" or "not versioned".
type ObjectRef struct {
	Type    string // "<context>.<kind>", such as "trd.quote"
	ID      string
	Version int64
}

// Receipt is what every context's command answer shares.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Result is a command's answer: the shared receipt and the context's full typed answer.
type Result struct {
	Receipt
	Detail any
}

// Capability is a registered command. Its zero value is not usable; build one with Define.
type Capability struct {
	Spec
	input   reflect.Type
	object  func(any) ObjectRef
	run     func(context.Context, any) (any, error)
	prepare func(context.Context, any) (any, error)
}

// KeyField is the api command field that carries the idempotency key. The execution entry sets it
// from the persisted action intent; a model never supplies it (baseline 17.1).
const KeyField = "IdempotencyKey"

var (
	// ErrInvalid reports a capability that cannot be registered.
	ErrInvalid = errors.New("capability: invalid")

	commandName = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_.]*$`)
	contextCode = regexp.MustCompile(`^[a-z]{2,8}$`)
)

// Define returns the capability of spec for api command type C. object names the object a command
// acts on; run executes the command at the owning context and returns its typed receipt R, which
// carries the shared receipt fields (ActionID, ObjectID, RowVersion, Status, Replayed). run is
// called only when the capability runs, so a catalogue can be built before its contexts exist.
func Define[C any, R any](spec Spec, object func(C) ObjectRef, run func(context.Context, C) (R, error)) (Capability, error) {
	in := reflect.TypeFor[C]()
	if err := checkSpec(spec); err != nil {
		return Capability{}, err
	}
	if err := checkInput(in); err != nil {
		return Capability{}, fmt.Errorf("%w: %s: %w", ErrInvalid, spec.Command, err)
	}
	if err := checkReceipt(reflect.TypeFor[R]()); err != nil {
		return Capability{}, fmt.Errorf("%w: %s: %w", ErrInvalid, spec.Command, err)
	}
	if object == nil || run == nil {
		return Capability{}, fmt.Errorf("%w: %s needs an object and a run function", ErrInvalid, spec.Command)
	}
	spec.Tenants = slices.Clone(spec.Tenants)
	return Capability{
		Spec:   spec,
		input:  in,
		object: func(cmd any) ObjectRef { return object(cmd.(C)) },
		run: func(ctx context.Context, cmd any) (any, error) {
			r, err := run(ctx, cmd.(C))
			return r, err
		},
	}, nil
}

// Prepared returns c, built by Define (with its error err), with a preparation: deterministic
// server-side completion of a decoded command before it is hashed, admitted and run (the gateway
// calls it after the delegation check). Unit B4's source locating of rfq.save_draft is one: the
// model's field locators are verified against the extracts and its lines' pending fields completed
// (AI-INV-004: the model's claims are candidates). A preparation never decides what the owning
// context decides; its rejections are the call's. Re-sending a prepared call uses the prepared
// command, so its request hash stays the one the target ledger records.
func Prepared[C any](c Capability, err error, prepare func(context.Context, C) (C, error)) (Capability, error) {
	if err != nil {
		return Capability{}, err
	}
	if c.input != reflect.TypeFor[C]() || prepare == nil {
		return Capability{}, fmt.Errorf("%w: %s: the preparation does not take %s", ErrInvalid, c.Command, c.input)
	}
	c.prepare = func(ctx context.Context, cmd any) (any, error) { return prepare(ctx, cmd.(C)) }
	return c, nil
}

// Prepare completes cmd, a value of the input type, with the capability's preparation; a
// capability without one returns cmd as it is.
func (c Capability) Prepare(ctx context.Context, cmd any) (any, error) {
	if reflect.TypeOf(cmd) != c.input {
		return nil, fmt.Errorf("capability: %s prepares %s, not %T", c.Command, c.input, cmd)
	}
	if c.prepare == nil {
		return cmd, nil
	}
	return c.prepare(ctx, cmd)
}

func checkSpec(s Spec) error {
	bad := func(what string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, s.Command, what) }
	switch {
	case !commandName.MatchString(s.Command) || len(s.Command) > 64:
		return bad("command name")
	case IsHumanOnly(s.Command):
		return bad("a human-only command is never an AI capability (X-INV-002, UL-INV-17)")
	case !contextCode.MatchString(s.Context):
		return bad("context code")
	case s.Contract == "":
		return bad("contract")
	case s.Version < 1:
		return bad("version")
	case !s.SideEffect.Valid():
		return bad("side-effect class")
	case s.Risk != RiskLow && s.Risk != RiskMedium && s.Risk != RiskHigh:
		return bad("risk level")
	case s.Approval != ApprovalNone && s.Approval != ApprovalGrant:
		return bad("approval")
	case s.SideEffect == Submit && s.Approval != ApprovalGrant:
		// P0 keeps a content-bound human approval before every step towards an effective quote or
		// order (baseline 8.7, decision D-7).
		return bad("a submit capability needs an approval grant")
	case strings.TrimSpace(s.Summary) == "":
		return bad("summary")
	}
	for _, t := range s.Tenants {
		if !kernel.ValidIdentifier(string(t)) {
			return bad("tenant")
		}
	}
	return nil
}

// platformOwned lists field names (lower case, without underscores) that only the platform may
// set: scope, actor, task, control epoch, delegation and grant come from the Principal and TSK,
// never from model output (AI-INV-004). An api type that has such a field cannot be a capability.
var platformOwned = []string{
	"tenant", "tenantid", "company", "companyid", "scope", "principal",
	"actor", "actorid", "actorkind", "initiator", "initiatorid",
	"task", "taskid", "controlepoch", "expectedcontrolepoch", "authorityepoch", "epoch", "claim",
	"grant", "grantid", "grantref", "delegation", "delegationid", "approvalgrant",
}

func checkInput(t reflect.Type) error {
	if t.Kind() != reflect.Struct {
		return errors.New("the input is not a struct")
	}
	if f, ok := t.FieldByName(KeyField); !ok || f.Type.Kind() != reflect.String || len(f.Index) != 1 {
		return errors.New("the input has no top-level IdempotencyKey string")
	}
	return walkFields(t, map[reflect.Type]bool{}, func(path string, f reflect.StructField) error {
		name := strings.ToLower(strings.ReplaceAll(jsonName(f), "_", ""))
		if slices.Contains(platformOwned, name) {
			return fmt.Errorf("field %s is platform-owned (AI-INV-004)", path)
		}
		switch base(f.Type).Kind() {
		case reflect.Float32, reflect.Float64:
			return fmt.Errorf("field %s is a float; business values are decimals or minor units", path)
		case reflect.Interface, reflect.Func, reflect.Chan, reflect.Complex64, reflect.Complex128, reflect.UnsafePointer:
			return fmt.Errorf("field %s has kind %s, which is not a closed JSON structure", path, base(f.Type).Kind())
		}
		return nil
	})
}

// walkFields calls fn for every exported field reachable from struct type t.
func walkFields(t reflect.Type, seen map[reflect.Type]bool, fn func(path string, f reflect.StructField) error) error {
	t = base(t)
	if t.Kind() != reflect.Struct || seen[t] || t.PkgPath() == "time" {
		return nil
	}
	seen[t] = true
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() || f.Tag.Get("json") == "-" {
			continue
		}
		if err := fn(t.Name()+"."+f.Name, f); err != nil {
			return err
		}
		if err := walkFields(f.Type, seen, fn); err != nil {
			return err
		}
	}
	return nil
}

// base strips pointers, slices, arrays and maps down to their element type.
func base(t reflect.Type) reflect.Type {
	for {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
		default:
			return t
		}
	}
}

// jsonName is the member name encoding/json and strictjson use for f.
func jsonName(f reflect.StructField) string {
	if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" {
		return name
	}
	return f.Name
}

var receiptFields = map[string]reflect.Kind{
	"ActionID": reflect.String, "ObjectID": reflect.String, "RowVersion": reflect.Int64, "Status": reflect.String, "Replayed": reflect.Bool,
}

func checkReceipt(t reflect.Type) error {
	if t.Kind() != reflect.Struct {
		return errors.New("the receipt is not a struct")
	}
	for name, kind := range receiptFields {
		if f, ok := t.FieldByName(name); !ok || f.Type.Kind() != kind {
			return fmt.Errorf("the receipt has no %s", name)
		}
	}
	return nil
}

// Input returns the api command type, the capability's input schema.
func (c Capability) Input() reflect.Type { return c.input }

// Object returns the object cmd, a value of the input type, acts on.
func (c Capability) Object(cmd any) ObjectRef { return c.object(cmd) }

// WithKey returns cmd, a value of the input type, carrying idempotency key.
func (c Capability) WithKey(cmd any, key string) any {
	v := reflect.New(c.input).Elem()
	v.Set(reflect.ValueOf(cmd))
	v.FieldByName(KeyField).SetString(key)
	return v.Interface()
}

// Key returns the idempotency key cmd carries.
func (c Capability) Key(cmd any) string {
	return reflect.ValueOf(cmd).FieldByName(KeyField).String()
}

// RequestHash returns the request hash of cmd as the owning context's ledger computes it: the
// SHA-256 of the JSON encoding of the command without its idempotency key and without its expected
// versions (every top-level integer field named Expected*), so a replay with another CAS value
// still matches (foundation/action RequestHash). An approval grant binds this hash; the object
// version it binds separately (ObjectRef).
func (c Capability) RequestHash(cmd any) (string, error) {
	v := reflect.New(c.input).Elem()
	v.Set(reflect.ValueOf(cmd))
	v.FieldByName(KeyField).SetString("")
	for i := range c.input.NumField() {
		f := c.input.Field(i)
		if strings.HasPrefix(f.Name, "Expected") && v.Field(i).CanInt() {
			v.Field(i).SetInt(0)
		}
	}
	return action.RequestHash(v.Interface())
}

// Run executes cmd, a value of the input type that carries its idempotency key, at the owning
// context with the Principal and claims in ctx. Errors are the context's: *kernel.Rejection for a
// refusal, anything else an infrastructure failure.
func (c Capability) Run(ctx context.Context, cmd any) (Result, error) {
	if reflect.TypeOf(cmd) != c.input {
		return Result{}, fmt.Errorf("capability: %s runs %s, not %T", c.Command, c.input, cmd)
	}
	r, err := c.run(ctx, cmd)
	if err != nil {
		return Result{}, err
	}
	v := reflect.ValueOf(r)
	return Result{Detail: r, Receipt: Receipt{
		ActionID: v.FieldByName("ActionID").String(), ObjectID: v.FieldByName("ObjectID").String(),
		RowVersion: v.FieldByName("RowVersion").Int(), Status: v.FieldByName("Status").String(),
		Replayed: v.FieldByName("Replayed").Bool(),
	}}, nil
}

// Registry is the set of capabilities of a process. A command that is not registered is refused.
type Registry struct {
	byCommand map[string]Capability
}

// NewRegistry registers caps; every command appears once.
func NewRegistry(caps ...Capability) (*Registry, error) {
	r := &Registry{byCommand: make(map[string]Capability, len(caps))}
	for _, c := range caps {
		if c.input == nil {
			return nil, fmt.Errorf("%w: capability %q was not built with Define", ErrInvalid, c.Command)
		}
		if err := checkSpec(c.Spec); err != nil {
			return nil, err
		}
		if _, dup := r.byCommand[c.Command]; dup {
			return nil, fmt.Errorf("%w: %s is registered twice", ErrInvalid, c.Command)
		}
		r.byCommand[c.Command] = c
	}
	return r, nil
}

// Lookup returns the capability of command for tenant. A human-only command is PERMISSION_DENIED
// (AI-INV-001: quote.activate); any other command that is not registered, or not available to the
// tenant, is UNSUPPORTED_CONTRACT. path is the request member the rejection points at.
func (r *Registry) Lookup(tenant kernel.TenantID, command, path string) (Capability, error) {
	if IsHumanOnly(command) {
		return Capability{}, kernel.Reject(kernel.CodePermissionDenied, path, "")
	}
	c, ok := Capability{}, false
	if r != nil {
		c, ok = r.byCommand[command]
	}
	if !ok || !c.AppliesTo(tenant) {
		return Capability{}, kernel.Reject(kernel.CodeUnsupportedContract, path, "")
	}
	return c, nil
}

// Commands returns the registered commands in order.
func (r *Registry) Commands() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.byCommand))
	for c := range r.byCommand {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Capabilities returns the registered capabilities in command order.
func (r *Registry) Capabilities() []Capability {
	out := make([]Capability, 0, len(r.Commands()))
	for _, c := range r.Commands() {
		out = append(out, r.byCommand[c])
	}
	return out
}
