package authorization

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The authority guard row of a scope and the actor grants live in brant_auth, which
// foundation/authorization owns (migrations/auth). A check locks the scope row and then the grant
// row FOR SHARE; a change locks the scope row FOR NO KEY UPDATE first. Both take the scope row
// first, so they never deadlock, and a change waits for the commands already holding the guard.
const (
	lockScopeShared = `SELECT authority_epoch FROM brant_auth.authority_scope
		WHERE tenant_id = $1 AND company_id = $2 FOR SHARE`
	lockGrantShared = `SELECT actor_kind, active, $4 = ANY (permissions), data_scope, coalesce(team_id, '')
		FROM brant_auth.actor_grant WHERE tenant_id = $1 AND company_id = $2 AND actor_id = $3 FOR SHARE`
	lockScopeForChange = `SELECT authority_epoch FROM brant_auth.authority_scope
		WHERE tenant_id = $1 AND company_id = $2 FOR NO KEY UPDATE`
	readGrant = `SELECT actor_kind, active, permissions FROM brant_auth.actor_grant
		WHERE tenant_id = $1 AND company_id = $2 AND actor_id = $3`
	readDataScope = `SELECT data_scope, coalesce(team_id, '') FROM brant_auth.actor_grant
		WHERE tenant_id = $1 AND company_id = $2 AND actor_id = $3`
	setDataScope = `UPDATE brant_auth.actor_grant SET data_scope = $4, team_id = nullif($5, ''), changed_epoch = $6,
		changed_at = $7 WHERE tenant_id = $1 AND company_id = $2 AND actor_id = $3`
	bumpEpoch = `UPDATE brant_auth.authority_scope SET authority_epoch = authority_epoch + 1
		WHERE tenant_id = $1 AND company_id = $2 RETURNING authority_epoch`
	upsertGrant = `INSERT INTO brant_auth.actor_grant
		(tenant_id, company_id, actor_id, actor_kind, permissions, active, changed_epoch, changed_at)
		VALUES ($1, $2, $3, $4, $5, true, $6, $7)
		ON CONFLICT (tenant_id, company_id, actor_id) DO UPDATE SET actor_kind = EXCLUDED.actor_kind,
		permissions = EXCLUDED.permissions, active = true, changed_epoch = EXCLUDED.changed_epoch,
		changed_at = EXCLUDED.changed_at`
	deactivateGrant = `UPDATE brant_auth.actor_grant SET active = false, changed_epoch = $4, changed_at = $5
		WHERE tenant_id = $1 AND company_id = $2 AND actor_id = $3`
	provisionScope = `INSERT INTO brant_auth.authority_scope (tenant_id, company_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`
)

// DataScope is the part of a company's records an actor sees (BC-D02).
type DataScope string

const (
	DataScopeOwn     DataScope = "own"     // records the actor owns
	DataScopeTeam    DataScope = "team"    // and records its team owns
	DataScopeCompany DataScope = "company" // every record of the company
)

// Valid reports whether s is one of the three data scopes.
func (s DataScope) Valid() bool {
	return s == DataScopeOwn || s == DataScopeTeam || s == DataScopeCompany
}

// Decision is a granted check. Epoch is the scope's authority epoch the command saw; the command
// records it with its result. Actor, DataScope and Team decide which records the actor sees.
type Decision struct {
	Epoch     int64
	Actor     kernel.ActorRef
	DataScope DataScope
	Team      string
}

// Sees reports whether the actor may see a record owned by ownerActor, whose team was ownerTeam
// when it took ownership. Records outside the data scope are reported as not found.
func (d Decision) Sees(ownerActor, ownerTeam string) bool {
	switch {
	case ownerActor != "" && ownerActor == d.Actor.ID:
		return true
	case d.DataScope == DataScopeCompany:
		return true
	case d.DataScope == DataScopeTeam:
		return d.Team != "" && d.Team == ownerTeam
	}
	return false
}

// Authorizer admits commands by Policy and re-checks grants through the caller's transaction.
type Authorizer struct {
	Policy Policy
}

// Authorize runs inside the caller's ScopeTransactor transaction, with exec from the caller's own
// Bento DB. It admits the actor kind of the Principal in ctx, then locks the scope's guard row and
// the actor's grant row FOR SHARE in that transaction. A revocation that committed first denies
// the command; a revocation that starts later waits until the command commits.
func (a Authorizer) Authorize(ctx context.Context, exec bentopg.Executor, command string) (Decision, error) {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return Decision{}, err
	}
	rule, err := a.Policy.Admit(p, command)
	if err != nil {
		return Decision{}, err
	}
	return guard(ctx, exec, p, rule.Permission, command)
}

// Access is Authorize without a command: it holds the same guard rows FOR SHARE and returns the
// actor's data scope as long as the actor has an active grant of the right kind. A context uses it
// to answer reference lookups from downstream contexts, which need no permission of their own
// beyond seeing the record (Python _master_ref).
func (a Authorizer) Access(ctx context.Context, exec bentopg.Executor) (Decision, error) {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return Decision{}, err
	}
	if err = p.Validate(); err != nil {
		return Decision{}, err
	}
	return guard(ctx, exec, p, "", "access")
}

