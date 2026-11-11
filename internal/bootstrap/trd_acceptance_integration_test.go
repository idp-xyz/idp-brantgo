//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The customer response and acceptance suite (WBS S3-06, the contract-formation half of S3-13;
// TRD-INV-011, 012, 017; ADR-BR-013; SC-07, TD-AT03, BC-AT10–12) on the TRD world: "editor" (the
// author) drafts, activates and records the customer's replies; "reviewer" approves the quotes and
// verifies the replies; both are humans with every permission and company data scope unless a test
// provisions them otherwise. The customer's messages are the EVD source "mail" the editor
// registers, one locator per reply. The fixture quotes customer.1 at revision 2, contact.1 at row
// version 2 and line L1. Test names cite the Python V0.50 test they port.

// mail registers the customer's mailbox export, EVD source "mail", as the editor.
func (q *trdWorld) mail() {
	q.t.Helper()
	q.source(q.author, "mail", "合成客户邮件：确认接受报价全部条款与全部行。")
}

// reply is quote.response.record of response id to revision rev of quote at hash, classified class,
// in message mail at locator id, that reached us at at on basis (none when at is zero).
func (q *trdWorld) reply(id, quoteID string, rev int64, hash, class string, at time.Time, basis string) trdapi.RecordResponse {
	c := trdapi.RecordResponse{IdempotencyKey: q.key(), ResponseID: id, QuoteID: quoteID, BusinessRevision: rev, BusinessHash: hash,
		Classification: class, Summary: "客户回复：" + class, SourceRef: trdapi.Evidence{SourceID: "mail", Locator: id}}
	if !at.IsZero() {
		c.ReceivedAt, c.ReceivedAtBasis = kernel.FormatTimestamp(at), basis
	}
	return c
}

// full is a full_unconditional reply that reached the channel server now.
func (q *trdWorld) full(id, quoteID string, rev int64, hash string) trdapi.RecordResponse {
	return q.reply(id, quoteID, rev, hash, trdapi.ClassificationFullUnconditional, time.Now(), trdapi.ArrivalChannelServer)
}

// verification is quote.response.verify of response into acceptance, restating revision rev at hash
// as the fixture quotes it.
func (q *trdWorld) verification(response, acceptance string, rev int64, hash string) trdapi.VerifyResponse {
	return trdapi.VerifyResponse{IdempotencyKey: q.key(), ResponseID: response, AcceptanceID: acceptance, BusinessRevision: rev,
		BusinessHash: hash, CustomerRef: trdapi.PartyRef{PartyID: "customer.1", Revision: 2},
		ContactRef: trdapi.ContactRef{ContactID: "contact.1", RowVersion: 2}, LineIDs: []string{"L1"}}
}

// accepted activates quote id, records the customer's full acceptance as "response.<id>" and has
// the reviewer verify it as "acceptance.<id>".
func (q *trdWorld) accepted(id string) (trdapi.FormalizationReceipt, trdapi.AcceptanceReceipt) {
	q.t.Helper()
	a := q.activated(id)
	_, err := q.c.TRD.RecordResponse(q.author, q.full("response."+id, id, 1, a.BusinessHash))
	q.must(err)
	v, err := q.c.TRD.VerifyResponse(q.reviewer, q.verification("response."+id, "acceptance."+id, 1, a.BusinessHash))
	q.must(err)
	return a, v
}

func (q *trdWorld) invalidation(id string, expected int64) trdapi.InvalidateAcceptance {
	return trdapi.InvalidateAcceptance{IdempotencyKey: q.key(), AcceptanceID: id, ExpectedEffectivityVersion: expected,
		Reason: "核实人认错了联系人", Evidence: q.proof}
}

func (q *trdWorld) dispute(id string, expected int64) trdapi.DisputeAcceptance {
	return trdapi.DisputeAcceptance{IdempotencyKey: q.key(), AcceptanceID: id, ExpectedEffectivityVersion: expected,
		Reason: "客户否认发出该邮件", Evidence: q.proof}
}

// acceptanceGuard calls AcceptanceGuard v1 in company.A as a downstream transaction would.
func (q *trdWorld) acceptanceGuard(b trdapi.AcceptanceBinding) trdapi.AcceptanceVerdict {
	q.t.Helper()
	var v trdapi.AcceptanceVerdict
	q.must(q.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = trdapi.ScanAcceptanceGuard(b, exec.QueryRow(ctx, trdapi.AcceptanceGuardSQL, trdapi.AcceptanceGuardArgs(b)...).Scan)
		return err
	}))
	return v
}

func bindingOf(v trdapi.AcceptanceReceipt) trdapi.AcceptanceBinding {
	return trdapi.AcceptanceBinding{AcceptanceID: v.ObjectID, QuoteID: v.QuoteID, BusinessRevision: v.BusinessRevision, BusinessHash: v.BusinessHash}
}

// fact is what the lookup says of acceptance id now.
func (q *trdWorld) fact(id string) trdapi.AcceptanceFact {
	q.t.Helper()
	found, err := q.c.TRD.ResolveAcceptances(q.author, []string{id})
	q.must(err)
	return found[id]
}

