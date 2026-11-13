package codes

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// statusOf is the HTTP status a transport answers each category with (httpapi server.go).
var statusOf = map[kernel.Category]int{
	kernel.CategoryForbidden: http.StatusForbidden,
	kernel.CategoryConflict:  http.StatusConflict,
	kernel.CategoryInvalid:   http.StatusUnprocessableEntity,
}

// The AI layer's codes and their transport classes are those of the archive's error catalog
// (docs/00_START_HERE/tools/data/error_catalog.json, category 任务与 AI).
func TestCodesMatchTheErrorCatalog(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "00_START_HERE", "tools", "data", "error_catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Codes [][]any `json:"codes"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	type row struct {
		category  string
		status    int
		retryable string
	}
	catalog := map[string]row{}
	for _, r := range doc.Codes {
		if len(r) < 4 {
			continue
		}
		code, _ := r[0].(string)
		category, _ := r[1].(string)
		status, _ := r[2].(float64)
		retryable, _ := r[3].(string)
		catalog[code] = row{category, int(status), retryable}
	}
	for code, category := range categories {
		r, ok := catalog[code]
		switch {
		case !ok:
			t.Errorf("%s is not in the error catalog", code)
		case r.category != "任务与 AI":
			t.Errorf("%s has catalog category %q", code, r.category)
		case r.status != statusOf[category]:
			t.Errorf("%s answers %d, the catalog says %d", code, statusOf[category], r.status)
		}
	}
	if r := catalog[ToolUnavailable]; r.status != http.StatusServiceUnavailable || r.retryable != "是" {
		t.Errorf("TOOL_UNAVAILABLE in the catalog = %+v, want a retryable 503", r)
	}
	if _, ok := Category(ToolUnavailable); ok {
		t.Error("TOOL_UNAVAILABLE is a transport answer, not a rejection")
	}
}

func TestRejectUsesTheCategory(t *testing.T) {
	r := Reject(GrantExpired, "/grant")
	if r.Code != GrantExpired || r.Category != kernel.CategoryForbidden || r.Fields[0].Path != "/grant" {
		t.Fatalf("rejection %+v", r)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an unknown code must panic")
		}
	}()
	_ = Reject("NOPE", "/")
}
