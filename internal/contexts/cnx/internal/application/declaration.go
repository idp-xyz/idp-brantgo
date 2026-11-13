package application

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	dcl "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/declaration"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Customs declarations (WBS S5-05a; CNX-AGG-DeclarationCase, SM-CNX-DECLARATION, CNX-INV-003,
// CNX-INV-006; CN-R05, CN-R07; CN-AT02, CN-AT03, CN-AT06, CN-AT13; S5 decisions E-1, D-17).
// Upstreams, all read before CNX's transaction: FUL (R36, the export lot through the port Exports),
// TRD (R46, the order and its S/C and PI through the port Orders), CAT (R39, the declaration
// profiles through the port Profiles), POL (R50, unit_conversion revisions with
// LookupUnitConversion) and EVD (R54, the sources of confirmations, submissions and receipts).
// None carries a guard: a version records the lot version, order revision and hash, profile
// revisions and hashes and conversion revisions and hashes it read, and confirmation asks CAT again.
// Inside the transaction the case root is locked FOR UPDATE (the handler's Lock), then the ledger
// takes the authority guard FOR SHARE.

// Rules of the declaration commands (humanOnlyCommands: confirm, record_submission and
// record_receipt; the documentation template holds the four): prepare is a candidate's path for
// humans and AI agents alike.
func declarationRules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandPrepareDeclaration, Permission: api.CommandPrepareDeclaration, ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		authorization.HumanOnly(api.CommandConfirmDeclaration, api.CommandConfirmDeclaration),
		authorization.HumanOnly(api.CommandRecordSubmission, api.CommandRecordSubmission),
		authorization.HumanOnly(api.CommandRecordReceipt, api.CommandRecordReceipt),
		{Command: api.CommandGetDeclaration, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandDeclarations, Permission: api.PermissionRead, ActorKinds: anyActor},
	}
}

// declarationRequest is request's part for the declaration and document bundle commands.
func declarationRequest(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.PrepareDeclaration:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandPrepareDeclaration, key, c
	case api.ConfirmDeclaration:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandConfirmDeclaration, key, c
	case api.RecordSubmission:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRecordSubmission, key, c
	case api.RecordReceipt:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRecordReceipt, key, c
	}
	return bundleRequest(cmd)
}

// evidenceOf turns the requested evidence IDs into the sources EVD resolved, REFERENCE_INVALID at
// path/i for one it did not.
func declarationEvidence(sources map[string]evdapi.SourceRef, ids []string, path string) ([]dcl.Evidence, error) {
	out := []dcl.Evidence{}
	for i, id := range ids {
		src, ok := sources[id]
		if !ok {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, path+"/"+strconv.Itoa(i), "No such source")
		}
		out = append(out, dcl.Evidence{SourceID: src.SourceID, ContentSHA256: src.ContentSHA256})
	}
	return out, nil
}

// declaration_case.prepare (human or AI; documentation). Before CNX's transaction: the export lot
// (FUL), the order (TRD), the declaration profile of every lot line's SKU specification under the
// code table (CAT) and the unit conversions the request names (POL). In the transaction, after the
// case FOR UPDATE (a revision) or the lot's latest case FOR UPDATE (a new case), the refusals in
// this order: a case the company lacks (NOT_FOUND), its row version (VERSION_CONFLICT), a case that
// is no longer a draft (INVALID_STATE), another lot or code table than the case's
// (INVALID_KNOWN_VALUE); for a new case, a lot whose last case is neither rejected nor withdrawn
// (BUSINESS_KEY_CONFLICT); a lot FUL does not resolve or whose record is not intact
// (REFERENCE_INVALID), a cancelled lot (INVALID_STATE); an order TRD does not show or whose sealed
// revision is not intact (REFERENCE_INVALID), an order TRD moved to another revision or hash than
// the lot ships (REFERENCE_STALE, R46's risk "报关资料引用旧订单修订"); then the item mapping
// (dcl.Prepare). Nothing is published.
type prepareDeclaration struct {
	s           Service
	cmd         api.PrepareDeclaration
	lot         *Export
	order       *Order
	profiles    map[ProfileRef]dcl.Profile
	conversions map[string]*dcl.Conversion
	refused     map[string]error
	declaration *dcl.Case
}

