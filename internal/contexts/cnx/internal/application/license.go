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
	el "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/exportlicense"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Export licences (WBS S5-14; CNX-AGG-ExportLicense, CNX-INV-002, MD-44; TD-AT13). Upstreams: POL
// (R50, ConfigRef v1 of the licence value's currency revision), EVD (R54, the licence's file and the
// sources of an external result), CAT (R39, declaration profiles, for a licence by classification)
// and FUL (R36, the shipments and export lots through the port Exports), all read before CNX's
// transaction. Inside it, the licence root is locked FOR UPDATE (the handler's Lock), then the
// authority guard FOR SHARE (the ledger); no upstream guard is taken: an allocation binds the
// target version and lines hash it read, and ExportLicenseGuard v1 lets FUL re-check them.

// licenseRefusal turns an upstream's NOT_FOUND for a referenced object into REFERENCE_INVALID at
// path; other errors pass.
func licenseRefusal(err error, path string) error {
	if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "No such object")
	}
	return err
}

// export_license.register (human only; documentation). Before CNX's transaction: the currency
// revision (POL), the licence's file and, for multi_use, the sources of the licence face (EVD). In
// the transaction, the refusals in this order: the issuer's licence number taken
// (BUSINESS_KEY_CONFLICT), the currency (REFERENCE_INVALID, POL's CONFIG_NOT_ACTIVE or
// CONFIG_EXPIRED), a value with more digits than the currency's minor units (INVALID_KNOWN_VALUE),
// the file, the usage evidence (REFERENCE_INVALID). S5R-D04: the usage mode (single_use when
// omitted) and the customs office are recorded. ExportLicenseRegistered is published.
type registerLicense struct {
	s        Service
	cmd      api.RegisterLicense
	currency *polapi.ResolvedConfig
	file     evdapi.FileView
	fileErr  error
	sources  map[string]evdapi.SourceRef
}

func (h *registerLicense) Prepare(ctx context.Context) error {
	h.currency, h.fileErr = nil, nil
	ref := polapi.ConfigRef{Kind: polapi.KindCurrency, ConfigID: h.cmd.Currency.ID, Revision: h.cmd.Currency.Revision}
	resolved, err := h.s.Upstream.Configs.ResolveConfigs(ctx, []polapi.ConfigRef{ref}, h.s.Clock.Now())
	if err != nil {
		return err
	}
	if r, ok := resolved[ref]; ok {
		h.currency = &r
	}
	if h.sources, err = h.s.sources(ctx, h.cmd.UsageEvidenceIDs); err != nil {
		return err
	}
	h.file, err = h.s.Upstream.Sources.GetFile(ctx, h.cmd.FileID)
	if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
		h.fileErr = licenseRefusal(err, "/args/file_id")
		return nil
	}
	return err
}

func (h *registerLicense) Lock(context.Context, bentopg.Executor) error { return nil }

