// Package postgres stores MFG in schema brant_mfg, on the executor of MFG's own transaction.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/internal/domain/plan"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/internal/domain/subcontract"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is MFG's schema.
const Schema = "brant_mfg"

// ErrConflict means a compare-and-set update found another version.
var ErrConflict = errors.New("mfg: version changed")

// Lock is how a read locks the rows it returns.
type Lock string

const (
	NoLock    Lock = ""
	ForUpdate Lock = " FOR UPDATE"
)

// Setting is the company switch.
type Setting struct {
	Enabled    bool
	RowVersion int64
	ActorKind  string
	ActorID    string
	Epoch      int64
	UpdatedAt  time.Time
}

// Subcontract is a stored subcontract order plus the snapshot a subject lookup returns.
type Subcontract struct {
	Order         subcontract.Order
	UnitRevision  int64
	Currency      string
	MinorUnits    int
	FeeMinor      string
	FactoryID     string
	SkuID         string
	SpecRevision  int64
	CommitmentID  string
	OrderID       string
	OrderLineID   string
	OrderRevision int64
	OrderHash     string
	SubjectHash   string
	OwnerTeam     string
	OpenedAt      time.Time
}

// Handover is the association of one stock movement to a subcontract.
type Handover struct {
	MovementID string
	Kind       string
	Hash       string
	OwnerActor string
	OwnerTeam  string
	Status     string
}

func scope2(s kernel.Scope) (string, string) { return string(s.Tenant), string(s.Company) }

// Text is the exact decimal text of d.
func Text(d kernel.Decimal) string {
	s, ok := d.Exact()
	if !ok {
		return "0"
	}
	return s
}

func quantity(s string) (kernel.Decimal, error) {
	d, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil {
		return kernel.Decimal{}, fmt.Errorf("mfg: stored quantity %q: %w", s, err)
	}
	return d, nil
}

