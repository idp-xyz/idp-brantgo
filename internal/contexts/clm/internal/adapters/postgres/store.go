// Package postgres stores CLM's claims in schema brant_clm, on the executor of CLM's own
// transaction.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/clm/internal/domain/claim"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is CLM's schema.
const Schema = "brant_clm"

// ErrConflict means a compare-and-set update found another version.
var ErrConflict = errors.New("clm: version changed")

// Lock is how a read locks the rows it returns.
type Lock string

const (
	NoLock    Lock = ""
	ForUpdate Lock = " FOR UPDATE"
)

func quantity(s string) (kernel.Decimal, error) {
	d, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil {
		return kernel.Decimal{}, fmt.Errorf("clm: stored quantity %q: %w", s, err)
	}
	return d, nil
}

// Text is the exact decimal text of d.
func Text(d kernel.Decimal) string {
	s, _ := d.Exact()
	return s
}

func scope2(s kernel.Scope) (string, string) { return string(s.Tenant), string(s.Company) }

func ids(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// RemedyConflict reports whether err is the unique remedy ID.
func RemedyConflict(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "claim_remedy_remedy_id"
}

const caseColumns = `claim_id, status, row_version, shipment_id, shipment_line_id, shipment_version, order_id, order_revision, order_hash,
	customer_party_id, order_line_id, sku_id, spec_revision, unit_id, unit_revision, shipped_quantity::text, currency_code, currency_revision,
	minor_units, reason, evidence_ids, request_hash, owner_actor_id, coalesce(owner_team_id, ''), opened_by_kind, opened_by_id, open_epoch,
	opened_at, closed_summary, closed_by_kind, closed_by_id, close_epoch, closed_at`

func loadOne(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*claim.Case, error) {
	t, co := scope2(scope)
	var c claim.Case
	var shipped, currency string
	var minor int16
	var openedKind string
	var closedSummary, closedKind, closedID *string
	var closeEpoch *int64
	var closedAt *time.Time
	err := exec.QueryRow(ctx, `SELECT `+caseColumns+` FROM brant_clm.claim_case WHERE tenant_id = $1 AND company_id = $2 AND claim_id = $3`+string(lock),
		t, co, id).Scan(&c.ID, &c.Status, &c.Version, &c.ShipmentID, &c.LineID, &c.ShipmentVersion, &c.OrderID, &c.OrderRevision, &c.OrderHash,
		&c.CustomerID, &c.OrderLineID, &c.SkuID, &c.SpecRevision, &c.UnitID, &c.UnitRevision, &shipped, &currency, &c.CurrencyRevision,
		&minor, &c.Reason, &c.Evidence, &c.RequestHash, &c.OwnerActor, &c.OwnerTeam, &openedKind, &c.Opened.By.ID, &c.Opened.Epoch,
		&c.Opened.At, &closedSummary, &closedKind, &closedID, &closeEpoch, &closedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = fill(&c, shipped, currency, int(minor), openedKind, closedSummary, closedKind, closedID, closeEpoch, closedAt); err != nil {
		return nil, err
	}
	if err = loadChildren(ctx, exec, scope, []*claim.Case{&c}); err != nil {
		return nil, err
	}
	return &c, nil
}

func fill(c *claim.Case, shipped, currency string, minor int, openedKind string, closedSummary, closedKind, closedID *string, closeEpoch *int64, closedAt *time.Time) error {
	q, err := quantity(shipped)
	if err != nil {
		return err
	}
	cur, err := kernel.NewCurrency(currency, minor)
	if err != nil {
		return fmt.Errorf("clm: stored currency %q/%d: %w", currency, minor, err)
	}
	c.Shipped, c.Currency = q, cur
	c.Opened.By.Kind = kernel.ActorKind(openedKind)
	c.Opened.At = kernel.Instant(c.Opened.At)
	if c.Evidence == nil {
		c.Evidence = []string{}
	}
	if closedAt != nil && closedSummary != nil && closedKind != nil && closedID != nil && closeEpoch != nil {
		c.Closure = &claim.Closure{Summary: *closedSummary, By: claim.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*closedKind), ID: *closedID},
			Epoch: *closeEpoch, At: kernel.Instant(*closedAt)}}
	}
	return nil
}