// guard locks the scope's guard row and then the actor's grant row FOR SHARE. An empty permission
// asks only for an active grant.
func guard(ctx context.Context, exec bentopg.Executor, p identity.Principal, permission, command string) (Decision, error) {
	if scope, ok := bento.ScopeFromContext(ctx); !ok || scope != p.Scope {
		return Decision{}, fmt.Errorf("%w: principal %s, transaction %s", ErrScopeMismatch, p.Scope, scope)
	}
	tenant, company := string(p.Scope.Tenant), string(p.Scope.Company)
	d := Decision{Actor: p.Actor}
	err := exec.QueryRow(ctx, lockScopeShared, tenant, company).Scan(&d.Epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, fmt.Errorf("%w: scope %s has no authority row", ErrForbidden, p.Scope)
	}
	if err != nil {
		return Decision{}, err
	}
	var kind, dataScope string
	var active, holds bool
	err = exec.QueryRow(ctx, lockGrantShared, tenant, company, p.Actor.ID, permission).Scan(&kind, &active, &holds, &dataScope, &d.Team)
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, fmt.Errorf("%w: %s has no grant in %s", ErrForbidden, p.Actor, p.Scope)
	}
	if err != nil {
		return Decision{}, err
	}
	if !active || (permission != "" && !holds) || kernel.ActorKind(kind) != p.Actor.Kind {
		return Decision{}, fmt.Errorf("%w: %s lacks %q for %s", ErrForbidden, p.Actor, permission, command)
	}
	d.DataScope = DataScope(dataScope)
	return d, nil
}

// Grants maintains the local authorization projection of a scope. Each change that alters access
// increments the scope's authority epoch in the same transaction.
type Grants struct{}

// Provision creates the scope's guard row at epoch 1 when it is missing.
func (Grants) Provision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) error {
	_, err := exec.Exec(ctx, provisionScope, string(scope.Tenant), string(scope.Company))
	return err
}

// Put gives actor exactly permissions and activates its grant. It returns the scope's epoch after
// the call, which is unchanged when the grant already said the same.
func (Grants) Put(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, actor kernel.ActorRef, permissions []string, at time.Time) (int64, error) {
	if !actor.Valid() {
		return 0, fmt.Errorf("%w: actor %s", ErrInvalidPolicy, actor)
	}
	perms := append([]string{}, permissions...) // never nil: permissions is NOT NULL
	slices.Sort(perms)
	perms = slices.Compact(perms)
	for _, p := range perms {
		if !kernel.ValidIdentifier(p) {
			return 0, fmt.Errorf("%w: permission %q", ErrInvalidPolicy, p)
		}
	}
	epoch, err := lockForChange(ctx, exec, scope)
	if err != nil {
		return 0, err
	}
	var kind string
	var active bool
	var current []string
	err = exec.QueryRow(ctx, readGrant, string(scope.Tenant), string(scope.Company), actor.ID).Scan(&kind, &active, &current)
	if err == nil && active && kernel.ActorKind(kind) == actor.Kind && slices.Equal(current, perms) {
		return epoch, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if epoch, err = bump(ctx, exec, scope); err != nil {
		return 0, err
	}
	_, err = exec.Exec(ctx, upsertGrant, string(scope.Tenant), string(scope.Company), actor.ID, string(actor.Kind), perms, epoch, kernel.Instant(at))
	return epoch, err
}

// Revoke deactivates the actor's grant and returns the scope's epoch after the call; revoking a
// missing or inactive grant changes nothing.
func (Grants) Revoke(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, actorID string, at time.Time) (int64, error) {
	epoch, err := lockForChange(ctx, exec, scope)
	if err != nil {
		return 0, err
	}
	var kind string
	var active bool
	var current []string
	err = exec.QueryRow(ctx, readGrant, string(scope.Tenant), string(scope.Company), actorID).Scan(&kind, &active, &current)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
		return epoch, nil
	}
	if err != nil {
		return 0, err
	}
	if epoch, err = bump(ctx, exec, scope); err != nil {
		return 0, err
	}
	_, err = exec.Exec(ctx, deactivateGrant, string(scope.Tenant), string(scope.Company), actorID, epoch, kernel.Instant(at))
	return epoch, err
}

// SetDataScope gives the actor's grant a data scope and, for DataScopeTeam, a team; team is
// recorded with the other scopes too. It returns the scope's epoch after the call, which is
// unchanged when nothing changes.
func (Grants) SetDataScope(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, actorID string, dataScope DataScope, team string, at time.Time) (int64, error) {
	if !dataScope.Valid() || (team != "" && !kernel.ValidIdentifier(team)) || (dataScope == DataScopeTeam && team == "") {
		return 0, fmt.Errorf("%w: data scope %q, team %q", ErrInvalidPolicy, dataScope, team)
	}
	epoch, err := lockForChange(ctx, exec, scope)
	if err != nil {
		return 0, err
	}
	var current, currentTeam string
	err = exec.QueryRow(ctx, readDataScope, string(scope.Tenant), string(scope.Company), actorID).Scan(&current, &currentTeam)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s has no grant in %s", ErrForbidden, actorID, scope)
	}
	if err != nil {
		return 0, err
	}
	if DataScope(current) == dataScope && currentTeam == team {
		return epoch, nil
	}
	if epoch, err = bump(ctx, exec, scope); err != nil {
		return 0, err
	}
	_, err = exec.Exec(ctx, setDataScope, string(scope.Tenant), string(scope.Company), actorID, string(dataScope), team, epoch, kernel.Instant(at))
	return epoch, err
}

func lockForChange(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) (int64, error) {
	var epoch int64
	err := exec.QueryRow(ctx, lockScopeForChange, string(scope.Tenant), string(scope.Company)).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: scope %s has no authority row", ErrForbidden, scope)
	}
	return epoch, err
}

func bump(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) (int64, error) {
	var epoch int64
	err := exec.QueryRow(ctx, bumpEpoch, string(scope.Tenant), string(scope.Company)).Scan(&epoch)
	return epoch, err
}