func (h *prepareDeclaration) Prepare(ctx context.Context) error {
	h.lot, h.order, h.profiles, h.conversions, h.refused = nil, nil, nil, map[string]*dcl.Conversion{}, map[string]error{}
	ref := ExportRef{Kind: api.TargetExportLot, ID: h.cmd.ExportLotID}
	exports, err := h.s.Upstream.Exports.ResolveExports(ctx, []ExportRef{ref})
	if err != nil {
		return err
	}
	x, ok := exports[ref]
	if !ok || x.Code != "" {
		if ok {
			h.lot = &x
		}
		return nil
	}
	h.lot = &x
	orders, err := h.s.Upstream.Orders.ResolveOrders(ctx, []string{x.OrderID})
	if err != nil {
		return err
	}
	if o, ok := orders[x.OrderID]; ok {
		h.order = &o
	}
	var keys []ProfileRef
	lines := map[string]ExportLine{}
	for _, l := range x.Lines {
		lines[l.LineID] = l
		k := ProfileRef{SkuID: l.SkuID, SpecRevision: l.SpecRevision, CodeTableVersion: h.cmd.CodeTableVersion}
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	if h.profiles, err = h.s.Upstream.Profiles.ResolveDeclarationProfiles(ctx, keys); err != nil {
		return err
	}
	at := h.s.Clock.Now()
	for _, it := range h.cmd.Items {
		for _, li := range it.Lines {
			l, ok := lines[li.LineID]
			if li.UnitConversion == nil || !ok {
				continue
			}
			q, err := h.s.Upstream.Configs.LookupUnitConversion(ctx, polapi.UnitQuery{Conversion: polapi.ConfigRef{Kind: polapi.KindUnitConversion,
				ConfigID: li.UnitConversion.ID, Revision: li.UnitConversion.Revision}, SkuID: l.SkuID, SpecRevision: l.SpecRevision, At: at})
			if _, ok := kernel.AsRejection(err); ok {
				h.refused[li.LineID] = err
				continue
			}
			if err != nil {
				return err
			}
			h.conversions[li.LineID] = &dcl.Conversion{ConfigID: q.Conversion.ConfigID, Revision: q.Conversion.Revision, ContentHash: q.ContentHash,
				From: q.From, To: q.To, Factor: q.Factor}
		}
	}
	return nil
}

func (h *prepareDeclaration) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	if h.cmd.DeclarationID != "" {
		h.declaration, err = postgres.LoadDeclaration(ctx, exec, scope(ctx), h.cmd.DeclarationID, postgres.ForUpdate)
	} else {
		h.declaration, err = postgres.DeclarationOfLot(ctx, exec, scope(ctx), h.cmd.ExportLotID, postgres.ForUpdate)
	}
	return err
}

