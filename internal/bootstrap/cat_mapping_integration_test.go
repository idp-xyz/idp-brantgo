//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The item code mapping suite ports the code_mapping and sku_profile vectors of the Python V0.50
// foundation package (tests/test_foundation.py). Editor and reviewer are polActors: humans with
// every permission and company data scope, who read the source "evidence".

var supplierCode = catapi.ItemCode{PartyID: "supplier", PartyRole: catapi.RoleSupplier, ExternalCode: "SUP-01"}

func (w *world) saveMapping(ctx context.Context, id string, expected int64, c catapi.ItemCode, skuID string, rev int64) (catapi.Receipt, error) {
	return w.c.CAT.SaveItemCodeMapping(ctx, catapi.SaveItemCodeMapping{IdempotencyKey: w.key(), MappingID: id, ExpectedVersion: expected,
		PartyID: c.PartyID, PartyRole: c.PartyRole, ExternalCode: c.ExternalCode, SkuID: skuID, SpecRevision: rev})
}

func publishMapping(key, id string, expected, revision int64) catapi.PublishItemCodeMapping {
	return catapi.PublishItemCodeMapping{IdempotencyKey: key, MappingID: id, ExpectedVersion: expected, Revision: revision,
		EvidenceIDs: []string{"evidence"}, Reason: "核实合成货号映射"}
}

func (w *world) publishMapping(ctx context.Context, id string, expected, revision int64) (catapi.Receipt, error) {
	return w.c.CAT.PublishItemCodeMapping(ctx, publishMapping(w.key(), id, expected, revision))
}

func (w *world) resolveCode(ctx context.Context, c catapi.ItemCode) catapi.ResolvedItemCode {
	w.t.Helper()
	got, err := w.c.CAT.ResolveItemCodes(ctx, []catapi.ItemCode{c})
	w.must(err)
	return got[c]
}

func (w *world) standing(ctx context.Context, skuID string, rev int64) catapi.SpecStanding {
	w.t.Helper()
	ref := catapi.SpecRef{SkuID: skuID, SpecRevision: rev}
	got, err := w.c.CAT.ResolveSpecs(ctx, []catapi.SpecRef{ref})
	w.must(err)
	if len(got) != 1 {
		w.t.Fatalf("every reference is answered: %+v", got)
	}
	return got[ref]
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code + " " + pgErr.ConstraintName
	}
	return code(err)
}