// Python test_t04_a36_customer_response.py test_conditional_record_is_not_an_acceptance_or_order
// (PYT-0461), test_t04_a37_response_verification.py
// test_full_unconditional_response_becomes_bound_acceptance_and_order (PYT-0468, the acceptance
// half), test_t04_a44_public_customer_response.py test_public_record_and_verify_commit_and_replay
// (PYT-0526) and test_read_reconciliation_supports_both_public_commands (PYT-0529),
// test_t04_a45_response_query_recovery.py test_new_public_registration_and_unverified_projection
// (PYT-0530), test_verified_projection_and_recovery_are_read_only (PYT-0531),
// test_all_five_classifications_remain_observations (PYT-0532) and
// test_new_public_response_feeds_existing_order_workflow (PYT-0550, what the order wave reads), and
// test_t03_a20_acceptance_order.py test_acceptance_is_version_bound_and_order_full_v1_is_idempotent
// (PYT-0166, the acceptance half); TRD-INV-011, ES-05, BC-AT10: an AI agent and a human record the
// customer's replies against the active formal revision, bound to its hash, number and validity;
// no classification creates an acceptance; a conditional reply is never verified; the reviewer
// verifies the full acceptance into an acceptance bound to the revision, valid at effectivity
// version 1 and formed at its arrival; keys replay, the same response or verification under another
// key answers from the stored one, both commands reconcile, and the reads, the lookups and the
// guard tell the order wave what to bind.
func TestCustomerResponseRecordAndVerifyLifecycle(t *testing.T) {
	q := newTrdWorld(t)
	agent := q.actorOf(kernel.ActorAI, "agent.1")
	a := q.activated("quote.1")
	q.mail()
	for _, class := range []string{trdapi.ClassificationConditional, trdapi.ClassificationPartial, trdapi.ClassificationReject,
		trdapi.ClassificationAmbiguous} {
		c := q.reply("response."+class, "quote.1", 1, a.BusinessHash, class, time.Now(), trdapi.ArrivalChannelServer)
		if class == trdapi.ClassificationAmbiguous {
			// test_t04_a45 test_local_long_summary_and_timestamp_are_preserved (PYT-0533).
			c.Summary = strings.Repeat("合成回复", 900)
		}
		r, err := q.c.TRD.RecordResponse(agent, c)
		if err != nil || r.Status != trdapi.StatusRecorded || r.ObjectID != "response."+class || r.RowVersion != 1 || r.Number != a.Number ||
			r.Classification != class || r.Late || r.QuoteID != "quote.1" || r.BusinessHash != a.BusinessHash {
			t.Fatalf("an agent records %s: %+v %v", class, r, err)
		}
		if v, err := q.c.TRD.GetCustomerResponse(q.author, c.ResponseID); err != nil || v.Summary != c.Summary || v.RecordedBy.ID != "agent.1" ||
			!v.RecordedAt.Equal(r.RecordedAt) || v.Classification != class {
			t.Fatalf("%s reads %+v %v", class, v, err)
		}
	}
	at := time.Now()
	cmd := q.reply("response.1", "quote.1", 1, a.BusinessHash, trdapi.ClassificationFullUnconditional, at, trdapi.ArrivalChannelServer)
	cmd.CustomerPONumber, cmd.Evidence = "PO-2026-017", q.proof
	r, err := q.c.TRD.RecordResponse(q.author, cmd)
	q.must(err)
	again, err := q.c.TRD.RecordResponse(q.author, cmd)
	if err != nil || !again.Replayed || again.ActionID != r.ActionID || again.Number != a.Number {
		t.Fatalf("the same key %+v %v", again, err)
	}
	same := cmd
	same.IdempotencyKey = q.key()
	if other, err := q.c.TRD.RecordResponse(q.author, same); err != nil || !other.Replayed || other.ObjectID != "response.1" ||
		!other.RecordedAt.Equal(r.RecordedAt) {
		t.Fatalf("the same response under another key %+v %v", other, err)
	}
	view, err := q.c.TRD.GetCustomerResponse(q.author, "response.1")
	q.must(err)
	if view.Classification != trdapi.ClassificationFullUnconditional || view.Number != a.Number || view.BusinessRevision != 1 ||
		!view.ReceivedAt.Equal(kernel.Instant(at)) || view.ReceivedAtBasis != trdapi.ArrivalChannelServer || view.RecordedBy.ID != "editor" ||
		view.CustomerPONumber != "PO-2026-017" || len(view.Evidence) != 1 || view.SourceRef.SourceID != "mail" || view.Late ||
		len(view.Acceptances) != 0 {
		t.Fatalf("the unverified response %+v", view)
	}
	if n := q.count("company.A", "brant_trd.acceptance"); n != 0 {
		t.Fatalf("%d acceptances from observations", n)
	}
	_, err = q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.conditional", "acceptance.c", 1, a.BusinessHash))
	if code(err) != trdapi.CodeAcceptanceNotUnconditional {
		t.Fatalf("a conditional reply verified: %s", code(err))
	}

	verify := q.verification("response.1", "acceptance.1", 1, a.BusinessHash)
	v, err := q.c.TRD.VerifyResponse(q.reviewer, verify)
	q.must(err)
	if v.Status != trdapi.AcceptanceValid || v.RowVersion != 1 || v.ObjectID != "acceptance.1" || v.ResponseID != "response.1" ||
		v.QuoteID != "quote.1" || v.BusinessRevision != 1 || v.BusinessHash != a.BusinessHash || v.Number != a.Number || !v.Formed ||
		!v.ArrivalAt.Equal(kernel.Instant(at)) || v.ArrivalBasis != trdapi.ArrivalChannelServer || !v.ExpiresAt.After(time.Now()) || v.Replayed {
		t.Fatalf("the verification %+v", v)
	}
	if replay, err := q.c.TRD.VerifyResponse(q.reviewer, verify); err != nil || !replay.Replayed || replay.ActionID != v.ActionID {
		t.Fatalf("the same key %+v %v", replay, err)
	}
	view, err = q.c.TRD.GetCustomerResponse(q.author, "response.1")
	q.must(err)
	if len(view.Acceptances) != 1 || view.Acceptances[0].AcceptanceID != "acceptance.1" || view.Acceptances[0].VerifiedBy.ID != "reviewer" {
		t.Fatalf("the verified response %+v", view.Acceptances)
	}
	got, err := q.c.TRD.GetAcceptance(q.author, "acceptance.1")
	q.must(err)
	if got.Effectivity != trdapi.AcceptanceValid || got.EffectivityVersion != 1 || len(got.History) != 1 || got.History[0].ChangedBy.ID != "reviewer" ||
		got.CustomerRef.PartyID != "customer.1" || got.ContactRef.RowVersion != 2 || !slices.Equal(got.LineIDs, []string{"L1"}) ||
		got.SourceRef != (trdapi.Evidence{SourceID: "mail", Locator: "response.1"}) || !got.Formed {
		t.Fatalf("the acceptance %+v", got)
	}
	// What the order wave binds: the acceptance at its effectivity version, its arrival and formation.
	f := q.fact("acceptance.1")
	if f.Code != "" || f.Check("/x") != nil || f.EffectivityVersion != 1 || !f.Formed || !f.ArrivalAt.Equal(kernel.Instant(at)) ||
		f.CheckFor(trdapi.QuoteRevisionBinding{QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: a.BusinessHash}, "/x") != nil {
		t.Fatalf("the lookup %+v", f)
	}
	byRevision, err := q.c.TRD.ResolveRevisionAcceptances(q.author, []trdapi.QuoteRevisionRef{{QuoteID: "quote.1", BusinessRevision: 1}})
	if err != nil || byRevision[trdapi.QuoteRevisionRef{QuoteID: "quote.1", BusinessRevision: 1}].AcceptanceID != "acceptance.1" {
		t.Fatalf("the revision's acceptance %+v %v", byRevision, err)
	}
	if g := q.acceptanceGuard(bindingOf(v)); !g.Usable() || g.ResponseID != "response.1" || g.EffectivityVersion != 1 || !g.Formed {
		t.Fatalf("the guard %+v", g)
	}
	recorded, verified := q.eventsOf(trdapi.EventCustomerResponseRecorded), q.eventsOf(trdapi.EventAcceptanceVerified)
	if len(recorded) != 5 || len(verified) != 1 || verified[0].Payload["acceptance_id"] != "acceptance.1" ||
		verified[0].Payload["formed"] != true || verified[0].Payload["arrival_basis"] != trdapi.ArrivalChannelServer ||
		verified[0].Payload["aggregate_type"] != "Acceptance" || recorded[4].Payload["number"] != a.Number || recorded[4].Payload["late"] != false {
		t.Fatalf("events %d recorded, %+v", len(recorded), verified)
	}
	conflicting, otherVerify := cmd, verify
	conflicting.Summary, otherVerify.LineIDs = "different", []string{"L9"}
	for _, c := range []struct {
		cmd  trdapi.Command
		want kernel.ReconcileState
	}{{cmd, kernel.ReconcileCommitted}, {verify, kernel.ReconcileCommitted}, {conflicting, kernel.ReconcilePayloadConflict},
		{otherVerify, kernel.ReconcilePayloadConflict}} {
		rec, err := q.c.TRD.Reconcile(q.reviewer, c.cmd)
		if err != nil || rec.State != c.want || (c.want == kernel.ReconcileCommitted) != (rec.Receipt != nil) {
			t.Fatalf("reconcile %T: %+v %v", c.cmd, rec, err)
		}
	}
	if n, m := q.count("company.A", "brant_trd.acceptance"), q.count("company.A", "brant_trd.acceptance_effectivity"); n != 1 || m != 1 {
		t.Fatalf("%d acceptances, %d ledger entries", n, m)
	}
}

