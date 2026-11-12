package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	docs "go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipmentdocumentset"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The shipment document set (WBS S5-08; api/document.go). shipment_document.prepare follows TRD's
// order.document.generate (S3): before FUL's transaction — after the ledger authorized the command
// and found no stored result for its key — Prepare
//
//  1. reads the set and the shipment in a FUL transaction of its own, so a set or shipment the
//     actor does not see, or a shipment that has another set, costs no number;
//  2. asks EVD which cited sources the actor reads and, for a B/L or CO, the file; for a CO that names
//     a certificate result, asks CNX whether it applies to the shipment (CertificateResultRef v1, R56,
//     S5 C5);
//  3. for a CI or PL reads from POL the templates, the number sequence, the seller's company profile
//     (in force now) and the order currency (whatever its state), from TRD the order's current
//     revision with its terms and address roles (OrderCommercialSnapshot v1), from PTY the names of
//     the customer and the order's consignee at the revisions bound (audited, pty.read);
//  4. has POL issue the number (number.issue as the acting actor, NumberIssue v1, R49; business
//     reference the set ID, so a retry and every later version of the kind get the same number back;
//     a refusal after it leaves the number issued to the set's ID: a gap if the set never comes, never
//     a reuse).
//
// In the transaction, in FUL's root order (shipment, then its other roots): the shipment FOR SHARE
// (packing, bookings, confirmation, cancellation and the commercial release lock it FOR UPDATE, so
// the version is prepared on facts that hold until it commits; document_release.record holds it and
// its release before it locks the set FOR SHARE, so the two never wait on each other in a cycle),
// then the set FOR UPDATE (a new set: none yet; two first prepares meet on its key and on the
// one-set-per-shipment key), the authority guard row FOR SHARE (the ledger), then POL's ConfigGuard
// v1 on the templates, the sequence and the seller profile, FOR SHARE, at the content hashes read
// before (REFERENCE_STALE when one moved); the version is decided and rendered (domain shipmentdocumentset), and its content,
// renditions' bytes, number and commit evidence are written in this one transaction (S3-D135).
// shipment_document.issue locks the shipment FOR SHARE, then the set FOR UPDATE, the authority guard
// and POL's ConfigGuard on every draft's revisions; record_signed locks the set FOR UPDATE only.

// documentRules are the rules of the booking and document commands and reads (humanOnlyCommands:
// shipment_document.issue and shipment_document.record_signed; role templates: booking.record is
// the merchandiser's, the document commands the documentation clerk's).
func documentRules() []authorization.Rule {
	return append([]authorization.Rule{
		{Command: api.CommandRecordBooking, Permission: api.CommandRecordBooking, ActorKinds: humanOrAgent},
		{Command: api.CommandPrepareShipmentDocument, Permission: api.CommandPrepareShipmentDocument, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandIssueShipmentDocuments, api.CommandIssueShipmentDocuments),
		authorization.HumanOnly(api.CommandRecordSignedShipmentDocument, api.CommandRecordSignedShipmentDocument),
		{Command: api.CommandGetBookings, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetDocumentSet, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetShipmentDocument, Permission: api.PermissionRead, ActorKinds: anyActor},
	}, insuranceRules()...) // S5-16 (insurance.go)
}

// documentRequest returns the name, idempotency key and business arguments of a booking or document
// command, or "" for another command.
func documentRequest(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.RecordBooking:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRecordBooking, key, c
	case api.PrepareShipmentDocument:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandPrepareShipmentDocument, key, c
	case api.IssueShipmentDocuments:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandIssueShipmentDocuments, key, c
	case api.RecordSignedShipmentDocument:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandRecordSignedShipmentDocument, key, c
	case api.RecordInsurance: // S5-16
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRecordInsurance, key, c
	}
	return "", "", nil
}

// documentVisible reports whether d's actor sees the set a stored result of a document command names;
// handled is false for other commands.
func documentVisible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (visible, handled bool, err error) {
	switch command {
	case api.CommandPrepareShipmentDocument, api.CommandIssueShipmentDocuments, api.CommandRecordSignedShipmentDocument,
		api.CommandRecordInsurance:
		s, err := postgres.LoadSet(ctx, exec, scope(ctx), id, postgres.NoLock)
		return err == nil && s != nil && d.Sees(s.OwnerActor, s.OwnerTeam), true, err
	}
	return false, false, nil
}

