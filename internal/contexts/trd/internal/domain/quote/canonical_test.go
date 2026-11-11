package quote

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// pythonVectors is t03_candidate/examples/hash-vectors.json of Python V0.50, copied byte for byte
// (SHA-256 baf9228f7d4cb05897cacc2dfba3f067a30cf8409c2c7c4827256008665b7275).
const pythonVectors = "testdata/python-v050-hash-vectors.json"

type vector struct {
	Name       string          `json:"name"`
	Input      json.RawMessage `json:"input"`
	Normalized string          `json:"normalized_canonical"`
	SHA256     string          `json:"sha256"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	raw, err := os.ReadFile(pythonVectors)
	if err != nil {
		t.Fatal(err)
	}
	if got := Hash(raw); got != "baf9228f7d4cb05897cacc2dfba3f067a30cf8409c2c7c4827256008665b7275" {
		t.Fatalf("the vector file is not Python V0.50's: %s", got)
	}
	var vs []vector
	if err = json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) != 4 {
		t.Fatalf("%d vectors", len(vs))
	}
	return vs
}

// Python t03_candidate/verify.py "python-golden" (and interop.mjs, the independent Node check):
// every vector of hash-vectors.json normalizes to its normalized_canonical bytes and hashes to its
// sha256 — the full registered closure, equivalent decimal strings and manifest order (same hash),
// changed Chinese text with a kept line break and both forms of é (another hash), and evidence
// ordered by code point with a supplementary-plane character.
func TestPythonHashVectorsAreReproducedByteForByte(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			tree, err := Tree(v.Input)
			if err != nil {
				t.Fatal(err)
			}
			canonical, hash, err := Digest(tree)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(canonical, []byte(v.Normalized)) {
				t.Fatalf("canonical form differs at byte %d", firstDifference(canonical, []byte(v.Normalized)))
			}
			if hash != v.SHA256 {
				t.Fatalf("hash %s, want %s", hash, v.SHA256)
			}
			// The canonical form is a fixed point: normalizing it again changes nothing.
			again, err := Tree(canonical)
			if err != nil {
				t.Fatal(err)
			}
			if c, h, err := Digest(again); err != nil || !bytes.Equal(c, canonical) || h != hash {
				t.Fatalf("not a fixed point: %v", err)
			}
		})
	}
}

// The relations the vectors encode: equivalent decimal strings and another manifest order give the
// same hash; changed customer-visible text gives another.
func TestPythonVectorRelations(t *testing.T) {
	vs := loadVectors(t)
	if vs[0].SHA256 != vs[1].SHA256 || vs[0].Normalized != vs[1].Normalized {
		t.Fatal("equivalent numeric strings and manifest order must hash alike")
	}
	for _, i := range []int{2, 3} {
		if vs[i].SHA256 == vs[0].SHA256 {
			t.Fatalf("vector %q must hash differently", vs[i].Name)
		}
	}
}

func firstDifference(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// Canonical escapes exactly what Python's json.dumps(ensure_ascii=False) escapes.
func TestCanonicalStringEscapes(t *testing.T) {
	tree := map[string]any{"s": "\"\\\b\f\n\r\t\x00\x1f\x7f  é<>&𐀀"}
	got, err := Canonical(tree)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"s":"\"\\\b\f\n\r\t\u0000\u001f` + "\x7f  é<>&𐀀" + `"}`
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Floats, invalid UTF-8, unsortable lists and non-decimal values at decimal paths are refused.
func TestCanonicalRefusesWhatPythonRefuses(t *testing.T) {
	for name, doc := range map[string]string{
		"float":                 `{"a":1.5}`,
		"exponent":              `{"a":1e3}`,
		"decimal path number":   `{"data":{"lines":[{"line_id":"L1","quantity":120}]}}`,
		"sort member missing":   `{"data":{"lines":[{"line_id":"L1"},{"quantity":"1"}]}}`,
		"mixed sort member":     `{"dependency_manifest":[{"kind":"a","id":"x","version":1},{"kind":"a","id":"x","version":"1"}]}`,
		"sorted item no object": `{"data":{"charges":["x"]}}`,
	} {
		tree, err := Tree([]byte(doc))
		if err == nil {
			_, _, err = Digest(tree)
		}
		if !errors.Is(err, ErrNotCanonical) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Canonical(map[string]any{"s": "\xff"}); !errors.Is(err, ErrNotCanonical) {
		t.Errorf("invalid UTF-8: %v", err)
	}
	if _, err := Canonical(map[string]any{"f": 1.5}); !errors.Is(err, ErrNotCanonical) {
		t.Errorf("float64: %v", err)
	}
}

// Decimal trimming at the decimal paths only, and stable sorting by code point.
func TestNormalizeTrimsDecimalsAndSorts(t *testing.T) {
	tree, err := Tree([]byte(`{"data":{"lines":[{"line_id":"L2","quantity":"0.000","unit_price":"10.50"},
		{"line_id":"L1","quantity":"120","unit_price":"8.0000","note":"8.0000"}],
		"evidence":[{"source_id":"s","locator":"𐀀"},{"source_id":"s","locator":"合"}]},
		"calculation":{"excluded_cost_categories":["b","a"],"payments":[{"stage_id":"P2","percent":"70.0"},{"stage_id":"P1","percent":"30"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := Digest(tree)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"calculation":{"excluded_cost_categories":["a","b"],"payments":[{"percent":"30","stage_id":"P1"},{"percent":"70","stage_id":"P2"}]},` +
		`"data":{"evidence":[{"locator":"合","source_id":"s"},{"locator":"𐀀","source_id":"s"}],` +
		`"lines":[{"line_id":"L1","note":"8.0000","quantity":"120","unit_price":"8"},{"line_id":"L2","quantity":"0","unit_price":"10.5"}]}}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
