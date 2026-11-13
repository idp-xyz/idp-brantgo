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
	cert "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/certificate"
	db "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/documentbundle"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Official certificate results (WBS S5-10 CNX half; CNX-AGG-CertificateResult, CNX-INV-007, MD-40;
// CN-R14, CN-AT14; Context Map R56 CertificateResultRef v1). Upstreams, read before CNX's transaction:
// FUL (R36, the shipment or export lot a certificate applies to, through the port Exports) and EVD (R54,
// the certificate's file and the evidence of its verification). A correction locks the certificate root
// FOR UPDATE (the handler's Lock), then the ledger takes the authority guard FOR SHARE; a licence a
// licence copy names is CNX's own, read without a lock. No upstream guard is taken: a revision binds
// what it read (the target's row version and lines, the file's SHA-256) and never changes.
//
// CertificateResultRef v1 (ResolveCertificates) reads the targets through FUL before a CNX read
// transaction in which it reads the certificates and, for a target whose consignee country PTY does not
// record, the destination of the trade compliance evaluation of the order revision the target ships
// (S5 D-8, as export licence allocations do). It needs an active grant only and audits nothing: it is
// another context's lookup, like CAT's DeclarationProfileRef v1.

// certificateRules: humanOnlyCommands name certificate.register (documentation template); the reads
// need cnx.read.
func certificateRules() []authorization.Rule {
	return []authorization.Rule{
		authorization.HumanOnly(api.CommandRegisterCertificate, api.CommandRegisterCertificate),
		{Command: api.CommandGetCertificate, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandCertificates, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandCertificateApplicability, Permission: api.PermissionRead, ActorKinds: anyActor},
	}
}

// certificateRequest is request's part for certificate.register: everything but the key and the
// expected version.
func certificateRequest(cmd api.Command) (name, key string, args any) {
	if c, ok := cmd.(api.RegisterCertificate); ok {
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRegisterCertificate, key, c
	}
	return openingRequest(cmd)
}

// visibleCertificate is Visible's part for certificates, which are company-wide like licences.
func visibleCertificate(ctx context.Context, exec bentopg.Executor, command, id string) (ok, visible bool, err error) {
	if command != api.CommandRegisterCertificate {
		return false, false, nil
	}
	c, err := postgres.LoadCertificate(ctx, exec, scope(ctx), id, postgres.NoLock)
	return true, c != nil, err
}

// certificate.register (human only; documentation). Before CNX's transaction: the object (FUL, port
// Exports) for an export lot or shipment, the file (EVD GetFile) and the evidence (EVD). In the
// transaction, after the certificate FOR UPDATE for a correction, the refusals in this order: a
// certificate the company lacks (NOT_FOUND), another row version (VERSION_CONFLICT), another kind
// (INVALID_KNOWN_VALUE); the issuer's number of the kind held by another certificate
// (BUSINESS_KEY_CONFLICT); an object FUL does not resolve or whose record is not intact
// (REFERENCE_INVALID) or that is cancelled (INVALID_STATE); the file and the evidence
// (REFERENCE_INVALID); a licence CNX lacks (REFERENCE_INVALID) or of another number
// (INVALID_KNOWN_VALUE); a correction that changes nothing (INVALID_KNOWN_VALUE).
// CertificateRegistered is published.
type registerCertificate struct {
	s       Service
	cmd     api.RegisterCertificate
	object  *Export
	file    evdapi.FileView
	fileErr error
	sources map[string]evdapi.SourceRef
	// in the transaction
	root     *cert.Certificate
	revision *cert.Revision
}

func (h *registerCertificate) Prepare(ctx context.Context) error {
	h.object, h.fileErr = nil, nil
	if o := h.cmd.Object; o.Kind != api.ObjectSkuSpec {
		ref := ExportRef{Kind: o.Kind, ID: o.ID}
		got, err := h.s.Upstream.Exports.ResolveExports(ctx, []ExportRef{ref})
		if err != nil {
			return err
		}
		if x, ok := got[ref]; ok {
			h.object = &x
		}
	}
	var err error
	h.file, err = h.s.Upstream.Sources.GetFile(ctx, h.cmd.FileID)
	if rej, ok := kernel.AsRejection(err); ok && (rej.Code == kernel.CodeNotFound || rej.Code == kernel.CodePermissionDenied) {
		h.fileErr = kernel.Reject(kernel.CodeReferenceInvalid, "/args/file_id", "No file the actor reads")
	} else if err != nil {
		return err
	}
	h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *registerCertificate) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.root, h.revision = nil, nil
	if h.cmd.CertificateID == "" {
		return nil
	}
	var err error
	h.root, err = postgres.LoadCertificate(ctx, exec, scope(ctx), h.cmd.CertificateID, postgres.ForUpdate)
	return err
}

