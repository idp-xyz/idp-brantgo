package oidc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// maxNumericDate is 9999-12-31T23:59:59Z; later dates are refused as malformed.
const maxNumericDate = 253402300799

// claimSet holds the top-level members of a verified JWT claims set, undecoded.
type claimSet map[string]json.RawMessage

// decodeClaims reads a JSON object and refuses duplicate member names, which JSON parsers resolve
// differently.
func decodeClaims(payload []byte) (claimSet, error) {
	malformed := unauthenticated("claims are not a JSON object")
	dec := json.NewDecoder(bytes.NewReader(payload))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, malformed
	}
	claims := claimSet{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, malformed
		}
		name, ok := tok.(string)
		if !ok {
			return nil, malformed
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, malformed
		}
		if _, dup := claims[name]; dup {
			return nil, unauthenticated("duplicate claim")
		}
		claims[name] = raw
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, malformed
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, malformed
	}
	return claims, nil
}

// str returns the string claim name; it must be present and a JSON string.
func (c claimSet) str(name string) (string, error) {
	raw, ok := c[name]
	if !ok {
		return "", unauthenticated("missing claim " + name)
	}
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", unauthenticated("claim " + name + " is not a string")
	}
	return s, nil
}

// identifier returns the string claim name, which must satisfy kernel.ValidIdentifier.
func (c claimSet) identifier(name string) (string, error) {
	s, err := c.str(name)
	if err != nil {
		return "", err
	}
	if !kernel.ValidIdentifier(s) {
		return "", unauthenticated("claim " + name + " is not a valid identifier")
	}
	return s, nil
}

// audience returns "aud", a string or a non-empty array of strings.
func (c claimSet) audience() ([]string, error) {
	raw, ok := c["aud"]
	if !ok {
		return nil, unauthenticated("missing claim aud")
	}
	if len(raw) > 0 && raw[0] == '"' {
		s, err := c.str("aud")
		return []string{s}, err
	}
	var list []string
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &list) != nil || len(list) == 0 {
		return nil, unauthenticated("claim aud is not a string or an array of strings")
	}
	return list, nil
}

// date returns the NumericDate claim name and whether it is present. Only non-negative decimal
// seconds are accepted, without exponent; fractions are truncated to microseconds.
func (c claimSet) date(name string) (time.Time, bool, error) {
	raw, ok := c[name]
	if !ok {
		return time.Time{}, false, nil
	}
	bad := unauthenticated("claim " + name + " is not a NumericDate")
	whole, frac, hasFrac := strings.Cut(string(raw), ".")
	if !digits(whole) || (hasFrac && !digits(frac)) {
		return time.Time{}, false, bad
	}
	sec, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || sec > maxNumericDate {
		return time.Time{}, false, bad
	}
	micros := int64(0)
	if hasFrac {
		frac = (frac + "000000")[:6]
		micros, _ = strconv.ParseInt(frac, 10, 64)
	}
	return kernel.Instant(time.Unix(sec, micros*int64(time.Microsecond))), true, nil
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