func nullTeam(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// UniqueViolation reports whether err is a unique constraint failure.
func UniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// LoadSetting reads the company switch. A missing row is (nil, nil).
func LoadSetting(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, lock Lock) (*Setting, error) {
	t, c := scope2(scope)
	var s Setting
	var kind string
	err := exec.QueryRow(ctx, `SELECT enabled, row_version, updated_by_kind, updated_by_id, update_epoch, updated_at
		FROM brant_mfg.company_setting WHERE tenant_id = $1 AND company_id = $2`+string(lock), t, c).
		Scan(&s.Enabled, &s.RowVersion, &kind, &s.ActorID, &s.Epoch, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.ActorKind = kind
	s.UpdatedAt = kernel.Instant(s.UpdatedAt)
	return &s, nil
}

// InsertSetting inserts the company's first switch.
func InsertSetting(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s Setting) error {
	t, c := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_mfg.company_setting (tenant_id, company_id, enabled, row_version,
		updated_by_kind, updated_by_id, update_epoch, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		t, c, s.Enabled, s.RowVersion, s.ActorKind, s.ActorID, s.Epoch, s.UpdatedAt)
	return err
}

// SaveSetting updates the switch when the stored version is from.
func SaveSetting(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s Setting, from int64) error {
	t, c := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_mfg.company_setting SET enabled = $3, row_version = $4, updated_by_kind = $5,
		updated_by_id = $6, update_epoch = $7, updated_at = $8 WHERE tenant_id = $1 AND company_id = $2 AND row_version = $9`,
		t, c, s.Enabled, s.RowVersion, s.ActorKind, s.ActorID, s.Epoch, s.UpdatedAt, from)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// LoadPlan reads one plan and its children. A missing plan is (nil, nil).
func LoadPlan(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*plan.Plan, error) {
	t, c := scope2(scope)
	p := &plan.Plan{}
	var note string
	err := exec.QueryRow(ctx, `SELECT plan_id, status, row_version, factory_party_id, note, request_hash, owner_actor_id,
		coalesce(owner_team_id, '') FROM brant_mfg.collaboration_plan WHERE tenant_id = $1 AND company_id = $2 AND plan_id = $3`+string(lock),
		t, c, id).Scan(&p.ID, &p.Status, &p.Version, &p.FactoryID, &note, &p.RequestHash, &p.OwnerActor, &p.OwnerTeam)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Note = note
	if err = loadChildren(ctx, exec, scope, p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadChildren(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p *plan.Plan) error {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT operation_id, name, sequence, sku_id, spec_revision, quantity::text, unit_id, unit_revision
		FROM brant_mfg.operation_task WHERE tenant_id = $1 AND company_id = $2 AND plan_id = $3 ORDER BY sequence, operation_id`,
		t, c, p.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var op plan.Operation
		var q string
		if err = rows.Scan(&op.ID, &op.Name, &op.Sequence, &op.SkuID, &op.SpecRevision, &q, &op.UnitID, &op.UnitRevision); err != nil {
			rows.Close()
			return err
		}
		if op.Quantity, err = quantity(q); err != nil {
			rows.Close()
			return err
		}
		p.Operations = append(p.Operations, op)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	mrows, err := exec.Query(ctx, `SELECT material_id, kind, sku_id, quantity::text, unit_id, unit_revision, loss_basis
		FROM brant_mfg.material_responsibility WHERE tenant_id = $1 AND company_id = $2 AND plan_id = $3 ORDER BY material_id`,
		t, c, p.ID)
	if err != nil {
		return err
	}
	defer mrows.Close()
	for mrows.Next() {
		var m plan.Material
		var q string
		if err = mrows.Scan(&m.ID, &m.Kind, &m.SkuID, &q, &m.UnitID, &m.UnitRevision, &m.LossBasis); err != nil {
			return err
		}
		if m.Quantity, err = quantity(q); err != nil {
			return err
		}
		p.Materials = append(p.Materials, m)
	}
	return mrows.Err()
}

// InsertPlan inserts a plan and its children.
func InsertPlan(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p *plan.Plan, kind, actor string, epoch int64, at time.Time) error {
	t, c := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_mfg.collaboration_plan (tenant_id, company_id, plan_id, status, row_version,
		factory_party_id, note, request_hash, owner_actor_id, owner_team_id, opened_by_kind, opened_by_id, open_epoch, opened_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		t, c, p.ID, p.Status, p.Version, p.FactoryID, p.Note, p.RequestHash, p.OwnerActor, nullTeam(p.OwnerTeam), kind, actor, epoch, at)
	if err != nil {
		return err
	}
	return insertChildren(ctx, exec, scope, p)
}

// SavePlan updates the plan head when the stored version is from. Children are replaced only for
// a draft save, which the caller does with ReplaceChildren before this update while the plan is
// still draft.
func SavePlan(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p *plan.Plan, from int64) error {
	t, c := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_mfg.collaboration_plan SET status = $4, row_version = $5, note = $6, request_hash = $7
		WHERE tenant_id = $1 AND company_id = $2 AND plan_id = $3 AND row_version = $8`,
		t, c, p.ID, p.Status, p.Version, p.Note, p.RequestHash, from)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// ReplaceChildren replaces a draft's operations and materials. The plan row is still draft.
func ReplaceChildren(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p *plan.Plan) error {
	t, c := scope2(scope)
	if _, err := exec.Exec(ctx, `DELETE FROM brant_mfg.operation_task WHERE tenant_id = $1 AND company_id = $2 AND plan_id = $3`, t, c, p.ID); err != nil {
		return err
	}
	if _, err := exec.Exec(ctx, `DELETE FROM brant_mfg.material_responsibility WHERE tenant_id = $1 AND company_id = $2 AND plan_id = $3`, t, c, p.ID); err != nil {
		return err
	}
	return insertChildren(ctx, exec, scope, p)
}

func insertChildren(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p *plan.Plan) error {
	t, c := scope2(scope)
	for _, op := range p.Operations {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_mfg.operation_task (tenant_id, company_id, plan_id, operation_id, name,
			sequence, sku_id, spec_revision, quantity, unit_id, unit_revision) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10, $11)`,
			t, c, p.ID, op.ID, op.Name, op.Sequence, op.SkuID, op.SpecRevision, Text(op.Quantity), op.UnitID, op.UnitRevision); err != nil {
			return err
		}
	}
	for _, m := range p.Materials {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_mfg.material_responsibility (tenant_id, company_id, plan_id, material_id, kind,
			sku_id, quantity, unit_id, unit_revision, loss_basis) VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9, $10)`,
			t, c, p.ID, m.ID, m.Kind, m.SkuID, Text(m.Quantity), m.UnitID, m.UnitRevision, m.LossBasis); err != nil {
			return err
		}
	}
	return nil
}

// LoadSubcontract reads one subcontract and its progress claims. A missing row is (nil, nil).
func LoadSubcontract(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*Subcontract, error) {
	t, c := scope2(scope)
	var s Subcontract
	var q string
	var minor int16
	var commitment *string
	err := exec.QueryRow(ctx, `SELECT subcontract_id, status, row_version, plan_id, operation_id, quantity::text, unit_id, unit_revision,
		currency_code, minor_units, processing_fee_minor, factory_party_id, sku_id, spec_revision, commitment_id, order_id, order_line_id,
		order_revision, order_hash, subject_hash, issue_hash, request_hash, owner_actor_id, coalesce(owner_team_id, ''), opened_at
		FROM brant_mfg.subcontract_order WHERE tenant_id = $1 AND company_id = $2 AND subcontract_id = $3`+string(lock),
		t, c, id).Scan(&s.Order.ID, &s.Order.Status, &s.Order.Version, &s.Order.PlanID, &s.Order.OperationID, &q, &s.Order.UnitID,
		&s.UnitRevision, &s.Currency, &minor, &s.FeeMinor, &s.FactoryID, &s.SkuID, &s.SpecRevision, &commitment, &s.OrderID, &s.OrderLineID,
		&s.OrderRevision, &s.OrderHash, &s.SubjectHash, &s.Order.IssueHash, &s.Order.RequestHash, &s.Order.OwnerActor, &s.OwnerTeam, &s.OpenedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if s.Order.Quantity, err = quantity(q); err != nil {
		return nil, err
	}
	s.MinorUnits = int(minor)
	if commitment != nil {
		s.CommitmentID = *commitment
	}
	s.OpenedAt = kernel.Instant(s.OpenedAt)
	if err = loadProgress(ctx, exec, scope, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func loadProgress(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *Subcontract) error {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT progress_id, operation_id, progress_kind, quantity::text, unit_id, request_hash, actor_id
		FROM brant_mfg.operation_progress WHERE tenant_id = $1 AND company_id = $2 AND subcontract_id = $3 ORDER BY reported_at, progress_id`,
		t, c, s.Order.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p subcontract.Progress
		var q string
		if err = rows.Scan(&p.ID, &p.Operation, &p.Kind, &q, &p.UnitID, &p.Hash, &p.ActorID); err != nil {
			return err
		}
		if p.Quantity, err = quantity(q); err != nil {
			return err
		}
		s.Order.Claims = append(s.Order.Claims, p)
	}
	return rows.Err()
}

