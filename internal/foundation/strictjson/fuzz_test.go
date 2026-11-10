package strictjson

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// FuzzDecode asserts that Decode never panics, refuses only with a rejection carrying a fixed
// reason, accepts nothing encoding/json considers invalid, and that whatever it accepts encodes
// back to JSON it accepts again as the same value.
func FuzzDecode(f *testing.F) {
	for _, tc := range rejectionCases {
		f.Add([]byte(tc.body))
	}
	for _, body := range []string{
		`{}`,
		`{"contract":"brant.trade-core/0.1.0","command":"rfq.save_draft","idempotency_key":"k","args":{"id":"r","expected_version":3}}`,
		`{"args":{"lines":[{"line_id":"L1","quantity":"1.5"},{"line_id":"L2"}],"tags":[],"pair":[1]}}`,
		`{"args":{"attributes":{"a":[1,"x",true,{"b":{}}],"a/b~c":-0,"<&>":"\"\\"}}}`,
		`{"args":{"priority":-2147483648,"count":65535,"urgent":true,"customer_id":""}}`,
	} {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		roundTrip[map[string]any](t, data, Limits{})
		roundTrip[request](t, data, Limits{})
		roundTrip[map[string]any](t, data, Limits{MaxBytes: 256, MaxDepth: 4, MaxValues: 16})
	})
}

func roundTrip[T any](t *testing.T, data []byte, limits Limits) {
	t.Helper()
	var v T
	err := Decode(bytes.NewReader(data), &v, limits)
	if err != nil {
		r, ok := kernel.AsRejection(err)
		switch {
		case !ok:
			t.Fatalf("%q: error %v is not a rejection", data, err)
		case r.Code != kernel.CodeSchemaInvalid && r.Code != kernel.CodeRequestTooLarge:
			t.Fatalf("%q: code %s", data, r.Code)
		case len(r.Fields) != 1 || !strings.HasPrefix(r.Fields[0].Path, "/") || !slices.Contains(reasons, r.Fields[0].Reason):
			t.Fatalf("%q: fields %+v", data, r.Fields)
		}
		return
	}
	if !json.Valid(data) || !utf8.Valid(data) {
		t.Fatalf("%q: accepted, but it is not valid JSON in UTF-8", data)
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%q: decoded %#v does not encode: %v", data, v, err)
	}
	// Escaping may lengthen the text and a typed target adds its required members.
	again := Limits{MaxBytes: 64 << 20, MaxDepth: limits.MaxDepth, MaxValues: limits.withDefaults().MaxValues + 16}
	var decoded T
	if err := Decode(bytes.NewReader(encoded), &decoded, again); err != nil {
		t.Fatalf("%q: re-encoded as %q, rejected: %v", data, encoded, err)
	}
	if !reflect.DeepEqual(v, decoded) {
		t.Fatalf("%q: decoded %#v, after a round trip %#v", data, v, decoded)
	}
}