func (h *prepareDeclaration) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd, c := scope(ctx), h.cmd, h.declaration
	const lotPath = "/args/export_lot_id"
	revise := cmd.DeclarationID != ""
	if revise {
		if c == nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/declaration_id", "")
		}
		if err := dcl.CheckVersion(c, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if err := dcl.CheckDraft(c); err != nil {
			return action.Outcome{}, err
		}
		if c.ExportLotID != cmd.ExportLotID {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, lotPath, "The declaration is of export lot "+c.ExportLotID)
		}
		if c.CodeTableVersion != cmd.CodeTableVersion {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/code_table_version", "The declaration is under "+c.CodeTableVersion)
		}
	} else if c != nil && !dcl.Terminal(c.Status) {
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, lotPath, "The export lot's declaration "+c.ID+" is "+c.Status)
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
	o := h.order
	switch {
	case o == nil:
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, lotPath, "TRD shows no order "+x.OrderID)
	case o.Code != "":
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, lotPath, "TRD's sealed revision of order "+x.OrderID+" is not intact")
	case o.Binding.OrderRevision != x.OrderRevision || (x.OrderHash != "" && o.Binding.OrderHash != x.OrderHash):
		return action.Outcome{}, api.Reject(api.CodeReferenceStale, lotPath, "The lot ships revision "+strconv.FormatInt(x.OrderRevision, 10)+
			" of the order; TRD's is "+strconv.FormatInt(o.Binding.OrderRevision, 10))
	}
	in := dcl.PrepareInput{Profiles: map[string]dcl.Profile{}}
	for _, l := range x.Lines {
		in.Lines = append(in.Lines, dcl.LotLine{LineID: l.LineID, ShipmentID: l.ShipmentID, OrderLineID: l.OrderLineID, SkuID: l.SkuID,
			SpecRevision: l.SpecRevision, Quantity: l.Quantity, Unit: l.Unit, Amount: l.Amount})
		if p, ok := h.profiles[ProfileRef{SkuID: l.SkuID, SpecRevision: l.SpecRevision, CodeTableVersion: cmd.CodeTableVersion}]; ok {
			in.Profiles[l.LineID] = p
		}
	}
	for _, it := range cmd.Items {
		item := dcl.ItemInput{ItemNo: it.ItemNo}
		for _, li := range it.Lines {
			item.Lines = append(item.Lines, dcl.LineInput{LineID: li.LineID, Conversion: h.conversions[li.LineID], Err: h.refused[li.LineID]})
			if li.UnitConversion != nil && h.conversions[li.LineID] == nil && h.refused[li.LineID] == nil &&
				slices.ContainsFunc(x.Lines, func(l ExportLine) bool { return l.LineID == li.LineID }) {
				// POL answered nothing for the revision: as not configured.
				item.Lines[len(item.Lines)-1].Err = kernel.Reject(kernel.CodeReferenceInvalid, "/conversion", "")
			}
		}
		in.Items = append(in.Items, item)
	}
	items, mapping, err := dcl.Prepare(in)
	if err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	if revise {
		c.Version++
		c.Mapping = mapping
	} else {
		attempt := 1
		if c != nil {
			attempt = c.Attempt + 1
		}
		shipment := ""
		if len(x.Lines) > 0 {
			shipment = x.Lines[0].ShipmentID
		}
		c = &dcl.Case{ID: action.NewID("dcl", now), ExportLotID: cmd.ExportLotID, Attempt: attempt, ShipmentID: shipment, OrderID: x.OrderID,
			OrderRevision: x.OrderRevision, OrderHash: o.Binding.OrderHash, CodeTableVersion: cmd.CodeTableVersion, Status: dcl.Draft, Version: 1,
			Mapping: mapping, RowVersion: 1, OpenedBy: d.Actor, Epoch: d.Epoch, OpenedAt: now}
	}
	doc := dcl.Document{DeclarationID: c.ID, Version: c.Version, Kind: dcl.VersionPrepared,
		ExportLot: dcl.DocLot{ExportLotID: x.Ref.ID, Version: x.Version, Status: x.Status, ShipmentID: c.ShipmentID, OrderID: x.OrderID,
			OrderRevision: x.OrderRevision, Currency: x.Value.Currency().Code(), ValueMinor: x.Value.MinorString()},
		Order:            dcl.DocOrder{OrderID: o.Binding.OrderID, OrderRevision: o.Binding.OrderRevision, OrderHash: o.Binding.OrderHash, Status: o.Status},
		CodeTableVersion: cmd.CodeTableVersion, Mapping: mapping, Items: dcl.DocItems(items),
		PreparedBy: dcl.DocActor{Kind: string(d.Actor.Kind), ID: d.Actor.ID}, AuthorityEpoch: d.Epoch, PreparedAt: kernel.FormatTimestamp(now)}
	for _, sd := range o.Documents {
		doc.Order.Documents = append(doc.Order.Documents, dcl.DocSalesRecord{Kind: sd.Kind, Number: sd.Number, OrderRevision: sd.OrderRevision,
			DocumentSHA256: sd.DocumentSHA256, Current: sd.Current, Signatures: sd.Signatures})
	}
	v, err := dcl.NewVersion(doc)
	if err != nil {
		return action.Outcome{}, err
	}
	if revise {
		from := c.RowVersion
		c.RowVersion++
		err = postgres.SaveDeclaration(ctx, exec, sc, c, from, now)
	} else {
		err = postgres.InsertDeclaration(ctx, exec, sc, c)
	}
	if err == nil {
		err = postgres.InsertVersion(ctx, exec, sc, v, d.Actor, d.Epoch, now)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: c.ID, RowVersion: c.RowVersion, Status: c.Status}, nil
}

