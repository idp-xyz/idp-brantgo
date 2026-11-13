// Package application runs EVD's commands and queries on the command ledger. Source rules live in
// domain/source; SQL lives in adapters/postgres.
package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/domain/file"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/domain/source"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Read commands and their rules.
const (
	CommandGetFile        = "file.get"
	CommandGetSource      = "source.get"
	CommandGetExtract     = "source.extract.get"
	CommandResolveSources = "source.resolve"
	CommandAudit          = "evd.audit"
	PermissionRead        = "evd.read"
	PermissionAudit       = "evd.audit"
)

var anyActor = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}

// Rules is EVD's authorization policy.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandRegisterSource, Permission: api.CommandRegisterSource, ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		{Command: api.CommandRegisterFile, Permission: api.CommandRegisterFile, ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		{Command: CommandGetSource, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandGetFile, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandGetExtract, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandResolveSources, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandAudit, Permission: PermissionAudit, ActorKinds: anyActor},
		// The formal-file consumer's command (formal.go): its system actor only (R65, WBS S4-06c).
		{Command: api.CommandRegisterFormalFile, Permission: api.CommandRegisterFormalFile, ActorKinds: []kernel.ActorKind{kernel.ActorSystem}},
		{Command: CommandGetFormalFile, Permission: PermissionRead, ActorKinds: anyActor},
	}
}

// Visible reports whether d's actor sees the source origin or file a stored result names.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	scope, _ := bento.ScopeFromContext(ctx)
	if command == api.CommandRegisterFile || command == api.CommandRegisterFormalFile {
		f, err := postgres.LoadFile(ctx, exec, scope, id, false)
		return err == nil && f != nil && d.Sees(f.OwnerActor, f.OwnerTeam), err
	}
	o, err := postgres.OriginByID(ctx, exec, scope, id)
	return err == nil && o != nil && o.Readable(d.Sees), err
}

// Service runs EVD's use cases.
type Service struct {
	Ledger *action.Ledger
	Clock  application.Clock
	// Extractor reads business files for file_extract sources (internal/ai/ingest, wired by
	// bootstrap); without one, file_extract is UNSUPPORTED_CONTRACT.
	Extractor api.Extractor
	// Formal returns TRD's FormalDocument read for the formal-file consumer (formal.go; R65), bound
	// when asked because EVD is composed before TRD; nil without the consumer.
	Formal func() FormalDocuments
}

// RegisterSource registers a text source (Python Core._register_source).
func (s Service) RegisterSource(ctx context.Context, cmd api.RegisterSource) (api.Receipt, error) {
	if err := cmd.Validate(); err != nil {
		return api.Receipt{}, s.rejectEarly(ctx, api.CommandRegisterSource, cmd.SourceID, err)
	}
	reg := source.Registration{OriginID: cmd.SourceID, Kind: source.Kind(cmd.SourceKind), Text: cmd.ContentText,
		Channel: source.Channel{Provider: cmd.Provider, AccountRef: cmd.AccountRef, ExternalMessageID: cmd.ExternalMessageID, AuthorClaim: cmd.AuthorClaim}}
	r, err := s.RequestOf(cmd)
	if err != nil {
		return api.Receipt{}, err
	}
	h := &register{clock: s.Clock, reg: reg}
	var handler action.Handler = h
	if reg.Kind == source.FileExtract {
		if s.Extractor == nil {
			return api.Receipt{}, s.rejectEarly(ctx, api.CommandRegisterSource, cmd.SourceID,
				kernel.Reject(kernel.CodeUnsupportedContract, "/args/source_kind", "no extractor is configured"))
		}
		handler = &registerExtract{register: h, s: s, fileID: cmd.FileID}
	}
	res, err := s.Ledger.Execute(ctx, action.Command{Name: r.Command, IdempotencyKey: r.IdempotencyKey, RequestHash: r.RequestHash, ObjectID: cmd.SourceID},
		handler)
	return api.Receipt(res), err
}

// rejectEarly audits a request refused before the ledger saw it.
func (s Service) rejectEarly(ctx context.Context, command, objectID string, rejection error) error {
	if rej, ok := kernel.AsRejection(rejection); ok {
		if err := s.Ledger.AuditRejected(ctx, command, objectID, rej.Code); err != nil {
			return errors.Join(rejection, err)
		}
	}
	return rejection
}

type register struct {
	clock application.Clock
	reg   source.Registration
	// extract is the file extract a file_extract registration stores (registerExtract).
	extract *postgres.Extract
}

