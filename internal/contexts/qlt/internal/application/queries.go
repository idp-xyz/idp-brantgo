package application

import (
	"context"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/domain/inspection"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/domain/release"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func invalidID(s Service, ctx context.Context, command string) error {
	err := kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	if auditErr := s.Ledger.AuditRejected(ctx, command, "", kernel.CodeSchemaInvalid); auditErr != nil {
		return auditErr
	}
	return err
}

// subjectReadable asks PUR, before QLT's read transaction, whether the caller still reads ref.
func (s Service) subjectReadable(ctx context.Context, ref inspection.SubjectRef) (bool, error) {
	got, err := s.subjects(ctx, []api.SubjectRef{apiRef(ref)}, "/id")
	if err != nil {
		return false, err
	}
	_, ok := got[apiRef(ref)]
	return ok, nil
}

// GetInspection returns an inspection the caller sees, audited as inspection.get (qlt.read):
// NOT_FOUND otherwise; REFERENCE_INVALID when the caller no longer reads its subject in PUR, or its
// standard or a source of its evidence in EVD (R24, R72).
func (s Service) GetInspection(ctx context.Context, id string) (api.InspectionView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.InspectionView{}, invalidID(s, ctx, api.CommandGetInspection)
	}
	var stored *inspection.Inspection
	if err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		stored, err = postgres.LoadInspection(ctx, exec, scope(ctx), id, postgres.NoLock)
		return err
	}); err != nil {
		return api.InspectionView{}, err
	}
	readable := false
	if stored != nil {
		ok, err := s.subjectReadable(ctx, stored.Subject.Ref)
		if err != nil {
			return api.InspectionView{}, err
		}
		cited := append([]string{stored.Standard.ID}, stored.Evidence...)
		if stored.Disposition != nil {
			cited = append(cited, stored.Disposition.Evidence...)
		}
		found, err := s.sources(ctx, cited)
		if err != nil {
			return api.InspectionView{}, err
		}
		readable = ok && inspection.Readable(cited, found, "/id") == nil && found[stored.Standard.ID] == stored.Standard.SHA256
	}
	var v api.InspectionView
	err := s.Ledger.Read(ctx, api.CommandGetInspection, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		i, err := postgres.LoadInspection(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if i == nil || !d.Sees(i.OwnerActor, i.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		if !readable || stored == nil {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/id", "The caller no longer reads the subject or a source the inspection cites")
		}
		v = inspectionView(i)
		return nil
	})
	return v, err
}

func bindingView(s inspection.Subject) api.SubjectBinding {
	return api.SubjectBinding{Ref: apiRef(s.Ref), Revision: s.Revision, Hash: s.Hash, SupplierID: s.SupplierID, OrderID: s.OrderID,
		OrderRevision: s.OrderRevision, OrderHash: s.OrderHash, OrderLineID: s.OrderLineID, SkuID: s.SkuID, SpecRevision: s.SpecRevision,
		UnitID: s.UnitID, UnitRevision: s.UnitRevision, Quantity: postgres.Text(s.Quantity)}
}

