package authorization

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Role templates and separation of duties (BC-D02, simulated review SR-32). The catalog in
// roles_gen.go is generated from the master archive; tenants assign templates directly or merge
// several into roles of their own, never into a forbidden combination.

// Template is a default role: what one job does, as the permissions it holds.
type Template struct {
	Code        string
	Name        string
	Duties      string
	Permissions []string
}

// ForbiddenCombination is a pair of templates no role and no actor may hold together.
type ForbiddenCombination struct {
	A, B string
	Rule string
}

// ExclusivePermission is a permission only one template grants.
type ExclusivePermission struct {
	Permission string
	Template   string
	Rule       string
}

// ErrSeparationOfDuties refuses a role or an assignment that holds a forbidden combination.
var ErrSeparationOfDuties = errors.New("authorization: separation of duties")

// Templates returns the default role templates.
func Templates() []Template { return slices.Clone(templates) }

// TemplateByCode returns the template code.
func TemplateByCode(code string) (Template, bool) {
	i := slices.IndexFunc(templates, func(t Template) bool { return t.Code == code })
	if i < 0 {
		return Template{}, false
	}
	return templates[i], true
}

// ForbiddenCombinations returns the pairs of templates no role or actor may hold together.
func ForbiddenCombinations() []ForbiddenCombination { return slices.Clone(forbiddenCombinations) }

// ExclusivePermissions returns the permissions only one template grants.
func ExclusivePermissions() []ExclusivePermission { return slices.Clone(exclusivePermissions) }

// CheckSeparation refuses a set of templates that holds a forbidden combination.
func CheckSeparation(codes []string) error {
	for _, f := range forbiddenCombinations {
		if slices.Contains(codes, f.A) && slices.Contains(codes, f.B) {
			return fmt.Errorf("%w: %s and %s (%s)", ErrSeparationOfDuties, f.A, f.B, f.Rule)
		}
	}
	return nil
}

