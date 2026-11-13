//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// EVD's Inbox consumer evd.formal_files on PostgreSQL 16 (WBS S4-06c, unit C4; Context Map R65 TRD→EVD,
// Published Language events, contract "TRD 集成事件 v1、FormalDocument 读取", no guard lock; S4 decision
// D-8; IG-08 closed on PostgreSQL storage and this consumer; tactical model EVD-AGG-BusinessFile
// notes), through the assembled relay with the in-process dispatcher as its publisher
// (Platform.Relay, as brant-worker -dispatch runs it) and the assembled replayer. Each test relays a
// Bento outbox of its own (newOrderEvents) holding TRD's own events, unchanged. Python V0.50 has no
// event consumer and no EVD copy of a formal document; the tests cite design IDs.

// formalActor is the system actor evd.formal_files runs as.
var formalActor = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.evd.formal_files"}

func quoteDoc(id string, rev int64) evdapi.FormalDocumentRef {
	return evdapi.FormalDocumentRef{Kind: evdapi.FormalQuote, QuoteID: id, BusinessRevision: rev}
}

func orderDoc(kind, id string, rev int64) evdapi.FormalDocumentRef {
	return evdapi.FormalDocumentRef{Kind: kind, OrderID: id, OrderRevision: rev}
}

// formalFile reads the file EVD registered for ref as ctx's actor.
func (w *world) formalFile(ctx context.Context, ref evdapi.FormalDocumentRef) evdapi.FormalFile {
	w.t.Helper()
	f, err := w.c.EVD.GetFormalFile(ctx, ref)
	w.must(err)
	return f
}

// fileRegistrations are EVD's FileRegistered events in company.A of actor, as "file_id sha256".
func (w *world) fileRegistrations(actor kernel.ActorRef) []string {
	out := []string{}
	for _, e := range w.events("company.A", evdapi.Source) {
		if e.Type == evdapi.EventFileRegistered && e.Payload["actor_kind"] == string(actor.Kind) && e.Payload["actor_id"] == actor.ID {
			out = append(out, e.Payload["file_id"].(string)+" "+e.Payload["content_sha256"].(string))
		}
	}
	return out
}

// evdAudit are EVD's audit entries of command with outcome as "object actor".
func (w *world) evdAudit(command, outcome string) []string {
	w.t.Helper()
	entries, err := w.c.EVD.Audit(w.actor("evd.auditor", "evd.audit"))
	w.must(err)
	out := []string{}
	for _, e := range entries {
		if e.Command == command && e.Outcome == outcome {
			out = append(out, e.ObjectID+" "+e.Actor.ID)
		}
	}
	return out
}