// PrepareDeclaration prepares or revises a declaration (declaration_case.prepare).
func (s Service) PrepareDeclaration(ctx context.Context, cmd api.PrepareDeclaration) (api.DeclarationReceipt, error) {
	h := &prepareDeclaration{s: s, cmd: cmd}
	return s.runDeclaration(ctx, action.Command{Name: api.CommandPrepareDeclaration, IdempotencyKey: cmd.IdempotencyKey,
		ObjectID: cmd.DeclarationID}, cmd, cmd.Validate(), h, "")
}

// declaration_case.confirm (human only; documentation). Before CNX's transaction: the case's
// current version (without a lock), the profiles verified now of its lines (CAT) and the evidence
// (EVD). In the transaction, after the case FOR UPDATE: NOT_FOUND, VERSION_CONFLICT, then
// dcl.CheckConfirm (INVALID_STATE, REFERENCE_STALE, DECLARATION_MAPPING_UNVERIFIED), a line whose
// profile CAT verified anew or no longer answers verified (REFERENCE_STALE: prepare again), the
// evidence (REFERENCE_INVALID). Nothing is published.
type confirmDeclaration struct {
	s           Service
	cmd         api.ConfirmDeclaration
	profiles    map[ProfileRef]dcl.Profile
	sources     map[string]evdapi.SourceRef
	declaration *dcl.Case
}

func (h *confirmDeclaration) Prepare(ctx context.Context) error {
	h.profiles = nil
	var err error
	if h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs); err != nil {
		return err
	}
	var v *dcl.Version
	if err = h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		c, err := postgres.LoadDeclaration(ctx, exec, scope(ctx), h.cmd.DeclarationID, postgres.NoLock)
		if err != nil || c == nil {
			return err
		}
		vs, err := postgres.Versions(ctx, exec, scope(ctx), c.ID, c.Version)
		if err == nil && len(vs) == 1 {
			v = vs[0]
		}
		return err
	}); err != nil || v == nil {
		return err
	}
	h.profiles, err = h.s.Upstream.Profiles.ResolveDeclarationProfiles(ctx, versionProfileRefs(v))
	return err
}

