//go:build integration

package bootstrap_test

import (
	"context"
	"sync"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// party saves a draft party named after its ID and, unless draft, confirms it on evidence.
func (w *world) party(ctx context.Context, id string, confirm bool, evidence ...string) ptyapi.Receipt {
	w.t.Helper()
	r, err := w.c.PTY.SaveParty(ctx, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: id, Name: id, Roles: []string{ptyapi.RoleCustomer}})
	w.must(err)
	if confirm {
		r, err = w.c.PTY.ConfirmParty(ctx, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: id, ExpectedVersion: 1, EvidenceIDs: evidence})
		w.must(err)
	}
	return r
}

func (w *world) contact(ctx context.Context, id, partyID string, confirm bool, evidence ...string) ptyapi.Receipt {
	w.t.Helper()
	r, err := w.c.PTY.SaveContact(ctx, ptyapi.SaveContact{IdempotencyKey: w.key(), ContactID: id, Name: id, PartyID: partyID})
	w.must(err)
	if confirm {
		r, err = w.c.PTY.ConfirmContact(ctx, ptyapi.ConfirmContact{IdempotencyKey: w.key(), ContactID: id, ExpectedVersion: 1, EvidenceIDs: evidence})
		w.must(err)
	}
	return r
}

// Python test_core.py test_confirmed_master_cannot_be_edited_or_retyped.
func TestConfirmedPartyCannotBeEdited(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.source(ctx, "src.1", "合成询价120件")
	r := w.party(ctx, "c", true, "src.1")
	if r.Status != ptyapi.StatusConfirmed || r.RowVersion != 2 {
		t.Fatalf("confirm %+v", r)
	}
	_, err := w.c.PTY.SaveParty(ctx, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: "c", ExpectedVersion: 2, Name: "changed", Roles: []string{ptyapi.RoleSupplier}})
	w.want(err, kernel.CodeInvalidState)
	v, err := w.c.PTY.GetParty(ctx, "c")
	w.must(err)
	if v.Name != "c" || len(v.Revisions) != 1 || v.Revisions[0].EvidenceIDs[0] != "src.1" || v.Revisions[0].ConfirmedBy.ID != "human.1" {
		t.Fatalf("party %+v", v)
	}
}

// PTY-INV-001 in the database: even the application role cannot edit a confirmed party in place.
func TestDatabaseRefusesEditingAConfirmedParty(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.source(ctx, "src.1", "text")
	w.party(ctx, "c", true, "src.1")
	err := w.exec("company.A", `UPDATE brant_pty.party SET name = 'edited' WHERE party_id = 'c'`)
	if err == nil {
		t.Fatal("the trigger must refuse an in-place edit")
	}
	if err = w.exec("company.A", `UPDATE brant_pty.party_revision SET name = 'edited'`); err == nil {
		t.Fatal("confirmed revisions must be immutable")
	}
}

// Python test_core.py test_ai_cannot_verify_sources_even_with_broad_command_grants.
func TestAICannotConfirmEvenWithTheGrant(t *testing.T) {
	w := newWorld(t)
	human := w.actor("human.1", allPermissions...)
	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "ai.1"}, Permissions: allPermissions})
	w.source(human, "src.1", "text")
	_, err := w.c.PTY.SaveParty(ai, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: "c", Name: "c", Roles: []string{ptyapi.RoleCustomer}})
	w.must(err) // an agent may prepare the draft
	_, err = w.c.PTY.ConfirmParty(ai, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "c", ExpectedVersion: 1, EvidenceIDs: []string{"src.1"}})
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.c.PTY.DeactivateParty(ai, ptyapi.DeactivateParty{IdempotencyKey: w.key(), PartyID: "c", ExpectedVersion: 1, Reason: "x"})
	w.want(err, kernel.CodePermissionDenied)
}

// Python test_core.py test_confirmation_requires_source_read_permission.
func TestConfirmationRequiresSourceReadPermission(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.source(ctx, "src.1", "text")
	w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "human.1"},
		Permissions: allPermissions, DataScope: "company"})
	w.party(ctx, "c", false)
	limited := w.actor("limited", "party.confirm")
	_, err := w.c.PTY.ConfirmParty(limited, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "c", ExpectedVersion: 1, EvidenceIDs: []string{"src.1"}})
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.c.PTY.ConfirmParty(ctx, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "c", ExpectedVersion: 1, EvidenceIDs: []string{"src.1", "missing"}})
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence_ids/1" {
		t.Fatalf("unreadable evidence: %v at %s", err, path(err))
	}
}