func (h *registerLicense) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd := scope(ctx), h.cmd
	taken, err := postgres.LicenseByNumber(ctx, exec, sc, cmd.Issuer, cmd.LicenseNumber)
	if err != nil {
		return action.Outcome{}, err
	}
	if taken != "" {
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/license_number", "The issuer's licence number is registered")
	}
	const curPath = "/args/currency"
	r := h.currency
	if r == nil || r.Ref.Kind != polapi.KindCurrency || r.Data.Currency == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, curPath, "")
	}
	if err = r.Check(curPath); err != nil {
		return action.Outcome{}, err
	}
	cur := el.Currency{ConfigID: r.Ref.ConfigID, Revision: r.Ref.Revision, Code: r.Data.Currency.Code, MinorUnits: r.Data.Currency.MinorUnits}
	kc, err := cur.Kernel()
	if err != nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, curPath, "")
	}
	value, err := kernel.ParseAmount(kc, cmd.LicensedValue)
	if err != nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/licensed_value", "More digits than "+kc.Code()+" has minor units")
	}
	if h.fileErr != nil {
		return action.Outcome{}, h.fileErr
	}
	if h.file.FileID != cmd.FileID {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/file_id", "")
	}
	usage, usageEvidence := cmd.UsageMode, []el.Evidence(nil)
	if usage == "" {
		usage = el.SingleUse
	}
	for i, id := range cmd.UsageEvidenceIDs {
		src, ok := h.sources[id]
		if !ok {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/usage_evidence_ids/"+strconv.Itoa(i), "No such source")
		}
		usageEvidence = append(usageEvidence, el.Evidence{SourceID: src.SourceID, ContentSHA256: src.ContentSHA256})
	}
	quantity, _ := kernel.ParseDecimal(cmd.LicensedQuantity, kernel.QuantityFormat)
	from, _ := kernel.ParseTimestamp(cmd.ValidFrom)
	until, _ := kernel.ParseTimestamp(cmd.ValidUntil)
	now := kernel.Instant(h.s.Clock.Now())
	l := &el.License{ID: action.NewID("elc", now), Number: cmd.LicenseNumber, Issuer: cmd.Issuer, Holder: cmd.Holder, Goods: goodsOf(cmd.Goods),
		Unit: kernel.UnitCode(cmd.Unit), Quantity: quantity, Currency: cur, Value: value, Destination: cmd.Destination,
		EndUserPartyID: cmd.EndUser.PartyID, EndUserName: cmd.EndUser.Name, Validity: kernel.ValidityInterval{From: kernel.Instant(from),
			Until: kernel.Instant(until)}, FileID: h.file.FileID, FileSHA256: h.file.ContentSHA256, UsageMode: usage, UsageEvidence: usageEvidence,
		CustomsOffice: cmd.CustomsOffice, RowVersion: 1, RegisteredBy: d.Actor, Epoch: d.Epoch, RegisteredAt: now}
	if err = postgres.InsertLicense(ctx, exec, sc, l); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: l.ID, RowVersion: l.RowVersion, Status: el.Registered, Events: []action.Event{{
		Type: api.EventLicenseRegistered, AggregateType: "ExportLicense", AggregateID: l.ID, AggregateVersion: l.RowVersion,
		Fields: map[string]any{"license_id": l.ID, "license_number": l.Number, "issuer": l.Issuer, "destination": l.Destination,
			"end_user_party_id": l.EndUserPartyID, "unit": string(l.Unit), "licensed_quantity": l.Quantity.String(),
			"currency": l.Currency.Code, "licensed_value_minor": l.Value.MinorString(), "valid_from": kernel.FormatTimestamp(l.Validity.From),
			"valid_until": kernel.FormatTimestamp(l.Validity.Until), "file_id": l.FileID, "file_sha256": l.FileSHA256}}}}, nil
}

func goodsOf(g api.LicensedGoods) el.Goods {
	var out el.Goods
	for _, s := range g.SkuSpecs {
		out.Specs = append(out.Specs, el.Spec{SkuID: s.SkuID, SpecRevision: s.SpecRevision})
	}
	if c := g.Classification; c != nil {
		out.CodeTable, out.CommodityCodes = c.CodeTableVersion, c.CommodityCodes
	}
	return out
}

// RegisterLicense registers an export licence (export_license.register).
func (s Service) RegisterLicense(ctx context.Context, cmd api.RegisterLicense) (api.LicenseReceipt, error) {
	h := &registerLicense{s: s, cmd: cmd}
	return s.runLicense(ctx, action.Command{Name: api.CommandRegisterLicense, IdempotencyKey: cmd.IdempotencyKey}, cmd, cmd.Validate(), h, "", "")
}