func inspectionView(i *inspection.Inspection) api.InspectionView {
	v := api.InspectionView{InspectionID: i.ID, Status: i.Status, RowVersion: i.RowVersion, Subject: bindingView(i.Subject),
		InspectedQuantity: postgres.Text(i.Inspected), ConformingQuantity: postgres.Text(i.Conforming),
		NonconformingQuantity: postgres.Text(i.Nonconforming), Sampling: api.Sampling{Method: i.Sampling.Method, Plan: i.Sampling.Plan},
		Standard:    api.SourceBinding{SourceID: i.Standard.ID, ContentSHA256: i.Standard.SHA256},
		Inspector:   api.InspectorView{Kind: i.Inspector.Kind, ActorID: i.Inspector.ActorID},
		Report:      api.FileBinding{FileID: i.Report.ID, ContentSHA256: i.Report.SHA256, MimeType: i.Report.MimeType},
		InspectedAt: i.InspectedAt, SupersedesInspectionID: i.Supersedes, Reason: i.Reason, EvidenceIDs: slices.Clone(i.Evidence),
		SupersededBy: i.SupersededBy, SupersededAt: i.SupersededAt, RecordedBy: i.Recorded.By, RecordedAt: i.Recorded.At,
		AuthorityEpoch: i.Recorded.Epoch}
	if size := i.Sampling.SampleSize; size != nil {
		v.Sampling.SampleSize = postgres.Text(*size)
	}
	if i.Inspector.Kind == inspection.InspectorAgency {
		v.Inspector.Agency = &api.AgencyRef{PartyID: i.Inspector.PartyID, ServiceRoleID: i.Inspector.ServiceRoleID, Revision: i.Inspector.RoleRevision}
	}
	if d := i.Disposition; d != nil {
		dv := &api.DispositionView{Reason: d.Reason, EvidenceIDs: slices.Clone(d.Evidence), DecidedBy: d.Decided.By, DecidedAt: d.Decided.At,
			AuthorityEpoch: d.Decided.Epoch}
		for _, it := range d.Items {
			dv.Items = append(dv.Items, api.DispositionItem{Action: it.Action, Quantity: postgres.Text(it.Quantity)})
		}
		v.Disposition = dv
	}
	return v
}

// GetRelease returns a release the caller sees, audited as quality_release.get (qlt.read):
// NOT_FOUND otherwise; REFERENCE_INVALID when the caller no longer reads its subject in PUR. Its
// state is decided now.
func (s Service) GetRelease(ctx context.Context, id string) (api.ReleaseView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.ReleaseView{}, invalidID(s, ctx, api.CommandGetRelease)
	}
	var stored *release.Release
	if err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		stored, err = postgres.LoadRelease(ctx, exec, scope(ctx), id, postgres.NoLock)
		return err
	}); err != nil {
		return api.ReleaseView{}, err
	}
	readable := false
	if stored != nil {
		var err error
		if readable, err = s.subjectReadable(ctx, stored.Subject.Ref); err != nil {
			return api.ReleaseView{}, err
		}
	}
	var v api.ReleaseView
	err := s.Ledger.Read(ctx, api.CommandGetRelease, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		r, err := postgres.LoadRelease(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if r == nil || !d.Sees(r.OwnerActor, r.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		if !readable || stored == nil {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/id", "The caller no longer reads the subject of the release")
		}
		v = releaseView(r, s.Clock.Now())
		return nil
	})
	return v, err
}

func releaseView(r *release.Release, now time.Time) api.ReleaseView {
	at := kernel.Instant(now)
	v := api.ReleaseView{ReleaseID: r.ID, Status: r.Status, State: r.State(at), RowVersion: r.RowVersion, Subject: bindingView(r.Subject),
		Quantity: postgres.Text(r.Quantity), ValidFrom: r.ValidFrom, ValidUntil: r.ValidUntil, Conforming: postgres.Text(r.Conforming),
		ReleasedBefore: postgres.Text(r.ReleasedBefore), InspectionIDs: slices.Clone(r.InspectionIDs), EvidenceIDs: slices.Clone(r.Evidence),
		GrantedBy: r.Granted.By, GrantedAt: r.Granted.At, AuthorityEpoch: r.Granted.Epoch, CheckedAt: at}
	if x := r.Revocation; x != nil {
		v.Revocation = &api.RevocationView{Reason: x.Reason, EvidenceIDs: slices.Clone(x.Evidence), RevokedBy: x.Revoked.By,
			RevokedAt: x.Revoked.At, AuthorityEpoch: x.Revoked.Epoch}
	}
	return v
}

