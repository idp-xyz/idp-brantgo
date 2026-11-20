// Package batchlineage records an input batch against an output batch or a shipment line
// (STK-AGG-BatchLineage, baseline 3.7). R1 has no command of its own: a receipt or an issue
// appends the links it was given. A link names one output, never both, and never neither.
package batchlineage

import "go.idp.xyz/idp-brant/internal/kernel"

// Output kinds.
const (
	OutputBatch    = "batch"
	OutputShipment = "shipment_line"
)

// Link is one input batch contributing Quantity to one output.
type Link struct {
	InputBatchID  string
	OutputKind    string
	OutputBatchID string
	OutputID      string
	OutputLineID  string
	Quantity      kernel.Decimal
	UnitID        string
	UnitRevision  int64
}

// Normalize checks the link and sets OutputKind from which output was named.
func Normalize(inputBatch, outputBatch, shipmentID, shipmentLine, quantity, unit string, revision int64) (Link, error) {
	qty, err := kernel.ParseDecimal(quantity, kernel.QuantityFormat)
	if err != nil || qty.Sign() <= 0 || !kernel.ValidIdentifier(inputBatch) || !kernel.ValidIdentifier(unit) || revision <= 0 {
		return Link{}, errForm
	}
	batch := outputBatch != ""
	ship := shipmentID != "" || shipmentLine != ""
	switch {
	case batch == ship:
		return Link{}, errForm
	case batch && !kernel.ValidIdentifier(outputBatch):
		return Link{}, errForm
	case ship && (!kernel.ValidIdentifier(shipmentID) || !kernel.ValidIdentifier(shipmentLine)):
		return Link{}, errForm
	}
	link := Link{InputBatchID: inputBatch, Quantity: qty, UnitID: unit, UnitRevision: revision}
	if batch {
		link.OutputKind, link.OutputBatchID = OutputBatch, outputBatch
	} else {
		link.OutputKind, link.OutputID, link.OutputLineID = OutputShipment, shipmentID, shipmentLine
	}
	return link, nil
}

var errForm = errType("stk: batch lineage link is not one input to one output")

type errType string

func (e errType) Error() string { return string(e) }

// ErrForm is a link that does not name exactly one output.
func ErrForm() error { return errForm }
