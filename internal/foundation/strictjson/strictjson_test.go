package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// request mirrors the shape of a Brant command request, with every optional member a pointer so
// that an absent member differs from a present one.
type request struct {
	Contract       string `json:"contract"`
	Command        string `json:"command"`
	IdempotencyKey string `json:"idempotency_key"`
	Args           *args  `json:"args,omitempty"`
}

type args struct {
	ID              string          `json:"id"`
	ExpectedVersion int64           `json:"expected_version"`
	CustomerID      *string         `json:"customer_id,omitempty"`
	Lines           *[]line         `json:"lines,omitempty"`
	Tags            *[]string       `json:"tags,omitempty"`
	Attributes      *map[string]any `json:"attributes,omitempty"`
	Priority        *int32          `json:"priority,omitempty"`
	Count           *uint16         `json:"count,omitempty"`
	Urgent          *bool           `json:"urgent,omitempty"`
	Pair            *[2]int64       `json:"pair,omitempty"`
}

type line struct {
	LineID   string  `json:"line_id"`
	Quantity *string `json:"quantity,omitempty"`
}

type rejectionCase struct {
	name   string
	body   string
	code   string
	path   string
	reason string
}

// rejectionCases decode into request. The Python V0.50 vectors come from
// tests/test_cli.py::test_local_execute_rejects_duplicate_keys_and_size_before_parsing and
// tests/test_core.py::test_null_extra_scope_and_float_are_schema_invalid.
var rejectionCases = []rejectionCase{
	// Duplicate keys, compared after unescaping.
	{"duplicate key (V0.50 test_cli)", `{"command":"x","command":"y"}`, kernel.CodeSchemaInvalid, "/command", ReasonDuplicateKey},
	{"duplicate key in an array element", `{"args":{"lines":[{"line_id":"L1"},{"line_id":"L2","line_id":"L3"}]}}`, kernel.CodeSchemaInvalid, "/args/lines/1/line_id", ReasonDuplicateKey},
	{"duplicate key spelled with an escape", `{"command":"x","\u0063ommand":"y"}`, kernel.CodeSchemaInvalid, "/command", ReasonDuplicateKey},
	{"duplicate key in a free-form map", `{"args":{"attributes":{"a":1,"a":2}}}`, kernel.CodeSchemaInvalid, "/args/attributes/a", ReasonDuplicateKey},

	// null anywhere; an unknown value is omitted instead.
	{"null at top level", `null`, kernel.CodeSchemaInvalid, "/", ReasonNull},
	{"null member (V0.50 test_core customer_id)", `{"args":{"id":"r","expected_version":0,"customer_id":null}}`, kernel.CodeSchemaInvalid, "/args/customer_id", ReasonNull},
	{"null array element", `{"args":{"tags":["a",null]}}`, kernel.CodeSchemaInvalid, "/args/tags/1", ReasonNull},
	{"null inside a free-form value", `{"args":{"attributes":{"x":[null]}}}`, kernel.CodeSchemaInvalid, "/args/attributes/x/0", ReasonNull},

	// Numbers are integer literals within int64; decimals travel as strings.
	{"float 120.0 (V0.50 test_core quantity)", `{"args":{"lines":[{"line_id":"L1","quantity":120.0}]}}`, kernel.CodeSchemaInvalid, "/args/lines/0/quantity", ReasonNotInteger},
	{"exponent 1e3", `{"args":{"expected_version":1e3}}`, kernel.CodeSchemaInvalid, "/args/expected_version", ReasonNotInteger},
	{"fraction -0.5", `{"args":{"expected_version":-0.5}}`, kernel.CodeSchemaInvalid, "/args/expected_version", ReasonNotInteger},
	{"exponent 1E+2", `{"args":{"expected_version":1E+2}}`, kernel.CodeSchemaInvalid, "/args/expected_version", ReasonNotInteger},
	{"exponent 0e0", `{"args":{"expected_version":0e0}}`, kernel.CodeSchemaInvalid, "/args/expected_version", ReasonNotInteger},
	{"beyond int64", `{"args":{"expected_version":9223372036854775808}}`, kernel.CodeSchemaInvalid, "/args/expected_version", ReasonNotInteger},
	{"below int64", `{"args":{"expected_version":-9223372036854775809}}`, kernel.CodeSchemaInvalid, "/args/expected_version", ReasonNotInteger},
	{"float in a free-form value", `{"args":{"attributes":{"n":1.5}}}`, kernel.CodeSchemaInvalid, "/args/attributes/n", ReasonNotInteger},
	{"NaN", `{"args":{"expected_version":NaN}}`, kernel.CodeSchemaInvalid, "/args/expected_version", ReasonInvalidJSON},
	{"Infinity", `{"args":{"expected_version":Infinity}}`, kernel.CodeSchemaInvalid, "/args/expected_version", ReasonInvalidJSON},
	{"-Infinity", `{"args":{"expected_version":-Infinity}}`, kernel.CodeSchemaInvalid, "/args/expected_version", ReasonInvalidJSON},
	{"leading zero", `{"args":{"expected_version":01}}`, kernel.CodeSchemaInvalid, "/args", ReasonInvalidJSON}, // 0, then a stray 1

	// Invalid UTF-8, which encoding/json would replace silently.
	{"invalid UTF-8 in a string", "{\"args\":{\"id\":\"r\xff\"}}", kernel.CodeSchemaInvalid, "/args/id", ReasonInvalidUTF8},
	{"invalid UTF-8 in a key", "{\"args\":{\"i\xffd\":\"r\"}}", kernel.CodeSchemaInvalid, "/args", ReasonInvalidUTF8},
	{"invalid UTF-8 outside strings", "{\"args\":\xff}", kernel.CodeSchemaInvalid, "/args", ReasonInvalidUTF8},
	{"overlong encoding", "{\"command\":\"\xc0\xaf\"}", kernel.CodeSchemaInvalid, "/command", ReasonInvalidUTF8},
	{"encoded surrogate", "{\"command\":\"\xed\xa0\x80\"}", kernel.CodeSchemaInvalid, "/command", ReasonInvalidUTF8},
	{"truncated sequence", "{\"command\":\"\xe2\x82\"}", kernel.CodeSchemaInvalid, "/command", ReasonInvalidUTF8},
	{"lone high surrogate escape", `{"command":"\ud800"}`, kernel.CodeSchemaInvalid, "/command", ReasonInvalidUTF8},
	{"lone low surrogate escape", `{"command":"\udc00x"}`, kernel.CodeSchemaInvalid, "/command", ReasonInvalidUTF8},
	{"high surrogate before a non-surrogate", `{"command":"\ud800\u0041"}`, kernel.CodeSchemaInvalid, "/command", ReasonInvalidUTF8},
	{"lone surrogate escape in a key", `{"args":{"attributes":{"\udfff":1}}}`, kernel.CodeSchemaInvalid, "/args/attributes", ReasonInvalidUTF8},

	// Exactly one top-level object.
	{"trailing object", `{}{}`, kernel.CodeSchemaInvalid, "/", ReasonTrailingData},
	{"trailing garbage", `{"command":"x"} x`, kernel.CodeSchemaInvalid, "/", ReasonTrailingData},
	{"trailing bracket", `{}]`, kernel.CodeSchemaInvalid, "/", ReasonTrailingData},
	{"top-level array", `[]`, kernel.CodeSchemaInvalid, "/", ReasonWrongType},
	{"top-level string", `"x"`, kernel.CodeSchemaInvalid, "/", ReasonWrongType},
	{"top-level number", `1`, kernel.CodeSchemaInvalid, "/", ReasonWrongType},
	{"empty body", ``, kernel.CodeSchemaInvalid, "/", ReasonInvalidJSON},
	{"whitespace only", " \n\t ", kernel.CodeSchemaInvalid, "/", ReasonInvalidJSON},
	{"byte order mark", "\ufeff{}", kernel.CodeSchemaInvalid, "/", ReasonInvalidJSON},
	{"trailing comma", `{"command":"x",}`, kernel.CodeSchemaInvalid, "/", ReasonInvalidJSON},
	{"missing colon", `{"command" "x"}`, kernel.CodeSchemaInvalid, "/command", ReasonInvalidJSON},
	{"truncated", `{"args":{"lines":[`, kernel.CodeSchemaInvalid, "/args/lines", ReasonInvalidJSON},

	// Closed structure: unknown fields, matched exactly rather than case-insensitively.
	{"unknown field status (V0.50 test_core)", `{"args":{"id":"r","expected_version":0,"status":"ready"}}`, kernel.CodeSchemaInvalid, "/args/status", ReasonUnknownField},
	{"scope field tenant_id (V0.50 test_core)", `{"args":{"id":"r","tenant_id":"other"}}`, kernel.CodeSchemaInvalid, "/args/tenant_id", ReasonUnknownField},
	{"unknown field at top level", `{"actor":"someone"}`, kernel.CodeSchemaInvalid, "/actor", ReasonUnknownField},
	{"unknown field in an array element", `{"args":{"lines":[{"line_id":"L1","price":"1"}]}}`, kernel.CodeSchemaInvalid, "/args/lines/0/price", ReasonUnknownField},
	{"key differing in case", `{"Command":"x"}`, kernel.CodeSchemaInvalid, "/Command", ReasonUnknownField},
	{"case variant after the exact key", `{"command":"x","COMMAND":"y"}`, kernel.CodeSchemaInvalid, "/COMMAND", ReasonUnknownField},
	{"Kelvin sign folding to k", `{"idempotency_\u212aey":"k1"}`, kernel.CodeSchemaInvalid, "/idempotency_\u212aey", ReasonUnknownField},

	// Pointer escaping (RFC 6901).
	{"keys with / and ~", `{"args":{"attributes":{"a/b":{"x~y":null}}}}`, kernel.CodeSchemaInvalid, "/args/attributes/a~1b/x~0y", ReasonNull},
	{"unknown key with / and ~", `{"a/b~c":1}`, kernel.CodeSchemaInvalid, "/a~1b~0c", ReasonUnknownField},
	{"unknown key that looks escaped", `{"~1":1}`, kernel.CodeSchemaInvalid, "/~01", ReasonUnknownField},

	// Types.
	{"number where a string is expected", `{"args":{"lines":[{"line_id":"L1","quantity":120}]}}`, kernel.CodeSchemaInvalid, "/args/lines/0/quantity", ReasonWrongType},
	{"string where an integer is expected", `{"args":{"expected_version":"1"}}`, kernel.CodeSchemaInvalid, "/args/expected_version", ReasonWrongType},
	{"boolean where a string is expected", `{"command":true}`, kernel.CodeSchemaInvalid, "/command", ReasonWrongType},
	{"string where a boolean is expected", `{"args":{"urgent":"true"}}`, kernel.CodeSchemaInvalid, "/args/urgent", ReasonWrongType},
	{"object where an array is expected", `{"args":{"lines":{}}}`, kernel.CodeSchemaInvalid, "/args/lines", ReasonWrongType},
	{"array where an object is expected", `{"args":[]}`, kernel.CodeSchemaInvalid, "/args", ReasonWrongType},
	{"object where a string is expected", `{"args":{"lines":[{"line_id":{}}]}}`, kernel.CodeSchemaInvalid, "/args/lines/0/line_id", ReasonWrongType},
	{"int32 overflow", `{"args":{"priority":2147483648}}`, kernel.CodeSchemaInvalid, "/args/priority", ReasonWrongType},
	{"int32 underflow", `{"args":{"priority":-2147483649}}`, kernel.CodeSchemaInvalid, "/args/priority", ReasonWrongType},
	{"negative unsigned", `{"args":{"count":-1}}`, kernel.CodeSchemaInvalid, "/args/count", ReasonWrongType},
	{"uint16 overflow", `{"args":{"count":65536}}`, kernel.CodeSchemaInvalid, "/args/count", ReasonWrongType},
	{"fixed array overflow", `{"args":{"pair":[1,2,3]}}`, kernel.CodeSchemaInvalid, "/args/pair/2", ReasonWrongType},
}

