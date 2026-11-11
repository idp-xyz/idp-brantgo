package quote

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The business hash of a frozen commercial revision (TRD-INV-002, WBS S3-01) is the hash of Python
// V0.50 t03_candidate/protocol.py, byte for byte: business_hash = SHA-256, in lower-case hex, of the
// UTF-8 bytes of canonical(normalize(commercial)).
//
// normalize walks the document with a JSON Pointer-like path in which every list index is "*":
// at a path of DecimalPaths a decimal string loses trailing fractional zeros and a trailing point
// ("120.000000" → "120", "0.00" → "0"); a list at a path of SortPaths is sorted, stably, by the
// tuple of the members SortPaths names (by its items when it names none), strings by Unicode code
// point and integers by value. Nothing else changes: no Unicode normalization, no trimming, line
// breaks kept.
//
// canonical is Python's json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"),
// allow_nan=False): object members in code point order of their names at every level, no
// whitespace, integers in decimal, and strings with only '"', '\\', the control characters below
// U+0020 escaped ("\b", "\t", "\n", "\f", "\r" by name, the others as "\u00xx" in lower-case hex);
// every other character, U+2028, U+2029 and U+007F included, is written as itself in UTF-8. A
// document holds objects, lists, strings, integers, booleans and null; floats and strings that are
// not valid UTF-8 are refused.

// NormalizationVersion names the normalization above (Python protocol.NORMALIZATION). A commercial
// document carries it as normalization_version.
const NormalizationVersion = "brant.quote-normalization/0.1.0"

// DecimalPaths are the paths whose decimal strings normalize drops trailing zeros from (Python
// protocol.DECIMAL_PATHS).
var DecimalPaths = []string{
	"/data/lines/*/quantity",
	"/data/lines/*/unit_price",
	"/data/lines/*/price_basis_quantity",
	"/data/lines/*/discount_ratio",
	"/data/charges/*/amount",
	"/calculation/payments/*/percent",
}

// SortPaths are the lists normalize sorts and the members they sort by (Python
// protocol.SORT_PATHS).
var SortPaths = map[string][]string{
	"/data/lines":                           {"line_id"},
	"/data/charges":                         {"charge_id"},
	"/calculation/lines":                    {"id"},
	"/calculation/charges":                  {"id"},
	"/calculation/payments":                 {"stage_id"},
	"/dependency_manifest":                  {"kind", "id", "version"},
	"/data/evidence":                        {"source_id", "locator"},
	"/data/lines/*/evidence":                {"source_id", "locator"},
	"/data/charges/*/evidence":              {"source_id", "locator"},
	"/calculation/excluded_cost_categories": {},
}

// ErrNotCanonical means a document cannot be normalized or encoded canonically: a float, a string
// that is not valid UTF-8, a value of a type JSON has no form for, or a list SortPaths names whose
// items lack a sort member or mix strings and integers in one.
var ErrNotCanonical = errors.New("quote: document is not canonical JSON")

// Tree decodes a JSON document into the tree normalize and Canonical walk: map[string]any, []any,
// string, json.Number, bool and nil. Duplicate member names keep the last value, as Python's
// json.loads does; contract inputs are decoded strictly before they get here.
func Tree(doc []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotCanonical, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data", ErrNotCanonical)
	}
	return v, nil
}

// TreeOf encodes v with encoding/json and decodes it as Tree: the tree of a document of this
// package's types.
func TreeOf(v any) (any, error) {
	doc, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotCanonical, err)
	}
	return Tree(doc)
}

// Normalize returns the normalized copy of tree (Python protocol.normalize, without its schema
// validation: callers validate documents before they hash them).
func Normalize(tree any) (any, error) { return normalize(tree, "") }

func normalize(v any, path string) (any, error) {
	if slices.Contains(DecimalPaths, path) {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s is not a decimal string", ErrNotCanonical, path)
		}
		return trimDecimal(s), nil
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			n, err := normalize(child, path+"/"+k)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			n, err := normalize(child, path+"/*")
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		if keys, ok := SortPaths[path]; ok {
			if err := sortItems(out, keys, path); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return v, nil
}

// trimDecimal is Python's value.rstrip("0").rstrip(".") for a value with a point, "0" when nothing
// is left.
func trimDecimal(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	t := strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if t == "" {
		return "0"
	}
	return t
}

func sortItems(items []any, keys []string, path string) error {
	tuples := make([][]any, len(items))
	for i, item := range items {
		if len(keys) == 0 {
			tuples[i] = []any{item}
			continue
		}
		obj, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: %s holds an item that is not an object", ErrNotCanonical, path)
		}
		for _, k := range keys {
			v, ok := obj[k]
			if !ok {
				return fmt.Errorf("%w: %s holds an item without %s", ErrNotCanonical, path, k)
			}
			tuples[i] = append(tuples[i], v)
		}
	}
	var failure error
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		for j := range tuples[a] {
			c, err := compare(tuples[a][j], tuples[b][j])
			if err != nil && failure == nil {
				failure = fmt.Errorf("%w: %s: %w", ErrNotCanonical, path, err)
			}
			if c != 0 {
				return c
			}
		}
		return 0
	})
	if failure != nil {
		return failure
	}
	sorted := make([]any, len(items))
	for i, j := range order {
		sorted[i] = items[j]
	}
	copy(items, sorted)
	return nil
}

// compare orders two sort members as Python compares them: strings by code point (byte order of
// valid UTF-8), integers by value; anything else cannot be ordered.
func compare(a, b any) (int, error) {
	if x, ok := a.(string); ok {
		if y, ok := b.(string); ok {
			return strings.Compare(x, y), nil
		}
		return 0, errors.New("a string and a non-string are not ordered")
	}
	x, err := integer(a)
	if err != nil {
		return 0, err
	}
	y, err := integer(b)
	if err != nil {
		return 0, err
	}
	return x.Cmp(y), nil
}

func integer(v any) (*big.Int, error) {
	switch x := v.(type) {
	case json.Number:
		n, ok := new(big.Int).SetString(string(x), 10)
		if !ok {
			return nil, fmt.Errorf("%s is not an integer", x)
		}
		return n, nil
	case int64:
		return big.NewInt(x), nil
	case int:
		return big.NewInt(int64(x)), nil
	}
	return nil, fmt.Errorf("%T is not ordered", v)
}

// Canonical returns the canonical JSON of tree.
func Canonical(tree any) ([]byte, error) {
	var b bytes.Buffer
	if err := encode(&b, tree); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func encode(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		return encodeString(b, x)
	case json.Number:
		if _, err := integer(x); err != nil {
			return fmt.Errorf("%w: %s is not an integer", ErrNotCanonical, x)
		}
		b.WriteString(string(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int:
		b.WriteString(strconv.Itoa(x))
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := encode(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, cmp.Compare[string])
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := encodeString(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := encode(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("%w: %T", ErrNotCanonical, v)
	}
	return nil
}

func encodeString(b *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: a string is not valid UTF-8", ErrNotCanonical)
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return nil
}

// Digest returns the normalized canonical bytes of tree and their SHA-256 in lower-case hex (Python
// protocol.business_hash for a commercial document).
func Digest(tree any) (canonical []byte, hash string, err error) {
	n, err := Normalize(tree)
	if err != nil {
		return nil, "", err
	}
	if canonical, err = Canonical(n); err != nil {
		return nil, "", err
	}
	return canonical, Hash(canonical), nil
}

// Hash returns the SHA-256 of b in lower-case hex.
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
