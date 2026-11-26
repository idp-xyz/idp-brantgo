package application

import (
	"context"
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/listquery"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CommandListSkus is the audited list read (api/lists.go), under cat.read.
const CommandListSkus = "sku.list"

// ListSkus returns a page of the SKUs the caller sees, audited as sku.list. A query that fails
// validation is refused and audited.
func (s Service) ListSkus(ctx context.Context, q api.SkuQuery) (api.SkuPage, error) {
	if err := q.Validate(); err != nil {
		if rej, ok := kernel.AsRejection(err); ok {
			if aerr := s.Ledger.AuditRejected(ctx, CommandListSkus, "", rej.Code); aerr != nil {
				return api.SkuPage{}, errors.Join(err, aerr)
			}
		}
		return api.SkuPage{}, err
	}
	pos, _ := kernel.ListPosition(q.After, q.Limit) // Validate accepted it
	size := kernel.ListSize(q.Limit)
	out := api.SkuPage{Skus: []api.SkuSummary{}}
	err := s.Ledger.Read(ctx, CommandListSkus, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		rows, err := postgres.ListSkus(ctx, exec, scope(ctx), q.Status, q.Search,
			listquery.Sight{DataScope: string(d.DataScope), ActorID: d.Actor.ID, Team: d.Team, After: pos, Limit: size + 1})
		if err != nil {
			return err
		}
		if len(rows) > size {
			rows = rows[:size]
			out.Next = kernel.ListCursor{At: rows[size-1].UpdatedAt, ID: rows[size-1].ID}.String()
		}
		for _, r := range rows {
			out.Skus = append(out.Skus, api.SkuSummary{SkuID: r.ID, Name: r.Name, Status: r.Status, BaseUnit: r.BaseUnit, SpecRevision: r.SpecRevision,
				RowVersion: r.RowVersion, OwnerActorID: r.OwnerActor, UpdatedAt: r.UpdatedAt})
		}
		return nil
	})
	if err != nil {
		return api.SkuPage{}, err
	}
	return out, nil
}