// Lock locks nothing: origins, contents and imports are immutable, and concurrent first
// registrations meet in the unique constraints (the ledger retries once).
func (h *register) Lock(context.Context, bentopg.Executor) error { return nil }

func (h *register) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	scope, _ := bento.ScopeFromContext(ctx)
	byID, err := postgres.OriginByID(ctx, exec, scope, h.reg.OriginID)
	if err != nil {
		return action.Outcome{}, err
	}
	var byChannel *source.Origin
	if h.reg.Kind == source.EmailText {
		if byChannel, err = postgres.OriginByChannel(ctx, exec, scope, h.reg.Channel); err != nil {
			return action.Outcome{}, err
		}
	}
	// A known origin the actor does not see is NOT_FOUND whatever the body: guessing a body never
	// reveals a private source (Python test_private_source_cannot_be_probed_by_guessing_body).
	for _, o := range []*source.Origin{byID, byChannel} {
		if o != nil && !o.Readable(d.Sees) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/id", "")
		}
	}
	return h.store(ctx, exec, d, byID, byChannel)
}

// store records the registration once the known origins were judged visible.
func (h *register) store(ctx context.Context, exec bentopg.Executor, d authorization.Decision, byID, byChannel *source.Origin) (action.Outcome, error) {
	scope, _ := bento.ScopeFromContext(ctx)
	existing, err := source.Match(h.reg, byID, byChannel)
	if errors.As(err, new(source.Conflict)) {
		return action.Outcome{}, kernel.RejectAs(api.CodeSourceIdentityConflict, kernel.CategoryConflict, "/args/id", "")
	}
	now := h.clock.Now()
	out := action.Outcome{RowVersion: 1, Status: "registered"}
	if existing != nil {
		out.ObjectID = existing.ID
	} else {
		hash := h.reg.ContentHash()
		contentID, err := postgres.ContentByHash(ctx, exec, scope, hash)
		if err != nil {
			return action.Outcome{}, err
		}
		if contentID == "" {
			contentID = action.NewID("content", now)
			if err = postgres.InsertContent(ctx, exec, scope, contentID, hash, h.reg.Text, now); err != nil {
				return action.Outcome{}, err
			}
		}
		o := source.Origin{ID: h.reg.OriginID, ContentID: contentID, Kind: h.reg.Kind, Channel: h.reg.Channel,
			OwnerActor: d.Actor.ID, OwnerTeam: d.Team}
		extractID := ""
		if h.extract != nil {
			if extractID, err = h.storeExtract(ctx, exec, now); err != nil {
				return action.Outcome{}, err
			}
		}
		if err = postgres.InsertOrigin(ctx, exec, scope, o, extractID, now); err != nil {
			return action.Outcome{}, err
		}
		out.ObjectID = o.ID
		out.Events = []action.Event{{Type: api.EventSourceRegistered, AggregateType: "Source", AggregateID: o.ID, AggregateVersion: 1,
			Fields: map[string]any{"source_id": o.ID, "content_sha256": hash}}}
	}
	err = postgres.InsertImport(ctx, exec, scope, action.NewID("import", now), out.ObjectID, d.Actor, now)
	return out, err
}

// storeExtract returns the ID of the stored extract of the registration's file by its extractor
// under its profile, storing it first if it is new. One file read by one extractor version under
// one profile gives one extract (deterministic readers, A6-D15): another located form is a broken
// extractor, not a refusal.
func (h *register) storeExtract(ctx context.Context, exec bentopg.Executor, now time.Time) (string, error) {
	scope, _ := bento.ScopeFromContext(ctx)
	x := *h.extract
	stored, err := postgres.ExtractByFile(ctx, exec, scope, x.FileID, x.Extractor, x.Profile)
	if err != nil {
		return "", err
	}
	if stored != nil {
		if stored.SHA256 != x.SHA256 {
			return "", fmt.Errorf("evd: extractor %s read file %s into another located form (%s, stored %s)", x.Extractor, x.FileID, x.SHA256, stored.SHA256)
		}
		return stored.ID, nil
	}
	x.ID, x.CreatedAt = action.NewID("extract", now), now
	return x.ID, postgres.InsertExtract(ctx, exec, scope, x)
}