// export_license.allocate (human or AI; documentation; ES-12 step 3). Before CNX's transaction: the
// target (FUL, port Exports), the licence without a lock and, for a licence by classification, the
// declaration profiles of the target's lines under its code table (CAT, R39: the revision verified
// now). In the transaction, after the licence FOR UPDATE: a licence the company lacks (NOT_FOUND),
// the allocation ID taken (BUSINESS_KEY_CONFLICT), a target FUL does not resolve or whose record is
// not intact (REFERENCE_INVALID), one not prepared or registered or still holding an allocation
// (INVALID_STATE), a shipment that would hold its own allocation beside its export lots'
// (INVALID_STATE, S5R-D04: one licence per declaration, several per shipment through its lots), a
// live declaration of the target (confirmed or later) naming another customs office than the
// licence's (INVALID_KNOWN_VALUE, 一证一关), then the domain's EXPORT_LICENSE_EXPIRED, INVALID_STATE
// (a single_use licence live or declared), EXPORT_LICENSE_GOODS_MISMATCH, INVALID_KNOWN_VALUE and
// EXPORT_LICENSE_EXCEEDED on the usage derived from every allocation of the licence under the lock.
// The allocation records the shipment it ships, an export lot line's shipment line and where the end
// user came from (S5R-D40). ExportLicenseAllocated is published.
type allocateLicense struct {
	s        Service
	cmd      api.AllocateLicense
	target   *Export
	profiles map[ProfileRef]tcc.Profile
	license  *el.License
}

func (h *allocateLicense) Prepare(ctx context.Context) error {
	h.target, h.profiles = nil, nil
	ref := ExportRef{Kind: h.cmd.Target.Kind, ID: h.cmd.Target.ID}
	exports, err := h.s.Upstream.Exports.ResolveExports(ctx, []ExportRef{ref})
	if err != nil {
		return err
	}
	x, ok := exports[ref]
	if !ok {
		return nil
	}
	h.target = &x
	var l *el.License
	if err = h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		l, err = postgres.LoadLicense(ctx, exec, scope(ctx), h.cmd.LicenseID, postgres.NoLock)
		return err
	}); err != nil || l == nil || !l.Goods.ByClassification() {
		return err
	}
	var keys []ProfileRef
	for _, line := range x.Lines {
		k := ProfileRef{SkuID: line.SkuID, SpecRevision: line.SpecRevision, CodeTableVersion: l.Goods.CodeTable}
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	h.profiles, err = h.s.Upstream.Profiles.ResolveProfiles(ctx, keys)
	return err
}

func (h *allocateLicense) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.license, err = postgres.LoadLicense(ctx, exec, scope(ctx), h.cmd.LicenseID, postgres.ForUpdate)
	return err
}

