package application

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/domain/file"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Unit C4 (WBS S4-06c; Context Map R65 TRD→EVD, risk "EVD 登记的文件与 TRD 封存的字节不一致"; IG-08):
// what the consumer reads of TRD before its transaction decides whether a formal document is filed.

const quoteText = "Quotation QT-2026-00002\nsynthetic\n"

var hashQ = strings.Repeat("a", 64)

// trd answers TRD's FormalDocument read with doc and order, or err.
type trd struct {
	doc   trdapi.FormalDocument
	order trdapi.OrderDocument
	err   error
	asked []string
}

func (r *trd) GetFormalDocument(_ context.Context, quoteID string, rev int64) (trdapi.FormalDocument, error) {
	r.asked = append(r.asked, "quote "+quoteID)
	return r.doc, r.err
}

func (r *trd) GetOrderDocument(_ context.Context, orderID string, rev int64, kind string) (trdapi.OrderDocument, error) {
	r.asked = append(r.asked, kind+" "+orderID)
	return r.order, r.err
}

func service(r *trd) Service {
	return Service{Formal: func() FormalDocuments { return r }}
}

// replacement is QuoteReplaced of revision 2 of quote.1 (replacing 1) and TRD's read of it.
func replacement(content string) (FormalEvent, *trd) {
	sum := file.Hash([]byte(content))
	sealed := file.Formal{Kind: file.FormalQuote, QuoteID: "quote.1", BusinessRevision: 2, BusinessHash: hashQ, ReplacedRevision: 1,
		Number: "QT-2026-00002", FileName: "QT-2026-00002.txt", MimeType: api.MimeText, SHA256: sum, Size: len(content)}
	doc := trdapi.FormalDocument{QuoteID: "quote.1", BusinessRevision: 2, BusinessHash: hashQ, Status: "active", Content: []byte(content),
		OwnerActorID: "editor", OwnerTeamID: "team.sales", Formal: trdapi.FormalView{Number: "QT-2026-00002", FileName: "QT-2026-00002.txt",
			MimeType: api.MimeText, DocumentSHA256: sum, SizeBytes: len(content), Commit: trdapi.CommitView{ReplacedRevision: 1}}}
	return FormalEvent{EventID: "evt-1", EventType: trdapi.EventQuoteReplaced, Sealed: sealed}, &trd{doc: doc}
}

