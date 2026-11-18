package capability

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
)

// Member is the member of a tool's arguments that a rejection's path names, with what the api type
// states about it. A context points at its request in the names of its transports
// ("/args/data/lines/0/unit_price"); a model sends the api command's Go names
// (Data.Lines[0].UnitPrice). Member is the bridge, so a refusal can say which argument it is about
// and what that argument needs (S4 G3).
type Member struct {
	// Path is the member in the notation of the tool's arguments: Go names joined by dots, an
	// item of a list by its index in brackets.
	Path  string
	Rule  Rule
	steps []memberStep
}

type memberStep struct {
	name  string // the member's name in the arguments (jsonName); "" for an item
	index int
}

// Member resolves path, a JSON Pointer of a rejection, to the member of c's arguments it names. A
// path that is not below /args, or that names no member of the api command, resolves to nothing:
// "/idempotency_key", "/command", "/grant" and "/budget/…" are the platform's, not arguments.
func (c Capability) Member(path string) (Member, bool) {
	rest, ok := strings.CutPrefix(path, "/args/")
	if !ok || rest == "" {
		return Member{}, false
	}
	t := c.input
	var m Member
	var rule, parent Rule
	var b strings.Builder
	for i, raw := range strings.Split(rest, "/") {
		seg := strings.NewReplacer("~1", "/", "~0", "~").Replace(raw)
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Slice, reflect.Array:
			n, err := strconv.Atoi(seg)
			if err != nil || n < 0 || t.Elem().Kind() == reflect.Uint8 {
				return Member{}, false
			}
			m.steps = append(m.steps, memberStep{index: n})
			b.WriteString("[" + strconv.Itoa(n) + "]")
			t = t.Elem()
			parent, rule = Rule{}, Rule{Doc: rule.Doc}
		case reflect.Struct:
			f, ok := memberField(t, seg, i == 0)
			if !ok {
				return Member{}, false
			}
			r, err := RuleOf(f)
			if err != nil {
				return Member{}, false
			}
			if f.Name == "Kind" && parent.Kind != "" {
				// The Kind of a reference whose member names one kind.
				r = Rule{Required: true, Enum: []string{parent.Kind}}
			}
			name := jsonName(f)
			m.steps = append(m.steps, memberStep{name: name})
			if b.Len() > 0 {
				b.WriteString(".")
			}
			b.WriteString(name)
			t = f.Type
			parent, rule = r, r
		default:
			return Member{}, false
		}
	}
	m.Path, m.Rule = b.String(), rule
	return m, true
}

// memberField finds the field of struct t a path segment names: by its wire name when the rule
// gives one, else by the snake_case of its name (compared without underscores and case, so
// "unit_price" and "UnitPrice" are one name), or by the name itself, as the gateway's own paths
// use it.
func memberField(t reflect.Type, seg string, top bool) (reflect.StructField, bool) {
	want := plainName(seg)
	var found reflect.StructField
	hits := 0
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() || f.Tag.Get("json") == "-" || (top && f.Name == KeyField) {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if f.Anonymous && ft.Kind() == reflect.Struct && jsonName(f) == f.Name {
			if g, ok := memberField(ft, seg, false); ok {
				found, hits = g, hits+1
			}
			continue
		}
		if r, err := RuleOf(f); err == nil && r.Wire != "" && r.Wire == seg {
			return f, true
		}
		if plainName(jsonName(f)) == want {
			found, hits = f, hits+1
		}
	}
	return found, hits == 1
}

func plainName(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "_", ""))
}

// Missing reports whether args, the arguments of a call, do not carry the member. Arguments that
// are not a JSON object carry nothing.
func (m Member) Missing(args json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return true
	}
	for _, s := range m.steps {
		switch node := v.(type) {
		case map[string]any:
			next, ok := node[s.name]
			if !ok || s.name == "" {
				return true
			}
			v = next
		case []any:
			if s.name != "" || s.index >= len(node) {
				return true
			}
			v = node[s.index]
		default:
			return true
		}
	}
	return v == nil
}
