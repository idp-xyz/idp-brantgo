package qlt

import (
	"context"
	"sync"

	mfgapi "go.idp.xyz/idp-brant/internal/contexts/mfg/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/adapters/subjects"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/application"
)

// FactorySlot is the late binding of MFG's InspectionSubject v1 (R25). QLT is composed before MFG,
// so the slot starts empty and the composition root binds it after mfg.New. Commitment lines still
// go to PUR. A subcontract operation asks the slot; an unbound slot is DEPENDENCY_NOT_SUPPORTED.
type FactorySlot struct {
	mu    sync.Mutex
	inner Subjects
}

// NewFactorySlot returns an unbound slot.
func NewFactorySlot() *FactorySlot { return &FactorySlot{} }

// Bind sets the MFG lookup. It is called once, before the process serves.
func (s *FactorySlot) Bind(subjects Subjects) {
	s.mu.Lock()
	s.inner = subjects
	s.mu.Unlock()
}

// ResolveSubjects answers subcontract-operation refs from the bound lookup.
func (s *FactorySlot) ResolveSubjects(ctx context.Context, refs []api.SubjectRef) (map[api.SubjectRef]application.Subject, error) {
	s.mu.Lock()
	inner := s.inner
	s.mu.Unlock()
	if inner == nil {
		return nil, application.ErrSubjectsNotWired
	}
	return inner.ResolveSubjects(ctx, refs)
}

type joinedSubjects struct {
	pur     Subjects
	factory *FactorySlot
}

// JoinedSubjects reads commitment lines from pur and subcontract operations from factory.
func JoinedSubjects(pur Subjects, factory *FactorySlot) Subjects {
	return joinedSubjects{pur: pur, factory: factory}
}

// ResolveSubjects splits refs by kind. A commitment line is unchanged from PUR's port. A
// subcontract operation is MFG's. Any other kind is absent, as PUR's port already left it.
func (j joinedSubjects) ResolveSubjects(ctx context.Context, refs []api.SubjectRef) (map[api.SubjectRef]application.Subject, error) {
	var commitments, operations []api.SubjectRef
	for _, r := range refs {
		if r.Kind == api.SubjectSubcontractOperation {
			operations = append(operations, r)
			continue
		}
		commitments = append(commitments, r)
	}
	out := map[api.SubjectRef]application.Subject{}
	if len(commitments) > 0 && j.pur != nil {
		got, err := j.pur.ResolveSubjects(ctx, commitments)
		if err != nil {
			return nil, err
		}
		for k, v := range got {
			out[k] = v
		}
	}
	if len(operations) > 0 {
		if j.factory == nil {
			return nil, application.ErrSubjectsNotWired
		}
		got, err := j.factory.ResolveSubjects(ctx, operations)
		if err != nil {
			return nil, err
		}
		for k, v := range got {
			out[k] = v
		}
	}
	return out, nil
}

// MFGSubjects is the port to MFG over InspectionSubject v1 (R25).
func MFGSubjects(q mfgapi.Queries) Subjects { return subjects.MFG{Subjects: q} }
