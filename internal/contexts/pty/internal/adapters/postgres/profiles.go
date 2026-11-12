package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/contact"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/party"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/profile"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ContactsLocked returns the contacts among ids, locked in ID order.
func ContactsLocked(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ids []string, lock Lock) ([]*contact.Contact, error) {
	rows, err := exec.Query(ctx, `SELECT `+contactColumns+` FROM brant_pty.contact
		WHERE tenant_id = $1 AND company_id = $2 AND contact_id = ANY ($3) ORDER BY contact_id`+string(lock), scoped(scope, ids)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*contact.Contact, error) { return scanContact(row) })
}

// AddressKey names an address of a confirmed party revision (AddressRoleRef v1).
type AddressKey struct {
	PartyID   string
	Revision  int64
	AddressID string
}

// RevisionAddresses returns the addresses among keys that confirmed revisions keep.
func RevisionAddresses(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, keys []AddressKey) (map[AddressKey]party.Address, error) {
	parties, revisions, ids := make([]string, len(keys)), make([]int64, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		parties[i], revisions[i], ids[i] = k.PartyID, k.Revision, k.AddressID
	}
	rows, err := exec.Query(ctx, `SELECT party_id, revision, address_id, role, address, coalesce(country, '') FROM brant_pty.party_revision_address
		WHERE tenant_id = $1 AND company_id = $2
		AND (party_id, revision, address_id) IN (SELECT * FROM unnest($3::text[], $4::bigint[], $5::text[]))`,
		scoped(scope, parties, revisions, ids)...)
	if err != nil {
		return nil, err
	}
	out := map[AddressKey]party.Address{}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var k AddressKey
		var a party.Address
		var role string
		err := row.Scan(&k.PartyID, &k.Revision, &a.ID, &role, &a.Text, &a.Country)
		k.AddressID, a.Role = a.ID, party.AddressRole(role)
		out[k] = a
		return struct{}{}, err
	})
	return out, err
}

const serviceRoleColumns = `s.party_id, s.service_role_id, s.revision, s.party_revision, s.role, s.service_code, s.origin, s.destination,
	s.valid_from, s.valid_until, s.evidence_ids, s.verified_by_kind, s.verified_by_id, s.authority_epoch, s.verified_at`

// StoredServiceRole is a verified service role revision with its party and the latest revision of
// its role.
type StoredServiceRole struct {
	PartyID string
	Role    party.ServiceRole
	Latest  int64
}

func scanServiceRole(row pgx.CollectableRow) (StoredServiceRole, error) {
	var s StoredServiceRole
	var kind, verifiedKind string
	r := &s.Role
	err := row.Scan(&s.PartyID, &r.ID, &r.Revision, &r.PartyRevision, &kind, &r.ServiceCode, &r.Origin, &r.Destination,
		&r.Validity.From, &r.Validity.Until, &r.EvidenceIDs, &verifiedKind, &r.VerifiedBy.ID, &r.Epoch, &r.At, &s.Latest)
	r.Kind, r.VerifiedBy.Kind = party.ServiceKind(kind), kernel.ActorKind(verifiedKind)
	r.Validity.From, r.Validity.Until, r.At = kernel.Instant(r.Validity.From), kernel.Instant(r.Validity.Until), kernel.Instant(r.At)
	return s, err
}

const latestServiceRole = `(SELECT max(l.revision) FROM brant_pty.service_role l WHERE l.tenant_id = s.tenant_id
	AND l.company_id = s.company_id AND l.party_id = s.party_id AND l.service_role_id = s.service_role_id)`

// LatestServiceRole returns the latest revision of a party's service role, 0 when it has none. The
// caller holds the party FOR UPDATE, which serializes the revisions of its roles.
func LatestServiceRole(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID, roleID string) (int64, error) {
	var latest int64
	err := exec.QueryRow(ctx, `SELECT coalesce(max(revision), 0) FROM brant_pty.service_role
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3 AND service_role_id = $4`, scoped(scope, partyID, roleID)...).Scan(&latest)
	return latest, err
}

// InsertServiceRole appends a verified revision of a party's service role.
func InsertServiceRole(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID string, r party.ServiceRole) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pty.service_role (tenant_id, company_id, party_id, service_role_id, revision, party_revision,
		role, service_code, origin, destination, valid_from, valid_until, evidence_ids, verified_by_kind, verified_by_id, authority_epoch,
		verified_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		scoped(scope, partyID, r.ID, r.Revision, r.PartyRevision, string(r.Kind), r.ServiceCode, r.Origin, r.Destination, r.Validity.From,
			r.Validity.Until, r.EvidenceIDs, string(r.VerifiedBy.Kind), r.VerifiedBy.ID, r.Epoch, r.At)...)
	return err
}

