// Package postgres stores PTY parties and contacts in schema brant_pty, on the executor of PTY's own
// transaction.
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/contact"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/lifecycle"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/party"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is PTY's schema.
const Schema = "brant_pty"

// Lock is the row lock a load takes.
type Lock string

const (
	NoLock    Lock = ""
	ForUpdate Lock = " FOR UPDATE" // the command's own aggregate root
	ForShare  Lock = " FOR SHARE"  // another aggregate the command relies on
)

// ErrConflict means a compare-and-set update found another row version.
var ErrConflict = errors.New("pty: row version changed")

const partyColumns = `party_id, status, row_version, revision, coalesce(name, ''), roles, owner_actor_id, coalesce(owner_team_id, '')`

func scanParty(row pgx.Row) (*party.Party, error) {
	var p party.Party
	var status string
	var roles []string
	err := row.Scan(&p.ID, &status, &p.RowVersion, &p.Revision, &p.Name, &roles, &p.OwnerActor, &p.OwnerTeam)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	p.Status = lifecycle.Status(status)
	for _, r := range roles {
		p.Roles = append(p.Roles, party.Role(r))
	}
	return &p, err
}

// LoadParty returns the party id, or nil, taking lock.
func LoadParty(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*party.Party, error) {
	return scanParty(exec.QueryRow(ctx, `SELECT `+partyColumns+` FROM brant_pty.party
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3`+string(lock), string(scope.Tenant), string(scope.Company), id))
}

// Parties returns the parties among ids, without locks.
func Parties(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ids []string) ([]*party.Party, error) {
	rows, err := exec.Query(ctx, `SELECT `+partyColumns+` FROM brant_pty.party
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = ANY ($3)`, string(scope.Tenant), string(scope.Company), ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*party.Party, error) { return scanParty(row) })
}

func roles(p *party.Party) []string {
	out := make([]string, len(p.Roles))
	for i, r := range p.Roles {
		out[i] = string(r)
	}
	return out
}

// InsertParty stores a new party.
func InsertParty(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p *party.Party, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pty.party (tenant_id, company_id, party_id, status, row_version, revision, name, roles,
		owner_actor_id, owner_team_id, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), $8, $9, nullif($10, ''), $11, $11)`,
		string(scope.Tenant), string(scope.Company), p.ID, string(p.Status), p.RowVersion, p.Revision, p.Name, roles(p), p.OwnerActor, p.OwnerTeam, at)
	return err
}

// UpdateParty writes p if the stored row is still at version from.
func UpdateParty(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p *party.Party, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_pty.party SET status = $4, row_version = $5, revision = $6, name = nullif($7, ''),
		roles = $8, updated_at = $9 WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3 AND row_version = $10`,
		string(scope.Tenant), string(scope.Company), p.ID, string(p.Status), p.RowVersion, p.Revision, p.Name, roles(p), at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// Confirmation is who confirmed a revision, on which evidence and under which authority epoch.
type Confirmation struct {
	EvidenceIDs []string
	By          kernel.ActorRef
	Epoch       int64
	At          time.Time
}

// InsertPartyRevision keeps the confirmed revision of p unchanged for good.
func InsertPartyRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p *party.Party, c Confirmation) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pty.party_revision (tenant_id, company_id, party_id, revision, row_version, name, roles,
		evidence_ids, confirmed_by_kind, confirmed_by_id, authority_epoch, confirmed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		string(scope.Tenant), string(scope.Company), p.ID, p.Revision, p.RowVersion, p.Name, roles(p), c.EvidenceIDs,
		string(c.By.Kind), c.By.ID, c.Epoch, c.At)
	return err
}

// Revision is a stored confirmed revision.
type Revision struct {
	Revision     int64
	Name         string
	Roles        []string
	Confirmation Confirmation
}

