//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The scenario of Python test_cli.py test_foundation_demo_has_explicit_synthetic_truth_values
// (brant_core/foundation_demo.py), checked in the database: every configuration of the result file
// is the revision in force, saved by the editor and published by the reviewer on the synthetic
// evidence; the snapshot holds what the lookups adopted; and a second run works in a company of
// its own with the same values. The CLI half is cmd/brant-cli TestFoundationDemoHasExplicitSyntheticTruthValues.
func TestFoundationDemoPublishesEveryKindIndependently(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	result, err := w.platform.FoundationDemo(ctx, bootstrap.DemoConfig{Directory: dir})
	w.must(err)
	raw, err := os.ReadFile(filepath.Join(dir, bootstrap.DemoResultFile))
	w.must(err)
	var saved bootstrap.DemoResult
	w.must(json.Unmarshal(raw, &saved))
	saved.ResultFile = result.ResultFile
	if !reflect.DeepEqual(saved, result) {
		t.Fatalf("the file holds %+v, the run returned %+v", saved, result)
	}

	if len(saved.Configs) != 20 || len(saved.Kinds) != 18 || slices.ContainsFunc(saved.Kinds, func(k string) bool { return !polapi.KindKnown(k) }) {
		t.Fatalf("configs %v, kinds %v", saved.Configs, saved.Kinds)
	}
	reader := w.provision(bootstrap.Grant{Scope: kernel.Scope{Tenant: bootstrap.DemoTenant, Company: kernel.CompanyID(saved.Company)},
		Actor: actorRef("human", "auditor"), Permissions: []string{"pol.read"}, DataScope: "company"})
	refs := make([]polapi.ConfigRef, len(saved.Configs))
	for i, c := range saved.Configs {
		refs[i] = polapi.ConfigRef{Kind: c.Kind, ConfigID: c.ID, Revision: c.Revision}
		v, err := w.c.POL.GetConfig(reader, c.Kind, c.ID)
		w.must(err)
		rev := v.Revisions[len(v.Revisions)-1]
		if v.ActiveRevision != c.Revision || rev.SavedBy != actorRef("human", "editor") || rev.PublishedBy != actorRef("human", "reviewer") ||
			!slices.Equal(rev.EvidenceIDs, []string{"evidence"}) {
			t.Fatalf("%s:%s is not independently published: %+v", c.Kind, c.ID, v)
		}
	}
	resolved, err := w.c.POL.ResolveConfigs(reader, refs, time.Now())
	w.must(err)
	for _, r := range refs {
		if resolved[r].Code != "" {
			t.Fatalf("%+v is not in force: %s", r, resolved[r].Code)
		}
	}
	snap, err := w.c.POL.GetSnapshot(reader, "snapshot")
	w.must(err)
	if snap.ContentHash != saved.Snapshot.ContentHash || len(snap.Entries) != len(saved.Snapshot.Entries) || len(saved.Lookups) != 3 {
		t.Fatalf("snapshot %+v, file %+v", snap, saved.Snapshot)
	}
	for _, l := range saved.Lookups {
		if !slices.Contains(saved.Snapshot.Entries, l.Adopted) || len(l.Adopted.ContentHash) != 64 {
			t.Fatalf("lookup %+v adopted a revision the snapshot does not hold", l)
		}
	}
	if saved.Number.DocumentNumber != "QT-"+quoteYear(t)+"-00001" || len(saved.Commands) != 45 {
		t.Fatalf("number %+v after %d commands", saved.Number, len(saved.Commands))
	}

	again, err := w.platform.FoundationDemo(ctx, bootstrap.DemoConfig{Directory: t.TempDir()})
	w.must(err)
	if again.Company == saved.Company || again.FX != saved.FX || again.Unit != saved.Unit || again.Tax != saved.Tax ||
		again.Configurations != saved.Configurations {
		t.Fatalf("second run %+v", again)
	}
}
