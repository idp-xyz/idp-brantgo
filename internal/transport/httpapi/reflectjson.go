package httpapi

import (
	"reflect"
	"strings"
	"sync"
	"unicode"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
)

// POL's configuration data are plain api structures without JSON tags. The transport maps them to
// JSON with snake_case member names derived from the Go field names (LegalName → legal_name,
// MaxFXAgeSeconds → max_fx_age_seconds), and a ConfigRef as {"kind","id","revision"}, so a new
// kind needs no hand-written mapping and the OpenAPI document follows it.

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
	var out reflect.Type
	switch t.Kind() {
	case reflect.Pointer:
		out = reflect.PointerTo(taggedType(t.Elem()))
	case reflect.Slice:
		out = reflect.SliceOf(taggedType(t.Elem()))
	case reflect.Struct:
		fields := make([]reflect.StructField, 0, t.NumField())
		for f := range t.Fields() {
			name := snake(f.Name)
			if t == reflect.TypeFor[polapi.ConfigRef]() && f.Name == "ConfigID" {
				name = "id"
			}
			tag := `json:"` + name + `"`
			switch f.Type.Kind() {
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
	taggedTypes.Store(t, out)
	return out
}

// copyValue copies src into dst field by field; the two types differ only in struct tags.
func copyValue(dst, src reflect.Value) {
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
