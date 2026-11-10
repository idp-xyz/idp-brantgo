package strictjson

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"unicode"
)

var (
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	numberType          = reflect.TypeFor[json.Number]()
)

// resolve follows pointers to the type a JSON value decodes into, checking for custom unmarshalers
// where encoding/json does. It returns nil when the type accepts any JSON value (a
// json.Unmarshaler, which decides for itself) and text when the value must be a JSON string (an
// encoding.TextUnmarshaler).
func resolve(t reflect.Type) (_ reflect.Type, text bool) {
	if t == nil {
		return nil, false
	}
	if t.Kind() != reflect.Pointer && t.Name() != "" {
		switch p := reflect.PointerTo(t); {
		case p.Implements(jsonUnmarshalerType):
			return nil, false
		case p.Implements(textUnmarshalerType):
			return t, true
		}
	}
	for t.Kind() == reflect.Pointer {
		switch {
		case t.Implements(jsonUnmarshalerType):
			return nil, false
		case t.Implements(textUnmarshalerType):
			return t, true
		}
		t = t.Elem()
	}
	return t, false
}

// isBytes reports whether t is a byte slice, which encoding/json also fills from a base64 string.
func isBytes(t reflect.Type) bool {
	return t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8
}

// field is a struct member as encoding/json decodes it.
type field struct {
	typ    reflect.Type
	quoted bool // the ",string" option: the value travels inside a JSON string
}

var fieldCache sync.Map // reflect.Type → map[string]field

// fieldsOf returns the JSON names of struct type t with the field each one decodes into.
func fieldsOf(t reflect.Type) map[string]field {
	if f, ok := fieldCache.Load(t); ok {
		return f.(map[string]field)
	}
	f, _ := fieldCache.LoadOrStore(t, typeFields(t))
	return f.(map[string]field)
}

// typeFields resolves the JSON names of struct type t by the rules of encoding/json: exported
// fields, tag names, promotion from embedded structs breadth first, and per name the shallowest
// field, or the only tagged one among the shallowest; any other name collision drops the name.
func typeFields(t reflect.Type) map[string]field {
	type candidate struct {
		field
		name   string
		depth  int
		tagged bool
	}
	var candidates []candidate
	visited := map[reflect.Type]bool{}
	var count map[reflect.Type]int
	next, nextCount := []reflect.Type{t}, map[reflect.Type]int{}
	for depth := 0; len(next) > 0; depth++ {
		current := next
		next, count, nextCount = nil, nextCount, map[reflect.Type]int{}
		for _, st := range current {
			if visited[st] {
				continue
			}
			visited[st] = true
			for i := range st.NumField() {
				sf := st.Field(i)
				if sf.Anonymous {
					et := sf.Type
					if et.Kind() == reflect.Pointer {
						et = et.Elem()
					}
					if !sf.IsExported() && et.Kind() != reflect.Struct {
						continue
					}
				} else if !sf.IsExported() {
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, opts, _ := strings.Cut(tag, ",")
				if !isValidTag(name) {
					name = ""
				}
				ft := sf.Type
				if ft.Name() == "" && ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if name == "" && sf.Anonymous && ft.Kind() == reflect.Struct {
					nextCount[ft]++
					if nextCount[ft] == 1 {
						next = append(next, ft)
					}
					continue
				}
				c := candidate{field: field{typ: sf.Type, quoted: hasOption(opts, "string") && quotable(ft)}, name: name, depth: depth, tagged: name != ""}
				if !c.tagged {
					c.name = sf.Name
				}
				candidates = append(candidates, c)
				if count[st] > 1 { // the same struct embedded twice at one level: its names collide
					candidates = append(candidates, c)
				}
			}
		}
	}

	type best struct {
		c          candidate
		depth      int
		atDepth    int // candidates at the shallowest depth
		taggedSeen int // tagged candidates at the shallowest depth
	}
	byName := map[string]*best{}
	for _, c := range candidates {
		b := byName[c.name]
		if b == nil || c.depth < b.depth {
			b = &best{depth: c.depth}
			byName[c.name] = b
		}
		if c.depth > b.depth {
			continue
		}
		b.atDepth++
		if c.tagged {
			b.taggedSeen++
			b.c = c
		} else if b.taggedSeen == 0 {
			b.c = c
		}
	}
	fields := make(map[string]field, len(byName))
	for name, b := range byName {
		if b.atDepth == 1 || b.taggedSeen == 1 {
			fields[name] = b.c.field
		}
	}
	return fields
}

func hasOption(opts, option string) bool {
	for opts != "" {
		var o string
		o, opts, _ = strings.Cut(opts, ",")
		if o == option {
			return true
		}
	}
	return false
}

// quotable reports whether the ",string" option applies to a field of type t.
func quotable(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.String:
		return true
	}
	return false
}

// isValidTag is encoding/json's test for a usable tag name; any other tag name is ignored.
func isValidTag(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
		case !unicode.IsLetter(c) && !unicode.IsDigit(c):
			return false
		}
	}
	return true
}