func versionProfileRefs(v *dcl.Version) []ProfileRef {
	var keys []ProfileRef
	for _, it := range v.Items {
		for _, l := range it.Lines {
			k := ProfileRef{SkuID: l.SkuID, SpecRevision: l.SpecRevision, CodeTableVersion: v.CodeTableVersion}
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

func (h *confirmDeclaration) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.declaration, err = postgres.LoadDeclaration(ctx, exec, scope(ctx), h.cmd.DeclarationID, postgres.ForUpdate)
	return err
}

func (h *confirmDeclaration) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd, c := scope(ctx), h.cmd, h.declaration
	if c == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/declaration_id", "")
	}
	if err := dcl.CheckVersion(c, cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	vs, err := postgres.Versions(ctx, exec, sc, c.ID, c.Version)
	if err != nil {
		return action.Outcome{}, err
	}
	if len(vs) != 1 {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/version", "The declaration has no such version")
	}
	v := vs[0]
	if err = dcl.CheckConfirm(c, cmd.Version, dcl.Items(v.Document)); err != nil {
		return action.Outcome{}, err
	}
	for _, it := range v.Items {
		for _, l := range it.Lines {
			p, ok := h.profiles[ProfileRef{SkuID: l.SkuID, SpecRevision: l.SpecRevision, CodeTableVersion: v.CodeTableVersion}]
			if !ok || !p.Verified || p.Revision != l.Profile.Revision || p.ContentHash != l.Profile.ContentHash {
				return action.Outcome{}, api.Reject(api.CodeReferenceStale, "/args/version", "The declaration profile of line "+l.LineID+
					" is no longer the one verified now: prepare the declaration again")
			}
		}
	}
	evidence, err := declarationEvidence(h.sources, cmd.EvidenceIDs, "/args/evidence_ids")
	if err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	from := c.RowVersion
	c.RowVersion++
	c.Status = dcl.Confirmed
	if err = postgres.InsertConfirmation(ctx, exec, sc, &dcl.Confirmation{DeclarationID: c.ID, Version: c.Version, Evidence: evidence,
		Reason: cmd.Reason, By: d.Actor, Epoch: d.Epoch, At: now}); err == nil {
		err = postgres.SaveDeclaration(ctx, exec, sc, c, from, now)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: c.ID, RowVersion: c.RowVersion, Status: c.Status}, nil
}

// ConfirmDeclaration confirms a declaration's current version (declaration_case.confirm).
func (s Service) ConfirmDeclaration(ctx context.Context, cmd api.ConfirmDeclaration) (api.DeclarationReceipt, error) {
	h := &confirmDeclaration{s: s, cmd: cmd}
	return s.runDeclaration(ctx, action.Command{Name: api.CommandConfirmDeclaration, IdempotencyKey: cmd.IdempotencyKey,
		ObjectID: cmd.DeclarationID, Reason: cmd.Reason}, cmd, cmd.Validate(), h, "")
}

// declaration_case.record_submission (human only; documentation). Before CNX's transaction: the
// evidence (EVD). In the transaction, after the case FOR UPDATE: NOT_FOUND, VERSION_CONFLICT,
// dcl.CheckSubmission (EXTERNAL_RESULT_UNKNOWN while the result is unknown, CN-AT13; INVALID_STATE
// unless confirmed), the operation reference taken (BUSINESS_KEY_CONFLICT: one business operation),
// a submission instant in the future (INVALID_KNOWN_VALUE), the evidence (REFERENCE_INVALID).
// Nothing is published. The broker's party is recorded as stated (no PTY read: no PTY→CNX
// relationship covers service providers).
type recordSubmission struct {
	s           Service
	cmd         api.RecordSubmission
	sources     map[string]evdapi.SourceRef
	declaration *dcl.Case
}

func (h *recordSubmission) Prepare(ctx context.Context) (err error) {
	h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *recordSubmission) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.declaration, err = postgres.LoadDeclaration(ctx, exec, scope(ctx), h.cmd.DeclarationID, postgres.ForUpdate)
	return err
}

func (h *recordSubmission) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd, c := scope(ctx), h.cmd, h.declaration
	if c == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/declaration_id", "")
	}
	if err := dcl.CheckVersion(c, cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	if err := dcl.CheckSubmission(c); err != nil {
		return action.Outcome{}, err
	}
	taken, err := postgres.OperationTaken(ctx, exec, sc, cmd.OperationReference)
	if err != nil {
		return action.Outcome{}, err
	}
	if taken {
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/operation_reference", "The business operation is recorded")
	}
	now := kernel.Instant(h.s.Clock.Now())
	submitted, _ := kernel.ParseTimestamp(cmd.SubmittedAt)
	if kernel.Instant(submitted).After(now) {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/submitted_at", "The submission is in the future")
	}
	evidence, err := declarationEvidence(h.sources, cmd.EvidenceIDs, "/args/evidence_ids")
	if err != nil {
		return action.Outcome{}, err
	}
	from := c.RowVersion
	c.RowVersion++
	c.Status = dcl.Submitted
	c.Submissions++
	if err = postgres.InsertSubmission(ctx, exec, sc, &dcl.Submission{DeclarationID: c.ID, No: c.Submissions, Version: c.Version,
		OperationReference: cmd.OperationReference, SubmitterKind: cmd.Submitter.Kind, SubmitterPartyID: cmd.Submitter.PartyID,
		SubmitterName: cmd.Submitter.Name, SubmittedAt: kernel.Instant(submitted), Evidence: evidence, Note: cmd.Note, By: d.Actor, Epoch: d.Epoch,
		At: now}); err == nil {
		err = postgres.SaveDeclaration(ctx, exec, sc, c, from, now)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: c.ID, RowVersion: c.RowVersion, Status: c.Status}, nil
}

