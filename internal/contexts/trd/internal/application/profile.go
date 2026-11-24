package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// OpenProfile reports a non-terminal quote revision or sales order of profile.
// plugin.disable asks before its own transaction. Terminal quote revisions are discarded,
// superseded, withdrawn and cancelled. Terminal orders are cancelled, closed_unfulfilled and closed.
// An active revision whose full_v1 slot is taken stays active: quote.cancel is
// ROOT_CONVERSION_LOCKED (TRD-INV-010). The order is the in-flight document.
func (s Service) OpenProfile(ctx context.Context, profile string) (bool, error) {
	if profile == "" {
		profile = "generic"
	}
	var open bool
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		return exec.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM brant_trd.quote_root r
			JOIN brant_trd.quote_revision v ON v.tenant_id = r.tenant_id AND v.company_id = r.company_id AND v.quote_id = r.quote_id
			WHERE r.tenant_id = $1 AND r.company_id = $2 AND r.document_profile = $3
			  AND v.status IN ('working', 'frozen', 'pending_approval', 'active')
			  AND NOT (v.status = 'active' AND EXISTS (
			      SELECT 1 FROM brant_trd.quote_conversion_slot s
			      WHERE s.tenant_id = r.tenant_id AND s.company_id = r.company_id AND s.quote_id = r.quote_id))
		) OR EXISTS (
			SELECT 1 FROM brant_trd.sales_order o
			WHERE o.tenant_id = $1 AND o.company_id = $2 AND o.document_profile = $3
			  AND o.status IN ('draft', 'active')
		)`, string(scope.Tenant), string(scope.Company), profile).Scan(&open)
	})
	return open, err
}
