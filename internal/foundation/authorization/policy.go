// Package authorization decides whether a principal may issue a command. A context registers one
// Rule per command; Authorizer admits the actor kind and then re-checks the actor's grant inside
// the caller's own transaction against the local authorization projection in brant_auth.
package authorization

import (
	"errors"
	"fmt"
	"slices"

	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	ErrUnknownCommand  = errors.New("authorization: command has no rule")
	ErrActorKindDenied = errors.New("authorization: actor kind not allowed for command")
	ErrForbidden       = errors.New("authorization: permission denied")
	ErrScopeMismatch   = errors.New("authorization: principal scope differs from the transaction scope")
	ErrInvalidPolicy   = errors.New("authorization: invalid policy")
)

// Rule states who may issue one command: the permission a grant must hold and the actor kinds
// allowed to hold it for this command.
type Rule struct {
	Command    string
	Permission string
	ActorKinds []kernel.ActorKind
}

// HumanOnly returns a rule that only human actors satisfy, whatever their grants say.
func HumanOnly(command, permission string) Rule {
	return Rule{Command: command, Permission: permission, ActorKinds: []kernel.ActorKind{kernel.ActorHuman}}
}

// Admits reports whether the rule allows actors of kind.
func (r Rule) Admits(kind kernel.ActorKind) bool { return slices.Contains(r.ActorKinds, kind) }

func (r Rule) validate() error {
	if r.Command == "" || !kernel.ValidIdentifier(r.Permission) || len(r.ActorKinds) == 0 {
		return fmt.Errorf("%w: rule %+v", ErrInvalidPolicy, r)
	}
	for _, k := range r.ActorKinds {
		if !k.Valid() {
			return fmt.Errorf("%w: command %s allows unknown actor kind %q", ErrInvalidPolicy, r.Command, k)
		}
	}
	return nil
}

// Policy is the set of rules for a context's commands. A command without a rule is denied.
type Policy struct {
	rules map[string]Rule
}

// NewPolicy returns the policy of rules; every command appears once.
func NewPolicy(rules ...Rule) (Policy, error) {
	p := Policy{rules: make(map[string]Rule, len(rules))}
	for _, r := range rules {
		if err := r.validate(); err != nil {
			return Policy{}, err
		}
		if _, dup := p.rules[r.Command]; dup {
			return Policy{}, fmt.Errorf("%w: command %s has two rules", ErrInvalidPolicy, r.Command)
		}
		r.ActorKinds = slices.Clone(r.ActorKinds)
		p.rules[r.Command] = r
	}
	return p, nil
}

// Rule returns the rule of command.
func (p Policy) Rule(command string) (Rule, error) {
	r, ok := p.rules[command]
	if !ok {
		return Rule{}, fmt.Errorf("%w: %s", ErrUnknownCommand, command)
	}
	return r, nil
}

// Admit checks, before any database work, that the command has a rule and that the rule allows
// the principal's actor kind; an AI or system actor never passes a human-only rule.
func (p Policy) Admit(principal identity.Principal, command string) (Rule, error) {
	if err := principal.Validate(); err != nil {
		return Rule{}, err
	}
	r, err := p.Rule(command)
	if err != nil {
		return Rule{}, err
	}
	if !r.Admits(principal.Actor.Kind) {
		return Rule{}, fmt.Errorf("%w: %s may not issue %s", ErrActorKindDenied, principal.Actor, command)
	}
	return r, nil
}
