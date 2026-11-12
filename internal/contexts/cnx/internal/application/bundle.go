package application

import (
	"context"
	"errors"
	"slices"
	"strconv"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	dcl "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/declaration"
	db "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/documentbundle"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Document bundles and external reconciliation (WBS S5-05a; CNX-AGG-DocumentBundle, CNX-INV-008;
// CN-R11, CN-R13; CN-AT14 bundle half). Upstreams, read before CNX's transaction: FUL (R36, the
// export lot), EVD (R54, the attachments and the evidence of a reconciliation), STL (R38,
// FundRecordRef v1 of a fund_record requirement or subject) and POL (R50, the currency revision of
// an amount). CNX's own declarations and tax refund cases are read inside the transaction. The
// bundle root is locked FOR UPDATE (the handler's Lock), then the ledger takes the authority guard
// FOR SHARE; a check reads the lot's declaration without a lock (it records what it saw).

// Rules of the bundle commands: humanOnlyCommands name external_reconciliation.resolve (tax
// specialist); document_bundle.check (documentation) admits humans and AI agents, but only a human
// states a requirement verified.
func bundleRules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandCheckBundle, Permission: api.CommandCheckBundle, ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		authorization.HumanOnly(api.CommandResolveReconciliation, api.CommandResolveReconciliation),
		{Command: api.CommandGetBundle, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandBundles, Permission: api.PermissionRead, ActorKinds: anyActor},
	}
}

func bundleRequest(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.CheckBundle:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandCheckBundle, key, c
	case api.ResolveReconciliation:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandResolveReconciliation, key, c
	}
	return "", "", nil
}

// visibleDeclaration is Visible's part for declarations and bundles, which are company-wide like
// licences: ok reports whether command is one of them.
func visibleDeclaration(ctx context.Context, exec bentopg.Executor, command, id string) (ok, visible bool, err error) {
	switch command {
	case api.CommandPrepareDeclaration, api.CommandConfirmDeclaration, api.CommandRecordSubmission, api.CommandRecordReceipt:
		c, err := postgres.LoadDeclaration(ctx, exec, scope(ctx), id, postgres.NoLock)
		return true, c != nil, err
	case api.CommandCheckBundle, api.CommandResolveReconciliation:
		b, err := postgres.LoadBundle(ctx, exec, scope(ctx), id, postgres.NoLock)
		return true, b != nil, err
	}
	return false, false, nil
}

func bundleEvidence(sources map[string]evdapi.SourceRef, ids []string, path string) ([]db.Evidence, error) {
	out := []db.Evidence{}
	for i, id := range ids {
		src, ok := sources[id]
		if !ok {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, path+"/"+strconv.Itoa(i), "No such source")
		}
		out = append(out, db.Evidence{SourceID: src.SourceID, ContentSHA256: src.ContentSHA256})
	}
	return out, nil
}

// document_bundle.check (human or AI; documentation). Before CNX's transaction: the export lot
// (FUL), every attachment (EVD) and every fund record (STL). In the transaction, after the bundle
// FOR UPDATE: the bundle's row version (VERSION_CONFLICT; 0 for a lot without one), a lot FUL does
// not resolve or whose record is not intact (REFERENCE_INVALID), a cancelled lot (INVALID_STATE), an
// attachment (REFERENCE_INVALID), a declaration that is not one of the lot's (REFERENCE_INVALID), a
// fund record STL does not resolve (REFERENCE_INVALID), then db.Check (PERMISSION_DENIED for a
// verification by an AI agent, DOCUMENT_REQUIREMENT_UNVERIFIED). DocumentBundleChecked is published.
type checkBundle struct {
	s       Service
	cmd     api.CheckBundle
	lot     *Export
	sources map[string]evdapi.SourceRef
	funds   map[string]FundRecord
	bundle  *db.Bundle
	counts  db.Counts
	checkNo int
}

func (h *checkBundle) Prepare(ctx context.Context) error {
	h.lot = nil
	ref := ExportRef{Kind: api.TargetExportLot, ID: h.cmd.ExportLotID}
	exports, err := h.s.Upstream.Exports.ResolveExports(ctx, []ExportRef{ref})
	if err != nil {
		return err
	}
	if x, ok := exports[ref]; ok {
		h.lot = &x
	}
	var ids, funds []string
	for _, r := range h.cmd.Requirements {
		ids = append(ids, r.EvidenceIDs...)
		for _, f := range r.FundRecordIDs {
			if !slices.Contains(funds, f) {
				funds = append(funds, f)
			}
		}
	}
	if h.sources, err = h.s.sources(ctx, ids); err != nil {
		return err
	}
	h.funds = map[string]FundRecord{}
	if len(funds) > 0 {
		h.funds, err = h.s.Upstream.Funds.ResolveFundRecords(ctx, funds)
	}
	return err
}

