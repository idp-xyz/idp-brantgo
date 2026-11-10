package httpapi

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
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
			if prevLower || (unicode.IsUpper(r[i-1]) && nextLower) {
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
			if t == reflect.TypeFor[polapi.ConfigRef]() && f.Name == "ConfigID" {
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