// permissionsOf returns the sorted union of the templates' permissions.
func permissionsOf(codes []string) ([]string, error) {
	var out []string
	for _, c := range codes {
		t, ok := TemplateByCode(c)
		if !ok {
			return nil, fmt.Errorf("%w: unknown role template %q", ErrInvalidPolicy, c)
		}
		out = append(out, t.Permissions...)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

const (
	defineRole = `INSERT INTO brant_auth.role (tenant_id, company_id, role_code, name, templates, defined_epoch, defined_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, company_id, role_code) DO UPDATE SET name = EXCLUDED.name, templates = EXCLUDED.templates,
		defined_epoch = EXCLUDED.defined_epoch, defined_at = EXCLUDED.defined_at`
	readRole     = `SELECT templates FROM brant_auth.role WHERE tenant_id = $1 AND company_id = $2 AND role_code = $3`
	rolesHolding = `SELECT DISTINCT actor_id FROM brant_auth.actor_role WHERE tenant_id = $1 AND company_id = $2 AND role_code = $3
		AND removed_epoch IS NULL ORDER BY actor_id`
	actorRoles = `SELECT role_code FROM brant_auth.actor_role WHERE tenant_id = $1 AND company_id = $2 AND actor_id = $3
		AND removed_epoch IS NULL ORDER BY role_code`
	removeActorRole = `UPDATE brant_auth.actor_role SET removed_epoch = $5, removed_at = $6 WHERE tenant_id = $1 AND company_id = $2
		AND actor_id = $3 AND role_code = $4 AND removed_epoch IS NULL`
	addActorRole = `INSERT INTO brant_auth.actor_role (tenant_id, company_id, actor_id, role_code, assigned_epoch, assigned_at)
		VALUES ($1, $2, $3, $4, $5, $6)`
)

// DefineRole creates or redefines a tenant role that merges templates. A role code may not be a
// template code, and the templates may not hold a forbidden combination. Redefining a role that
// actors hold re-derives their permissions, which bumps the authority epoch.
func (g Grants) DefineRole(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, code, name string, templateCodes []string, at time.Time) (int64, error) {
	if !kernel.ValidIdentifier(code) || name == "" || len(templateCodes) == 0 {
		return 0, fmt.Errorf("%w: role %q", ErrInvalidPolicy, code)
	}
	if _, isTemplate := TemplateByCode(code); isTemplate {
		return 0, fmt.Errorf("%w: role %q is a template code", ErrInvalidPolicy, code)
	}
	codes := slices.Compact(slices.Sorted(slices.Values(templateCodes)))
	if _, err := permissionsOf(codes); err != nil {
		return 0, err
	}
	if err := CheckSeparation(codes); err != nil {
		return 0, err
	}
	epoch, err := lockForChange(ctx, exec, scope)
	if err != nil {
		return 0, err
	}
	if _, err = exec.Exec(ctx, defineRole, string(scope.Tenant), string(scope.Company), code, name, codes, epoch, kernel.Instant(at)); err != nil {
		return 0, err
	}
	rows, err := exec.Query(ctx, rolesHolding, string(scope.Tenant), string(scope.Company), code)
	if err != nil {
		return 0, err
	}
	var holders []string
	for rows.Next() {
		var a string
		if err = rows.Scan(&a); err != nil {
			rows.Close()
			return 0, err
		}
		holders = append(holders, a)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	for _, a := range holders {
		if epoch, err = g.reassign(ctx, exec, scope, a, at); err != nil {
			return 0, err
		}
	}
	return epoch, nil
}

// AssignRoles gives an actor exactly roles, each a template or a tenant role, and derives the
// actor's permissions from them. The union of their templates may not hold a forbidden
// combination. It returns the scope's authority epoch after the change.
func (g Grants) AssignRoles(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, actor kernel.ActorRef, roles []string, at time.Time) (int64, error) {
	if !actor.Valid() {
		return 0, fmt.Errorf("%w: actor %s", ErrInvalidPolicy, actor)
	}
	roles = slices.Compact(slices.Sorted(slices.Values(roles)))
	codes, err := g.templatesOf(ctx, exec, scope, roles)
	if err != nil {
		return 0, err
	}
	if err = CheckSeparation(codes); err != nil {
		return 0, err
	}
	permissions, err := permissionsOf(codes)
	if err != nil {
		return 0, err
	}
	epoch, err := g.Put(ctx, exec, scope, actor, permissions, at)
	if err != nil {
		return 0, err
	}
	current, err := g.Roles(ctx, exec, scope, actor.ID)
	if err != nil {
		return 0, err
	}
	// Assignments are ended, never deleted: the history of who held which role stays.
	t, c := string(scope.Tenant), string(scope.Company)
	for _, r := range current {
		if !slices.Contains(roles, r) {
			if _, err = exec.Exec(ctx, removeActorRole, t, c, actor.ID, r, epoch, kernel.Instant(at)); err != nil {
				return 0, err
			}
		}
	}
	for _, r := range roles {
		if !slices.Contains(current, r) {
			if _, err = exec.Exec(ctx, addActorRole, t, c, actor.ID, r, epoch, kernel.Instant(at)); err != nil {
				return 0, err
			}
		}
	}
	return epoch, nil
}

// Roles returns the roles an actor holds.
func (Grants) Roles(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, actorID string) ([]string, error) {
	rows, err := exec.Query(ctx, actorRoles, string(scope.Tenant), string(scope.Company), actorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err = rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// reassign re-derives an actor's permissions after one of its roles changed.
func (g Grants) reassign(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, actorID string, at time.Time) (int64, error) {
	roles, err := g.Roles(ctx, exec, scope, actorID)
	if err != nil {
		return 0, err
	}
	var kind string
	if err = exec.QueryRow(ctx, readGrant+` FOR NO KEY UPDATE`, string(scope.Tenant), string(scope.Company), actorID).Scan(&kind, new(bool), new([]string)); err != nil {
		return 0, err
	}
	return g.AssignRoles(ctx, exec, scope, kernel.ActorRef{Kind: kernel.ActorKind(kind), ID: actorID}, roles, at)
}

// templatesOf resolves roles to the sorted union of their templates.
func (Grants) templatesOf(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, roles []string) ([]string, error) {
	var codes []string
	for _, r := range roles {
		if _, ok := TemplateByCode(r); ok {
			codes = append(codes, r)
			continue
		}
		var merged []string
		err := exec.QueryRow(ctx, readRole, string(scope.Tenant), string(scope.Company), r).Scan(&merged)
		if err != nil {
			return nil, fmt.Errorf("%w: unknown role %q: %w", ErrInvalidPolicy, r, err)
		}
		codes = append(codes, merged...)
	}
	return slices.Compact(slices.Sorted(slices.Values(codes))), nil
}
