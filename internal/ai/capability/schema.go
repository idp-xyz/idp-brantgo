package capability

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

// Schema returns the JSON Schema (draft 2020-12 subset) of the arguments a model may send for c:
// the api command's exported fields under their Go names, which is how strictjson decodes them into
// the command, without the idempotency key, and with no additional properties at any level. It is
// what the model sees; the gateway still decodes strictly, and the context still validates.
func (c Capability) Schema() (json.RawMessage, error) {
	s, err := schemaOf(c.input, true, map[reflect.Type]bool{})
	if err != nil {
		return nil, fmt.Errorf("capability: schema of %s: %w", c.Command, err)
	}
	return json.Marshal(s)
}

var timeType = reflect.TypeFor[time.Time]()

func schemaOf(t reflect.Type, top bool, open map[reflect.Type]bool) (map[string]any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}, nil
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8:
		return map[string]any{"type": "string", "contentEncoding": "base64"}, nil
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string", "minLength": 1}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Slice, reflect.Array:
		items, err := schemaOf(t.Elem(), false, open)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map key %s is not a string", t.Key())
		}
		values, err := schemaOf(t.Elem(), false, open)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": values}, nil
	case reflect.Struct:
		if open[t] {
			return nil, fmt.Errorf("type %s is recursive", t)
		}
		open[t] = true
		defer delete(open, t)
		props := map[string]any{}
		if err := structProperties(t, top, open, props); err != nil {
			return nil, err
		}
		// encoding/json writes the members in key order, so the schema of a type is one byte string.
		return map[string]any{"type": "object", "properties": props, "additionalProperties": false}, nil
	}
	return nil, fmt.Errorf("kind %s has no JSON schema", t.Kind())
}

// structProperties adds the members of struct t to props, promoting the members of untagged
// embedded structs as encoding/json does.
func structProperties(t reflect.Type, top bool, open map[reflect.Type]bool, props map[string]any) error {
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Tag.Get("json") == "-" || (top && f.Name == KeyField) {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if f.Anonymous && ft.Kind() == reflect.Struct && jsonName(f) == f.Name {
			if err := structProperties(ft, false, open, props); err != nil {
				return err
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		name := jsonName(f)
		if _, dup := props[name]; dup {
			return fmt.Errorf("member %s of %s appears twice", name, t)
		}
		s, err := schemaOf(f.Type, false, open)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
		}
		props[name] = s
	}
	return nil
}