func (h *checkBundle) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.bundle, err = postgres.BundleOfLot(ctx, exec, scope(ctx), h.cmd.ExportLotID, postgres.ForUpdate)
	return err
}

func (h *checkBundle) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd, b := scope(ctx), h.cmd, h.bundle
	const lotPath = "/args/export_lot_id"
	if err := db.CheckVersion(b, cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	x := h.lot
	switch {
	case x == nil:
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, lotPath, "FUL resolves no such export lot")
	case x.Code != "":
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, lotPath, "FUL's record of the export lot is not intact")
	case x.Status == ExportLotCancelled:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, lotPath, "The export lot's shipment is cancelled")
	}
	var requirements []db.Requirement
	for i, r := range cmd.Requirements {
		p := "/args/requirements/" + strconv.Itoa(i)
		req := db.Requirement{RequirementID: r.RequirementID, Kind: r.Kind, Version: r.Version, Status: r.Status,
			Owner: db.Owner{Kind: r.Owner.Kind, ID: r.Owner.ID}, DueOn: r.DueOn, Basis: r.Basis, Note: r.Note}
		var err error
		if req.Evidence, err = bundleEvidence(h.sources, r.EvidenceIDs, p+"/evidence_ids"); err != nil {
			return action.Outcome{}, err
		}
		if r.DeclarationID != "" {
			c, err := postgres.LoadDeclaration(ctx, exec, sc, r.DeclarationID, postgres.NoLock)
			if err != nil {
				return action.Outcome{}, err
			}
			if c == nil || c.ExportLotID != cmd.ExportLotID {
				return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, p+"/declaration_id", "The export lot has no such declaration")
			}
			req.Declaration = &db.DeclarationRef{DeclarationID: c.ID, Status: c.Status, DeclarationNumber: c.DeclarationNumber,
				Exported: dcl.Exported(c.Status)}
			if !c.ExportedAt.IsZero() {
				req.Declaration.ExportedAt = kernel.FormatTimestamp(c.ExportedAt)
			}
		}
		for j, id := range r.FundRecordIDs {
			f, ok := h.funds[id]
			if !ok {
				return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, p+"/fund_record_ids/"+strconv.Itoa(j), "STL resolves no such fund record")
			}
			req.FundRecords = append(req.FundRecords, db.FundRef{FundRecordID: f.ID, Kind: f.Kind, Direction: f.Direction, Currency: f.Currency,
				ReceivedMinor: f.ReceivedMinor, Label: db.CommercialAllocation})
		}
		requirements = append(requirements, req)
	}
	counts, err := db.Check(db.CheckInput{Requirements: requirements, ExpectComplete: cmd.ExpectComplete, Human: d.Actor.Kind == kernel.ActorHuman})
	if err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	if b == nil {
		b = &db.Bundle{ID: action.NewID("dbn", now), ExportLotID: cmd.ExportLotID, OrderID: x.OrderID, Status: counts.Status(), Checks: 1,
			RowVersion: 1, OpenedBy: d.Actor, Epoch: d.Epoch, OpenedAt: now}
		err = postgres.InsertBundle(ctx, exec, sc, b)
	} else {
		from := b.RowVersion
		b.RowVersion++
		b.Checks++
		b.Status = counts.Status()
		err = postgres.SaveBundle(ctx, exec, sc, b, from, now)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	sealed, err := db.Seal(db.CheckDocument{BundleID: b.ID, CheckNo: b.Checks, ExportLotID: b.ExportLotID, LotVersion: x.Version, OrderID: x.OrderID,
		Requirements: requirements, Status: b.Status, Missing: counts.Missing, Present: counts.Present, Verified: counts.Verified,
		CheckedBy: db.Owner{Kind: string(d.Actor.Kind), ID: d.Actor.ID}, AuthorityEpoch: d.Epoch, CheckedAt: kernel.FormatTimestamp(now)})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertBundleCheck(ctx, exec, sc, sealed, d.Actor, d.Epoch, now); err != nil {
		return action.Outcome{}, err
	}
	h.counts, h.checkNo = counts, b.Checks
	missing, present := counts.MissingIDs, counts.PresentIDs
	if missing == nil {
		missing = []string{}
	}
	if present == nil {
		present = []string{}
	}
	return action.Outcome{ObjectID: b.ID, RowVersion: b.RowVersion, Status: b.Status, Events: []action.Event{{
		Type: api.EventDocumentBundleChecked, AggregateType: "DocumentBundle", AggregateID: b.ID, AggregateVersion: b.RowVersion,
		Fields: map[string]any{"bundle_id": b.ID, "export_lot_id": b.ExportLotID, "order_id": b.OrderID, "check_no": b.Checks, "status": b.Status,
			"missing": missing, "unverified": present, "verified": counts.Verified, "document_sha256": sealed.SHA256}}}}, nil
}

