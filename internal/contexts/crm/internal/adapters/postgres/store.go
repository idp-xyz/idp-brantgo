// Package postgres stores CRM leads, opportunities and follow-ups in schema brant_crm, on the
// executor of CRM's own transaction.
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/crm/internal/domain/lead"
	"go.idp.xyz/idp-brant/internal/contexts/crm/internal/domain/opportunity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is CRM's schema.
const Schema = "brant_crm"

var (
	// ErrConflict means a compare-and-set update found another row version.
	ErrConflict = errors.New("crm: row version changed")
	// ErrDuplicate means an id this scope already holds.
	ErrDuplicate = errors.New("crm: duplicate id")
)

// LoadLead returns the lead, or nil. forUpdate locks the root.
func LoadLead(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, forUpdate bool) (*lead.Lead, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var l lead.Lead
	var team *string
	err := exec.QueryRow(ctx, `SELECT lead_id, source_channel, company_name, contact_name, summary, status, row_version,
		owner_actor_id, owner_team_id FROM brant_crm.lead WHERE tenant_id = $1 AND company_id = $2 AND lead_id = $3`+lock,
		string(scope.Tenant), string(scope.Company), id).Scan(&l.ID, &l.SourceChannel, &l.CompanyName, &l.ContactName,
		&l.Summary, &l.Status, &l.RowVersion, &l.OwnerActor, &team)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if team != nil {
		l.OwnerTeam = *team
	}
	return &l, nil
}

// InsertLead stores a new lead.
func InsertLead(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, l *lead.Lead, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_crm.lead (tenant_id, company_id, lead_id, source_channel, company_name,
		contact_name, summary, status, row_version, owner_actor_id, owner_team_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, nullif($11, ''), $12)`,
		string(scope.Tenant), string(scope.Company), l.ID, l.SourceChannel, l.CompanyName, l.ContactName, l.Summary,
		l.Status, l.RowVersion, l.OwnerActor, l.OwnerTeam, at)
	return duplicate(err)
}

// LoadOpportunity returns the opportunity and its follow-ups, or nil. forUpdate locks the root
// only; follow-ups are read under that lock.
func LoadOpportunity(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, forUpdate bool) (*opportunity.Opportunity, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var o opportunity.Opportunity
	var team, leadID, customerID, currency *string
	var minor *int64
	err := exec.QueryRow(ctx, `SELECT opportunity_id, lead_id, customer_id, title, stage, expected_amount_minor, currency,
		row_version, owner_actor_id, owner_team_id FROM brant_crm.opportunity
		WHERE tenant_id = $1 AND company_id = $2 AND opportunity_id = $3`+lock,
		string(scope.Tenant), string(scope.Company), id).Scan(&o.ID, &leadID, &customerID, &o.Title, &o.Stage, &minor,
		&currency, &o.RowVersion, &o.OwnerActor, &team)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if leadID != nil {
		o.LeadID = *leadID
	}
	if customerID != nil {
		o.CustomerID = *customerID
	}
	if minor != nil {
		o.AmountMinor = *minor
	}
	if currency != nil {
		o.Currency = *currency
	}
	if team != nil {
		o.OwnerTeam = *team
	}
	rows, err := exec.Query(ctx, `SELECT follow_up_id, summary, next_step, recorded_at FROM brant_crm.follow_up
		WHERE tenant_id = $1 AND company_id = $2 AND opportunity_id = $3 ORDER BY recorded_at, follow_up_id`,
		string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	o.FollowUps, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (opportunity.FollowUp, error) {
		var f opportunity.FollowUp
		err := row.Scan(&f.ID, &f.Summary, &f.NextStep, &f.RecordedAt)
		return f, err
	})
	return &o, err
}

// InsertOpportunity stores a new opportunity.
func InsertOpportunity(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, o *opportunity.Opportunity, at time.Time) error {
	var minor any
	var currency any
	if o.Currency != "" {
		minor, currency = o.AmountMinor, o.Currency
	}
	_, err := exec.Exec(ctx, `INSERT INTO brant_crm.opportunity (tenant_id, company_id, opportunity_id, lead_id, customer_id,
		title, stage, expected_amount_minor, currency, row_version, owner_actor_id, owner_team_id, created_at, updated_at)
		VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), $6, $7, $8, $9, $10, $11, nullif($12, ''), $13, $13)`,
		string(scope.Tenant), string(scope.Company), o.ID, o.LeadID, o.CustomerID, o.Title, o.Stage, minor, currency,
		o.RowVersion, o.OwnerActor, o.OwnerTeam, at)
	return duplicate(err)
}

// UpdateOpportunity writes the stage and row version when the stored row is still at from.
func UpdateOpportunity(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, o *opportunity.Opportunity, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_crm.opportunity SET stage = $4, row_version = $5, updated_at = $6
		WHERE tenant_id = $1 AND company_id = $2 AND opportunity_id = $3 AND row_version = $7`,
		string(scope.Tenant), string(scope.Company), o.ID, o.Stage, o.RowVersion, at, from)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// InsertFollowUp appends a follow-up. The caller holds the opportunity FOR UPDATE.
func InsertFollowUp(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, opportunityID string, f opportunity.FollowUp) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_crm.follow_up (tenant_id, company_id, follow_up_id, opportunity_id, summary,
		next_step, recorded_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		string(scope.Tenant), string(scope.Company), f.ID, opportunityID, f.Summary, f.NextStep, f.RecordedAt)
	return duplicate(err)
}

func duplicate(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return ErrDuplicate
	}
	return err
}