func loadChildren(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, cases []*claim.Case) error {
	if len(cases) == 0 {
		return nil
	}
	byID := map[string]*claim.Case{}
	ids := make([]string, 0, len(cases))
	for _, c := range cases {
		c.Returns = nil
		byID[c.ID] = c
		ids = append(ids, c.ID)
	}
	t, co := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT claim_id, return_id, quantity::text, condition, note, evidence_ids, request_hash, registered_by_kind,
		registered_by_id, register_epoch, registered_at FROM brant_clm.claim_return
		WHERE tenant_id = $1 AND company_id = $2 AND claim_id = ANY ($3) ORDER BY claim_id, return_id`, t, co, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var claimID, returnID, qty, condition, note, hash, kind, actor string
		var evidence []string
		var epoch int64
		var at time.Time
		if err = rows.Scan(&claimID, &returnID, &qty, &condition, &note, &evidence, &hash, &kind, &actor, &epoch, &at); err != nil {
			return err
		}
		q, err := quantity(qty)
		if err != nil {
			return err
		}
		if evidence == nil {
			evidence = []string{}
		}
		byID[claimID].Returns = append(byID[claimID].Returns, claim.ReturnRecord{ID: returnID, Quantity: q, Condition: condition, Note: note,
			Evidence: evidence, RequestHash: hash, By: claim.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(kind), ID: actor}, Epoch: epoch, At: kernel.Instant(at)}})
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rrows, err := exec.Query(ctx, `SELECT claim_id, remedy_id, kind, amount_minor, reason, request_hash, decided_by_kind, decided_by_id, decide_epoch, decided_at
		FROM brant_clm.claim_remedy WHERE tenant_id = $1 AND company_id = $2 AND claim_id = ANY ($3)`, t, co, ids)
	if err != nil {
		return err
	}
	defer rrows.Close()
	for rrows.Next() {
		var claimID, remedyID, kind, amount, reason, hash, actorKind, actorID string
		var epoch int64
		var at time.Time
		if err = rrows.Scan(&claimID, &remedyID, &kind, &amount, &reason, &hash, &actorKind, &actorID, &epoch, &at); err != nil {
			return err
		}
		c := byID[claimID]
		m, err := kernel.ParseMinor(c.Currency, amount)
		if err != nil {
			return fmt.Errorf("clm: stored amount %q: %w", amount, err)
		}
		c.Remedy = &claim.Remedy{ID: remedyID, Kind: kind, Amount: m, Reason: reason, RequestHash: hash,
			By: claim.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(actorKind), ID: actorID}, Epoch: epoch, At: kernel.Instant(at)}}
	}
	return rrows.Err()
}

// LoadCase returns the claim with its returns and remedy, or nil. lock locks the claim row.
func LoadCase(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*claim.Case, error) {
	return loadOne(ctx, exec, scope, id, lock)
}

// LockLine locks the shipment line's claim lock FOR UPDATE, inserting the row when this is the
// line's first claim. Call it after the claim root is locked.
func LockLine(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, shipmentID, lineID string) error {
	t, co := scope2(scope)
	for range 2 {
		var one int
		err := exec.QueryRow(ctx, `SELECT 1 FROM brant_clm.shipment_line_claim_lock WHERE tenant_id = $1 AND company_id = $2
			AND shipment_id = $3 AND shipment_line_id = $4 FOR UPDATE`, t, co, shipmentID, lineID).Scan(&one)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err = exec.Exec(ctx, `INSERT INTO brant_clm.shipment_line_claim_lock (tenant_id, company_id, shipment_id, shipment_line_id)
			VALUES ($1, $2, $3, $4) ON CONFLICT (tenant_id, company_id, shipment_id, shipment_line_id) DO NOTHING`, t, co, shipmentID, lineID); err != nil {
			return err
		}
	}
	return errors.New("clm: shipment line lock missing")
}

// CountedReturns is the sum of return quantities on non-rejected claims of the shipment line, as
// a statement started now sees them. The caller holds the line lock.
func CountedReturns(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, shipmentID, lineID string) (kernel.Decimal, error) {
	t, co := scope2(scope)
	var text string
	err := exec.QueryRow(ctx, `SELECT COALESCE(SUM(r.quantity), 0)::text FROM brant_clm.claim_return r
		JOIN brant_clm.claim_case c ON c.tenant_id = r.tenant_id AND c.company_id = r.company_id AND c.claim_id = r.claim_id
		WHERE c.tenant_id = $1 AND c.company_id = $2 AND c.shipment_id = $3 AND c.shipment_line_id = $4
		  AND NOT EXISTS (SELECT 1 FROM brant_clm.claim_remedy m WHERE m.tenant_id = c.tenant_id AND m.company_id = c.company_id
		    AND m.claim_id = c.claim_id AND m.kind = 'reject')`, t, co, shipmentID, lineID).Scan(&text)
	if err != nil {
		return kernel.Decimal{}, err
	}
	return quantity(text)
}

// InsertCase inserts a new open claim and its shipment-line lock row.
func InsertCase(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *claim.Case) error {
	t, co := scope2(scope)
	if _, err := exec.Exec(ctx, `INSERT INTO brant_clm.shipment_line_claim_lock (tenant_id, company_id, shipment_id, shipment_line_id)
		VALUES ($1, $2, $3, $4) ON CONFLICT (tenant_id, company_id, shipment_id, shipment_line_id) DO NOTHING`,
		t, co, c.ShipmentID, c.LineID); err != nil {
		return err
	}
	_, err := exec.Exec(ctx, `INSERT INTO brant_clm.claim_case (tenant_id, company_id, claim_id, status, row_version, shipment_id, shipment_line_id,
		shipment_version, order_id, order_revision, order_hash, customer_party_id, order_line_id, sku_id, spec_revision, unit_id, unit_revision,
		shipped_quantity, currency_code, currency_revision, minor_units, reason, evidence_ids, request_hash, owner_actor_id, owner_team_id,
		opened_by_kind, opened_by_id, open_epoch, opened_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18::numeric, $19, $20, $21, $22, $23, $24, $25, $26,
		$27, $28, $29, $30)`,
		t, co, c.ID, c.Status, c.Version, c.ShipmentID, c.LineID, c.ShipmentVersion, c.OrderID, c.OrderRevision, c.OrderHash, c.CustomerID,
		c.OrderLineID, c.SkuID, c.SpecRevision, c.UnitID, c.UnitRevision, Text(c.Shipped), c.Currency.Code(), c.CurrencyRevision, c.Currency.MinorUnits(),
		c.Reason, ids(c.Evidence), c.RequestHash, c.OwnerActor, nullable(c.OwnerTeam), string(c.Opened.By.Kind), c.Opened.By.ID, c.Opened.Epoch, c.Opened.At)
	return err
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// InsertReturn inserts one return. The claim row is advanced separately.
func InsertReturn(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, claimID string, r claim.ReturnRecord) error {
	t, co := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_clm.claim_return (tenant_id, company_id, claim_id, return_id, quantity, condition, note, evidence_ids,
		request_hash, registered_by_kind, registered_by_id, register_epoch, registered_at)
		VALUES ($1, $2, $3, $4, $5::numeric, $6, $7, $8, $9, $10, $11, $12, $13)`,
		t, co, claimID, r.ID, Text(r.Quantity), r.Condition, r.Note, ids(r.Evidence), r.RequestHash, string(r.By.By.Kind), r.By.By.ID, r.By.Epoch, r.By.At)
	return err
}