// TRD-INV-011's binding and quote.Usable (the activation wave): a reply answers the quote's active
// formal revision at its hash — a revision never formalized is REFERENCE_INVALID, another hash
// BUSINESS_HASH_MISMATCH, a revision paused by a risk hold after its approval was revoked
// APPROVAL_REVOKED, a cancelled or superseded one QUOTE_NOT_CURRENT — and a verification re-checks
// it; a verifiable arrival after the recording or before the formalization is
// ACCEPTANCE_TIME_CONFLICT. Nothing is written by a refusal; recording does not move the root version.
func TestResponsesAnswerOnlyTheActiveUnheldRevision(t *testing.T) {
	q := newTrdWorld(t)
	q.mail()
	f, _ := q.approved("quote.p")
	_, err := q.c.TRD.RecordResponse(q.author, q.full("response.p", "quote.p", 1, f.BusinessHash))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/business_revision" {
		t.Fatalf("a revision pending approval: %s at %s", code(err), path(err))
	}
	a := q.activated("quote.1")
	_, err = q.c.TRD.RecordResponse(q.author, q.full("response.x", "quote.1", 1, sha256Hex("other")))
	q.want(err, trdapi.CodeBusinessHashMismatch)
	_, err = q.c.TRD.RecordResponse(q.author, q.reply("response.x", "quote.1", 1, a.BusinessHash, trdapi.ClassificationFullUnconditional,
		time.Now().Add(time.Hour), trdapi.ArrivalChannelServer))
	q.want(err, trdapi.CodeAcceptanceTimeConflict)
	committed := q.getQuote("quote.1").Revisions[0].Formal.Commit.CommittedAt
	_, err = q.c.TRD.RecordResponse(q.author, q.reply("response.x", "quote.1", 1, a.BusinessHash, trdapi.ClassificationFullUnconditional,
		committed.Add(-time.Microsecond), trdapi.ArrivalImportRecord))
	q.want(err, trdapi.CodeAcceptanceTimeConflict)

	// Cancelled after the reply was recorded: the root version did not move, the verification and a
	// new reply are refused.
	_, err = q.c.TRD.RecordResponse(q.author, q.full("response.c", "quote.1", 1, a.BusinessHash))
	q.must(err)
	if v := q.getQuote("quote.1"); v.RootVersion != a.RowVersion {
		t.Fatalf("recording moved the root to %d", v.RootVersion)
	}
	_, err = q.c.TRD.Cancel(q.author, trdapi.Cancel{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: a.RowVersion, BusinessRevision: 1,
		Reason: "客户取消询价", Evidence: q.proof})
	q.must(err)
	_, err = q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.c", "acceptance.c", 1, a.BusinessHash))
	q.want(err, trdapi.CodeQuoteNotCurrent)
	_, err = q.c.TRD.RecordResponse(q.author, q.full("response.c2", "quote.1", 1, a.BusinessHash))
	q.want(err, trdapi.CodeQuoteNotCurrent)

	// Held: the approval the revision was formalized with is revoked.
	h := q.activated("quote.h")
	_, err = q.c.TRD.RecordResponse(q.author, q.full("response.h", "quote.h", 1, h.BusinessHash))
	q.must(err)
	_, err = q.c.TRD.RevokeApproval(q.reviewer, trdapi.RevokeApproval{IdempotencyKey: q.key(), QuoteID: "quote.h", ApprovalID: "approval.quote.h",
		ExpectedEffectivityVersion: 1, Reason: "客户信用变化", Evidence: q.proof})
	q.must(err)
	_, err = q.c.TRD.RecordResponse(q.author, q.full("response.h2", "quote.h", 1, h.BusinessHash))
	q.want(err, trdapi.CodeApprovalRevoked)
	_, err = q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.h", "acceptance.h", 1, h.BusinessHash))
	q.want(err, trdapi.CodeApprovalRevoked)

	// Superseded by a replacement.
	s := q.activated("quote.s")
	_, err = q.c.TRD.RecordResponse(q.author, q.full("response.s", "quote.s", 1, s.BusinessHash))
	q.must(err)
	n, _, version := q.revisedAndApproved("quote.s", s.RowVersion, trdapi.TargetReplace, "review.s.2", "approval.s.2")
	_, err = q.c.TRD.Replace(q.author, q.replaceCmd("quote.s", version, n, "approval.s.2"))
	q.must(err)
	_, err = q.c.TRD.RecordResponse(q.author, q.full("response.s2", "quote.s", 1, s.BusinessHash))
	q.want(err, trdapi.CodeQuoteNotCurrent)
	_, err = q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.s", "acceptance.s", 1, s.BusinessHash))
	q.want(err, trdapi.CodeQuoteNotCurrent)

	if r, acc := q.count("company.A", "brant_trd.customer_response"), q.count("company.A", "brant_trd.acceptance"); r != 3 || acc != 0 {
		t.Fatalf("%d responses, %d acceptances after the refusals", r, acc)
	}
}

// Python test_t04_a36 test_permissions_scope_and_source (PYT-0463, the source half) and
// test_t04_a45 test_source_scope_change_is_rechecked_independently_of_quote (PYT-0538): the recorder
// reads the customer's message and every evidence source, the verifier reads the response's, a
// reader of the response reads its sources (REFERENCE_INVALID otherwise, although the reader sees
// the quote), and an effectivity change's new evidence is read by its actor.
func TestActorsReadTheEvidenceTheyCite(t *testing.T) {
	q := newTrdWorld(t)
	outsider := q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: actorRef("human", "outsider"), Permissions: allPermissions,
		DataScope: "team", Team: "other"})
	clerk := q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: actorRef("human", "clerk"), Permissions: allPermissions,
		DataScope: "team", Team: "trade"})
	q.source(outsider, "foreign", "其他团队登记的客户邮件")
	q.mail()
	a := q.activated("quote.1")
	foreign := q.full("response.f", "quote.1", 1, a.BusinessHash)
	foreign.SourceRef.SourceID = "foreign"
	_, err := q.c.TRD.RecordResponse(clerk, foreign)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/source_ref/source_id" {
		t.Fatalf("an unread message: %s at %s", code(err), path(err))
	}
	missing := q.full("response.m", "quote.1", 1, a.BusinessHash)
	missing.Evidence = []trdapi.Evidence{{SourceID: "evidence", Locator: "p"}, {SourceID: "foreign", Locator: "p"}}
	_, err = q.c.TRD.RecordResponse(clerk, missing)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence/1/source_id" {
		t.Fatalf("unread evidence: %s at %s", code(err), path(err))
	}
	foreign.IdempotencyKey = q.key()
	_, err = q.c.TRD.RecordResponse(q.author, foreign)
	q.must(err)
	verify := q.verification("response.f", "acceptance.f", 1, a.BusinessHash)
	_, err = q.c.TRD.VerifyResponse(clerk, verify)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/response_id" {
		t.Fatalf("a verifier who does not read the message: %s at %s", code(err), path(err))
	}
	_, err = q.c.TRD.GetCustomerResponse(clerk, "response.f")
	q.want(err, kernel.CodeReferenceInvalid)
	_, err = q.c.TRD.GetCustomerResponse(q.author, "response.f")
	q.must(err)
	verify.IdempotencyKey = q.key()
	_, err = q.c.TRD.VerifyResponse(q.reviewer, verify)
	q.must(err)
	change := q.invalidation("acceptance.f", 1)
	change.Evidence = []trdapi.Evidence{{SourceID: "foreign", Locator: "更正"}}
	_, err = q.c.TRD.InvalidateAcceptance(clerk, change)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence/0/source_id" {
		t.Fatalf("unread new evidence: %s at %s", code(err), path(err))
	}
	if f := q.fact("acceptance.f"); f.Effectivity != trdapi.AcceptanceValid {
		t.Fatalf("after the refusal %+v", f)
	}
}