// GetSubject returns a subject's quality standing now, audited as inspection_subject.get
// (qlt.read): what its valid inspections cover and find conforming at PUR's current specification
// of the subject, what was released at it (revoked releases included) and what may still be
// released; records the caller does not see count without their IDs. NOT_FOUND when PUR does not
// show the caller the subject.
func (s Service) GetSubject(ctx context.Context, ref api.SubjectRef) (api.SubjectView, error) {
	if err := ref.Validate("/subject"); err != nil {
		if auditErr := s.Ledger.AuditRejected(ctx, api.CommandGetSubject, ref.ID, kernel.CodeSchemaInvalid); auditErr != nil {
			return api.SubjectView{}, auditErr
		}
		return api.SubjectView{}, err
	}
	got, err := s.subjects(ctx, []api.SubjectRef{ref}, "/subject")
	if err != nil {
		return api.SubjectView{}, err
	}
	var subject *inspection.Subject
	if p, ok := got[ref]; ok {
		if subject, err = subjectOf(p); err != nil {
			return api.SubjectView{}, err
		}
	}
	var v api.SubjectView
	err = s.Ledger.Read(ctx, api.CommandGetSubject, ref.ID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		if subject == nil {
			return kernel.Reject(kernel.CodeNotFound, "/subject", "")
		}
		sc := scope(ctx)
		is, err := postgres.LoadSubjectInspections(ctx, exec, sc, subject.Ref)
		if err != nil {
			return err
		}
		rs, err := postgres.LoadSubjectReleases(ctx, exec, sc, subject.Ref)
		if err != nil {
			return err
		}
		v = subjectView(*subject, is, rs, d, s.Clock.Now())
		return nil
	})
	return v, err
}

func subjectView(subject inspection.Subject, is []*inspection.Inspection, rs []*release.Release, d authorization.Decision, now time.Time) api.SubjectView {
	at := kernel.Instant(now)
	spec := subject.Spec()
	covered, conforming, _ := inspection.Totals(is, spec)
	released := release.Released(rs, spec)
	releasable := conforming.Sub(released)
	if releasable.Sign() < 0 {
		releasable = kernel.Decimal{}
	}
	v := api.SubjectView{Subject: bindingView(subject), Inspected: postgres.Text(covered), Conforming: postgres.Text(conforming),
		Released: postgres.Text(released), Releasable: postgres.Text(releasable), Inspections: []api.SubjectInspection{},
		Releases: []api.SubjectRelease{}, CheckedAt: at}
	for _, i := range is {
		e := api.SubjectInspection{Status: i.Status, Inspected: postgres.Text(i.Inspected), Conforming: postgres.Text(i.Conforming), Counted: i.Counts(spec)}
		if d.Sees(i.OwnerActor, i.OwnerTeam) {
			e.InspectionID = i.ID
		}
		v.Inspections = append(v.Inspections, e)
	}
	for _, r := range rs {
		e := api.SubjectRelease{Status: r.Status, State: r.State(at), Quantity: postgres.Text(r.Quantity), Counted: r.Counts(spec)}
		if d.Sees(r.OwnerActor, r.OwnerTeam) {
			e.ReleaseID = r.ID
		}
		v.Releases = append(v.Releases, e)
	}
	return v
}

// ResolveReleases answers QualityReleaseRef v1 lookups (api.ReleaseLookups, R26): the releases among
// ids that the caller sees, at instant at. It needs an active grant only; it is not audited.
func (s Service) ResolveReleases(ctx context.Context, ids []string, at time.Time) (map[api.QualityReleaseRef]api.ResolvedRelease, error) {
	out := map[api.QualityReleaseRef]api.ResolvedRelease{}
	if err := api.ValidateReleaseIDs(ids); err != nil || len(ids) == 0 {
		return out, err
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		found, err := postgres.LoadReleases(ctx, exec, scope(ctx), ids)
		if err != nil {
			return err
		}
		for id, r := range found {
			if !d.Sees(r.OwnerActor, r.OwnerTeam) {
				continue
			}
			ref := api.QualityReleaseRef{ReleaseID: id}
			sub := r.Subject
			out[ref] = api.ResolvedRelease{Ref: ref, Status: r.Status, State: r.State(at), RowVersion: r.RowVersion,
				Scope: api.ReleaseScope{Subject: apiRef(sub.Ref), SupplierID: sub.SupplierID, OrderID: sub.OrderID, OrderLineID: sub.OrderLineID,
					SkuID: sub.SkuID, SpecRevision: sub.SpecRevision, UnitID: sub.UnitID, UnitRevision: sub.UnitRevision},
				Quantity: postgres.Text(r.Quantity), ValidFrom: r.ValidFrom, ValidUntil: r.ValidUntil, Code: r.Code(at)}
		}
		return nil
	})
	return out, err
}