func refusalCode(err error) string {
	var r *FormalRefusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// The document TRD reads is the one the event sealed: EVD keeps its bytes under the quote root's
// owner. Bytes, a binding or a document that differ refuse the event for good; so does TRD refusing
// its own document for its integrity. What a grant or time can cure — NOT_FOUND, PERMISSION_DENIED,
// TRD not wired yet, an owner missing — is retried.
func TestPrepareReadsTheSealedQuoteDocument(t *testing.T) {
	e, r := replacement(quoteText)
	p, err := service(r).PrepareFormalFile(context.Background(), e)
	if err != nil || p.Event != e || string(p.Content) != quoteText || p.OwnerActor != "editor" || p.OwnerTeam != "team.sales" ||
		!slices.Equal(r.asked, []string{"quote quote.1"}) {
		t.Fatalf("prepared %+v, %v (asked %v)", p, err, r.asked)
	}
	for _, c := range []struct {
		name string
		edit func(*trd)
		want string // failure code; "" for a retry
	}{
		{"other bytes than the sealed", func(r *trd) { r.doc.Content = []byte(strings.ToUpper(quoteText)) }, FailureFormalMismatch},
		{"another binding", func(r *trd) { r.doc.Formal.DocumentSHA256 = hashQ }, FailureFormalMismatch},
		{"another number", func(r *trd) { r.doc.Formal.Number = "QT-2026-00003" }, FailureFormalMismatch},
		{"the activation's commit", func(r *trd) { r.doc.Formal.Commit.ReplacedRevision = 0 }, FailureFormalMismatch},
		{"another revision", func(r *trd) { r.doc.BusinessRevision = 3 }, FailureFormalMismatch},
		{"TRD's bytes are not its bound ones", func(r *trd) {
			r.err = trdapi.Reject(trdapi.CodeFormalDocumentIntegrityFailure, "/document")
		}, FailureFormalMismatch},
		{"TRD's binding is missing", func(r *trd) { r.err = trdapi.Reject(trdapi.CodeDocumentBindingMissing, "/document") }, FailureFormalMismatch},
		{"not found in the consumer's data scope", func(r *trd) { r.err = kernel.Reject(kernel.CodeNotFound, "/id", "") }, ""},
		{"the consumer not provisioned", func(r *trd) { r.err = kernel.Reject(kernel.CodePermissionDenied, "/", "") }, ""},
		{"TRD names no owner", func(r *trd) { r.doc.OwnerActorID = "" }, ""},
	} {
		e, r := replacement(quoteText)
		c.edit(r)
		_, err := service(r).PrepareFormalFile(context.Background(), e)
		if err == nil || refusalCode(err) != c.want {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	e, _ = replacement(quoteText)
	if _, err = (Service{}).PrepareFormalFile(context.Background(), e); err == nil || refusalCode(err) != "" {
		t.Fatalf("without TRD's read: %v", err)
	}
	if _, err = (Service{Formal: func() FormalDocuments { return nil }}).PrepareFormalFile(context.Background(), e); err == nil || refusalCode(err) != "" {
		t.Fatalf("TRD not composed yet: %v", err)
	}
	// An event whose document is not of its type is refused before TRD is asked.
	e, r = replacement(quoteText)
	e.EventType = trdapi.EventQuoteActivated
	if _, err = service(r).PrepareFormalFile(context.Background(), e); refusalCode(err) != FailureFormalEventInvalid || len(r.asked) != 0 {
		t.Fatalf("an activation that replaces: %v (asked %v)", err, r.asked)
	}
}

// An order revision's S/C or PI: TRD's read names the order revision, its hash and the document;
// the quote revision the order came from is the event's. A document EVD cannot keep as a business
// file (here: bytes over EVD's 10 MiB) is refused for good as not fileable.
func TestPrepareReadsTheSealedOrderDocument(t *testing.T) {
	const content = "SALES CONTRACT SC-2026-00001\n"
	sum, hashO := file.Hash([]byte(content)), strings.Repeat("b", 64)
	sealed := file.Formal{Kind: file.FormalSalesContract, QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: hashQ, OrderID: "order.1",
		OrderRevision: 2, OrderHash: hashO, Number: "SC-2026-00001", FileName: "SC-2026-00001-r2.txt", MimeType: api.MimeText, SHA256: sum,
		Size: len(content)}
	e := FormalEvent{EventID: "evt-2", EventType: trdapi.EventOrderDocumentGenerated, Sealed: sealed}
	r := &trd{order: trdapi.OrderDocument{OrderID: "order.1", Content: []byte(content), OwnerActorID: "editor", Document: trdapi.OrderDocumentView{
		OrderRevision: 2, OrderHash: hashO, DocumentKind: trdapi.DocumentSalesContract, Number: "SC-2026-00001", FileName: "SC-2026-00001-r2.txt",
		MimeType: api.MimeText, DocumentSHA256: sum, SizeBytes: len(content)}}}
	p, err := service(r).PrepareFormalFile(context.Background(), e)
	if err != nil || string(p.Content) != content || p.OwnerActor != "editor" || p.OwnerTeam != "" ||
		!slices.Equal(r.asked, []string{"sales_contract order.1"}) {
		t.Fatalf("prepared %+v, %v (asked %v)", p, err, r.asked)
	}
	r.order.Document.OrderHash = hashQ
	if _, err = service(r).PrepareFormalFile(context.Background(), e); refusalCode(err) != FailureFormalMismatch {
		t.Fatalf("another order hash: %v", err)
	}
	r.order.Document.OrderHash, r.err = hashO, trdapi.Reject(trdapi.CodeDocumentIntegrityFailure, "/document")
	if _, err = service(r).PrepareFormalFile(context.Background(), e); refusalCode(err) != FailureFormalMismatch {
		t.Fatalf("TRD's integrity failure: %v", err)
	}

	big := strings.Repeat("x", api.MaxFileBytes+1)
	sealed.SHA256, sealed.Size = file.Hash([]byte(big)), len(big)
	r = &trd{order: r.order}
	r.order.Content, r.order.Document.DocumentSHA256, r.order.Document.SizeBytes = []byte(big), sealed.SHA256, sealed.Size
	e.Sealed = sealed
	if _, err = service(r).PrepareFormalFile(context.Background(), e); refusalCode(err) != FailureFormalNotFileable {
		t.Fatalf("over 10 MiB: %v", err)
	}
}

// formal_file.register is the consumer's command: system actors only, its own permission (granted
// to system:inbox.evd.formal_files); the read of a formal file needs evd.read.
func TestFormalFileRules(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	r, err := policy.Rule(api.CommandRegisterFormalFile)
	if err != nil || r.Permission != api.CommandRegisterFormalFile || !slices.Equal(r.ActorKinds, []kernel.ActorKind{kernel.ActorSystem}) {
		t.Fatalf("%+v", r)
	}
	if r, err = policy.Rule(CommandGetFormalFile); err != nil || r.Permission != PermissionRead {
		t.Fatalf("%+v", r)
	}
	for _, tpl := range authorization.Templates() {
		if slices.Contains(tpl.Permissions, api.CommandRegisterFormalFile) {
			t.Errorf("template %s holds %s, which only the consumer holds", tpl.Code, api.CommandRegisterFormalFile)
		}
	}
}
