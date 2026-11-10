package screening

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	at         = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	compliance = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "compliance"}
	screener   = kernel.ActorRef{Kind: kernel.ActorAI, ID: "screener"}
	h1         = "1111111111111111111111111111111111111111111111111111111111111111"
	h2         = "2222222222222222222222222222222222222222222222222222222222222222"
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}

func reg(lists ...EnabledList) Regulation {
	return Regulation{ID: "screening", Revision: 1, Hash: h1, Lists: lists, RescreenDays: 30}
}

var (
	cn = EnabledList{ListCode: "cn.uel", Jurisdiction: "CN"}
	un = EnabledList{ListCode: "un.consolidated", Jurisdiction: "UN"}
)

func versions(vs ...ListVersion) map[string]ListVersion {
	m := map[string]ListVersion{}
	for _, v := range vs {
		m[v.ListCode] = v
	}
	return m
}

func hit(list, version, entry, hash string) Hit {
	return Hit{ListCode: list, Version: version, EntryID: entry, EntryHash: hash, PrimaryName: "Acme Trading", MatchedName: "Acme Trading",
		NameKey: "acme trading"}
}

func input(party Party, hits ...Hit) Input {
	return Input{ID: "scr-1", Party: party, Regulation: reg(cn, un),
		Versions: versions(ListVersion{ListCode: "cn.uel", Jurisdiction: "CN", Version: "v1", ContentHash: h1},
			ListVersion{ListCode: "un.consolidated", Jurisdiction: "UN", Version: "2026-11", ContentHash: h2}),
		Hits: hits, Confirmed: map[EntryKey]bool{}, Normalization: "name-key-v1", By: screener, Epoch: 1, At: at}
}

// TD-AT15 (PTY-INV-002) at the domain level: a suspected hit is a match; compliance decides it a
// false positive and registers a positive-list entry; the entry applies while the party revision
// and the list entry's content are those it was registered for, and stops applying when either
// changes.
func TestTDAT15PositiveListBindsRevisionAndEntryContent(t *testing.T) {
	acme := Party{ID: "acme", Revision: 1, Name: "Acme Trading", Confirmed: true}
	res, err := Screen(input(acme, hit("cn.uel", "v1", "E1", h1)))
	if err != nil || res.Outcome != OutcomeMatch || res.Status != StatusMatch || res.Hits[0].Disposition != Suspected {
		t.Fatalf("suspected hit: %+v %v", res, err)
	}
	if !res.ValidUntil.Equal(at.Add(30*24*time.Hour)) || len(res.Lists) != 2 || len(res.Missing) != 0 {
		t.Fatalf("validity and lists %+v", res)
	}
	root := &Root{PartyID: "acme"}
	root.Record(res)
	n := 0
	newID := func() string { n++; return "pos-" + string(rune('0'+n)) }
	positives, err := root.Decide(res, &acme, "scr-1", FalsePositive, true, newID, "different registered address", compliance, 2, at)
	if err != nil || root.Status != StatusClear || root.RowVersion != 2 || len(positives) != 1 || positives[0].EntryHash != h1 ||
		positives[0].PartyRevision != 1 {
		t.Fatalf("false positive: %+v %+v %v", root, positives, err)
	}
	// Re-screened unchanged: the positive-list entry covers the hit.
	again := input(acme, hit("cn.uel", "v1", "E1", h1))
	again.Positives = positives
	res, _ = Screen(again)
	if res.Outcome != OutcomeClear || res.Hits[0].Disposition != PositiveList || res.Hits[0].PositiveID != "pos-1" {
		t.Fatalf("covered hit: %+v", res)
	}
	// The list entry changed in a new version: the positive-list entry no longer applies.
	changed := input(acme, hit("cn.uel", "v2", "E1", h2))
	changed.Versions["cn.uel"] = ListVersion{ListCode: "cn.uel", Jurisdiction: "CN", Version: "v2", ContentHash: h2}
	changed.Positives = positives
	if res, _ = Screen(changed); res.Outcome != OutcomeMatch || res.Hits[0].Disposition != Suspected {
		t.Fatalf("changed entry: %+v", res)
	}
	// The party was confirmed in a new revision: the positive-list entry no longer applies.
	revised := input(Party{ID: "acme", Revision: 2, Name: "Acme Trading", Confirmed: true}, hit("cn.uel", "v1", "E1", h1))
	revised.Positives = positives
	if res, _ = Screen(revised); res.Outcome != OutcomeMatch || res.Hits[0].Disposition != Suspected {
		t.Fatalf("new revision: %+v", res)
	}
}

