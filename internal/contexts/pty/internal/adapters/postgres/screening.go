package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/screening"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func scoped(scope kernel.Scope, more ...any) []any {
	return append([]any{string(scope.Tenant), string(scope.Company)}, more...)
}

// LoadScreeningRoot returns the screening root of a party, or nil, taking lock.
func LoadScreeningRoot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID string, lock Lock) (*screening.Root, error) {
	var r screening.Root
	var status string
	err := exec.QueryRow(ctx, `SELECT party_id, row_version, coalesce(current_screening_id, ''), coalesce(status, ''), negative
		FROM brant_pty.party_screening WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3`+string(lock),
		scoped(scope, partyID)...).Scan(&r.PartyID, &r.RowVersion, &r.Current, &status, &r.Negative)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	r.Status = screening.Status(status)
	return &r, err
}

// SaveScreeningRoot inserts the root at row version 1 or moves it from row version from.
func SaveScreeningRoot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *screening.Root, from int64, at time.Time) error {
	if from == 0 {
		_, err := exec.Exec(ctx, `INSERT INTO brant_pty.party_screening (tenant_id, company_id, party_id, row_version, current_screening_id,
			status, negative, created_at, updated_at) VALUES ($1, $2, $3, $4, nullif($5, ''), nullif($6, ''), $7, $8, $8)`,
			scoped(scope, r.PartyID, r.RowVersion, r.Current, string(r.Status), r.Negative, at)...)
		return err
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_pty.party_screening SET row_version = $4, current_screening_id = nullif($5, ''),
		status = nullif($6, ''), negative = $7, updated_at = $8 WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3 AND row_version = $9`,
		scoped(scope, r.PartyID, r.RowVersion, r.Current, string(r.Status), r.Negative, at, from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// InsertScreeningResult appends a conclusion with the list versions it used and its hits.
func InsertScreeningResult(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *screening.Result) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pty.screening_result (tenant_id, company_id, screening_id, party_id, party_revision,
		screened_name, regulation_id, regulation_revision, regulation_hash, outcome, status, missing_lists, normalization,
		screened_by_kind, screened_by_id, authority_epoch, screened_at, valid_until)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		scoped(scope, r.ID, r.PartyID, r.PartyRevision, r.Name, r.Regulation.ID, r.Regulation.Revision, r.Regulation.Hash,
			string(r.Outcome), string(r.Status), r.Missing, r.Normalization, string(r.By.Kind), r.By.ID, r.Epoch, r.At, r.ValidUntil)...)
	for _, l := range r.Lists {
		if err != nil {
			return err
		}
		_, err = exec.Exec(ctx, `INSERT INTO brant_pty.screening_list_used (tenant_id, company_id, screening_id, list_code, jurisdiction,
			version, content_hash) VALUES ($1, $2, $3, $4, $5, $6, $7)`, scoped(scope, r.ID, l.ListCode, l.Jurisdiction, l.Version, l.ContentHash)...)
	}
	for _, h := range r.Hits {
		if err != nil {
			return err
		}
		_, err = exec.Exec(ctx, `INSERT INTO brant_pty.screening_hit (tenant_id, company_id, screening_id, list_code, entry_id, version,
			entry_content_hash, primary_name, matched_name, name_key, disposition, positive_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, nullif($12, ''))`,
			scoped(scope, r.ID, h.ListCode, h.EntryID, h.Version, h.EntryHash, h.PrimaryName, h.MatchedName, h.NameKey,
				string(h.Disposition), h.PositiveID)...)
	}
	return err
}

// Decision is a stored decision on a suspected match.
type Decision struct {
	Decision screening.Decision
	Reason   string
	By       kernel.ActorRef
	Epoch    int64
	At       time.Time
}

// InsertDecision appends the decision on a conclusion and the positive-list entries it registers.
func InsertDecision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, res *screening.Result, d Decision, positives []screening.Positive) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pty.screening_decision (tenant_id, company_id, screening_id, party_id, decision, reason,
		decided_by_kind, decided_by_id, authority_epoch, decided_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		scoped(scope, res.ID, res.PartyID, string(d.Decision), d.Reason, string(d.By.Kind), d.By.ID, d.Epoch, d.At)...)
	for _, p := range positives {
		if err != nil {
			return err
		}
		_, err = exec.Exec(ctx, `INSERT INTO brant_pty.positive_list_entry (tenant_id, company_id, positive_id, party_id, party_revision,
			list_code, entry_id, entry_content_hash, screening_id, reason, registered_by_kind, registered_by_id, authority_epoch, registered_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			scoped(scope, p.ID, p.PartyID, p.PartyRevision, p.ListCode, p.EntryID, p.EntryHash, p.ScreeningID, p.Reason,
				string(p.By.Kind), p.By.ID, p.Epoch, p.At)...)
	}
	return err
}

// Negative is a stored negative-list entry.
type Negative struct {
	Reason string
	By     kernel.ActorRef
	Epoch  int64
	At     time.Time
}

