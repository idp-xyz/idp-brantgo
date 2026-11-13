// Package postgres is CNX's PostgreSQL adapter: the trade compliance checks, their evaluations,
// decisions and reopenings in schema brant_cnx (migrations/cnx). Every statement names the scope
// explicitly; FORCE RLS on app.tenant_id and app.company_id confines it as well.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is CNX's schema.
const Schema = "brant_cnx"

// Lock is the row lock a load takes.
type Lock string

const (
	NoLock    Lock = ""
	ForUpdate Lock = " FOR UPDATE" // the command's own aggregate root
)

// ErrConflict means a compare-and-set update found another row version.
var ErrConflict = errors.New("cnx: row version changed")

func scoped(scope kernel.Scope, more ...any) []any {
	return append([]any{string(scope.Tenant), string(scope.Company)}, more...)
}

const checkColumns = `check_id, order_id, order_revision, order_hash, status, row_version, coalesce(current_evaluation_id, ''),
	coalesce(current_decision_id, ''), coalesce(previous_check_id, ''), owner_actor_id, coalesce(owner_team_id, ''), opened_by_kind,
	opened_by_id, open_epoch, opened_at`

func scanCheck(row pgx.Row) (*tcc.Check, error) {
	var c tcc.Check
	var status, kind string
	err := row.Scan(&c.ID, &c.Binding.OrderID, &c.Binding.OrderRevision, &c.Binding.OrderHash, &status, &c.RowVersion, &c.Evaluation,
		&c.Decision, &c.Previous, &c.OwnerActor, &c.OwnerTeam, &kind, &c.OpenedBy.ID, &c.OpenEpoch, &c.OpenedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Status, c.OpenedBy.Kind, c.OpenedAt = tcc.Status(status), kernel.ActorKind(kind), kernel.Instant(c.OpenedAt)
	return &c, nil
}

// LoadCheck returns a check by ID, or nil, taking lock.
func LoadCheck(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, checkID string, lock Lock) (*tcc.Check, error) {
	return scanCheck(exec.QueryRow(ctx, `SELECT `+checkColumns+` FROM brant_cnx.trade_compliance_check
		WHERE tenant_id = $1 AND company_id = $2 AND check_id = $3`+string(lock), scoped(scope, checkID)...))
}

// LoadCheckOfRevision returns the check of an order revision, or nil, taking lock.
func LoadCheckOfRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string, revision int64, lock Lock) (*tcc.Check, error) {
	return scanCheck(exec.QueryRow(ctx, `SELECT `+checkColumns+` FROM brant_cnx.trade_compliance_check
		WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 AND order_revision = $4`+string(lock), scoped(scope, orderID, revision)...))
}