func (h *allocateLicense) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd, l := scope(ctx), h.cmd, h.license
	if l == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/license_id", "")
	}
	owner, err := postgres.AllocationOwner(ctx, exec, sc, cmd.AllocationID)
	if err != nil {
		return action.Outcome{}, err
	}
	if owner != "" {
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/allocation_id", "The allocation ID is taken")
	}
	const tPath = "/args/target/id"
	x := h.target
	switch {
	case x == nil:
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, tPath, "FUL resolves no such "+cmd.Target.Kind)
	case x.Code != "":
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, tPath, "FUL's record of the "+cmd.Target.Kind+" is not intact")
	case cmd.Target.Kind == api.TargetShipment && x.Status != ShipmentPrepared, cmd.Target.Kind == api.TargetExportLot && x.Status != ExportLotRegistered:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, tPath, "The "+cmd.Target.Kind+" is "+x.Status)
	}
	attempt, held, err := postgres.TargetAttempt(ctx, exec, sc, cmd.Target.Kind, cmd.Target.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	if held {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, tPath, "The "+cmd.Target.Kind+" holds an allocation")
	}
	shipmentID := cmd.Target.ID
	if cmd.Target.Kind == api.TargetExportLot {
		if shipmentID = ""; len(x.Lines) > 0 {
			shipmentID = x.Lines[0].ShipmentID
		}
		if shipmentID == "" {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, tPath, "FUL's export lot names no shipment")
		}
	}
	mixed, err := postgres.MixedAllocation(ctx, exec, sc, cmd.Target.Kind, cmd.Target.ID, shipmentID)
	if err != nil {
		return action.Outcome{}, err
	}
	if mixed != "" {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, tPath, "Shipment "+shipmentID+" is licensed through allocation "+mixed+
			": a shipment is licensed as a whole or through its export lots, one licence per declaration (S5R-D04)")
	}
	if err = declaredOffices(ctx, exec, sc, l, cmd.Target, tPath); err != nil {
		return action.Outcome{}, err
	}
	allocations, err := postgres.Allocations(ctx, exec, sc, l, "")
	if err != nil {
		return action.Outcome{}, err
	}
	declared, err := postgres.LicenseDeclared(ctx, exec, sc, l.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	destination := x.Destination
	if destination == "" {
		if destination, err = evaluatedDestination(ctx, exec, sc, x.OrderID, x.OrderRevision); err != nil {
			return action.Outcome{}, err
		}
	}
	source := x.EndUserSource
	if source == "" {
		source = api.EndUserConsignee
	}
	t := el.Target{Kind: cmd.Target.Kind, ID: cmd.Target.ID, Version: x.Version, OrderID: x.OrderID, OrderRevision: x.OrderRevision,
		Destination: destination, EndUserPartyID: x.EndUserPartyID, EndUserSource: source, ShipmentID: shipmentID, Value: x.Value}
	for _, line := range x.Lines {
		ln := el.Line{LineID: line.LineID, SkuID: line.SkuID, SpecRevision: line.SpecRevision, Quantity: line.Quantity, Unit: line.Unit}
		if cmd.Target.Kind == api.TargetExportLot {
			ln.ShipmentLineID = line.ShipmentLineID
		}
		if l.Goods.ByClassification() {
			if p, ok := h.profiles[ProfileRef{SkuID: line.SkuID, SpecRevision: line.SpecRevision, CodeTableVersion: l.Goods.CodeTable}]; ok && p.Verified {
				ln.CommodityCode = p.CommodityCode
			}
		}
		t.Lines = append(t.Lines, ln)
	}
	now := kernel.Instant(h.s.Clock.Now())
	a, err := el.Allocate(l, allocations, el.AllocateInput{ID: cmd.AllocationID, Target: t, Attempt: attempt, Declared: declared, By: d.Actor,
		Epoch: d.Epoch, At: now})
	if err != nil {
		return action.Outcome{}, err
	}
	from := l.RowVersion
	l.RowVersion++
	if err = postgres.InsertAllocation(ctx, exec, sc, a); err == nil {
		err = postgres.TouchLicense(ctx, exec, sc, l, from, now)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: l.ID, RowVersion: l.RowVersion, Status: el.Registered, Events: []action.Event{{
		Type: api.EventLicenseAllocated, AggregateType: "ExportLicense", AggregateID: l.ID, AggregateVersion: l.RowVersion,
		Fields: map[string]any{"license_id": l.ID, "allocation_id": a.ID, "target_kind": t.Kind, "target_id": t.ID, "target_version": t.Version,
			"attempt": a.Attempt, "order_id": t.OrderID, "order_revision": t.OrderRevision, "quantity": a.Quantity.String(), "unit": string(l.Unit),
			"value_minor": a.Value.MinorString(), "currency": l.Currency.Code, "lines_hash": a.LinesHash}}}}, nil
}

// AllocateLicense allocates a licence to a shipment or export lot (export_license.allocate).
func (s Service) AllocateLicense(ctx context.Context, cmd api.AllocateLicense) (api.LicenseReceipt, error) {
	h := &allocateLicense{s: s, cmd: cmd}
	return s.runLicense(ctx, action.Command{Name: api.CommandAllocateLicense, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.LicenseID},
		cmd, cmd.Validate(), h, cmd.AllocationID, "")
}