// InsertSubcontract inserts an issued subcontract.
func InsertSubcontract(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *Subcontract, actor string, epoch int64, at time.Time) error {
	t, c := scope2(scope)
	var commitment *string
	if s.CommitmentID != "" {
		commitment = &s.CommitmentID
	}
	_, err := exec.Exec(ctx, `INSERT INTO brant_mfg.subcontract_order (tenant_id, company_id, subcontract_id, status, row_version,
		plan_id, operation_id, quantity, unit_id, unit_revision, currency_code, minor_units, processing_fee_minor, factory_party_id,
		sku_id, spec_revision, commitment_id, order_id, order_line_id, order_revision, order_hash, subject_hash, issue_hash, request_hash,
		owner_actor_id, owner_team_id, opened_by_kind, opened_by_id, open_epoch, opened_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24,
		$25, $26, 'human', $27, $28, $29)`,
		t, c, s.Order.ID, s.Order.Status, s.Order.Version, s.Order.PlanID, s.Order.OperationID, Text(s.Order.Quantity), s.Order.UnitID,
		s.UnitRevision, s.Currency, s.MinorUnits, s.FeeMinor, s.FactoryID, s.SkuID, s.SpecRevision, commitment, s.OrderID, s.OrderLineID,
		s.OrderRevision, s.OrderHash, s.SubjectHash, s.Order.IssueHash, s.Order.RequestHash, s.Order.OwnerActor, nullTeam(s.OwnerTeam),
		actor, epoch, at)
	return err
}

