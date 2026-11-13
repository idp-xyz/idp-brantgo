//go:build integration

package bootstrap_test

import (
	"context"
	"strings"
	"testing"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ActionReconciliation v1 by request hash (Context Map R18 TRD→TSK and R66–R69 EVD/INQ/SUP/CST→TSK,
// S4 decision D-11; WBS S4-02's "reconcile-by-hash on every context the Q01 capabilities write"):
// TSK's ActionExecution keeps the command name, the idempotency key and the request hash it persisted
// before sending the command (TSK-INV-003), not the command. Each context answers RequestOf with the
// hash its ledger stores, and ReconcileHash with what Reconcile answers for the command itself
// (contract 06 动作对账; X-INV-001, X-INV-003); Python t03_reconciliation.reconcile takes this form
// (command, idempotency_key, request_hash).

// reconciledAs is a context's answer reduced to what every context's Reconciliation carries.
type reconciledAs struct {
	State    kernel.ReconcileState
	ActionID string
	ObjectID string
	Receipt  string // the stored receipt's action ID, "" without one
}

// hashCase is one context's committed command and how to reconcile it.
type hashCase struct {
	name     string
	schema   string                                      // the context's ledger schema
	request  kernel.ActionRequest                        // RequestOf of the command as sent
	conflict kernel.ActionRequest                        // RequestOf of another request under the same key
	receipt  string                                      // the action ID the command answered
	full     func(context.Context) (reconciledAs, error) // Reconcile with the command as sent
	byHash   func(context.Context, string, string, string) (reconciledAs, error)
	owner    context.Context // reads the object
	noRead   context.Context // lacks the context's read permission
	outsider context.Context // holds it, own data scope: does not see the object
}

// storedHash is the request hash the ledger of schema stored for command and key in company.A.
func storedHash(t *testing.T, w *world, schema, command, key string) string {
	t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	var hash string
	w.must(bento.ScopeTransactor{DB: db, Scope: w.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return exec.QueryRow(ctx, `SELECT request_hash FROM `+schema+`.action_result WHERE command = $1 AND idempotency_key = $2`,
			command, key).Scan(&hash)
	}))
	return hash
}

// checkByHash runs the reconciliation by hash of c: RequestOf is the stored hash; the same answer as
// the command itself (committed with the stored receipt); another request under the key
// payload_conflict without a receipt; an unknown key not_committed; a malformed hash, key or command
// SCHEMA_INVALID; without the read permission PERMISSION_DENIED; outside the data scope NOT_FOUND
// for both hashes; each answer audited (action.reconcile).
func checkByHash(t *testing.T, w *world, c hashCase) {
	t.Helper()
	r := c.request
	if got := storedHash(t, w, c.schema, r.Command, r.IdempotencyKey); got != r.RequestHash || !kernel.ValidRequestHash(got) {
		t.Fatalf("%s: RequestOf %s, the ledger stored %s", c.name, r.RequestHash, got)
	}
	if c.conflict.Command != r.Command || c.conflict.IdempotencyKey != r.IdempotencyKey || c.conflict.RequestHash == r.RequestHash {
		t.Fatalf("%s: the conflicting request %+v", c.name, c.conflict)
	}
	full, err := c.full(c.owner)
	w.must(err)
	got, err := c.byHash(c.owner, r.Command, r.IdempotencyKey, r.RequestHash)
	w.must(err)
	if got != full || got.State != kernel.ReconcileCommitted || got.Receipt != c.receipt || got.ActionID != c.receipt {
		t.Fatalf("%s: by hash %+v, with the command %+v (receipt %s)", c.name, got, full, c.receipt)
	}
	if got, err = c.byHash(c.owner, r.Command, r.IdempotencyKey, c.conflict.RequestHash); err != nil ||
		got != (reconciledAs{State: kernel.ReconcilePayloadConflict, ActionID: full.ActionID, ObjectID: full.ObjectID}) {
		t.Fatalf("%s: another request %+v %v", c.name, got, err)
	}
	if got, err = c.byHash(c.owner, r.Command, "k.never", r.RequestHash); err != nil || got != (reconciledAs{State: kernel.ReconcileNotCommitted}) {
		t.Fatalf("%s: an unknown key %+v %v", c.name, got, err)
	}
	for _, bad := range [][3]string{{r.Command, r.IdempotencyKey, strings.ToUpper(r.RequestHash)}, {r.Command, r.IdempotencyKey, "abc"},
		{r.Command, "", r.RequestHash}, {"no.such_command", r.IdempotencyKey, r.RequestHash}} {
		if _, err = c.byHash(c.owner, bad[0], bad[1], bad[2]); code(err) != kernel.CodeSchemaInvalid {
			t.Fatalf("%s: %q: %s", c.name, bad, code(err))
		}
	}
	for _, hash := range []string{r.RequestHash, c.conflict.RequestHash} {
		if _, err = c.byHash(c.noRead, r.Command, r.IdempotencyKey, hash); code(err) != kernel.CodePermissionDenied {
			t.Fatalf("%s: without the read permission: %s", c.name, code(err))
		}
		if _, err = c.byHash(c.outsider, r.Command, r.IdempotencyKey, hash); code(err) != kernel.CodeNotFound {
			t.Fatalf("%s: outside the data scope: %s", c.name, code(err))
		}
	}
}

// reconciledFrom reduces a context's Reconciliation (State, ActionID, ObjectID and a Receipt with an
// ActionID).
func reconciledFrom(state kernel.ReconcileState, actionID, objectID string, receipt *string) reconciledAs {
	out := reconciledAs{State: state, ActionID: actionID, ObjectID: objectID}
	if receipt != nil {
		out.Receipt = *receipt
	}
	return out
}

func inqReconciled(r inqapi.Reconciliation, err error) (reconciledAs, error) {
	var id *string
	if r.Receipt != nil {
		id = &r.Receipt.ActionID
	}
	return reconciledFrom(r.State, r.ActionID, r.ObjectID, id), err
}

func evdReconciled(r evdapi.Reconciliation, err error) (reconciledAs, error) {
	var id *string
	if r.Receipt != nil {
		id = &r.Receipt.ActionID
	}
	return reconciledFrom(r.State, r.ActionID, r.ObjectID, id), err
}

func supReconciled(r supapi.Reconciliation, err error) (reconciledAs, error) {
	var id *string
	if r.Receipt != nil {
		id = &r.Receipt.ActionID
	}
	return reconciledFrom(r.State, r.ActionID, r.ObjectID, id), err
}

func cstReconciled(r cstapi.Reconciliation, err error) (reconciledAs, error) {
	var id *string
	if r.Receipt != nil {
		id = &r.Receipt.ActionID
	}
	return reconciledFrom(r.State, r.ActionID, r.ObjectID, id), err
}

func trdReconciled(r trdapi.Reconciliation, err error) (reconciledAs, error) {
	var id *string
	if r.Receipt != nil {
		id = &r.Receipt.ActionID
	}
	return reconciledFrom(r.State, r.ActionID, r.ObjectID, id), err
}

// The ActionReconciliation v1 of EVD (R66), INQ (R67) and SUP (R68) on the SUP world: a source
// registered, an RFQ drafted and a supplier offer drafted by the author, each reconciled by its
// request hash as by the command (EVD and SUP reconcile for the first time: S3 gap "动作对账：EVD、
// SUP 尚无"); readers need evd.read, inq.read and sup.read now and see only their data scope.
func TestEVDINQSUPReconcileByRequestHash(t *testing.T) {
	s := newSupWorld(t)
	w := s.world
	reader := func(id string, scope string, permissions ...string) context.Context {
		return w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", id), Permissions: permissions,
			DataScope: authorizationScope(scope), Team: "trade"})
	}

	register := evdapi.RegisterSource{IdempotencyKey: "k.hash.src", SourceID: "src.hash", SourceKind: evdapi.KindManualText, ContentText: "合成对账来源"}
	src, err := w.c.EVD.RegisterSource(s.author, register)
	w.must(err)
	evdReq, err := w.c.EVD.RequestOf(register)
	w.must(err)
	other := register
	other.ContentText = "另一份来源"
	evdOther, err := w.c.EVD.RequestOf(other)
	w.must(err)
	checkByHash(t, w, hashCase{name: "EVD source.register", schema: "brant_evd", request: evdReq, conflict: evdOther, receipt: src.ActionID,
		full: func(ctx context.Context) (reconciledAs, error) {
			return evdReconciled(w.c.EVD.Reconcile(ctx, register))
		},
		byHash: func(ctx context.Context, c, k, h string) (reconciledAs, error) {
			return evdReconciled(w.c.EVD.ReconcileHash(ctx, c, k, h))
		},
		owner: s.author, noRead: reader("evd.noread", "company", "inq.read", "sup.read"), outsider: reader("evd.outsider", "own", "evd.read")})

	save := inqapi.SaveRfq{IdempotencyKey: "k.hash.rfq", RfqID: "rfq.hash", Lines: []inqapi.RfqLine{{LineID: "L1", RequestedItem: "待核实商品"}}}
	rfq, err := w.c.INQ.SaveRfq(s.author, save)
	w.must(err)
	inqReq, err := w.c.INQ.RequestOf(save)
	w.must(err)
	stale := save
	stale.ExpectedVersion = 7 // the expected version is not part of the request
	if again, err := w.c.INQ.RequestOf(stale); err != nil || again != inqReq {
		t.Fatalf("another CAS value: %+v %v", again, err)
	}
	changed := save
	changed.UnresolvedFields = []string{"交期"}
	inqOther, err := w.c.INQ.RequestOf(changed)
	w.must(err)
	checkByHash(t, w, hashCase{name: "INQ rfq.save_draft", schema: "brant_inq", request: inqReq, conflict: inqOther, receipt: rfq.ActionID,
		full: func(ctx context.Context) (reconciledAs, error) { return inqReconciled(w.c.INQ.Reconcile(ctx, save)) },
		byHash: func(ctx context.Context, c, k, h string) (reconciledAs, error) {
			return inqReconciled(w.c.INQ.ReconcileHash(ctx, c, k, h))
		},
		owner: s.author, noRead: reader("inq.noread", "company", "evd.read", "sup.read"), outsider: reader("inq.outsider", "own", "inq.read")})

	offer := supapi.SaveSupplierOffer{IdempotencyKey: "k.hash.offer", OfferID: "offer.hash", Data: s.offer()}
	saved, err := w.c.SUP.SaveSupplierOffer(s.author, offer)
	w.must(err)
	supReq, err := w.c.SUP.RequestOf(offer)
	w.must(err)
	otherOffer := offer
	otherOffer.Data.RfqLineIDs = []string{"L1", "L2"}
	supOther, err := w.c.SUP.RequestOf(otherOffer)
	w.must(err)
	checkByHash(t, w, hashCase{name: "SUP supplier_offer.save_draft", schema: "brant_sup", request: supReq, conflict: supOther, receipt: saved.ActionID,
		full: func(ctx context.Context) (reconciledAs, error) { return supReconciled(w.c.SUP.Reconcile(ctx, offer)) },
		byHash: func(ctx context.Context, c, k, h string) (reconciledAs, error) {
			return supReconciled(w.c.SUP.ReconcileHash(ctx, c, k, h))
		},
		owner: s.author, noRead: reader("sup.noread", "company", "evd.read", "inq.read"), outsider: reader("sup.outsider", "own", "sup.read")})
	// A supply condition kind SUP does not know has no ledger identity.
	if _, err = w.c.SUP.RequestOf(supapi.Revise{IdempotencyKey: "k.x", Kind: "quote", ID: "x", ExpectedVersion: 1, BaseRevision: 1,
		Reason: "x"}); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("an unknown kind: %s", code(err))
	}

	// Reconciliations are reads: audited, nothing executed or published.
	reads := func() int {
		entries, err := w.c.SUP.Audit(s.author)
		w.must(err)
		n := 0
		for _, e := range entries {
			if e.Command == action.ReconcileQuery && e.Outcome == "read" && e.ObjectID == "offer.hash" {
				n++
			}
		}
		return n
	}
	events, audited := len(w.events("company.A", supapi.Source)), reads()
	if _, err = w.c.SUP.ReconcileHash(s.author, supReq.Command, supReq.IdempotencyKey, supReq.RequestHash); err != nil {
		t.Fatal(err)
	}
	if reads() != audited+1 || len(w.events("company.A", supapi.Source)) != events {
		t.Fatal("the reconciliation by hash was not audited as a read, or published an event")
	}
}