// reasons is every reason a rejection may carry: fixed phrases that never echo the request.
var reasons = []string{
	ReasonInvalidJSON, ReasonInvalidUTF8, ReasonDuplicateKey, ReasonNull, ReasonNotInteger, ReasonUnknownField,
	ReasonWrongType, ReasonTooDeep, ReasonTooManyValues, ReasonTrailingData, kernel.CodeRequestTooLarge,
}

func TestDecodeRejects(t *testing.T) {
	for _, tc := range rejectionCases {
		t.Run(tc.name, func(t *testing.T) {
			var got request
			err := Decode(strings.NewReader(tc.body), &got, Limits{})
			wantRejection(t, err, tc.code, tc.path, tc.reason)
		})
	}
}

func TestDecodeAcceptsAClosedRequest(t *testing.T) {
	body := `
	{
		"contract": "brant.trade-core/0.1.0",
		"command": "rfq.save_draft",
		"idempotency_key": "cli.1",
		"args": {
			"id": "r",
			"expected_version": 9223372036854775807,
			"customer_id": "客户\ud83d\ude00",
			"lines": [{"line_id": "L1", "quantity": "120.000000"}],
			"priority": -2147483648,
			"count": 65535,
			"urgent": false,
			"pair": [-9223372036854775808],
			"attributes": {"n": -0, "big": 9223372036854775807, "nested": [{"a/b": "x"}, true, "s"]}
		}
	}`
	var got request
	if err := Decode(strings.NewReader(body), &got, Limits{}); err != nil {
		t.Fatal(err)
	}
	customer, quantity, priority, count, urgent := "客户😀", "120.000000", int32(-2147483648), uint16(65535), false
	want := request{
		Contract:       "brant.trade-core/0.1.0",
		Command:        "rfq.save_draft",
		IdempotencyKey: "cli.1",
		Args: &args{
			ID:              "r",
			ExpectedVersion: 9223372036854775807,
			CustomerID:      &customer,
			Lines:           &[]line{{LineID: "L1", Quantity: &quantity}},
			Priority:        &priority,
			Count:           &count,
			Urgent:          &urgent,
			Pair:            &[2]int64{-9223372036854775808, 0},
			Attributes: &map[string]any{
				"n":      json.Number("-0"),
				"big":    json.Number("9223372036854775807"),
				"nested": []any{map[string]any{"a/b": "x"}, true, "s"},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %#v, want %#v", got, want)
	}
}

func TestDecodeKeepsPresence(t *testing.T) {
	type presence struct {
		Lines   *[]line   `json:"lines,omitempty"`
		Tags    []string  `json:"tags,omitempty"`
		Note    *string   `json:"note,omitempty"`
		Version *int64    `json:"version,omitempty"`
		Labels  *[]string `json:"labels,omitempty"`
	}
	var omitted presence
	if err := Decode(strings.NewReader(`{}`), &omitted, Limits{}); err != nil {
		t.Fatal(err)
	}
	if omitted.Lines != nil || omitted.Tags != nil || omitted.Note != nil || omitted.Version != nil || omitted.Labels != nil {
		t.Fatalf("omitted members decoded to %+v, want all nil", omitted)
	}
	var empty presence
	if err := Decode(strings.NewReader(`{"lines":[],"tags":[],"note":"","version":0,"labels":[]}`), &empty, Limits{}); err != nil {
		t.Fatal(err)
	}
	switch {
	case empty.Lines == nil || *empty.Lines == nil || len(*empty.Lines) != 0:
		t.Fatalf("[] decoded to %#v, want a pointer to an empty non-nil slice", empty.Lines)
	case empty.Tags == nil || len(empty.Tags) != 0:
		t.Fatalf("[] decoded to %#v, want an empty non-nil slice", empty.Tags)
	case empty.Labels == nil || *empty.Labels == nil:
		t.Fatalf("[] decoded to %#v, want a pointer to an empty non-nil slice", empty.Labels)
	case empty.Note == nil || *empty.Note != "" || empty.Version == nil || *empty.Version != 0:
		t.Fatalf("present zero values decoded to %+v", empty)
	}
}

func TestDecodeNestingLimit(t *testing.T) {
	// n nested objects: {"a":{"a":...{}}}, the top-level object being level 1.
	objects := func(n int) string {
		return strings.Repeat(`{"a":`, n-1) + "{}" + strings.Repeat("}", n-1)
	}
	// The top-level object holding n-1 nested arrays: {"a":[[...]]}.
	arrays := func(n int) string {
		return `{"a":` + strings.Repeat("[", n-1) + strings.Repeat("]", n-1) + "}"
	}
	var v map[string]any
	if err := Decode(strings.NewReader(objects(DefaultMaxDepth)), &v, Limits{}); err != nil {
		t.Fatalf("64 levels of objects: %v", err)
	}
	if err := Decode(strings.NewReader(arrays(DefaultMaxDepth)), &v, Limits{}); err != nil {
		t.Fatalf("64 levels of arrays: %v", err)
	}
	wantRejection(t, Decode(strings.NewReader(objects(DefaultMaxDepth+1)), &v, Limits{}),
		kernel.CodeSchemaInvalid, strings.Repeat("/a", DefaultMaxDepth), ReasonTooDeep)
	wantRejection(t, Decode(strings.NewReader(arrays(DefaultMaxDepth+1)), &v, Limits{}),
		kernel.CodeSchemaInvalid, "/a"+strings.Repeat("/0", DefaultMaxDepth-1), ReasonTooDeep)
	// A custom limit.
	if err := Decode(strings.NewReader(`{"a":{}}`), &v, Limits{MaxDepth: 2}); err != nil {
		t.Fatal(err)
	}
	wantRejection(t, Decode(strings.NewReader(`{"a":{"b":{}}}`), &v, Limits{MaxDepth: 2}), kernel.CodeSchemaInvalid, "/a/b", ReasonTooDeep)
}

func TestDecodeValueLimit(t *testing.T) {
	// The top-level object, the array and its elements: n+2 values.
	withElements := func(n int) string {
		return `{"a":[` + strings.TrimSuffix(strings.Repeat("0,", n), ",") + `]}`
	}
	var ok map[string]any
	if err := Decode(strings.NewReader(withElements(DefaultMaxValues-2)), &ok, Limits{}); err != nil {
		t.Fatalf("100,000 values: %v", err)
	}
	if got := len(ok["a"].([]any)); got != DefaultMaxValues-2 {
		t.Fatalf("decoded %d elements", got)
	}
	var tooMany map[string]any
	wantRejection(t, Decode(strings.NewReader(withElements(DefaultMaxValues-1)), &tooMany, Limits{}), kernel.CodeSchemaInvalid, "/", ReasonTooManyValues)
	// Keys are not values; a custom limit.
	if err := Decode(strings.NewReader(`{"a":[1],"b":"x"}`), &ok, Limits{MaxValues: 4}); err != nil {
		t.Fatalf("4 values: %v", err)
	}
	wantRejection(t, Decode(strings.NewReader(`{"a":[1,2],"b":"x"}`), &tooMany, Limits{MaxValues: 4}), kernel.CodeSchemaInvalid, "/", ReasonTooManyValues)
}

func TestDecodeSizeLimit(t *testing.T) {
	// A valid request padded with spaces to n bytes.
	padded := func(n int) string {
		body := `{"command":"rfq.save_draft"}`
		return body + strings.Repeat(" ", n-len(body))
	}
	var got request
	if err := Decode(strings.NewReader(padded(DefaultMaxBytes)), &got, Limits{}); err != nil || got.Command != "rfq.save_draft" {
		t.Fatalf("exactly 1 MiB: %v, %+v", err, got)
	}
	for name, body := range map[string]string{
		"1 MiB + 1 spaces (V0.50 test_cli)": strings.Repeat(" ", DefaultMaxBytes+1),
		"1 MiB + 1 valid request":           padded(DefaultMaxBytes + 1),
		"far beyond":                        padded(4 * DefaultMaxBytes),
	} {
		t.Run(name, func(t *testing.T) {
			wantRejection(t, Decode(strings.NewReader(body), &got, Limits{}), kernel.CodeRequestTooLarge, "/", kernel.CodeRequestTooLarge)
			wantRejection(t, Unmarshal([]byte(body), &got, Limits{}), kernel.CodeRequestTooLarge, "/", kernel.CodeRequestTooLarge)
		})
	}
	// Exactly 1 MiB of spaces is within the limit and simply not JSON.
	wantRejection(t, Decode(strings.NewReader(strings.Repeat(" ", DefaultMaxBytes)), &got, Limits{}), kernel.CodeSchemaInvalid, "/", ReasonInvalidJSON)
	// A custom limit.
	wantRejection(t, Decode(strings.NewReader(`{"command":"x"}`), &got, Limits{MaxBytes: 14}), kernel.CodeRequestTooLarge, "/", kernel.CodeRequestTooLarge)
	if err := Decode(strings.NewReader(`{"command":"x"}`), &got, Limits{MaxBytes: 15}); err != nil {
		t.Fatal(err)
	}
}

// endless is an infinite stream of spaces that counts what is read from it.
type endless struct{ read int64 }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	e.read += int64(len(p))
	return len(p), nil
}

func TestDecodeReadsAtMostOneByteBeyondTheLimit(t *testing.T) {
	src := &endless{}
	var got request
	wantRejection(t, Decode(src, &got, Limits{MaxBytes: 1000}), kernel.CodeRequestTooLarge, "/", kernel.CodeRequestTooLarge)
	if src.read > 1001 {
		t.Fatalf("read %d bytes for a limit of 1000", src.read)
	}
}

func TestDecodeReadFailures(t *testing.T) {
	var got request
	// A transport that also caps the body with http.MaxBytesReader gets the same verdict.
	capped := http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(strings.NewReader(`{"command":"x"}`)), 5)
	wantRejection(t, Decode(capped, &got, Limits{}), kernel.CodeRequestTooLarge, "/", kernel.CodeRequestTooLarge)

	broken := errors.New("connection reset")
	err := Decode(io.MultiReader(strings.NewReader(`{"comm`), iotestErrReader{broken}), &got, Limits{})
	if _, ok := kernel.AsRejection(err); ok || !errors.Is(err, broken) {
		t.Fatalf("got %v, want the read error and no rejection", err)
	}
}

