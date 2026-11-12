package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestManifestOrdersDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "migrations")
	manifest := filepath.Join(root, "manifest", "contexts.txt")
	write(t, manifest, "# order\nauth\n\nevd\n")
	dirs, err := readManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "auth") + "," + filepath.Join(root, "evd"); strings.Join(dirs, ",") != want {
		t.Fatalf("dirs = %v, want %s", dirs, want)
	}
	for _, bad := range []string{"auth\nauth\n", "../auth\n", "a.b\n", "# nothing\n", "two words\n"} {
		write(t, manifest, bad)
		if _, err := readManifest(manifest); err == nil {
			t.Errorf("manifest %q must be rejected", bad)
		}
	}
}

func TestMigrationFilesAreContiguousAndCarryTheirDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "evd")
	write(t, filepath.Join(dir, "000001_evd_sources.sql"), "SELECT 1;\r\n")
	items, err := loadDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "brant:000001_evd_sources" || items[0].SQL != "SELECT 1;\n" || items[0].Checksum != checksum("SELECT 1;\n") {
		t.Fatalf("items = %+v", items)
	}
	write(t, filepath.Join(dir, "000003_evd_files.sql"), "SELECT 3;")
	if _, err := loadDirectory(dir); err == nil {
		t.Fatal("a gap in the numbering must be rejected")
	}
	other := filepath.Join(t.TempDir(), "pty")
	write(t, filepath.Join(other, "000001_evd_sources.sql"), "SELECT 1;")
	if _, err := loadDirectory(other); err == nil {
		t.Fatal("a file named for another directory must be rejected")
	}
	empty := filepath.Join(t.TempDir(), "cat")
	write(t, filepath.Join(empty, "000001_cat_items.sql"), " \n")
	if _, err := loadDirectory(empty); err == nil {
		t.Fatal("an empty migration must be rejected")
	}
}

func TestRepositoryManifestLoads(t *testing.T) {
	dirs, err := readManifest(filepath.Join("..", "..", "migrations", "manifest", "contexts.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range dirs {
		items, err := loadDirectory(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range items {
			ids = append(ids, m.ID)
		}
	}
	want := []string{
		"brant:000001_auth_authority_guard", "brant:000002_auth_data_scope", "brant:000003_auth_roles", "brant:000001_msg_dead_letter",
		"brant:000001_ref_screening_lists", "brant:000002_ref_country_programmes", "brant:000001_evd_sources", "brant:000002_evd_business_files",
		"brant:000001_pty_parties", "brant:000002_pty_party_screening", "brant:000003_pty_profiles", "brant:000004_pty_profile_guards",
		"brant:000005_pty_party_guard", "brant:000006_pty_address_country", "brant:000001_cat_skus",
		"brant:000002_cat_item_code_mappings", "brant:000003_cat_sku_guard", "brant:000004_cat_declaration_profiles",
		"brant:000001_pol_configuration", "brant:000002_pol_config_guard", "brant:000003_pol_legal_terms", "brant:000004_pol_order_document_template",
		"brant:000005_pol_s5_kinds",
		"brant:000001_inq_customer_rfqs", "brant:000002_inq_rfq_guard", "brant:000001_sup_supply_conditions", "brant:000002_sup_supply_condition_guard",
		"brant:000001_cst_costing_contexts", "brant:000002_cst_cost_scenarios", "brant:000003_cst_cost_snapshots",
		"brant:000001_trd_quote_roots", "brant:000002_trd_quote_reviews", "brant:000003_trd_quote_formalization",
		"brant:000004_trd_customer_acceptance", "brant:000005_trd_sales_orders", "brant:000006_trd_order_amendments", "brant:000007_trd_order_documents",
		"brant:000001_pur_purchase_drafts", "brant:000002_pur_purchase_commitments",
		"brant:000001_qlt_quality", "brant:000001_stl_customer_credit", "brant:000001_cnx_trade_compliance",
	}
	if got := strings.Join(ids, " "); got != strings.Join(want, " ") {
		t.Fatalf("migration order = %s", got)
	}
	framework, err := frameworkMigrations()
	if err != nil || len(framework) == 0 || !strings.HasPrefix(framework[0].ID, "framework:") || len(framework[0].Checksum) != 64 {
		t.Fatalf("framework migrations = %+v, %v", framework, err)
	}
}
