package application

import (
	"cmp"
	"context"
	"errors"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/domain/inspection"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func subjectRef(r api.SubjectRef) inspection.SubjectRef {
	return inspection.SubjectRef{Kind: r.Kind, ID: r.ID, LineID: r.LineID}
}

func apiRef(r inspection.SubjectRef) api.SubjectRef {
	return api.SubjectRef{Kind: r.Kind, ID: r.ID, LineID: r.LineID}
}

// subjectOf is PUR's subject as the rules read it, or an error for a quantity PUR should never
// answer.
func subjectOf(s Subject) (*inspection.Subject, error) {
	q, ok := api.Quantity(s.Quantity)
	if !ok || q.Sign() <= 0 {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/subject", "PUR's subject has no quantity")
	}
	return &inspection.Subject{Ref: subjectRef(s.Ref), Revision: s.Revision, Hash: s.Hash, SupplierID: s.SupplierID, OrderID: s.OrderID,
		OrderRevision: s.OrderRevision, OrderHash: s.OrderHash, OrderLineID: s.OrderLineID, SkuID: s.SkuID, SpecRevision: s.SpecRevision,
		UnitID: s.UnitID, UnitRevision: s.UnitRevision, Quantity: q}, nil
}

// recordRequest is the domain's request for cmd, whose structure Validate checked.
func recordRequest(cmd api.RecordInspection) inspection.Request {
	r := inspection.Request{ID: cmd.InspectionID, Subject: subjectRef(cmd.Subject), StandardID: cmd.Standard.SourceID,
		InspectorKind: cmd.Inspector.Kind, ReportFileID: cmd.ReportFileID, Supersedes: cmd.SupersedesInspectionID, Reason: cmd.Reason,
		Evidence: slices.Clone(cmd.EvidenceIDs), RequestHash: requestHash(cmd),
		Sampling: inspection.Sampling{Method: cmd.Sampling.Method, Plan: cmd.Sampling.Plan}}
	r.Inspected, _ = api.Quantity(cmd.InspectedQuantity)
	r.Conforming, _ = api.Quantity(cmd.ConformingQuantity)
	r.Nonconforming, _ = api.Quantity(cmd.NonconformingQuantity)
	if size, ok := api.Quantity(cmd.Sampling.SampleSize); ok {
		r.Sampling.SampleSize = &size
	}
	if a := cmd.Inspector.Agency; a != nil {
		r.Agency = &inspection.Inspector{Kind: inspection.InspectorAgency, PartyID: a.PartyID, ServiceRoleID: a.ServiceRoleID, RoleRevision: a.Revision}
	}
	r.InspectedAt, _ = kernel.ParseTimestamp(cmd.InspectedAt)
	return r
}

// recordCommand runs inspection.record. Before the transaction it reads the subject from PUR, the
// standard, evidence and report from EVD and the agency from PTY; inside it, it locks the
// inspection its ID names, or else every inspection of the subject FOR UPDATE, decides and writes
// the inspection and, for a re-inspection or correction, the supersession of the one it cites.
type recordCommand struct {
	s        Service
	cmd      api.RecordInspection
	req      inspection.Request
	subject  *inspection.Subject
	code     string
	sources  map[string]string
	report   *inspection.Binding
	agency   *inspection.Agency
	existing *inspection.Inspection
	others   []*inspection.Inspection
	replay   bool
}

func (h *recordCommand) Prepare(ctx context.Context) error {
	var taken bool
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		i, err := postgres.LoadInspection(ctx, exec, scope(ctx), h.req.ID, postgres.NoLock)
		taken = i != nil
		return err
	})
	if err != nil || taken {
		// A taken inspection ID is answered from the stored inspection; nothing upstream is asked.
		return err
	}
	got, err := h.s.subjects(ctx, []api.SubjectRef{h.cmd.Subject}, "/args/subject")
	if err != nil {
		return err
	}
	if s, ok := got[h.cmd.Subject]; ok {
		if h.subject, err = subjectOf(s); err != nil {
			return err
		}
		h.code = s.Code
	}
	if h.sources, err = h.s.sources(ctx, append([]string{h.req.StandardID}, h.req.Evidence...)); err != nil {
		return err
	}
	file, err := h.s.Upstream.Sources.GetFile(ctx, h.req.ReportFileID)
	switch {
	case err == nil:
		h.report = fileBinding(file)
	case isNotFound(err):
	default:
		return factErr(err, "/args/report_file_id")
	}
	if a := h.req.Agency; a != nil {
		ref := ptyapi.ServiceRoleRef{PartyID: a.PartyID, ServiceRoleID: a.ServiceRoleID, Revision: a.RoleRevision}
		roles, err := h.s.Upstream.Parties.ResolveServiceRoles(ctx, []ptyapi.ServiceRoleRef{ref}, h.req.InspectedAt)
		if err != nil {
			return err
		}
		if r, ok := roles[ref]; ok {
			h.agency = &inspection.Agency{Role: r.Role, Check: r.Check}
		}
	}
	return nil
}