// CheckBundle states a bundle's checklist (document_bundle.check).
func (s Service) CheckBundle(ctx context.Context, cmd api.CheckBundle) (api.BundleReceipt, error) {
	h := &checkBundle{s: s, cmd: cmd}
	out, err := s.runBundle(ctx, action.Command{Name: api.CommandCheckBundle, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ExportLotID},
		cmd, cmd.Validate(), h)
	if err == nil && !out.Replayed {
		out.CheckNo, out.Missing, out.Unverified = h.checkNo, h.counts.MissingIDs, h.counts.PresentIDs
	}
	return out, err
}

// external_reconciliation.resolve (human only; tax specialist). Before CNX's transaction: the
// currency revision of an amount (POL), the evidence (EVD) and a fund record subject (STL). In the
// transaction, after the bundle FOR UPDATE: NOT_FOUND, VERSION_CONFLICT, the reconciliation ID taken
// (BUSINESS_KEY_CONFLICT), a subject that is not the bundle's lot or one of its declarations, tax
// refund cases or requirements, or a fund record STL does not resolve (REFERENCE_INVALID), the
// currency (REFERENCE_INVALID, POL's verdict), values with more digits than its minor units
// (INVALID_KNOWN_VALUE), the evidence (REFERENCE_INVALID), an approved adjustment without a
// difference (INVALID_KNOWN_VALUE). Nothing is published (the Context Map names no fact for it).
type resolveReconciliation struct {
	s        Service
	cmd      api.ResolveReconciliation
	currency *polapi.ResolvedConfig
	sources  map[string]evdapi.SourceRef
	funds    map[string]FundRecord
	bundle   *db.Bundle
}

func (h *resolveReconciliation) Prepare(ctx context.Context) error {
	var err error
	h.currency, h.funds = nil, map[string]FundRecord{}
	if c := h.cmd.Currency; c != nil {
		if h.currency, err = h.s.resolveCurrency(ctx, *c); err != nil {
			return err
		}
	}
	if h.sources, err = h.s.sources(ctx, h.cmd.External.EvidenceIDs); err != nil {
		return err
	}
	if h.cmd.Subject.Kind == api.SubjectFundRecord {
		h.funds, err = h.s.Upstream.Funds.ResolveFundRecords(ctx, []string{h.cmd.Subject.ID})
	}
	return err
}

func (h *resolveReconciliation) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.bundle, err = postgres.LoadBundle(ctx, exec, scope(ctx), h.cmd.BundleID, postgres.ForUpdate)
	return err
}

