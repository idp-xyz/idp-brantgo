package declaration

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Python V0.50 has no declaration profile ("—（Python 未实现）"); these vectors come from the design:
// CAT-INV-002, SM-CAT-DECLARATION, S5 D-22, MD-41 and the role templates' independence row.

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return "OK"
}

var (
	agent  = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}
	doc1   = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "doc.1"}
	doc2   = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "doc.2"}
	at     = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	stKey  = Key{SkuID: "ST01.red.S", SpecRevision: 1, CodeTableVersion: "CN-2026"}
	tshirt = Content{
		Commodity:       Commodity{Code: "6109100021", Basis: "合成：针织棉制 T 恤，按税则归类", SourceIDs: []string{"spec.2", "spec.1"}},
		DeclarationName: "棉制针织T恤衫",
		Elements:        []Element{{Name: "品牌类型", Value: "无品牌"}, {Name: "成分含量", Value: "100%棉"}},
		LegalUnits:      []LegalUnit{{Unit: "件", PerBaseUnit: "1", Basis: "一件即一 PCS"}, {Unit: "千克", PerBaseUnit: "0.250", Basis: "合成：实测单件净重"}},
		Origin:          Origin{Country: "CN", Criterion: WhollyObtained, Basis: "合成：境内生产"},
		ExportControl:   ExportControl{Status: NotControlled, Basis: "合成：不属于管制清单"},
	}
)

// D-22: a profile keeps the SKU specification revision and code table version of its first save;
// its draft revision is saved whole; a verified revision is never touched by a later save, which
// starts the next revision instead.
func TestSaveFixesTheKeyAndKeepsVerifiedRevisions(t *testing.T) {
	if got := code(CheckSave(nil, 1, stKey)); got != "NOT_FOUND /args/id" {
		t.Fatal(got)
	}
	if got := code(CheckSave(nil, 0, stKey)); got != "OK" {
		t.Fatal(got)
	}
	r := New("profile.1", stKey)
	d, created := r.Save(tshirt, agent, 1, at)
	if !created || d.Number != 1 || r.Version != 1 || r.Draft != d || d.Status != Draft || d.SavedBy != agent || d.Hash != tshirt.Hash() {
		t.Fatalf("%+v %+v", r, d)
	}
	for _, c := range []struct {
		expected int64
		key      Key
		want     string
	}{
		{0, stKey, "VERSION_CONFLICT /args/expected_version"},
		{1, Key{SkuID: "ST01.red.M", SpecRevision: 1, CodeTableVersion: "CN-2026"}, "INVALID_KNOWN_VALUE /args/sku_id"},
		{1, Key{SkuID: "ST01.red.S", SpecRevision: 2, CodeTableVersion: "CN-2026"}, "INVALID_KNOWN_VALUE /args/spec_revision"},
		{1, Key{SkuID: "ST01.red.S", SpecRevision: 1, CodeTableVersion: "CN-2027"}, "INVALID_KNOWN_VALUE /args/code_table_version"},
		{1, stKey, "OK"},
	} {
		if got := code(CheckSave(r, c.expected, c.key)); got != c.want {
			t.Errorf("%d %+v: %s, want %s", c.expected, c.key, got, c.want)
		}
	}
	edited := tshirt
	edited.DeclarationName = "棉制针织T恤"
	again, created := r.Save(edited, doc1, 2, at.Add(time.Minute))
	if created || again != d || d.Content.DeclarationName != "棉制针织T恤" || d.SavedBy != doc1 || r.Version != 2 || r.LastRevision != 1 {
		t.Fatalf("the draft is replaced whole by its last editor: %+v %+v", r, d)
	}
	if code(CheckKey(true)) != "BUSINESS_KEY_CONFLICT /args/id" || CheckKey(false) != nil {
		t.Fatal("one profile per key")
	}

	if _, err := r.CheckVerify(1, doc2); err != nil {
		t.Fatal(err)
	}
	r.Verify(doc2, 3, []string{"ruling.1"}, at.Add(2*time.Minute))
	verified := r.Verified
	frozen := *verified
	next, created := r.Save(tshirt, agent, 4, at.Add(3*time.Minute))
	if !created || next.Number != 2 || r.Draft != next || r.Verified != verified || r.LastRevision != 2 || r.Version != 4 {
		t.Fatalf("an update is the next revision: %+v", r)
	}
	if verified.Content.DeclarationName != frozen.Content.DeclarationName || verified.Hash != frozen.Hash || verified.Status != Verified {
		t.Fatalf("the verified revision is untouched: %+v", verified)
	}
}

