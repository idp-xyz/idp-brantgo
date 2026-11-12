// Package subjects implements QLT's port to PUR (Context Map R24, InspectionSubject v1) over PUR's
// published lookup ResolveCommitmentLines.
package subjects

import (
	"context"

	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/application"
)

// PUR answers QLT's subjects from PUR's InspectionSubject v1.
type PUR struct{ Lines purapi.InspectionSubjects }

// ResolveSubjects asks PUR for the commitment lines among refs; references of another kind and
// lines PUR does not show the caller are absent. A subject is the line as PUR states it now: the
// commitment at its row version and the content hash of the draft it commits, its state and PUR's
// reason when it is not an inspection subject, the order line it serves, the SKU specification and
// unit, and the quantity bought (allocated plus an approved overbuy).
func (p PUR) ResolveSubjects(ctx context.Context, refs []api.SubjectRef) (map[api.SubjectRef]application.Subject, error) {
	var asked []purapi.CommitmentLineRef
	for _, r := range refs {
		if r.Kind == api.SubjectCommitmentLine {
			asked = append(asked, purapi.CommitmentLineRef{CommitmentID: r.ID, OrderLineID: r.LineID})
		}
	}
	out := map[api.SubjectRef]application.Subject{}
	if len(asked) == 0 {
		return out, nil
	}
	lines, err := p.Lines.ResolveCommitmentLines(ctx, asked)
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		s := Subject(l)
		out[s.Ref] = s
	}
	return out, nil
}

// Subject is QLT's view of a PUR commitment line.
func Subject(l purapi.CommitmentLine) application.Subject {
	return application.Subject{Ref: api.SubjectRef{Kind: api.SubjectCommitmentLine, ID: l.Ref.CommitmentID, LineID: l.Ref.OrderLineID},
		Revision: l.RowVersion, Hash: l.DraftHash, Status: l.Status, Code: l.Code, SupplierID: l.SupplierID, OrderID: l.OrderID,
		OrderRevision: l.OrderRevision, OrderHash: l.OrderHash, OrderLineID: l.Ref.OrderLineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision,
		UnitID: l.UnitID, UnitRevision: l.UnitRevision, Quantity: l.PurchaseQuantity}
}