// RecordSubmission records a declaration's manual submission (declaration_case.record_submission).
func (s Service) RecordSubmission(ctx context.Context, cmd api.RecordSubmission) (api.DeclarationReceipt, error) {
	h := &recordSubmission{s: s, cmd: cmd}
	return s.runDeclaration(ctx, action.Command{Name: api.CommandRecordSubmission, IdempotencyKey: cmd.IdempotencyKey,
		ObjectID: cmd.DeclarationID}, cmd, cmd.Validate(), h, "")
}

// declaration_case.record_receipt (human only, D-17; documentation). Before CNX's transaction: the
// evidence (EVD). In the transaction, after the case FOR UPDATE: NOT_FOUND, VERSION_CONFLICT, the
// receipt ID taken (BUSINESS_KEY_CONFLICT), then dcl.CheckReceipt (INVALID_STATE, then
// DECLARATION_RECEIPT_STALE for a receipt not after the result recorded, INVALID_STATE for an
// outcome that does not follow, INVALID_KNOWN_VALUE), the evidence (REFERENCE_INVALID). An amendment
// that names items makes the next version (kind amended). DeclarationReceiptRecorded is published.
type recordReceipt struct {
	s           Service
	cmd         api.RecordReceipt
	sources     map[string]evdapi.SourceRef
	declaration *dcl.Case
}

func (h *recordReceipt) Prepare(ctx context.Context) (err error) {
	h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *recordReceipt) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.declaration, err = postgres.LoadDeclaration(ctx, exec, scope(ctx), h.cmd.DeclarationID, postgres.ForUpdate)
	return err
}