// R65, IG-08: TRD's QuoteActivated and QuoteReplaced each have EVD file the revision's formal document
// once — the bytes TRD sealed, under the quote root's owner, with the TRD document, its business hash,
// number, SHA-256 and event recorded (FileRef v1 for the contexts downstream of EVD).
//
// The activation arrives before the operator provisions system:inbox.evd.formal_files: TRD's read in
// Prepare is denied, the delivery fails and, its budget spent, is dead-lettered with an alert, nothing
// filed. Provisioned, the replacement is filed through the relay, then the activation's dead letter is
// replayed — after the later event of its aggregate — and filed once; a second replay is refused. Each
// file is the bytes TRD's FormalDocument read returns, owned by the quote's owner (editor, team trade):
// a teammate with team data scope reads it, a reader of another team or with own data scope sees
// neither the file nor TRD's document. Redelivery of both events by the dispatcher and by a second
// dispatcher, and the activation under another event ID, file nothing more; EVD audits two
// registrations by the consumer, which read TRD's documents as itself (TRD's audit).
func TestFormalQuoteDocumentsAreFiledOncePerEvent(t *testing.T) {
	q := newTrdWorld(t)
	ctx := context.Background()
	scope := q.scope("company.A")
	a := q.activated("quote.1")
	n, _, version := q.revisedAndApproved("quote.1", a.RowVersion, trdapi.TargetReplace, "review.2", "approval.2")
	r, err := q.c.TRD.Replace(q.author, q.replaceCmd("quote.1", version, n, "approval.2"))
	q.must(err)
	activated := outboxOf(t, scope, trdapi.Source, trdapi.EventQuoteActivated)
	replaced := outboxOf(t, scope, trdapi.Source, trdapi.EventQuoteReplaced)
	if len(activated) != 1 || len(replaced) != 1 {
		t.Fatalf("TRD's Outbox: %d activations, %d replacements", len(activated), len(replaced))
	}
	files := q.count("company.A", "brant_evd.business_file")

	subs := q.c.EVD.Subscriptions()
	ev := newOrderEvents(t, subs...)
	ev.enqueue(activated[0])
	ev.once(ev.relay(1))
	if s, al := ev.status(activated[0].ID), ev.alerts.of(activated[0].ID); s != "ABANDONED" || len(al) != 1 || !al[0].Stored ||
		al[0].Code != messaging.FailurePublishFailed || q.count("company.A", "brant_evd.formal_file") != 0 {
		t.Fatalf("before provisioning: outbox %s, alerts %+v; want the delivery failed and dead-lettered, nothing filed", s, al)
	}
	if _, err = q.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	ev.enqueue(replaced[0])
	ev.drain(ev.relay(5))
	ev.published(replaced[0])
	operator := q.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator"},
		Permissions: []string{messaging.ReplayPermission}})
	replayer := ev.replayer()
	if _, err = replayer.Replay(operator, activated[0].Source, string(activated[0].ID)); err != nil {
		t.Fatal(err)
	}
	if _, err = replayer.Replay(operator, activated[0].Source, string(activated[0].ID)); !errors.Is(err, messaging.ErrAlreadyReplayed) {
		t.Fatalf("second replay: %v", err)
	}

	f1, f2 := q.formalFile(q.author, quoteDoc("quote.1", 1)), q.formalFile(q.author, quoteDoc("quote.1", 2))
	for _, c := range []struct {
		f        evdapi.FormalFile
		receipt  trdapi.FormalizationReceipt
		event    eventing.Envelope
		replaced int64
	}{{f1, a, activated[0], 0}, {f2, r, replaced[0], 1}} {
		f, rc := c.f, c.receipt
		if f.FileSHA256 != rc.DocumentSHA256 || f.Document != quoteDoc("quote.1", rc.BusinessRevision) || f.QuoteID != "quote.1" ||
			f.BusinessRevision != rc.BusinessRevision || f.BusinessHash != rc.BusinessHash || f.ReplacedRevision != c.replaced || f.OrderHash != "" ||
			f.Number != rc.Number || f.FileName != rc.FileName || f.MimeType != rc.MimeType || f.SizeBytes != rc.SizeBytes ||
			f.EventID != string(c.event.ID) || f.EventType != string(c.event.Type) || f.RegisteredAt.IsZero() {
			t.Fatalf("formal file of revision %d: %+v (TRD %+v)", rc.BusinessRevision, f, rc)
		}
		doc, err := q.c.TRD.GetFormalDocument(q.author, "quote.1", rc.BusinessRevision)
		q.must(err)
		got, err := q.c.EVD.GetFile(q.author, f.FileID)
		q.must(err)
		if string(got.Content) != string(doc.Content) || got.ContentSHA256 != rc.DocumentSHA256 || got.FileName != rc.FileName ||
			got.MimeType != evdapi.MimeText || got.SupersedesFileID != "" || got.Formal == nil || *got.Formal != f ||
			doc.OwnerActorID != "editor" || doc.OwnerTeamID != "trade" {
			t.Fatalf("EVD's file %s: %+v, TRD's document owned by %s/%s", f.FileID, got.Formal, doc.OwnerActorID, doc.OwnerTeamID)
		}
	}
	if f1.FileID == f2.FileID || q.count("company.A", "brant_evd.business_file") != files+2 || q.count("company.A", "brant_evd.formal_file") != 2 {
		t.Fatalf("files %s and %s; %d business files (was %d)", f1.FileID, f2.FileID, q.count("company.A", "brant_evd.business_file"), files)
	}
	want := []string{f2.FileID + " " + f2.FileSHA256, f1.FileID + " " + f1.FileSHA256}
	if got := q.fileRegistrations(formalActor); !slices.Equal(got, want) {
		t.Fatalf("FileRegistered by the consumer %v, want %v", got, want)
	}
	if got := q.evdAudit(evdapi.CommandRegisterFormalFile, "succeeded"); !slices.Equal(got,
		[]string{f2.FileID + " " + formalActor.ID, f1.FileID + " " + formalActor.ID}) {
		t.Fatalf("EVD audits %v", got)
	}
	reads, err := q.c.TRD.Audit(q.author)
	q.must(err)
	if !slices.ContainsFunc(reads, func(e kernel.AuditEntry) bool {
		return e.Command == "quote.document.get" && e.Actor == formalActor && e.Outcome == "read" && e.ObjectID == "quote.1"
	}) {
		t.Fatal("TRD's audit holds no read of the document by the consumer")
	}

	// Who sees the quote in TRD sees its formal files in EVD (data scope of the quote root's owner).
	reader := func(id, dataScope, team string) context.Context {
		return q.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id},
			Permissions: []string{"evd.read", "trd.read"}, DataScope: authorizationScope(dataScope), Team: team})
	}
	if got, err := q.c.EVD.GetFile(reader("teammate", "team", "trade"), f2.FileID); err != nil || got.ContentSHA256 != f2.FileSHA256 {
		t.Fatalf("a teammate: %v", err)
	}
	for _, other := range []context.Context{reader("other.team", "team", "export"), reader("own.only", "own", "trade")} {
		_, err = q.c.EVD.GetFile(other, f2.FileID)
		q.want(err, kernel.CodeNotFound)
		_, err = q.c.EVD.GetFormalFile(other, quoteDoc("quote.1", 2))
		q.want(err, kernel.CodeNotFound)
		_, err = q.c.TRD.GetFormalDocument(other, "quote.1", 2)
		q.want(err, kernel.CodeNotFound)
	}
	_, err = q.c.EVD.GetFormalFile(q.author, quoteDoc("quote.1", 3))
	q.want(err, kernel.CodeNotFound)
	_, err = q.c.EVD.GetFormalFile(q.author, evdapi.FormalDocumentRef{Kind: evdapi.FormalQuote, QuoteID: "quote.1"})
	q.want(err, kernel.CodeSchemaInvalid)

	// Once per event and per document.
	second, err := messaging.NewDispatcher(subs...)
	q.must(err)
	again := activated[0]
	again.ID = eventing.EventID(string(again.ID) + ".again")
	for _, e := range []struct {
		d   *messaging.Dispatcher
		env eventing.Envelope
	}{{ev.dispatcher, activated[0]}, {ev.dispatcher, replaced[0]}, {second, activated[0]}, {second, replaced[0]}, {second, again}} {
		if err := e.d.Publish(ctx, e.env); err != nil {
			t.Fatalf("redelivery %s: %v", e.env.ID, err)
		}
	}
	if q.count("company.A", "brant_evd.business_file") != files+2 || q.count("company.A", "brant_evd.formal_file") != 2 ||
		len(q.fileRegistrations(formalActor)) != 2 || len(q.evdAudit(evdapi.CommandRegisterFormalFile, "succeeded")) != 2 {
		t.Fatal("a redelivery filed again")
	}
}

