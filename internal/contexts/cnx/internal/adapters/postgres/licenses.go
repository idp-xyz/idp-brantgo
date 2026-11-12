package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	el "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/exportlicense"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Export licences, their allocations and releases (migrations/cnx/000002_cnx_export_licences.sql).
// Quantities and minor-unit values cross as numeric text, never as floats.

const licenseColumns = `license_id, license_number, issuer, holder, goods, unit, trim_scale(licensed_quantity)::text, currency_id,
	currency_revision, currency_code, currency_minor_units, licensed_value_minor::text, destination, end_user_party_id, end_user_name,
	valid_from, valid_until, file_id, file_sha256, row_version, registered_by_kind, registered_by_id, authority_epoch, registered_at`

func decimal(s string) (kernel.Decimal, error) {
	d, err := kernel.ParseDecimal(s, kernel.DecimalFormat{IntDigits: kernel.MaxMinorDigits, Scale: 6})
	return d, err
}

func minor(c kernel.Currency, s string) (kernel.Money, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return kernel.Money{}, errors.New("cnx: unreadable minor units " + strconv.Quote(s))
	}
	return kernel.MoneyFromMinor(c, v)
}

func scanLicense(row pgx.Row) (*el.License, error) {
	var l el.License
	var goods, unit, quantity, value, kind string
	err := row.Scan(&l.ID, &l.Number, &l.Issuer, &l.Holder, &goods, &unit, &quantity, &l.Currency.ConfigID, &l.Currency.Revision,
		&l.Currency.Code, &l.Currency.MinorUnits, &value, &l.Destination, &l.EndUserPartyID, &l.EndUserName, &l.Validity.From,
		&l.Validity.Until, &l.FileID, &l.FileSHA256, &l.RowVersion, &kind, &l.RegisteredBy.ID, &l.Epoch, &l.RegisteredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(goods), &l.Goods); err != nil {
		return nil, err
	}
	l.Unit, l.RegisteredBy.Kind = kernel.UnitCode(unit), kernel.ActorKind(kind)
	l.Validity.From, l.Validity.Until, l.RegisteredAt = kernel.Instant(l.Validity.From), kernel.Instant(l.Validity.Until), kernel.Instant(l.RegisteredAt)
	if l.Quantity, err = decimal(quantity); err != nil {
		return nil, err
	}
	c, err := l.Currency.Kernel()
	if err != nil {
		return nil, err
	}
	if l.Value, err = minor(c, value); err != nil {
		return nil, err
	}
	return &l, nil
}

// LoadLicense returns a licence by ID, or nil, taking lock.
func LoadLicense(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, licenseID string, lock Lock) (*el.License, error) {
	return scanLicense(exec.QueryRow(ctx, `SELECT `+licenseColumns+` FROM brant_cnx.export_license
		WHERE tenant_id = $1 AND company_id = $2 AND license_id = $3`+string(lock), scoped(scope, licenseID)...))
}