// Python test_foundation.py test_supplier_contacts_addresses_and_external_sku_mapping, the
// code_mapping half: a published mapping returns its SKU; a second mapping of the same party, role
// and external code is saved but cannot be published (BUSINESS_KEY_CONFLICT), and the database
// refuses a second active revision of the key even when the command is bypassed. A code without a
// published mapping is unmapped, never matched to a similar one.
func TestSupplierItemCodeMappingResolvesAndIsPublishedOnce(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	w.sku(editor, "sku", true, "evidence")
	saved, err := w.saveMapping(editor, "mapping1", 0, supplierCode, "sku", 1)
	w.must(err)
	if saved.ObjectID != "mapping1" || saved.RowVersion != 1 || saved.Status != catapi.StatusDraft {
		t.Fatalf("saved %+v", saved)
	}
	published, err := w.publishMapping(reviewer, "mapping1", 1, 1)
	w.must(err)
	if published.ObjectID != "mapping1" || published.RowVersion != 2 || published.Status != catapi.StatusActive || published.Replayed {
		t.Fatalf("published %+v", published)
	}
	v, err := w.c.CAT.GetItemCodeMapping(editor, "mapping1")
	w.must(err)
	if v.Code != supplierCode || v.RootVersion != 2 || v.ActiveRevision != 1 || v.DraftRevision != 0 || len(v.Revisions) != 1 {
		t.Fatalf("mapping %+v", v)
	}
	if r := v.Revisions[0]; r.Sku != (catapi.SpecRef{SkuID: "sku", SpecRevision: 1}) || r.Status != catapi.StatusActive ||
		r.SavedBy.ID != "editor" || r.PublishedBy != actorRef("human", "reviewer") || !slices.Equal(r.EvidenceIDs, []string{"evidence"}) ||
		r.PublishedAt.IsZero() || !r.SupersededAt.IsZero() {
		t.Fatalf("revision %+v", r)
	}

	similar := catapi.ItemCode{PartyID: "supplier", PartyRole: catapi.RoleSupplier, ExternalCode: "SUP-1"}
	asCustomer := catapi.ItemCode{PartyID: "supplier", PartyRole: catapi.RoleCustomer, ExternalCode: "SUP-01"}
	resolved, err := w.c.CAT.ResolveItemCodes(editor, []catapi.ItemCode{supplierCode, similar, asCustomer, supplierCode})
	w.must(err)
	if len(resolved) != 3 {
		t.Fatalf("every code is answered once: %+v", resolved)
	}
	if r := resolved[supplierCode]; !r.Mapped() || r.Mapping != (catapi.ItemCodeMappingRef{MappingID: "mapping1", Revision: 1}) ||
		r.Sku != (catapi.SpecRef{SkuID: "sku", SpecRevision: 1}) || r.Code != supplierCode {
		t.Fatalf("SUP-01: %+v", r)
	}
	for _, c := range []catapi.ItemCode{similar, asCustomer} {
		if r := resolved[c]; r.Status != catapi.ItemCodeUnmapped || r.Mapped() || r.Sku != (catapi.SpecRef{}) || r.Mapping != (catapi.ItemCodeMappingRef{}) {
			t.Fatalf("%+v must stay unmapped for manual verification: %+v", c, r)
		}
	}

	_, err = w.saveMapping(editor, "mapping2", 0, supplierCode, "sku", 1)
	w.must(err)
	_, err = w.publishMapping(reviewer, "mapping2", 1, 1)
	if code(err) != catapi.CodeBusinessKeyConflict || path(err) != "/args/id" {
		t.Fatalf("a second mapping of SUP-01: %v", err)
	}
	if rej, _ := kernel.AsRejection(err); rej.Category != kernel.CategoryConflict {
		t.Fatalf("BUSINESS_KEY_CONFLICT is a conflict: %+v", rej)
	}
	audit, err := w.c.CAT.Audit(editor)
	w.must(err)
	if !slices.ContainsFunc(audit, func(e kernel.AuditEntry) bool {
		return e.Command == catapi.CommandPublishItemCodeMapping && e.ObjectID == "mapping2" && e.Outcome == "rejected" && e.Code == catapi.CodeBusinessKeyConflict
	}) {
		t.Fatalf("the refusal is audited: %+v", audit)
	}
	if r := w.resolveCode(reviewer, supplierCode); r.Mapping.MappingID != "mapping1" {
		t.Fatalf("SUP-01 stays mapped by mapping1: %+v", r)
	}

	// The unique index holds the rule below the command: activating mapping2's draft directly fails.
	err = w.exec("company.A", `UPDATE brant_cat.item_code_mapping_revision SET status = 'active', published_by_kind = 'human',
		published_by_id = 'reviewer', publish_epoch = 1, published_at = now(), evidence_ids = ARRAY['evidence'] WHERE mapping_id = 'mapping2'`)
	if got := sqlState(err); got != "23505 item_code_mapping_published_key" {
		t.Fatalf("a second active revision of SUP-01: %s", got)
	}

	var events []event
	for _, e := range w.events("company.A", catapi.Source) {
		if e.Type == catapi.EventItemCodeMappingPublished {
			events = append(events, e)
		}
	}
	if len(events) != 1 {
		t.Fatalf("events %+v", events)
	}
	p := events[0].Payload
	if p["mapping_id"] != "mapping1" || p["revision"] != float64(1) || p["root_version"] != float64(2) || p["party_id"] != "supplier" ||
		p["party_role"] != "supplier" || p["sku_id"] != "sku" || p["spec_revision"] != float64(1) || p["aggregate_type"] != "ItemCodeMapping" {
		t.Fatalf("payload %+v", p)
	}
	if _, ok := p["external_code"]; ok {
		t.Fatal("events carry identifiers and versions, not the external code")
	}
	if _, ok := p["superseded_revision"]; ok {
		t.Fatal("nothing was superseded")
	}
}

