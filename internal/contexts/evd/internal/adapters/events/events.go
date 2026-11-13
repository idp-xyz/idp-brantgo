// Package events holds EVD's Inbox consumer: the inbound adapter that turns TRD's formal document
// events into calls on EVD's application (repository structure §07). The Dispatcher runs the handler
// in a transaction on EVD's own transaction owner, as the consumer's system actor, once per event ID
// (foundation/messaging).
//
// evd.formal_files files TRD's formal documents as business files (Context Map R65 TRD→EVD,
// Published Language events, contract "TRD 集成事件 v1、FormalDocument 读取", no guard lock; S4
// decision D-8; WBS S4-06c; IG-08): QuoteActivated and QuoteReplaced announce a quote revision's
// formal document, OrderDocumentGenerated an order revision's S/C or PI. Prepare reads the document
// through TRD's FormalDocument read before EVD's transaction and checks its bytes against the SHA-256
// TRD sealed; the handler registers it once (application.PrepareFormalFile, RegisterFormalFile).
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/application"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/domain/file"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// FormalFiles is the consumer of TRD's formal document events. Its handler runs as
// system:inbox.evd.formal_files, which brant-admin inbox grants FormalFilePermissions in every
// company it serves.
const FormalFiles eventing.InboxConsumer = "evd.formal_files"

// The failure codes of the events the consumer refuses for good: a payload that is not the one TRD
// writes, a document whose bytes are not the ones TRD sealed (or that EVD holds with other bytes),
// a document EVD cannot keep as a business file.
const (
	FailureEventInvalid eventing.FailureCode = application.FailureFormalEventInvalid
	FailureMismatch     eventing.FailureCode = application.FailureFormalMismatch
	FailureNotFileable  eventing.FailureCode = application.FailureFormalNotFileable
)

// trdRead is TRD's read permission, which its FormalDocument read (quote.document.get,
// order.document.get) asks of the consumer's actor in Prepare.
const trdRead = "trd.read"

// FormalFilePermissions are what the consumer needs: formal_file.register, its own system command;
// evd.read, which the command ledger asks for before it answers a command from its stored result;
// and trd.read, for TRD's FormalDocument read.
func FormalFilePermissions() []string {
	return []string{api.CommandRegisterFormalFile, application.PermissionRead, trdRead}
}

// EventTypes are TRD's events that announce a formal document.
var EventTypes = []eventing.EventType{trdapi.EventQuoteActivated, trdapi.EventQuoteReplaced, trdapi.EventOrderDocumentGenerated}

// Files is what evd.formal_files asks of EVD's application; application.Service implements it.
type Files interface {
	PrepareFormalFile(ctx context.Context, e application.FormalEvent) (application.PreparedFormal, error)
	RegisterFormalFile(ctx context.Context, exec bentopg.Executor, p application.PreparedFormal) (application.FormalOutcome, error)
}

// Subscriptions returns EVD's subscriptions on db, EVD's own transaction owner.
func Subscriptions(db *bentopg.DB, app Files) []messaging.Subscription {
	return []messaging.Subscription{{
		Consumer:    FormalFiles,
		Source:      trdapi.Source,
		EventTypes:  EventTypes,
		DB:          db,
		Permissions: FormalFilePermissions(),
		Prepare:     FormalPrepare(app),
		Handler:     FormalHandler(app),
	}}
}

// FormalPrepare reads and checks, before EVD's transaction, the document the event announces
// (application.PrepareFormalFile). A payload that is not TRD's and a document whose bytes are not the
// sealed ones are refused for good; a failed read is retried.
func FormalPrepare(files Files) func(ctx context.Context, envelope eventing.Envelope) (any, error) {
	return func(ctx context.Context, envelope eventing.Envelope) (any, error) {
		e, err := FormalEventOf(envelope)
		if err != nil {
			return nil, messaging.RejectEvent(FailureEventInvalid, err)
		}
		p, err := files.PrepareFormalFile(ctx, e)
		return p, refusal(err)
	}
}

// FormalHandler registers the document FormalPrepare read (application.RegisterFormalFile). A
// document EVD holds with other bytes is refused for good; every other failure — the consumer's
// actor not provisioned, a concurrent registration, the database — rolls the transaction back and
// the event is delivered again.
func FormalHandler(files Files) messaging.EventFunc {
	return func(ctx context.Context, e messaging.Event) error {
		p, ok := e.Prepared.(application.PreparedFormal)
		if !ok {
			return fmt.Errorf("evd: %s delivered without its prepared document", FormalFiles)
		}
		if ev, err := FormalEventOf(e.Envelope); err != nil || ev != p.Event {
			return fmt.Errorf("evd: %s prepared another event than %s", FormalFiles, e.Envelope.ID)
		}
		_, err := files.RegisterFormalFile(ctx, e.Exec, p)
		return refusal(err)
	}
}