// LicenseByNumber returns the ID of the issuer's licence number, or "".
func LicenseByNumber(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, issuer, number string) (string, error) {
	var id string
	err := exec.QueryRow(ctx, `SELECT license_id FROM brant_cnx.export_license WHERE tenant_id = $1 AND company_id = $2 AND issuer = $3
		AND license_number = $4`, scoped(scope, issuer, number)...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Licenses returns the scope's licences, newest first.
func Licenses(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) ([]*el.License, error) {
	rows, err := exec.Query(ctx, `SELECT `+licenseColumns+` FROM brant_cnx.export_license WHERE tenant_id = $1 AND company_id = $2
		ORDER BY registered_at DESC, license_id`, scoped(scope)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*el.License
	for rows.Next() {
		l, err := scanLicense(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// InsertLicense inserts a new licence at row version 1.
func InsertLicense(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, l *el.License) error {
	goods, err := json.Marshal(l.Goods)
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.export_license (tenant_id, company_id, license_id, license_number, issuer, holder, goods,
		unit, licensed_quantity, currency_id, currency_revision, currency_code, currency_minor_units, licensed_value_minor, destination,
		end_user_party_id, end_user_name, valid_from, valid_until, file_id, file_sha256, status, row_version, registered_by_kind,
		registered_by_id, authority_epoch, registered_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10, $11, $12, $13, $14::numeric, $15, $16, $17, $18, $19, $20, $21, 'registered',
			1, $22, $23, $24, $25, $25)`,
		scoped(scope, l.ID, l.Number, l.Issuer, l.Holder, string(goods), string(l.Unit), l.Quantity.String(), l.Currency.ConfigID,
			l.Currency.Revision, l.Currency.Code, l.Currency.MinorUnits, l.Value.MinorString(), l.Destination, l.EndUserPartyID, l.EndUserName,
			kernel.Instant(l.Validity.From), kernel.Instant(l.Validity.Until), l.FileID, l.FileSHA256, string(l.RegisteredBy.Kind),
			l.RegisteredBy.ID, l.Epoch, kernel.Instant(l.RegisteredAt))...)
	return err
}

// TouchLicense moves the licence from row version from to its row version now.
func TouchLicense(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, l *el.License, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cnx.export_license SET row_version = $4, updated_at = $5
		WHERE tenant_id = $1 AND company_id = $2 AND license_id = $3 AND row_version = $6`,
		scoped(scope, l.ID, l.RowVersion, kernel.Instant(at), from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

type lineDoc struct {
	LineID        string `json:"line_id"`
	SkuID         string `json:"sku_id"`
	SpecRevision  int64  `json:"spec_revision"`
	Quantity      string `json:"quantity"`
	Unit          string `json:"unit"`
	CommodityCode string `json:"commodity_code,omitempty"`
}

const allocationColumns = `a.allocation_id, a.license_id, a.target_kind, a.target_id, a.target_version, a.attempt, a.order_id, a.order_revision,
	a.destination, a.end_user_party_id, a.lines, a.lines_hash, trim_scale(a.quantity)::text, a.value_minor::text, a.allocated_by_kind,
	a.allocated_by_id, a.authority_epoch, a.allocated_at,
	coalesce((SELECT trim_scale(sum(r.quantity))::text FROM brant_cnx.export_license_release r WHERE r.tenant_id = a.tenant_id
		AND r.company_id = a.company_id AND r.allocation_id = a.allocation_id), '0'),
	coalesce((SELECT sum(r.value_minor)::text FROM brant_cnx.export_license_release r WHERE r.tenant_id = a.tenant_id
		AND r.company_id = a.company_id AND r.allocation_id = a.allocation_id), '0')`

// Allocations returns the allocations of licence l, oldest first, with what their releases
// released; with allocationID only that one.
func Allocations(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, l *el.License, allocationID string) ([]*el.Allocation, error) {
	c, err := l.Currency.Kernel()
	if err != nil {
		return nil, err
	}
	rows, err := exec.Query(ctx, `SELECT `+allocationColumns+` FROM brant_cnx.export_license_allocation a
		WHERE a.tenant_id = $1 AND a.company_id = $2 AND a.license_id = $3 AND ($4 = '' OR a.allocation_id = $4)
		ORDER BY a.allocated_at, a.allocation_id`, scoped(scope, l.ID, allocationID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*el.Allocation
	for rows.Next() {
		var a el.Allocation
		var lines, quantity, value, kind, released, releasedValue string
		if err := rows.Scan(&a.ID, &a.LicenseID, &a.Target.Kind, &a.Target.ID, &a.Target.Version, &a.Attempt, &a.Target.OrderID,
			&a.Target.OrderRevision, &a.Target.Destination, &a.Target.EndUserPartyID, &lines, &a.LinesHash, &quantity, &value, &kind, &a.By.ID,
			&a.Epoch, &a.At, &released, &releasedValue); err != nil {
			return nil, err
		}
		var docs []lineDoc
		if err := json.Unmarshal([]byte(lines), &docs); err != nil {
			return nil, err
		}
		for _, d := range docs {
			q, err := decimal(d.Quantity)
			if err != nil {
				return nil, err
			}
			a.Target.Lines = append(a.Target.Lines, el.Line{LineID: d.LineID, SkuID: d.SkuID, SpecRevision: d.SpecRevision, Quantity: q,
				Unit: kernel.UnitCode(d.Unit), CommodityCode: d.CommodityCode})
		}
		if a.Quantity, err = decimal(quantity); err != nil {
			return nil, err
		}
		if a.Released, err = decimal(released); err != nil {
			return nil, err
		}
		if a.Value, err = minor(c, value); err != nil {
			return nil, err
		}
		if a.ReleasedValue, err = minor(c, releasedValue); err != nil {
			return nil, err
		}
		a.Target.Value, a.By.Kind, a.At = a.Value, kernel.ActorKind(kind), kernel.Instant(a.At)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// AllocationOwner returns the licence an allocation ID belongs to, or "".
func AllocationOwner(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, allocationID string) (string, error) {
	var id string
	err := exec.QueryRow(ctx, `SELECT license_id FROM brant_cnx.export_license_allocation WHERE tenant_id = $1 AND company_id = $2
		AND allocation_id = $3`, scoped(scope, allocationID)...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// TargetAttempt returns the next attempt of a target's allocations and whether one of them still
// has quantity outstanding.
func TargetAttempt(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, kind, id string) (int, bool, error) {
	var n int
	var held bool
	err := exec.QueryRow(ctx, `SELECT count(*) + 1, coalesce(bool_or(a.quantity > coalesce((SELECT sum(r.quantity)
		FROM brant_cnx.export_license_release r WHERE r.tenant_id = a.tenant_id AND r.company_id = a.company_id
		AND r.allocation_id = a.allocation_id), 0)), false)
		FROM brant_cnx.export_license_allocation a WHERE a.tenant_id = $1 AND a.company_id = $2 AND a.target_kind = $3 AND a.target_id = $4`,
		scoped(scope, kind, id)...).Scan(&n, &held)
	return n, held, err
}

// InsertAllocation appends an allocation.
func InsertAllocation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, a *el.Allocation) error {
	docs := []lineDoc{}
	for _, l := range a.Target.Lines {
		docs = append(docs, lineDoc{LineID: l.LineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision, Quantity: l.Quantity.String(),
			Unit: string(l.Unit), CommodityCode: l.CommodityCode})
	}
	lines, err := json.Marshal(docs)
	if err != nil {
		return err
	}
	t := a.Target
	_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.export_license_allocation (tenant_id, company_id, allocation_id, license_id, target_kind,
		target_id, target_version, attempt, order_id, order_revision, destination, end_user_party_id, lines, lines_hash, quantity, value_minor,
		currency_code, allocated_by_kind, allocated_by_id, authority_epoch, allocated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::numeric, $16::numeric, $17, $18, $19, $20, $21)`,
		scoped(scope, a.ID, a.LicenseID, t.Kind, t.ID, t.Version, a.Attempt, t.OrderID, t.OrderRevision, t.Destination, t.EndUserPartyID,
			string(lines), a.LinesHash, a.Quantity.String(), a.Value.MinorString(), a.Value.Currency().Code(), string(a.By.Kind), a.By.ID, a.Epoch,
			kernel.Instant(a.At))...)
	return err
}

// Releases returns the releases of licence l's allocations, oldest first.
func Releases(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, l *el.License) ([]*el.Release, error) {
	c, err := l.Currency.Kernel()
	if err != nil {
		return nil, err
	}
	rows, err := exec.Query(ctx, `SELECT release_id, allocation_id, kind, result_source, declaration_number, receipt_text, received_at, evidence,
		trim_scale(quantity)::text, value_minor::text, reason, released_by_kind, released_by_id, authority_epoch, released_at
		FROM brant_cnx.export_license_release WHERE tenant_id = $1 AND company_id = $2 AND license_id = $3 ORDER BY released_at, release_id`,
		scoped(scope, l.ID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*el.Release
	for rows.Next() {
		r := el.Release{LicenseID: l.ID}
		var evidence, quantity, value, kind string
		if err := rows.Scan(&r.ID, &r.AllocationID, &r.Kind, &r.Source, &r.DeclarationNumber, &r.ReceiptText, &r.ReceivedAt, &evidence,
			&quantity, &value, &r.Reason, &kind, &r.By.ID, &r.Epoch, &r.At); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(evidence), &r.Evidence); err != nil {
			return nil, err
		}
		if r.Quantity, err = decimal(quantity); err != nil {
			return nil, err
		}
		if r.Value, err = minor(c, value); err != nil {
			return nil, err
		}
		r.By.Kind, r.ReceivedAt, r.At = kernel.ActorKind(kind), kernel.Instant(r.ReceivedAt), kernel.Instant(r.At)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// ReleaseExists reports whether a release ID is taken in the scope.
func ReleaseExists(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, releaseID string) (bool, error) {
	var taken bool
	err := exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brant_cnx.export_license_release WHERE tenant_id = $1 AND company_id = $2
		AND release_id = $3)`, scoped(scope, releaseID)...).Scan(&taken)
	return taken, err
}

// InsertRelease appends a release.
func InsertRelease(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *el.Release) error {
	evidence, err := json.Marshal(r.Evidence)
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.export_license_release (tenant_id, company_id, release_id, license_id, allocation_id, kind,
		result_source, declaration_number, receipt_text, received_at, evidence, quantity, value_minor, reason, released_by_kind, released_by_id,
		authority_epoch, released_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::numeric, $13::numeric, $14, $15, $16, $17, $18)`,
		scoped(scope, r.ID, r.LicenseID, r.AllocationID, r.Kind, r.Source, r.DeclarationNumber, r.ReceiptText, kernel.Instant(r.ReceivedAt),
			string(evidence), r.Quantity.String(), r.Value.MinorString(), r.Reason, string(r.By.Kind), r.By.ID, r.Epoch, kernel.Instant(r.At))...)
	return err
}

// ShipmentStateSQL derives ExportLicenseGuard v1's verdict for a shipment without locks, in the
// guard's columns (brant_cnx.export_license_state).
const ShipmentStateSQL = `SELECT covered, status, reason, license_id, allocation_id, quantity, unit, value_minor, currency, lines_hash,
	valid_until, code FROM brant_cnx.export_license_state($1, $2, false)`