// A true match stays blocked: re-screening keeps the confirmed hit blocked while the entry is
// listed, even when its content changes; a delisted entry no longer hits.
func TestTrueMatchStaysBlocked(t *testing.T) {
	acme := Party{ID: "acme", Revision: 1, Name: "Acme Trading", Confirmed: true}
	res, _ := Screen(input(acme, hit("cn.uel", "v1", "E1", h1)))
	root := &Root{PartyID: "acme"}
	root.Record(res)
	positives, err := root.Decide(res, &acme, "scr-1", TrueMatch, false, nil, "same entity", compliance, 2, at)
	if err != nil || root.Status != StatusBlocked || positives != nil {
		t.Fatalf("true match: %+v %v", root, err)
	}
	again := input(acme, hit("cn.uel", "v2", "E1", h2))
	again.Versions["cn.uel"] = ListVersion{ListCode: "cn.uel", Jurisdiction: "CN", Version: "v2", ContentHash: h2}
	again.Confirmed[EntryKey{"cn.uel", "E1"}] = true
	if res, _ = Screen(again); res.Outcome != OutcomeMatch || res.Status != StatusBlocked || res.Hits[0].Disposition != ConfirmedMatch {
		t.Fatalf("confirmed hit: %+v", res)
	}
	delisted := input(acme)
	delisted.Confirmed[EntryKey{"cn.uel", "E1"}] = true
	if res, _ = Screen(delisted); res.Status != StatusClear {
		t.Fatalf("delisted: %+v", res)
	}
	if _, err = root.Decide(res, &acme, "scr-1", FalsePositive, false, nil, "x", compliance, 3, at); code(err) != "INVALID_STATE /args/screening_id" {
		t.Fatalf("a blocked conclusion awaits no decision: %s", code(err))
	}
}

// PTY-INV-002: only the lists the regulation enables are used, in their current versions; a list
// without an imported version leaves the screening pending; a list imported under another
// jurisdiction than the regulation enables is refused; validity never outlasts the regulation.
func TestOnlyEnabledListsInTheirCurrentVersions(t *testing.T) {
	acme := Party{ID: "acme", Revision: 1, Name: "Acme Trading", Confirmed: true}
	in := input(acme, hit("us.ofac.sdn", "2026-11", "S1", h1), hit("cn.uel", "v0", "E1", h1))
	if res, _ := Screen(in); res.Outcome != OutcomeClear || len(res.Hits) != 0 {
		t.Fatalf("hits outside the enabled lists or versions count: %+v", res)
	}
	missing := input(acme)
	delete(missing.Versions, "un.consolidated")
	res, _ := Screen(missing)
	if res.Outcome != OutcomePending || res.Status != StatusPending || len(res.Missing) != 1 || res.Missing[0] != "un.consolidated" {
		t.Fatalf("missing list: %+v", res)
	}
	root := &Root{PartyID: "acme"}
	missingHit := input(acme, hit("cn.uel", "v1", "E1", h1))
	delete(missingHit.Versions, "un.consolidated")
	res, _ = Screen(missingHit)
	root.Record(res)
	if _, err := root.Decide(res, &acme, res.ID, FalsePositive, false, nil, "x", compliance, 2, at); err != nil || root.Status != StatusPending {
		t.Fatalf("a cleared hit leaves an incomplete screening pending: %+v %v", root, err)
	}
	wrong := input(acme)
	wrong.Versions["cn.uel"] = ListVersion{ListCode: "cn.uel", Jurisdiction: "foreign", Version: "v1", ContentHash: h1}
	if _, err := Screen(wrong); code(err) != "REFERENCE_INVALID /args/regulation_id" {
		t.Fatalf("jurisdiction mismatch: %s", code(err))
	}
	short := input(acme)
	short.Regulation.ValidUntil = at.Add(24 * time.Hour)
	if res, _ = Screen(short); !res.ValidUntil.Equal(short.Regulation.ValidUntil) {
		t.Fatalf("validity %s", res.ValidUntil)
	}
}

func TestDecisionsAndTheNegativeList(t *testing.T) {
	acme := Party{ID: "acme", Revision: 1, Name: "Acme Trading", Confirmed: true}
	res, _ := Screen(input(acme, hit("cn.uel", "v1", "E1", h1)))
	root := &Root{PartyID: "acme"}
	root.Record(res)
	if code(CheckVersion(root, 0)) != "VERSION_CONFLICT /args/expected_version" || CheckVersion(nil, 0) != nil ||
		code(CheckVersion(nil, 1)) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal("root version checks")
	}
	if _, err := root.Decide(res, &acme, "scr-old", FalsePositive, false, nil, "x", compliance, 2, at); code(err) != "REFERENCE_STALE /args/screening_id" {
		t.Fatalf("not the current screening: %s", code(err))
	}
	revised := Party{ID: "acme", Revision: 2, Name: "Acme Trading", Confirmed: true}
	if _, err := root.Decide(res, &revised, "scr-1", FalsePositive, false, nil, "x", compliance, 2, at); code(err) != "REFERENCE_STALE /args/screening_id" {
		t.Fatalf("party changed: %s", code(err))
	}
	if err := root.Block(); err != nil || !root.Negative || root.RowVersion != 2 {
		t.Fatalf("block %+v %v", root, err)
	}
	if code(root.Block()) != "INVALID_STATE /args/id" {
		t.Fatal("blocked once")
	}
	if _, err := root.Decide(res, &acme, "scr-1", FalsePositive, false, nil, "x", compliance, 2, at); code(err) != "INVALID_STATE /args/id" {
		t.Fatalf("no decision lifts the negative list: %s", code(err))
	}
	if code(CheckScreenable(&Party{ID: "d", Confirmed: false})) != "INVALID_STATE /args/id" {
		t.Fatal("only confirmed parties are screened")
	}
}
