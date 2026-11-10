// Package strictjson decodes inbound JSON requests as closed structures (engineering contract C7)
// within the ingress resource limits: a request of at most 1 MiB, nested at most 64 levels deep
// and holding at most 100,000 values. It rejects duplicate keys, null, numbers that are not integer
// literals within int64, unknown fields, invalid UTF-8 and trailing data; the top-level value is
// an object. Business decimals travel as strings and are parsed by kernel.ParseDecimal.
//
// Every refusal is a *kernel.Rejection: REQUEST_TOO_LARGE at "/" for a body over the byte limit,
// SCHEMA_INVALID otherwise, at the JSON Pointer of the offending member when it is known. Reasons
// are fixed phrases and never echo request content.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The ingress resource limits of the engineering contract.
const (
	DefaultMaxBytes  = 1 << 20 // raw request: 1 MiB
	DefaultMaxDepth  = 64      // nested objects and arrays, the top-level object counting as 1
	DefaultMaxValues = 100_000 // objects, arrays, strings, numbers and booleans; keys do not count
)

// Limits bounds one request. A zero or negative field takes its default.
type Limits struct {
	MaxBytes  int64
	MaxDepth  int
	MaxValues int
}

func (l Limits) withDefaults() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultMaxBytes
	}
	l.MaxBytes = min(l.MaxBytes, math.MaxInt64-1) // room for the byte that detects an excess
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultMaxDepth
	}
	if l.MaxValues <= 0 {
		l.MaxValues = DefaultMaxValues
	}
	return l
}

// Reasons of SCHEMA_INVALID rejections.
const (
	ReasonInvalidJSON   = "invalid JSON"
	ReasonInvalidUTF8   = "invalid UTF-8"
	ReasonDuplicateKey  = "duplicate key"
	ReasonNull          = "null"
	ReasonNotInteger    = "not an integer"
	ReasonUnknownField  = "unknown field"
	ReasonWrongType     = "wrong type"
	ReasonTooDeep       = "too deep"
	ReasonTooManyValues = "too many values"
	ReasonTrailingData  = "trailing data"
)

// Decode reads one request of at most limits.MaxBytes from r and decodes it into v, a non-nil
// pointer, as Unmarshal does. It reads at most one byte beyond the limit; an *http.MaxBytesError
// from a reader the transport capped itself is REQUEST_TOO_LARGE as well. An error that is not a
// *kernel.Rejection is a read failure or a programming error, never a verdict on the request.
func Decode(r io.Reader, v any, limits Limits) error {
	limits = limits.withDefaults()
	data, err := io.ReadAll(io.LimitReader(r, limits.MaxBytes+1))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return tooLarge()
		}
		return fmt.Errorf("strictjson: read request: %w", err)
	}
	return Unmarshal(data, v, limits)
}

// Unmarshal checks data against the limits and the closed-structure rules, then decodes it into
// v, a non-nil pointer, with unknown fields disallowed and numbers kept exact (an interface value
// receives a json.Number). Object keys must match field names exactly, not case-insensitively as
// encoding/json would otherwise allow. Because null never reaches v, a pointer, slice or map
// field stays nil exactly when its member is absent.
func Unmarshal(data []byte, v any, limits Limits) error {
	limits = limits.withDefaults()
	if int64(len(data)) > limits.MaxBytes {
		return tooLarge()
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("strictjson: decode target %T is not a non-nil pointer", v)
	}
	if err := scan(data, rv.Type().Elem(), limits); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		// The scan mirrors encoding/json's field resolution and type rules, so this is reached only
		// through a custom unmarshaler, a map key type or an exotic struct layout.
		if r, ok := kernel.AsRejection(err); ok {
			return r
		}
		reason := ReasonWrongType
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			reason = ReasonUnknownField
		}
		return kernel.Reject(kernel.CodeSchemaInvalid, "/", reason)
	}
	return nil
}

func tooLarge() error { return kernel.Reject(kernel.CodeRequestTooLarge, "/", "") }

