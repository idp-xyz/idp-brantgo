package api

import (
	"bytes"
	"encoding/json"
)

// MarshalJSON matches encoding/json when order_ids is omitted, and records the list when a caller
// cites more than the shipment's one order.
func (c PrepareShipment) MarshalJSON() ([]byte, error) {
	type alias PrepareShipment
	b, err := json.Marshal(alias(c))
	if err != nil {
		return nil, err
	}
	if len(c.OrderIDs) == 0 {
		return b, nil
	}
	raw, err := json.Marshal(c.OrderIDs)
	if err != nil {
		return nil, err
	}
	return insertJSON(b, "OrderIDs", raw), nil
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