// Python test_foundation.py test_master_revision_invalidates_old_sku_profile_for_new_conversion, the
// SKU half: after the SKU is revised and confirmed again, its old specification revision is no
// longer current for new use. Python answers REFERENCE_INVALID (its sku_profile carries the SKU's
// row version); CAT answers REFERENCE_STALE, the error catalog's code for a reference whose version
// no longer applies, and keeps REFERENCE_INVALID for unknown, unconfirmed and inactive SKUs
// (CAT-INV-001). A mapping is saved and published only on a current revision.
func TestRevisedSkuSpecificationIsStaleForNewUse(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	w.sku(editor, "sku", true, "evidence")
	w.sku(editor, "sku.draft", false)
	if s := w.standing(editor, "sku", 1); !s.Current || s.Code != "" || s.Status != catapi.StatusConfirmed || s.CurrentRevision != 1 {
		t.Fatalf("r1 confirmed: %+v", s)
	}
	_, err := w.saveMapping(editor, "mapping1", 0, supplierCode, "sku", 1)
	w.must(err)

	_, err = w.c.CAT.ReviseSpec(editor, catapi.ReviseSpec{IdempotencyKey: w.key(), SkuID: "sku", ExpectedVersion: 2, Reason: "new technical review"})
	w.must(err)
	if s := w.standing(editor, "sku", 1); s.Current || s.Code != catapi.CodeReferenceStale || s.Status != catapi.StatusDraft || s.CurrentRevision != 0 {
		t.Fatalf("r1 under revision: %+v", s)
	}
	_, err = w.publishMapping(reviewer, "mapping1", 1, 1)
	if code(err) != catapi.CodeReferenceStale || path(err) != "/args/revision" {
		t.Fatalf("publishing on r1 under revision: %v", err)
	}
	_, err = w.c.CAT.ConfirmSku(editor, catapi.ConfirmSku{IdempotencyKey: w.key(), SkuID: "sku", ExpectedVersion: 3, EvidenceIDs: []string{"evidence"}})
	w.must(err)

	refs := []catapi.SpecRef{{SkuID: "sku", SpecRevision: 1}, {SkuID: "sku", SpecRevision: 2}, {SkuID: "sku", SpecRevision: 3},
		{SkuID: "sku.draft", SpecRevision: 1}, {SkuID: "sku.unknown", SpecRevision: 1}}
	got, err := w.c.CAT.ResolveSpecs(editor, refs)
	w.must(err)
	want := []string{catapi.CodeReferenceStale, "", kernel.CodeReferenceInvalid, kernel.CodeReferenceInvalid, kernel.CodeReferenceInvalid}
	for i, r := range refs {
		s := got[r]
		if s.Ref != r || s.Code != want[i] || s.Current != (want[i] == "") {
			t.Errorf("%+v: %+v, want %q", r, s, want[i])
		}
	}
	if s := got[refs[0]]; s.CurrentRevision != 2 || s.Status != catapi.StatusConfirmed || s.Check("/lines/0") == nil {
		t.Fatalf("r1 after r2: %+v", s)
	}
	if code(got[refs[0]].Check("/lines/0")) != catapi.CodeReferenceStale || code(got[refs[4]].Check("/lines/0")) != kernel.CodeReferenceInvalid {
		t.Fatal("Check passes CAT's verdict on")
	}

	_, err = w.saveMapping(editor, "mapping1", 1, supplierCode, "sku", 1)
	if code(err) != catapi.CodeReferenceStale || path(err) != "/args/spec_revision" {
		t.Fatalf("saving on r1 after r2: %v", err)
	}
	_, err = w.saveMapping(editor, "mapping.draft", 0, supplierCode, "sku.draft", 1)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/spec_revision" {
		t.Fatalf("an unconfirmed SKU is never mapped: %v", err)
	}
	_, err = w.saveMapping(editor, "mapping.unknown", 0, supplierCode, "sku.unknown", 1)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/sku_id" {
		t.Fatalf("an unknown SKU is never created or mapped: %v", err)
	}
	_, err = w.saveMapping(editor, "mapping1", 1, supplierCode, "sku", 2)
	w.must(err)
	_, err = w.publishMapping(reviewer, "mapping1", 2, 1)
	w.must(err)
	if r := w.resolveCode(editor, supplierCode); r.Sku != (catapi.SpecRef{SkuID: "sku", SpecRevision: 2}) {
		t.Fatalf("mapped to r2: %+v", r)
	}

	_, err = w.c.CAT.DeactivateSku(reviewer, catapi.DeactivateSku{IdempotencyKey: w.key(), SkuID: "sku", ExpectedVersion: 4, Reason: "停用"})
	w.must(err)
	r := w.resolveCode(editor, supplierCode)
	if !r.Mapped() || r.Sku.SpecRevision != 2 {
		t.Fatalf("the published mapping stays as published: %+v", r)
	}
	if s := w.standing(editor, r.Sku.SkuID, r.Sku.SpecRevision); s.Current || s.Code != kernel.CodeReferenceInvalid || s.Status != catapi.StatusInactive {
		t.Fatalf("an inactive SKU is not for new use: %+v", s)
	}
}