// Python test_t04_a36 test_permissions_scope_and_source (PYT-0463, the grant half), test_t04_a37
// test_identity_permissions_and_company_scope (PYT-0474), test_t04_a45
// test_read_grants_and_company_tenant_isolation (PYT-0535), test_own_team_and_company_scope_are_current
// (PYT-0536), test_revoked_write_grants_keep_read_only_recovery_available (PYT-0554) and
// test_read_audit_is_atomic_and_failure_has_no_business_effect (PYT-0548, the audit half), and
// test_t03_a20 test_acceptance_and_order_queries_are_scoped (PYT-0169, the acceptance half): each
// command needs its permission and verification a human; another company sees nothing; the reads
// need trd.read (and EVD's read for the response's sources), the data scope applies to them and to
// the lookups; reads and reconciliation stay available to a reader whose write grants were
// revoked, a replay of the write does not; reads are audited.
func TestResponseAndAcceptanceAccessFollowGrantsAndScope(t *testing.T) {
	q := newTrdWorld(t)
	agent := q.actorOf(kernel.ActorAI, "agent.1")
	restricted := q.regrant("restricted", "company", []string{"trd.read", "evd.read"})
	foreigner := q.provision(bootstrap.Grant{Scope: q.scope("company.B"), Actor: actorRef("human", "foreigner"), Permissions: allPermissions,
		DataScope: "company"})
	q.mail()
	a := q.activated("quote.1")
	cmd := q.full("response.1", "quote.1", 1, a.BusinessHash)
	_, err := q.c.TRD.RecordResponse(restricted, cmd)
	q.want(err, kernel.CodePermissionDenied)
	_, err = q.c.TRD.RecordResponse(q.author, cmd)
	q.must(err)
	verify := q.verification("response.1", "acceptance.1", 1, a.BusinessHash)
	for name, ctx := range map[string]context.Context{"an AI agent": agent, "a reader": restricted} {
		if _, err = q.c.TRD.VerifyResponse(ctx, verify); code(err) != kernel.CodePermissionDenied {
			t.Fatalf("%s verifies: %s", name, code(err))
		}
	}
	_, err = q.c.TRD.VerifyResponse(foreigner, verify)
	q.want(err, kernel.CodeNotFound)
	_, err = q.c.TRD.VerifyResponse(q.reviewer, verify)
	q.must(err)
	if _, err = q.c.TRD.InvalidateAcceptance(agent, q.invalidation("acceptance.1", 1)); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("an AI agent invalidates: %s", code(err))
	}

	if _, err = q.c.TRD.GetCustomerResponse(restricted, "response.1"); err != nil {
		t.Fatalf("a reader reads the response: %v", err)
	}
	for name, grants := range map[string][]string{"no grant": {}, "EVD only": {"evd.read"}, "TRD only": {"trd.read"}} {
		_, err := q.c.TRD.GetCustomerResponse(q.regrant("limited."+strings.ReplaceAll(name, " ", "."), "company", grants), "response.1")
		if code(err) != kernel.CodePermissionDenied {
			t.Errorf("%s reads the response: %s", name, code(err))
		}
	}
	if _, err = q.c.TRD.GetAcceptance(q.regrant("trd.reader", "company", []string{"trd.read"}), "acceptance.1"); err != nil {
		t.Fatalf("a TRD reader reads the acceptance: %v", err)
	}
	own := q.regrant("own", "own", []string{"trd.read", "evd.read"})
	_, err = q.c.TRD.GetCustomerResponse(own, "response.1")
	q.want(err, kernel.CodeNotFound)
	_, err = q.c.TRD.GetAcceptance(own, "acceptance.1")
	q.want(err, kernel.CodeNotFound)
	if found, err := q.c.TRD.ResolveAcceptances(own, []string{"acceptance.1"}); err != nil || len(found) != 0 {
		t.Fatalf("an own-scope lookup %+v %v", found, err)
	}
	for name, ctx := range map[string]context.Context{"another company": foreigner, "a missing response": q.author} {
		id := "response.1"
		if ctx == q.author {
			id = "response.missing"
		}
		if _, err = q.c.TRD.GetCustomerResponse(ctx, id); code(err) != kernel.CodeNotFound {
			t.Errorf("%s: %s", name, code(err))
		}
	}
	auditor := q.actorOf(kernel.ActorHuman, "auditor")
	entries, err := q.c.TRD.Audit(auditor)
	q.must(err)
	var read, refused bool
	for _, e := range entries {
		read = read || (e.Command == "quote.response.get" && e.ObjectID == "response.1" && e.Outcome == "read")
		refused = refused || (e.Command == "quote.response.get" && e.ObjectID == "response.missing" && e.Outcome == "rejected" &&
			e.Code == kernel.CodeNotFound)
	}
	if !read || !refused {
		t.Fatalf("the reads are not audited: read %v, refused %v", read, refused)
	}

	// Write grants revoked: reads and reconciliation stay, replays of the writes do not.
	editor := q.regrant("editor", "company", []string{"trd.read", "evd.read"})
	reviewer := q.regrant("reviewer", "company", []string{"trd.read", "evd.read"})
	if _, err = q.c.TRD.GetCustomerResponse(editor, "response.1"); err != nil {
		t.Fatalf("the former recorder reads: %v", err)
	}
	for _, c := range []struct {
		ctx context.Context
		cmd trdapi.Command
	}{{editor, cmd}, {reviewer, verify}} {
		if rec, err := q.c.TRD.Reconcile(c.ctx, c.cmd); err != nil || rec.State != kernel.ReconcileCommitted {
			t.Fatalf("reconcile %T: %+v %v", c.cmd, rec, err)
		}
	}
	_, err = q.c.TRD.RecordResponse(editor, cmd)
	q.want(err, kernel.CodePermissionDenied)
	_, err = q.c.TRD.VerifyResponse(reviewer, verify)
	q.want(err, kernel.CodePermissionDenied)
}

// Python test_t04_a36 test_classifications_and_immutable_id (PYT-0462, RESPONSE_IMMUTABLE),
// test_t04_a37 test_quote_scope_is_rechecked (PYT-0470), test_response_binding_and_source_message_are_unique
// (PYT-0471) and test_existing_acceptance_cannot_be_attached_and_invalid_unicode_is_rejected (PYT-0475,
// ACCEPTANCE_ALREADY_EXISTS), test_t04_a44 test_closed_schema_and_payload_conflict (PYT-0527, the
// payload conflict) and test_verification_reuses_existing_business_guard_and_does_not_repeat_acceptance
// (PYT-0528), test_t03_a20 test_late_and_duplicate_confirmation_are_rejected_without_residue (PYT-0167,
// the duplicate half); BC-AT11, TRD-INV-011: a response ID keeps its content; a verification
// restates the revision's hash, customer, contact and complete line set; a verified response keeps
// its acceptance; a message backs one live acceptance and a revision has one valid acceptance.
func TestVerificationScopeImmutabilityAndUniqueness(t *testing.T) {
	q := newTrdWorld(t)
	q.mail()
	a := q.activated("quote.1")
	b := q.activated("quote.2")
	_, err := q.c.TRD.RecordResponse(q.author, q.full("response.1", "quote.1", 1, a.BusinessHash))
	q.must(err)
	changed := q.full("response.1", "quote.1", 1, a.BusinessHash)
	changed.Summary = "different"
	_, err = q.c.TRD.RecordResponse(q.author, changed)
	q.want(err, trdapi.CodeResponseImmutable)
	second := q.full("response.2", "quote.1", 1, a.BusinessHash)
	_, err = q.c.TRD.RecordResponse(q.author, second)
	q.must(err)
	second.Summary = "different"
	_, err = q.c.TRD.RecordResponse(q.author, second)
	q.want(err, kernel.CodeIdempotencyPayloadConflict)

	for want, edit := range map[string]func(v *trdapi.VerifyResponse){
		"/args/business_hash":     func(v *trdapi.VerifyResponse) { v.BusinessHash = sha256Hex("other") },
		"/args/business_revision": func(v *trdapi.VerifyResponse) { v.BusinessRevision = 2 },
		"/args/customer_ref":      func(v *trdapi.VerifyResponse) { v.CustomerRef.Revision = 1 },
		"/args/contact_ref":       func(v *trdapi.VerifyResponse) { v.ContactRef.ContactID = "contact.other" },
	} {
		v := q.verification("response.1", "acceptance.1", 1, a.BusinessHash)
		edit(&v)
		if _, err = q.c.TRD.VerifyResponse(q.reviewer, v); code(err) != trdapi.CodeAcceptanceScopeMismatch || path(err) != want {
			t.Errorf("%s: %s at %s", want, code(err), path(err))
		}
	}
	lines := q.verification("response.1", "acceptance.1", 1, a.BusinessHash)
	lines.LineIDs = []string{"L1", "L2"}
	_, err = q.c.TRD.VerifyResponse(q.reviewer, lines)
	q.want(err, trdapi.CodeAcceptanceLinesMismatch)

	v, err := q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.1", "acceptance.1", 1, a.BusinessHash))
	q.must(err)
	other := q.verification("response.1", "acceptance.1", 1, a.BusinessHash)
	other.ContactRef.RowVersion = 1
	_, err = q.c.TRD.VerifyResponse(q.reviewer, other)
	q.want(err, trdapi.CodeResponseVerificationImmutable)
	if again, err := q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.1", "acceptance.1", 1, a.BusinessHash)); err != nil ||
		!again.Replayed || again.ObjectID != "acceptance.1" || again.ActionID == v.ActionID {
		t.Fatalf("the same verification under another key %+v %v", again, err)
	}
	_, err = q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.2", "acceptance.1", 1, a.BusinessHash))
	if code(err) != trdapi.CodeAcceptanceAlreadyExists || path(err) != "/args/acceptance_id" {
		t.Fatalf("a taken acceptance ID: %s at %s", code(err), path(err))
	}
	_, err = q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.2", "acceptance.2", 1, a.BusinessHash))
	if code(err) != trdapi.CodeAcceptanceAlreadyExists || path(err) != "/args/business_revision" {
		t.Fatalf("a second acceptance of the revision: %s at %s", code(err), path(err))
	}
	// The same message recorded again, for quote.2, cannot be confirmed twice.
	dup := q.full("response.dup", "quote.2", 1, b.BusinessHash)
	dup.SourceRef.Locator = "response.1"
	_, err = q.c.TRD.RecordResponse(q.author, dup)
	q.must(err)
	_, err = q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.dup", "acceptance.dup", 1, b.BusinessHash))
	q.want(err, trdapi.CodeSourceMessageDuplicate)
	if n := q.count("company.A", "brant_trd.acceptance"); n != 1 {
		t.Fatalf("%d acceptances", n)
	}
}