// ServiceRoles returns every verified revision of a party's service roles, by role ID and revision.
func ServiceRoles(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID string) ([]StoredServiceRole, error) {
	rows, err := exec.Query(ctx, `SELECT `+serviceRoleColumns+`, `+latestServiceRole+` FROM brant_pty.service_role s
		WHERE s.tenant_id = $1 AND s.company_id = $2 AND s.party_id = $3 ORDER BY s.service_role_id, s.revision`, scoped(scope, partyID)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanServiceRole)
}

// ServiceRoleKey names one revision of a service role (ServiceRoleRef v1).
type ServiceRoleKey struct {
	PartyID       string
	ServiceRoleID string
	Revision      int64
}

// ServiceRolesByKey returns the service role revisions among keys that exist.
func ServiceRolesByKey(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, keys []ServiceRoleKey) (map[ServiceRoleKey]StoredServiceRole, error) {
	parties, ids, revisions := make([]string, len(keys)), make([]string, len(keys)), make([]int64, len(keys))
	for i, k := range keys {
		parties[i], ids[i], revisions[i] = k.PartyID, k.ServiceRoleID, k.Revision
	}
	rows, err := exec.Query(ctx, `SELECT `+serviceRoleColumns+`, `+latestServiceRole+` FROM brant_pty.service_role s
		WHERE s.tenant_id = $1 AND s.company_id = $2
		AND (s.party_id, s.service_role_id, s.revision) IN (SELECT * FROM unnest($3::text[], $4::text[], $5::bigint[]))`,
		scoped(scope, parties, ids, revisions)...)
	if err != nil {
		return nil, err
	}
	stored, err := pgx.CollectRows(rows, scanServiceRole)
	out := make(map[ServiceRoleKey]StoredServiceRole, len(stored))
	for _, s := range stored {
		out[ServiceRoleKey{PartyID: s.PartyID, ServiceRoleID: s.Role.ID, Revision: s.Role.Revision}] = s
	}
	return out, err
}

const profileRevisionColumns = `party_id, revision, status, content_hash, party_revision, payment_terms_id, payment_terms_revision,
	trade_terms_id, trade_terms_revision, owner_actor_id, coalesce(owner_team_id, ''), saved_by_kind, saved_by_id, saved_epoch, saved_at,
	published_by_kind, published_by_id, publish_epoch, published_at, evidence_ids, ended_at`

// ProfileKey names one revision of a party's trading profile (TradingProfileRef v1).
type ProfileKey struct {
	PartyID  string
	Revision int64
}

func scanProfileRevision(row pgx.CollectableRow) (ProfileKey, *profile.Revision, error) {
	var k ProfileKey
	var r profile.Revision
	var status, savedKind string
	var publishedKind, publishedID *string
	var publishEpoch *int64
	var publishedAt, endedAt *time.Time
	var evidence []string
	c := &r.Content
	err := row.Scan(&k.PartyID, &k.Revision, &status, &r.Hash, &c.PartyRevision, &c.PaymentTerms.ConfigID, &c.PaymentTerms.Revision,
		&c.TradeTerms.ConfigID, &c.TradeTerms.Revision, &c.Owner.ActorID, &c.Owner.TeamID, &savedKind, &r.SavedBy.ID, &r.SavedEpoch,
		&r.SavedAt, &publishedKind, &publishedID, &publishEpoch, &publishedAt, &evidence, &endedAt)
	if err != nil {
		return k, nil, err
	}
	r.Number, r.Status, r.SavedBy.Kind, r.SavedAt = k.Revision, profile.Status(status), kernel.ActorKind(savedKind), kernel.Instant(r.SavedAt)
	c.PaymentTerms.Kind, c.TradeTerms.Kind, c.Contacts = profile.KindPaymentTerms, profile.KindTradeTerms, []profile.ContactRef{}
	if publishedAt != nil {
		r.Publication = &profile.Publication{By: kernel.ActorRef{Kind: kernel.ActorKind(*publishedKind), ID: *publishedID},
			Epoch: *publishEpoch, At: kernel.Instant(*publishedAt), EvidenceIDs: evidence}
	}
	if endedAt != nil {
		r.EndedAt = kernel.Instant(*endedAt)
	}
	return k, &r, nil
}