// PartyRevisions returns the confirmed revisions of a party, oldest first.
func PartyRevisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]Revision, error) {
	rows, err := exec.Query(ctx, `SELECT revision, name, roles, evidence_ids, confirmed_by_kind, confirmed_by_id, authority_epoch, confirmed_at
		FROM brant_pty.party_revision WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3 ORDER BY revision`,
		string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Revision, error) {
		var r Revision
		var kind string
		err := row.Scan(&r.Revision, &r.Name, &r.Roles, &r.Confirmation.EvidenceIDs, &kind, &r.Confirmation.By.ID,
			&r.Confirmation.Epoch, &r.Confirmation.At)
		r.Confirmation.By.Kind = kernel.ActorKind(kind)
		return r, err
	})
}

const contactColumns = `contact_id, status, row_version, revision, coalesce(name, ''), coalesce(party_id, ''), owner_actor_id, coalesce(owner_team_id, '')`

func scanContact(row pgx.Row) (*contact.Contact, error) {
	var c contact.Contact
	var status string
	err := row.Scan(&c.ID, &status, &c.RowVersion, &c.Revision, &c.Name, &c.PartyID, &c.OwnerActor, &c.OwnerTeam)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	c.Status = lifecycle.Status(status)
	return &c, err
}

// LoadContact returns the contact id, or nil, taking lock.
func LoadContact(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*contact.Contact, error) {
	return scanContact(exec.QueryRow(ctx, `SELECT `+contactColumns+` FROM brant_pty.contact
		WHERE tenant_id = $1 AND company_id = $2 AND contact_id = $3`+string(lock), string(scope.Tenant), string(scope.Company), id))
}

// Contacts returns the contacts among ids, without locks.
func Contacts(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ids []string) ([]*contact.Contact, error) {
	rows, err := exec.Query(ctx, `SELECT `+contactColumns+` FROM brant_pty.contact
		WHERE tenant_id = $1 AND company_id = $2 AND contact_id = ANY ($3)`, string(scope.Tenant), string(scope.Company), ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*contact.Contact, error) { return scanContact(row) })
}

// InsertContact stores a new contact.
func InsertContact(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *contact.Contact, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pty.contact (tenant_id, company_id, contact_id, party_id, status, row_version, revision, name,
		owner_actor_id, owner_team_id, created_at, updated_at) VALUES ($1, $2, $3, nullif($4, ''), $5, $6, $7, nullif($8, ''), $9, nullif($10, ''), $11, $11)`,
		string(scope.Tenant), string(scope.Company), c.ID, c.PartyID, string(c.Status), c.RowVersion, c.Revision, c.Name, c.OwnerActor, c.OwnerTeam, at)
	return err
}

// UpdateContact writes c if the stored row is still at version from.
func UpdateContact(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *contact.Contact, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_pty.contact SET party_id = nullif($4, ''), status = $5, row_version = $6, revision = $7,
		name = nullif($8, ''), updated_at = $9 WHERE tenant_id = $1 AND company_id = $2 AND contact_id = $3 AND row_version = $10`,
		string(scope.Tenant), string(scope.Company), c.ID, c.PartyID, string(c.Status), c.RowVersion, c.Revision, c.Name, at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// InsertContactRevision keeps the confirmed revision of c unchanged for good.
func InsertContactRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *contact.Contact, conf Confirmation) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pty.contact_revision (tenant_id, company_id, contact_id, revision, row_version, party_id, name,
		evidence_ids, confirmed_by_kind, confirmed_by_id, authority_epoch, confirmed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		string(scope.Tenant), string(scope.Company), c.ID, c.Revision, c.RowVersion, c.PartyID, c.Name, conf.EvidenceIDs,
		string(conf.By.Kind), conf.By.ID, conf.Epoch, conf.At)
	return err
}

// ContactEvidence returns the evidence of the contact's last confirmation.
func ContactEvidence(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]string, error) {
	var ids []string
	err := exec.QueryRow(ctx, `SELECT evidence_ids FROM brant_pty.contact_revision WHERE tenant_id = $1 AND company_id = $2
		AND contact_id = $3 ORDER BY revision DESC LIMIT 1`, string(scope.Tenant), string(scope.Company), id).Scan(&ids)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return ids, err
}