// Python test_t04_a36 test_failure_rolls_back_and_reopens (PYT-0464), test_t04_a37
// test_verification_and_acceptance_roll_back_together (PYT-0472) and test_t04_a45
// test_public_verification_audit_failure_rolls_back_all_facts (PYT-0551); TRD-INV-016: a failing
// Outbox write rolls the recording, the verification or the invalidation back whole, and the same
// key succeeds once the store accepts the write.
func TestFailedWritesLeaveNoResponseOrAcceptance(t *testing.T) {
	q := newTrdWorld(t)
	q.mail()
	a := q.activated("quote.1")
	cmd := q.full("response.1", "quote.1", 1, a.BusinessHash)
	failed := func(err error) {
		t.Helper()
		if _, refused := kernel.AsRejection(err); err == nil || refused || !strings.Contains(err.Error(), "test storage failure") {
			t.Fatalf("with a failing Outbox: %v", err)
		}
	}
	drop := q.failOutbox(trdapi.EventCustomerResponseRecorded)
	_, err := q.c.TRD.RecordResponse(q.author, cmd)
	failed(err)
	if n := q.count("company.A", "brant_trd.customer_response"); n != 0 {
		t.Fatalf("%d responses after the failure", n)
	}
	drop()
	if r, err := q.c.TRD.RecordResponse(q.author, cmd); err != nil || r.Replayed {
		t.Fatalf("retry %+v %v", r, err)
	}
	verify := q.verification("response.1", "acceptance.1", 1, a.BusinessHash)
	drop = q.failOutbox(trdapi.EventAcceptanceVerified)
	_, err = q.c.TRD.VerifyResponse(q.reviewer, verify)
	failed(err)
	if n, m := q.count("company.A", "brant_trd.acceptance"), q.count("company.A", "brant_trd.acceptance_effectivity"); n != 0 || m != 0 {
		t.Fatalf("%d acceptances, %d ledger entries after the failure", n, m)
	}
	if rec, err := q.c.TRD.Reconcile(q.reviewer, verify); err != nil || rec.State != kernel.ReconcileNotCommitted {
		t.Fatalf("reconcile after the failure %+v %v", rec, err)
	}
	drop()
	if v, err := q.c.TRD.VerifyResponse(q.reviewer, verify); err != nil || v.Replayed {
		t.Fatalf("retry %+v %v", v, err)
	}
	invalidate := q.invalidation("acceptance.1", 1)
	drop = q.failOutbox(trdapi.EventAcceptanceInvalidated)
	_, err = q.c.TRD.InvalidateAcceptance(q.reviewer, invalidate)
	failed(err)
	if f := q.fact("acceptance.1"); f.Effectivity != trdapi.AcceptanceValid || q.count("company.A", "brant_trd.acceptance_effectivity") != 1 {
		t.Fatalf("after the failure %+v", f)
	}
	drop()
	if r, err := q.c.TRD.InvalidateAcceptance(q.reviewer, invalidate); err != nil || r.Status != trdapi.AcceptanceInvalidated {
		t.Fatalf("retry %+v %v", r, err)
	}
}

// shortlyActivated saves, freezes, approves and activates the fixture as quote id with a validity
// ending in validity from now; it returns the activation and the quote's expires_at.
func (q *trdWorld) shortlyActivated(id string, validity time.Duration) (trdapi.FormalizationReceipt, time.Time) {
	q.t.Helper()
	expires := kernel.Instant(time.Now().Add(validity))
	d := q.data()
	d.ExpiresAt = kernel.FormatTimestamp(expires)
	_, err := q.saveQuote(q.author, id, 0, d)
	q.must(err)
	f, err := q.freezeQuote(q.author, id, 1, 1)
	q.must(err)
	_, err = q.submit(q.author, id, f.RowVersion, 1, "review."+id)
	q.must(err)
	decide := q.approval(id, f.RowVersion+1, "review."+id, f.BusinessHash, "approval."+id)
	decide.ExpiresAt = kernel.FormatTimestamp(expires.Add(-time.Second))
	dec, err := q.c.TRD.DecideReview(q.reviewer, decide)
	q.must(err)
	a, err := q.c.TRD.Activate(q.author, q.activateCmd(id, dec.RowVersion, 1, "approval."+id))
	q.must(err)
	return a, expires
}