// scanner walks the token stream once, alongside the Go type each value decodes into, and stops at
// the first violation in document order.
type scanner struct {
	data   []byte
	dec    *json.Decoder
	limits Limits
	values int
	bad    int       // offset of the first invalid UTF-8 byte, or -1
	path   []segment // reference tokens from the root to the current member
}

type segment struct {
	key   string
	index int // used when isKey is false
	isKey bool
}

func scan(data []byte, t reflect.Type, limits Limits) error {
	s := &scanner{data: data, dec: json.NewDecoder(bytes.NewReader(data)), limits: limits, bad: invalidUTF8(data)}
	s.dec.UseNumber()
	tok, err := s.token()
	if err != nil {
		return err
	}
	switch tok {
	case nil:
		return s.reject(ReasonNull)
	case json.Delim('{'):
	default:
		return s.reject(ReasonWrongType)
	}
	if err := s.value(tok, t, false, 1); err != nil {
		return err
	}
	if _, err := s.dec.Token(); !errors.Is(err, io.EOF) {
		if s.bad >= 0 {
			return s.reject(ReasonInvalidUTF8)
		}
		return s.reject(ReasonTrailingData)
	}
	return nil
}

// token returns the next token. Invalid UTF-8 is reported at the token that holds it, since
// encoding/json would silently replace it, and so is a string escape that encodes a lone UTF-16
// surrogate.
func (s *scanner) token() (json.Token, error) {
	start := s.dec.InputOffset()
	tok, err := s.dec.Token()
	if s.bad >= 0 && (err != nil || s.dec.InputOffset() > int64(s.bad)) {
		return nil, s.reject(ReasonInvalidUTF8)
	}
	if err != nil {
		return nil, s.reject(ReasonInvalidJSON)
	}
	if str, ok := tok.(string); ok && strings.ContainsRune(str, utf8.RuneError) &&
		loneSurrogate(s.data[start:s.dec.InputOffset()]) {
		return nil, s.reject(ReasonInvalidUTF8)
	}
	return tok, nil
}

// value checks the value that begins with tok at the current path and nesting depth against t,
// the type it decodes into; quoted marks a field with the ",string" option.
func (s *scanner) value(tok json.Token, t reflect.Type, quoted bool, depth int) error {
	s.values++
	if s.values > s.limits.MaxValues {
		return kernel.Reject(kernel.CodeSchemaInvalid, "/", ReasonTooManyValues)
	}
	t, text := resolve(t)
	if quoted {
		text = true
	}
	free := t == nil || t.Kind() == reflect.Interface // the type does not constrain the value
	switch tok := tok.(type) {
	case nil:
		return s.reject(ReasonNull)
	case json.Delim:
		if depth > s.limits.MaxDepth {
			return s.reject(ReasonTooDeep)
		}
		if tok == '{' {
			if !free && (text || t.Kind() != reflect.Struct && t.Kind() != reflect.Map) {
				return s.reject(ReasonWrongType)
			}
			return s.object(t, free, depth)
		}
		if !free && (text || t.Kind() != reflect.Slice && t.Kind() != reflect.Array) {
			return s.reject(ReasonWrongType)
		}
		return s.array(t, free, depth)
	case string:
		if !free && !text && (t.Kind() != reflect.String || t == numberType) && !isBytes(t) {
			return s.reject(ReasonWrongType)
		}
	case bool:
		if !free && (text || t.Kind() != reflect.Bool) {
			return s.reject(ReasonWrongType)
		}
	case json.Number:
		n, ok := integer(tok)
		if !ok {
			return s.reject(ReasonNotInteger)
		}
		if !free && (text || !fitsNumber(t, n)) {
			return s.reject(ReasonWrongType)
		}
	}
	return nil
}

