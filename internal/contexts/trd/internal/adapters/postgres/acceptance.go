package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/acceptance"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/response"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Customer responses, acceptances and their effectivity ledger (migrations/trd/000004), written in
// the transaction of the quote root they belong to: a response once, an acceptance with its first
// ledger entry, a change of effectivity as its ledger entry and then the acceptance's effectivity.
// Nothing is updated otherwise or deleted.

const responseColumns = `response_id, quote_id, revision, business_hash, number, expires_at, classification, summary, source_id, locator,
	evidence, received_at, coalesce(received_at_basis, ''), coalesce(customer_po_number, ''), recorded_by_kind, recorded_by_id, record_epoch,
	recorded_at`

// LoadResponse returns customer response id, or nil.
func LoadResponse(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*response.Response, error) {
	rows, err := exec.Query(ctx, `SELECT `+responseColumns+` FROM brant_trd.customer_response
		WHERE tenant_id = $1 AND company_id = $2 AND response_id = $3`, string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, scanResponse)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out[0], nil
}

func scanResponse(row pgx.CollectableRow) (*response.Response, error) {
	r := &response.Response{}
	var classification, proofs, basis, kind string
	var received *time.Time
	err := row.Scan(&r.ID, &r.QuoteID, &r.Revision, &r.BusinessHash, &r.Number, &r.ExpiresAt, &classification, &r.Summary, &r.Message.SourceID,
		&r.Message.Locator, &proofs, &received, &basis, &r.CustomerPO, &kind, &r.Recorded.By.ID, &r.Recorded.Epoch, &r.Recorded.At)
	if err != nil {
		return nil, err
	}
	r.Classification, r.Recorded.By.Kind = response.Classification(classification), kernel.ActorKind(kind)
	r.ExpiresAt, r.Recorded.At = kernel.Instant(r.ExpiresAt), kernel.Instant(r.Recorded.At)
	if received != nil {
		r.Arrival = &response.Arrival{At: kernel.Instant(*received), Basis: response.Basis(basis)}
	}
	if err = json.Unmarshal([]byte(proofs), &r.Evidence); err != nil {
		return nil, err
	}
	if len(r.Evidence) == 0 {
		r.Evidence = nil
	}
	return r, nil
}