// item_code_mapping.save_draft is open to AI agents with the grant; item_code_mapping.publish is
// human only (05 stage humanOnlyCommands). Publishing a mapping's next revision supersedes the
// active one; published revisions never change; the key is fixed; mappings stay in their company.
func TestItemCodeMappingRevisionsAndPublication(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "agent.1"), Permissions: allPermissions, DataScope: "company"})
	w.sku(editor, "sku", true, "evidence")
	w.sku(editor, "sku.2", true, "evidence")
	customerCode := catapi.ItemCode{PartyID: "customer.1", PartyRole: catapi.RoleCustomer, ExternalCode: "客户货号 ST01-红-S"}

	key := w.key()
	cmd := catapi.SaveItemCodeMapping{IdempotencyKey: key, MappingID: "m.customer", PartyID: customerCode.PartyID, PartyRole: customerCode.PartyRole,
		ExternalCode: customerCode.ExternalCode, SkuID: "sku", SpecRevision: 1}
	first, err := w.c.CAT.SaveItemCodeMapping(ai, cmd)
	w.must(err)
	replay, err := w.c.CAT.SaveItemCodeMapping(ai, cmd)
	w.must(err)
	if !replay.Replayed || replay.ActionID != first.ActionID || replay.RowVersion != 1 {
		t.Fatalf("replay %+v of %+v", replay, first)
	}
	_, err = w.c.CAT.PublishItemCodeMapping(ai, publishMapping(w.key(), "m.customer", 1, 1))
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.saveMapping(editor, "m.customer", 0, customerCode, "sku", 1)
	w.want(err, kernel.CodeVersionConflict)
	moved := customerCode
	moved.ExternalCode = "客户货号 ST01-红-M"
	_, err = w.saveMapping(editor, "m.customer", 1, moved, "sku", 1)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/external_code" {
		t.Fatalf("the key is fixed: %v", err)
	}
	_, err = w.publishMapping(reviewer, "m.customer", 1, 2)
	if code(err) != kernel.CodeNotFound || path(err) != "/args/revision" {
		t.Fatalf("a revision never assigned: %v", err)
	}
	_, err = w.c.CAT.PublishItemCodeMapping(reviewer, catapi.PublishItemCodeMapping{IdempotencyKey: w.key(), MappingID: "m.customer",
		ExpectedVersion: 1, Revision: 1, EvidenceIDs: []string{"evidence.unknown"}, Reason: "x"})
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence_ids/0" {
		t.Fatalf("unreadable evidence: %v", err)
	}
	_, err = w.publishMapping(reviewer, "m.customer", 1, 1)
	w.must(err)
	_, err = w.publishMapping(reviewer, "m.customer", 2, 1)
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/revision" {
		t.Fatalf("an active revision is not published again: %v", err)
	}

	second, err := w.saveMapping(ai, "m.customer", 2, customerCode, "sku.2", 1)
	w.must(err)
	if second.RowVersion != 3 || second.Status != catapi.StatusDraft {
		t.Fatalf("a new draft revision: %+v", second)
	}
	if r := w.resolveCode(ai, customerCode); r.Mapping.Revision != 1 || r.Sku.SkuID != "sku" {
		t.Fatalf("a draft does not change the resolution: %+v", r)
	}
	_, err = w.publishMapping(editor, "m.customer", 3, 2)
	w.must(err)
	v, err := w.c.CAT.GetItemCodeMapping(ai, "m.customer")
	w.must(err)
	if v.ActiveRevision != 2 || v.DraftRevision != 0 || v.RootVersion != 4 || len(v.Revisions) != 2 ||
		v.Revisions[0].Status != catapi.StatusSuperseded || v.Revisions[0].SupersededAt.IsZero() || v.Revisions[0].PublishedBy.ID != "reviewer" ||
		v.Revisions[1].Status != catapi.StatusActive || v.Revisions[1].SavedBy != actorRef("ai", "agent.1") || v.Revisions[1].PublishedBy.ID != "editor" {
		t.Fatalf("mapping %+v", v)
	}
	if r := w.resolveCode(ai, customerCode); r.Mapping != (catapi.ItemCodeMappingRef{MappingID: "m.customer", Revision: 2}) ||
		r.Sku != (catapi.SpecRef{SkuID: "sku.2", SpecRevision: 1}) {
		t.Fatalf("resolution after supersession: %+v", r)
	}
	var last event
	for _, e := range w.events("company.A", catapi.Source) {
		if e.Type == catapi.EventItemCodeMappingPublished {
			last = e
		}
	}
	if last.Payload["revision"] != float64(2) || last.Payload["superseded_revision"] != float64(1) {
		t.Fatalf("event %+v", last)
	}

	for _, c := range []struct{ sql, want string }{
		// The trigger: a published revision is not edited, and an active one is only superseded.
		{`UPDATE brant_cat.item_code_mapping_revision SET sku_id = 'sku.2' WHERE mapping_id = 'm.customer' AND revision = 1`, "23000 "},
		{`UPDATE brant_cat.item_code_mapping_revision SET status = 'draft', published_by_kind = NULL, published_by_id = NULL,
			publish_epoch = NULL, published_at = NULL, evidence_ids = NULL WHERE mapping_id = 'm.customer' AND revision = 2`, "23000 "},
		{`UPDATE brant_cat.item_code_mapping SET root_version = 1 WHERE mapping_id = 'm.customer'`, "23000 "},
		// The grants: brant_app updates no key column and deletes nothing.
		{`UPDATE brant_cat.item_code_mapping SET external_code = 'other' WHERE mapping_id = 'm.customer'`, "42501 "},
		{`DELETE FROM brant_cat.item_code_mapping_revision WHERE mapping_id = 'm.customer'`, "42501 "},
	} {
		if got := sqlState(w.exec("company.A", c.sql)); got != c.want {
			t.Fatalf("%s: %s, want %s", c.sql, got, c.want)
		}
	}

	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: actorRef("human", "editor"), Permissions: allPermissions, DataScope: "company"})
	_, err = w.c.CAT.GetItemCodeMapping(other, "m.customer")
	w.want(err, kernel.CodeNotFound)
	if r := w.resolveCode(other, customerCode); r.Mapped() {
		t.Fatalf("another company's mapping: %+v", r)
	}
	if n := w.count("company.B", "brant_cat.item_code_mapping_revision"); n != 0 {
		t.Fatalf("FORCE RLS shows company.B %d revisions of company.A", n)
	}
	if n := w.count("company.A", "brant_cat.item_code_mapping_revision"); n != 2 {
		t.Fatalf("company.A has %d revisions", n)
	}
}