func (h *registerCertificate) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd := scope(ctx), h.cmd
	if err := cert.Open(h.root, cmd.ExpectedVersion, cmd.CertificateKind); err != nil {
		return action.Outcome{}, err
	}
	owner, err := postgres.CertificateByKey(ctx, exec, sc, cmd.CertificateKind, cmd.Issuer, cmd.Number)
	if err != nil {
		return action.Outcome{}, err
	}
	if owner != "" && owner != cmd.CertificateID {
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/number", "The issuer's "+cmd.CertificateKind+" number is certificate "+owner+"'s")
	}
	content := cert.Content{Kind: cmd.CertificateKind, Number: cmd.Number, Issuer: cmd.Issuer, IssuedOn: cmd.IssuedOn, Market: cmd.Market,
		LicenseID: cmd.LicenseID, Basis: cmd.Basis, Evidence: []cert.Evidence{}}
	if content.Object, err = h.objectOf(); err != nil {
		return action.Outcome{}, err
	}
	if h.fileErr != nil {
		return action.Outcome{}, h.fileErr
	}
	if h.file.FileID != cmd.FileID {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/file_id", "No file the actor reads")
	}
	content.File = cert.Evidence{SourceID: h.file.FileID, ContentSHA256: h.file.ContentSHA256}
	for i, id := range cmd.EvidenceIDs {
		src, ok := h.sources[id]
		if !ok {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/evidence_ids/"+strconv.Itoa(i), "No such source")
		}
		content.Evidence = append(content.Evidence, cert.Evidence{SourceID: src.SourceID, ContentSHA256: src.ContentSHA256})
	}
	if cmd.LicenseID != "" {
		l, err := postgres.LoadLicense(ctx, exec, sc, cmd.LicenseID, postgres.NoLock)
		if err != nil {
			return action.Outcome{}, err
		}
		if l == nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/license_id", "CNX has no such export licence")
		}
		if l.Number != cmd.Number {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/number", "The licence copy states licence "+l.Number)
		}
	}
	from, _ := kernel.ParseTimestamp(cmd.ValidFrom)
	until, _ := kernel.ParseTimestamp(cmd.ValidUntil)
	content.ValidFrom, content.ValidUntil = kernel.FormatTimestamp(from), kernel.FormatTimestamp(until)
	var current *cert.Revision
	if h.root != nil {
		revisions, err := postgres.CertificateRevisions(ctx, exec, sc, h.root.ID)
		if err != nil {
			return action.Outcome{}, err
		}
		if n := len(revisions); n > 0 {
			current = revisions[n-1]
		}
	}
	now := kernel.Instant(h.s.Clock.Now())
	id := cmd.CertificateID
	if id == "" {
		id = action.NewID("crt", now)
	}
	root, rev, err := cert.Register(h.root, current, id, cmd.ExpectedVersion, cert.Registration{Content: content, Reason: cmd.Reason, By: d.Actor,
		Epoch: d.Epoch, At: now})
	if err != nil {
		return action.Outcome{}, err
	}
	if h.root == nil {
		err = postgres.InsertCertificate(ctx, exec, sc, root)
	} else {
		err = postgres.SaveCertificate(ctx, exec, sc, root, h.root.RowVersion)
	}
	if err == nil {
		err = postgres.InsertCertificateRevision(ctx, exec, sc, rev)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	h.root, h.revision = root, rev
	fields := map[string]any{"certificate_id": root.ID, "revision": rev.Revision, "kind": rev.Kind, "object_kind": rev.Object.Kind,
		"market": rev.Market, "valid_from": rev.ValidFrom, "valid_until": rev.ValidUntil, "document_sha256": rev.SHA256}
	if rev.Object.ID != "" {
		fields["object_id"] = rev.Object.ID
	}
	if rev.Supersedes > 0 {
		fields["supersedes"] = rev.Supersedes
	}
	return action.Outcome{ObjectID: root.ID, RowVersion: root.RowVersion, Status: cert.Registered, Events: []action.Event{{
		Type: api.EventCertificateRegistered, AggregateType: "CertificateResult", AggregateID: root.ID, AggregateVersion: root.RowVersion,
		Fields: fields}}}, nil
}