// InsertNegative appends the party's negative-list entry.
func InsertNegative(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID string, n Negative) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pty.negative_list_entry (tenant_id, company_id, party_id, reason, blocked_by_kind,
		blocked_by_id, authority_epoch, blocked_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		scoped(scope, partyID, n.Reason, string(n.By.Kind), n.By.ID, n.Epoch, n.At)...)
	return err
}

// LoadNegative returns the party's negative-list entry, or nil.
func LoadNegative(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID string) (*Negative, error) {
	var n Negative
	var kind string
	err := exec.QueryRow(ctx, `SELECT reason, blocked_by_kind, blocked_by_id, authority_epoch, blocked_at FROM brant_pty.negative_list_entry
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3`, scoped(scope, partyID)...).Scan(&n.Reason, &kind, &n.By.ID, &n.Epoch, &n.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	n.By.Kind, n.At = kernel.ActorKind(kind), kernel.Instant(n.At)
	return &n, err
}

// StoredResult is a conclusion with its decision, if any.
type StoredResult struct {
	*screening.Result
	Decision *Decision
}

const resultColumns = `r.screening_id, r.party_id, r.party_revision, r.screened_name, r.regulation_id, r.regulation_revision,
	r.regulation_hash, r.outcome, r.status, r.missing_lists, r.normalization, r.screened_by_kind, r.screened_by_id, r.authority_epoch,
	r.screened_at, r.valid_until, d.decision, d.reason, d.decided_by_kind, d.decided_by_id, d.authority_epoch, d.decided_at`

// ScreeningResults returns the conclusions of a party, oldest first, or only the one with id when
// id is not empty, with their lists, hits and decisions.
func ScreeningResults(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID, id string) ([]StoredResult, error) {
	rows, err := exec.Query(ctx, `SELECT `+resultColumns+` FROM brant_pty.screening_result r
		LEFT JOIN brant_pty.screening_decision d ON d.tenant_id = r.tenant_id AND d.company_id = r.company_id AND d.screening_id = r.screening_id
		WHERE r.tenant_id = $1 AND r.company_id = $2 AND r.party_id = $3 AND ($4 = '' OR r.screening_id = $4)
		ORDER BY r.screened_at, r.screening_id`, scoped(scope, partyID, id)...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (StoredResult, error) {
		r := &screening.Result{}
		var outcome, status, byKind string
		var decision, reason, decidedKind, decidedID *string
		var decidedEpoch *int64
		var decidedAt *time.Time
		err := row.Scan(&r.ID, &r.PartyID, &r.PartyRevision, &r.Name, &r.Regulation.ID, &r.Regulation.Revision, &r.Regulation.Hash,
			&outcome, &status, &r.Missing, &r.Normalization, &byKind, &r.By.ID, &r.Epoch, &r.At, &r.ValidUntil,
			&decision, &reason, &decidedKind, &decidedID, &decidedEpoch, &decidedAt)
		r.Outcome, r.Status, r.By.Kind = screening.Outcome(outcome), screening.Status(status), kernel.ActorKind(byKind)
		r.At, r.ValidUntil = kernel.Instant(r.At), kernel.Instant(r.ValidUntil)
		s := StoredResult{Result: r}
		if decision != nil {
			s.Decision = &Decision{Decision: screening.Decision(*decision), Reason: *reason,
				By: kernel.ActorRef{Kind: kernel.ActorKind(*decidedKind), ID: *decidedID}, Epoch: *decidedEpoch, At: kernel.Instant(*decidedAt)}
		}
		return s, err
	})
	if err != nil || len(out) == 0 {
		return out, err
	}
	byID := map[string]*screening.Result{}
	ids := make([]string, len(out))
	for i, s := range out {
		byID[s.ID], ids[i] = s.Result, s.ID
	}
	rows, err = exec.Query(ctx, `SELECT screening_id, list_code, jurisdiction, version, content_hash FROM brant_pty.screening_list_used
		WHERE tenant_id = $1 AND company_id = $2 AND screening_id = ANY ($3) ORDER BY screening_id, list_code`, scoped(scope, ids)...)
	if err != nil {
		return nil, err
	}
	var sid string
	var l screening.ListVersion
	_, err = pgx.ForEachRow(rows, []any{&sid, &l.ListCode, &l.Jurisdiction, &l.Version, &l.ContentHash}, func() error {
		byID[sid].Lists = append(byID[sid].Lists, l)
		return nil
	})
	if err != nil {
		return nil, err
	}
	rows, err = exec.Query(ctx, `SELECT screening_id, list_code, entry_id, version, entry_content_hash, primary_name, matched_name, name_key,
		disposition, coalesce(positive_id, '') FROM brant_pty.screening_hit
		WHERE tenant_id = $1 AND company_id = $2 AND screening_id = ANY ($3) ORDER BY screening_id, list_code, entry_id`, scoped(scope, ids)...)
	if err != nil {
		return nil, err
	}
	var h screening.Hit
	var disposition string
	_, err = pgx.ForEachRow(rows, []any{&sid, &h.ListCode, &h.EntryID, &h.Version, &h.EntryHash, &h.PrimaryName, &h.MatchedName, &h.NameKey,
		&disposition, &h.PositiveID}, func() error {
		h.Disposition = screening.Disposition(disposition)
		byID[sid].Hits = append(byID[sid].Hits, h)
		return nil
	})
	return out, err
}

// Positives returns the party's positive-list entries, oldest first.
func Positives(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID string) ([]screening.Positive, error) {
	rows, err := exec.Query(ctx, `SELECT positive_id, party_id, party_revision, list_code, entry_id, entry_content_hash, screening_id, reason,
		registered_by_kind, registered_by_id, authority_epoch, registered_at FROM brant_pty.positive_list_entry
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3 ORDER BY registered_at, positive_id`, scoped(scope, partyID)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (screening.Positive, error) {
		var p screening.Positive
		var kind string
		err := row.Scan(&p.ID, &p.PartyID, &p.PartyRevision, &p.ListCode, &p.EntryID, &p.EntryHash, &p.ScreeningID, &p.Reason, &kind,
			&p.By.ID, &p.Epoch, &p.At)
		p.By.Kind, p.At = kernel.ActorKind(kind), kernel.Instant(p.At)
		return p, err
	})
}

// ConfirmedMatches returns the list entries a decision found to be true matches of the party: the
// suspected hits of its conclusions decided true_match.
func ConfirmedMatches(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, partyID string) (map[screening.EntryKey]bool, error) {
	rows, err := exec.Query(ctx, `SELECT DISTINCT h.list_code, h.entry_id FROM brant_pty.screening_hit h
		JOIN brant_pty.screening_decision d ON d.tenant_id = h.tenant_id AND d.company_id = h.company_id AND d.screening_id = h.screening_id
		WHERE h.tenant_id = $1 AND h.company_id = $2 AND d.party_id = $3 AND d.decision = 'true_match' AND h.disposition = 'suspected'`,
		scoped(scope, partyID)...)
	if err != nil {
		return nil, err
	}
	out := map[screening.EntryKey]bool{}
	var k screening.EntryKey
	_, err = pgx.ForEachRow(rows, []any{&k.ListCode, &k.EntryID}, func() error {
		out[k] = true
		return nil
	})
	return out, err
}

// State is the screening status of a party at an instant, as brant_pty.party_screening_state
// derives it.
type State struct {
	Clear         bool
	Status        string
	Reason        string
	ScreeningID   string
	PartyRevision int64
}

// ScreeningState derives the status of a party at instant at without locking.
func ScreeningState(ctx context.Context, exec bentopg.Executor, partyID string, at time.Time) (State, error) {
	var s State
	err := exec.QueryRow(ctx, `SELECT clear, status, reason, screening_id, party_revision FROM brant_pty.party_screening_state($1, $2, false)`,
		partyID, at).Scan(&s.Clear, &s.Status, &s.Reason, &s.ScreeningID, &s.PartyRevision)
	return s, err
}

// Due is a confirmed party, its screening state at an instant and the regulation of its current
// conclusion.
type Due struct {
	State
	PartyID            string
	Revision           int64
	RowVersion         int64
	OwnerActor         string
	OwnerTeam          string
	RegulationID       string
	RegulationRevision int64
}

// ScreeningStates derives, without locking, the screening state at instant at of every confirmed
// party of the scope, ordered by party ID.
func ScreeningStates(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, at time.Time) ([]Due, error) {
	rows, err := exec.Query(ctx, `SELECT p.party_id, p.revision, coalesce(s.row_version, 0), p.owner_actor_id, coalesce(p.owner_team_id, ''),
		coalesce(r.regulation_id, ''), coalesce(r.regulation_revision, 0),
		st.clear, st.status, st.reason, st.screening_id, st.party_revision
		FROM brant_pty.party p
		LEFT JOIN brant_pty.party_screening s ON s.tenant_id = p.tenant_id AND s.company_id = p.company_id AND s.party_id = p.party_id
		LEFT JOIN brant_pty.screening_result r ON r.tenant_id = s.tenant_id AND r.company_id = s.company_id
		 AND r.screening_id = s.current_screening_id
		CROSS JOIN LATERAL brant_pty.party_screening_state(p.party_id, $3, false) st
		WHERE p.tenant_id = $1 AND p.company_id = $2 AND p.status = 'confirmed'
		ORDER BY p.party_id`, scoped(scope, at)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Due, error) {
		var d Due
		err := row.Scan(&d.PartyID, &d.Revision, &d.RowVersion, &d.OwnerActor, &d.OwnerTeam, &d.RegulationID, &d.RegulationRevision,
			&d.Clear, &d.Status, &d.Reason, &d.ScreeningID, &d.PartyRevision)
		return d, err
	})
}
