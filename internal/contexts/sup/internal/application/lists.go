package application

import (
	"context"
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/listquery"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The list reads (api/lists.go), audited under sup.read.
const (
	CommandListSupplierRfqs = "supplier_rfq.list"
	CommandListOffers       = "supplier_offer.list"
)

// listRead runs the audited list read command. fetch returns up to limit+1 rows of the caller's
// sight; listRead reports the page size and whether more follow. A query that fails validation is
// refused and audited.
func (s Service) listRead(ctx context.Context, command string, validation error, after string, limit int,
	fetch func(ctx context.Context, exec bentopg.Executor, sight listquery.Sight) (int, error)) (size int, more bool, err error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if aerr := s.Ledger.AuditRejected(ctx, command, "", rej.Code); aerr != nil {
				return 0, false, errors.Join(validation, aerr)
			}
		}
		return 0, false, validation
	}
	pos, _ := kernel.ListPosition(after, limit) // validation accepted it
	size = kernel.ListSize(limit)
	err = s.Ledger.Read(ctx, command, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		n, err := fetch(ctx, exec, listquery.Sight{DataScope: string(d.DataScope), ActorID: d.Actor.ID, Team: d.Team, After: pos, Limit: size + 1})
		more = n > size
		return err
	})
	return size, more, err
}

// ListSupplierRfqs returns a page of the supplier RFQs the caller sees, audited as supplier_rfq.list.
func (s Service) ListSupplierRfqs(ctx context.Context, q api.SupplierRfqQuery) (api.SupplierRfqPage, error) {
	var rows []postgres.SupplierRfqListRow
	size, more, err := s.listRead(ctx, CommandListSupplierRfqs, q.Validate(), q.After, q.Limit,
		func(ctx context.Context, exec bentopg.Executor, sight listquery.Sight) (int, error) {
			var err error
			rows, err = postgres.ListSupplierRfqs(ctx, exec, scope(ctx), q.RfqID, q.SupplierID, q.Status, sight)
			return len(rows), err
		})
	if err != nil {
		return api.SupplierRfqPage{}, err
	}
	out := api.SupplierRfqPage{SupplierRfqs: []api.SupplierRfqSummary{}}
	if more {
		rows = rows[:size]
		out.Next = kernel.ListCursor{At: rows[size-1].UpdatedAt, ID: rows[size-1].ID}.String()
	}
	for _, r := range rows {
		out.SupplierRfqs = append(out.SupplierRfqs, api.SupplierRfqSummary{SupplierRfqID: r.ID, Status: r.Status, RowVersion: r.RowVersion,
			RfqID: r.RfqID, SupplierID: r.SupplierID, Lines: r.Lines, OwnerActorID: r.OwnerActor, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

// ListOffers returns a page of the supplier offers the caller sees, audited as supplier_offer.list.
func (s Service) ListOffers(ctx context.Context, q api.OfferQuery) (api.OfferPage, error) {
	var rows []postgres.OfferListRow
	size, more, err := s.listRead(ctx, CommandListOffers, q.Validate(), q.After, q.Limit,
		func(ctx context.Context, exec bentopg.Executor, sight listquery.Sight) (int, error) {
			var err error
			rows, err = postgres.ListOffers(ctx, exec, scope(ctx), q.RfqID, q.SupplierID, q.Status, sight)
			return len(rows), err
		})
	if err != nil {
		return api.OfferPage{}, err
	}
	out := api.OfferPage{Offers: []api.OfferSummary{}}
	if more {
		rows = rows[:size]
		out.Next = kernel.ListCursor{At: rows[size-1].UpdatedAt, ID: rows[size-1].ID}.String()
	}
	for _, r := range rows {
		o := api.OfferSummary{OfferID: r.ID, RootVersion: r.RootVersion, LastRevision: r.LastRevision, Status: r.Status,
			ActiveRevision: r.ActiveRevision, RfqID: r.RfqID, SupplierID: r.SupplierID, SupplierRfqID: r.SupplierRfqID, Lines: r.Lines,
			OwnerActorID: r.OwnerActor, UpdatedAt: r.UpdatedAt}
		if r.ValidFrom != nil {
			o.ValidFrom = *r.ValidFrom
		}
		if r.ValidUntil != nil {
			o.ValidUntil = *r.ValidUntil
		}
		out.Offers = append(out.Offers, o)
	}
	return out, nil
}