// SC-07, ES-08, TD-AT03 and BC-AT12 on PostgreSQL 16 (TRD-INV-012, 017; ADR-BR-013), with Python
// test_t04_a37 test_expiry_boundary_and_historical_replay (PYT-0473, as a deliberate divergence),
// test_t03_a20 test_late_and_duplicate_confirmation_are_rejected_without_residue (PYT-0167, the late
// half), test_t04_a45 test_restart_and_expiry_keep_historical_recovery (PYT-0540) and
// test_task_control_blocks_writes_but_allows_historical_observation (PYT-0539): the quote expires
// seconds after its activation. An acceptance that reached the channel server before then is
// verified after it and forms the contract at its arrival; the lookups and the guard keep saying so
// after the quote expired. One that reached it exactly at expires_at is late — recorded as a late
// reply, never verified (ACCEPTANCE_LATE) — and so are a reply whose only time is the sender's claim
// and one with no time at all, both judged by their verification after the quote expired. The
// verification replays after expiry, and a binding fences the replay unless it carries the task's
// claim, while reads and reconciliation stay.
func TestAcceptanceArrivingBeforeExpiryFormsTheContractAfterExpiry(t *testing.T) {
	q := newTrdWorld(t)
	tsk := q.actorOf(kernel.ActorSystem, "tsk.1")
	q.mail()
	a, expires := q.shortlyActivated("quote.1", 8*time.Second)
	arrived := time.Now()
	for _, c := range []trdapi.RecordResponse{
		q.reply("response.timely", "quote.1", 1, a.BusinessHash, trdapi.ClassificationFullUnconditional, arrived, trdapi.ArrivalChannelServer),
		q.reply("response.claimed", "quote.1", 1, a.BusinessHash, trdapi.ClassificationFullUnconditional, arrived, trdapi.ArrivalSourceClaimed),
		q.reply("response.none", "quote.1", 1, a.BusinessHash, trdapi.ClassificationFullUnconditional, time.Time{}, ""),
	} {
		if r, err := q.c.TRD.RecordResponse(q.author, c); err != nil || r.Late || !r.ExpiresAt.Equal(expires) {
			t.Fatalf("%s before expiry %+v %v", c.ResponseID, r, err)
		}
	}
	time.Sleep(time.Until(expires) + 100*time.Millisecond)
	late, err := q.c.TRD.RecordResponse(q.author, q.reply("response.late", "quote.1", 1, a.BusinessHash, trdapi.ClassificationFullUnconditional,
		expires, trdapi.ArrivalChannelServer))
	if err != nil || !late.Late {
		t.Fatalf("a reply that arrived at expires_at %+v %v", late, err)
	}
	for _, id := range []string{"response.late", "response.claimed", "response.none"} {
		_, err := q.c.TRD.VerifyResponse(q.reviewer, q.verification(id, "acceptance."+id, 1, a.BusinessHash))
		if code(err) != trdapi.CodeAcceptanceLate {
			t.Errorf("%s verified after expiry: %s", id, code(err))
		}
	}
	if v, err := q.c.TRD.GetCustomerResponse(q.author, "response.late"); err != nil || !v.Late {
		t.Fatalf("the late reply reads %+v %v", v, err)
	}
	verify := q.verification("response.timely", "acceptance.1", 1, a.BusinessHash)
	v, err := q.c.TRD.VerifyResponse(q.reviewer, verify)
	q.must(err)
	if !v.Formed || !v.ArrivalAt.Equal(kernel.Instant(arrived)) || !v.ExpiresAt.Equal(expires) || v.ArrivalBasis != trdapi.ArrivalChannelServer {
		t.Fatalf("the timely acceptance %+v", v)
	}
	got, err := q.c.TRD.GetAcceptance(q.author, "acceptance.1")
	if err != nil || !got.VerifiedAt.After(expires) {
		t.Fatalf("verified after expiry %+v %v", got, err)
	}
	if f := q.fact("acceptance.1"); f.Check("/x") != nil || !f.Formed {
		t.Fatalf("the lookup after expiry %+v", f)
	}
	if g := q.acceptanceGuard(bindingOf(v)); !g.Usable() {
		t.Fatalf("the guard after expiry %+v", g)
	}
	if view, err := q.c.TRD.GetCustomerResponse(q.author, "response.timely"); err != nil || view.Late {
		t.Fatalf("the timely reply reads %+v %v", view, err)
	}
	if n := q.count("company.A", "brant_trd.acceptance"); n != 1 {
		t.Fatalf("%d acceptances", n)
	}
	replay, err := q.c.TRD.VerifyResponse(q.reviewer, verify)
	if err != nil || !replay.Replayed || replay.ActionID != v.ActionID {
		t.Fatalf("a replay after expiry %+v %v", replay, err)
	}
	_, err = q.c.TRD.BindControl(tsk, trdapi.BindControl{IdempotencyKey: q.key(), QuoteID: "quote.1", TaskID: "task.1"})
	q.must(err)
	_, err = q.c.TRD.VerifyResponse(q.reviewer, verify)
	q.want(err, trdapi.CodeObjectControlConflict)
	claimed, err := trdapi.WithControl(q.reviewer, trdapi.ControlClaim{TaskID: "task.1", ControlEpoch: 1})
	q.must(err)
	if again, err := q.c.TRD.VerifyResponse(claimed, verify); err != nil || !again.Replayed {
		t.Fatalf("the task's replay %+v %v", again, err)
	}
	if _, err = q.c.TRD.GetAcceptance(q.author, "acceptance.1"); err != nil {
		t.Fatalf("a read of a bound root: %v", err)
	}
	if rec, err := q.c.TRD.Reconcile(q.reviewer, verify); err != nil || rec.State != kernel.ReconcileCommitted {
		t.Fatalf("reconcile on a bound root %+v %v", rec, err)
	}
}

// SM-TRD-ACCEPTANCE, TRD-INV-017's negative and UL TRD-AcceptanceEffectivity: a human invalidates a
// valid acceptance at its effectivity version (VERSION_CONFLICT otherwise) with a reason and new
// evidence, or marks it disputed; the ledger keeps every entry; neither moves again
// (INVALID_STATE); the lookups and the guard say ACCEPTANCE_NOT_VALID; AcceptanceInvalidated and
// AcceptanceDisputed are published in the acceptance's order. An invalidated acceptance frees its
// response and message: a new verification creates a new acceptance. A disputed one keeps them,
// and another message of the customer may still accept the revision.
func TestInvalidateAndDisputeAcceptance(t *testing.T) {
	q := newTrdWorld(t)
	q.mail()
	a, v := q.accepted("quote.1")
	_, err := q.c.TRD.InvalidateAcceptance(q.reviewer, q.invalidation("acceptance.quote.1", 2))
	q.want(err, kernel.CodeVersionConflict)
	cmd := q.invalidation("acceptance.quote.1", 1)
	r, err := q.c.TRD.InvalidateAcceptance(q.reviewer, cmd)
	q.must(err)
	if r.Status != trdapi.AcceptanceInvalidated || r.RowVersion != 2 || r.ObjectID != "acceptance.quote.1" || !r.Formed {
		t.Fatalf("invalidated %+v", r)
	}
	if again, err := q.c.TRD.InvalidateAcceptance(q.reviewer, cmd); err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("the same key %+v %v", again, err)
	}
	_, err = q.c.TRD.DisputeAcceptance(q.author, q.dispute("acceptance.quote.1", 2))
	q.want(err, kernel.CodeInvalidState)
	got, err := q.c.TRD.GetAcceptance(q.author, "acceptance.quote.1")
	q.must(err)
	if got.Effectivity != trdapi.AcceptanceInvalidated || got.EffectivityVersion != 2 || len(got.History) != 2 ||
		got.History[1].Reason != cmd.Reason || got.History[1].ChangedBy.ID != "reviewer" || len(got.History[1].Evidence) != 1 ||
		got.History[0].Effectivity != trdapi.AcceptanceValid {
		t.Fatalf("the ledger %+v", got)
	}
	if f := q.fact("acceptance.quote.1"); f.Code != trdapi.CodeAcceptanceNotValid || code(f.Check("/x")) != trdapi.CodeAcceptanceNotValid {
		t.Fatalf("the lookup %+v", f)
	}
	if g := q.acceptanceGuard(bindingOf(v)); g.Usable() || g.Code != trdapi.CodeAcceptanceNotValid || g.EffectivityVersion != 2 {
		t.Fatalf("the guard %+v", g)
	}
	reverified, err := q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.quote.1", "acceptance.quote.1.b", 1, a.BusinessHash))
	if err != nil || reverified.Status != trdapi.AcceptanceValid {
		t.Fatalf("a new verification %+v %v", reverified, err)
	}
	current, err := q.c.TRD.ResolveRevisionAcceptances(q.author, []trdapi.QuoteRevisionRef{{QuoteID: "quote.1", BusinessRevision: 1}})
	if err != nil || current[trdapi.QuoteRevisionRef{QuoteID: "quote.1", BusinessRevision: 1}].AcceptanceID != "acceptance.quote.1.b" {
		t.Fatalf("the revision's current acceptance %+v %v", current, err)
	}

	b, w := q.accepted("quote.2")
	d, err := q.c.TRD.DisputeAcceptance(q.author, q.dispute(w.ObjectID, 1))
	if err != nil || d.Status != trdapi.AcceptanceDisputed || d.RowVersion != 2 {
		t.Fatalf("disputed %+v %v", d, err)
	}
	_, err = q.c.TRD.InvalidateAcceptance(q.reviewer, q.invalidation(w.ObjectID, 2))
	q.want(err, kernel.CodeInvalidState)
	_, err = q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.quote.2", "acceptance.quote.2.b", 1, b.BusinessHash))
	q.want(err, trdapi.CodeResponseVerificationImmutable)
	_, err = q.c.TRD.RecordResponse(q.author, q.full("response.quote.2.po", "quote.2", 1, b.BusinessHash))
	q.must(err)
	if _, err = q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.quote.2.po", "acceptance.quote.2.po", 1, b.BusinessHash)); err != nil {
		t.Fatalf("another message while the first is disputed: %v", err)
	}
	var kinds []string
	for _, e := range q.events("company.A", trdapi.Source) {
		if e.Payload["acceptance_id"] == "acceptance.quote.1" || e.Payload["acceptance_id"] == w.ObjectID {
			kinds = append(kinds, e.Type[len("idp-brant.trd."):]+"@"+e.Payload["acceptance_id"].(string))
		}
	}
	if !slices.Equal(kinds, []string{"acceptance_verified.v1@acceptance.quote.1", "acceptance_invalidated.v1@acceptance.quote.1",
		"acceptance_verified.v1@acceptance.quote.2", "acceptance_disputed.v1@acceptance.quote.2"}) {
		t.Fatalf("events %v", kinds)
	}
}

