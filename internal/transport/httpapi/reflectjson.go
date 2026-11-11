package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// POL's configuration data and the screening views are plain api structures without JSON tags. The
// transport maps them to JSON with snake_case member names derived from the Go field names
// (LegalName → legal_name, MaxFXAgeSeconds → max_fx_age_seconds), a ConfigRef as
// {"kind","id","revision"}, an embedded structure's members inline and an instant as the
// transport's UTC timestamp, so a new kind or view field needs no hand-written mapping and the
// OpenAPI document follows it.

var taggedTypes sync.Map // reflect.Type → reflect.Type

// snake converts a Go field name to its JSON member name.
func snake(name string) string {
	r := []rune(name)
	var b strings.Builder
	for i, c := range r {
		if unicode.IsUpper(c) && i > 0 {
			prevLower := unicode.IsLower(r[i-1]) || unicode.IsDigit(r[i-1])
			nextLower := i+1 < len(r) && unicode.IsLower(r[i+1])
			// A plural acronym keeps its s: EvidenceIDs is evidence_ids, not evidence_i_ds.
			plural := nextLower && r[i+1] == 's' && (i+2 == len(r) || unicode.IsUpper(r[i+2]))
			if prevLower || (unicode.IsUpper(r[i-1]) && nextLower && !plural) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(c))
	}
	return b.String()
}

// taggedType returns t with JSON tags on every struct field, recursively.
func taggedType(t reflect.Type) reflect.Type {
	if v, ok := taggedTypes.Load(t); ok {
		return v.(reflect.Type)
	}
	out := reflect.TypeFor[string]()
	if t != timeType {
		out = taggedKind(t)
	}
	taggedTypes.Store(t, out)
	return out
}

var timeType = reflect.TypeFor[time.Time]()

func taggedKind(t reflect.Type) reflect.Type {
	var out reflect.Type
	switch t.Kind() {
	case reflect.Pointer:
		out = reflect.PointerTo(taggedType(t.Elem()))
	case reflect.Slice:
		out = reflect.SliceOf(taggedType(t.Elem()))
	case reflect.Struct:
		fields := make([]reflect.StructField, 0, t.NumField())
		for f := range t.Fields() {
			if f.Anonymous {
				fields = append(fields, reflect.StructField{Name: f.Name, Type: taggedType(f.Type), Anonymous: true})
				continue
			}
			name := snake(f.Name)
			if explicit, _, _ := strings.Cut(f.Tag.Get("json"), ","); explicit != "" && explicit != "-" {
				// An api package that names a member explicitly (CST's RfqRef.RfqID is "id") wins.
				name = explicit
			} else if _, hasID := t.FieldByName("ID"); f.Name == "ConfigID" && !hasID {
				// A configuration reference is {"kind","id","revision"} wherever it appears.
				name = "id"
			}
			tag := `json:"` + name + `"`
			switch taggedType(f.Type).Kind() {
			case reflect.String, reflect.Pointer, reflect.Slice:
				// Strict JSON refuses empty strings and null, so an absent value stays absent and
				// a view reads back as a valid request. A zero number is a value (minor_units 0).
				tag = `json:"` + name + `,omitempty"`
			}
			fields = append(fields, reflect.StructField{Name: f.Name, Type: taggedType(f.Type), Tag: reflect.StructTag(tag)})
		}
		out = reflect.StructOf(fields)
	default:
		out = t
	}
	return out
}

// copyValue copies src into dst field by field; the two types differ only in struct tags.
func copyValue(dst, src reflect.Value) {
	if src.Type() == timeType {
		if t := src.Interface().(time.Time); !t.IsZero() {
			dst.SetString(timestamp(t))
		}
		return
	}
	switch src.Kind() {
	case reflect.Pointer:
		if src.IsNil() {
			return
		}
		dst.Set(reflect.New(dst.Type().Elem()))
		copyValue(dst.Elem(), src.Elem())
	case reflect.Slice:
		if src.IsNil() {
			return
		}
		dst.Set(reflect.MakeSlice(dst.Type(), src.Len(), src.Len()))
		for i := range src.Len() {
			copyValue(dst.Index(i), src.Index(i))
		}
	case reflect.Struct:
		for i := range src.NumField() {
			copyValue(dst.Field(i), src.Field(i))
		}
	default:
		dst.Set(src.Convert(dst.Type()))
	}
}