// InsertRemedy inserts the claim's remedy.
func InsertRemedy(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, claimID string, r *claim.Remedy) error {
	t, co := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_clm.claim_remedy (tenant_id, company_id, claim_id, remedy_id, kind, amount_minor, currency_code, reason,
		request_hash, decided_by_kind, decided_by_id, decide_epoch, decided_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		t, co, claimID, r.ID, r.Kind, r.Amount.MinorString(), r.Amount.Currency().Code(), r.Reason, r.RequestHash, string(r.By.By.Kind), r.By.By.ID,
		r.By.Epoch, r.By.At)
	return err
}

// Advance writes the claim's new version, and its closure when it has one, if the stored row is
// still at from.
func Advance(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *claim.Case, from int64) error {
	t, co := scope2(scope)
	var summary, kind, id *string
	var epoch *int64
	var at *time.Time
	if c.Closure != nil {
		summary = &c.Closure.Summary
		k := string(c.Closure.By.By.Kind)
		kind = &k
		id = &c.Closure.By.By.ID
		epoch = &c.Closure.By.Epoch
		when := c.Closure.By.At
		at = &when
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_clm.claim_case SET status = $4, row_version = $5, closed_summary = $6, closed_by_kind = $7,
		closed_by_id = $8, close_epoch = $9, closed_at = $10
		WHERE tenant_id = $1 AND company_id = $2 AND claim_id = $3 AND row_version = $11`,
		t, co, c.ID, c.Status, c.Version, summary, kind, id, epoch, at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// ListOrder returns the claims of an order, with remedies, in claim ID order. Returns are not loaded.
func ListOrder(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string) ([]*claim.Case, error) {
	t, co := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT `+caseColumns+` FROM brant_clm.claim_case WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3
		ORDER BY claim_id`, t, co, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*claim.Case
	for rows.Next() {
		var c claim.Case
		var shipped, currency string
		var minor int16
		var openedKind string
		var closedSummary, closedKind, closedID *string
		var closeEpoch *int64
		var closedAt *time.Time
		if err = rows.Scan(&c.ID, &c.Status, &c.Version, &c.ShipmentID, &c.LineID, &c.ShipmentVersion, &c.OrderID, &c.OrderRevision, &c.OrderHash,
			&c.CustomerID, &c.OrderLineID, &c.SkuID, &c.SpecRevision, &c.UnitID, &c.UnitRevision, &shipped, &currency, &c.CurrencyRevision,
			&minor, &c.Reason, &c.Evidence, &c.RequestHash, &c.OwnerActor, &c.OwnerTeam, &openedKind, &c.Opened.By.ID, &c.Opened.Epoch,
			&c.Opened.At, &closedSummary, &closedKind, &closedID, &closeEpoch, &closedAt); err != nil {
			return nil, err
		}
		if err = fill(&c, shipped, currency, int(minor), openedKind, closedSummary, closedKind, closedID, closeEpoch, closedAt); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, loadRemedies(ctx, exec, scope, out)
}

func loadRemedies(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, cases []*claim.Case) error {
	if len(cases) == 0 {
		return nil
	}
	return loadChildren(ctx, exec, scope, cases)
}
