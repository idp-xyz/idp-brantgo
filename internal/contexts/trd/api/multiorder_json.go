package api

import (
	"bytes"
	"encoding/json"
)

// MarshalJSON matches encoding/json for a line that states no cap, and appends the cap when one is
// set. The fields are json:"-" so the Q01 schema does not change; the request hash still sees them.
func (l Line) MarshalJSON() ([]byte, error) {
	type alias Line
	b, err := json.Marshal(alias(l))
	if err != nil {
		return nil, err
	}
	if l.MaxOrderQuantity != "" {
		raw, err := json.Marshal(l.MaxOrderQuantity)
		if err != nil {
			return nil, err
		}
		b = insertJSON(b, "MaxOrderQuantity", raw)
	}
	if l.Uncapped != nil && *l.Uncapped {
		b = insertJSON(b, "Uncapped", []byte("true"))
	}
	return b, nil
}

// MarshalJSON matches encoding/json when conversion_mode is the default (empty or single_full),
// and records multi_order when the draft opts in.
func (d QuoteData) MarshalJSON() ([]byte, error) {
	type alias QuoteData
	b, err := json.Marshal(alias(d))
	if err != nil {
		return nil, err
	}
	if d.ConversionMode != "" && d.ConversionMode != "single_full" {
		raw, err := json.Marshal(d.ConversionMode)
		if err != nil {
			return nil, err
		}
		b = insertJSON(b, "ConversionMode", raw)
	}
	if d.DocumentProfile != "" && d.DocumentProfile != "generic" {
		raw, err := json.Marshal(d.DocumentProfile)
		if err != nil {
			return nil, err
		}
		b = insertJSON(b, "DocumentProfile", raw)
	}
	if d.CostBOMRef != nil {
		raw, err := json.Marshal(d.CostBOMRef)
		if err != nil {
			return nil, err
		}
		b = insertJSON(b, "CostBOMRef", raw)
	}
	return b, nil
}

// insertJSON appends one member to a JSON object, leaving the existing members in their order.
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