// registerExtract registers a file_extract: before its transaction it reads the file's bytes, if
// the registrant sees the file, and has the extractor read them; in the transaction it decides in
// the order: a known origin the registrant does not see (NOT_FOUND /args/id), the file missing or
// not seen (NOT_FOUND /args/file_id), the file outside the input profile (INPUT_PROFILE_UNSUPPORTED
// /args/file_id), another origin under the ID (SOURCE_IDENTITY_CONFLICT), then the registration.
type registerExtract struct {
	*register
	s      Service
	fileID string
	// set by Prepare
	file    *postgres.File
	read    *api.FileExtract
	refusal error
}

// Prepare reads the file with EVD's own read and extracts it (Context Map: EVD holds no parsing
// code; the extractor is the AI layer's). It decides nothing: Apply judges what it found.
func (h *registerExtract) Prepare(ctx context.Context) error {
	h.file, h.read, h.refusal = nil, nil, nil
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		f, err := postgres.LoadFile(ctx, exec, scope, h.fileID, true)
		if err == nil && f != nil && d.Sees(f.OwnerActor, f.OwnerTeam) {
			h.file = f
		}
		return err
	})
	if err != nil || h.file == nil {
		return err
	}
	x, err := h.s.Extractor.Extract(h.file.MimeType, h.file.Content)
	if rej, ok := kernel.AsRejection(err); ok {
		reason := ""
		if len(rej.Fields) > 0 {
			reason = rej.Fields[0].Reason
		}
		h.refusal = kernel.RejectAs(rej.Code, rej.Category, "/args/file_id", reason)
		return nil
	}
	if err != nil {
		return err
	}
	if err = checkExtract(x, h.file); err != nil {
		return err
	}
	h.read = &x
	return nil
}

// checkExtract checks what EVD can check of an extract without parsing it; a failure is a broken
// extractor (an infrastructure error), except a located form beyond EVD's bound, which the input
// decides (INPUT_PROFILE_UNSUPPORTED).
func checkExtract(x api.FileExtract, f *postgres.File) error {
	bad := func(what string) error { return fmt.Errorf("evd: the extractor's answer for file %s: %s", f.ID, what) }
	sum := sha256.Sum256([]byte(x.LocatedForm))
	switch {
	case x.FileSHA256 != f.SHA256:
		return bad("read other bytes than the file's")
	case x.SHA256 != hex.EncodeToString(sum[:]):
		return bad("the SHA-256 is not that of the located form")
	case len(x.LocatedForm) > api.MaxLocatedFormBytes:
		return kernel.RejectAs(api.CodeInputProfileUnsupported, kernel.CategoryInvalid, "/args/file_id",
			"limit=located_form actual="+strconv.Itoa(len(x.LocatedForm))+" max="+strconv.Itoa(api.MaxLocatedFormBytes))
	case !utf8.ValidString(x.LocatedForm) || !utf8.ValidString(x.Text):
		return bad("not UTF-8")
	case utf8.RuneCountInString(x.Text) != x.Characters || x.Characters < 1 || x.Characters > api.MaxContentCharacters:
		return bad("the text does not have the extract's characters within the source bound")
	case x.Kind != "text" && x.Kind != "pdf" && x.Kind != "xlsx", x.Extractor == "", x.Profile == "":
		return bad("kind, extractor or profile")
	}
	return nil
}

func (h *registerExtract) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	scope, _ := bento.ScopeFromContext(ctx)
	byID, err := postgres.OriginByID(ctx, exec, scope, h.reg.OriginID)
	if err != nil {
		return action.Outcome{}, err
	}
	if byID != nil && !byID.Readable(d.Sees) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	}
	// The file as the transaction sees it: still there and seen by the registrant (CT16).
	f, err := postgres.LoadFile(ctx, exec, scope, h.fileID, false)
	if err != nil {
		return action.Outcome{}, err
	}
	if f == nil || !d.Sees(f.OwnerActor, f.OwnerTeam) || h.file == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/file_id", "")
	}
	if h.refusal != nil {
		return action.Outcome{}, h.refusal
	}
	x := h.read
	h.reg.Text = x.Text
	h.reg.Extract = source.ExtractID{FileID: f.ID, SHA256: x.SHA256}
	ref := x.ExtractRef
	ref.FileID = f.ID
	h.extract = &postgres.Extract{ExtractRef: ref, LocatedForm: x.LocatedForm}
	return h.store(ctx, exec, d, byID, nil)
}

