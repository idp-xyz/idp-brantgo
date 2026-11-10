package kernel

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrInvalidIdentifier = errors.New("kernel: invalid identifier")
	ErrInvalidScope      = errors.New("kernel: invalid scope")
	ErrInvalidActor      = errors.New("kernel: invalid actor")
)

// ValidIdentifier reports whether s is 1–64 characters of A–Z, a–z, 0–9, '.', '_', ':' and '-'
// that starts with a letter or digit: the identifier rule of the Python contracts.
func ValidIdentifier(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !alnum && (i == 0 || (c != '.' && c != '_' && c != ':' && c != '-')) {
			return false
		}
	}
	return true
}

type (
	TenantID  string
	CompanyID string
)

// Scope is the tenant and company a command or query acts for. It always comes from a verified
// token; a request body never supplies it. IAM and POL are its authorities; the kernel only
// carries it.
type Scope struct {
	Tenant  TenantID
	Company CompanyID
}

// NewScope returns the scope of tenant and company.
func NewScope(tenant, company string) (Scope, error) {
	s := Scope{Tenant: TenantID(tenant), Company: CompanyID(company)}
	if !s.Valid() {
		return Scope{}, fmt.Errorf("%w: %q/%q", ErrInvalidScope, tenant, company)
	}
	return s, nil
}

// Valid reports whether both identifiers are valid.
func (s Scope) Valid() bool {
	return ValidIdentifier(string(s.Tenant)) && ValidIdentifier(string(s.Company))
}

func (s Scope) String() string { return string(s.Tenant) + "/" + string(s.Company) }

// ParseScope parses the "<tenant>/<company>" form that String returns, as event envelopes carry it.
func ParseScope(s string) (Scope, error) {
	tenant, company, ok := strings.Cut(s, "/")
	if !ok {
		return Scope{}, fmt.Errorf("%w: %q has no tenant/company separator", ErrInvalidScope, s)
	}
	return NewScope(tenant, company)
}

// ActorKind tells who issued a command. Human-only commands reject ai and system actors.
type ActorKind string

const (
	ActorHuman  ActorKind = "human"
	ActorAI     ActorKind = "ai"
	ActorSystem ActorKind = "system"
)

// Valid reports whether k is one of the three actor kinds.
func (k ActorKind) Valid() bool { return k == ActorHuman || k == ActorAI || k == ActorSystem }

// ActorRef identifies the actor of a command within its scope.
type ActorRef struct {
	Kind ActorKind
	ID   string
}

// NewActorRef returns the actor id of kind.
func NewActorRef(kind ActorKind, id string) (ActorRef, error) {
	a := ActorRef{Kind: kind, ID: id}
	if !a.Valid() {
		return ActorRef{}, fmt.Errorf("%w: %q/%q", ErrInvalidActor, kind, id)
	}
	return a, nil
}

// Valid reports whether the kind is known and the ID is a valid identifier.
func (a ActorRef) Valid() bool { return a.Kind.Valid() && ValidIdentifier(a.ID) }

// IsHuman reports whether a human issued the command.
func (a ActorRef) IsHuman() bool { return a.Kind == ActorHuman }

func (a ActorRef) String() string { return string(a.Kind) + ":" + a.ID }