func isNotFound(err error) bool {
	rej, ok := kernel.AsRejection(err)
	return ok && rej.Code == kernel.CodeNotFound
}

func fileBinding(f evdapi.FileView) *inspection.Binding {
	return &inspection.Binding{ID: f.FileID, SHA256: f.ContentSHA256, MimeType: f.MimeType}
}

func (h *recordCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.existing, h.others, h.replay = nil, nil, false
	sc := scope(ctx)
	i, err := postgres.LoadInspection(ctx, exec, sc, h.req.ID, postgres.ForUpdate)
	if err != nil || i != nil {
		h.existing = i
		return err
	}
	if err = postgres.LockSubject(ctx, exec, sc, h.req.Subject); err != nil {
		return err
	}
	// Read after the lock: a statement started now sees what the commands that held it committed.
	h.others, err = postgres.LoadSubjectInspections(ctx, exec, sc, h.req.Subject)
	return err
}

func (h *recordCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	if i := h.existing; i != nil {
		if !d.Sees(i.OwnerActor, i.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/inspection_id", "")
		}
		if err := i.Answer(h.req.RequestHash, d.Actor); err != nil {
			return action.Outcome{}, err
		}
		h.replay = true
		return action.Outcome{ObjectID: i.ID, RowVersion: i.RowVersion, Status: i.Status}, nil
	}
	in := inspection.Inputs{Subject: h.subject, SubjectCode: h.code, Existing: h.others,
		Visible: func(i *inspection.Inspection) bool { return d.Sees(i.OwnerActor, i.OwnerTeam) }, Sources: h.sources, Report: h.report,
		Agency: h.agency, Recorder: d.Actor, Epoch: d.Epoch, Team: d.Team, Now: h.s.Clock.Now()}
	i, old, err := inspection.Record(h.req, in)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertInspection(ctx, exec, sc, i); err != nil {
		return action.Outcome{}, err
	}
	fields := map[string]any{"inspection_id": i.ID, "subject_kind": i.Subject.Ref.Kind, "subject_id": i.Subject.Ref.ID,
		"subject_line_id": i.Subject.Ref.LineID, "subject_revision": i.Subject.Revision, "subject_hash": i.Subject.Hash,
		"order_id": i.Subject.OrderID, "order_line_id": i.Subject.OrderLineID, "sku_id": i.Subject.SkuID, "spec_revision": i.Subject.SpecRevision,
		"unit_id": i.Subject.UnitID, "inspected_quantity": postgres.Text(i.Inspected), "conforming_quantity": postgres.Text(i.Conforming),
		"nonconforming_quantity": postgres.Text(i.Nonconforming), "inspector_kind": i.Inspector.Kind, "standard_source_id": i.Standard.ID,
		"standard_sha256": i.Standard.SHA256, "report_file_id": i.Report.ID, "report_sha256": i.Report.SHA256,
		"inspected_at": kernel.FormatTimestamp(i.InspectedAt)}
	if i.Inspector.Kind == inspection.InspectorAgency {
		fields["agency_party_id"], fields["agency_service_role_id"], fields["agency_role_revision"] =
			i.Inspector.PartyID, i.Inspector.ServiceRoleID, i.Inspector.RoleRevision
	}
	if old != nil {
		if err = postgres.SupersedeInspection(ctx, exec, sc, old, old.RowVersion-1); err != nil {
			return action.Outcome{}, conflict(err)
		}
		fields["supersedes_inspection_id"] = old.ID
	}
	event := action.Event{Type: api.EventInspectionRecorded, AggregateType: "Inspection", AggregateID: i.ID, AggregateVersion: i.RowVersion,
		Fields: fields}
	return action.Outcome{ObjectID: i.ID, RowVersion: i.RowVersion, Status: i.Status, Events: []action.Event{event}}, nil
}

