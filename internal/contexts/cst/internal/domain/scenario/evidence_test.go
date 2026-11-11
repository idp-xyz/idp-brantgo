package scenario

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/costing"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// withSources is the fixture whose context verification and supply conditions cite sources: the
// context's seal src.ctx and an assumption-free draft, offer.1's content src.offer and its
// verification src.verify, log.1's verification src.verify.
func withSources() Inputs {
	in := fixture()
	in.Context.Revision.Seal.Evidence = []costing.Evidence{{SourceID: "src.ctx", Locator: "p.1"}}
	offer := Ref{Kind: KindSupplierOffer, ID: "offer.1", Revision: 1}
	s := in.Supply[offer]
	s.Sources = []string{"src.offer", "src.verify"}
	in.Supply[offer] = s
	log := Ref{Kind: KindLogisticsOffer, ID: "log.1", Revision: 1}
	l := in.Supply[log]
	l.Sources = []string{"src.verify"}
	in.Supply[log] = l
	in.Sources = map[string]Source{"src.ctx": {ContentHash: "h-ctx"}, "src.offer": {ContentHash: "h-offer"}, "src.verify": {ContentHash: "h-verify"}}
	return in
}

// Python _source on every business revision an evaluation reads: each source is bound once with
// its content hash, in ID order.
func TestEvaluateBindsTheSourcesItRestsOn(t *testing.T) {
	c, err := Evaluate(withSources())
	ok(t, err)
	var got []BoundInput
	for _, b := range c.Input.Inputs {
		if b.Context == FromEVD {
			got = append(got, b)
		}
	}
	want := []BoundInput{{Context: FromEVD, Kind: KindSource, ID: "src.ctx", Revision: 1, ContentHash: "h-ctx", Use: UseEvidence},
		{Context: FromEVD, Kind: KindSource, ID: "src.offer", Revision: 1, ContentHash: "h-offer", Use: UseEvidence},
		{Context: FromEVD, Kind: KindSource, ID: "src.verify", Revision: 1, ContentHash: "h-verify", Use: UseEvidence}}
	if len(got) != len(want) {
		t.Fatalf("sources %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("source %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	plain, err := Evaluate(fixture())
	ok(t, err)
	if plain.InputHash == c.InputHash || plain.ResultHash == c.ResultHash {
		t.Fatal("the sources are part of the normalized input")
	}
}

// Python _source: a source the caller does not read is PERMISSION_DENIED without detail, a denied
// one REFERENCE_STALE at its path (Python /source_denial); the first in ID order decides.
func TestEvaluateRefusesUnreadableAndDeniedSources(t *testing.T) {
	in := withSources()
	delete(in.Sources, "src.offer")
	_, err := Evaluate(in)
	rejected(t, err, kernel.CodePermissionDenied, "/")

	in = withSources()
	in.Sources["src.verify"] = Source{ContentHash: "h-verify", Denied: true}
	_, err = Evaluate(in)
	rejected(t, err, CodeReferenceStale, "/sources/src.verify")

	// An anchor context's sources are part of the chain.
	in = withSources()
	in.Contexts = map[ContextRef]AnchorContext{{ContextID: "anchor", Revision: 1}: {Sealed: true, Status: "active", Hash: "a", Sources: []string{"src.anchor"}}}
	e := &engine{in: in}
	if got := e.sourcesOf(BoundInput{Context: FromCST, Kind: KindCostingContext, ID: "anchor", Revision: 1, Use: UseAnchor}); len(got) != 1 || got[0] != "src.anchor" {
		t.Fatalf("anchor sources %v", got)
	}
}

func TestContextSources(t *testing.T) {
	r := &costing.Revision{Seal: &costing.Seal{Evidence: []costing.Evidence{{SourceID: "a"}}},
		Data: costing.Data{Assumptions: []costing.Assumption{{ID: "x", Evidence: []costing.Evidence{{SourceID: "b"}, {SourceID: "a"}}}}}}
	if got := ContextSources(r); len(got) != 3 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("sources %v", got)
	}
	if got := ContextSources(&costing.Revision{}); len(got) != 0 {
		t.Fatalf("a working revision rests on no verification: %v", got)
	}
}
