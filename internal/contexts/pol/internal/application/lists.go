package application

import (
	"context"
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/listquery"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CommandListConfigs is the audited list of the configurations of a kind (api/lists.go).
const CommandListConfigs = "config.list"

// ListConfigs returns a page of the configurations of a kind, audited as config.list. A query
// that fails validation is refused and audited. bank_account is refused as in ConfigsInForce.
func (s Service) ListConfigs(ctx context.Context, q api.ConfigQuery) (api.ConfigPage, error) {
	if err := q.Validate(); err != nil {
		if rej, ok := kernel.AsRejection(err); ok {
			if aerr := s.Ledger.AuditRejected(ctx, CommandListConfigs, q.Kind, rej.Code); aerr != nil {
				return api.ConfigPage{}, errors.Join(err, aerr)
			}
		}
		return api.ConfigPage{}, err
	}
	pos, _ := kernel.ListPosition(q.After, q.Limit) // Validate accepted it
	size := kernel.ListSize(q.Limit)
	var rows []postgres.ConfigListRow
	err := s.Ledger.Read(ctx, CommandListConfigs, q.Kind, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		if q.Kind == api.KindBankAccount {
			return kernel.Reject(kernel.CodePermissionDenied, "/kind", "which bank accounts exist follows the bank account read rules, which this list does not apply")
		}
		var err error
		rows, err = postgres.ListConfigs(ctx, exec, scope(ctx), q.Kind, q.Search, q.Active, listquery.Sight{After: pos, Limit: size + 1})
		return err
	})
	if err != nil {
		return api.ConfigPage{}, err
	}
	out := api.ConfigPage{Kind: q.Kind, Configs: []api.ConfigSummary{}}
	if len(rows) > size {
		rows = rows[:size]
		out.Next = kernel.ListCursor{At: rows[size-1].UpdatedAt, ID: rows[size-1].ID}.String()
	}
	for _, r := range rows {
		c := api.ConfigSummary{ConfigID: r.ID, RootVersion: r.RootVersion, LastRevision: r.LastRevision, Status: r.Status,
			ActiveRevision: r.ActiveRev, UpdatedAt: r.UpdatedAt}
		if r.ValidFrom != nil {
			c.ValidFrom = *r.ValidFrom
		}
		if r.ValidUntil != nil {
			c.ValidUntil = *r.ValidUntil
		}
		out.Configs = append(out.Configs, c)
	}
	return out, nil
}
