// openapigen writes the candidate OpenAPI 3.1 document of the HTTP API (contracts/openapi), which
// internal/transport/httpapi generates from its own command tables and request structures.
package main

import (
	"fmt"
	"os"

	"go.idp.xyz/idp-brant/internal/transport/httpapi"
)

func main() {
	doc, err := httpapi.OpenAPI()
	if err == nil {
		err = os.WriteFile(httpapi.OpenAPIPath, doc, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapigen:", err)
		os.Exit(1)
	}
}