// refusal turns the application's refusal for good into the Inbox's.
func refusal(err error) error {
	var r *application.FormalRefusal
	if errors.As(err, &r) {
		return messaging.RejectEvent(eventing.FailureCode(r.Code), r)
	}
	return err
}

// formalPayload is what the consumer reads of TRD's formal document events (contract C4: identifiers,
// versions and hashes; TRD's formalization and order.document.generate write them).
type formalPayload struct {
	AggregateType    string `json:"aggregate_type"`
	AggregateID      string `json:"aggregate_id"`
	AggregateVersion int64  `json:"aggregate_version"`
	QuoteID          string `json:"quote_id"`
	BusinessRevision int64  `json:"business_revision"`
	BusinessHash     string `json:"business_hash"`
	ReplacedRevision int64  `json:"replaced_revision"` // QuoteReplaced
	OrderID          string `json:"order_id"`          // OrderDocumentGenerated, as every order event
	DocumentKind     string `json:"document_kind"`
	DocumentRevision int64  `json:"document_order_revision"`
	DocumentHash     string `json:"document_order_hash"`
	Number           string `json:"number"`
	FileName         string `json:"file_name"`
	MimeType         string `json:"mime_type"`
	FileSHA256       string `json:"file_sha256"`
	FileSize         int    `json:"file_size"`
}

// maxEventID is the length formal_file keeps of an event ID.
const maxEventID = 128

// FormalEventOf reads envelope as one of TRD's formal document events, or says what its payload
// lacks: QuoteActivated and QuoteReplaced of a QuoteRoot (a replacement names the revision it
// superseded, an activation none), OrderDocumentGenerated of a SalesOrder (the document's kind,
// order revision and hash, and the quote revision the order came from).
func FormalEventOf(envelope eventing.Envelope) (application.FormalEvent, error) {
	var p formalPayload
	if err := json.Unmarshal(envelope.Payload, &p); err != nil {
		return application.FormalEvent{}, fmt.Errorf("evd: formal document event payload: %w", err)
	}
	fail := func(what string) (application.FormalEvent, error) {
		return application.FormalEvent{}, fmt.Errorf("evd: formal document event %s of type %s: %s", envelope.ID, envelope.Type, what)
	}
	if envelope.ID == "" || len(envelope.ID) > maxEventID {
		return fail("the event ID is empty or longer than 128 bytes")
	}
	f := file.Formal{QuoteID: p.QuoteID, BusinessRevision: p.BusinessRevision, BusinessHash: p.BusinessHash, Number: p.Number,
		FileName: p.FileName, MimeType: p.MimeType, SHA256: p.FileSHA256, Size: p.FileSize}
	switch envelope.Type {
	case trdapi.EventQuoteActivated, trdapi.EventQuoteReplaced:
		f.Kind, f.ReplacedRevision = file.FormalQuote, p.ReplacedRevision
		switch {
		case p.AggregateType != "QuoteRoot":
			return fail("aggregate_type is not QuoteRoot")
		case !kernel.ValidIdentifier(p.QuoteID) || p.QuoteID != p.AggregateID:
			return fail("quote_id is not the aggregate's identifier")
		case (envelope.Type == trdapi.EventQuoteReplaced) != (p.ReplacedRevision > 0):
			return fail("replaced_revision belongs to a replacement only")
		case p.OrderID != "" || p.DocumentKind != "" || p.DocumentRevision != 0 || p.DocumentHash != "":
			return fail("a quote event names an order document")
		}
	case trdapi.EventOrderDocumentGenerated:
		f.Kind, f.OrderID, f.OrderRevision, f.OrderHash = p.DocumentKind, p.OrderID, p.DocumentRevision, p.DocumentHash
		switch {
		case p.AggregateType != "SalesOrder":
			return fail("aggregate_type is not SalesOrder")
		case !kernel.ValidIdentifier(p.OrderID) || p.OrderID != p.AggregateID:
			return fail("order_id is not the aggregate's identifier")
		case !kernel.ValidIdentifier(p.QuoteID):
			return fail("quote_id is not an identifier")
		case p.DocumentKind != trdapi.DocumentSalesContract && p.DocumentKind != trdapi.DocumentProformaInvoice:
			return fail("document_kind is not sales_contract or proforma_invoice")
		case p.ReplacedRevision != 0:
			return fail("an order document replaces no revision")
		}
	default:
		return fail("not a formal document event " + string(FormalFiles) + " handles")
	}
	if !f.Valid() {
		return fail("the document's revisions, hashes, number, file, SHA-256 or size are missing or malformed")
	}
	return application.FormalEvent{EventID: string(envelope.ID), EventType: string(envelope.Type), Sealed: f}, nil
}
