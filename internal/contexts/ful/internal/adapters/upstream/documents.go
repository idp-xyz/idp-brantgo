package upstream

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// POLConfig is FUL's port to POL's configuration and numbering (R49): ConfigRef v1 lookups,
// ConfigGuard v1 with FUL's executor and NumberIssue v1.
type POLConfig struct {
	Queries  polapi.Queries
	Commands polapi.Commands
}

func (p POLConfig) ResolveConfigs(ctx context.Context, refs []polapi.ConfigRef, at time.Time) (map[polapi.ConfigRef]polapi.ResolvedConfig, error) {
	return p.Queries.ResolveConfigs(ctx, refs, at)
}

// GuardConfig runs ConfigGuard v1, which locks the revision row FOR SHARE until FUL's transaction
// ends.
func (POLConfig) GuardConfig(ctx context.Context, exec bentopg.Executor, ref polapi.ConfigRef, at time.Time) (polapi.ConfigGuardVerdict, error) {
	return polapi.ScanConfigGuard(ref, exec.QueryRow(ctx, polapi.ConfigGuardSQL, polapi.ConfigGuardArgs(ref, at)...).Scan)
}

func (p POLConfig) IssueNumber(ctx context.Context, cmd polapi.IssueNumber) (polapi.Receipt, error) {
	return p.Commands.IssueNumber(ctx, cmd)
}

// PTYParties is FUL's port to PTY's parties and service roles (R44).
type PTYParties struct {
	Parties  ptyapi.Queries
	Profiles ptyapi.ProfileQueries
}

// PartyName reads the party (audited, pty.read) and returns its name at revision; a party the actor
// does not see is not found.
func (p PTYParties) PartyName(ctx context.Context, partyID string, revision int64) (string, bool, error) {
	v, err := p.Parties.GetParty(ctx, partyID)
	if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	for _, r := range v.Revisions {
		if r.Revision == revision {
			return r.Name, true, nil
		}
	}
	return "", false, nil
}

func (p PTYParties) ResolveServiceRoles(ctx context.Context, refs []ptyapi.ServiceRoleRef, at time.Time) (map[ptyapi.ServiceRoleRef]ptyapi.ResolvedServiceRole, error) {
	return p.Profiles.ResolveServiceRoles(ctx, refs, at)
}
