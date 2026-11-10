package httpapi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The committed candidate OpenAPI document must match the transport: regenerate it with
// `go run ./tools/openapigen` after changing routes or request structures.
func TestOpenAPIDocumentMatchesTheTransport(t *testing.T) {
	want, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("..", "..", "..", OpenAPIPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; run go run ./tools/openapigen", OpenAPIPath)
	}
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err = json.Unmarshal(got, &doc); err != nil || doc.OpenAPI != "3.1.0" {
		t.Fatalf("document: %v", err)
	}
	for _, p := range []string{"/api/brant/v1/inq/commands/rfq.confirm_scope", "/api/brant/v1/pol/commands/config.publish", "/api/brant/v1/evd/files/{id}"} {
		if doc.Paths[p] == nil {
			t.Errorf("missing %s", p)
		}
	}
}
