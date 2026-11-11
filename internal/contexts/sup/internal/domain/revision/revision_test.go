package revision

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	author   = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "author"}
	reviewer = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"}
	t0       = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	evidence = []Evidence{{SourceID: "source.1", Locator: "line 1"}}
)

func content(hash string) Content {
	return Content{Data: []byte(`{}`), Hash: hash, Header: Header{ValidFrom: t0.Add(-time.Hour), ValidUntil: t0.Add(24 * time.Hour)}}
}

func rejected(t *testing.T, err error, code, path string) {
	t.Helper()
	r, ok := kernel.AsRejection(err)
	if !ok || r.Code != code || r.Fields[0].Path != path {
		t.Fatalf("got %v, want %s at %s", err, code, path)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// sealed returns a root whose revision 1 the reviewer verified.
func sealed(t *testing.T, kind Kind) *Root {
	r := New(kind, "offer.1", "author", "trade")
	_, created, err := r.Save(content("h1"), author, 1, t0)
	must(t, err)
	if !created || r.Version != 1 || r.Working.Number != 1 {
		t.Fatalf("first save %+v", r)
	}
	must(t, r.CheckSeal(1, 0, reviewer))
	if superseded := r.Seal(reviewer, 1, evidence, t0); superseded != nil {
		t.Fatalf("superseded %+v", superseded)
	}
	return r
}

// Python test_costing_lifecycle.py PersistentLifecycleTests.lifecycle, as the aggregate decides it:
// save, verify, revise, discard, revise, save, verify superseding revision 1, withdraw revision 1;
// the root is at version 8 with revision 3 active (T02 §13.2).
func TestLifecycle(t *testing.T) {
	r := sealed(t, SupplierOffer)
	r1 := r.Active
	rev2, err := r.Revise(r1, author, 1, t0)
	must(t, err)
	if rev2.Number != 2 || rev2.Base != 1 || rev2.Content.Hash != "h1" || r.Active != r1 {
		t.Fatalf("revise %+v", rev2)
	}
	discarded, err := r.Discard(2, author, 1, "丢弃", t0)
	must(t, err)
	if discarded.Status != Discarded || discarded.End.Reason != "丢弃" || r.Working != nil {
		t.Fatalf("discard %+v", discarded)
	}
	rev3, err := r.Revise(r1, author, 1, t0)
	must(t, err)
	if rev3.Number != 3 {
		t.Fatalf("a discarded number is not reused: %d", rev3.Number)
	}
	_, created, err := r.Save(content("h3"), author, 1, t0)
	must(t, err)
	if created {
		t.Fatal("the draft is replaced, not created")
	}
	rejected(t, r.CheckSeal(3, 0, reviewer), kernel.CodeVersionConflict, "/args/supersedes_revision")
	must(t, r.CheckSeal(3, 1, reviewer))
	superseded := r.Seal(reviewer, 1, evidence, t0.Add(time.Minute))
	if superseded != r1 || r1.Status != Superseded || !r1.SupersededAt.Equal(t0.Add(time.Minute)) || r.Active != rev3 {
		t.Fatalf("supersession %+v", r)
	}
	must(t, r.End(r1, reviewer, 1, "撤销旧修订", evidence, t0))
	if r.Version != 8 || r.Active != rev3 || r1.Status != Withdrawn || !r1.SupersededAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after withdrawing the superseded revision: %+v %+v", r, r1)
	}
}

// SUP-INV-001: a sealed root takes a new draft only through revise; revise needs a sealed base and
// no draft; sealing names the draft; superseded and withdrawn revisions are never active again.
func TestStateRules(t *testing.T) {
	r := sealed(t, SupplierOffer)
	_, _, err := r.Save(content("h2"), author, 1, t0)
	rejected(t, err, kernel.CodeInvalidState, "/args/id")
	rejected(t, r.CheckSeal(1, 1, reviewer), kernel.CodeInvalidState, "/args/working_revision")
	draft, err := r.Revise(r.Active, author, 1, t0)
	must(t, err)
	_, err = r.Revise(r.Active, author, 1, t0)
	rejected(t, err, kernel.CodeInvalidState, "/args/base_revision")
	_, err = r.Discard(1, author, 1, "x", t0)
	rejected(t, err, kernel.CodeInvalidState, "/args/working_revision")
	rejected(t, r.End(draft, reviewer, 1, "x", evidence, t0), kernel.CodeInvalidState, "/args/target_revision")
	rejected(t, r.End(nil, reviewer, 1, "x", evidence, t0), kernel.CodeInvalidState, "/args/target_revision")
	_, err = New(SupplierOffer, "x", "a", "").Revise(nil, author, 1, t0)
	rejected(t, err, kernel.CodeInvalidState, "/args/base_revision")

	active := r.Active
	must(t, r.End(active, reviewer, 1, "撤销", evidence, t0))
	if r.Active != nil || active.Status != Withdrawn {
		t.Fatalf("withdrawing the active revision leaves none: %+v", r)
	}
	rejected(t, r.End(active, reviewer, 1, "again", evidence, t0), kernel.CodeInvalidState, "/args/target_revision")
	must(t, r.CheckSeal(draft.Number, 0, reviewer))

	// A root that was never sealed starts a new draft after a discard.
	fresh := New(LogisticsOffer, "log.1", "author", "")
	_, _, err = fresh.Save(content("h"), author, 1, t0)
	must(t, err)
	_, err = fresh.Discard(1, author, 1, "x", t0)
	must(t, err)
	rev, created, err := fresh.Save(content("h"), author, 1, t0)
	must(t, err)
	if !created || rev.Number != 2 {
		t.Fatalf("after a discard %+v", rev)
	}
}

// SUP-INV-003: nobody who edited a draft seals it, whoever saved it last; purchase terms end as
// retired, offers as withdrawn.
func TestIndependentSealAndEndStatus(t *testing.T) {
	r := New(PurchaseTerms, "terms.1", "author", "")
	_, _, err := r.Save(content("h"), author, 1, t0)
	must(t, err)
	_, _, err = r.Save(content("h"), reviewer, 1, t0) // the reviewer edits too
	must(t, err)
	if got := r.Working.Contributors; len(got) != 2 || got[0] != "author" || got[1] != "reviewer" {
		t.Fatalf("contributors %v", got)
	}
	rejected(t, r.CheckSeal(1, 0, author), CodeIndependentReviewRequired, "/args/working_revision")
	rejected(t, r.CheckSeal(1, 0, reviewer), CodeIndependentReviewRequired, "/args/working_revision")
	third := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "publisher"}
	must(t, r.CheckSeal(1, 0, third))
	r.Seal(third, 1, evidence, t0)
	must(t, r.End(r.Active, third, 1, "停用", evidence, t0))
	if r.Named != nil || r.Revision(1) != nil {
		t.Fatalf("the ended revision is no longer working or active: %+v", r)
	}
	if PurchaseTerms.EndStatus() != Retired || SupplierOffer.EndStatus() != Withdrawn || LogisticsOffer.EndStatus() != Withdrawn {
		t.Fatal("end statuses")
	}
}

