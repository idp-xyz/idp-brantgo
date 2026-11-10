package mapping

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return "OK"
}

var (
	editor   = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}
	reviewer = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"}
	at       = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	key      = Key{PartyID: "supplier", PartyRole: Supplier, ExternalCode: "SUP-01"}
)

func TestSaveFixesTheKeyAndReplacesTheDraftWhole(t *testing.T) {
	if got := code(CheckSave(nil, 1, key)); got != "NOT_FOUND /args/id" {
		t.Fatal(got)
	}
	if got := code(CheckSave(nil, 0, key)); got != "OK" {
		t.Fatal(got)
	}
	r := New("mapping1", key)
	d, created := r.Save(Target{SkuID: "sku", SpecRevision: 1}, editor, 1, at)
	if !created || d.Number != 1 || r.Version != 1 || r.Draft != d || d.Status != Draft {
		t.Fatalf("%+v %+v", r, d)
	}
	for _, c := range []struct {
		expected int64
		key      Key
		want     string
	}{
		{0, key, "VERSION_CONFLICT /args/expected_version"},
		{1, Key{PartyID: "other", PartyRole: Supplier, ExternalCode: "SUP-01"}, "INVALID_KNOWN_VALUE /args/party_id"},
		{1, Key{PartyID: "supplier", PartyRole: Customer, ExternalCode: "SUP-01"}, "INVALID_KNOWN_VALUE /args/party_role"},
		{1, Key{PartyID: "supplier", PartyRole: Supplier, ExternalCode: "SUP-1"}, "INVALID_KNOWN_VALUE /args/external_code"},
		{1, key, "OK"},
	} {
		if got := code(CheckSave(r, c.expected, c.key)); got != c.want {
			t.Errorf("%d %+v: %s, want %s", c.expected, c.key, got, c.want)
		}
	}
	again, created := r.Save(Target{SkuID: "sku", SpecRevision: 2}, reviewer, 2, at.Add(time.Minute))
	if created || again != d || d.Target.SpecRevision != 2 || d.SavedBy != reviewer || r.Version != 2 || r.LastRevision != 1 {
		t.Fatalf("the draft is replaced whole: %+v %+v", r, d)
	}
}

// Python test_foundation.py test_supplier_contacts_addresses_and_external_sku_mapping (the
// code_mapping half, as domain rules): a second mapping of the same party, role and external code is
// refused BUSINESS_KEY_CONFLICT; publishing a mapping's next revision supersedes its active one.
func TestPublicationSupersedesAndTheKeyIsPublishedOnce(t *testing.T) {
	r := New("mapping1", key)
	r.Save(Target{SkuID: "sku", SpecRevision: 1}, editor, 1, at)
	if got := code(r.CheckPublish(2)); got != "NOT_FOUND /args/revision" {
		t.Fatal("a revision never assigned:", got)
	}
	if got := code(r.CheckPublish(1)); got != "OK" {
		t.Fatal(got)
	}
	if got := code(CheckKey(true)); got != "BUSINESS_KEY_CONFLICT /args/id" {
		t.Fatal(got)
	}
	if rej, _ := kernel.AsRejection(CheckKey(true)); rej.Category != kernel.CategoryConflict {
		t.Fatalf("BUSINESS_KEY_CONFLICT is 409 in the error catalog: %+v", rej)
	}
	if code(CheckKey(false)) != "OK" {
		t.Fatal("a free key")
	}
	if superseded := r.Publish(reviewer, 3, []string{"evidence"}, at); superseded != nil || r.Active.Number != 1 || r.Draft != nil || r.Version != 2 {
		t.Fatalf("%+v", r)
	}
	if got := code(r.CheckPublish(1)); got != "INVALID_STATE /args/revision" {
		t.Fatal("a published revision is not published again:", got)
	}
	r.Save(Target{SkuID: "sku", SpecRevision: 2}, editor, 3, at)
	first := r.Active
	superseded := r.Publish(reviewer, 3, []string{"evidence"}, at.Add(time.Hour))
	if superseded != first || first.Status != Superseded || !first.SupersededAt.Equal(at.Add(time.Hour)) || !first.Publication.At.Equal(at) ||
		r.Active.Number != 2 || r.Active.Target.SpecRevision != 2 || r.LastRevision != 2 || r.Version != 4 {
		t.Fatalf("%+v superseded %+v", r, superseded)
	}
	if r.Active.Publication.By != reviewer || r.Active.SavedBy != editor {
		t.Fatalf("publication %+v", r.Active)
	}
}
