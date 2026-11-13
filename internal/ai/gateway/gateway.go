// Package gateway turns a model's tool call into a registered api command and admits it (plan
// S4-03 gateway; AI-INV-001, AI-INV-004). A model's output is a candidate and a tool call request,
// nothing more: the gateway decodes the arguments strictly (strictjson, contract C7) into the
// capability's api command, and everything that decides who acts, where and with which authority —
// tenant, company, actor, task, control epoch, delegation and grant — comes from the Principal of
// the request and from the Binding the trusted executor (TSK, internal/ai/execution) supplies. A
// model naming any of these in its arguments is SCHEMA_INVALID; a model naming a tool that is not
// registered is UNSUPPORTED_CONTRACT, or PERMISSION_DENIED for a human-only command. Model text
// never reaches the gateway and never changes a task's state: completion is goal.verify's
// (UL-INV-19).
//
// A call goes through Decode (structure, capability, delegation), then Admit with the idempotency
// key of the action intent the executor persisted (the delegation re-checked and, where the
// capability needs one, the approval grant used), then Run at the owning context. Every refusal is
// audited in brant_ai under the AI principal.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/approvalgrant"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/codes"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/foundation/strictjson"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Binding is what the trusted executor states about a step. None of it comes from model output.
type Binding struct {
	TaskID       string // the TSK task the step belongs to
	DelegationID string // the delegation the task's agent runs under
	GrantID      string // the approval grant a human issued for this call; "" when none
}

// ErrBinding is a binding that names no task or delegation: a programming error of the executor,
// not a verdict on the model's call.
var ErrBinding = errors.New("gateway: the binding needs a task and a delegation")

func (b Binding) check() error {
	if !kernel.ValidIdentifier(b.TaskID) || !kernel.ValidIdentifier(b.DelegationID) ||
		(b.GrantID != "" && !kernel.ValidIdentifier(b.GrantID)) {
		return ErrBinding
	}
	return nil
}

// Call is a tool call decoded into a registered api command and allowed by the delegation.
type Call struct {
	Capability  capability.Capability
	Command     any    // the api command, without its idempotency key
	RequestHash string // capability.RequestHash: what an approval grant binds
	Object      capability.ObjectRef
	Delegation  policy.Delegation
}

// NeedsGrant reports whether the call needs a human-issued approval grant before it runs (D-7). A
// call that needs one and has none waits for approval; the executor shows the issuer the command,
// the request hash and the object.
func (c Call) NeedsGrant() bool { return c.Capability.Approval == capability.ApprovalGrant }

// Admitted is a call cleared to run with its idempotency key.
type Admitted struct {
	Call
	Key   string
	Grant *approvalgrant.Grant // the grant the call used, for a capability that needs one
	Epoch int64                // the scope's authority epoch the admission saw
}

// Run executes the call at the owning context with the Principal and claims of ctx (the executor
// adds the task's control claim, trdapi.WithControl). Errors are the context's.
func (a Admitted) Run(ctx context.Context) (capability.Result, error) {
	return a.Capability.Run(ctx, a.Capability.WithKey(a.Command, a.Key))
}

// Auditor records a refusal in the AI layer's audit trail.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Gateway decodes and admits the tool calls of AI actors.
type Gateway struct {
	Registry *capability.Registry
	Policy   policy.Service
	Grants   approvalgrant.Service
	Auditor  Auditor
	// Limits bound the arguments of one call; zero fields take strictjson's ingress defaults.
	Limits strictjson.Limits
}

// Decode decodes call for the AI actor of ctx under b. In order (contract C5's order: contract,
// structure, then authority):
//
//   - the actor is an AI actor (PERMISSION_DENIED);
//   - the tool is a registered capability available to the tenant (PERMISSION_DENIED for a
//     human-only command, UNSUPPORTED_CONTRACT otherwise; AI-INV-001);
//   - the arguments are one closed JSON object of the capability's api command, without the
//     idempotency key and without empty strings (SCHEMA_INVALID at /args/...; AI-INV-004);
//   - the delegation allows the capability for the task now (policy.Check: GRANT_INVALID,
//     GRANT_EXPIRED, PERMISSION_DENIED).
func (g Gateway) Decode(ctx context.Context, b Binding, call model.ToolCall) (Call, error) {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return Call{}, err
	}
	if err = b.check(); err != nil {
		return Call{}, err
	}
	ctx = action.WithCorrelation(ctx, action.Correlation(ctx))
	audited := auditName(call.Name)
	if p.Actor.Kind != kernel.ActorAI {
		return Call{}, g.audit(ctx, audited, "", kernel.Reject(kernel.CodePermissionDenied, "/", "the gateway serves AI actors"))
	}
	c, err := g.Registry.Lookup(p.Scope.Tenant, call.Name, "/command")
	if err != nil {
		return Call{}, g.audit(ctx, audited, "", err)
	}
	cmd, err := g.decode(c, call.Arguments)
	if err != nil {
		return Call{}, g.audit(ctx, c.Command, "", err)
	}
	hash, err := c.RequestHash(cmd)
	if err != nil {
		return Call{}, err
	}
	auth, err := g.Policy.Check(ctx, policy.Step{DelegationID: b.DelegationID, TaskID: b.TaskID, Capability: c})
	if err != nil {
		return Call{}, err // audited by Check
	}
	return Call{Capability: c, Command: cmd, RequestHash: hash, Object: c.Object(cmd), Delegation: auth.Delegation}, nil
}

