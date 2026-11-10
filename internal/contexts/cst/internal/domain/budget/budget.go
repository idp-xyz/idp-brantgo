// Package budget is the unified resource budget of a CST calculation (T02 §13.6, §16.4): distinct
// direct references per request, the objects a calculation actually loads and the references they
// carry, the dependency depth, expansion operations, loaded bytes, components, inclusion edges,
// measurement packages and candidate sources. A calculation beyond any limit is refused before it
// produces a result (CALCULATION_LIMIT_EXCEEDED); a dependency cycle is REFERENCE_INVALID.
//
// The request-level limits (1 MiB, 64 levels, 100,000 values, no floats or duplicate keys) belong
// to the transport's strict JSON decoding; the numeric bounds of single values to package rules.
// A Budget counts one calculation and is not safe for concurrent use.
package budget

import (
	"cmp"
	"fmt"
	"slices"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Limits of T02 §16.4.
const (
	MaxDirectRefs       = 1000     // distinct (kind, id, version) references of one request
	MaxExpandedNodes    = 5000     // loaded objects and the references they carry, deduplicated
	MaxDependencyDepth  = 8        // longest dependency chain
	MaxOperations       = 10000    // resolver calls and dependency traversal steps
	MaxLoadedBytes      = 64 << 20 // canonical UTF-8 payload of distinct loaded objects
	MaxComponents       = 500      // purchase components and charges, excluded ones included
	MaxInclusionEdges   = 2000     // edges of the charge inclusion graph
	MaxPackages         = 1000     // package lines over all distinct measurement revisions
	MaxCandidateSources = 200      // distinct supply condition revisions
)

// CodeCalculationLimitExceeded is the error catalog code of a budget refusal (422); package api
// declares the same.
const CodeCalculationLimitExceeded = "CALCULATION_LIMIT_EXCEEDED"

// Categories maps the code above to its error catalog category.
var Categories = map[string]kernel.Category{CodeCalculationLimitExceeded: kernel.CategoryInvalid}

func exceeded(what string, limit int) error {
	return kernel.RejectAs(CodeCalculationLimitExceeded, kernel.CategoryInvalid, "/", fmt.Sprintf("%s exceed %d", what, limit))
}

// Key identifies one version of an object: a configuration or business revision, a master row
// version or a source.
type Key struct {
	Kind    string
	ID      string
	Version int64
}

func compareKey(a, b Key) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Version, b.Version))
}

// DirectRefs refuses a request that names more than 1000 distinct references; the same object at
// another version counts again.
func DirectRefs(refs []Key) error {
	seen := make(map[Key]bool, len(refs))
	for _, r := range refs {
		seen[r] = true
	}
	if len(seen) > MaxDirectRefs {
		return exceeded("Direct references", MaxDirectRefs)
	}
	return nil
}

// CandidateSources refuses more than 200 distinct supply condition revisions.
func CandidateSources(sources []Key) error {
	seen := make(map[Key]bool, len(sources))
	for _, s := range sources {
		seen[s] = true
	}
	if len(seen) > MaxCandidateSources {
		return exceeded("Candidate sources", MaxCandidateSources)
	}
	return nil
}

// Budget counts what one calculation loads and expands.
type Budget struct {
	order          []Key
	graph          map[Key][]Key
	vertices       map[Key]bool
	operations     int
	components     int
	packages       int
	inclusionEdges int
	bytes          int64
}

// New returns an empty budget.
func New() *Budget { return &Budget{graph: map[Key][]Key{}, vertices: map[Key]bool{}} }

// Tick counts one resolver call or traversal step.
func (b *Budget) Tick() error {
	if b.operations++; b.operations > MaxOperations {
		return exceeded("Expansion operations", MaxOperations)
	}
	return nil
}

// Observe records an object the calculation loaded: its canonical payload size, the references it
// carries (a historical source-only use passes only its source references) and, for a measurement,
// its package lines. The same object version is counted once.
func (b *Budget) Observe(k Key, size int64, edges []Key, packages int) error {
	if _, seen := b.graph[k]; seen {
		return nil
	}
	if b.bytes += size; b.bytes > MaxLoadedBytes {
		return exceeded("Loaded bytes", MaxLoadedBytes)
	}
	out := make([]Key, 0, len(edges))
	for _, e := range edges {
		if e != k && !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, compareKey)
	b.graph[k] = out
	b.order = append(b.order, k)
	b.vertices[k] = true
	for _, e := range out {
		b.vertices[e] = true
	}
	if len(b.vertices) > MaxExpandedNodes {
		return exceeded("Expanded nodes", MaxExpandedNodes)
	}
	if b.packages += packages; b.packages > MaxPackages {
		return exceeded("Measurement package lines", MaxPackages)
	}
	return nil
}

// ClaimComponents counts n components of the calculation.
func (b *Budget) ClaimComponents(n int) error {
	if b.components += n; b.components > MaxComponents {
		return exceeded("Components", MaxComponents)
	}
	return nil
}

// Inclusions counts n inclusion edges of the charge graph.
func (b *Budget) Inclusions(n int) error {
	if b.inclusionEdges += n; b.inclusionEdges > MaxInclusionEdges {
		return exceeded("Inclusion edges", MaxInclusionEdges)
	}
	return nil
}

// Report is what a calculation used; the evaluation keeps it as evidence.
type Report struct {
	ExpandedNodes  int
	LoadedObjects  int
	Operations     int
	LoadedBytes    int64
	Components     int
	Packages       int
	InclusionEdges int
}

// Finish walks the dependency graph of the loaded objects, one tick per step: a chain longer than
// 8 is CALCULATION_LIMIT_EXCEEDED and a cycle REFERENCE_INVALID. It returns the usage.
func (b *Budget) Finish() (Report, error) {
	visiting := map[Key]bool{}
	height := map[Key]int{}
	var walk func(k Key, depth int) (int, error)
	walk = func(k Key, depth int) (int, error) {
		if err := b.Tick(); err != nil {
			return 0, err
		}
		if depth > MaxDependencyDepth {
			return 0, exceeded("Dependency depth", MaxDependencyDepth)
		}
		if visiting[k] {
			return 0, kernel.Reject(kernel.CodeReferenceInvalid, "/", "Dependencies form a cycle")
		}
		if h, done := height[k]; done {
			if depth+h > MaxDependencyDepth {
				return 0, exceeded("Dependency depth", MaxDependencyDepth)
			}
			return h, nil
		}
		visiting[k] = true
		h := 0
		for _, child := range b.graph[k] {
			c, err := walk(child, depth+1)
			if err != nil {
				return 0, err
			}
			h = max(h, c+1)
		}
		delete(visiting, k)
		height[k] = h
		return h, nil
	}
	for _, k := range b.order {
		if _, err := walk(k, 0); err != nil {
			return Report{}, err
		}
	}
	return Report{
		ExpandedNodes: len(b.vertices), LoadedObjects: len(b.order), Operations: b.operations, LoadedBytes: b.bytes,
		Components: b.components, Packages: b.packages, InclusionEdges: b.inclusionEdges,
	}, nil
}
