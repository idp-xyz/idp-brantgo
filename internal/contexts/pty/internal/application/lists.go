package application

import (
	"context"
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/listquery"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The list reads (api/lists.go), audited under pty.read.
const (
	CommandListParties  = "party.list"
	CommandListContacts = "contact.list"
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

// ListParties returns a page of the parties the caller sees, audited as party.list.
func (s Service) ListParties(ctx context.Context, q api.PartyQuery) (api.PartyPage, error) {
	var rows []postgres.PartyListRow
	size, more, err := s.listRead(ctx, CommandListParties, q.Validate(), q.After, q.Limit,
		func(ctx context.Context, exec bentopg.Executor, sight listquery.Sight) (int, error) {
			var err error
			rows, err = postgres.ListParties(ctx, exec, scope(ctx), q.Status, q.Role, q.Search, sight)
			return len(rows), err
		})
	if err != nil {
		return api.PartyPage{}, err
	}
	out := api.PartyPage{Parties: []api.PartySummary{}}
	if more {
		rows = rows[:size]
		out.Next = kernel.ListCursor{At: rows[size-1].UpdatedAt, ID: rows[size-1].ID}.String()
	}
	for _, r := range rows {
		out.Parties = append(out.Parties, api.PartySummary{PartyID: r.ID, Name: r.Name, Status: r.Status, Roles: r.Roles, Revision: r.Revision,
			RowVersion: r.RowVersion, OwnerActorID: r.OwnerActor, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

// ListContacts returns a page of the contacts the caller sees, audited as contact.list.
func (s Service) ListContacts(ctx context.Context, q api.ContactQuery) (api.ContactPage, error) {
	var rows []postgres.ContactListRow
	size, more, err := s.listRead(ctx, CommandListContacts, q.Validate(), q.After, q.Limit,
		func(ctx context.Context, exec bentopg.Executor, sight listquery.Sight) (int, error) {
			var err error
			rows, err = postgres.ListContacts(ctx, exec, scope(ctx), q.PartyID, q.Status, q.Search, sight)
			return len(rows), err
		})
	if err != nil {
		return api.ContactPage{}, err
	}
	out := api.ContactPage{Contacts: []api.ContactSummary{}}
	if more {
		rows = rows[:size]
		out.Next = kernel.ListCursor{At: rows[size-1].UpdatedAt, ID: rows[size-1].ID}.String()
	}
	for _, r := range rows {
		out.Contacts = append(out.Contacts, api.ContactSummary{ContactID: r.ID, PartyID: r.PartyID, Name: r.Name, Status: r.Status,
			Revision: r.Revision, RowVersion: r.RowVersion, OwnerActorID: r.OwnerActor, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}
