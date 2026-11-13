package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/domain/file"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// EVD's half of R65 (WBS S4-06c; Context Map R65 TRD→EVD, Published Language events, contract "TRD
// 集成事件 v1、FormalDocument 读取", no guard lock; S4 decision D-8; IG-08): the Inbox consumer
// evd.formal_files (adapters/events) files TRD's formal documents as business files.
//
//	Prepare, before EVD's transaction,     PrepareFormalFile: TRD's FormalDocument read
//	as the consumer's system actor          (GetFormalDocument, GetOrderDocument, audited by TRD),
//	                                        checked against what the event says TRD sealed
//	                                        (file.Verify) and against EVD's file rules (file.Check)
//	the handler, in EVD's transaction       RegisterFormalFile: the document already filed with the
//	                                        same SHA-256 → nothing; with another → refused; else
//	                                        formal_file.register through EVD's command ledger: the
//	                                        business file, the formal_file row, FileRegistered
//
// Refused for good (FormalRefusal, the adapter's messaging.RejectEvent; dead letter and alert,
// ADR-BR-015, never replayable): bytes that are not the sealed ones — another SHA-256 or size than
// the event and TRD's binding carry, a TRD read that refuses its own document for its integrity, a
// read of another document, number or file — and a document EVD already holds with other bytes
// (evd.formal_document_mismatch); a document that cannot be a business file (evd.formal_document_
// not_fileable). Everything a grant, the configuration or time can cure is retried: TRD's NOT_FOUND
// (the consumer's data scope) and PERMISSION_DENIED, the consumer's own grant, the database.
//
// Idempotent per event: the Inbox deduplicates by event ID, the command's idempotency key is the
// event's, and formal_file holds one row per TRD document and one per event; a second event naming a
// document EVD holds with the same SHA-256 registers nothing. No lock is taken before the ledger's
// authority guard: business files and formal files are immutable, and concurrent registrations of
// one document meet in the unique indexes (the transaction rolls back and the event is retried).

// CommandGetFormalFile is the audited read of the formal file of a TRD document (evd.read).
const CommandGetFormalFile = "file.formal.get"

// Failure codes of the events the consumer refuses for good.
const (
	FailureFormalEventInvalid = "evd.formal_document_event_invalid"
	FailureFormalMismatch     = "evd.formal_document_mismatch"
	FailureFormalNotFileable  = "evd.formal_document_not_fileable"
)

// FormalDocuments is TRD's FormalDocument read as EVD's consumer asks it (R65): a quote revision's
// formal document and an order revision's S/C or PI with their bytes, which TRD checks against its
// binding on every read, and the owner of the quote root. trd.Module implements it.
type FormalDocuments interface {
	GetFormalDocument(ctx context.Context, quoteID string, businessRevision int64) (trdapi.FormalDocument, error)
	GetOrderDocument(ctx context.Context, orderID string, orderRevision int64, documentKind string) (trdapi.OrderDocument, error)
}

// FormalEvent is one of TRD's events that announce a formal document — QuoteActivated,
// QuoteReplaced or OrderDocumentGenerated — as the consumer reads it: the event, and the document
// as TRD sealed it.
type FormalEvent struct {
	EventID   string
	EventType string
	Sealed    file.Formal
}

// FormalRefusal refuses an event for good with Code; the adapter returns it as messaging.RejectEvent.
type FormalRefusal struct {
	Code   string
	Reason string
}

func (e *FormalRefusal) Error() string { return "evd: " + e.Code + ": " + e.Reason }

func refuseFormal(code, reason string) error { return &FormalRefusal{Code: code, Reason: reason} }

// PreparedFormal is what PrepareFormalFile read of TRD: the event, the owner of the quote root (who
// will own the file) and the document's bytes, checked.
type PreparedFormal struct {
	Event      FormalEvent
	OwnerActor string
	OwnerTeam  string
	Content    []byte
}

// formalDocuments is TRD's read, bound when the consumer asks (EVD is composed before TRD).
func (s Service) formalDocuments() FormalDocuments {
	if s.Formal == nil {
		return nil
	}
	return s.Formal()
}