// objectOf decides the object a certificate applies to: SKU specification revisions as stated, or the
// export lot or shipment FUL showed, with its lines.
func (h *registerCertificate) objectOf() (cert.Object, error) {
	o := h.cmd.Object
	out := cert.Object{Kind: o.Kind}
	if o.Kind == api.ObjectSkuSpec {
		for _, s := range o.SkuSpecs {
			out.Specs = append(out.Specs, cert.Spec{SkuID: s.SkuID, SpecRevision: s.SpecRevision})
		}
		return out, nil
	}
	const path = "/args/object/id"
	x := h.object
	switch {
	case x == nil:
		return out, kernel.Reject(kernel.CodeReferenceInvalid, path, "FUL resolves no such "+o.Kind)
	case x.Code != "":
		return out, kernel.Reject(kernel.CodeReferenceInvalid, path, "FUL's record of the "+o.Kind+" is not intact")
	case x.Status == ExportLotCancelled:
		return out, kernel.Reject(kernel.CodeInvalidState, path, "The "+o.Kind+" is cancelled")
	}
	out.ID, out.Version, out.OrderID, out.OrderRevision = o.ID, x.Version, x.OrderID, x.OrderRevision
	out.Lines = targetLines(x)
	return out, nil
}

// targetLines are an export's lines as a certificate covers them.
func targetLines(x *Export) []cert.Line {
	out := []cert.Line{}
	for _, l := range x.Lines {
		out = append(out, cert.Line{ShipmentID: l.ShipmentID, LineID: l.LineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision,
			Quantity: l.Quantity.String(), Unit: string(l.Unit)})
	}
	return out
}

// RegisterCertificate registers a certificate result or a correction (certificate.register).
func (s Service) RegisterCertificate(ctx context.Context, cmd api.RegisterCertificate) (api.CertificateReceipt, error) {
	h := &registerCertificate{s: s, cmd: cmd}
	validation := cmd.Validate()
	acmd := action.Command{Name: api.CommandRegisterCertificate, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.CertificateID, Reason: cmd.Reason}
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, acmd.Name, acmd.ObjectID, rej.Code); err != nil {
				return api.CertificateReceipt{}, errors.Join(validation, err)
			}
		}
		return api.CertificateReceipt{}, validation
	}
	_, _, args := request(cmd)
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.CertificateReceipt{}, err
	}
	acmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, acmd, h)
	if err != nil {
		return api.CertificateReceipt{}, err
	}
	out := api.CertificateReceipt{Receipt: api.Receipt(res), CertificateID: res.ObjectID}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		revisions, err := postgres.CertificateRevisions(ctx, exec, scope(ctx), res.ObjectID)
		if err != nil {
			return err
		}
		// the revision the command registered: its row version is its revision number
		for _, r := range revisions {
			if int64(r.Revision) == res.RowVersion {
				out.Revision, out.ContentSHA256 = int64(r.Revision), r.SHA256
			}
		}
		return nil
	})
	return out, err
}

// certificateUse is a use as the lookups judge it: the certificate, revision and target, and the
// target's destination as CNX established it.
type certificateUse struct {
	api.CertificateUse
	target *Export
}