func polRef(r api.ConfigRef) polapi.ConfigRef {
	return polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ID, Revision: r.Revision}
}

func docRef(r polapi.ConfigRef) docs.Ref {
	return docs.Ref{Kind: r.Kind, ID: r.ConfigID, Revision: r.Revision}
}

func apiRef(r docs.Ref) api.ConfigRef {
	return api.ConfigRef{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
}

func numberKey(name, key string) string {
	sum := sha256.Sum256([]byte(name + "\n" + key))
	return "ful." + hex.EncodeToString(sum[:])[:56]
}

// guarded is a POL revision a command re-checks with ConfigGuard v1 in its transaction, at the
// content hash read before, and where its refusal points.
type guarded struct {
	ref  polapi.ConfigRef
	hash string
	path string
}

// guardConfigs runs ConfigGuard v1 on every revision of gs at instant at: not in force is POL's
// refusal, another content hash REFERENCE_STALE. It returns the evidence entries.
func (s Service) guardConfigs(ctx context.Context, exec bentopg.Executor, gs []guarded, at time.Time) ([]docs.Entry, error) {
	var out []docs.Entry
	for _, g := range gs {
		v, err := s.Upstream.Documents.Configs.GuardConfig(ctx, exec, g.ref, at)
		if err != nil {
			return nil, err
		}
		if err = v.Check(g.path); err != nil {
			return nil, err
		}
		if v.ContentHash != g.hash {
			return nil, docs.Reject(docs.CodeReferenceStale, g.path, "The revision's content hash is not the one rendered from")
		}
		out = append(out, docs.Entry{Name: "pol:" + g.ref.Kind + ":" + g.ref.ConfigID + "@" + strconv.FormatInt(g.ref.Revision, 10),
			Value: "in_force/" + v.ContentHash})
	}
	return out, nil
}

// prepareDocumentCommand runs shipment_document.prepare.
type prepareDocumentCommand struct {
	s      Service
	cmd    api.PrepareShipmentDocument
	req    docs.Request
	up     docs.Upstream
	guards []guarded
	// in the transaction
	set    *docs.Set
	other  string // the set the shipment has, when it is not cmd's
	facts  docs.Facts
	result *docs.Document
}

func (h *prepareDocumentCommand) Prepare(ctx context.Context) error {
	h.up, h.guards = docs.Upstream{}, nil
	var ship *shipment.Shipment
	var prices map[string]shipment.Price
	refused := false
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		set, err := postgres.LoadSet(ctx, exec, sc, h.cmd.DocumentSetID, postgres.NoLock)
		if err != nil {
			return err
		}
		if set != nil && (!d.Sees(set.OwnerActor, set.OwnerTeam) || set.ShipmentID != h.cmd.ShipmentID) {
			refused = true // the transaction refuses it with its path
			return nil
		}
		if set == nil {
			other, err := postgres.SetOfShipment(ctx, exec, sc, h.cmd.ShipmentID)
			if err != nil || other != "" {
				refused = other != ""
				return err
			}
		}
		if ship, err = postgres.LoadShipment(ctx, exec, sc, h.cmd.ShipmentID, postgres.NoLock); err != nil || ship == nil {
			return err
		}
		if !d.Sees(ship.OwnerActor, ship.OwnerTeam) || ship.Status == shipment.StatusCancelled {
			ship = nil
			return nil
		}
		prices, err = postgres.LoadPrices(ctx, exec, sc, ship.ID)
		return err
	})
	if err != nil || refused || ship == nil {
		return err
	}
	if h.up.Found, err = h.s.sources(ctx, h.cmd.EvidenceIDs); err != nil {
		return err
	}
	if !docs.Rendered(h.cmd.DocumentKind) {
		return h.file(ctx)
	}
	return h.rendered(ctx, ship, prices)
}

// file reads the EVD file of a B/L or CO record, and asks CNX about the certificate result a CO
// record names.
func (h *prepareDocumentCommand) file(ctx context.Context) error {
	f, err := h.s.Upstream.Sources.GetFile(ctx, h.req.FileID)
	if rej, ok := kernel.AsRejection(err); ok && (rej.Code == kernel.CodeNotFound || rej.Code == kernel.CodePermissionDenied) {
		return nil // the domain refuses a file the actor does not read
	}
	if err != nil {
		return err
	}
	h.up.File = &docs.File{ID: f.FileID, SHA256: f.ContentSHA256, MimeType: f.MimeType}
	return h.certificate(ctx) // a CO's certificate result (CNX CertificateResultRef v1, S5 C5)
}