// conflict is VERSION_CONFLICT for a compare-and-set that found another version.
func conflict(err error) error {
	if errors.Is(err, postgres.ErrConflict) {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return err
}

// RecordInspection records an inspection (inspection.record; SM-QLT-INSPECTION, QLT-INV-001, F-06,
// BT08).
func (s Service) RecordInspection(ctx context.Context, cmd api.RecordInspection) (api.InspectionReceipt, error) {
	h := &recordCommand{s: s, cmd: cmd}
	err := cmd.Validate()
	if err == nil {
		h.req = recordRequest(cmd)
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandRecordInspection, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.InspectionID,
		Reason: cmd.Reason}, cmd, err, h)
	if err != nil {
		return api.InspectionReceipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return s.inspectionReceipt(ctx, res)
}

// disposeCommand runs disposition.decide: it locks the inspection FOR UPDATE, decides the
// disposition and writes it with its items. It reads EVD before the transaction for the evidence
// it cites, nothing else upstream.
type disposeCommand struct {
	s        Service
	cmd      api.DecideDisposition
	items    []inspection.Item
	sources  map[string]string
	existing *inspection.Inspection
}

func (h *disposeCommand) Prepare(ctx context.Context) error {
	var err error
	h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *disposeCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.existing, err = postgres.LoadInspection(ctx, exec, scope(ctx), h.cmd.InspectionID, postgres.ForUpdate)
	return err
}

func (h *disposeCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	i := h.existing
	if i == nil || !d.Sees(i.OwnerActor, i.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/inspection_id", "")
	}
	from := i.RowVersion
	act := inspection.Act{By: d.Actor, Epoch: d.Epoch, At: h.s.Clock.Now()}
	if err := i.Decide(h.cmd.ExpectedVersion, h.items, h.cmd.Reason, h.cmd.EvidenceIDs, h.sources, act); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.DisposeInspection(ctx, exec, scope(ctx), i, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	items := make([]map[string]any, len(i.Disposition.Items))
	for n, it := range i.Disposition.Items {
		items[n] = map[string]any{"action": it.Action, "quantity": postgres.Text(it.Quantity)}
	}
	event := action.Event{Type: api.EventDispositionDecided, AggregateType: "Inspection", AggregateID: i.ID, AggregateVersion: i.RowVersion,
		Fields: map[string]any{"inspection_id": i.ID, "subject_kind": i.Subject.Ref.Kind, "subject_id": i.Subject.Ref.ID,
			"subject_line_id": i.Subject.Ref.LineID, "unit_id": i.Subject.UnitID, "nonconforming_quantity": postgres.Text(i.Nonconforming),
			"items": items}}
	return action.Outcome{ObjectID: i.ID, RowVersion: i.RowVersion, Status: i.Status, Events: []action.Event{event}}, nil
}

// DecideDisposition disposes an inspection's nonconforming quantity (disposition.decide;
// SM-QLT-INSPECTION; UL QLT-Disposition: it creates no supply, credit or releasable quantity).
func (s Service) DecideDisposition(ctx context.Context, cmd api.DecideDisposition) (api.InspectionReceipt, error) {
	h := &disposeCommand{s: s, cmd: cmd}
	for _, it := range cmd.Items {
		q, _ := api.Quantity(it.Quantity)
		h.items = append(h.items, inspection.Item{Action: it.Action, Quantity: q})
	}
	slices.SortFunc(h.items, func(a, b inspection.Item) int { return cmp.Compare(a.Action, b.Action) })
	res, err := s.run(ctx, action.Command{Name: api.CommandDecideDisposition, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.InspectionID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.InspectionReceipt{}, err
	}
	return s.inspectionReceipt(ctx, res)
}

// inspectionReceipt completes res with what the stored inspection binds.
func (s Service) inspectionReceipt(ctx context.Context, res api.Receipt) (api.InspectionReceipt, error) {
	out := api.InspectionReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		i, err := postgres.LoadInspection(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || i == nil {
			return err
		}
		out.Subject, out.SupersedesInspectionID, out.UnitID = apiRef(i.Subject.Ref), i.Supersedes, i.Subject.UnitID
		out.InspectedQuantity, out.ConformingQuantity = postgres.Text(i.Inspected), postgres.Text(i.Conforming)
		out.NonconformingQuantity = postgres.Text(i.Nonconforming)
		return nil
	})
	return out, err
}
