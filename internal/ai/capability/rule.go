package capability

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Rule is what an api command type states about one of its members for a caller that learns the
// command from a description (S4 G3: the tool schema a model is sent, and the words a refusal
// carries). It stands on the member itself, as struct tags of the api type:
//
//	UnitPrice string `contract:"required,decimal=price" doc:"the price of PriceBasisQuantity units"`
//
// The api packages import nothing for it; this package reads the tags by reflection. A rule
// describes what the context's own validation enforces and decides nothing: the gateway does not
// check a call against it. The tests of this package keep every rule true against the command's
// Validate (a required member left out is refused, every value of an enumeration is accepted and
// another refused, a call built from the rules' minimum passes).
//
// The tokens of the contract tag, separated by commas:
//
//	required          the member must be given; a required list has at least one item
//	id                an identifier (kernel.ValidIdentifier)
//	decimal=quantity  an unsigned decimal string in kernel.QuantityFormat (6 decimals), price in
//	                  kernel.PriceFormat (8) or factor in kernel.FactorFormat (12)
//	positive          the decimal is above 0
//	ratio             a kernel.PriceFormat decimal from 0 to below 1
//	date              YYYY-MM-DD
//	instant           the contracts' UTC timestamp (kernel.ParseTimestamp)
//	hash              64 lower-case hexadecimal digits
//	enum=a|b          one of the values
//	kind=x            the member is a reference whose Kind is x
//	min=n, max=n      bounds of an integer, or of the items of a list
//	when=F:a|b        the member belongs to the form in which sibling F is a or b: required then,
//	                  refused otherwise (the schema says "for F a or b")
//	optional          with when: allowed in that form, not required
//	with=F            the member is required when sibling F is given
//	wire=name         the member's name in the context's request paths, when it is not the snake_case
//	                  of the Go name (the object's own "id")
type Rule struct {
	Required bool
	ID       bool
	Decimal  string // quantity, price or factor
	Positive bool
	Ratio    bool
	Date     bool
	Instant  bool
	Hash     bool
	Enum     []string
	Kind     string
	Min, Max *int64
	When     *Condition
	Optional bool
	With     string
	Wire     string
	Doc      string
}

// Condition is a sibling member holding one of the values.
type Condition struct {
	Field  string
	Values []string
}

// Holds reports whether value satisfies the condition.
func (c *Condition) Holds(value string) bool {
	for _, v := range c.Values {
		if v == value {
			return true
		}
	}
	return false
}

// Empty reports whether the member states nothing.
func (r Rule) Empty() bool {
	return !r.Required && !r.ID && r.Decimal == "" && !r.Positive && !r.Ratio && !r.Date && !r.Instant && !r.Hash && len(r.Enum) == 0 &&
		r.Kind == "" && r.Min == nil && r.Max == nil && r.When == nil && r.With == "" && r.Wire == "" && r.Doc == ""
}

var decimalScale = map[string]int{"quantity": 6, "price": 8, "factor": 12}

// RuleOf reads the rule a struct field states.
func RuleOf(f reflect.StructField) (Rule, error) {
	r := Rule{Doc: strings.TrimSpace(f.Tag.Get("doc"))}
	tag := f.Tag.Get("contract")
	if tag == "" {
		return r, nil
	}
	bad := func(token string) (Rule, error) {
		return Rule{}, fmt.Errorf("contract tag of %s: %q", f.Name, token)
	}
	for _, token := range strings.Split(tag, ",") {
		name, value, has := strings.Cut(token, "=")
		switch name {
		case "required", "id", "positive", "ratio", "date", "instant", "hash", "optional":
			if has {
				return bad(token)
			}
		default:
			if !has || value == "" {
				return bad(token)
			}
		}
		switch name {
		case "required":
			r.Required = true
		case "id":
			r.ID = true
		case "positive":
			r.Positive = true
		case "ratio":
			r.Ratio = true
		case "date":
			r.Date = true
		case "instant":
			r.Instant = true
		case "hash":
			r.Hash = true
		case "optional":
			r.Optional = true
		case "decimal":
			if _, ok := decimalScale[value]; !ok {
				return bad(token)
			}
			r.Decimal = value
		case "enum":
			r.Enum = strings.Split(value, "|")
		case "kind":
			r.Kind = value
		case "min", "max":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return bad(token)
			}
			if name == "min" {
				r.Min = &n
			} else {
				r.Max = &n
			}
		case "when":
			field, values, ok := strings.Cut(value, ":")
			if !ok || field == "" || values == "" {
				return bad(token)
			}
			r.When = &Condition{Field: field, Values: strings.Split(values, "|")}
		case "with":
			r.With = value
		case "wire":
			r.Wire = value
		default:
			return bad(token)
		}
	}
	return r, r.fits(f)
}

