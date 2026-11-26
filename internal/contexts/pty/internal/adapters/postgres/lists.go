package postgres

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/listquery"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// PartyListRow is one party of a list.
type PartyListRow struct {
	ID, Name, Status, OwnerActor string
	Roles                        []string
	Revision, RowVersion         int64
	UpdatedAt                    time.Time
}

// ListParties returns the parties of the scope the caller sees, in (updated_at, party_id)
// descending order (index party_listing), without locks. status, role and search narrow when
// not empty; search matches the name or the ID.
func ListParties(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, status, role, search string, s listquery.Sight) ([]PartyListRow, error) {
	var w listquery.Where
	w.Scope("", scope)
	if status != "" {
		w.Add("status = " + w.Arg(status))
	}
	if role != "" {
		w.Add(w.Arg(role) + " = ANY(roles)")
	}
	if search != "" {
		w.Contains(search, "name", "party_id")
	}
	w.Sees(s, "owner_actor_id", "owner_team_id")
	w.After(s, "updated_at", "party_id")
	rows, err := exec.Query(ctx, `SELECT party_id, coalesce(name, ''), status, roles, revision, row_version, owner_actor_id, updated_at
		FROM brant_pty.party WHERE `+w.SQL()+` ORDER BY updated_at DESC, party_id DESC LIMIT `+w.Arg(s.Limit), w.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PartyListRow
	for rows.Next() {
		var r PartyListRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Status, &r.Roles, &r.Revision, &r.RowVersion, &r.OwnerActor, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ContactListRow is one contact of a list.
type ContactListRow struct {
	ID, PartyID, Name, Status, OwnerActor string
	Revision, RowVersion                  int64
	UpdatedAt                             time.Time
}

// ListContacts returns the contacts of the scope the caller sees, in (updated_at, contact_id)
// descending order (index contact_listing), without locks. party, status and search narrow when
// not empty; search matches the name or the ID.
func ListContacts(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, party, status, search string, s listquery.Sight) ([]ContactListRow, error) {
	var w listquery.Where
	w.Scope("", scope)
	if party != "" {
		w.Add("party_id = " + w.Arg(party))
	}
	if status != "" {
		w.Add("status = " + w.Arg(status))
	}
	if search != "" {
		w.Contains(search, "name", "contact_id")
	}
	w.Sees(s, "owner_actor_id", "owner_team_id")
	w.After(s, "updated_at", "contact_id")
	rows, err := exec.Query(ctx, `SELECT contact_id, coalesce(party_id, ''), coalesce(name, ''), status, revision, row_version, owner_actor_id, updated_at
		FROM brant_pty.contact WHERE `+w.SQL()+` ORDER BY updated_at DESC, contact_id DESC LIMIT `+w.Arg(s.Limit), w.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactListRow
	for rows.Next() {
		var r ContactListRow
		if err := rows.Scan(&r.ID, &r.PartyID, &r.Name, &r.Status, &r.Revision, &r.RowVersion, &r.OwnerActor, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