func (s *scanner) object(t reflect.Type, free bool, depth int) error {
	var fields map[string]field // the accepted names when t is a struct
	var elem reflect.Type       // the value type when t is a map
	closed := !free && t.Kind() == reflect.Struct
	if closed {
		fields = fieldsOf(t)
	} else if !free {
		elem = t.Elem()
	}
	var seen map[string]struct{}
	for {
		tok, err := s.token()
		if err != nil {
			return err
		}
		if tok == json.Delim('}') {
			return nil
		}
		key, ok := tok.(string)
		if !ok { // the decoder returns only strings at a key position
			return s.reject(ReasonInvalidJSON)
		}
		s.path = append(s.path, segment{key: key, isKey: true})
		if _, dup := seen[key]; dup {
			return s.reject(ReasonDuplicateKey)
		}
		if seen == nil {
			seen = make(map[string]struct{})
		}
		seen[key] = struct{}{}
		ft, quoted := elem, false
		if closed {
			f, ok := fields[key]
			if !ok {
				return s.reject(ReasonUnknownField)
			}
			ft, quoted = f.typ, f.quoted
		}
		if tok, err = s.token(); err != nil {
			return err
		}
		if err := s.value(tok, ft, quoted, depth+1); err != nil {
			return err
		}
		s.path = s.path[:len(s.path)-1]
	}
}

func (s *scanner) array(t reflect.Type, free bool, depth int) error {
	var elem reflect.Type
	length := -1
	if !free {
		elem = t.Elem()
		if t.Kind() == reflect.Array {
			length = t.Len()
		}
	}
	for i := 0; ; i++ {
		tok, err := s.token()
		if err != nil {
			return err
		}
		if tok == json.Delim(']') {
			return nil
		}
		s.path = append(s.path, segment{index: i})
		if i == length { // encoding/json would drop the excess elements silently
			return s.reject(ReasonWrongType)
		}
		if err := s.value(tok, elem, false, depth+1); err != nil {
			return err
		}
		s.path = s.path[:len(s.path)-1]
	}
}

func (s *scanner) reject(reason string) error {
	return kernel.Reject(kernel.CodeSchemaInvalid, s.pointer(), reason)
}

var pointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")

// pointer returns the RFC 6901 JSON Pointer of the current member, "/" for the root.
func (s *scanner) pointer() string {
	if len(s.path) == 0 {
		return "/"
	}
	var b strings.Builder
	for _, seg := range s.path {
		b.WriteByte('/')
		if seg.isKey {
			b.WriteString(pointerEscaper.Replace(seg.key))
		} else {
			b.WriteString(strconv.Itoa(seg.index))
		}
	}
	return b.String()
}

// integer reports whether n is an integer literal within int64 and returns its value. The
// grammar already excludes leading zeros, NaN and Infinity.
func integer(n json.Number) (int64, bool) {
	if strings.ContainsAny(string(n), ".eE") {
		return 0, false
	}
	v, err := strconv.ParseInt(string(n), 10, 64)
	return v, err == nil
}

// fitsNumber reports whether the integer n decodes into t without encoding/json refusing it.
func fitsNumber(t reflect.Type, n int64) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		bits := t.Bits()
		return bits == 64 || n >= -1<<(bits-1) && n < 1<<(bits-1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		bits := t.Bits()
		return n >= 0 && (bits == 64 || n < 1<<bits)
	case reflect.Float32, reflect.Float64:
		return true
	case reflect.String:
		return t == numberType
	}
	return false
}

// invalidUTF8 returns the offset of the first byte of data that is not valid UTF-8, or -1.
func invalidUTF8(data []byte) int {
	if utf8.Valid(data) {
		return -1
	}
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return -1
}

// loneSurrogate reports whether the raw string token holds a \u escape of a UTF-16 surrogate
// that is not part of a valid pair.
func loneSurrogate(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' {
			continue // a one-character escape such as \\ or \"
		}
		r := hex4(raw[i+1:])
		i += 4
		if !utf16.IsSurrogate(r) {
			continue
		}
		if r < 0xDC00 && i+6 < len(raw) && raw[i+1] == '\\' && raw[i+2] == 'u' {
			if low := hex4(raw[i+3:]); low >= 0xDC00 && low <= 0xDFFF {
				i += 6
				continue
			}
		}
		return true
	}
	return false
}

// hex4 decodes four hexadecimal digits, or returns -1.
func hex4(b []byte) rune {
	if len(b) < 4 {
		return -1
	}
	v, err := strconv.ParseUint(string(b[:4]), 16, 16)
	if err != nil {
		return -1
	}
	return rune(v)
}