// rendered reads what a CI or PL needs from POL, TRD and PTY and has POL issue the number.
func (h *prepareDocumentCommand) rendered(ctx context.Context, ship *shipment.Shipment, prices map[string]shipment.Price) error {
	now := h.s.Clock.Now()
	refs := []polapi.ConfigRef{polRef(*h.cmd.SequenceRef), polRef(*h.cmd.SellerRef)}
	for _, t := range h.cmd.Templates {
		refs = append(refs, polRef(t))
	}
	var currency polapi.ConfigRef
	for _, p := range prices {
		currency = polapi.ConfigRef{Kind: polapi.KindCurrency, ConfigID: p.CurrencyID, Revision: p.CurrencyRevision}
	}
	if currency.ConfigID != "" {
		refs = append(refs, currency)
	}
	got, err := h.s.Upstream.Documents.Configs.ResolveConfigs(ctx, refs, now)
	if err != nil {
		return err
	}
	in := func(ref polapi.ConfigRef, path string) (polapi.ResolvedConfig, error) {
		r, ok := got[ref]
		if !ok {
			return r, kernel.Reject(kernel.CodeReferenceInvalid, path, "No such POL revision")
		}
		h.guards = append(h.guards, guarded{ref: ref, hash: r.ContentHash, path: path})
		return r, r.Check(path)
	}
	h.req.Templates = nil
	for i, t := range h.cmd.Templates {
		path := "/args/templates/" + strconv.Itoa(i)
		r, err := in(polRef(t), path)
		if err != nil {
			return err
		}
		x := r.Data.ShipmentDocumentTemplate
		if x == nil {
			return kernel.Reject(kernel.CodeReferenceInvalid, path, "Not a shipment document template")
		}
		h.req.Templates = append(h.req.Templates, docs.Template{Ref: docRef(polRef(t)), Hash: r.ContentHash, DocumentKind: x.DocumentKind,
			Language: x.Language, RendererVersion: x.RendererVersion, Title: x.Title, Sections: slices.Clone(x.Sections), Clauses: slices.Clone(x.Clauses),
			FooterNote: x.FooterNote})
	}
	seq, err := in(polRef(*h.cmd.SequenceRef), "/args/sequence_ref")
	if err != nil {
		return err
	}
	if seq.Data.NumberSequence == nil {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/sequence_ref", "Not a number sequence")
	}
	h.req.Sequence = &docs.Sequence{Ref: docRef(polRef(*h.cmd.SequenceRef)), Hash: seq.ContentHash, DocumentType: seq.Data.NumberSequence.DocumentType}
	seller, err := in(polRef(*h.cmd.SellerRef), "/args/seller_ref")
	if err != nil {
		return err
	}
	if p := seller.Data.CompanyProfile; p != nil {
		h.req.Seller = &docs.Seller{Ref: docRef(polRef(*h.cmd.SellerRef)), Hash: seller.ContentHash, LegalName: p.LegalName,
			RegistrationNo: p.RegistrationNo, Country: p.Country, Address: p.Address}
	}
	if c, ok := got[currency]; ok && c.Data.Currency != nil {
		h.up.Currency = &docs.Currency{Code: currency.ConfigID, Revision: currency.Revision, MinorUnits: c.Data.Currency.MinorUnits}
	}
	if err = h.order(ctx, ship); err != nil {
		return err
	}
	// The domain refuses a layout, sequence or seller in the transaction; no number is spent on it.
	if !docs.Numbered(h.req) {
		return nil
	}
	issued, err := h.s.Upstream.Documents.Numbers.IssueNumber(ctx, polapi.IssueNumber{
		IdempotencyKey: numberKey(api.CommandPrepareShipmentDocument, h.cmd.IdempotencyKey), Sequence: polRef(*h.cmd.SequenceRef),
		BusinessRef: h.cmd.DocumentSetID})
	if err != nil {
		if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 && rej.Fields[0].Path == "/args/sequence" {
			return kernel.RejectAs(rej.Code, rej.Category, "/args/sequence_ref", rej.Fields[0].Reason)
		}
		return err
	}
	h.up.Number = issued.ObjectID
	return nil
}

