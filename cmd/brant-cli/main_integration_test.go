//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
)

// Python test_cli.py test_foundation_demo_has_explicit_synthetic_truth_values (PYT-0003): the demo
// writes explicit synthetic truth values into foundation-demo-result.json. Python publishes 22
// configurations of its 18 kinds; Brant publishes 20 of POL's 18 kinds, because sku_profile,
// party_profile, contact_profile and code_mapping are not POL kinds and POL has tax_application,
// quote_document_template, credit_policy and screening_regulation (bootstrap.FoundationDemo). A
// second run into the same directory is refused and leaves the result as it was (Python requires a
// fresh directory).
func TestFoundationDemoHasExplicitSyntheticTruthValues(t *testing.T) {
	dir := t.TempDir()
	var stdout bytes.Buffer
	if err := run([]string{"foundation-demo", "--directory", dir}, pgtest.AppDSN(t), &stdout); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "foundation-demo-result.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Synthetic          bool              `json:"synthetic"`
		Notice             string            `json:"notice"`
		Tenant             string            `json:"tenant"`
		Company            string            `json:"company"`
		ConfigurationKinds int               `json:"configuration_kinds"`
		Configurations     int               `json:"configurations"`
		Kinds              []string          `json:"kinds"`
		FX                 map[string]string `json:"fx"`
		Unit               map[string]string `json:"unit"`
		Tax                map[string]string `json:"tax"`
		ModelCalls         *int              `json:"model_calls"`
		ExternalActions    *int              `json:"external_business_actions"`
	}
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ConfigurationKinds != 18 || saved.Configurations != 20 {
		t.Fatalf("kinds, configurations = %d, %d; want 18, 20 (Python 18, 22)", saved.ConfigurationKinds, saved.Configurations)
	}
	if want := map[string]string{"amount": "854.81", "currency": "CNY"}; !reflect.DeepEqual(saved.FX, want) {
		t.Fatalf("fx %v, want %v", saved.FX, want)
	}
	if saved.Tax["gross_amount"] != "113.00" || saved.Tax["net_amount"] != "100.00" || saved.Tax["tax_amount"] != "13.00" || saved.Tax["currency"] != "CNY" {
		t.Fatalf("tax %v", saved.Tax)
	}
	if saved.Unit["quantity"] != "24" || saved.Unit["unit"] != "PCS" {
		t.Fatalf("unit %v", saved.Unit)
	}
	if saved.ModelCalls == nil || *saved.ModelCalls != 0 || saved.ExternalActions == nil || *saved.ExternalActions != 0 {
		t.Fatalf("model calls %v, external business actions %v", saved.ModelCalls, saved.ExternalActions)
	}
	if !saved.Synthetic || !strings.HasPrefix(saved.Notice, "SYNTHETIC ONLY") || saved.Tenant != "tenant.synthetic.demo" ||
		!strings.HasPrefix(saved.Company, "company.demo.") {
		t.Fatalf("synthetic labels: %v %q %s/%s", saved.Synthetic, saved.Notice, saved.Tenant, saved.Company)
	}
	pythonOnly := []string{"sku_profile", "party_profile", "contact_profile", "code_mapping"}
	for _, k := range append(pythonOnly, "company", "conversion", "fx", "fee_type", "sequence") {
		if strings.Contains(" "+strings.Join(saved.Kinds, " ")+" ", " "+k+" ") {
			t.Fatalf("kind %s is Python's, not POL's: %v", k, saved.Kinds)
		}
	}

	var summary map[string]any
	if err = json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("stdout %q: %v", stdout.String(), err)
	}
	if summary["synthetic"] != true || summary["result_file"] != file || summary["configurations"] != float64(20) ||
		summary["configuration_kinds"] != float64(18) || summary["company"] != saved.Company {
		t.Fatalf("summary %v", summary)
	}

	stdout.Reset()
	if err = run([]string{"foundation-demo", "-directory", dir}, pgtest.AppDSN(t), &stdout); err == nil || !strings.Contains(err.Error(), "fresh directory required") {
		t.Fatalf("second run into the same directory: %v", err)
	}
	again, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(again, raw) || stdout.Len() != 0 {
		t.Fatalf("a refused run must leave the result as it was: %v", err)
	}
}