// export_license.release (human only; documentation). Before CNX's transaction: the evidence
// sources of the external result (EVD), or, on basis local_cancellation (S5R-D03), the allocation's
// target as FUL shows it now (port Exports). In the transaction, after the licence FOR UPDATE: a
// licence the company lacks (NOT_FOUND), its row version (VERSION_CONFLICT), the release ID taken
// (BUSINESS_KEY_CONFLICT), an allocation not of this licence (NOT_FOUND); for an external result the
// evidence (REFERENCE_INVALID) and a value with more digits than the currency's minor units
// (INVALID_KNOWN_VALUE); for a local cancellation a target FUL does not resolve (REFERENCE_INVALID),
// one FUL has not cancelled (INVALID_STATE), one a declaration case covers that is submitted or later
// (INVALID_STATE /args/basis: declared usage is released on its external result only, MD-44); then
// the domain's INVALID_STATE (nothing outstanding; more than is outstanding). No event: the
// tactical model names none for a release (S3-D09).
type releaseLicense struct {
	s       Service
	cmd     api.ReleaseLicense
	sources map[string]evdapi.SourceRef
	target  *Export
	license *el.License
}

func (h *releaseLicense) Prepare(ctx context.Context) (err error) {
	h.target = nil
	if !h.cmd.LocalCancellation() {
		h.sources, err = h.s.sources(ctx, h.cmd.ExternalResult.EvidenceIDs)
		return err
	}
	var ref *ExportRef
	if err = h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		l, err := postgres.LoadLicense(ctx, exec, scope(ctx), h.cmd.LicenseID, postgres.NoLock)
		if err != nil || l == nil {
			return err
		}
		as, err := postgres.Allocations(ctx, exec, scope(ctx), l, h.cmd.AllocationID)
		if err == nil && len(as) == 1 {
			ref = &ExportRef{Kind: as[0].Target.Kind, ID: as[0].Target.ID}
		}
		return err
	}); err != nil || ref == nil {
		return err
	}
	exports, err := h.s.Upstream.Exports.ResolveExports(ctx, []ExportRef{*ref})
	if x, ok := exports[*ref]; ok && err == nil {
		h.target = &x
	}
	return err
}

func (h *releaseLicense) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.license, err = postgres.LoadLicense(ctx, exec, scope(ctx), h.cmd.LicenseID, postgres.ForUpdate)
	return err
}

func (h *releaseLicense) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd, l := scope(ctx), h.cmd, h.license
	if l == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/license_id", "")
	}
	if err := el.CheckVersion(l, cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	taken, err := postgres.ReleaseExists(ctx, exec, sc, cmd.ReleaseID)
	if err != nil {
		return action.Outcome{}, err
	}
	if taken {
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/release_id", "The release ID is taken")
	}
	allocations, err := postgres.Allocations(ctx, exec, sc, l, cmd.AllocationID)
	if err != nil {
		return action.Outcome{}, err
	}
	if len(allocations) != 1 {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/allocation_id", "")
	}
	now := kernel.Instant(h.s.Clock.Now())
	var rel *el.Release
	if cmd.LocalCancellation() {
		if rel, err = h.localCancellation(ctx, exec, sc, allocations[0], d, now); err != nil {
			return action.Outcome{}, err
		}
	} else if rel, err = h.externalResult(ctx, exec, sc, l, allocations[0], d, now); err != nil {
		return action.Outcome{}, err
	}
	from := l.RowVersion
	l.RowVersion++
	if err = postgres.InsertRelease(ctx, exec, sc, rel); err == nil {
		err = postgres.TouchLicense(ctx, exec, sc, l, from, now)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: l.ID, RowVersion: l.RowVersion, Status: el.Registered}, nil
}