// Python test_foundation.py test_master_revision_preserves_old_confirmed_record.
func TestRevisionPreservesTheConfirmedRecord(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("editor", allPermissions...)
	w.source(ctx, "evidence", "text")
	w.party(ctx, "customer", true, "evidence")
	r, err := w.c.PTY.ReviseParty(ctx, ptyapi.ReviseParty{IdempotencyKey: w.key(), PartyID: "customer", ExpectedVersion: 2, Reason: "受控修订"})
	w.must(err)
	if r.Status != ptyapi.StatusDraft || r.RowVersion != 3 {
		t.Fatalf("revise %+v", r)
	}
	_, err = w.c.PTY.SaveParty(ctx, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: "customer", ExpectedVersion: 3, Name: "new name", Roles: []string{ptyapi.RoleCustomer}})
	w.must(err)
	v, err := w.c.PTY.GetParty(ctx, "customer")
	w.must(err)
	if v.Name != "new name" || v.Status != ptyapi.StatusDraft || len(v.Revisions) != 1 || v.Revisions[0].Name != "customer" {
		t.Fatalf("party after revision %+v", v)
	}
	refs, err := w.c.PTY.ResolveParties(ctx, ptyapi.Resolve{PartyIDs: []string{"customer"}})
	w.must(err)
	if refs.Parties["customer"].Status != ptyapi.StatusDraft {
		t.Fatalf("a revised party is not current until confirmed again: %+v", refs.Parties)
	}
	audit, err := w.c.PTY.Audit(ctx)
	w.must(err)
	var reason string
	for _, e := range audit {
		if e.Command == ptyapi.CommandReviseParty {
			reason = e.Reason
		}
	}
	if reason != "受控修订" {
		t.Fatalf("revision reason %q", reason)
	}
}

func TestContactBelongsToACustomerAndConfirmsOnlyUnderAConfirmedOne(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.source(ctx, "src.1", "text")
	_, err := w.c.PTY.SaveParty(ctx, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: "supplier.1", Name: "s", Roles: []string{ptyapi.RoleSupplier}})
	w.must(err)
	_, err = w.c.PTY.SaveContact(ctx, ptyapi.SaveContact{IdempotencyKey: w.key(), ContactID: "x", Name: "x", PartyID: "supplier.1"})
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/party_id" {
		t.Fatalf("contact of a supplier: %v", err)
	}
	w.party(ctx, "customer.1", false)
	w.contact(ctx, "contact.1", "customer.1", false)
	_, err = w.c.PTY.ConfirmContact(ctx, ptyapi.ConfirmContact{IdempotencyKey: w.key(), ContactID: "contact.1", ExpectedVersion: 1, EvidenceIDs: []string{"src.1"}})
	w.want(err, kernel.CodeReferenceInvalid) // the customer is still a draft
	_, err = w.c.PTY.ConfirmParty(ctx, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: 1, EvidenceIDs: []string{"src.1"}})
	w.must(err)
	r, err := w.c.PTY.ConfirmContact(ctx, ptyapi.ConfirmContact{IdempotencyKey: w.key(), ContactID: "contact.1", ExpectedVersion: 1, EvidenceIDs: []string{"src.1"}})
	w.must(err)
	if r.Status != ptyapi.StatusConfirmed || r.RowVersion != 2 {
		t.Fatalf("contact %+v", r)
	}
	w.contact(ctx, "contact.2", "", false)
	_, err = w.c.PTY.ConfirmContact(ctx, ptyapi.ConfirmContact{IdempotencyKey: w.key(), ContactID: "contact.2", ExpectedVersion: 1, EvidenceIDs: []string{"src.1"}})
	if code(err) != kernel.CodeDraftNotReady || path(err) != "/args/party_id" {
		t.Fatalf("contact without a party: %v", err)
	}
}

// PTY-INV-001: concurrent revisions have one winner.
func TestConcurrentRevisionsHaveOneWinner(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.source(ctx, "src.1", "text")
	w.party(ctx, "c", true, "src.1")
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = w.c.PTY.ReviseParty(ctx, ptyapi.ReviseParty{IdempotencyKey: "revise." + string(rune('a'+i)), PartyID: "c", ExpectedVersion: 2, Reason: "r"})
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("want one winner: %v", errs)
	}
	for _, err := range errs {
		if err != nil && code(err) != kernel.CodeVersionConflict {
			t.Fatalf("loser: %v", err)
		}
	}
}

func TestConfirmationPublishesAnEventWithIdentifiersOnly(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.source(ctx, "src.1", "text")
	w.party(ctx, "c", true, "src.1")
	events := w.events("company.A", "idp-brant.pty")
	if len(events) != 1 || events[0].Type != ptyapi.EventPartyConfirmed {
		t.Fatalf("events %+v", events)
	}
	p := events[0].Payload
	if p["party_id"] != "c" || p["row_version"] != float64(2) || p["actor_id"] != "human.1" || p["name"] != nil {
		t.Fatalf("payload %v", p)
	}
}