// Admit clears call to run with key, the idempotency key of the action intent the executor
// persisted before the side effect (TSK-INV-003; baseline 17.1). It re-checks the delegation at
// this step and, for a capability that needs one, uses the approval grant b names for exactly this
// call (approvalgrant.Service.Use: GRANT_INVALID when b names none, or a forged, foreign, used,
// revoked or mismatched one; GRANT_EXPIRED). The same call admitted again with the same key is the
// same use.
func (g Gateway) Admit(ctx context.Context, b Binding, call Call, key string) (Admitted, error) {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return Admitted{}, err
	}
	if err = b.check(); err != nil {
		return Admitted{}, err
	}
	if !kernel.ValidIdentifier(key) || call.Command == nil || reflect.TypeOf(call.Command) != call.Capability.Input() {
		return Admitted{}, errors.New("gateway: admit needs a decoded call and the intent's idempotency key")
	}
	ctx = action.WithCorrelation(ctx, action.Correlation(ctx))
	if p.Actor.Kind != kernel.ActorAI {
		return Admitted{}, g.audit(ctx, call.Capability.Command, "", kernel.Reject(kernel.CodePermissionDenied, "/", "the gateway serves AI actors"))
	}
	step := policy.Step{DelegationID: b.DelegationID, TaskID: b.TaskID, Capability: call.Capability}
	out := Admitted{Call: call, Key: key}
	if !call.NeedsGrant() {
		auth, err := g.Policy.Check(ctx, step)
		if err != nil {
			return Admitted{}, err // audited by Check
		}
		out.Delegation, out.Epoch = auth.Delegation, auth.Epoch
		return out, nil
	}
	if b.GrantID == "" {
		return Admitted{}, g.audit(ctx, call.Capability.Command, call.Object.ID, codes.Reject(codes.GrantInvalid, "/grant"))
	}
	grant, auth, err := g.Grants.Use(ctx, approvalgrant.Use{GrantID: b.GrantID, Step: step, RequestHash: call.RequestHash,
		Object: call.Object, Key: key})
	if err != nil {
		return Admitted{}, err // audited by Use
	}
	out.Grant, out.Delegation, out.Epoch = &grant, auth.Delegation, auth.Epoch
	return out, nil
}

// Handle decodes, admits and runs call with key: the whole path of one tool call.
func (g Gateway) Handle(ctx context.Context, b Binding, call model.ToolCall, key string) (capability.Result, error) {
	c, err := g.Decode(ctx, b, call)
	if err != nil {
		return capability.Result{}, err
	}
	a, err := g.Admit(ctx, b, c, key)
	if err != nil {
		return capability.Result{}, err
	}
	return a.Run(ctx)
}

// Tools returns the registered capabilities that a delegation allows and that are available to the
// tenant of the Principal in ctx, as the tools offered to the model with their argument schemas: the
// model is never offered a tool it may not call.
func (g Gateway) Tools(ctx context.Context, d policy.Delegation) ([]model.Tool, error) {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Tool
	for _, c := range g.Registry.Capabilities() {
		if !d.Allows(c) || !c.AppliesTo(p.Scope.Tenant) {
			continue
		}
		schema, err := c.Schema()
		if err != nil {
			return nil, err
		}
		out = append(out, model.Tool{Name: c.Command, Description: c.Summary, Parameters: schema})
	}
	return out, nil
}

// decode decodes args strictly into a value of c's api command.
func (g Gateway) decode(c capability.Capability, args json.RawMessage) (any, error) {
	v := reflect.New(c.Input())
	if err := strictjson.Unmarshal(args, v.Interface(), g.Limits); err != nil {
		rej, ok := kernel.AsRejection(err)
		if !ok {
			return nil, err
		}
		path := "/args"
		if f := rej.Fields[0]; f.Path != "/" {
			path += f.Path
		}
		return nil, kernel.Reject(rej.Code, path, rej.Fields[0].Reason)
	}
	cmd := v.Elem().Interface()
	if c.Key(cmd) != "" {
		// The idempotency key is the persisted intent's, never the model's (baseline 17.1).
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/args/"+capability.KeyField, "set by the platform")
	}
	if p := emptyString(args); p != "" {
		// Every string of the contracts has at least one character; an unknown value is omitted.
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/args"+p, "empty string")
	}
	return cmd, nil
}

func (g Gateway) audit(ctx context.Context, command, objectID string, err error) error {
	rej, ok := kernel.AsRejection(err)
	if !ok || g.Auditor == nil {
		return err
	}
	if auditErr := g.Auditor.AuditRejected(ctx, command, objectID, rej.Code); auditErr != nil {
		return errors.Join(err, auditErr)
	}
	return err
}

var commandName = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_.]*$`)

// auditName is the name a tool call is audited under: the tool's name when it is shaped like a
// command, "invalid" otherwise, so model output never reaches the audit trail unshaped.
func auditName(name string) string {
	if len(name) <= 64 && commandName.MatchString(name) {
		return name
	}
	return "invalid"
}

// emptyString returns the JSON Pointer (below /args) of the first empty string in a valid JSON
// document, or "".
func emptyString(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return findEmpty(v, "")
}

var pointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")

func findEmpty(v any, path string) string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return path
		}
	case []any:
		for i, e := range t {
			if p := findEmpty(e, path+"/"+strconv.Itoa(i)); p != "" {
				return p
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if p := findEmpty(t[k], path+"/"+pointerEscaper.Replace(k)); p != "" {
				return p
			}
		}
	}
	return ""
}