// InsertResponse stores a fresh response.
func InsertResponse(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *response.Response) error {
	proofs, err := proofsOf(r.Evidence)
	if err != nil {
		return err
	}
	var received, basis, po any
	if a := r.Arrival; a != nil {
		received, basis = a.At, string(a.Basis)
	}
	if r.CustomerPO != "" {
		po = r.CustomerPO
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_trd.customer_response (tenant_id, company_id, response_id, quote_id, revision, business_hash,
		number, expires_at, classification, summary, source_id, locator, evidence, received_at, received_at_basis, customer_po_number,
		recorded_by_kind, recorded_by_id, record_epoch, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		string(scope.Tenant), string(scope.Company), r.ID, r.QuoteID, r.Revision, r.BusinessHash, r.Number, r.ExpiresAt, string(r.Classification),
		r.Summary, r.Message.SourceID, r.Message.Locator, proofs, received, basis, po, string(r.Recorded.By.Kind), r.Recorded.By.ID,
		r.Recorded.Epoch, r.Recorded.At)
	return err
}

// acceptanceColumns are an acceptance's columns and the formation its revision seals (the
// commercial document's legal_terms.formation, NULL for a revision without legal terms: the
// default), read in a scalar subquery so that a load FOR UPDATE locks the acceptance only. The
// revision's document never changes, so neither does the formation the acceptance was judged by.
const acceptanceColumns = `acceptance_id, response_id, quote_id, revision, business_hash, number, customer_party_id, customer_revision,
	contact_id, contact_row_version, line_ids, message_source_id, message_locator, arrival_at, arrival_basis, expires_at, verified_by_kind,
	verified_by_id, verify_epoch, verified_at, effectivity, effectivity_version,
	(SELECT (r.commercial::jsonb #> '{legal_terms,formation}')::text FROM brant_trd.quote_revision r
		WHERE r.tenant_id = acceptance.tenant_id AND r.company_id = acceptance.company_id AND r.quote_id = acceptance.quote_id
			AND r.revision = acceptance.revision)`

// AcceptanceFilter selects acceptances: by ID, of a quote (and revision), of a response, or the live
// one of a customer message.
type AcceptanceFilter struct {
	ID              string
	QuoteID         string
	Revision        int64 // with QuoteID; 0: every revision
	ResponseID      string
	MessageSourceID string // with MessageLocator: the live acceptance of that message
	MessageLocator  string
}

// LoadAcceptances returns the acceptances f selects with their effectivity ledgers, in the order
// they were verified, locking them as lock says.
func LoadAcceptances(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, f AcceptanceFilter, lock Lock) ([]*acceptance.Acceptance, error) {
	where, args := `tenant_id = $1 AND company_id = $2`, []any{string(scope.Tenant), string(scope.Company)}
	add := func(clause string, v any) {
		args = append(args, v)
		where += " AND " + clause + " = $" + strconv.Itoa(len(args))
	}
	switch {
	case f.ID != "":
		add("acceptance_id", f.ID)
	case f.QuoteID != "":
		add("quote_id", f.QuoteID)
		if f.Revision > 0 {
			add("revision", f.Revision)
		}
	case f.ResponseID != "":
		add("response_id", f.ResponseID)
	case f.MessageSourceID != "":
		add("message_source_id", f.MessageSourceID)
		add("message_locator", f.MessageLocator)
		where += " AND effectivity IN ('valid', 'disputed')"
	default:
		return nil, errors.New("trd: an acceptance filter selects nothing")
	}
	rows, err := exec.Query(ctx, `SELECT `+acceptanceColumns+` FROM brant_trd.acceptance WHERE `+where+
		` ORDER BY verified_at, acceptance_id`+string(lock), args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, scanAcceptance)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	byID := map[string]*acceptance.Acceptance{}
	ids := make([]string, len(out))
	for i, a := range out {
		byID[a.ID], ids[i] = a, a.ID
	}
	rows, err = exec.Query(ctx, `SELECT acceptance_id, effectivity_version, effectivity, changed_by_kind, changed_by_id, change_epoch, changed_at,
		reason, evidence FROM brant_trd.acceptance_effectivity WHERE tenant_id = $1 AND company_id = $2 AND acceptance_id = ANY ($3)
		ORDER BY acceptance_id, effectivity_version`, string(scope.Tenant), string(scope.Company), ids)
	if err != nil {
		return nil, err
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var id, effectivity, kind string
		var reason, proofs *string
		c := &acceptance.Change{}
		if err := row.Scan(&id, &c.Version, &effectivity, &kind, &c.Act.By.ID, &c.Act.Epoch, &c.Act.At, &reason, &proofs); err != nil {
			return struct{}{}, err
		}
		c.Effectivity, c.Act.By.Kind, c.Act.At = acceptance.Effectivity(effectivity), kernel.ActorKind(kind), kernel.Instant(c.Act.At)
		if reason != nil {
			c.Act.Reason = *reason
		}
		if proofs != nil {
			if err := json.Unmarshal([]byte(*proofs), &c.Act.Evidence); err != nil {
				return struct{}{}, err
			}
		}
		if a := byID[id]; a != nil {
			a.History = append(a.History, c)
		}
		return struct{}{}, nil
	})
	return out, err
}

// LoadAcceptance returns acceptance id with its ledger, or nil, locking it as lock says.
func LoadAcceptance(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*acceptance.Acceptance, error) {
	if id == "" { // no acceptance has an empty ID; an empty filter would select nothing at all
		return nil, nil
	}
	out, err := LoadAcceptances(ctx, exec, scope, AcceptanceFilter{ID: id}, lock)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out[0], nil
}