// Two mappings of one item code published at the same time: exactly one wins, the other is
// BUSINESS_KEY_CONFLICT whether it saw the winner's commit or collided with it on the unique index
// (the ledger retries it once and it then sees the winner); one revision of the key is active.
func TestConcurrentPublicationOfOneItemCodeHasOneWinner(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	w.sku(editor, "sku", true, "evidence")
	for round := range 8 {
		c := catapi.ItemCode{PartyID: "supplier", PartyRole: catapi.RoleSupplier, ExternalCode: "SUP-" + strconv.Itoa(round)}
		ids := []string{"race." + strconv.Itoa(round) + ".a", "race." + strconv.Itoa(round) + ".b"}
		cmds := make([]catapi.PublishItemCodeMapping, len(ids))
		for i, id := range ids {
			_, err := w.saveMapping(editor, id, 0, c, "sku", 1)
			w.must(err)
			cmds[i] = publishMapping(w.key(), id, 1, 1)
		}
		receipts, errs := make([]catapi.Receipt, len(ids)), make([]error, len(ids))
		var wg sync.WaitGroup
		for i := range cmds {
			wg.Add(1)
			go func() {
				defer wg.Done()
				receipts[i], errs[i] = w.c.CAT.PublishItemCodeMapping(reviewer, cmds[i])
			}()
		}
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("round %d: want one winner: %v", round, errs)
		}
		winner := 0
		if errs[0] != nil {
			winner = 1
		}
		if code(errs[1-winner]) != catapi.CodeBusinessKeyConflict || receipts[winner].Status != catapi.StatusActive {
			t.Fatalf("round %d: winner %+v, loser %v", round, receipts[winner], errs[1-winner])
		}
		if r := w.resolveCode(editor, c); r.Mapping != (catapi.ItemCodeMappingRef{MappingID: ids[winner], Revision: 1}) {
			t.Fatalf("round %d: %+v", round, r)
		}
		loser, err := w.c.CAT.GetItemCodeMapping(editor, ids[1-winner])
		w.must(err)
		if loser.ActiveRevision != 0 || loser.DraftRevision != 1 || loser.RootVersion != 1 {
			t.Fatalf("round %d: the loser keeps its draft: %+v", round, loser)
		}
	}
	if n := w.count("company.A", "brant_cat.item_code_mapping_revision WHERE status = 'active'"); n != 8 {
		t.Fatalf("%d active revisions for 8 codes", n)
	}
}