// Python test_t04_a36 test_task_binding_and_human_required (PYT-0467, the binding half; an AI agent
// records in Brant, TestCustomerResponseRecordAndVerifyLifecycle) and TRD-INV-015: while the quote
// root is bound to a task, recording, verifying and changing an acceptance need the task's claim
// (OBJECT_CONTROL_CONFLICT without it), and a claim of an earlier control epoch is
// CONTROL_EPOCH_STALE; reads are not fenced.
func TestResponseAndAcceptanceWritesPassTheObjectControl(t *testing.T) {
	q := newTrdWorld(t)
	tsk := q.actorOf(kernel.ActorSystem, "tsk.1")
	q.mail()
	a, v := q.accepted("quote.1")
	_, err := q.c.TRD.RecordResponse(q.author, q.full("response.b", "quote.1", 1, a.BusinessHash))
	q.must(err)
	_, err = q.c.TRD.BindControl(tsk, trdapi.BindControl{IdempotencyKey: q.key(), QuoteID: "quote.1", TaskID: "task.1"})
	q.must(err)
	claim := func(ctx context.Context, epoch int64) context.Context {
		c, err := trdapi.WithControl(ctx, trdapi.ControlClaim{TaskID: "task.1", ControlEpoch: epoch})
		q.must(err)
		return c
	}
	record := q.full("response.c", "quote.1", 1, a.BusinessHash)
	_, err = q.c.TRD.RecordResponse(q.author, record)
	q.want(err, trdapi.CodeObjectControlConflict)
	_, err = q.c.TRD.RecordResponse(claim(q.author, 1), record)
	q.must(err)
	verifyB := q.verification("response.b", "acceptance.b", 1, a.BusinessHash)
	_, err = q.c.TRD.VerifyResponse(q.reviewer, verifyB)
	q.want(err, trdapi.CodeObjectControlConflict)
	_, err = q.c.TRD.InvalidateAcceptance(q.reviewer, q.invalidation(v.ObjectID, 1))
	q.want(err, trdapi.CodeObjectControlConflict)
	_, err = q.c.TRD.InvalidateAcceptance(claim(q.reviewer, 1), q.invalidation(v.ObjectID, 1))
	q.must(err)
	_, err = q.c.TRD.VerifyResponse(claim(q.reviewer, 1), verifyB)
	q.must(err)
	_, err = q.c.TRD.StopControl(q.author, trdapi.StopControl{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedControlEpoch: 1, Reason: "人工接管"})
	q.must(err)
	_, err = q.c.TRD.DisputeAcceptance(claim(q.author, 1), q.dispute("acceptance.b", 1))
	q.want(err, trdapi.CodeControlEpochStale)
	_, err = q.c.TRD.DisputeAcceptance(q.author, q.dispute("acceptance.b", 1))
	q.want(err, trdapi.CodeObjectControlConflict)
	if _, err = q.c.TRD.GetCustomerResponse(q.author, "response.c"); err != nil {
		t.Fatalf("a read of a stopping root: %v", err)
	}
}

// Concurrency under the quote root (its FOR UPDATE lock first, then the acceptance's): an
// invalidation and a dispute of one acceptance at one effectivity version have one winner; two
// verifications of two replies for one revision have one winner (one valid acceptance per revision);
// two verifications of one reply into two acceptances have one winner.
func TestConcurrentAcceptanceWritesHaveOneWinner(t *testing.T) {
	q := newTrdWorld(t)
	q.mail()
	_, v := q.accepted("quote.1")
	b := q.activated("quote.2")
	c := q.activated("quote.3")
	for _, r := range []trdapi.RecordResponse{q.full("response.2a", "quote.2", 1, b.BusinessHash), q.full("response.2b", "quote.2", 1, b.BusinessHash),
		q.full("response.3", "quote.3", 1, c.BusinessHash)} {
		_, err := q.c.TRD.RecordResponse(q.author, r)
		q.must(err)
	}
	race := func(fns ...func() error) []string {
		got := make([]string, len(fns))
		var wg sync.WaitGroup
		for i, fn := range fns {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got[i] = code(fn())
			}()
		}
		wg.Wait()
		return slices.Sorted(slices.Values(got))
	}
	// Every command is built (and its key drawn) before the goroutines start.
	invalidate, dispute := q.invalidation(v.ObjectID, 1), q.dispute(v.ObjectID, 1)
	got := race(func() error {
		_, err := q.c.TRD.InvalidateAcceptance(q.reviewer, invalidate)
		return err
	}, func() error {
		_, err := q.c.TRD.DisputeAcceptance(q.author, dispute)
		return err
	})
	if !slices.Equal(got, []string{"OK", kernel.CodeVersionConflict}) {
		t.Fatalf("invalidate and dispute: %v", got)
	}
	if acc, err := q.c.TRD.GetAcceptance(q.author, v.ObjectID); err != nil || len(acc.History) != 2 {
		t.Fatalf("the ledger %+v %v", acc, err)
	}
	verify := func(cmd trdapi.VerifyResponse) func() error {
		return func() error {
			_, err := q.c.TRD.VerifyResponse(q.reviewer, cmd)
			return err
		}
	}
	got = race(verify(q.verification("response.2a", "acceptance.2a", 1, b.BusinessHash)),
		verify(q.verification("response.2b", "acceptance.2b", 1, b.BusinessHash)))
	if !slices.Equal(got, []string{trdapi.CodeAcceptanceAlreadyExists, "OK"}) {
		t.Fatalf("two replies for one revision: %v", got)
	}
	got = race(verify(q.verification("response.3", "acceptance.3a", 1, c.BusinessHash)),
		verify(q.verification("response.3", "acceptance.3b", 1, c.BusinessHash)))
	if !slices.Equal(got, []string{"OK", trdapi.CodeResponseVerificationImmutable}) {
		t.Fatalf("one reply into two acceptances: %v", got)
	}
	if n := q.count("company.A", "brant_trd.acceptance"); n != 3 {
		t.Fatalf("%d acceptances", n)
	}
}