// PreviousCheck returns the ID of the check of the order's latest revision before revision, or "".
func PreviousCheck(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string, revision int64) (string, error) {
	var id string
	err := exec.QueryRow(ctx, `SELECT check_id FROM brant_cnx.trade_compliance_check WHERE tenant_id = $1 AND company_id = $2
		AND order_id = $3 AND order_revision < $4 ORDER BY order_revision DESC LIMIT 1`, scoped(scope, orderID, revision)...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// ChecksOfOrder returns the checks of an order's revisions, newest revision first.
func ChecksOfOrder(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string) ([]*tcc.Check, error) {
	rows, err := exec.Query(ctx, `SELECT `+checkColumns+` FROM brant_cnx.trade_compliance_check
		WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY order_revision DESC`, scoped(scope, orderID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tcc.Check
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// InsertCheck inserts a new check at row version 1.
func InsertCheck(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *tcc.Check) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cnx.trade_compliance_check (tenant_id, company_id, check_id, order_id, order_revision,
		order_hash, status, row_version, current_evaluation_id, current_decision_id, previous_check_id, owner_actor_id, owner_team_id,
		opened_by_kind, opened_by_id, open_epoch, opened_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, ''), nullif($10, ''), nullif($11, ''), $12, nullif($13, ''), $14, $15, $16, $17, $17)`,
		scoped(scope, c.ID, c.Binding.OrderID, c.Binding.OrderRevision, c.Binding.OrderHash, string(c.Status), c.RowVersion, c.Evaluation,
			c.Decision, c.Previous, c.OwnerActor, c.OwnerTeam, string(c.OpenedBy.Kind), c.OpenedBy.ID, c.OpenEpoch, c.OpenedAt)...)
	return err
}

// SaveCheck moves the check from row version from to its state now.
func SaveCheck(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *tcc.Check, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cnx.trade_compliance_check SET status = $4, row_version = $5, current_evaluation_id = nullif($6, ''),
		current_decision_id = nullif($7, ''), updated_at = $8 WHERE tenant_id = $1 AND company_id = $2 AND check_id = $3 AND row_version = $9`,
		scoped(scope, c.ID, string(c.Status), c.RowVersion, c.Evaluation, c.Decision, kernel.Instant(at), from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// NextEvaluationSeq returns the sequence number of the check's next evaluation.
func NextEvaluationSeq(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, checkID string) (int, error) {
	var n int
	err := exec.QueryRow(ctx, `SELECT count(*) + 1 FROM brant_cnx.trade_compliance_evaluation WHERE tenant_id = $1 AND company_id = $2
		AND check_id = $3`, scoped(scope, checkID)...).Scan(&n)
	return n, err
}

// InsertEvaluation appends an evaluation with its document and the programme list versions it
// used, which TradeComplianceGuard v1 compares with the lists' current versions.
func InsertEvaluation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ev *tcc.Evaluation) error {
	d := ev.Document
	at, err := kernel.ParseTimestamp(d.EvaluatedAt)
	if err != nil {
		return err
	}
	until, err := kernel.ParseTimestamp(d.ValidUntil)
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.trade_compliance_evaluation (tenant_id, company_id, evaluation_id, check_id, seq, order_id,
		order_revision, order_hash, regulation_id, regulation_revision, regulation_hash, outcome, document, facts_hash, valid_until,
		evaluated_by_kind, evaluated_by_id, authority_epoch, evaluated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
		scoped(scope, d.EvaluationID, d.CheckID, d.Seq, d.Order.OrderID, d.Order.OrderRevision, d.Order.OrderHash, d.Regulation.ID,
			d.Regulation.Revision, d.Regulation.ContentHash, string(d.Outcome), string(ev.Bytes), ev.FactsHash, until, d.EvaluatedBy.Kind,
			d.EvaluatedBy.ID, d.AuthorityEpoch, at)...)
	for _, p := range d.Programmes {
		if err != nil {
			return err
		}
		_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.trade_compliance_list_used (tenant_id, company_id, evaluation_id, check_id, list_code,
			jurisdiction, version, content_hash) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			scoped(scope, d.EvaluationID, d.CheckID, p.Code, p.Jurisdiction, p.Version, p.ContentHash)...)
	}
	return err
}

// Evaluations returns a check's evaluations in sequence; with evaluationID only that one. A
// document that does not match its stored hash is REFERENCE_INVALID.
func Evaluations(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, checkID, evaluationID string) ([]*tcc.Evaluation, error) {
	rows, err := exec.Query(ctx, `SELECT document, facts_hash FROM brant_cnx.trade_compliance_evaluation WHERE tenant_id = $1 AND company_id = $2
		AND check_id = $3 AND ($4 = '' OR evaluation_id = $4) ORDER BY seq`, scoped(scope, checkID, evaluationID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tcc.Evaluation
	for rows.Next() {
		var doc, hash string
		if err := rows.Scan(&doc, &hash); err != nil {
			return nil, err
		}
		ev, err := tcc.Parse([]byte(doc), hash)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// authorizationDoc is an authorization as a decision stores it (cnx 000008, S5R-D17).
type authorizationDoc struct {
	SourceID      string `json:"source_id"`
	ContentSHA256 string `json:"content_sha256"`
	Kind          string `json:"kind"`
	ValidUntil    string `json:"valid_until"`
}

// InsertDecision appends a decision.
func InsertDecision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, d *tcc.Decision) error {
	evidence, err := json.Marshal(d.Evidence)
	if err != nil {
		return err
	}
	var until *time.Time
	if !d.ValidUntil.IsZero() {
		t := kernel.Instant(d.ValidUntil)
		until = &t
	}
	var authorizations *string
	if len(d.Authorizations) > 0 {
		docs := make([]authorizationDoc, 0, len(d.Authorizations))
		for _, a := range d.Authorizations {
			docs = append(docs, authorizationDoc{SourceID: a.SourceID, ContentSHA256: a.ContentSHA256, Kind: a.Kind,
				ValidUntil: kernel.FormatTimestamp(a.ValidUntil)})
		}
		b, err := json.Marshal(docs)
		if err != nil {
			return err
		}
		text := string(b)
		authorizations = &text
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.trade_compliance_decision (tenant_id, company_id, decision_id, check_id, evaluation_id,
		conclusion, reason, license_required, regulation_id, regulation_revision, regulation_hash, valid_until, evidence, decided_by_kind,
		decided_by_id, authority_epoch, decided_at, authorizations)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		scoped(scope, d.ID, d.CheckID, d.EvaluationID, string(d.Conclusion), d.Reason, d.LicenseRequired, d.Regulation.ID, d.Regulation.Revision,
			d.Regulation.ContentHash, until, string(evidence), string(d.By.Kind), d.By.ID, d.Epoch, kernel.Instant(d.At), authorizations)...)
	return err
}

// Decisions returns a check's decisions oldest first; with decisionID only that one.
func Decisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, checkID, decisionID string) ([]*tcc.Decision, error) {
	rows, err := exec.Query(ctx, `SELECT decision_id, evaluation_id, conclusion, reason, license_required, regulation_id, regulation_revision,
		regulation_hash, valid_until, evidence, decided_by_kind, decided_by_id, authority_epoch, decided_at, authorizations
		FROM brant_cnx.trade_compliance_decision WHERE tenant_id = $1 AND company_id = $2 AND check_id = $3 AND ($4 = '' OR decision_id = $4)
		ORDER BY decided_at, decision_id`, scoped(scope, checkID, decisionID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tcc.Decision
	for rows.Next() {
		d := tcc.Decision{CheckID: checkID}
		var conclusion, evidence, kind string
		var until *time.Time
		var authorizations *string
		if err := rows.Scan(&d.ID, &d.EvaluationID, &conclusion, &d.Reason, &d.LicenseRequired, &d.Regulation.ID, &d.Regulation.Revision,
			&d.Regulation.ContentHash, &until, &evidence, &kind, &d.By.ID, &d.Epoch, &d.At, &authorizations); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(evidence), &d.Evidence); err != nil {
			return nil, err
		}
		if authorizations != nil {
			var docs []authorizationDoc
			if err := json.Unmarshal([]byte(*authorizations), &docs); err != nil {
				return nil, err
			}
			for _, a := range docs {
				t, err := kernel.ParseTimestamp(a.ValidUntil)
				if err != nil {
					return nil, err
				}
				d.Authorizations = append(d.Authorizations, tcc.Authorization{Evidence: tcc.Evidence{SourceID: a.SourceID,
					ContentSHA256: a.ContentSHA256}, Kind: a.Kind, ValidUntil: t})
			}
		}
		d.Conclusion, d.By.Kind, d.At = tcc.Status(conclusion), kernel.ActorKind(kind), kernel.Instant(d.At)
		if until != nil {
			d.ValidUntil = kernel.Instant(*until)
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// InsertReopening appends a reopening.
func InsertReopening(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *tcc.Reopening) error {
	var regID *string
	var regRevision *int64
	if r.Regulation != nil {
		regID, regRevision = &r.Regulation.ID, &r.Regulation.Revision
	}
	updated := r.Updated
	if updated == nil {
		updated = []string{}
	}
	_, err := exec.Exec(ctx, `INSERT INTO brant_cnx.trade_compliance_reopening (tenant_id, company_id, reopening_id, check_id, cause, decision_id,
		regulation_id, regulation_revision, updated_lists, reason, reopened_by_kind, reopened_by_id, authority_epoch, reopened_at)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7, $8, $9, $10, $11, $12, $13, $14)`,
		scoped(scope, r.ID, r.CheckID, r.Cause, r.DecisionID, regID, regRevision, updated, r.Reason, string(r.By.Kind), r.By.ID, r.Epoch,
			kernel.Instant(r.At))...)
	return err
}

// Reopenings returns a check's reopenings oldest first.
func Reopenings(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, checkID string) ([]*tcc.Reopening, error) {
	rows, err := exec.Query(ctx, `SELECT reopening_id, cause, coalesce(decision_id, ''), regulation_id, regulation_revision, updated_lists,
		reason, reopened_by_kind, reopened_by_id, authority_epoch, reopened_at FROM brant_cnx.trade_compliance_reopening
		WHERE tenant_id = $1 AND company_id = $2 AND check_id = $3 ORDER BY reopened_at, reopening_id`, scoped(scope, checkID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tcc.Reopening
	for rows.Next() {
		r := tcc.Reopening{CheckID: checkID}
		var regID *string
		var regRevision *int64
		var kind string
		if err := rows.Scan(&r.ID, &r.Cause, &r.DecisionID, &regID, &regRevision, &r.Updated, &r.Reason, &kind, &r.By.ID, &r.Epoch, &r.At); err != nil {
			return nil, err
		}
		if regID != nil && regRevision != nil {
			r.Regulation = &tcc.RegulationRef{ID: *regID, Revision: *regRevision}
		}
		r.By.Kind, r.At = kernel.ActorKind(kind), kernel.Instant(r.At)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// State is a check's state at an instant as TradeComplianceGuard v1 decides it.
type State struct {
	Clear           bool
	Status          string
	Reason          string
	CheckID         string
	DecisionID      string
	LicenseRequired bool
	ValidUntil      time.Time
}

// StateOf derives the state of order revision b at at without locks
// (brant_cnx.trade_compliance_state, the guard's logic).
func StateOf(ctx context.Context, exec bentopg.Executor, b tcc.Binding, at time.Time) (State, error) {
	var s State
	var regID *string
	var regRevision *int64
	var until *time.Time
	var code *string
	err := exec.QueryRow(ctx, `SELECT clear, status, reason, check_id, decision_id, license_required, regulation_id, regulation_revision,
		valid_until, code FROM brant_cnx.trade_compliance_state($1, $2, $3, $4, false)`, b.OrderID, b.OrderRevision, b.OrderHash,
		kernel.Instant(at)).Scan(&s.Clear, &s.Status, &s.Reason, &s.CheckID, &s.DecisionID, &s.LicenseRequired, &regID, &regRevision, &until, &code)
	if until != nil {
		s.ValidUntil = kernel.Instant(*until)
	}
	return s, err
}