// CAT-INV-002 and SM-CAT-DECLARATION: draft → verified by a human other than the revision's last
// editor; verifying the next revision supersedes the verified one in the same step.
func TestVerifyIsIndependentOfTheLastEditorAndSupersedes(t *testing.T) {
	r := New("profile.1", stKey)
	r.Save(tshirt, doc1, 1, at)
	for _, c := range []struct {
		number int64
		by     kernel.ActorRef
		want   string
	}{
		{2, doc2, "NOT_FOUND /args/revision"},
		{1, doc1, "INDEPENDENT_REVIEW_REQUIRED /args/revision"},
		// The same ID as another kind is the same editor: independence compares actor IDs, as the
		// database's CHECK does.
		{1, kernel.ActorRef{Kind: kernel.ActorAI, ID: "doc.1"}, "INDEPENDENT_REVIEW_REQUIRED /args/revision"},
		{1, doc2, "OK"},
	} {
		if _, err := r.CheckVerify(c.number, c.by); code(err) != c.want {
			t.Errorf("%d by %+v: %s, want %s", c.number, c.by, code(err), c.want)
		}
	}
	if superseded := r.Verify(doc2, 2, []string{"ruling.2", "ruling.1"}, at); superseded != nil {
		t.Fatalf("nothing was verified before: %+v", superseded)
	}
	first := r.Verified
	if first.Status != Verified || r.Draft != nil || r.Version != 2 || first.Verification.By != doc2 ||
		!slices.Equal(first.Verification.EvidenceIDs, []string{"ruling.1", "ruling.2"}) {
		t.Fatalf("verified %+v", r)
	}
	if _, err := r.CheckVerify(1, doc2); code(err) != "INVALID_STATE /args/revision" {
		t.Fatalf("a verified revision is not verified again: %v", err)
	}

	// An AI candidate is saved like any draft; the human who verifies it is independent of it.
	r.Save(tshirt, agent, 3, at.Add(time.Hour))
	if _, err := r.CheckVerify(2, doc1); err != nil {
		t.Fatal(err)
	}
	superseded := r.Verify(doc1, 3, []string{"ruling.3"}, at.Add(2*time.Hour))
	if superseded != first || first.Status != Superseded || !first.SupersededAt.Equal(at.Add(2*time.Hour)) || first.Verification.By != doc2 ||
		r.Verified.Number != 2 || r.Verified.Status != Verified || r.Version != 4 {
		t.Fatalf("supersession %+v %+v", first, r.Verified)
	}
}