// AcceptanceGuard v1 (ADR-BR-004 option B, ADR-BR-011) decides every commit order with
// acceptance.invalidate: an invalidation committed first makes the downstream step's guard answer
// ACCEPTANCE_NOT_VALID; a downstream transaction holding the guard makes the invalidation wait until
// it commits, having seen the acceptance valid; an invalidation holding its locks makes the guard
// wait and then read the acceptance invalidated. The guard binds the revision it is asked for
// (ACCEPTANCE_SCOPE_MISMATCH, REFERENCE_INVALID for an unknown acceptance).
func TestAcceptanceGuardDecidesCommitOrder(t *testing.T) {
	q := newTrdWorld(t)
	q.mail()
	d := newDownstream(q.world)

	// The invalidation first.
	_, v1 := q.accepted("quote.1")
	_, err := q.c.TRD.InvalidateAcceptance(q.reviewer, q.invalidation(v1.ObjectID, 1))
	q.must(err)
	if g := q.acceptanceGuard(bindingOf(v1)); g.Code != trdapi.CodeAcceptanceNotValid || code(g.Check("/args/acceptance_id")) !=
		trdapi.CodeAcceptanceNotValid {
		t.Fatalf("after the invalidation %+v", g)
	}
	other := bindingOf(v1)
	other.BusinessRevision = 2
	if g := q.acceptanceGuard(other); g.Code != trdapi.CodeAcceptanceScopeMismatch {
		t.Fatalf("another revision %+v", g)
	}
	other.AcceptanceID = "acceptance.unknown"
	if g := q.acceptanceGuard(other); g.Known || code(g.Check("/x")) != kernel.CodeReferenceInvalid {
		t.Fatalf("an unknown acceptance %+v", g)
	}

	// The downstream step holds the guard: the invalidation waits for it.
	_, v2 := q.accepted("quote.2")
	holding, hold := make(chan struct{}), make(chan struct{})
	guarded := make(chan error, 1)
	go func() {
		guarded <- q.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
			b := bindingOf(v2)
			g, err := trdapi.ScanAcceptanceGuard(b, exec.QueryRow(ctx, trdapi.AcceptanceGuardSQL, trdapi.AcceptanceGuardArgs(b)...).Scan)
			if err == nil {
				err = g.Check("/args/acceptance_id")
			}
			close(holding)
			<-hold
			return err
		})
	}()
	<-holding
	invalidated := make(chan error, 1)
	second := q.invalidation(v2.ObjectID, 1)
	go func() {
		_, err := q.c.TRD.InvalidateAcceptance(q.reviewer, second)
		invalidated <- err
	}()
	ok := awaitInHold(t, "the invalidation waits for the guarded root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, invalidated)
	close(hold)
	if !ok {
		return
	}
	if err := <-guarded; err != nil {
		t.Fatalf("the downstream step saw: %v", err)
	}
	if err := <-invalidated; err != nil {
		t.Fatalf("the invalidation after the downstream step: %v", err)
	}

	// The invalidation holds its locks: the guard waits and reads the acceptance invalidated.
	_, v3 := q.accepted("quote.3")
	cmd := q.invalidation(v3.ObjectID, 1)
	blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandInvalidateAcceptance, cmd.IdempotencyKey)
	go func() {
		_, err := q.c.TRD.InvalidateAcceptance(q.reviewer, cmd)
		invalidated <- err
	}()
	if !awaitInHold(t, "the invalidation waits at its action result", func() bool { return d.blocks(blocker) }, invalidated) {
		release()
		return
	}
	verdicts := make(chan trdapi.AcceptanceVerdict, 1)
	go func() { verdicts <- q.acceptanceGuard(bindingOf(v3)) }()
	ok = awaitInHold(t, "the guard waits for the invalidating root", func() bool { return lockWaiter(t, trdapi.AcceptanceGuardFunction) }, verdicts)
	release()
	if !ok {
		return
	}
	if err := <-invalidated; err != nil {
		t.Fatalf("the invalidation: %v", err)
	}
	if g := <-verdicts; g.Code != trdapi.CodeAcceptanceNotValid || g.Effectivity != trdapi.AcceptanceInvalidated {
		t.Fatalf("the guard behind the invalidation %+v", g)
	}
}

// TRD-INV-011, 012 and the effectivity ledger as storage (Python test_t04_a36
// test_conditional_record_is_not_an_acceptance_or_order, PYT-0461, and test_t04_a37
// test_link_is_immutable, PYT-0476, the database halves): the application role cannot change or
// delete a response, an acceptance's binding or its ledger, return an acceptance to valid, change
// its effectivity without a ledger entry, create an acceptance other than valid, from a conditional
// reply, arriving at expires_at or without its ledger, or record a reply to a revision that was
// never formalized. A
// row written past the command path the way verification writes it is taken (the control), so each
// refusal is the rule it names.
func TestDatabaseRefusesIllegalAcceptanceChanges(t *testing.T) {
	q := newTrdWorld(t)
	q.mail()
	_, v := q.accepted("quote.1")
	b, w := q.accepted("quote.2")
	_, err := q.c.TRD.InvalidateAcceptance(q.reviewer, q.invalidation(w.ObjectID, 1))
	q.must(err)
	for _, r := range []trdapi.RecordResponse{
		q.reply("response.c", "quote.2", 1, b.BusinessHash, trdapi.ClassificationConditional, time.Now(), trdapi.ArrivalChannelServer),
		q.full("response.f", "quote.2", 1, b.BusinessHash), q.full("response.g", "quote.2", 1, b.BusinessHash),
		q.reply("response.n", "quote.2", 1, b.BusinessHash, trdapi.ClassificationFullUnconditional, time.Time{}, ""),
	} {
		_, err = q.c.TRD.RecordResponse(q.author, r)
		q.must(err)
	}
	q.approved("quote.p")
	// fromResponse writes acceptance id of response as quote.response.verify would, effective as
	// effectivity, arrived at arrival and verified at verified.
	fromResponse := func(id, response, effectivity, arrival, verified string) string {
		return `INSERT INTO brant_trd.acceptance SELECT p.tenant_id, p.company_id, '` + id + `', p.response_id, p.quote_id, p.revision,
			p.business_hash, p.number, 'customer.1', 2, 'contact.1', 2, '{L1}', p.source_id, p.locator, ` + arrival + `,
			coalesce(p.received_at_basis, 'verification'), p.expires_at, 'human', 'reviewer', 1, ` + verified + `, '` + effectivity + `', 1
			FROM brant_trd.customer_response p WHERE p.response_id = '` + response + `'`
	}
	for name, sql := range map[string]string{
		"a response changed":     `UPDATE brant_trd.customer_response SET summary = 'x'`,
		"a response deleted":     `DELETE FROM brant_trd.customer_response`,
		"an acceptance deleted":  `DELETE FROM brant_trd.acceptance`,
		"a binding changed":      `UPDATE brant_trd.acceptance SET line_ids = '{L9}'`,
		"a ledger entry deleted": `DELETE FROM brant_trd.acceptance_effectivity`,
		"a ledger entry changed": `UPDATE brant_trd.acceptance_effectivity SET reason = 'x'`,
		"back to valid": `UPDATE brant_trd.acceptance SET effectivity = 'valid', effectivity_version = 3 WHERE acceptance_id = '` +
			w.ObjectID + `'`,
		"a change without its ledger entry": `UPDATE brant_trd.acceptance SET effectivity = 'invalidated', effectivity_version = 2
			WHERE acceptance_id = '` + v.ObjectID + `'`,
		"a second change of an invalidated acceptance": `INSERT INTO brant_trd.acceptance_effectivity SELECT tenant_id, company_id, acceptance_id, 2,
			'disputed', changed_by_kind, changed_by_id, change_epoch, changed_at, 'r', '[]' FROM brant_trd.acceptance_effectivity
			WHERE acceptance_id = '` + w.ObjectID + `' AND effectivity_version = 1`,
		"an acceptance of a conditional reply": fromResponse("acceptance.c", "response.c", "valid", "p.received_at", "now()"),
		"an acceptance created invalidated":    fromResponse("acceptance.i", "response.g", "invalidated", "p.received_at", "now()"),
		"an acceptance arriving at expires_at": fromResponse("acceptance.e", "response.n", "valid", "p.expires_at", "p.expires_at"),
		"an acceptance without its ledger":     fromResponse("acceptance.g", "response.g", "valid", "p.received_at", "now()"),
		"a reply to a revision never formalized": `INSERT INTO brant_trd.customer_response SELECT tenant_id, company_id, 'response.p', 'quote.p', 1,
			business_hash, number, expires_at, classification, summary, source_id, 'p', evidence, NULL, NULL, NULL, recorded_by_kind,
			recorded_by_id, record_epoch, recorded_at FROM brant_trd.customer_response WHERE response_id = 'response.c'`,
	} {
		if err := q.exec("company.A", sql); err == nil {
			t.Errorf("%s: the database took it", name)
		}
	}
	if err := q.exec("company.A", fromResponse("acceptance.f", "response.f", "valid", "p.received_at", "now()")+`;
		INSERT INTO brant_trd.acceptance_effectivity SELECT tenant_id, company_id, acceptance_id, 1, 'valid', 'human', 'reviewer', 1, verified_at,
			NULL, NULL FROM brant_trd.acceptance WHERE acceptance_id = 'acceptance.f'`); err != nil {
		t.Fatalf("the control: %v", err)
	}
	if f := q.fact(v.ObjectID); f.Effectivity != trdapi.AcceptanceValid {
		t.Fatalf("after the refusals %+v", f)
	}
}