// order reads the shipment's order from TRD and the names of its customer and consignee from PTY.
func (h *prepareDocumentCommand) order(ctx context.Context, ship *shipment.Shipment) error {
	facts, err := h.s.Upstream.Documents.Orders.ResolveOrderSnapshots(ctx, []string{ship.Order.OrderID})
	if err != nil {
		return err
	}
	f, ok := facts[ship.Order.OrderID]
	if !ok || f.Code != "" {
		return nil
	}
	o := &docs.OrderFacts{Binding: docs.OrderBinding{OrderID: f.Binding.OrderID, OrderRevision: f.Binding.OrderRevision, OrderHash: f.Binding.OrderHash},
		Incoterm: f.TradeTerms.Incoterm, Edition: f.TradeTerms.Edition, NamedPlace: f.TradeTerms.NamedPlace.ID}
	for _, st := range f.Stages {
		o.Stages = append(o.Stages, docs.Stage{StageID: st.StageID, Percent: st.Percent, Trigger: st.Trigger, Days: st.Days, DayBasis: st.DayBasis})
	}
	for _, a := range f.Addresses {
		o.Addresses = append(o.Addresses, docs.Address{Role: a.Role, PartyID: a.PartyID, PartyRevision: a.PartyRevision, Text: a.Address})
	}
	h.up.Order = o
	h.up.Names = map[docs.PartyKey]string{}
	keys := []docs.PartyKey{}
	for _, p := range ship.Parties {
		if p.Role == shipment.RoleCustomer {
			keys = append(keys, docs.PartyKey{PartyID: p.PartyID, Revision: p.PartyRevision})
		}
	}
	if a := o.Address("consignee"); a != nil {
		keys = append(keys, docs.PartyKey{PartyID: a.PartyID, Revision: a.PartyRevision})
	}
	for _, k := range keys {
		if _, done := h.up.Names[k]; done {
			continue
		}
		name, ok, err := h.s.Upstream.Documents.Parties.PartyName(ctx, k.PartyID, k.Revision)
		if err != nil {
			return err
		}
		if ok {
			h.up.Names[k] = name
		}
	}
	return nil
}

func (h *prepareDocumentCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.set, h.other, h.facts, h.result = nil, "", docs.Facts{}, nil
	sc := scope(ctx)
	shipmentID, err := postgres.ShipmentOfSet(ctx, exec, sc, h.cmd.DocumentSetID)
	if err != nil {
		return err
	}
	if shipmentID == "" {
		shipmentID = h.cmd.ShipmentID
	}
	if err = loadFacts(ctx, exec, sc, shipmentID, &h.facts); err != nil {
		return err
	}
	if h.set, err = postgres.LoadSet(ctx, exec, sc, h.cmd.DocumentSetID, postgres.ForUpdate); err != nil || h.set != nil {
		return err
	}
	h.other, err = postgres.SetOfShipment(ctx, exec, sc, shipmentID)
	return err
}

// loadFacts reads shipment id FOR SHARE with its prices and current booking.
func loadFacts(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string, f *docs.Facts) error {
	var err error
	if f.Shipment, err = postgres.LoadShipment(ctx, exec, sc, id, postgres.ForShare); err != nil || f.Shipment == nil {
		return err
	}
	if f.Prices, err = postgres.LoadPrices(ctx, exec, sc, id); err != nil {
		return err
	}
	f.Booking, err = postgres.CurrentBooking(ctx, exec, sc, id)
	return err
}

