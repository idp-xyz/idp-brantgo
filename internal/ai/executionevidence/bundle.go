// Package executionevidence is the AI layer's evidence of execution in brant_ai (plan S4 decision
// D-6, unit C2): the ExecutionBundle an AI task runs with (baseline 17.3), the budget it runs under
// and the ledger of every model call and tool call, reserved at the execution entry before the call
// and settled after it (baseline 17.4; AI-INV-003).
//
// What is stored is evidence, not content: the model ID the provider returned, the tokens it
// reported, the latency, the cost in exact minor units and the SHA-256 of what was sent and received
// (decision E-6). Prompts and outputs are not kept; a task's next turn is rebuilt from the
// authoritative records (TSK's task and actions), never from a stored conversation.
package executionevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Application names the execution contract of this code: a runtime of another application version
// runs no task whose bundle names this one unless it lists the bundle as compatible.
const Application = "brant.ai.execution/0.1.0"

// Canonicalization names how request hashes are formed (foundation/action RequestHash: SHA-256 of
// the JSON of the api command without its idempotency key and expected versions).
const Canonicalization = "brant.request_hash/1"

// ErrInvalid is a bundle or profile that cannot be recorded.
var ErrInvalid = errors.New("executionevidence: invalid")

// ToolRef is one capability in a bundle: the command, its context and contract, its capability
// version, class and approval need, and the SHA-256 of the argument schema the model is offered.
type ToolRef struct {
	Command      string `json:"command"`
	Context      string `json:"context"`
	Contract     string `json:"contract"`
	Version      int    `json:"version"`
	SideEffect   string `json:"side_effect"`
	Approval     string `json:"approval"`
	SchemaSHA256 string `json:"schema_sha256"`
}

// Bundle is an ExecutionBundle (baseline 17.3): everything a task's execution depends on that may
// change under it. Its SHA-256 over the canonical JSON is what a task records (task.create
// execution_bundle {bundle_id, sha256}); the model ID a provider actually served is recorded per
// call, since providers may only offer rolling aliases.
type Bundle struct {
	BundleID         string       `json:"bundle_id"`
	Application      string       `json:"application"`
	TaskProfile      string       `json:"task_profile"`
	Goals            []string     `json:"goals"` // "<goal code>@<goal version>"
	Model            ModelProfile `json:"model"`
	PromptSHA256     string       `json:"prompt_sha256"`
	Tools            []ToolRef    `json:"tools"`
	Canonicalization string       `json:"canonicalization"`
	DefaultBudget    BudgetSpec   `json:"default_budget"`
}

// BudgetSpec is a budget as a bundle states it: model calls, retries and an optional cost cap
// (amount in currency, a decimal with at most minor_units digits).
type BudgetSpec struct {
	MaxModelCalls int    `json:"max_model_calls"`
	MaxRetries    int    `json:"max_retries"`
	Currency      string `json:"currency"`
	MinorUnits    int    `json:"minor_units"`
	MaxCost       string `json:"max_cost,omitempty"`
}

// Ref names a bundle and its SHA-256.
type Ref struct {
	BundleID string
	SHA256   string
}

// ToolsOf returns the bundle's tool references for the capabilities of registry, in command order.
func ToolsOf(registry *capability.Registry) ([]ToolRef, error) {
	var out []ToolRef
	for _, c := range registry.Capabilities() {
		schema, err := c.Schema()
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(schema)
		out = append(out, ToolRef{Command: c.Command, Context: c.Context, Contract: c.Contract, Version: c.Version,
			SideEffect: string(c.SideEffect), Approval: string(c.Approval), SchemaSHA256: hex.EncodeToString(sum[:])})
	}
	return out, nil
}

var goalRef = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}@[1-9][0-9]{0,8}$`)

// Validate checks that the bundle can be recorded.
func (b Bundle) Validate() error {
	bad := func(what string) error { return fmt.Errorf("%w: bundle %s: %s", ErrInvalid, b.BundleID, what) }
	switch {
	case !kernel.ValidIdentifier(b.BundleID):
		return bad("bundle id")
	case b.Application == "" || b.TaskProfile == "" || b.Canonicalization == "":
		return bad("application, task profile and canonicalization are required")
	case len(b.Goals) == 0:
		return bad("goals")
	case !hex64(b.PromptSHA256):
		return bad("prompt template hash")
	case len(b.Tools) == 0:
		return bad("tools")
	}
	for _, g := range b.Goals {
		if !goalRef.MatchString(g) {
			return bad("goal " + g)
		}
	}
	for _, t := range b.Tools {
		if t.Command == "" || t.Context == "" || t.Contract == "" || t.Version < 1 || !hex64(t.SchemaSHA256) {
			return bad("tool " + t.Command)
		}
	}
	if err := b.Model.Validate(); err != nil {
		return err
	}
	if _, err := b.DefaultBudget.Limits("runner"); err != nil {
		return err
	}
	return nil
}

// Canonical returns the canonical JSON of the bundle: goals and tools sorted, fields in their
// declared order.
func (b Bundle) Canonical() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	b.Goals = slices.Clone(b.Goals)
	slices.Sort(b.Goals)
	b.Tools = slices.Clone(b.Tools)
	slices.SortFunc(b.Tools, func(x, y ToolRef) int { return strings.Compare(x.Command, y.Command) })
	return json.Marshal(b)
}

// Ref returns the bundle's ID and the SHA-256 of its canonical JSON.
func (b Bundle) Ref() (Ref, error) {
	raw, err := b.Canonical()
	if err != nil {
		return Ref{}, err
	}
	sum := sha256.Sum256(raw)
	return Ref{BundleID: b.BundleID, SHA256: hex.EncodeToString(sum[:])}, nil
}

// SHA256Hex returns the SHA-256 of b in lower-case hex.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hex64(s string) bool { return kernel.ValidRequestHash(s) }