// PrepareFormalFile reads the formal document e announces through TRD's FormalDocument read, as the
// consumer's system actor in e's scope (ctx), outside any transaction, and checks it: the document
// TRD reads must be the one e says TRD sealed, its bytes must hash to the sealed SHA-256
// (file.Verify) and fit EVD's file rules (file.Check). A *FormalRefusal refuses e for good; any other
// error is retried.
func (s Service) PrepareFormalFile(ctx context.Context, e FormalEvent) (PreparedFormal, error) {
	sealed := e.Sealed
	if !sealed.Valid() || !formalEventOf(e.EventType, sealed) {
		return PreparedFormal{}, refuseFormal(FailureFormalEventInvalid, "the event does not name a formal document of its type")
	}
	docs := s.formalDocuments()
	if docs == nil {
		return PreparedFormal{}, errors.New("evd: TRD's formal document read is not wired")
	}
	p := PreparedFormal{Event: e}
	var read file.Formal
	if sealed.Kind == file.FormalQuote {
		d, err := docs.GetFormalDocument(ctx, sealed.QuoteID, sealed.BusinessRevision)
		if err != nil {
			return PreparedFormal{}, trdReadFailure(err)
		}
		f := d.Formal
		read = file.Formal{Kind: file.FormalQuote, QuoteID: d.QuoteID, BusinessRevision: d.BusinessRevision, BusinessHash: d.BusinessHash,
			ReplacedRevision: f.Commit.ReplacedRevision, Number: f.Number, FileName: f.FileName, MimeType: f.MimeType, SHA256: f.DocumentSHA256,
			Size: f.SizeBytes}
		p.Content, p.OwnerActor, p.OwnerTeam = d.Content, d.OwnerActorID, d.OwnerTeamID
	} else {
		d, err := docs.GetOrderDocument(ctx, sealed.OrderID, sealed.OrderRevision, sealed.Kind)
		if err != nil {
			return PreparedFormal{}, trdReadFailure(err)
		}
		v := d.Document
		// TRD's read of an order document does not repeat the quote revision the order came from: the
		// event's (TRD's order binding) is recorded.
		read = file.Formal{Kind: v.DocumentKind, QuoteID: sealed.QuoteID, BusinessRevision: sealed.BusinessRevision,
			BusinessHash: sealed.BusinessHash, OrderID: d.OrderID, OrderRevision: v.OrderRevision, OrderHash: v.OrderHash, Number: v.Number,
			FileName: v.FileName, MimeType: v.MimeType, SHA256: v.DocumentSHA256, Size: v.SizeBytes}
		p.Content, p.OwnerActor, p.OwnerTeam = d.Content, d.OwnerActorID, d.OwnerTeamID
	}
	if err := file.Verify(sealed, read, p.Content); err != nil {
		return PreparedFormal{}, refuseFormal(FailureFormalMismatch, err.Error())
	}
	if _, _, err := file.Check(sealed.FileName, sealed.MimeType, p.Content, api.MaxFileBytes); err != nil {
		return PreparedFormal{}, refuseFormal(FailureFormalNotFileable, err.Error())
	}
	if !kernel.ValidIdentifier(p.OwnerActor) {
		return PreparedFormal{}, fmt.Errorf("evd: TRD names no owner of quote %s", sealed.QuoteID)
	}
	return p, nil
}

// formalEventOf reports whether f is a document of the kind eventType announces.
func formalEventOf(eventType string, f file.Formal) bool {
	switch eventType {
	case trdapi.EventQuoteActivated:
		return f.Kind == file.FormalQuote && f.ReplacedRevision == 0
	case trdapi.EventQuoteReplaced:
		return f.Kind == file.FormalQuote && f.ReplacedRevision > 0
	case trdapi.EventOrderDocumentGenerated:
		return f.Kind == file.FormalSalesContract || f.Kind == file.FormalProformaInvoice
	}
	return false
}

// trdReadFailure is what a failed TRD read means for the event: TRD's refusal to read its own
// document for its integrity — the stored bytes are not the bound ones, or the binding is missing —
// refuses it for good; anything else (NOT_FOUND and PERMISSION_DENIED for the consumer's grant, the
// database) is retried.
func trdReadFailure(err error) error {
	if rej, ok := kernel.AsRejection(err); ok {
		switch rej.Code {
		case trdapi.CodeFormalDocumentMissing, trdapi.CodeFormalDocumentIntegrityFailure, trdapi.CodeDocumentBindingMissing,
			trdapi.CodeBusinessHashMismatch, trdapi.CodeDocumentIntegrityFailure:
			return refuseFormal(FailureFormalMismatch, "TRD refuses to read its own document: "+rej.Code)
		}
	}
	return err
}

// FormalOutcome is what an event left: the business file that holds its document, and whether
// this delivery registered it (false: EVD held it before).
type FormalOutcome struct {
	FileID     string
	Registered bool
}

// formalKey is the idempotency key of the registration of event's document: the same on every
// delivery of the event.
func formalKey(event string) string {
	sum := sha256.Sum256([]byte(event))
	return "evd.ff." + hex.EncodeToString(sum[:20])
}

// formalRequest is what the registration's request hash covers.
type formalRequest struct {
	EventID          string `json:"event_id"`
	EventType        string `json:"event_type"`
	Kind             string `json:"document_kind"`
	QuoteID          string `json:"quote_id"`
	BusinessRevision int64  `json:"business_revision"`
	OrderID          string `json:"order_id"`
	OrderRevision    int64  `json:"order_revision"`
	SHA256           string `json:"document_sha256"`
}