func (h *prepareDocumentCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	set, sh := h.set, h.facts.Shipment
	switch {
	case set != nil && !d.Sees(set.OwnerActor, set.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/document_set_id", "")
	case set != nil:
		if err := set.Open(h.cmd.ExpectedVersion, h.cmd.ShipmentID); err != nil {
			return action.Outcome{}, err
		}
	case h.cmd.ExpectedVersion != 0:
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/document_set_id", "")
	case h.other != "":
		return action.Outcome{}, docs.Reject(docs.CodeBusinessKeyConflict, "/args/shipment_id", "The shipment's document set is "+h.other)
	}
	if sh == nil || !d.Sees(sh.OwnerActor, sh.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/shipment_id", "No shipment the actor reads")
	}
	now := kernel.Instant(h.s.Clock.Now())
	act := docs.Act{By: d.Actor, Epoch: d.Epoch, At: now}
	fresh := set == nil
	if fresh {
		set = docs.NewSet(h.cmd.DocumentSetID, sh, d.Team, act)
	}
	doc, err := docs.Prepare(set, h.req, docs.Facts{Shipment: sh, Prices: h.facts.Prices, Booking: h.facts.Booking}, h.up, act)
	if err != nil {
		return action.Outcome{}, err
	}
	var upstream []docs.Entry
	if docs.Rendered(doc.Kind) {
		if upstream, err = h.s.guardConfigs(ctx, exec, h.guards, now); err != nil {
			return action.Outcome{}, err
		}
	}
	from := set.RowVersion
	superseded := set.Add(doc)
	doc.Entries = prepareEntries(set, doc, d, upstream, now)
	doc.EntriesJSON, doc.EntriesSHA256 = shipment.Seal(doc.Entries)
	sc := scope(ctx)
	if fresh {
		err = postgres.InsertSet(ctx, exec, sc, set)
	} else if superseded != nil {
		err = postgres.MoveDocument(ctx, exec, sc, set.ID, superseded)
	}
	if err == nil {
		err = postgres.InsertDocument(ctx, exec, sc, set.ID, doc)
	}
	if err == nil && !fresh {
		err = conflict(postgres.MoveSet(ctx, exec, sc, set, from))
	}
	if err != nil {
		return action.Outcome{}, err
	}
	h.result = doc
	return action.Outcome{ObjectID: set.ID, RowVersion: set.RowVersion, Status: docs.Draft}, nil
}

// prepareEntries is a version's commit evidence: what it is, what it was rendered from with POL's
// verdicts in the transaction, its files, who prepared it under which epoch, when.
func prepareEntries(set *docs.Set, d *docs.Document, dec authorization.Decision, upstream []docs.Entry, now time.Time) []docs.Entry {
	out := []docs.Entry{{Name: "command", Value: api.CommandPrepareShipmentDocument}, {Name: "document_set_id", Value: set.ID},
		{Name: "shipment_id", Value: set.ShipmentID}, {Name: "document_kind", Value: d.Kind}, {Name: "version", Value: strconv.FormatInt(d.Version, 10)},
		{Name: "set_version", Value: strconv.FormatInt(d.SetVersion, 10)}, {Name: "number", Value: d.Number},
		{Name: "content_sha256", Value: d.ContentSHA256}}
	if d.Sequence != nil {
		out = append(out, docs.Entry{Name: "sequence_ref", Value: d.Sequence.String()}, docs.Entry{Name: "seller_ref", Value: d.Seller.String()},
			docs.Entry{Name: "renderer_version", Value: d.RendererVersion})
	}
	for _, r := range d.Renditions {
		out = append(out, docs.Entry{Name: "rendition:" + r.Language, Value: r.FileName + " " + r.SHA256 + " " + strconv.Itoa(r.Size) + " " +
			r.Template.String() + " " + r.TemplateHash})
	}
	if f := d.File; f != nil {
		out = append(out, docs.Entry{Name: "file", Value: f.ID + " " + f.SHA256 + " " + f.MimeType})
	}
	out = append(out, d.Sources...)
	if len(upstream) > 0 {
		out = append(out, docs.Entry{Name: "upstream_checked_at", Value: kernel.FormatTimestamp(now)})
		out = append(out, upstream...)
	}
	return append(out, docs.Entry{Name: "prepared_by", Value: dec.Actor.String()}, docs.Entry{Name: "authority_epoch",
		Value: strconv.FormatInt(dec.Epoch, 10)}, docs.Entry{Name: "prepared_at", Value: kernel.FormatTimestamp(now)},
		docs.Entry{Name: "idempotency_key", Value: d.IdempotencyKey})
}

func particulars(p *api.Particulars) docs.Particulars {
	if p == nil {
		return docs.Particulars{}
	}
	out := docs.Particulars{Marks: p.Marks, PortOfLoading: p.PortOfLoading, PortOfDischarge: p.PortOfDischarge, Names: map[string]map[string]string{}}
	if c := p.Consignee; c != nil {
		out.Consignee = &docs.Party{Name: c.Name, Address: c.Address}
	}
	if n := p.Notify; n != nil {
		out.Notify = &docs.Party{Name: n.Name, Address: n.Address}
	}
	for _, d := range p.Descriptions {
		if out.Names[d.SkuID] == nil {
			out.Names[d.SkuID] = map[string]string{}
		}
		out.Names[d.SkuID][d.Language] = d.Text
		out.NameOrder = append(out.NameOrder, docs.NameRef{SkuID: d.SkuID, Language: d.Language})
	}
	return out
}

func documentParty(p *api.DocumentParty) *docs.PartyContent {
	if p == nil {
		return nil
	}
	return &docs.PartyContent{Name: p.Name, Address: p.Address}
}

// PrepareShipmentDocument prepares a new draft version of a kind in a shipment's document set
// (shipment_document.prepare; SM-FUL-DOCUMENT, FUL-INV-004, R49, CN-AT17).
func (s Service) PrepareShipmentDocument(ctx context.Context, cmd api.PrepareShipmentDocument) (api.DocumentReceipt, error) {
	h := &prepareDocumentCommand{s: s, cmd: cmd}
	err := cmd.Validate()
	if err == nil {
		h.req = docs.Request{Kind: cmd.DocumentKind, Particulars: particulars(cmd.Particulars), Evidence: slices.Clone(cmd.EvidenceIDs),
			IdempotencyKey: cmd.IdempotencyKey}
		if len(cmd.Packages) > 0 {
			h.req.Packages = packages(cmd.Packages)
		}
		if b := cmd.BillOfLading; b != nil {
			h.req.FileID = b.FileID
			h.req.BillOfLading = &docs.BillOfLading{Number: b.Number, Vessel: b.Vessel, Voyage: b.Voyage, PortOfLoading: b.PortOfLoading,
				PortOfDischarge: b.PortOfDischarge, ShippedOnBoard: b.ShippedOnBoard, Consignee: *documentParty(&b.Consignee),
				Notify: documentParty(b.Notify), Marks: b.Marks, Packages: b.Packages, GrossWeightKg: b.GrossWeightKg, VolumeCbm: b.VolumeCbm,
				ContainerNumbers: append([]string{}, b.ContainerNumbers...)}
		}
		if c := cmd.Certificate; c != nil {
			h.req.FileID = c.FileID
			h.req.Certificate = &docs.Certificate{Number: c.Number, Issuer: c.Issuer, IssuedOn: c.IssuedOn}
			if r := c.CertificateRef; r != nil {
				h.req.Certificate.CertificateID, h.req.Certificate.CertificateVersion = r.CertificateID, r.Version
			}
		}
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandPrepareShipmentDocument, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.DocumentSetID},
		cmd, err, h)
	if err != nil {
		return api.DocumentReceipt{}, err
	}
	out := api.DocumentReceipt{Receipt: res, DocumentKind: cmd.DocumentKind, Renditions: []api.RenditionReceipt{}}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		set, err := postgres.LoadSet(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || set == nil {
			return err
		}
		for _, d := range set.Versions(cmd.DocumentKind) {
			if d.SetVersion != res.RowVersion && d.IdempotencyKey != cmd.IdempotencyKey {
				continue
			}
			out.Version, out.Number, out.Renditions = d.Version, d.Number, []api.RenditionReceipt{}
			for _, r := range d.Renditions {
				out.Renditions = append(out.Renditions, api.RenditionReceipt{Language: r.Language, FileName: r.FileName, SHA256: r.SHA256, SizeBytes: r.Size})
			}
		}
		return nil
	})
	return out, err
}

