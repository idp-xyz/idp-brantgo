package capability

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"
)

// Schema returns the JSON Schema (draft 2020-12 subset) of the arguments a model may send for c:
// the api command's exported fields under their Go names, which is how strictjson decodes them into
// the command, without the idempotency key, and with no additional properties at any level.
//
// It states what the command needs as the api type's own rules say it (Rule, S4 G3): the members
// that are required, the values of a member that takes one of a set, the kind a reference names,
// the format of a decimal, a ratio, a date, an instant and a hash, the bounds of an integer, and a
// few words where the api type gives them. It is what the model sees; the gateway still decodes
// strictly and does not check a call against it, and the context still validates.
//
// Every string of the contracts has at least one character (the gateway refuses an empty one), so
// the schema does not repeat it on each member.
func (c Capability) Schema() (json.RawMessage, error) {
	s, err := schemaOf(c.input, Rule{}, true, map[reflect.Type]bool{})
	if err != nil {
		return nil, fmt.Errorf("capability: schema of %s: %w", c.Command, err)
	}
	return json.Marshal(s)
}

var timeType = reflect.TypeFor[time.Time]()

func schemaOf(t reflect.Type, r Rule, top bool, open map[reflect.Type]bool) (map[string]any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	described := func(s map[string]any) map[string]any {
		if w := r.words(); w != "" {
			s["description"] = w
		}
		return s
	}
	switch {
	case t == timeType:
		return described(map[string]any{"type": "string", "format": "date-time"}), nil
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8:
		return described(map[string]any{"type": "string", "contentEncoding": "base64"}), nil
	}
	switch t.Kind() {
	case reflect.String:
		s := map[string]any{"type": "string"}
		switch {
		case len(r.Enum) > 0:
			s["enum"] = r.Enum
		case r.Date:
			s["format"] = "date"
		case r.Instant:
			s["format"] = "date-time"
		case r.Hash:
			s["pattern"] = "^[0-9a-f]{64}$"
		}
		return described(s), nil
	case reflect.Bool:
		return described(map[string]any{"type": "boolean"}), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		s := map[string]any{"type": "integer"}
		if r.Min != nil {
			s["minimum"] = *r.Min
		}
		if r.Max != nil {
			s["maximum"] = *r.Max
		}
		return described(s), nil
	case reflect.Slice, reflect.Array:
		items, err := schemaOf(t.Elem(), Rule{}, false, open)
		if err != nil {
			return nil, err
		}
		// A list's upper bound (200 lines, 16 locators) is the contract's ingress limit, not
		// something a draft is written against: the schema leaves it out.
		s := map[string]any{"type": "array", "items": items}
		switch {
		case r.Min != nil:
			s["minItems"] = *r.Min
		case r.Required:
			s["minItems"] = 1
		}
		return described(s), nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map key %s is not a string", t.Key())
		}
		values, err := schemaOf(t.Elem(), Rule{}, false, open)
		if err != nil {
			return nil, err
		}
		return described(map[string]any{"type": "object", "additionalProperties": values}), nil
	case reflect.Struct:
		if open[t] {
			return nil, fmt.Errorf("type %s is recursive", t)
		}
		open[t] = true
		defer delete(open, t)
		props := map[string]any{}
		var required []string
		if err := structProperties(t, top, open, props, &required); err != nil {
			return nil, err
		}
		if r.Kind != "" {
			// The reference names one kind: the member says which, instead of "any string".
			if _, ok := props["Kind"]; !ok {
				return nil, fmt.Errorf("type %s has no Kind for kind=%s", t, r.Kind)
			}
			props["Kind"] = map[string]any{"const": r.Kind}
		}
		// encoding/json writes the members in key order, so the schema of a type is one byte string.
		s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			sort.Strings(required)
			s["required"] = required
		}
		return described(s), nil
	}
	return nil, fmt.Errorf("kind %s has no JSON schema", t.Kind())
}

// structProperties adds the members of struct t to props and the names of its required members to
// required, promoting the members of untagged embedded structs as encoding/json does.
func structProperties(t reflect.Type, top bool, open map[reflect.Type]bool, props map[string]any, required *[]string) error {
	for i := range t.NumField() {
		f := t.Field(i)
		// ai:"-" keeps a member on the api type, and in request hashes, without offering it to the
		// model. rfq.save_draft's OpportunityID is one (S6-D04): the tool schema, and therefore
		// q01.agent's bundle hash, stays the schema of the members the model already had.
		if f.Tag.Get("json") == "-" || f.Tag.Get("ai") == "-" || (top && f.Name == KeyField) {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if f.Anonymous && ft.Kind() == reflect.Struct && jsonName(f) == f.Name {
			if err := structProperties(ft, false, open, props, required); err != nil {
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
		rule, err := RuleOf(f)
		if err != nil {
			return fmt.Errorf("%s: %w", t.Name(), err)
		}
		s, err := schemaOf(f.Type, rule, false, open)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
		}
		props[name] = s
		if rule.Required {
			*required = append(*required, name)
		}
	}
	return nil
}