// The ActionReconciliation v1 of CST (R69) on the costing world: a costing context draft reconciled
// by its request hash as by the command; cst.read and the data scope are checked now.
func TestCSTReconcilesByRequestHash(t *testing.T) {
	c := newCostingWorld(t)
	w := c.world
	save := cstapi.SaveContext{IdempotencyKey: "k.hash.ctx", ContextID: "context.hash", Data: c.data()}
	saved, err := w.c.CST.SaveContext(c.author, save)
	w.must(err)
	req, err := w.c.CST.RequestOf(save)
	w.must(err)
	other := save
	other.Data.Kind = cstapi.ContextWhatIf
	conflict, err := w.c.CST.RequestOf(other)
	w.must(err)
	checkByHash(t, w, hashCase{name: "CST costing_context.save_draft", schema: "brant_cst", request: req, conflict: conflict, receipt: saved.ActionID,
		full: func(ctx context.Context) (reconciledAs, error) { return cstReconciled(w.c.CST.Reconcile(ctx, save)) },
		byHash: func(ctx context.Context, cmd, k, h string) (reconciledAs, error) {
			return cstReconciled(w.c.CST.ReconcileHash(ctx, cmd, k, h))
		},
		owner: c.author, noRead: w.actor("cst.noread", "inq.read"),
		outsider: w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "cst.outsider"), Permissions: []string{"cst.read"},
			DataScope: authorizationScope("own")})})
}