// issueCommand runs shipment_document.issue (human only).
type issueCommand struct {
	s      Service
	cmd    api.IssueShipmentDocuments
	set    *docs.Set
	facts  docs.Facts
	result *docs.Issue
}

func (h *issueCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.set, h.facts, h.result = nil, docs.Facts{}, nil
	sc := scope(ctx)
	shipmentID, err := postgres.ShipmentOfSet(ctx, exec, sc, h.cmd.DocumentSetID)
	if err != nil || shipmentID == "" {
		return err
	}
	if err = loadFacts(ctx, exec, sc, shipmentID, &h.facts); err != nil {
		return err
	}
	h.set, err = postgres.LoadSet(ctx, exec, sc, h.cmd.DocumentSetID, postgres.ForUpdate)
	return err
}

func (h *issueCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	set := h.set
	if set == nil || !d.Sees(set.OwnerActor, set.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/document_set_id", "")
	}
	if h.facts.Shipment == nil {
		return action.Outcome{}, errors.New("ful: the document set's shipment is missing")
	}
	now := kernel.Instant(h.s.Clock.Now())
	diffs := docs.Reconcile(set, h.facts)
	acks := make([]docs.Ack, len(h.cmd.Acknowledgements))
	for i, a := range h.cmd.Acknowledgements {
		acks[i] = docs.Ack{Path: a.Path, Reason: a.Reason}
	}
	from := set.RowVersion
	drafts := set.Drafts()
	iss, superseded, err := set.Issue(h.cmd.ExpectedVersion, h.facts.Shipment.Status, diffs, acks, docs.Act{By: d.Actor, Epoch: d.Epoch, At: now})
	if err != nil {
		return action.Outcome{}, err
	}
	var gs []guarded
	for _, x := range drafts {
		path := "/" + x.Kind + "/" + strconv.FormatInt(x.Version, 10)
		for _, r := range x.Renditions {
			gs = append(gs, guarded{ref: polapi.ConfigRef{Kind: r.Template.Kind, ConfigID: r.Template.ID, Revision: r.Template.Revision},
				hash: r.TemplateHash, path: path + "/templates/" + r.Language})
		}
		if x.Sequence != nil {
			gs = append(gs, guarded{ref: polapi.ConfigRef{Kind: x.Sequence.Kind, ConfigID: x.Sequence.ID, Revision: x.Sequence.Revision},
				hash: x.SequenceHash, path: path + "/sequence"})
			gs = append(gs, guarded{ref: polapi.ConfigRef{Kind: x.Seller.Kind, ConfigID: x.Seller.ID, Revision: x.Seller.Revision},
				hash: x.SellerHash, path: path + "/seller"})
		}
	}
	upstream, err := h.s.guardConfigs(ctx, exec, gs, now)
	if err != nil {
		return action.Outcome{}, err
	}
	iss.IdempotencyKey = h.cmd.IdempotencyKey
	iss.Entries = []docs.Entry{{Name: "command", Value: api.CommandIssueShipmentDocuments}, {Name: "document_set_id", Value: set.ID},
		{Name: "shipment_id", Value: set.ShipmentID}, {Name: "issue_seq", Value: strconv.FormatInt(iss.Seq, 10)},
		{Name: "set_version", Value: strconv.FormatInt(iss.SetVersion, 10)}, {Name: "reconciliation_sha256", Value: iss.ReconciliationSHA256},
		{Name: "differences", Value: strconv.Itoa(len(diffs))}, {Name: "acknowledged", Value: strconv.Itoa(len(iss.Acknowledged))}}
	for _, x := range iss.Documents {
		iss.Entries = append(iss.Entries, docs.Entry{Name: "issued:" + x.Kind, Value: "v" + strconv.FormatInt(x.Version, 10) + " " + x.Number + " " +
			x.ContentSHA256})
	}
	for _, x := range superseded {
		iss.Entries = append(iss.Entries, docs.Entry{Name: "superseded:" + x.Kind, Value: "v" + strconv.FormatInt(x.Version, 10)})
	}
	iss.Entries = append(iss.Entries, docs.Entry{Name: "upstream_checked_at", Value: kernel.FormatTimestamp(now)})
	iss.Entries = append(iss.Entries, upstream...)
	iss.Entries = append(iss.Entries, docs.Entry{Name: "issued_by", Value: d.Actor.String()}, docs.Entry{Name: "authority_epoch",
		Value: strconv.FormatInt(d.Epoch, 10)}, docs.Entry{Name: "issued_at", Value: kernel.FormatTimestamp(now)})
	iss.EntriesJSON, iss.EntriesSHA256 = shipment.Seal(iss.Entries)
	sc := scope(ctx)
	for _, x := range superseded {
		if err = postgres.MoveDocument(ctx, exec, sc, set.ID, x); err != nil {
			return action.Outcome{}, err
		}
	}
	for _, x := range drafts {
		if err = postgres.MoveDocument(ctx, exec, sc, set.ID, x); err != nil {
			return action.Outcome{}, err
		}
	}
	if err = postgres.InsertIssue(ctx, exec, sc, set.ID, iss); err != nil {
		return action.Outcome{}, err
	}
	if err = conflict(postgres.MoveSet(ctx, exec, sc, set, from)); err != nil {
		return action.Outcome{}, err
	}
	h.result = iss
	documents := make([]map[string]any, 0, len(iss.Documents))
	for _, x := range iss.Documents {
		documents = append(documents, map[string]any{"document_kind": x.Kind, "version": x.Version, "number": x.Number, "content_sha256": x.ContentSHA256})
	}
	event := action.Event{Type: api.EventShipmentDocumentIssued, AggregateType: "ShipmentDocumentSet", AggregateID: set.ID,
		AggregateVersion: set.RowVersion, Fields: map[string]any{"document_set_id": set.ID, "shipment_id": set.ShipmentID, "order_id": set.Order.OrderID,
			"order_revision": set.Order.OrderRevision, "order_hash": set.Order.OrderHash, "issue_seq": iss.Seq, "documents": documents,
			"reconciliation_sha256": iss.ReconciliationSHA256, "acknowledged": len(iss.Acknowledged), "issued_at": kernel.FormatTimestamp(now)}}
	return action.Outcome{ObjectID: set.ID, RowVersion: set.RowVersion, Status: docs.Issued, Events: []action.Event{event}}, nil
}