// judgeCertificate answers a use in CNX's read transaction: the certificate and the revision asked,
// the target's destination (FUL's, else the order revision's trade compliance evaluation), the verdict.
func judgeCertificate(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, u certificateUse, destinations map[ExportRef]string) (api.CertificateResult, error) {
	out := api.CertificateResult{Use: u.CertificateUse, CertificateID: u.Ref.CertificateID, Code: kernel.CodeReferenceInvalid,
		Reason: api.CertificateUnknown, Covered: []string{}, Uncovered: []string{}}
	c, err := postgres.LoadCertificate(ctx, exec, sc, u.Ref.CertificateID, postgres.NoLock)
	if err != nil || c == nil {
		return out, err
	}
	revisions, err := postgres.CertificateRevisions(ctx, exec, sc, c.ID)
	if err != nil {
		return out, err
	}
	asked := u.Ref.Version
	if asked == 0 {
		asked = int64(c.CurrentRevision)
	}
	i := slices.IndexFunc(revisions, func(r *cert.Revision) bool { return int64(r.Revision) == asked })
	if i < 0 {
		return out, nil
	}
	r := revisions[i]
	fillResult(&out, c, r, u.At)
	x := u.target
	if x == nil || x.Code != "" {
		out.Reason = api.CertificateTargetUnknown
		return out, nil
	}
	ref := ExportRef{Kind: u.Target.Kind, ID: u.Target.ID}
	destination, known := destinations[ref]
	if !known {
		destination = x.Destination
		if destination == "" {
			if destination, err = evaluatedDestination(ctx, exec, sc, x.OrderID, x.OrderRevision); err != nil {
				return out, err
			}
		}
		destinations[ref] = destination
	}
	use := cert.Use{Kind: u.Kind, Target: cert.Target{Kind: u.Target.Kind, ID: u.Target.ID, Destination: destination, Lines: targetLines(x)}, At: u.At}
	v := cert.Judge(r, c.CurrentRevision, use)
	out.Code, out.Reason, out.Applicable, out.Destination, out.Covered, out.Uncovered = v.Code(), v.Reason, v.Applicable, destination, v.Covered,
		v.Uncovered
	if !v.Current {
		out.Reason = api.CertificateSuperseded
	}
	return out, nil
}

// fillResult puts what revision r of certificate c states into out.
func fillResult(out *api.CertificateResult, c *cert.Certificate, r *cert.Revision, at time.Time) {
	out.Revision, out.CurrentRevision, out.ContentSHA256 = int64(r.Revision), int64(c.CurrentRevision), r.SHA256
	out.Kind, out.Number, out.Issuer, out.IssuedOn, out.Market = r.Kind, r.Number, r.Issuer, r.IssuedOn, r.Market
	out.ValidFrom, out.ValidUntil, out.State = r.Validity.From, r.Validity.Until, r.State(kernel.Instant(at))
	out.ObjectKind, out.ObjectID, out.FileID, out.FileSHA256 = r.Object.Kind, r.Object.ID, r.File.SourceID, r.File.ContentSHA256
	out.VerifiedBy = kernel.ActorRef{Kind: kernel.ActorKind(r.VerifiedBy.Kind), ID: r.VerifiedBy.ID}
	if t, err := kernel.ParseTimestamp(r.RegisteredAt); err == nil {
		out.RegisteredAt = kernel.Instant(t)
	}
}