// GetSource returns a source the caller sees, audited (Python Core.get kind=source).
func (s Service) GetSource(ctx context.Context, id string) (api.SourceView, error) {
	var view api.SourceView
	err := s.Ledger.Read(ctx, CommandGetSource, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		st, err := postgres.Load(ctx, exec, scope, id)
		if err != nil {
			return err
		}
		if st == nil || !st.Origin.Readable(d.Sees) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		o := st.Origin
		view = api.SourceView{SourceID: o.ID, SourceKind: string(o.Kind), Provider: o.Channel.Provider, AccountRef: o.Channel.AccountRef,
			ExternalMessageID: o.Channel.ExternalMessageID, AuthorClaim: o.Channel.AuthorClaim, ContentSHA256: o.ContentHash,
			ContentText: st.Text, RegisteredAt: st.RegisteredAt}
		for _, i := range st.Imports {
			view.Imports = append(view.Imports, api.SourceImport{ImportID: i.ID, Actor: i.Actor, ImportedAt: i.ImportedAt})
		}
		if o.Kind == source.FileExtract {
			x, err := postgres.OriginExtract(ctx, exec, scope, id)
			if err != nil || x == nil {
				return errors.Join(err, errors.New("evd: a file extract origin without its extract"))
			}
			ref := x.ExtractRef
			view.Extract = &ref
		}
		return nil
	})
	return view, err
}

// GetExtract returns the located form of a file_extract source, audited (api.ExtractQueries;
// CT16: the caller must see the source and its file now).
func (s Service) GetExtract(ctx context.Context, id string) (api.ExtractView, error) {
	var view api.ExtractView
	err := s.Ledger.Read(ctx, CommandGetExtract, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		o, err := postgres.OriginByID(ctx, exec, scope, id)
		if err != nil {
			return err
		}
		if o == nil || o.Kind != source.FileExtract || !o.Readable(d.Sees) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		x, err := postgres.OriginExtract(ctx, exec, scope, id)
		if err != nil || x == nil {
			return errors.Join(err, errors.New("evd: a file extract origin without its extract"))
		}
		view = api.ExtractView{SourceID: id, ExtractRef: x.ExtractRef, LocatedForm: x.LocatedForm, RegisteredAt: x.CreatedAt}
		return nil
	})
	return view, err
}

// ResolveSources returns the readable sources among ids.
func (s Service) ResolveSources(ctx context.Context, ids []string) (map[string]api.SourceRef, error) {
	out := map[string]api.SourceRef{}
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) == 0 {
		return out, nil
	}
	err := s.Ledger.Lookup(ctx, CommandResolveSources, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		origins, err := postgres.Origins(ctx, exec, scope, ids)
		for _, o := range origins {
			if o.Readable(d.Sees) {
				out[o.ID] = api.SourceRef{SourceID: o.ID, ContentSHA256: o.ContentHash}
			}
		}
		return err
	})
	return out, err
}

// Audit returns EVD's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}

// RegisterFile registers an immutable business file (Python foundation.file_register).
func (s Service) RegisterFile(ctx context.Context, cmd api.RegisterFile) (api.Receipt, error) {
	if err := cmd.Validate(); err != nil {
		return api.Receipt{}, s.rejectEarly(ctx, api.CommandRegisterFile, cmd.FileID, err)
	}
	r, err := s.RequestOf(cmd)
	if err != nil {
		return api.Receipt{}, err
	}
	res, err := s.Ledger.Execute(ctx, action.Command{Name: r.Command, IdempotencyKey: r.IdempotencyKey, RequestHash: r.RequestHash, ObjectID: cmd.FileID},
		&registerFile{clock: s.Clock, cmd: cmd})
	return api.Receipt(res), err
}

type registerFile struct {
	clock application.Clock
	cmd   api.RegisterFile
}

// Lock locks nothing: files are immutable, and a second successor of one file meets the unique
// index on supersedes_file_id (the ledger retries once and then sees the conflict).
func (h *registerFile) Lock(context.Context, bentopg.Executor) error { return nil }