// profileRevisions collects the revisions of rows and loads their contacts.
func profileRevisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, rows pgx.Rows, err error) (map[ProfileKey]*profile.Revision, []ProfileKey, error) {
	if err != nil {
		return nil, nil, err
	}
	out := map[ProfileKey]*profile.Revision{}
	var order []ProfileKey
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		k, r, err := scanProfileRevision(row)
		if err == nil {
			out[k], order = r, append(order, k)
		}
		return struct{}{}, err
	})
	if err != nil || len(order) == 0 {
		return out, order, err
	}
	parties, revisions := make([]string, len(order)), make([]int64, len(order))
	for i, k := range order {
		parties[i], revisions[i] = k.PartyID, k.Revision
	}
	rows, err = exec.Query(ctx, `SELECT party_id, revision, contact_id, contact_row_version FROM brant_pty.trading_profile_contact
		WHERE tenant_id = $1 AND company_id = $2 AND (party_id, revision) IN (SELECT * FROM unnest($3::text[], $4::bigint[]))
		ORDER BY party_id, revision, contact_id`, scoped(scope, parties, revisions)...)
	if err != nil {
		return nil, nil, err
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var k ProfileKey
		var c profile.ContactRef
		err := row.Scan(&k.PartyID, &k.Revision, &c.ContactID, &c.RowVersion)
		if r := out[k]; err == nil && r != nil {
			r.Content.Contacts = append(r.Content.Contacts, c)
		}
		return struct{}{}, err
	})
	return out, order, err
}

// LoadProfile returns the trading profile of partyID with its working and active revisions, or nil,
// taking lock on the root.
func LoadProfile(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID string, lock Lock) (*profile.Root, error) {
	r := profile.New(partyID)
	err := exec.QueryRow(ctx, `SELECT root_version, last_revision FROM brant_pty.trading_profile
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3`+string(lock), scoped(scope, partyID)...).Scan(&r.Version, &r.LastRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := exec.Query(ctx, `SELECT `+profileRevisionColumns+` FROM brant_pty.trading_profile_revision
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3 AND status IN ('working', 'active') ORDER BY revision`, scoped(scope, partyID)...)
	revisions, _, err := profileRevisions(ctx, exec, scope, rows, err)
	for _, rev := range revisions {
		switch rev.Status {
		case profile.Working:
			r.Working = rev
		case profile.Active:
			r.Active = rev
		}
	}
	return r, err
}

// ProfileRevisions returns every revision of a party's trading profile, oldest first.
func ProfileRevisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID string) ([]*profile.Revision, error) {
	rows, err := exec.Query(ctx, `SELECT `+profileRevisionColumns+` FROM brant_pty.trading_profile_revision
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3 ORDER BY revision`, scoped(scope, partyID)...)
	revisions, order, err := profileRevisions(ctx, exec, scope, rows, err)
	out := make([]*profile.Revision, len(order))
	for i, k := range order {
		out[i] = revisions[k]
	}
	return out, err
}

// ActiveProfiles returns the active revision number of the trading profiles of partyIDs that have
// one.
func ActiveProfiles(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyIDs []string) (map[string]int64, error) {
	rows, err := exec.Query(ctx, `SELECT party_id, revision FROM brant_pty.trading_profile_revision
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = ANY ($3) AND status = 'active'`, scoped(scope, partyIDs)...)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var id string
		var revision int64
		err := row.Scan(&id, &revision)
		out[id] = revision
		return struct{}{}, err
	})
	return out, err
}

// PublishedProfiles returns the published revisions among keys.
func PublishedProfiles(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, keys []ProfileKey) (map[ProfileKey]*profile.Revision, error) {
	parties, revisions := make([]string, len(keys)), make([]int64, len(keys))
	for i, k := range keys {
		parties[i], revisions[i] = k.PartyID, k.Revision
	}
	rows, err := exec.Query(ctx, `SELECT `+profileRevisionColumns+` FROM brant_pty.trading_profile_revision
		WHERE tenant_id = $1 AND company_id = $2 AND status <> 'working'
		AND (party_id, revision) IN (SELECT * FROM unnest($3::text[], $4::bigint[]))`, scoped(scope, parties, revisions)...)
	out, _, err := profileRevisions(ctx, exec, scope, rows, err)
	return out, err
}