// R65, IG-08, TRD-INV-020: OrderDocumentGenerated has EVD file each S/C and PI TRD generates, one
// file per order revision and kind. The order world's quote is activated (its QuoteActivated is
// filed too), the order activated, its S/C (order.sc.plain) and a PI (from a PI template without
// the law section, the quote sealing no legal terms) generated on revision 1; the customer reduces
// L1 to 100 and revision 2 gets its S/C, which carries the order's one contract number in its own
// file "-r2". Every TRD event of the world is relayed: the consumer files the four documents and
// settles the other events untouched; each file holds the bytes TRD's read returns and records the
// order revision and hash it binds and the quote revision the order came from. A second dispatcher
// redelivering every event files nothing more.
func TestOrderDocumentsAreFiledPerOrderRevision(t *testing.T) {
	d := newDocWorld(t, false)
	ctx := context.Background()
	scope := d.scope("company.A")
	ed := d.author
	piPlain := d.configure(ed, d.reviewer, polapi.KindOrderDocumentTemplate, "order.pi.plain", 0, d.win.from, time.Time{}, polapi.ConfigData{
		OrderDocumentTemplate: &polapi.OrderDocumentTemplate{Name: "order.pi.plain", DocumentKind: polapi.OrderDocumentProformaInvoice,
			Language: polapi.LanguageEn, RendererVersion: polapi.OrderRendererVersion, Title: "PROFORMA INVOICE",
			Sections: []string{"seller", "buyer", "identity", "lines", "totals", "terms", "delivery", "payments", "notes"}, FooterNote: "合成测试，非真实合同"}},
		true)
	o := d.activeOrder("quote.1")
	id := o.ObjectID
	v1 := d.getOrder(id)
	sc1, err := d.c.TRD.GenerateOrderDocument(ed, d.generateCmd(id, 1, v1.OrderHash, trdapi.DocumentSalesContract, d.plain, d.scSeq))
	d.must(err)
	pi1, err := d.c.TRD.GenerateOrderDocument(ed, d.generateCmd(id, 1, v1.OrderHash, trdapi.DocumentProformaInvoice, piPlain, d.piSeq))
	d.must(err)
	am := d.approvedAmendment(id, "amendment.1", 1, v1.OrderHash, "100")
	_, err = d.c.TRD.ApplyAmendment(d.reviewer, d.applyCmd(id, "amendment.1", am.RowVersion))
	d.must(err)
	v2 := d.getOrder(id)
	sc2, err := d.c.TRD.GenerateOrderDocument(ed, d.generateCmd(id, 2, v2.OrderHash, trdapi.DocumentSalesContract, d.plain, d.scSeq))
	d.must(err)
	if sc2.Number != sc1.Number || sc2.FileName != sc1.Number+"-r2.txt" || v2.OrderRevision != 2 {
		t.Fatalf("revision 2's S/C %+v (revision 1's %s)", sc2, sc1.Number)
	}

	subs := d.c.EVD.Subscriptions()
	if _, err = d.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	ev := newOrderEvents(t, subs...)
	all := ev.copyFrom(scope, trdapi.Source)
	ev.drain(ev.relay(5))
	ev.published(all...)
	generated := outboxOf(t, scope, trdapi.Source, trdapi.EventOrderDocumentGenerated)
	if len(generated) != 3 || len(all) <= 4 {
		t.Fatalf("%d documents generated, %d TRD events", len(generated), len(all))
	}

	quoteFile := d.formalFile(ed, quoteDoc("quote.1", 1))
	a := d.getQuote("quote.1").Revisions[0]
	if a.Formal == nil || quoteFile.FileSHA256 != a.Formal.DocumentSHA256 || quoteFile.EventType != trdapi.EventQuoteActivated {
		t.Fatalf("the order's quote: %+v", quoteFile)
	}
	for i, c := range []struct {
		receipt trdapi.OrderDocumentReceipt
		ref     evdapi.FormalDocumentRef
		hash    string
	}{
		{sc1, orderDoc(evdapi.FormalSalesContract, id, 1), v1.OrderHash},
		{pi1, orderDoc(evdapi.FormalProformaInvoice, id, 1), v1.OrderHash},
		{sc2, orderDoc(evdapi.FormalSalesContract, id, 2), v2.OrderHash},
	} {
		f, rc := d.formalFile(ed, c.ref), c.receipt
		if f.Document != c.ref || f.FileSHA256 != rc.DocumentSHA256 || f.OrderHash != c.hash || rc.OrderHash != c.hash ||
			f.QuoteID != "quote.1" || f.BusinessRevision != 1 || f.BusinessHash != a.BusinessHash || f.ReplacedRevision != 0 ||
			f.Number != rc.Number || f.FileName != rc.FileName || f.SizeBytes != rc.SizeBytes || f.EventID != string(generated[i].ID) ||
			f.EventType != trdapi.EventOrderDocumentGenerated {
			t.Fatalf("%s r%d: %+v (TRD %+v)", c.ref.Kind, c.ref.OrderRevision, f, rc)
		}
		doc, err := d.c.TRD.GetOrderDocument(ed, id, c.ref.OrderRevision, c.ref.Kind)
		d.must(err)
		got, err := d.c.EVD.GetFile(ed, f.FileID)
		d.must(err)
		if string(got.Content) != string(doc.Content) || got.ContentSHA256 != doc.Document.DocumentSHA256 || got.Formal == nil ||
			*got.Formal != f || doc.OwnerActorID != "editor" {
			t.Fatalf("EVD's file %s holds other bytes than TRD's %s r%d", f.FileID, c.ref.Kind, c.ref.OrderRevision)
		}
	}
	if n := d.count("company.A", "brant_evd.formal_file"); n != 4 || len(d.fileRegistrations(formalActor)) != 4 {
		t.Fatalf("%d formal files", n)
	}

	second, err := messaging.NewDispatcher(subs...)
	d.must(err)
	for _, e := range all {
		if err := second.Publish(ctx, e); err != nil {
			t.Fatalf("redelivery of %s %s: %v", e.Type, e.ID, err)
		}
	}
	if n := d.count("company.A", "brant_evd.formal_file"); n != 4 || len(d.fileRegistrations(formalActor)) != 4 {
		t.Fatalf("after the redeliveries: %d formal files", n)
	}
}