func (h *registerFile) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	scope, _ := bento.ScopeFromContext(ctx)
	conflict := func(path string) error {
		return kernel.RejectAs(api.CodeBusinessKeyConflict, kernel.CategoryConflict, path, "")
	}
	exists, err := postgres.FileExists(ctx, exec, scope, h.cmd.FileID)
	if err != nil {
		return action.Outcome{}, err
	}
	if exists {
		return action.Outcome{}, conflict("/args/id")
	}
	content, sum, err := file.Decode(h.cmd.FileName, h.cmd.MimeType, h.cmd.ContentBase64, api.MaxFileBytes)
	if err != nil {
		return action.Outcome{}, kernel.RejectAs(api.CodeFileInvalid, kernel.CategoryInvalid, "/args/content_base64", "")
	}
	if prev := h.cmd.SupersedesFileID; prev != "" {
		f, err := postgres.LoadFile(ctx, exec, scope, prev, false)
		if err != nil {
			return action.Outcome{}, err
		}
		if f == nil || !d.Sees(f.OwnerActor, f.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/supersedes_file_id", "")
		}
		taken, err := postgres.HasSuccessor(ctx, exec, scope, prev)
		if err != nil {
			return action.Outcome{}, err
		}
		if taken {
			return action.Outcome{}, conflict("/args/supersedes_file_id")
		}
	}
	f := postgres.File{ID: h.cmd.FileID, Name: h.cmd.FileName, MimeType: h.cmd.MimeType, Content: content, SHA256: sum,
		SupersedesFileID: h.cmd.SupersedesFileID, OwnerActor: d.Actor.ID, OwnerTeam: d.Team, RegisteredAt: h.clock.Now()}
	if err = postgres.InsertFile(ctx, exec, scope, f); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: f.ID, RowVersion: 1, Status: "registered", Events: []action.Event{{
		Type: api.EventFileRegistered, AggregateType: "BusinessFile", AggregateID: f.ID, AggregateVersion: 1,
		Fields: map[string]any{"file_id": f.ID, "content_sha256": sum, "byte_size": len(content), "supersedes_file_id": f.SupersedesFileID},
	}}}, nil
}

// GetFile returns a business file the caller sees, audited.
func (s Service) GetFile(ctx context.Context, id string) (api.FileView, error) {
	var v api.FileView
	err := s.Ledger.Read(ctx, CommandGetFile, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		f, err := postgres.LoadFile(ctx, exec, scope, id, true)
		if err != nil {
			return err
		}
		if f == nil || !d.Sees(f.OwnerActor, f.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.FileView{FileID: f.ID, FileName: f.Name, MimeType: f.MimeType, Content: f.Content, ContentSHA256: f.SHA256,
			SupersedesFileID: f.SupersedesFileID, RegisteredAt: f.RegisteredAt}
		x, err := postgres.FormalFileByID(ctx, exec, scope, id)
		if x != nil {
			formal := formalView(x)
			v.Formal = &formal
		}
		return err
	})
	return v, err
}

// RequestOf returns the ledger identity of cmd: its name, its idempotency key and the hash of every
// other member, as executing it hashes them (api.ActionReconciler; ActionReconciliation v1,
// Context Map R66).
func (s Service) RequestOf(cmd api.Command) (kernel.ActionRequest, error) {
	var name, key string
	var args any
	switch c := cmd.(type) {
	case api.RegisterSource:
		name, key, c.IdempotencyKey = api.CommandRegisterSource, c.IdempotencyKey, ""
		args = c
	case api.RegisterFile:
		name, key, c.IdempotencyKey = api.CommandRegisterFile, c.IdempotencyKey, ""
		args = c
	default:
		return kernel.ActionRequest{}, kernel.Reject(kernel.CodeSchemaInvalid, "/command", "")
	}
	hash, err := action.RequestHash(args)
	if err != nil {
		return kernel.ActionRequest{}, err
	}
	return kernel.ActionRequest{Command: name, IdempotencyKey: key, RequestHash: hash}, nil
}

// Reconcile reports what became of cmd, sent earlier with its idempotency key, without executing
// it (api.Reconciler; contract 06, 动作对账).
func (s Service) Reconcile(ctx context.Context, cmd api.Command) (api.Reconciliation, error) {
	r, err := s.RequestOf(cmd)
	if err != nil {
		return api.Reconciliation{}, err
	}
	return s.ReconcileHash(ctx, r.Command, r.IdempotencyKey, r.RequestHash)
}

// ReconcileHash reports what became of the command the ledger identity names, without executing
// anything (api.ActionReconciler; ActionReconciliation v1, Context Map R66): the ledger re-checks
// evd.read and the visibility of the source or file the stored result names now.
func (s Service) ReconcileHash(ctx context.Context, command, idempotencyKey, requestHash string) (api.Reconciliation, error) {
	r, err := s.Ledger.Reconcile(ctx, command, idempotencyKey, requestHash)
	if err != nil {
		return api.Reconciliation{}, err
	}
	out := api.Reconciliation{State: r.State, ActionID: r.ActionID, ObjectID: r.ObjectID}
	if r.Result != nil {
		receipt := api.Receipt(*r.Result)
		out.Receipt = &receipt
	}
	return out, nil
}