// The guard's rule: active, verified by the instant, validity containing it; dependent purchase
// terms also need their source offer in force.
func TestInForce(t *testing.T) {
	r := sealed(t, SupplierOffer)
	rev := r.Active
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{t0, ""},
		{t0.Add(-time.Second), CodeSupplyConditionNotCurrent}, // before the verification
		{t0.Add(24 * time.Hour), CodeSupplyConditionExpired},  // the end is exclusive
		{t0.Add(24*time.Hour - time.Microsecond), ""},
	} {
		if got := InForce(rev, c.at); got != c.want {
			t.Errorf("at %s: %q, want %q", c.at, got, c.want)
		}
	}
	if InForce(nil, t0) != CodeSupplyConditionNotCurrent {
		t.Fatal("a missing revision is not current")
	}
	terms := sealed(t, PurchaseTerms).Active
	terms.Content.Header.ValidityMode = "dependent_validity"
	if InForceWith(terms, rev, t0) != "" || InForceWith(terms, nil, t0) != CodeSupplyConditionNotCurrent {
		t.Fatal("dependent terms follow their source offer")
	}
	must(t, r.End(rev, reviewer, 1, "撤销", evidence, t0))
	if InForce(rev, t0) != CodeSupplyConditionNotCurrent || InForceWith(terms, rev, t0) != CodeSupplyConditionNotCurrent {
		t.Fatal("a withdrawn offer and the terms that depend on it are not current")
	}
	terms.Content.Header.ValidityMode = "independent_validity"
	if InForceWith(terms, rev, t0) != "" {
		t.Fatal("independent terms keep their own validity")
	}
}

// A save on a sealed root without a draft is refused for its state before anything else is
// judged.
func TestCheckSaveRefusesSealedRootsWithoutDraft(t *testing.T) {
	r := sealed(t, SupplierOffer)
	rejected(t, CheckSave(r, true, r.Version), kernel.CodeInvalidState, "/args/id")
	_, err := r.Revise(r.Active, author, 1, t0)
	must(t, err)
	must(t, CheckSave(r, true, r.Version))
}

func TestCheckAdmitsVisibleRootsAtTheirVersion(t *testing.T) {
	rejected(t, CheckSave(nil, false, 1), kernel.CodeNotFound, "/args/id")
	must(t, CheckSave(nil, false, 0))
	r := sealed(t, SupplierOffer)
	rejected(t, Check(r, false, r.Version), kernel.CodeNotFound, "/args/id")
	rejected(t, Check(r, true, 1), kernel.CodeVersionConflict, "/args/expected_version")
	must(t, Check(r, true, r.Version))
}
