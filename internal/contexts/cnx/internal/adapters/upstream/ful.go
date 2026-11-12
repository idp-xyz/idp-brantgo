package upstream

import (
	"context"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/application"
)

// FULNotWired is CNX's port to FUL's shipments and export lots (R36) until FUL is in the tree
// (S5 B1a shipments, B1b export lots): every lookup is DEPENDENCY_NOT_SUPPORTED, so
// export_license.allocate refuses in production instead of guessing what FUL holds.
type FULNotWired struct{}

// ResolveExports answers DEPENDENCY_NOT_SUPPORTED.
func (FULNotWired) ResolveExports(context.Context, []application.ExportRef) (map[application.ExportRef]application.Export, error) {
	return nil, api.Reject(api.CodeDependencyNotSupported, "/args/target", "FUL's shipments and export lots are not wired to CNX yet")
}
