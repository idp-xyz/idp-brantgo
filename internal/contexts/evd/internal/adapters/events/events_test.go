package events

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/application"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/domain/file"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Unit C4 (WBS S4-06c): the Inbox consumer evd.formal_files (Context Map R65 TRD→EVD; S4 decision
// D-8; IG-08). Python V0.50 has no event consumer; the tests cite design IDs.

var (
	hashQ = strings.Repeat("1", 64)
	hashO = strings.Repeat("2", 64)
	hashF = strings.Repeat("c", 64)
)

// files records what the consumer asks of EVD's application and answers with prepareErr and
// registerErr.
type files struct {
	prepared    []application.FormalEvent
	registered  []application.PreparedFormal
	prepareErr  error
	registerErr error
}

func (f *files) PrepareFormalFile(_ context.Context, e application.FormalEvent) (application.PreparedFormal, error) {
	f.prepared = append(f.prepared, e)
	return application.PreparedFormal{Event: e, OwnerActor: "editor", Content: []byte("doc")}, f.prepareErr
}

func (f *files) RegisterFormalFile(_ context.Context, _ bentopg.Executor, p application.PreparedFormal) (application.FormalOutcome, error) {
	f.registered = append(f.registered, p)
	return application.FormalOutcome{FileID: "file-1", Registered: true}, f.registerErr
}

// envelope is a TRD event of type typ as TRD's ledger writes it, with fields changed (nil deletes
// one).
func envelope(typ eventing.EventType, fields map[string]any) eventing.Envelope {
	p := map[string]any{"tenant_id": "tenant.1", "company_id": "company.A", "actor_kind": "human", "actor_id": "editor",
		"authority_epoch": 3, "action_id": "act-1", "number": "QT-2026-00001", "file_name": "QT-2026-00001.txt", "mime_type": "text/plain",
		"file_sha256": hashF, "file_size": 120}
	switch typ {
	case trdapi.EventQuoteActivated, trdapi.EventQuoteReplaced:
		for k, v := range map[string]any{"aggregate_type": "QuoteRoot", "aggregate_id": "quote.1", "aggregate_version": 9, "quote_id": "quote.1",
			"business_revision": 2, "business_hash": hashQ, "approval_id": "approval.1", "template_ref": "quote_document_template:t:1",
			"renderer_version": "quote-text/0.1.0", "cost_snapshot_id": "snapshot.1", "expires_at": "2027-01-01T00:00:00.000000Z"} {
			p[k] = v
		}
		if typ == trdapi.EventQuoteReplaced {
			p["replaced_revision"] = 1
		}
	default:
		for k, v := range map[string]any{"aggregate_type": "SalesOrder", "aggregate_id": "order.1", "aggregate_version": 4, "order_id": "order.1",
			"quote_id": "quote.1", "business_revision": 1, "business_hash": hashQ, "order_revision": 2, "order_hash": hashO, "order_status": "active",
			"document_kind": "sales_contract", "document_order_revision": 2, "document_order_hash": hashO, "number": "SC-2026-00001",
			"file_name": "SC-2026-00001-r2.txt", "sequence_ref": "number_sequence:s:1", "template_ref": "order_document_template:t:1",
			"template_content_hash": hashF, "renderer_version": "order-text/0.1.0", "generated_at": "2026-11-13T00:00:00.000000Z",
			"generated_by": "editor"} {
			p[k] = v
		}
	}
	for k, v := range fields {
		if v == nil {
			delete(p, k)
			continue
		}
		p[k] = v
	}
	body, _ := json.Marshal(p)
	return eventing.Envelope{ID: "evt-1", Source: trdapi.Source, Type: typ, Payload: body}
}

func withID(e eventing.Envelope, id string) eventing.Envelope {
	e.ID = eventing.EventID(id)
	return e
}

func TestSubscription(t *testing.T) {
	subs := Subscriptions(nil, &files{})
	if len(subs) != 1 {
		t.Fatalf("%d subscriptions", len(subs))
	}
	s := subs[0]
	if s.Consumer != "evd.formal_files" || s.Source != "idp-brant.trd" || s.Prepare == nil || s.Handler == nil ||
		!slices.Equal(s.EventTypes, []eventing.EventType{"idp-brant.trd.quote_activated.v1", "idp-brant.trd.quote_replaced.v1",
			"idp-brant.trd.order_document_generated.v1"}) ||
		!slices.Equal(s.Permissions, []string{"formal_file.register", "evd.read", "trd.read"}) {
		t.Fatalf("%+v", s)
	}
	if messaging.ConsumerContext(s.Consumer) != "evd" || messaging.SourceContext(s.Source) != "trd" {
		t.Fatalf("consumer %s of %s", s.Consumer, s.Source)
	}
	actor, err := messaging.InboxActor(s.Consumer)
	if err != nil || actor != (kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.evd.formal_files"}) {
		t.Fatalf("actor %v, %v", actor, err)
	}
}

// R65: each event names one formal document as TRD sealed it — QuoteActivated and QuoteReplaced a
// quote revision's (a replacement with the revision it superseded), OrderDocumentGenerated an order
// revision's S/C or PI with the revision the order came from.
func TestFormalEventsNameTheSealedDocument(t *testing.T) {
	quote := file.Formal{Kind: file.FormalQuote, QuoteID: "quote.1", BusinessRevision: 2, BusinessHash: hashQ, Number: "QT-2026-00001",
		FileName: "QT-2026-00001.txt", MimeType: "text/plain", SHA256: hashF, Size: 120}
	replaced := quote
	replaced.ReplacedRevision = 1
	order := file.Formal{Kind: file.FormalSalesContract, QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: hashQ, OrderID: "order.1",
		OrderRevision: 2, OrderHash: hashO, Number: "SC-2026-00001", FileName: "SC-2026-00001-r2.txt", MimeType: "text/plain", SHA256: hashF,
		Size: 120}
	pi := order
	pi.Kind = file.FormalProformaInvoice
	for _, c := range []struct {
		env  eventing.Envelope
		want file.Formal
	}{
		{envelope(trdapi.EventQuoteActivated, nil), quote},
		{envelope(trdapi.EventQuoteReplaced, nil), replaced},
		{envelope(trdapi.EventOrderDocumentGenerated, nil), order},
		{envelope(trdapi.EventOrderDocumentGenerated, map[string]any{"document_kind": "proforma_invoice"}), pi},
	} {
		got, err := FormalEventOf(c.env)
		if err != nil || got != (application.FormalEvent{EventID: "evt-1", EventType: string(c.env.Type), Sealed: c.want}) {
			t.Errorf("%s: %+v, %v", c.env.Type, got, err)
		}
	}
}

// A payload that is not the one TRD writes is refused for good (evd.formal_document_event_invalid),
// before anything is read.
func TestMalformedFormalEventsAreRefused(t *testing.T) {
	for name, env := range map[string]eventing.Envelope{
		"not JSON":                  {ID: "evt-1", Source: trdapi.Source, Type: trdapi.EventQuoteActivated, Payload: json.RawMessage(`[`)},
		"another event type":        envelope(trdapi.EventQuoteWithdrawn, map[string]any{"aggregate_type": "QuoteRoot"}),
		"no event ID":               withID(envelope(trdapi.EventQuoteActivated, nil), ""),
		"event ID too long":         withID(envelope(trdapi.EventQuoteActivated, nil), strings.Repeat("e", 129)),
		"quote of another root":     envelope(trdapi.EventQuoteActivated, map[string]any{"quote_id": "quote.2"}),
		"quote event of an order":   envelope(trdapi.EventQuoteActivated, map[string]any{"aggregate_type": "SalesOrder"}),
		"activation replaces":       envelope(trdapi.EventQuoteActivated, map[string]any{"replaced_revision": 1}),
		"replacement replaces none": envelope(trdapi.EventQuoteReplaced, map[string]any{"replaced_revision": nil}),
		"replaces itself":           envelope(trdapi.EventQuoteReplaced, map[string]any{"replaced_revision": 2}),
		"quote event names order":   envelope(trdapi.EventQuoteActivated, map[string]any{"document_kind": "sales_contract"}),
		"no business revision":      envelope(trdapi.EventQuoteActivated, map[string]any{"business_revision": nil}),
		"business hash malformed":   envelope(trdapi.EventQuoteActivated, map[string]any{"business_hash": "abc"}),
		"no number":                 envelope(trdapi.EventQuoteActivated, map[string]any{"number": nil}),
		"no file name":              envelope(trdapi.EventQuoteActivated, map[string]any{"file_name": nil}),
		"no type":                   envelope(trdapi.EventQuoteActivated, map[string]any{"mime_type": nil}),
		"SHA-256 upper case":        envelope(trdapi.EventQuoteActivated, map[string]any{"file_sha256": strings.ToUpper(hashF)}),
		"empty file":                envelope(trdapi.EventQuoteActivated, map[string]any{"file_size": 0}),
		"order of another root":     envelope(trdapi.EventOrderDocumentGenerated, map[string]any{"order_id": "order.2"}),
		"order event of a quote":    envelope(trdapi.EventOrderDocumentGenerated, map[string]any{"aggregate_type": "QuoteRoot"}),
		"order without quote":       envelope(trdapi.EventOrderDocumentGenerated, map[string]any{"quote_id": nil}),
		"unknown document kind":     envelope(trdapi.EventOrderDocumentGenerated, map[string]any{"document_kind": "invoice"}),
		"no document revision":      envelope(trdapi.EventOrderDocumentGenerated, map[string]any{"document_order_revision": nil}),
		"document hash malformed":   envelope(trdapi.EventOrderDocumentGenerated, map[string]any{"document_order_hash": "x"}),
		"order document replaces":   envelope(trdapi.EventOrderDocumentGenerated, map[string]any{"replaced_revision": 1}),
	} {
		f := &files{}
		_, err := FormalPrepare(f)(context.Background(), env)
		if !errors.Is(err, messaging.ErrEventRejected) || !strings.Contains(err.Error(), string(FailureEventInvalid)) || len(f.prepared) != 0 {
			t.Errorf("%s: %v (asked %d)", name, err, len(f.prepared))
		}
	}
}

// The application's refusal for good becomes the Inbox's rejection with its code; every other
// failure — a TRD read denied to the consumer's actor, a conflict, the database — is retried.
func TestRefusalsAreForGoodAndFailuresRetried(t *testing.T) {
	env := envelope(trdapi.EventQuoteActivated, nil)
	mismatch := &application.FormalRefusal{Code: application.FailureFormalMismatch, Reason: "the SHA-256 of the bytes"}
	denied := kernel.Reject(kernel.CodePermissionDenied, "/", "")

	f := &files{prepareErr: mismatch}
	_, err := FormalPrepare(f)(context.Background(), env)
	if !errors.Is(err, messaging.ErrEventRejected) || !strings.Contains(err.Error(), "evd.formal_document_mismatch") {
		t.Fatalf("a mismatch in Prepare: %v", err)
	}
	f = &files{prepareErr: denied}
	_, err = FormalPrepare(f)(context.Background(), env)
	if errors.Is(err, messaging.ErrEventRejected) || !errors.Is(err, denied) {
		t.Fatalf("a denied read in Prepare: %v", err)
	}

	f = &files{}
	p, err := FormalPrepare(f)(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	e := messaging.Event{Consumer: FormalFiles, Envelope: env, Prepared: p}
	if err = FormalHandler(f)(context.Background(), e); err != nil || len(f.registered) != 1 || f.registered[0].Event.Sealed.QuoteID != "quote.1" {
		t.Fatalf("handler: %v, %+v", err, f.registered)
	}
	for _, c := range []struct {
		err      error
		rejected bool
	}{{mismatch, true}, {denied, false}, {errors.New("database"), false}} {
		f.registerErr = c.err
		err = FormalHandler(f)(context.Background(), e)
		if errors.Is(err, messaging.ErrEventRejected) != c.rejected || err == nil {
			t.Errorf("register failing with %v: %v", c.err, err)
		}
	}
	// A delivery without its prepared document, or with another event's, is retried, never applied.
	f = &files{}
	for _, prepared := range []any{nil, application.PreparedFormal{}} {
		err = FormalHandler(f)(context.Background(), messaging.Event{Consumer: FormalFiles, Envelope: env, Prepared: prepared})
		if err == nil || errors.Is(err, messaging.ErrEventRejected) || len(f.registered) != 0 {
			t.Errorf("prepared %v: %v", prepared, err)
		}
	}
}