// RegisterFormalFile files the document p read in the consumer's transaction exec (ctx carries its
// scope and the consumer's system principal; EVD's command ledger joins the transaction): nothing
// when EVD holds the document with the same SHA-256, a *FormalRefusal when it holds it with another,
// otherwise formal_file.register.
func (s Service) RegisterFormalFile(ctx context.Context, exec bentopg.Executor, p PreparedFormal) (FormalOutcome, error) {
	sc, _ := bento.ScopeFromContext(ctx)
	sealed := p.Event.Sealed
	held, err := postgres.FormalFileByDocument(ctx, exec, sc, sealed.Key())
	if err != nil {
		return FormalOutcome{}, err
	}
	if held != nil {
		if held.Formal.SHA256 != sealed.SHA256 {
			return FormalOutcome{}, refuseFormal(FailureFormalMismatch, fmt.Sprintf("EVD holds the document as file %s with SHA-256 %s, event %s",
				held.FileID, held.Formal.SHA256, held.EventID))
		}
		return FormalOutcome{FileID: held.FileID}, nil
	}
	hash, err := action.RequestHash(formalRequest{EventID: p.Event.EventID, EventType: p.Event.EventType, Kind: sealed.Kind, QuoteID: sealed.QuoteID,
		BusinessRevision: sealed.BusinessRevision, OrderID: sealed.OrderID, OrderRevision: sealed.OrderRevision, SHA256: sealed.SHA256})
	if err != nil {
		return FormalOutcome{}, err
	}
	h := &registerFormal{clock: s.Clock, p: p, fileID: action.NewID("file", s.Clock.Now())}
	res, err := s.Ledger.Execute(ctx, action.Command{Name: api.CommandRegisterFormalFile, IdempotencyKey: formalKey(p.Event.EventID), RequestHash: hash,
		ObjectID: h.fileID, Reason: "TRD " + p.Event.EventType + " " + p.Event.EventID}, h)
	if err != nil {
		return FormalOutcome{}, err
	}
	return FormalOutcome{FileID: res.ObjectID, Registered: !res.Replayed}, nil
}

// registerFormal stores a formal document as a business file owned by the quote root's owner, and
// records what it holds.
type registerFormal struct {
	clock  application.Clock
	p      PreparedFormal
	fileID string
}

// Lock locks nothing: business files and formal files are immutable, and a concurrent registration
// of the same document meets the unique indexes.
func (h *registerFormal) Lock(context.Context, bentopg.Executor) error { return nil }

func (h *registerFormal) Apply(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) (action.Outcome, error) {
	sc, _ := bento.ScopeFromContext(ctx)
	sealed, now := h.p.Event.Sealed, h.clock.Now()
	f := postgres.File{ID: h.fileID, Name: sealed.FileName, MimeType: sealed.MimeType, Content: h.p.Content, SHA256: sealed.SHA256,
		OwnerActor: h.p.OwnerActor, OwnerTeam: h.p.OwnerTeam, RegisteredAt: now}
	if err := postgres.InsertFile(ctx, exec, sc, f); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.InsertFormalFile(ctx, exec, sc, postgres.FormalFile{FileID: f.ID, Formal: sealed, EventID: h.p.Event.EventID,
		EventType: h.p.Event.EventType, RegisteredAt: now}); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: f.ID, RowVersion: 1, Status: "registered", Events: []action.Event{{
		Type: api.EventFileRegistered, AggregateType: "BusinessFile", AggregateID: f.ID, AggregateVersion: 1,
		Fields: map[string]any{"file_id": f.ID, "content_sha256": f.SHA256, "byte_size": len(f.Content), "supersedes_file_id": ""},
	}}}, nil
}

// GetFormalFile returns the file EVD registered for TRD's formal document ref, audited
// (api.FormalFiles): NOT_FOUND when EVD holds none or the caller does not see the file.
func (s Service) GetFormalFile(ctx context.Context, ref api.FormalDocumentRef) (api.FormalFile, error) {
	if err := ref.Validate(); err != nil {
		return api.FormalFile{}, err
	}
	object := ref.QuoteID
	if ref.Kind != api.FormalQuote {
		object = ref.OrderID
	}
	var v api.FormalFile
	err := s.Ledger.Read(ctx, CommandGetFormalFile, object, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc, _ := bento.ScopeFromContext(ctx)
		x, err := postgres.FormalFileByDocument(ctx, exec, sc, file.DocumentKey{Kind: ref.Kind, QuoteID: ref.QuoteID,
			BusinessRevision: ref.BusinessRevision, OrderID: ref.OrderID, OrderRevision: ref.OrderRevision})
		if err != nil {
			return err
		}
		if x == nil || !d.Sees(x.OwnerActor, x.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/document", "")
		}
		v = formalView(x)
		return nil
	})
	return v, err
}

// formalView is a stored formal file as the reads show it.
func formalView(x *postgres.FormalFile) api.FormalFile {
	d := x.Formal
	k := d.Key()
	return api.FormalFile{FileID: x.FileID, FileSHA256: d.SHA256, Document: api.FormalDocumentRef{Kind: k.Kind, QuoteID: k.QuoteID,
		BusinessRevision: k.BusinessRevision, OrderID: k.OrderID, OrderRevision: k.OrderRevision}, QuoteID: d.QuoteID,
		BusinessRevision: d.BusinessRevision, BusinessHash: d.BusinessHash, ReplacedRevision: d.ReplacedRevision, OrderHash: d.OrderHash,
		Number: d.Number, FileName: d.FileName, MimeType: d.MimeType, SizeBytes: d.Size, EventID: x.EventID, EventType: x.EventType,
		RegisteredAt: x.RegisteredAt}
}