func (h *resolveReconciliation) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd, b := scope(ctx), h.cmd, h.bundle
	if b == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/bundle_id", "")
	}
	if err := db.CheckVersion(b, cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	taken, err := postgres.ReconciliationExists(ctx, exec, sc, cmd.ReconciliationID)
	if err != nil {
		return action.Outcome{}, err
	}
	if taken {
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/reconciliation_id", "The reconciliation ID is taken")
	}
	if err = h.subject(ctx, exec, sc, b); err != nil {
		return action.Outcome{}, err
	}
	r := &db.Reconciliation{ID: cmd.ReconciliationID, BundleID: b.ID, No: b.Reconciliations + 1, SubjectKind: cmd.Subject.Kind, SubjectID: cmd.Subject.ID,
		Measure: cmd.Measure, Unit: cmd.Unit, ExternalSource: cmd.External.Source, InternalSource: cmd.Internal.Source,
		ResponsibilityKind: cmd.Responsibility.Kind, ResponsibilityPartyID: cmd.Responsibility.PartyID, ResponsibilityNote: cmd.Responsibility.Note,
		Adjustment: cmd.Adjustment.Kind, AdjustmentNote: cmd.Adjustment.Note, Reason: cmd.Reason, By: d.Actor, Epoch: d.Epoch,
		At: kernel.Instant(h.s.Clock.Now())}
	if cmd.Measure == api.MeasureAmount {
		cur, err := currencyOf(h.currency, "/args/currency")
		if err != nil {
			return action.Outcome{}, err
		}
		kc, err := cur.Kernel()
		if err != nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/currency", "")
		}
		r.Unit = kc.Code()
		for _, x := range []struct {
			s, path string
			to      *kernel.Decimal
		}{{cmd.External.Value, "/args/external/value", &r.External}, {cmd.Internal.Value, "/args/internal/value", &r.Internal}} {
			m, err := kernel.ParseAmount(kc, x.s)
			if err != nil {
				return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, x.path, "More digits than "+kc.Code()+" has minor units")
			}
			*x.to = m.Decimal()
		}
	} else {
		r.External, _ = kernel.ParseDecimal(cmd.External.Value, kernel.QuantityFormat)
		r.Internal, _ = kernel.ParseDecimal(cmd.Internal.Value, kernel.QuantityFormat)
	}
	if r.Evidence, err = bundleEvidence(h.sources, cmd.External.EvidenceIDs, "/args/external/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	if err = db.Reconcile(r); err != nil {
		return action.Outcome{}, err
	}
	from := b.RowVersion
	b.RowVersion++
	b.Reconciliations++
	if err = postgres.SaveBundle(ctx, exec, sc, b, from, r.At); err == nil {
		err = postgres.InsertReconciliation(ctx, exec, sc, r)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: b.ID, RowVersion: b.RowVersion, Status: b.Status}, nil
}

// subject checks that the reconciliation's subject belongs to bundle b.
func (h *resolveReconciliation) subject(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, b *db.Bundle) error {
	const path = "/args/subject/id"
	s := h.cmd.Subject
	switch s.Kind {
	case api.SubjectExportLot:
		if s.ID == b.ExportLotID {
			return nil
		}
	case api.SubjectDeclaration:
		c, err := postgres.LoadDeclaration(ctx, exec, sc, s.ID, postgres.NoLock)
		if err != nil || (c != nil && c.ExportLotID == b.ExportLotID) {
			return err
		}
	case api.SubjectTaxRefundCase:
		c, err := postgres.LoadCase(ctx, exec, sc, s.ID, postgres.NoLock)
		if err != nil || (c != nil && c.ExportLotID == b.ExportLotID) {
			return err
		}
	case api.SubjectFundRecord:
		if _, ok := h.funds[s.ID]; ok {
			return nil
		}
	case api.SubjectDocument:
		checks, err := postgres.BundleChecks(ctx, exec, sc, b.ID)
		if err != nil {
			return err
		}
		if n := len(checks); n > 0 && slices.ContainsFunc(checks[n-1].Requirements, func(r db.Requirement) bool { return r.RequirementID == s.ID }) {
			return nil
		}
	}
	return kernel.Reject(kernel.CodeReferenceInvalid, path, "The bundle has no such "+s.Kind)
}

// ResolveReconciliation records an external reconciliation (external_reconciliation.resolve).
func (s Service) ResolveReconciliation(ctx context.Context, cmd api.ResolveReconciliation) (api.BundleReceipt, error) {
	h := &resolveReconciliation{s: s, cmd: cmd}
	out, err := s.runBundle(ctx, action.Command{Name: api.CommandResolveReconciliation, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.BundleID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	out.ReconciliationID = cmd.ReconciliationID
	return out, err
}

func (s Service) runBundle(ctx context.Context, cmd action.Command, req api.Command, validation error, h action.Handler) (api.BundleReceipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return api.BundleReceipt{}, errors.Join(validation, err)
			}
		}
		return api.BundleReceipt{}, validation
	}
	_, _, args := request(req)
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.BundleReceipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	if err != nil {
		return api.BundleReceipt{}, err
	}
	out := api.BundleReceipt{Receipt: api.Receipt(res), BundleID: res.ObjectID}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		b, err := postgres.LoadBundle(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || b == nil {
			return err
		}
		out.ExportLotID, out.CheckNo = b.ExportLotID, b.Checks
		return nil
	})
	return out, err
}