// wire returns v as its tagged copy, ready for encoding/json.
func wire(v any) any {
	src := reflect.ValueOf(v)
	dst := reflect.New(taggedType(src.Type())).Elem()
	copyValue(dst, src)
	return dst.Interface()
}

// okView is a read response: "ok": true and the members of v.
func okView[V any](v V) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(wire(v)); err != nil {
		panic("httpapi: encode view: " + err.Error()) // tagged copies of api structures always encode
	}
	body := bytes.TrimSpace(b.Bytes())
	if len(body) == 2 {
		return json.RawMessage(`{"ok":true}`)
	}
	return append([]byte(`{"ok":true,`), body[1:]...)
}

// configDataField returns the ConfigData field that holds kind, or false.
func configDataField(kind string) (reflect.StructField, bool) {
	for f := range reflect.TypeFor[polapi.ConfigData]().Fields() {
		if kindOf(f.Name) == kind {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// kindOf is the configuration kind a ConfigData field holds (FXRate → fx_rate).
func kindOf(field string) string { return snake(field) }

// reflected builds a commandFunc for an api command structure C whose members, apart from
// IdempotencyKey and Kind, are its arguments: the member named idField is "id", ExpectedVersion is
// the required "expected_version", the others follow the reflective mapping. kind, when not empty,
// fills C's Kind (SUP's revise, discard, seal and end commands serve three kinds).
func reflected[C any, R result](idField, kind string, call func(context.Context, C) (R, error)) commandFunc {
	ct := reflect.TypeFor[C]()
	var fields []reflect.StructField
	var source []int // index in C of each args field
	for f := range ct.Fields() {
		var sf reflect.StructField
		switch f.Name {
		case "IdempotencyKey", "Kind":
			continue
		case idField:
			sf = reflect.StructField{Name: f.Name, Type: f.Type, Tag: `json:"id"`}
		case "ExpectedVersion":
			sf = reflect.StructField{Name: f.Name, Type: reflect.TypeFor[*int64](), Tag: `json:"expected_version"`}
		default:
			tagged := taggedType(reflect.StructOf([]reflect.StructField{f}))
			sf = tagged.Field(0)
		}
		fields = append(fields, sf)
		source = append(source, f.Index[0])
	}
	at := reflect.StructOf(fields)
	return commandFunc{args: at, call: func(ctx context.Context, key string, args json.RawMessage, maxValues int) (receipt, error) {
		a := reflect.New(at)
		if err := decodeArgs(args, a.Interface(), maxValues); err != nil {
			return receipt{}, err
		}
		c := reflect.New(ct).Elem()
		c.FieldByName("IdempotencyKey").SetString(key)
		if kind != "" {
			c.FieldByName("Kind").SetString(kind)
		}
		for i, idx := range source {
			src, dst := a.Elem().Field(i), c.Field(idx)
			if fields[i].Name == "ExpectedVersion" {
				if src.IsNil() {
					return receipt{}, schemaError{kernel.Reject(kernel.CodeSchemaInvalid, "/args/expected_version", "required")}
				}
				dst.SetInt(src.Elem().Int())
				continue
			}
			copyValue(dst, src)
		}
		r, err := call(ctx, c.Interface().(C))
		if err != nil {
			return receipt{}, err
		}
		v := struct {
			ActionID   string
			ObjectID   string
			RowVersion int64
			Status     string
			Replayed   bool
		}(r)
		return receipt{ActionID: v.ActionID, ObjectID: v.ObjectID, RowVersion: v.RowVersion, Status: v.Status, Replayed: v.Replayed}, nil
	}}
}
