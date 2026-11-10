package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mod = "go.idp.xyz/idp-brant"

func TestClassify(t *testing.T) {
	for path, want := range map[string]class{
		"cmd/brant-worker":                        {layer: layerEntry},
		"internal/bootstrap":                      {layer: layerBootstrap},
		"internal/kernel":                         {layer: layerKernel},
		"internal/foundation/messaging":           {layer: layerFoundation},
		"internal/foundation/database/pgtest":     {layer: layerFoundation},
		"internal/transport/http":                 {layer: layerInbound},
		"internal/processes/order":                {layer: layerInbound},
		"internal/reference/api":                  {layer: layerReference, part: partAPI},
		"internal/contexts/trd":                   {layer: layerContext, context: "trd", part: partModule},
		"internal/contexts/trd/api":               {layer: layerContext, context: "trd", part: partAPI},
		"internal/contexts/trd/internal/domain":   {layer: layerContext, context: "trd", part: partDomain},
		"internal/contexts/trd/internal/domain/q": {layer: layerContext, context: "trd", part: partDomain},
		"internal/contexts/trd/internal/adapters": {layer: layerContext, context: "trd", part: partOther},
		"internal/contexts/trd/helpers":           {layer: layerContext, context: "trd", part: partStray},
		"internal/common":                         {layer: layerUnknown},
		"tests/e2e":                               {layer: layerTests},
		"tools/spikes/consistency":                {layer: layerTool},
	} {
		if got := classify(mod, mod+"/"+path); got != want {
			t.Errorf("classify(%s) = %+v, want %+v", path, got, want)
		}
	}
}

func TestImportRules(t *testing.T) {
	allow := map[[2]string]bool{{"trd", "cst"}: true}
	for _, tc := range []struct {
		from, imp, rule string
	}{
		{"internal/contexts/cst/api", "time", ""},
		{"internal/contexts/cst/api", mod + "/internal/kernel", ""},
		{"internal/contexts/cst/api", "go.idp.xyz/idp-bento-go/eventing", "D2"},
		{"internal/contexts/cst/api", mod + "/internal/contexts/pol/api", "D2"},
		{"internal/contexts/cst/internal/domain", mod + "/internal/kernel", ""},
		{"internal/contexts/cst/internal/domain", mod + "/internal/contexts/cst/internal/application", "domain"},
		{"internal/contexts/cst/internal/domain", "github.com/jackc/pgx/v5", "domain"},
		{"internal/contexts/cst/internal/adapters/postgres", "github.com/jackc/pgx/v5", ""},
		{"internal/contexts/cst/internal/adapters/postgres", mod + "/internal/foundation/bento", ""},
		{"internal/contexts/trd/internal/adapters/cst", mod + "/internal/contexts/cst/api", ""},
		{"internal/contexts/cst/internal/adapters/trd", mod + "/internal/contexts/trd/api", "D1"},
		{"internal/contexts/trd/internal/application", mod + "/internal/contexts/cst/internal/domain", "D1"},
		{"internal/contexts/trd", mod + "/internal/contexts/cst", "D1"},
		{"internal/contexts/trd", mod + "/internal/bootstrap", "D4"},
		{"internal/contexts/trd/internal/adapters/http", mod + "/internal/transport/http", "D4"},
		{"internal/foundation/authorization", mod + "/internal/kernel", ""},
		{"internal/foundation/authorization", mod + "/internal/foundation/identity", ""},
		{"internal/foundation/bento", mod + "/internal/contexts/trd/api", "D5"},
		{"internal/foundation/bento", mod + "/internal/bootstrap", "D5"},
		{"internal/kernel", "math/big", ""},
		{"internal/kernel", "github.com/shopspring/decimal", "kernel"},
		{"internal/kernel", mod + "/internal/foundation/bento", "kernel"},
		{"internal/transport/http", mod + "/internal/contexts/trd/api", ""},
		{"internal/transport/http", mod + "/internal/contexts/trd", "D6"},
		{"internal/transport/http", mod + "/internal/contexts/trd/internal/domain", "D6"},
		{"internal/transport/http", mod + "/internal/reference/api", ""},
		{"internal/transport/http", mod + "/internal/reference/internal/lists", "D6"},
		{"cmd/brant-worker", mod + "/internal/bootstrap", ""},
		{"cmd/brant-worker", mod + "/internal/foundation/messaging", "entry"},
		{"cmd/brant-worker", "github.com/jackc/pgx/v5", ""},
		{"internal/bootstrap", mod + "/internal/contexts/trd/internal/domain", ""},
		{"tests/e2e", "net/http", ""},
		{"tests/e2e", mod + "/internal/kernel", "AC6"},
		{"tools/spikes/consistency", mod + "/internal/foundation/bento", ""},
	} {
		f, bad := judge(mod, classify(mod, mod+"/"+tc.from), tc.imp, allow)
		if got := map[bool]string{true: f.rule}[bad]; got != tc.rule {
			t.Errorf("%s -> %s: rule %q (%s), want %q", tc.from, tc.imp, got, f.detail, tc.rule)
		}
	}
}

