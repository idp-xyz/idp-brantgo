package api

import (
	"bytes"
	"encoding/json"
)

// MarshalJSON matches encoding/json for a generic or omitted document profile, and records apparel
// so the request hash changes when the draft opts into the industry template.
func (c SaveRfq) MarshalJSON() ([]byte, error) {
	type alias SaveRfq
	b, err := json.Marshal(alias(c))
	if err != nil {
		return nil, err
	}
	if c.DocumentProfile != "" && c.DocumentProfile != "generic" {
		raw, err := json.Marshal(c.DocumentProfile)
		if err != nil {
			return nil, err
		}
		b = insertJSON(b, "DocumentProfile", raw)
	}
	return b, nil
}

func insertJSON(obj []byte, key string, value []byte) []byte {
	obj = bytes.TrimSpace(obj)
	if len(obj) < 2 || obj[0] != '{' || obj[len(obj)-1] != '}' {
		return obj
	}
	inner := obj[1 : len(obj)-1]
	piece := append([]byte(`,"`+key+`":`), value...)
	if len(bytes.TrimSpace(inner)) == 0 {
		piece = piece[1:]
	}
	out := make([]byte, 0, len(obj)+len(piece))
	out = append(out, '{')
	out = append(out, inner...)
	out = append(out, piece...)
	out = append(out, '}')
	return out
}