// InsertProfile stores a new trading profile root.
func InsertProfile(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *profile.Root, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pty.trading_profile (tenant_id, company_id, party_id, root_version, last_revision,
		created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $6)`, scoped(scope, r.PartyID, r.Version, r.LastRevision, at)...)
	return err
}

// UpdateProfile writes the root r if it is still at version from.
func UpdateProfile(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *profile.Root, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_pty.trading_profile SET root_version = $4, last_revision = $5, updated_at = $6
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3 AND root_version = $7`,
		scoped(scope, r.PartyID, r.Version, r.LastRevision, at, from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

type profileRow struct {
	publishedKind, publishedID *string
	publishEpoch               *int64
	publishedAt, endedAt       *time.Time
	evidence                   []string
	team                       *string
}

func profileRowOf(rev *profile.Revision) profileRow {
	var w profileRow
	if p := rev.Publication; p != nil {
		kind := string(p.By.Kind)
		w.publishedKind, w.publishedID, w.publishEpoch, w.publishedAt, w.evidence = &kind, &p.By.ID, &p.Epoch, &p.At, p.EvidenceIDs
	}
	if !rev.EndedAt.IsZero() {
		w.endedAt = &rev.EndedAt
	}
	if rev.Content.Owner.TeamID != "" {
		w.team = &rev.Content.Owner.TeamID
	}
	return w
}

// InsertProfileRevision stores a new revision of the trading profile of partyID with its contacts.
func InsertProfileRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID string, rev *profile.Revision) error {
	w, c := profileRowOf(rev), rev.Content
	_, err := exec.Exec(ctx, `INSERT INTO brant_pty.trading_profile_revision (tenant_id, company_id, party_id, revision, status, content_hash,
		party_revision, payment_terms_id, payment_terms_revision, trade_terms_id, trade_terms_revision, owner_actor_id, owner_team_id,
		saved_by_kind, saved_by_id, saved_epoch, saved_at, published_by_kind, published_by_id, publish_epoch, published_at, evidence_ids,
		ended_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)`,
		scoped(scope, partyID, rev.Number, string(rev.Status), rev.Hash, c.PartyRevision, c.PaymentTerms.ConfigID, c.PaymentTerms.Revision,
			c.TradeTerms.ConfigID, c.TradeTerms.Revision, c.Owner.ActorID, w.team, string(rev.SavedBy.Kind), rev.SavedBy.ID, rev.SavedEpoch,
			rev.SavedAt, w.publishedKind, w.publishedID, w.publishEpoch, w.publishedAt, w.evidence, w.endedAt)...)
	if err != nil {
		return err
	}
	return insertProfileContacts(ctx, exec, scope, partyID, rev)
}

func insertProfileContacts(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID string, rev *profile.Revision) error {
	for _, c := range rev.Content.Contacts {
		_, err := exec.Exec(ctx, `INSERT INTO brant_pty.trading_profile_contact (tenant_id, company_id, party_id, revision, contact_id,
			contact_row_version) VALUES ($1, $2, $3, $4, $5, $6)`, scoped(scope, partyID, rev.Number, c.ContactID, c.RowVersion)...)
		if err != nil {
			return err
		}
	}
	return nil
}

// UpdateProfileRevision writes rev; a working revision's contacts are replaced with its content's.
func UpdateProfileRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID string, rev *profile.Revision) error {
	w, c := profileRowOf(rev), rev.Content
	tag, err := exec.Exec(ctx, `UPDATE brant_pty.trading_profile_revision SET status = $5, content_hash = $6, party_revision = $7,
		payment_terms_id = $8, payment_terms_revision = $9, trade_terms_id = $10, trade_terms_revision = $11, owner_actor_id = $12,
		owner_team_id = $13, saved_by_kind = $14, saved_by_id = $15, saved_epoch = $16, saved_at = $17, published_by_kind = $18,
		published_by_id = $19, publish_epoch = $20, published_at = $21, evidence_ids = $22, ended_at = $23
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3 AND revision = $4`,
		scoped(scope, partyID, rev.Number, string(rev.Status), rev.Hash, c.PartyRevision, c.PaymentTerms.ConfigID, c.PaymentTerms.Revision,
			c.TradeTerms.ConfigID, c.TradeTerms.Revision, c.Owner.ActorID, w.team, string(rev.SavedBy.Kind), rev.SavedBy.ID, rev.SavedEpoch,
			rev.SavedAt, w.publishedKind, w.publishedID, w.publishEpoch, w.publishedAt, w.evidence, w.endedAt)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err != nil || rev.Status != profile.Working {
		return err
	}
	_, err = exec.Exec(ctx, `DELETE FROM brant_pty.trading_profile_contact WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3
		AND revision = $4`, scoped(scope, partyID, rev.Number)...)
	if err != nil {
		return err
	}
	return insertProfileContacts(ctx, exec, scope, partyID, rev)
}