// The ActionReconciliation v1 of TRD (R18) on the recorded world (quote.1 activated, the customer's
// acceptance recorded and verified, a draft saved): every recorded command reconciles by its request
// hash as by the command, and also while a task holds the root (reconciliation is not fenced).
func TestTRDReconcilesByRequestHash(t *testing.T) {
	r := newRecorded(t)
	q := r.trdWorld
	trdCase := func(name string, cmds [2]trdapi.Command, receipt string, owner context.Context) hashCase {
		req, err := q.c.TRD.RequestOf(cmds[0])
		q.must(err)
		conflict, err := q.c.TRD.RequestOf(cmds[1])
		q.must(err)
		return hashCase{name: name, schema: "brant_trd", request: req, conflict: conflict, receipt: receipt,
			full: func(ctx context.Context) (reconciledAs, error) { return trdReconciled(q.c.TRD.Reconcile(ctx, cmds[0])) },
			byHash: func(ctx context.Context, c, k, h string) (reconciledAs, error) {
				return trdReconciled(q.c.TRD.ReconcileHash(ctx, c, k, h))
			},
			owner: owner, noRead: q.provision(grantOf(q, "trd.noread."+name, kernel.ActorHuman, "company", without(allPermissions, "trd.read")...)),
			outsider: q.provision(grantOf(q, "trd.outsider."+name, kernel.ActorHuman, "own", "trd.read", "evd.read"))}
	}
	receipts := map[string]string{}
	for name, cmds := range r.commands() {
		ctx := q.author
		if name == trdapi.CommandVerifyResponse {
			ctx = q.reviewer
		}
		rec, err := q.c.TRD.Reconcile(ctx, cmds[0])
		q.must(err)
		receipts[name] = rec.Receipt.ActionID
	}
	before := q.business()
	for name, cmds := range r.commands() {
		owner := q.author
		if name == trdapi.CommandVerifyResponse {
			owner = q.reviewer
		}
		checkByHash(t, q.world, trdCase(name, cmds, receipts[name], owner))
	}
	if q.business() != before {
		t.Fatal("a reconciliation by hash changed TRD's business state")
	}

	// A bound root fences writes and their replays, not reconciliation.
	_, err := q.bindTask(q.author, "quote.1", "task.1", 0)
	q.must(err)
	act, err := q.c.TRD.RequestOf(r.activate)
	q.must(err)
	if rec, err := q.c.TRD.ReconcileHash(q.author, act.Command, act.IdempotencyKey, act.RequestHash); err != nil ||
		rec.State != kernel.ReconcileCommitted || rec.Receipt == nil {
		t.Fatalf("on a bound root: %+v %v", rec, err)
	}
}