func scanAcceptance(row pgx.CollectableRow) (*acceptance.Acceptance, error) {
	a := &acceptance.Acceptance{Formation: quote.DefaultFormation}
	var kind, effectivity string
	var formation *string
	err := row.Scan(&a.ID, &a.ResponseID, &a.QuoteID, &a.Revision, &a.BusinessHash, &a.Number, &a.Customer.PartyID, &a.Customer.Revision,
		&a.Contact.ContactID, &a.Contact.RowVersion, &a.LineIDs, &a.Message.SourceID, &a.Message.Locator, &a.ArrivalAt, &a.ArrivalBasis,
		&a.ExpiresAt, &kind, &a.Verified.By.ID, &a.Verified.Epoch, &a.Verified.At, &effectivity, &a.Version, &formation)
	if err != nil {
		return nil, err
	}
	if formation != nil {
		if err = json.Unmarshal([]byte(*formation), &a.Formation); err != nil {
			return nil, err
		}
	}
	a.Verified.By.Kind, a.Effectivity = kernel.ActorKind(kind), acceptance.Effectivity(effectivity)
	a.ArrivalAt, a.ExpiresAt, a.Verified.At = kernel.Instant(a.ArrivalAt), kernel.Instant(a.ExpiresAt), kernel.Instant(a.Verified.At)
	return a, nil
}

// SaveAcceptance writes what a command did to a: a fresh acceptance and its first ledger entry, or
// a fresh ledger entry and then the acceptance's effectivity (the trigger checks the entry exists).
func SaveAcceptance(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, a *acceptance.Acceptance) error {
	t, c := string(scope.Tenant), string(scope.Company)
	if a.Fresh {
		_, err := exec.Exec(ctx, `INSERT INTO brant_trd.acceptance (tenant_id, company_id, acceptance_id, response_id, quote_id, revision,
			business_hash, number, customer_party_id, customer_revision, contact_id, contact_row_version, line_ids, message_source_id,
			message_locator, arrival_at, arrival_basis, expires_at, verified_by_kind, verified_by_id, verify_epoch, verified_at, effectivity,
			effectivity_version) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22,
			$23, $24)`, t, c, a.ID, a.ResponseID, a.QuoteID, a.Revision, a.BusinessHash, a.Number, a.Customer.PartyID, a.Customer.Revision,
			a.Contact.ContactID, a.Contact.RowVersion, a.LineIDs, a.Message.SourceID, a.Message.Locator, a.ArrivalAt, a.ArrivalBasis, a.ExpiresAt,
			string(a.Verified.By.Kind), a.Verified.By.ID, a.Verified.Epoch, a.Verified.At, string(a.Effectivity), a.Version)
		if err != nil {
			return err
		}
	}
	for _, ch := range a.History {
		if !ch.Fresh {
			continue
		}
		var reason, proofs any
		if ch.Version > 1 {
			doc, err := proofsOf(ch.Act.Evidence)
			if err != nil {
				return err
			}
			reason, proofs = ch.Act.Reason, doc
		}
		if _, err := exec.Exec(ctx, `INSERT INTO brant_trd.acceptance_effectivity (tenant_id, company_id, acceptance_id, effectivity_version,
			effectivity, changed_by_kind, changed_by_id, change_epoch, changed_at, reason, evidence)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`, t, c, a.ID, ch.Version, string(ch.Effectivity), string(ch.Act.By.Kind),
			ch.Act.By.ID, ch.Act.Epoch, ch.Act.At, reason, proofs); err != nil {
			return err
		}
	}
	if a.Changed && !a.Fresh {
		tag, err := exec.Exec(ctx, `UPDATE brant_trd.acceptance SET effectivity = $4, effectivity_version = $5
			WHERE tenant_id = $1 AND company_id = $2 AND acceptance_id = $3 AND effectivity_version = $5 - 1`,
			t, c, a.ID, string(a.Effectivity), a.Version)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
	}
	return nil
}