type iotestErrReader struct{ err error }

func (r iotestErrReader) Read([]byte) (int, error) { return 0, r.err }

func TestDecodeNeedsANonNilPointer(t *testing.T) {
	var got request
	var nilPointer *request
	for _, target := range []any{got, nilPointer, nil} {
		err := Decode(strings.NewReader(`{}`), target, Limits{})
		if _, ok := kernel.AsRejection(err); err == nil || ok {
			t.Fatalf("target %#v: got %v, want a programming error", target, err)
		}
	}
}

// Field names resolve as encoding/json resolves them, but match exactly. Shared is ambiguous at
// depth 1, Plain is claimed by a tag over an untagged field at the same depth, and Deep at depth 0
// hides the one at depth 1.
type Embedded struct {
	Shared string
	Inner  string `json:"inner"`
	Plain  string
}

type Other struct {
	Shared string
	Third  string `json:"third"`
	Win    string `json:"Plain"`
	Deep   string
}

type tagged struct {
	Shared string `json:"shared"`
}

type layout struct {
	Embedded
	Other
	Deep     int64
	Named    tagged `json:"named"`
	Ignored  string `json:"-"`
	Quoted   int64  `json:"quoted,string"`
	unexport string
}

// jsonNames lists the member names encoding/json writes for struct value v, which are exactly the
// names it reads.
func jsonNames(t *testing.T, v any) []string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &members); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func resolvedNames(typ reflect.Type) []string {
	names := make([]string, 0)
	for name := range fieldsOf(typ) {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func TestDecodeResolvesFieldsLikeEncodingJSON(t *testing.T) {
	if got, want := resolvedNames(reflect.TypeFor[layout]()), jsonNames(t, layout{Quoted: 1, unexport: "hidden"}); !slices.Equal(got, want) {
		t.Fatalf("resolved %v, encoding/json uses %v", got, want)
	}
	var v layout
	body := `{"inner":"i","third":"t","Plain":"p","Deep":7,"quoted":"42","named":{"shared":"s"}}`
	if err := Decode(strings.NewReader(body), &v, Limits{}); err != nil {
		t.Fatal(err)
	}
	if v.Inner != "i" || v.Third != "t" || v.Win != "p" || v.Plain != "" || v.Deep != 7 || v.Quoted != 42 || v.Named.Shared != "s" {
		t.Fatalf("decoded %+v", v)
	}
	for body, path := range map[string]string{
		`{"Shared":"ambiguous"}`: "/Shared",
		`{"Ignored":"x"}`:        "/Ignored",
		`{"unexport":"x"}`:       "/unexport",
		`{"Embedded":{}}`:        "/Embedded",
		`{"plain":"p"}`:          "/plain",
		`{"Inner":"i"}`:          "/Inner",
	} {
		wantRejection(t, Decode(strings.NewReader(body), &v, Limits{}), kernel.CodeSchemaInvalid, path, ReasonUnknownField)
	}
	wantRejection(t, Decode(strings.NewReader(`{"Deep":"x"}`), &v, Limits{}), kernel.CodeSchemaInvalid, "/Deep", ReasonWrongType)
	wantRejection(t, Decode(strings.NewReader(`{"quoted":42}`), &v, Limits{}), kernel.CodeSchemaInvalid, "/quoted", ReasonWrongType)
}

func TestDecodeResolvesUnusualTagsLikeEncodingJSON(t *testing.T) {
	// Built at run time: linters rightly flag such tags in source.
	str := reflect.TypeFor[string]()
	typ := reflect.StructOf([]reflect.StructField{
		{Name: "Dash", Type: str, Tag: `json:"-,"`},     // named "-"
		{Name: "BadTag", Type: str, Tag: `json:"a\"b"`}, // an invalid name falls back to the field name
		{Name: "Spaced", Type: str, Tag: `json:"a b"`},  // spaces are allowed
	})
	if got, want := resolvedNames(typ), jsonNames(t, reflect.New(typ).Elem().Interface()); !slices.Equal(got, want) {
		t.Fatalf("resolved %v, encoding/json uses %v", got, want)
	}
	v := reflect.New(typ)
	if err := Decode(strings.NewReader(`{"-":"d","BadTag":"b","a b":"s"}`), v.Interface(), Limits{}); err != nil {
		t.Fatal(err)
	}
	if got := v.Elem(); got.Field(0).String() != "d" || got.Field(1).String() != "b" || got.Field(2).String() != "s" {
		t.Fatalf("decoded %+v", got.Interface())
	}
	wantRejection(t, Decode(strings.NewReader(`{"a\"b":"x"}`), v.Interface(), Limits{}), kernel.CodeSchemaInvalid, `/a"b`, ReasonUnknownField)
}

// textCode decodes from a JSON string only, as encoding/json requires of a TextUnmarshaler.
type textCode string

func (c *textCode) UnmarshalText(b []byte) error {
	*c = textCode(strings.ToUpper(string(b)))
	return nil
}

// custom decides its own JSON form.
type custom struct{ raw string }

func (c *custom) UnmarshalJSON(b []byte) error {
	switch {
	case bytes.Equal(b, []byte(`"reject"`)):
		return kernel.Reject(kernel.CodeSchemaInvalid, "/custom", ReasonWrongType)
	case bytes.Equal(b, []byte(`"fail"`)):
		return errors.New("custom failure")
	}
	c.raw = string(b)
	return nil
}

func TestDecodeHonoursCustomUnmarshalers(t *testing.T) {
	type target struct {
		Code   textCode `json:"code"`
		Custom *custom  `json:"custom,omitempty"`
	}
	var v target
	if err := Decode(strings.NewReader(`{"code":"ab","custom":{"any":[1,"x"]}}`), &v, Limits{}); err != nil {
		t.Fatal(err)
	}
	if v.Code != "AB" || v.Custom.raw != `{"any":[1,"x"]}` {
		t.Fatalf("decoded %+v", v)
	}
	wantRejection(t, Decode(strings.NewReader(`{"code":1}`), &v, Limits{}), kernel.CodeSchemaInvalid, "/code", ReasonWrongType)
	wantRejection(t, Decode(strings.NewReader(`{"code":{}}`), &v, Limits{}), kernel.CodeSchemaInvalid, "/code", ReasonWrongType)
	// The structural rules hold inside a custom value too.
	wantRejection(t, Decode(strings.NewReader(`{"custom":{"a":1,"a":2}}`), &v, Limits{}), kernel.CodeSchemaInvalid, "/custom/a", ReasonDuplicateKey)
	wantRejection(t, Decode(strings.NewReader(`{"custom":[1.5]}`), &v, Limits{}), kernel.CodeSchemaInvalid, "/custom/0", ReasonNotInteger)
	// A rejection from the unmarshaler passes through; any other failure is a wrong type at the root.
	wantRejection(t, Decode(strings.NewReader(`{"custom":"reject"}`), &v, Limits{}), kernel.CodeSchemaInvalid, "/custom", ReasonWrongType)
	wantRejection(t, Decode(strings.NewReader(`{"custom":"fail"}`), &v, Limits{}), kernel.CodeSchemaInvalid, "/", ReasonWrongType)
}

func TestDecodeIntoInterface(t *testing.T) {
	var v any
	if err := Decode(strings.NewReader(`{"n":12,"s":"x","a":[true]}`), &v, Limits{}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"n": json.Number("12"), "s": "x", "a": []any{true}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("decoded %#v, want %#v", v, want)
	}
}

func TestPointerEscaping(t *testing.T) {
	s := &scanner{path: []segment{{key: "a/b", isKey: true}, {index: 3}, {key: "~1/~0", isKey: true}, {key: "", isKey: true}}}
	if got, want := s.pointer(), "/a~1b/3/~01~1~00/"; got != want {
		t.Fatalf("pointer %q, want %q", got, want)
	}
}

func wantRejection(t *testing.T, err error, code, path, reason string) {
	t.Helper()
	r, ok := kernel.AsRejection(err)
	if !ok {
		t.Fatalf("got %v, want rejection %s at %s (%s)", err, code, path, reason)
	}
	if len(r.Fields) != 1 || r.Code != code || r.Fields[0].Path != path || r.Fields[0].Reason != reason {
		t.Fatalf("got %s %+v, want %s at %s (%s)", r.Code, r.Fields, code, path, reason)
	}
	if !slices.Contains(reasons, r.Fields[0].Reason) {
		t.Fatalf("reason %q is not a fixed phrase", r.Fields[0].Reason)
	}
}