// ResolveCertificates answers CertificateResultRef v1 (api.CertificateLookups) for every use, in
// order: targets through FUL before CNX's read transaction, then the certificates.
func (s Service) ResolveCertificates(ctx context.Context, uses []api.CertificateUse) ([]api.CertificateResult, error) {
	if len(uses) > api.MaxCertificateLookups {
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/uses", "")
	}
	var refs []ExportRef
	for i, u := range uses {
		if err := u.Validate("/uses/" + strconv.Itoa(i)); err != nil {
			return nil, err
		}
		if ref := (ExportRef{Kind: u.Target.Kind, ID: u.Target.ID}); !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	out := make([]api.CertificateResult, len(uses))
	if len(uses) == 0 {
		return out, nil
	}
	targets, err := s.Upstream.Exports.ResolveExports(ctx, refs)
	if err != nil {
		return nil, err
	}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		destinations := map[ExportRef]string{}
		for i, u := range uses {
			cu := certificateUse{CertificateUse: u}
			if x, ok := targets[ExportRef{Kind: u.Target.Kind, ID: u.Target.ID}]; ok {
				cu.target = &x
			}
			var err error
			if out[i], err = judgeCertificate(ctx, exec, scope(ctx), cu, destinations); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// requirementCertificates judges the certificates a bundle requirement names on the bundle's export
// lot x at at, in the check's transaction (certificates are CNX's own; read without a lock: a revision
// never changes, and the check records the one it read): an unknown certificate is REFERENCE_INVALID, one
// of another kind or market, outside its validity, or covering none of the lot's lines
// CERTIFICATE_NOT_APPLICABLE, at path/<index>. A certificate covering only some lines is recorded with
// them; db.Check decides whether the requirement may be verified.
func requirementCertificates(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, r api.Requirement, x *Export, at time.Time,
	destinations map[ExportRef]string, path string) ([]db.CertificateRef, error) {
	var out []db.CertificateRef
	for j, id := range r.CertificateIDs {
		p := path + "/" + strconv.Itoa(j)
		u := certificateUse{CertificateUse: api.CertificateUse{Ref: api.CertificateRef{CertificateID: id}, Kind: db.CertificateKind(r.Kind),
			Target: api.ExportTarget{Kind: api.TargetExportLot, ID: x.Ref.ID}, At: at}, target: x}
		res, err := judgeCertificate(ctx, exec, sc, u, destinations)
		if err != nil {
			return nil, err
		}
		switch {
		case res.Code == kernel.CodeReferenceInvalid:
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, p, "CNX has no such certificate result")
		case res.Code != "" && (res.Reason != api.CertificateGoods || len(res.Covered) == 0):
			return nil, api.Reject(api.CodeCertificateNotApplicable, p, "Certificate "+id+" does not apply to the export lot: "+res.Reason)
		}
		out = append(out, db.CertificateRef{CertificateID: id, Revision: int(res.Revision), DocumentSHA256: res.ContentSHA256, Kind: res.Kind,
			Number: res.Number, Issuer: res.Issuer, Market: res.Market, ValidFrom: kernel.FormatTimestamp(res.ValidFrom),
			ValidUntil: kernel.FormatTimestamp(res.ValidUntil), State: res.State, Covered: res.Covered})
	}
	return out, nil
}

// lotLineIDs are the IDs of an export lot's lines.
func lotLineIDs(x *Export) []string {
	out := []string{}
	for _, l := range x.Lines {
		out = append(out, l.LineID)
	}
	return out
}

// certificateLinks are a requirement's certificates as the reads show them.
func certificateLinks(refs []db.CertificateRef) []api.CertificateLink {
	var out []api.CertificateLink
	for _, c := range refs {
		out = append(out, api.CertificateLink{CertificateID: c.CertificateID, Revision: int64(c.Revision), DocumentSHA256: c.DocumentSHA256,
			Kind: c.Kind, Number: c.Number, Issuer: c.Issuer, Market: c.Market, ValidFrom: c.ValidFrom, ValidUntil: c.ValidUntil, State: c.State,
			Covered: slices.Clone(c.Covered)})
	}
	return out
}

func revisionView(r *cert.Revision, at time.Time) api.CertificateRevisionView {
	v := api.CertificateRevisionView{Revision: int64(r.Revision), Kind: r.Kind, Number: r.Number, Issuer: r.Issuer, IssuedOn: r.IssuedOn,
		Object: api.CertificateObjectView{Kind: r.Object.Kind, ID: r.Object.ID, Version: r.Object.Version, OrderID: r.Object.OrderID,
			OrderRevision: r.Object.OrderRevision, SkuSpecs: []api.SpecRef{}, Lines: []api.CertificateLineView{}},
		Market: r.Market, ValidFrom: r.Validity.From, ValidUntil: r.Validity.Until, State: r.State(at), File: api.EvidenceView(r.File),
		LicenseID: r.LicenseID, Basis: r.Basis, Evidence: []api.EvidenceView{}, Supersedes: int64(r.Supersedes), Reason: r.Reason,
		DocumentSHA256: r.SHA256, VerifiedBy: kernel.ActorRef{Kind: kernel.ActorKind(r.VerifiedBy.Kind), ID: r.VerifiedBy.ID},
		AuthorityEpoch: r.AuthorityEpoch}
	for _, s := range r.Object.Specs {
		v.Object.SkuSpecs = append(v.Object.SkuSpecs, api.SpecRef{SkuID: s.SkuID, SpecRevision: s.SpecRevision})
	}
	for _, l := range r.Object.Lines {
		v.Object.Lines = append(v.Object.Lines, api.CertificateLineView(l))
	}
	for _, e := range r.Evidence {
		v.Evidence = append(v.Evidence, api.EvidenceView(e))
	}
	if t, err := kernel.ParseTimestamp(r.RegisteredAt); err == nil {
		v.RegisteredAt = kernel.Instant(t)
	}
	return v
}

// GetCertificate returns a certificate with every revision (certificate.get, cnx.read).
func (s Service) GetCertificate(ctx context.Context, certificateID string) (api.CertificateView, error) {
	var v api.CertificateView
	if !kernel.ValidIdentifier(certificateID) {
		return v, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandGetCertificate, certificateID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		c, err := postgres.LoadCertificate(ctx, exec, sc, certificateID, postgres.NoLock)
		if err != nil {
			return err
		}
		if c == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		revisions, err := postgres.CertificateRevisions(ctx, exec, sc, c.ID)
		if err != nil {
			return err
		}
		now := kernel.Instant(s.Clock.Now())
		v = api.CertificateView{CertificateID: c.ID, Kind: c.Kind, Status: cert.Registered, CurrentRevision: int64(c.CurrentRevision),
			RowVersion: c.RowVersion, Revisions: []api.CertificateRevisionView{}, RegisteredBy: c.RegisteredBy, RegisteredAt: c.RegisteredAt,
			UpdatedAt: c.UpdatedAt, CheckedAt: now}
		for _, r := range revisions {
			v.Revisions = append(v.Revisions, revisionView(r, now))
			if r.Revision == c.CurrentRevision {
				v.State = r.State(now)
			}
		}
		return nil
	})
	return v, err
}

// Certificates returns the company's certificates, newest first, each as its current revision states
// it (certificate.list, cnx.read).
func (s Service) Certificates(ctx context.Context) ([]api.CertificateSummary, error) {
	out := []api.CertificateSummary{}
	err := s.Ledger.Read(ctx, api.CommandCertificates, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		list, err := postgres.Certificates(ctx, exec, sc)
		if err != nil {
			return err
		}
		now := kernel.Instant(s.Clock.Now())
		for _, c := range list {
			revisions, err := postgres.CertificateRevisions(ctx, exec, sc, c.ID)
			if err != nil {
				return err
			}
			i := slices.IndexFunc(revisions, func(r *cert.Revision) bool { return r.Revision == c.CurrentRevision })
			if i < 0 {
				return kernel.Reject(kernel.CodeReferenceInvalid, "/certificates", "A certificate stands on no revision")
			}
			r := revisions[i]
			out = append(out, api.CertificateSummary{CertificateID: c.ID, Kind: c.Kind, Number: r.Number, Issuer: r.Issuer, Market: r.Market,
				ObjectKind: r.Object.Kind, ObjectID: r.Object.ID, ValidFrom: r.Validity.From, ValidUntil: r.Validity.Until, State: r.State(now),
				CurrentRevision: int64(c.CurrentRevision), RowVersion: c.RowVersion, CheckedAt: now})
		}
		return nil
	})
	return out, err
}

