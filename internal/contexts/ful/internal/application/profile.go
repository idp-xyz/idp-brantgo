package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
)

// OpenProfile reports a prepared shipment of profile. Confirmed and cancelled shipments are finished.
func (s Service) OpenProfile(ctx context.Context, profile string) (bool, error) {
	if profile == "" {
		profile = "generic"
	}
	var open bool
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		return exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brant_ful.shipment
			WHERE tenant_id = $1 AND company_id = $2 AND document_profile = $3 AND status = 'prepared')`,
			string(scope.Tenant), string(scope.Company), profile).Scan(&open)
	})
	return open, err
}