// fits checks that the rule is one its member's type can carry.
func (r Rule) fits(f reflect.StructField) error {
	t := f.Type
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	text := r.ID || r.Decimal != "" || r.Positive || r.Ratio || r.Date || r.Instant || r.Hash || len(r.Enum) > 0
	bad := func(what string) error { return fmt.Errorf("contract tag of %s: %s", f.Name, what) }
	switch {
	case text && t.Kind() != reflect.String:
		return bad("a format or an enumeration is a string's")
	case r.Kind != "" && t.Kind() != reflect.Struct:
		return bad("kind is a reference's")
	case (r.Min != nil || r.Max != nil) && t.Kind() != reflect.Slice && (t.Kind() < reflect.Int || t.Kind() > reflect.Int64):
		return bad("bounds are an integer's or a list's")
	case r.Optional && r.When == nil, r.When != nil && r.Required, r.Positive && r.Decimal == "":
		return bad("tokens that do not go together")
	}
	return nil
}

// words says what the member takes, for the tool schema's description and a refusal's needs: the
// form it belongs to, its format, and the api type's own words.
func (r Rule) words() string {
	var parts []string
	switch {
	case r.When != nil && r.Optional:
		parts = append(parts, "optional, for "+r.When.Field+" "+strings.Join(r.When.Values, " or "))
	case r.When != nil:
		parts = append(parts, "for "+r.When.Field+" "+strings.Join(r.When.Values, " or "))
	case r.With != "":
		parts = append(parts, "required with "+r.With)
	}
	switch {
	case r.Decimal != "" && r.Positive:
		parts = append(parts, "decimal above 0, up to "+strconv.Itoa(decimalScale[r.Decimal])+" decimals")
	case r.Decimal != "":
		parts = append(parts, "decimal, up to "+strconv.Itoa(decimalScale[r.Decimal])+" decimals")
	case r.Ratio:
		parts = append(parts, "decimal from 0 to below 1")
	case r.Instant:
		parts = append(parts, "UTC, ends in Z")
	}
	if r.Doc != "" {
		parts = append(parts, r.Doc)
	}
	return strings.Join(parts, "; ")
}

// Needs says what the member needs, in the words the tool schema uses: whether it is required,
// its form, its format and the api type's own words. Empty when the member states nothing.
func (r Rule) Needs() string {
	var parts []string
	if r.Required {
		parts = append(parts, "required")
	}
	switch {
	case r.ID:
		parts = append(parts, "an identifier: 1 to 64 letters, digits and . _ : -, starting with a letter or digit")
	case r.Date:
		parts = append(parts, "a date YYYY-MM-DD")
	case r.Instant:
		parts = append(parts, "a timestamp YYYY-MM-DDThh:mm:ssZ")
	case r.Hash:
		parts = append(parts, "64 lower-case hexadecimal digits")
	case len(r.Enum) > 0:
		parts = append(parts, "one of the allowed values")
	case r.Kind != "":
		parts = append(parts, "a reference of kind "+r.Kind)
	}
	if r.Min != nil {
		parts = append(parts, "at least "+strconv.FormatInt(*r.Min, 10))
	}
	if r.Max != nil {
		parts = append(parts, "at most "+strconv.FormatInt(*r.Max, 10))
	}
	if w := r.words(); w != "" {
		parts = append(parts, w)
	}
	return strings.Join(parts, "; ")
}

// Allowed returns the values the member takes one of, or nil.
func (r Rule) Allowed() []string {
	if len(r.Enum) > 0 {
		return append([]string(nil), r.Enum...)
	}
	return nil
}