// CertificateApplicability returns what CertificateResultRef v1 answers now for the certificate's
// current revision, of its own kind, on target (certificate.applicability, cnx.read): the target
// through FUL before CNX's read.
func (s Service) CertificateApplicability(ctx context.Context, certificateID string, target api.ExportTarget) (api.CertificateApplicabilityView, error) {
	var v api.CertificateApplicabilityView
	switch {
	case !kernel.ValidIdentifier(certificateID):
		return v, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	case target.Kind != api.TargetShipment && target.Kind != api.TargetExportLot:
		return v, kernel.Reject(kernel.CodeSchemaInvalid, "/target_kind", "")
	case !kernel.ValidIdentifier(target.ID):
		return v, kernel.Reject(kernel.CodeSchemaInvalid, "/target_id", "")
	}
	ref := ExportRef{Kind: target.Kind, ID: target.ID}
	targets, err := s.Upstream.Exports.ResolveExports(ctx, []ExportRef{ref})
	if err != nil {
		return v, err
	}
	err = s.Ledger.Read(ctx, api.CommandCertificateApplicability, certificateID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		c, err := postgres.LoadCertificate(ctx, exec, sc, certificateID, postgres.NoLock)
		if err != nil {
			return err
		}
		if c == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		now := kernel.Instant(s.Clock.Now())
		u := certificateUse{CertificateUse: api.CertificateUse{Ref: api.CertificateRef{CertificateID: c.ID}, Kind: c.Kind, Target: target, At: now}}
		if x, ok := targets[ref]; ok {
			u.target = &x
		}
		v.CertificateResult, err = judgeCertificate(ctx, exec, sc, u, map[ExportRef]string{})
		v.CheckedAt = now
		return err
	})
	return v, err
}
