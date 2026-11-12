package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
)

// Configs is FUL's port to POL's configuration (Context Map R49): ConfigRef v1 before FUL's
// transaction, ConfigGuard v1 with FUL's executor inside it.
type Configs interface {
	ResolveConfigs(ctx context.Context, refs []polapi.ConfigRef, at time.Time) (map[polapi.ConfigRef]polapi.ResolvedConfig, error)
	GuardConfig(ctx context.Context, exec bentopg.Executor, ref polapi.ConfigRef, at time.Time) (polapi.ConfigGuardVerdict, error)
}

// Numbers is POL's NumberIssue v1 (R49): number.issue, which POL runs as the acting actor in a POL
// transaction before FUL's.
type Numbers interface {
	IssueNumber(ctx context.Context, cmd polapi.IssueNumber) (polapi.Receipt, error)
}

// Parties is FUL's port to PTY (R44): a party's name at a confirmed revision (the audited party
// read, pty.read; ok false for a party or revision the actor does not see) and ServiceRoleRef v1.
type Parties interface {
	PartyName(ctx context.Context, partyID string, revision int64) (name string, ok bool, err error)
	ResolveServiceRoles(ctx context.Context, refs []ptyapi.ServiceRoleRef, at time.Time) (map[ptyapi.ServiceRoleRef]ptyapi.ResolvedServiceRole, error)
}

// OrderSnapshots is FUL's port to TRD's OrderCommercialSnapshot v1 (R27): an order's current
// revision with its terms and address roles, in every order state.
type OrderSnapshots interface {
	ResolveOrderSnapshots(ctx context.Context, orderIDs []string) (map[string]trdapi.OrderSnapshotFact, error)
}

// DocumentUpstream are the upstreams of booking.record and the shipment document commands (WBS
// S5-08).
type DocumentUpstream struct {
	Configs Configs
	Numbers Numbers
	Parties Parties
	Orders  OrderSnapshots
}

// Complete reports whether every document upstream is wired.
func (u DocumentUpstream) Complete() bool {
	return u.Configs != nil && u.Numbers != nil && u.Parties != nil && u.Orders != nil
}