// IssueShipmentDocuments issues every draft of a set (shipment_document.issue, human only;
// SM-FUL-DOCUMENT, FUL-INV-004, BT12, R49).
func (s Service) IssueShipmentDocuments(ctx context.Context, cmd api.IssueShipmentDocuments) (api.IssueReceipt, error) {
	h := &issueCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandIssueShipmentDocuments, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.DocumentSetID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.IssueReceipt{}, err
	}
	out := api.IssueReceipt{Receipt: res, Issued: []api.DocumentRef{}}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		set, err := postgres.LoadSet(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || set == nil {
			return err
		}
		for _, i := range set.Issues {
			if i.SetVersion != res.RowVersion {
				continue
			}
			out.ReconciliationSHA256 = i.ReconciliationSHA256
			for _, x := range i.Documents {
				out.Issued = append(out.Issued, api.DocumentRef{DocumentKind: x.Kind, Version: x.Version})
			}
		}
		return nil
	})
	return out, err
}

// signCommand runs shipment_document.record_signed (human only).
type signCommand struct {
	s       Service
	cmd     api.RecordSignedShipmentDocument
	sources map[string]bool
	file    *docs.File
	set     *docs.Set
}

func (h *signCommand) Prepare(ctx context.Context) error {
	var err error
	if h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs); err != nil {
		return err
	}
	f, err := h.s.Upstream.Sources.GetFile(ctx, h.cmd.FileID)
	if rej, ok := kernel.AsRejection(err); ok && (rej.Code == kernel.CodeNotFound || rej.Code == kernel.CodePermissionDenied) {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/file_id", "No file the actor reads")
	}
	if err != nil {
		return err
	}
	h.file = &docs.File{ID: f.FileID, SHA256: f.ContentSHA256, MimeType: f.MimeType}
	return nil
}