func TestPackagesOutsideTheLayersAreReported(t *testing.T) {
	found := checkImports(mod, []listedPackage{
		{ImportPath: mod + "/internal/common"},
		{ImportPath: mod + "/internal/contexts/trd/helpers"},
		{ImportPath: mod + "/docs/tools"},
		{ImportPath: mod + "/internal/contexts/trd/api", Imports: []string{"time"}, XTestImports: []string{mod + "/internal/contexts/trd/api"}},
	}, nil)
	got := fmt.Sprint(found)
	for _, want := range []string{"layer internal/common", "layout internal/contexts/trd/helpers", "AC10 docs/tools"} {
		if !strings.Contains(got, want) {
			t.Errorf("findings %s lack %q", got, want)
		}
	}
	if len(found) != 3 {
		t.Fatalf("findings = %v", found)
	}
}

func TestTestSupportIsForTestFilesOnly(t *testing.T) {
	pgtest := mod + "/internal/foundation/database/pgtest"
	found := checkImports(mod, []listedPackage{
		{ImportPath: mod + "/cmd/brant-migrate", TestImports: []string{pgtest}},
		{ImportPath: mod + "/internal/contexts/trd/internal/integration", XTestImports: []string{pgtest}},
		{ImportPath: mod + "/internal/foundation/messaging", Imports: []string{pgtest}},
		{ImportPath: mod + "/tools/spikes/consistency", Imports: []string{pgtest}},
	}, nil)
	if len(found) != 1 || found[0].rule != "testsupport" || found[0].pkg != "internal/foundation/messaging" {
		t.Fatalf("findings = %v", found)
	}
}

func TestDocsHoldNoGoCode(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"docs/00_START_HERE/tools/build.py", "docs/90/sample.go", "docs/12/go.mod", "internal/kernel/money.go"} {
		path := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	found, err := checkDocs(root)
	if err != nil || fmt.Sprint(found) != "[AC10 docs/12/go.mod: the master archive under docs/ holds no Go code AC10 docs/90/sample.go: the master archive under docs/ holds no Go code]" {
		t.Fatalf("findings = %v, %v", found, err)
	}
	if found, err = checkDocs(t.TempDir()); err != nil || len(found) != 0 {
		t.Fatalf("missing docs/: %v, %v", found, err)
	}
}

func TestGeneratedAllowlistCoversPresentContextsOnly(t *testing.T) {
	var doc contextMapDoc
	if err := json.Unmarshal([]byte(`{"version":"0.1-draft","relationships":[
		{"frm":"PTY","to":"INQ"},{"frm":"CAT","to":"INQ"},{"frm":"CST","to":"TRD"},{"frm":"TRD","to":"PUR"},{"frm":"PUR","to":"TRD"}]}`), &doc); err != nil {
		t.Fatal(err)
	}
	lines := generate(doc, []string{"pty", "inq", "trd", "pur"})
	if got := strings.Join(lines, ";"); got != "inq pty;pur trd;trd pur" {
		t.Fatalf("allowlist = %q", got)
	}
	allow, err := parseAllowlist("# header\n" + strings.Join(lines, "\n") + "\n")
	if err != nil || !allow[[2]string{"inq", "pty"}] || allow[[2]string{"pty", "inq"}] {
		t.Fatalf("parsed allowlist = %v, %v", allow, err)
	}
	if _, err = parseAllowlist("inq pty cat\n"); err == nil {
		t.Fatal("a malformed line must be rejected")
	}
}

// The Brant Context Map in the master archive uses the field names the generator reads.
func TestArchiveContextMapIsReadable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "03_STRATEGIC_DDD", "01_IDP_Brant_Strategic_Context_Map_V0.1_Draft.json"))
	if err != nil {
		t.Skip("master archive not present:", err)
	}
	var doc contextMapDoc
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Relationships) == 0 {
		t.Fatal("no relationships decoded from the Context Map")
	}
	all := []string{"pty", "cat", "pol", "evd", "inq", "sup", "cst", "trd", "pur", "mfg", "qlt", "stk", "ful", "stl", "clm", "cnx", "crm", "tsk"}
	lines := generate(doc, all)
	if len(lines) == 0 || !contains(lines, "trd cst") || contains(lines, "cst trd") {
		t.Fatalf("allowlist for all contexts = %v", lines)
	}
}

func contains(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func TestCodeownersMustNameEveryContextAndTheKernel(t *testing.T) {
	required := []string{"internal/contexts/trd/", "internal/kernel/"}
	for name, tc := range map[string]struct {
		text string
		want int
	}{
		"both named":           {"*  @idpxyz\n/internal/contexts/trd/ @idpxyz\ninternal/kernel/** @idpxyz\n", 0},
		"catch-all only":       {"* @idpxyz\n", 2},
		"kernel without owner": {"/internal/contexts/trd/ @idpxyz\n/internal/kernel/\n", 1},
		"missing file":         {"", 2},
	} {
		if got := checkCodeowners(tc.text, required); len(got) != tc.want {
			t.Errorf("%s: %d findings %v, want %d", name, len(got), got, tc.want)
		}
	}
}