// localCancellation judges a release on basis local_cancellation (S5R-D03, CNX-INV-002): the
// allocation's target, as FUL showed it before the transaction, is cancelled, and no declaration
// case covering it is submitted or later (CNX's own cases, read under the licence's lock; the
// release trigger checks it again). It releases all that is outstanding.
func (h *releaseLicense) localCancellation(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, a *el.Allocation, d authorization.Decision,
	now time.Time) (*el.Release, error) {
	const path = "/args/allocation_id"
	x := h.target
	switch {
	case x == nil || x.Ref.Kind != a.Target.Kind || x.Ref.ID != a.Target.ID:
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "FUL resolves no "+a.Target.Kind+" "+a.Target.ID)
	case a.Target.Kind == api.TargetShipment && x.Status != ShipmentCancelled, a.Target.Kind == api.TargetExportLot && x.Status != ExportLotCancelled:
		return nil, kernel.Reject(kernel.CodeInvalidState, path, "The "+a.Target.Kind+" is "+x.Status+": a local cancellation releases a cancelled one")
	}
	declared, err := postgres.TargetDeclared(ctx, exec, sc, a.Target.Kind, a.Target.ID)
	if err != nil {
		return nil, err
	}
	if declared {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/basis", "A declaration of the "+a.Target.Kind+
			" was submitted: release on its external result (MD-44)")
	}
	return el.ReleaseAllocation(a, el.ReleaseInput{ID: h.cmd.ReleaseID, Basis: el.LocalCancellation, TargetVersion: x.Version,
		Reason: h.cmd.Reason, By: d.Actor, Epoch: d.Epoch, At: now})
}

// externalResult judges a release on an external result.
func (h *releaseLicense) externalResult(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, l *el.License, a *el.Allocation,
	d authorization.Decision, now time.Time) (*el.Release, error) {
	cmd := h.cmd
	r := cmd.ExternalResult
	evidence := []el.Evidence{}
	for i, id := range r.EvidenceIDs {
		src, ok := h.sources[id]
		if !ok {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/external_result/evidence_ids/"+strconv.Itoa(i), "No such source")
		}
		evidence = append(evidence, el.Evidence{SourceID: src.SourceID, ContentSHA256: src.ContentSHA256})
	}
	received, _ := kernel.ParseTimestamp(r.ReceivedAt)
	in := el.ReleaseInput{ID: cmd.ReleaseID, Basis: el.ExternalResult, Kind: r.Kind, DeclarationNumber: r.DeclarationNumber,
		ReceiptText: r.ReceiptText, ReceivedAt: received, Evidence: evidence, Reason: cmd.Reason, By: d.Actor, Epoch: d.Epoch, At: now}
	if err := declarationResult(ctx, exec, sc, a, r, &in); err != nil {
		return nil, err
	}
	if r.Kind == el.Amended {
		var err error
		in.Quantity, _ = kernel.ParseDecimal(cmd.Quantity, kernel.QuantityFormat)
		if in.Value, err = kernel.ParseAmount(l.Value.Currency(), cmd.Value); err != nil {
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/value", "More digits than "+l.Currency.Code+" has minor units")
		}
	}
	return el.ReleaseAllocation(a, in)
}

// ReleaseLicense releases an allocation on an external result (export_license.release).
func (s Service) ReleaseLicense(ctx context.Context, cmd api.ReleaseLicense) (api.LicenseReceipt, error) {
	h := &releaseLicense{s: s, cmd: cmd}
	return s.runLicense(ctx, action.Command{Name: api.CommandReleaseLicense, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.LicenseID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h, cmd.AllocationID, cmd.ReleaseID)
}

func (s Service) runLicense(ctx context.Context, cmd action.Command, req api.Command, validation error, h action.Handler, allocation, release string) (api.LicenseReceipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return api.LicenseReceipt{}, errors.Join(validation, err)
			}
		}
		return api.LicenseReceipt{}, validation
	}
	_, _, args := request(req)
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.LicenseReceipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	if err != nil {
		return api.LicenseReceipt{}, err
	}
	out := api.LicenseReceipt{Receipt: api.Receipt(res), LicenseID: res.ObjectID, AllocationID: allocation, ReleaseID: release}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		l, err := postgres.LoadLicense(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || l == nil {
			return err
		}
		allocations, err := postgres.Allocations(ctx, exec, scope(ctx), l, "")
		if err != nil {
			return err
		}
		out.Usage, err = usageView(l, allocations)
		return err
	})
	return out, err
}