func (h *signCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.set, err = postgres.LoadSet(ctx, exec, scope(ctx), h.cmd.DocumentSetID, postgres.ForUpdate)
	return err
}

func (h *signCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	set := h.set
	if set == nil || !d.Sees(set.OwnerActor, set.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/document_set_id", "")
	}
	if h.file == nil || h.file.ID != h.cmd.FileID {
		return action.Outcome{}, errors.New("ful: the signed copy's file was not read")
	}
	sig, replayed, err := set.Sign(docs.SignRequest{Kind: h.cmd.DocumentKind, Version: h.cmd.Version, Language: h.cmd.Language, File: *h.file,
		Evidence: slices.Clone(h.cmd.EvidenceIDs), IdempotencyKey: h.cmd.IdempotencyKey}, h.sources, docs.Act{By: d.Actor, Epoch: d.Epoch,
		At: h.s.Clock.Now()})
	if err != nil {
		return action.Outcome{}, err
	}
	if !replayed {
		if err = postgres.InsertSignature(ctx, exec, scope(ctx), set.ID, h.cmd.DocumentKind, h.cmd.Version, sig); err != nil {
			return action.Outcome{}, err
		}
	}
	return action.Outcome{ObjectID: set.ID, RowVersion: set.RowVersion, Status: api.StatusSigned}, nil
}

// RecordSignedShipmentDocument registers the signed or stamped copy of one language of an issued
// CI or PL version (shipment_document.record_signed, human only; CN-AT17).
func (s Service) RecordSignedShipmentDocument(ctx context.Context, cmd api.RecordSignedShipmentDocument) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandRecordSignedShipmentDocument, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.DocumentSetID},
		cmd, cmd.Validate(), &signCommand{s: s, cmd: cmd})
}
