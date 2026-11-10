package budget

import (
	"fmt"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The limits are T02 §16.4 and Python V0.50 brant_core/costing_engine/resource_budget.py LIMITS.

func code(t *testing.T, err error, want string) {
	t.Helper()
	r, ok := kernel.AsRejection(err)
	if !ok || r.Code != want {
		t.Fatalf("got %v, want %s", err, want)
	}
}

func key(kind string, i int) Key { return Key{Kind: kind, ID: fmt.Sprint(i), Version: 1} }

func TestDirectReferences(t *testing.T) {
	refs := make([]Key, 0, MaxDirectRefs+1)
	for i := range MaxDirectRefs {
		refs = append(refs, key("source", i), key("source", i)) // the same version counts once
	}
	if err := DirectRefs(refs); err != nil {
		t.Fatal(err)
	}
	refs = append(refs, Key{Kind: "source", ID: "0", Version: 2}) // another version counts again
	code(t, DirectRefs(refs), CodeCalculationLimitExceeded)

	sources := make([]Key, MaxCandidateSources+1)
	for i := range sources {
		sources[i] = key("supplier_offer", i)
	}
	if err := CandidateSources(sources[:MaxCandidateSources]); err != nil {
		t.Fatal(err)
	}
	code(t, CandidateSources(sources), CodeCalculationLimitExceeded)
}

// Python resource_budget.Budget.finish: a dependency chain of depth 8 passes, 9 is refused; a
// cycle is REFERENCE_INVALID; a node reached again counts its known height.
func TestDependencyDepthAndCycles(t *testing.T) {
	chain := func(n int) *Budget {
		b := New()
		for i := range n {
			if err := b.Observe(key("x", i), 10, []Key{key("x", i+1)}, 0); err != nil {
				t.Fatal(err)
			}
		}
		return b
	}
	report, err := chain(MaxDependencyDepth).Finish()
	if err != nil || report.LoadedObjects != 8 || report.ExpandedNodes != 9 || report.LoadedBytes != 80 {
		t.Fatalf("depth 8: %+v %v", report, err)
	}
	_, err = chain(MaxDependencyDepth + 1).Finish()
	code(t, err, CodeCalculationLimitExceeded)

	b := New()
	_ = b.Observe(key("x", 0), 1, []Key{key("x", 1)}, 0)
	_ = b.Observe(key("x", 1), 1, []Key{key("x", 0)}, 0)
	_, err = b.Finish()
	code(t, err, kernel.CodeReferenceInvalid)

	// A shallow root observed after a deep chain reaches the chain's tail: 1 + 8 > 8.
	b = chain(MaxDependencyDepth)
	_ = b.Observe(key("root", 0), 1, []Key{key("x", 0)}, 0)
	_, err = b.Finish()
	code(t, err, CodeCalculationLimitExceeded)
}

func TestCounters(t *testing.T) {
	b := New()
	if err := b.Observe(key("measurement", 1), 1, nil, MaxPackages); err != nil {
		t.Fatal(err)
	}
	if err := b.Observe(key("measurement", 1), 1, nil, MaxPackages); err != nil { // the same revision once
		t.Fatal(err)
	}
	code(t, b.Observe(key("measurement", 2), 1, nil, 1), CodeCalculationLimitExceeded)

	b = New()
	code(t, b.Observe(key("offer", 1), MaxLoadedBytes+1, nil, 0), CodeCalculationLimitExceeded)

	b = New()
	edges := make([]Key, MaxExpandedNodes)
	for i := range edges {
		edges[i] = key("source", i)
	}
	code(t, b.Observe(key("offer", 1), 1, edges, 0), CodeCalculationLimitExceeded)

	b = New()
	if err := b.ClaimComponents(MaxComponents); err != nil {
		t.Fatal(err)
	}
	code(t, b.ClaimComponents(1), CodeCalculationLimitExceeded)
	if err := b.Inclusions(MaxInclusionEdges); err != nil {
		t.Fatal(err)
	}
	code(t, b.Inclusions(1), CodeCalculationLimitExceeded)
	for range MaxOperations {
		if err := b.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	code(t, b.Tick(), CodeCalculationLimitExceeded)
}