// R65's risk "EVD 登记的文件与 TRD 封存的字节不一致", WBS S4-06c: a document whose bytes do not match the
// SHA-256 TRD sealed is refused for good — dead-lettered at once with an alert naming
// evd.formal_document_mismatch, never filed, and its replay refused — and the database refuses to
// record a formal file over other bytes.
//
//   - An event naming another SHA-256 than the one TRD sealed and reads (a forged or corrupted event).
//   - TRD's stored bytes rewritten after the activation together with their hash and size (the
//     document guard off, as only the owner role can): QuoteActivated names the sealed SHA-256, the
//     bytes TRD reads are other ones. With the bytes restored the refusal stands: the event is
//     settled as rejected.
//   - In SQL: a formal file naming a SHA-256 its business file does not carry, and one over a business
//     file whose recorded SHA-256 is not that of its bytes; a formal file is never changed.
func TestFormalBytesThatAreNotTheSealedOnesAreRefusedForGood(t *testing.T) {
	q := newTrdWorld(t)
	ctx := context.Background()
	scope := q.scope("company.A")
	a := q.activated("quote.1")
	activated := outboxOf(t, scope, trdapi.Source, trdapi.EventQuoteActivated)[0]
	subs := q.c.EVD.Subscriptions()
	if _, err := q.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	ev := newOrderEvents(t, subs...)
	operator := q.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator"},
		Permissions: []string{messaging.ReplayPermission}})
	replayer := ev.replayer()
	files := q.count("company.A", "brant_evd.business_file")
	refused := func(e eventing.Envelope) {
		t.Helper()
		ev.enqueue(e)
		ev.once(ev.relay(5))
		al := ev.alerts.of(e.ID)
		if s := ev.status(e.ID); s != "ABANDONED" || len(al) != 1 || !al[0].Stored || al[0].Code != messaging.FailurePublishRejected ||
			!strings.Contains(al[0].Error, "evd.formal_document_mismatch") {
			t.Fatalf("%s: outbox %s, alerts %+v", e.ID, s, al)
		}
		if _, err := replayer.Replay(operator, e.Source, string(e.ID)); !errors.Is(err, messaging.ErrReplayFailed) {
			t.Fatalf("replay of %s: %v", e.ID, err)
		}
		if q.count("company.A", "brant_evd.business_file") != files || q.count("company.A", "brant_evd.formal_file") != 0 ||
			len(q.fileRegistrations(formalActor)) != 0 {
			t.Fatalf("%s filed", e.ID)
		}
	}

	forged := activated
	forged.ID = eventing.EventID(string(activated.ID) + ".forged")
	var payload map[string]any
	q.must(json.Unmarshal(activated.Payload, &payload))
	payload["file_sha256"] = sha256Hex("another document")
	body, err := json.Marshal(payload)
	q.must(err)
	forged.Payload = body
	refused(forged)

	where := ` WHERE tenant_id = '` + q.tenant + `' AND quote_id = 'quote.1' AND revision = 1`
	off := "ALTER TABLE brant_trd.quote_document DISABLE TRIGGER quote_document_guard"
	on := "ALTER TABLE brant_trd.quote_document ENABLE TRIGGER quote_document_guard"
	q.must(oeqvOwner(q, off, `UPDATE brant_trd.quote_document SET content = content || '\x0a'::bytea, size_bytes = size_bytes + 1,
		content_sha256 = encode(sha256(content || '\x0a'::bytea), 'hex')`+where, on))
	refused(activated)
	q.must(oeqvOwner(q, off, `UPDATE brant_trd.quote_document SET content = substring(content from 1 for size_bytes - 1),
		size_bytes = size_bytes - 1, content_sha256 = encode(sha256(substring(content from 1 for size_bytes - 1)), 'hex')`+where, on))
	if doc, err := q.c.TRD.GetFormalDocument(q.author, "quote.1", 1); err != nil || sha256Hex(string(doc.Content)) != a.DocumentSHA256 {
		t.Fatalf("the sealed bytes restored: %v", err)
	}
	if err = ev.dispatcher.Publish(ctx, activated); !errors.Is(err, eventing.ErrPublishRejected) {
		t.Fatalf("the rejected activation delivered again: %v", err)
	}
	if q.count("company.A", "brant_evd.formal_file") != 0 {
		t.Fatal("a rejected event filed")
	}

	// The database keeps R65's rule whoever writes the rows.
	insertFile := func(id, content, sum string) {
		q.must(q.exec("company.A", `INSERT INTO brant_evd.business_file (tenant_id, company_id, file_id, file_name, mime_type, content, sha256,
			byte_size, owner_actor_id, registered_at) VALUES (current_setting('app.tenant_id'), current_setting('app.company_id'), '`+id+`',
			'`+id+`.txt', 'text/plain', convert_to('`+content+`', 'UTF8'), '`+sum+`', `+itoa(int64(len(content)))+`, 'editor', now())`))
	}
	insertFormal := func(id, sum string, size int) error {
		return q.exec("company.A", `INSERT INTO brant_evd.formal_file (tenant_id, company_id, file_id, document_kind, quote_id, business_revision,
			business_hash, number, document_sha256, size_bytes, event_id, event_type, registered_at) VALUES (current_setting('app.tenant_id'),
			current_setting('app.company_id'), '`+id+`', 'quote', 'quote.1', 1, '`+a.BusinessHash+`', '`+a.Number+`', '`+sum+`', `+
			itoa(int64(size))+`, 'evt.`+id+`', 'idp-brant.trd.quote_activated.v1', now())`)
	}
	insertFile("file.x", "sealed", sha256Hex("sealed"))
	if err = insertFormal("file.x", sha256Hex("other"), 6); err == nil || !strings.Contains(err.Error(), "formal_file") {
		t.Fatalf("a formal file naming another SHA-256 than its file's: %v", err)
	}
	insertFile("file.y", "sealed", sha256Hex("forged"))
	if err = insertFormal("file.y", sha256Hex("forged"), 6); err == nil || !strings.Contains(err.Error(), "other bytes than the document TRD sealed") {
		t.Fatalf("a formal file over bytes that do not hash to the recorded SHA-256: %v", err)
	}
	q.must(insertFormal("file.x", sha256Hex("sealed"), 6))
	for _, sql := range []string{"UPDATE brant_evd.formal_file SET number = 'QT-0'", "DELETE FROM brant_evd.formal_file"} {
		if err = q.exec("company.A", sql); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("%s as the application: %v", sql, err)
		}
		if err = oeqvOwner(q, sql); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("%s as the owner: %v", sql, err)
		}
	}
}