// Contract 06 §07 (来源权限撤回后，查询、同键写重放与对账均不能返回成功事实) by hash, Python
// test_t04_a45_response_query_recovery.py test_source_revocation_blocks_query_both_replays_and_both_hashes
// (PYT-0537, the reconciliation half) and t03_reconciliation.reconcile, which re-checks the customer
// response's sources through t04_response.validate_receipt: TRD stores the sources of a customer
// response, so quote.response.record and quote.response.verify reconciled by hash are refused, for
// both hashes, while the caller's data scope does not show the reply's evidence (REFERENCE_INVALID),
// as with the command. The sources another command cited (a withdrawal's evidence) are part of its
// request only, which a reconciliation by hash does not have: by hash it answers committed where the
// command itself is refused (S4 unit A3 decision, reported as a gap; TSK reconciles what it sent).
func TestTRDReconcileByHashRechecksTheStoredSources(t *testing.T) {
	q := newTrdWorld(t)
	q.mail()
	outsider := q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: actorRef("human", "outsider"), Permissions: allPermissions,
		DataScope: "team", Team: "other"})
	q.source(outsider, "foreign", "其他团队登记的客户邮件")
	foreign := []trdapi.Evidence{{SourceID: "foreign", Locator: "附件"}}
	a := q.activated("quote.1")
	record := q.full("response.1", "quote.1", 1, a.BusinessHash)
	record.Evidence = foreign
	_, err := q.c.TRD.RecordResponse(q.author, record)
	q.must(err)
	verify := q.verification("response.1", "acceptance.1", 1, a.BusinessHash)
	_, err = q.c.TRD.VerifyResponse(q.reviewer, verify)
	q.must(err)
	f := q.frozenQuote("quote.2")
	withdraw := trdapi.Withdraw{IdempotencyKey: q.key(), QuoteID: "quote.2", ExpectedVersion: f.RowVersion, BusinessRevision: 1,
		Reason: "客户撤回询价", Evidence: foreign}
	_, err = q.c.TRD.Withdraw(q.author, withdraw)
	q.must(err)
	teamOnly := func(id string) context.Context {
		return q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: actorRef("human", id), Permissions: allPermissions,
			DataScope: "team", Team: "trade"})
	}
	byHash := func(ctx context.Context, cmd trdapi.Command, hash string) string {
		t.Helper()
		r, err := q.c.TRD.RequestOf(cmd)
		q.must(err)
		if hash == "" {
			hash = r.RequestHash
		}
		rec, err := q.c.TRD.ReconcileHash(ctx, r.Command, r.IdempotencyKey, hash)
		if err != nil {
			return code(err)
		}
		return string(rec.State)
	}
	other := strings.Repeat("0", 64)
	author, reviewer := teamOnly("editor"), teamOnly("reviewer")
	for _, c := range []struct {
		ctx context.Context
		cmd trdapi.Command
	}{{author, record}, {reviewer, verify}} {
		for _, hash := range []string{"", other} {
			if got := byHash(c.ctx, c.cmd, hash); got != kernel.CodeReferenceInvalid {
				t.Errorf("%T by hash %q without the reply's source: %s", c.cmd, hash, got)
			}
		}
		if _, err = q.c.TRD.Reconcile(c.ctx, c.cmd); code(err) != kernel.CodeReferenceInvalid {
			t.Errorf("%T with the command: %s", c.cmd, code(err))
		}
	}
	if _, err = q.c.TRD.Reconcile(author, withdraw); code(err) != kernel.CodeReferenceInvalid {
		t.Errorf("the withdrawal with the command: %s", code(err))
	}
	if got := byHash(author, withdraw, ""); got != string(kernel.ReconcileCommitted) {
		t.Errorf("the withdrawal by hash: %s", got)
	}
	q.regrant("editor", "company", allPermissions)
	q.regrant("reviewer", "company", allPermissions)
	if got := byHash(q.author, record, ""); got != string(kernel.ReconcileCommitted) {
		t.Fatalf("the source readable again: %s", got)
	}
}
