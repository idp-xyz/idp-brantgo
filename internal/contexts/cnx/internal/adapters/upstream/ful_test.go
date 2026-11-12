package upstream

import (
	"context"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/application"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Until FUL is merged (S5 B1a, B1b), every allocation's lookup is DEPENDENCY_NOT_SUPPORTED (422).
func TestFULNotWired(t *testing.T) {
	_, err := FULNotWired{}.ResolveExports(context.Background(), []application.ExportRef{{Kind: "shipment", ID: "s1"}})
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != "DEPENDENCY_NOT_SUPPORTED" || rej.Category != kernel.CategoryInvalid {
		t.Fatalf("%v", err)
	}
}