// The content hash is the hash of the canonical content: source IDs are a set, a conversion factor
// is a number (0.250 is 0.25), the order of declaration elements is part of the content.
func TestCanonicalContentHashesEqually(t *testing.T) {
	same := tshirt
	same.Commodity.SourceIDs = []string{"spec.1", "spec.2", "spec.1"}
	same.LegalUnits = []LegalUnit{{Unit: "件", PerBaseUnit: "1.0", Basis: "一件即一 PCS"}, {Unit: "千克", PerBaseUnit: "0.25", Basis: "合成：实测单件净重"}}
	if same.Hash() != tshirt.Hash() {
		t.Fatalf("equal content hashes differently:\n%s\n%s", same.Canonical(), tshirt.Canonical())
	}
	n := Normalize(tshirt)
	if !slices.Equal(n.Commodity.SourceIDs, []string{"spec.1", "spec.2"}) || n.LegalUnits[1].PerBaseUnit != "0.25" ||
		n.Origin.SourceIDs == nil || !slices.Equal(tshirt.SourceIDs(), []string{"spec.1", "spec.2"}) {
		t.Fatalf("normalized %+v", n)
	}
	reordered := tshirt
	reordered.Elements = []Element{tshirt.Elements[1], tshirt.Elements[0]}
	controlled := tshirt
	controlled.ExportControl = ExportControl{Status: Controlled, ListReference: "合成管制清单 1C999", Basis: "合成"}
	for _, other := range []Content{reordered, controlled} {
		if other.Hash() == tshirt.Hash() {
			t.Fatalf("different content hashes equally: %s", other.Canonical())
		}
	}
	if strings.Contains(string(tshirt.Canonical()), `"list_reference"`) || !strings.Contains(string(controlled.Canonical()), `"list_reference"`) {
		t.Fatal("a list reference is part of a controlled classification only")
	}
}

// A profile is kept for any confirmed specification revision of a SKU the actor sees, also one the
// SKU has moved on from: goods made to it are still declared.
func TestCheckSku(t *testing.T) {
	for _, c := range []struct {
		visible        bool
		confirmed, rev int64
		want           string
	}{
		{false, 2, 1, "REFERENCE_INVALID /sku_id"},
		{true, 0, 1, "REFERENCE_INVALID /spec_revision"},
		{true, 2, 3, "REFERENCE_INVALID /spec_revision"},
		{true, 2, 1, "OK"},
		{true, 2, 2, "OK"},
	} {
		if got := code(CheckSku(c.visible, c.confirmed, c.rev)); got != c.want {
			t.Errorf("%+v: %s", c, got)
		}
	}
}

// DeclarationProfileRef v1 (R39, CAT-INV-002): only a verified revision answers; a draft is a
// candidate (DECLARATION_PROFILE_UNVERIFIED); a superseded revision is stale for new use but still
// names what was verified; a revision that never existed is invalid.
func TestResolveAnswersEveryReference(t *testing.T) {
	r := New("profile.1", stKey)
	r.Save(tshirt, agent, 1, at)
	draft1 := r.Draft
	for _, c := range []struct {
		root  *Root
		asked int64
		rev   *Revision
		want  string
		names *Revision
	}{
		{nil, 0, nil, CodeDeclarationProfileUnverified, nil},
		{nil, 1, nil, kernel.CodeReferenceInvalid, nil},
		{r, 0, nil, CodeDeclarationProfileUnverified, nil},
		{r, 1, draft1, CodeDeclarationProfileUnverified, draft1},
		{r, 2, nil, kernel.CodeReferenceInvalid, nil},
	} {
		a := Resolve(c.root, c.asked, c.rev)
		if a.Code != c.want || a.Revision != c.names {
			t.Errorf("before verification, %d: %+v, want %s", c.asked, a, c.want)
		}
	}
	r.Verify(doc2, 2, []string{"ruling.1"}, at)
	first := r.Verified
	r.Save(tshirt, doc1, 3, at)
	draft2 := r.Draft
	r.Verify(doc2, 4, []string{"ruling.2"}, at)
	second := r.Verified
	for _, c := range []struct {
		asked int64
		rev   *Revision
		want  string
		names *Revision
	}{
		{0, nil, "", second},
		{2, draft2, "", second},
		{1, first, CodeReferenceStale, first},
		{3, nil, kernel.CodeReferenceInvalid, nil},
		{1, second, kernel.CodeReferenceInvalid, nil}, // a stored row of another number is not revision 1
	} {
		a := Resolve(r, c.asked, c.rev)
		if a.Code != c.want || a.Revision != c.names {
			t.Errorf("after supersession, %d: %+v, want %q", c.asked, a, c.want)
		}
	}
}
