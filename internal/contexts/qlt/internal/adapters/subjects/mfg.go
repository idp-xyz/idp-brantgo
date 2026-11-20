package subjects

import (
	"context"

	mfgapi "go.idp.xyz/idp-brant/internal/contexts/mfg/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/application"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// MFG answers QLT's subjects of kind mfg_subcontract_operation from MFG's InspectionSubject v1
// (Context Map R25). A subcontract MFG does not show the caller is absent. A cancelled subcontract
// is present with Code INVALID_STATE, so QLT treats it as stale. A progress claim is not a subject.
type MFG struct{ Subjects mfgapi.Queries }

// ResolveSubjects asks MFG for the subcontract operations among refs.
func (m MFG) ResolveSubjects(ctx context.Context, refs []api.SubjectRef) (map[api.SubjectRef]application.Subject, error) {
	out := map[api.SubjectRef]application.Subject{}
	if m.Subjects == nil {
		return nil, application.ErrSubjectsNotWired
	}
	for _, r := range refs {
		if r.Kind != api.SubjectSubcontractOperation {
			continue
		}
		s, err := m.Subjects.ResolveInspectionSubject(ctx, r.ID, r.LineID)
		if err != nil {
			if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
				continue
			}
			return nil, err
		}
		out[r] = application.Subject{Ref: r, Revision: s.Revision, Hash: s.Hash, Status: s.Status, Code: s.Code,
			SupplierID: s.FactoryID, OrderID: s.OrderID, OrderRevision: s.OrderRevision, OrderHash: s.OrderHash,
			OrderLineID: s.OrderLineID, SkuID: s.SkuID, SpecRevision: s.SpecRevision, UnitID: s.UnitID,
			UnitRevision: s.UnitRevision, Quantity: s.Quantity}
	}
	return out, nil
}