// SaveSubcontract updates status, version and the last request hash.
func SaveSubcontract(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *Subcontract, from int64) error {
	t, c := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_mfg.subcontract_order SET status = $4, row_version = $5, request_hash = $6
		WHERE tenant_id = $1 AND company_id = $2 AND subcontract_id = $3 AND row_version = $7`,
		t, c, s.Order.ID, s.Order.Status, s.Order.Version, s.Order.RequestHash, from)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// InsertProgress appends one source claim.
func InsertProgress(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, subcontractID string, p subcontract.Progress, kind string, epoch int64, at time.Time) error {
	t, c := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_mfg.operation_progress (tenant_id, company_id, subcontract_id, progress_id, operation_id,
		progress_kind, quantity, unit_id, reported_at, request_hash, actor_kind, actor_id, authority_epoch)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9, $10, $11, $12, $13)`,
		t, c, subcontractID, p.ID, p.Operation, p.Kind, Text(p.Quantity), p.UnitID, at, p.Hash, kind, p.ActorID, epoch)
	return err
}

// LoadHandover reads one association. A missing row is (nil, nil).
func LoadHandover(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, movementID string) (*Handover, error) {
	t, c := scope2(scope)
	var h Handover
	err := exec.QueryRow(ctx, `SELECT h.movement_id, h.kind, h.request_hash, h.owner_actor_id, coalesce(h.owner_team_id, ''), o.status
		FROM brant_mfg.material_handover h JOIN brant_mfg.subcontract_order o
		ON o.tenant_id = h.tenant_id AND o.company_id = h.company_id AND o.subcontract_id = h.subcontract_id
		WHERE h.tenant_id = $1 AND h.company_id = $2 AND h.movement_id = $3`, t, c, movementID).
		Scan(&h.MovementID, &h.Kind, &h.Hash, &h.OwnerActor, &h.OwnerTeam, &h.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &h, err
}

// InsertHandover records the association. The stock movement already exists in STK.
func InsertHandover(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, subcontractID string, h Handover, cmdKind, counter, sku, location, batch, custody, toLocation, toBatch, toCustody, quantity, unit string, unitRevision int64, reason string, at time.Time) error {
	t, c := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_mfg.material_handover (tenant_id, company_id, movement_id, subcontract_id, kind,
		counter_movement_id, sku_id, location_id, batch_id, custody_kind, to_location_id, to_batch_id, to_custody_kind, quantity,
		unit_id, unit_revision, reason, request_hash, owner_actor_id, owner_team_id, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14::numeric, $15, $16, $17, $18, $19, $20, $21)`,
		t, c, h.MovementID, subcontractID, cmdKind, nullTeam(counter), sku, location, batch, custody, nullTeam(toLocation),
		nullTeam(toBatch), nullTeam(toCustody), quantity, unit, unitRevision, reason, h.Hash, h.OwnerActor, nullTeam(h.OwnerTeam), at)
	return err
}
