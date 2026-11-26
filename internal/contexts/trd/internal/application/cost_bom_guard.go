package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/plugin"
)

// bindCostBOMGuard records the cost BOM a freeze will lock. The SQL runs later, after CAT's
// SkuGuard and CST's AdoptabilityGuard, so the lock order stays CAT then CST.
func bindCostBOMGuard(g *guarded, ref *quote.CostBOMRef) {
	if ref != nil {
		g.costBOM = ref
	}
}

// guardCostBOM calls CostBOMGuard v1. An apparel quote that cites a BOM of another profile is
// DOCUMENT_PROFILE_MISMATCH. Draft, withdrawn and unmeasured revisions are BOM_UNVERIFIED.
func guardCostBOM(ctx context.Context, exec bentopg.Executor, ref quote.CostBOMRef, path string) error {
	cited := cstapi.CostBOMRef{BomID: ref.BomID, Revision: ref.Revision, ContentSHA256: ref.ContentSHA256, SizeCode: ref.SizeCode}
	verdict, err := cstapi.ScanCostBOMGuard(cited, exec.QueryRow(ctx, cstapi.CostBOMGuardSQL, cstapi.CostBOMGuardArgs(cited)...).Scan)
	if err != nil {
		return err
	}
	if verdict.Known {
		profile := verdict.DocumentProfile
		if profile == "" {
			profile = string(plugin.ProfileGeneric)
		}
		if profile != string(plugin.ProfileApparel) {
			return plugin.ProfileMismatch(path)
		}
	}
	return verdict.Check(path)
}