func (h *recordReceipt) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd, c := scope(ctx), h.cmd, h.declaration
	if c == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/declaration_id", "")
	}
	if err := dcl.CheckVersion(c, cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	taken, err := postgres.LoadReceipt(ctx, exec, sc, cmd.ReceiptID)
	if err != nil {
		return action.Outcome{}, err
	}
	if taken != nil {
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/receipt_id", "The receipt ID is taken")
	}
	vs, err := postgres.Versions(ctx, exec, sc, c.ID, c.Version)
	if err != nil {
		return action.Outcome{}, err
	}
	if len(vs) != 1 {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/declaration_id", "The declaration has no current version")
	}
	current := vs[0]
	now := kernel.Instant(h.s.Clock.Now())
	in := dcl.ReceiptInput{Outcome: cmd.Outcome, DeclarationNumber: cmd.DeclarationNumber, At: now, AmendedItems: map[int]bool{}}
	r := &dcl.Receipt{ID: cmd.ReceiptID, DeclarationID: c.ID, No: c.Receipts + 1, Outcome: cmd.Outcome, From: c.Status,
		MappingVersion: cmd.MappingVersion, DeclarationNumber: cmd.DeclarationNumber, Reason: cmd.Reason, By: d.Actor, Epoch: d.Epoch, At: now}
	if x := cmd.External; x != nil {
		received, _ := kernel.ParseTimestamp(x.ReceivedAt)
		in.Sequence, in.ReceivedAt = x.Sequence, kernel.Instant(received)
		r.Sequence, r.StatusCode, r.OriginalText, r.ReceivedAt = x.Sequence, x.StatusCode, x.OriginalText, in.ReceivedAt
	}
	if cmd.ExportedAt != "" {
		exported, _ := kernel.ParseTimestamp(cmd.ExportedAt)
		in.ExportedAt = kernel.Instant(exported)
		r.ExportedAt = in.ExportedAt
	}
	amended := map[int]string{}
	for _, a := range cmd.AmendedItems {
		in.AmendedItems[a.ItemNo] = true
		q, _ := kernel.ParseDecimal(a.Quantity, kernel.QuantityFormat)
		amended[a.ItemNo] = q.String()
	}
	if err = dcl.CheckReceipt(c, dcl.Items(current.Document), in); err != nil {
		return action.Outcome{}, err
	}
	if !in.ReceivedAt.IsZero() && in.ReceivedAt.After(now) {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/external/received_at", "The receipt is in the future")
	}
	if r.Evidence, err = declarationEvidence(h.sources, cmd.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	from := c.RowVersion
	c.RowVersion++
	c.Apply(in)
	var v *dcl.Version
	if len(amended) > 0 {
		c.Version++
		r.Version = c.Version
		doc := current.Document
		doc.Version, doc.Kind, doc.ReceiptID = c.Version, dcl.VersionAmended, r.ID
		doc.Items, doc.Amendments = dcl.Amend(current.Items, amended)
		doc.PreparedBy, doc.AuthorityEpoch, doc.PreparedAt = dcl.DocActor{Kind: string(d.Actor.Kind), ID: d.Actor.ID}, d.Epoch, kernel.FormatTimestamp(now)
		if v, err = dcl.NewVersion(doc); err != nil {
			return action.Outcome{}, err
		}
	}
	if err = postgres.InsertReceipt(ctx, exec, sc, r); err == nil && v != nil {
		err = postgres.InsertVersion(ctx, exec, sc, v, d.Actor, d.Epoch, now)
	}
	if err == nil {
		err = postgres.SaveDeclaration(ctx, exec, sc, c, from, now)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	fields := map[string]any{"declaration_id": c.ID, "export_lot_id": c.ExportLotID, "attempt": c.Attempt, "shipment_id": c.ShipmentID,
		"order_id": c.OrderID, "order_revision": c.OrderRevision, "receipt_id": r.ID, "receipt_no": r.No, "outcome": r.Outcome, "from": r.From,
		"version": c.Version}
	if r.Sequence > 0 {
		fields["external_sequence"], fields["mapping_version"], fields["declaration_number"] = r.Sequence, r.MappingVersion, c.DeclarationNumber
	}
	if !c.ExportedAt.IsZero() {
		fields["exported_at"] = kernel.FormatTimestamp(c.ExportedAt)
	}
	return action.Outcome{ObjectID: c.ID, RowVersion: c.RowVersion, Status: c.Status, Events: []action.Event{{
		Type: api.EventDeclarationReceiptRecorded, AggregateType: "DeclarationCase", AggregateID: c.ID, AggregateVersion: c.RowVersion,
		Fields: fields}}}, nil
}

// RecordReceipt records an external result of a declaration (declaration_case.record_receipt).
func (s Service) RecordReceipt(ctx context.Context, cmd api.RecordReceipt) (api.DeclarationReceipt, error) {
	h := &recordReceipt{s: s, cmd: cmd}
	return s.runDeclaration(ctx, action.Command{Name: api.CommandRecordReceipt, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.DeclarationID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h, cmd.ReceiptID)
}

func (s Service) runDeclaration(ctx context.Context, cmd action.Command, req api.Command, validation error, h action.Handler, receipt string) (api.DeclarationReceipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return api.DeclarationReceipt{}, errors.Join(validation, err)
			}
		}
		return api.DeclarationReceipt{}, validation
	}
	_, _, args := request(req)
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.DeclarationReceipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	if err != nil {
		return api.DeclarationReceipt{}, err
	}
	out := api.DeclarationReceipt{Receipt: api.Receipt(res), DeclarationID: res.ObjectID, ReceiptID: receipt}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		c, err := postgres.LoadDeclaration(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || c == nil {
			return err
		}
		out.ExportLotID, out.Attempt, out.Version, out.Mapping, out.DeclarationNumber = c.ExportLotID, c.Attempt, c.Version, c.Mapping, c.DeclarationNumber
		out.ExportedAt = c.ExportedAt
		return nil
	})
	return out, err
}

// lotExport is what CNX's own declaration says of an export lot's export: the lot's latest case
// and the export date customs stated on it, zero while it is not released (or amended after its
// release), or was withdrawn.
type lotExport struct {
	DeclarationID string
	ExportedAt    time.Time
}

// exportOfLot reads the lot's latest declaration with lock (S5 C3a: the export date of a lot is the
// 出口日期 customs states on its declaration, a fact of CNX's own, never FUL's departure milestone,
// CN-R07; lead decision 2026-11-12).
func exportOfLot(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, lotID string, lock postgres.Lock) (lotExport, error) {
	c, err := postgres.DeclarationOfLot(ctx, exec, sc, lotID, lock)
	if err != nil || c == nil {
		return lotExport{}, err
	}
	out := lotExport{DeclarationID: c.ID}
	if dcl.Exported(c.Status) {
		out.ExportedAt = c.ExportedAt
	}
	return out, nil
}