// The unique index decides the race the command's own check cannot see: another transaction has
// activated the code and not committed yet. The publication passes its check, waits on the index,
// fails with a unique violation when the other commits, and the ledger's retry then refuses it
// BUSINESS_KEY_CONFLICT; its draft stays a draft.
func TestPublicationWaitingOnThePublishedKeyIsBusinessKeyConflict(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	w.sku(editor, "sku", true, "evidence")
	for _, id := range []string{"held", "waiting"} {
		_, err := w.saveMapping(editor, id, 0, supplierCode, "sku", 1)
		w.must(err)
	}
	ctx := context.Background()
	pool := pgtest.AppPool(t)
	tx, err := pool.Begin(ctx)
	w.must(err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`, w.tenant)
	w.must(err)
	var pid int32
	w.must(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	_, err = tx.Exec(ctx, `UPDATE brant_cat.item_code_mapping_revision SET status = 'active', published_by_kind = 'human',
		published_by_id = 'reviewer', publish_epoch = 1, published_at = now(), evidence_ids = ARRAY['evidence'] WHERE mapping_id = 'held'`)
	w.must(err)

	cmd := publishMapping(w.key(), "waiting", 1, 1)
	done := make(chan error, 1)
	go func() {
		_, err := w.c.CAT.PublishItemCodeMapping(reviewer, cmd)
		done <- err
	}()
	waitBlockedBy(t, pool, pid, done)
	w.must(tx.Commit(ctx))
	err = <-done
	if code(err) != catapi.CodeBusinessKeyConflict || path(err) != "/args/id" {
		t.Fatalf("publication after the key was taken: %v", err)
	}
	if r := w.resolveCode(editor, supplierCode); r.Mapping.MappingID != "held" {
		t.Fatalf("SUP-01: %+v", r)
	}
	v, err := w.c.CAT.GetItemCodeMapping(editor, "waiting")
	w.must(err)
	if v.ActiveRevision != 0 || v.DraftRevision != 1 || v.RootVersion != 1 {
		t.Fatalf("the refused mapping keeps its draft: %+v", v)
	}
}
