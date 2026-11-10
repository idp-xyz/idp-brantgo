//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S1-02: address roles and service roles of parties, and company trading profiles. The editor
// and the reviewer are the Python FoundationTests actors (polActors): humans with every permission
// and company data scope, who both read the source "evidence".

// staffParty saves a party with roles and addresses as ctx and confirms it on "evidence".
func (w *world) staffParty(ctx context.Context, id string, roles []string, addresses ...ptyapi.Address) ptyapi.Receipt {
	w.t.Helper()
	_, err := w.c.PTY.SaveParty(ctx, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: id, Name: id, Roles: roles, Addresses: addresses})
	w.must(err)
	r, err := w.c.PTY.ConfirmParty(ctx, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: id, ExpectedVersion: 1, EvidenceIDs: []string{"evidence"}})
	w.must(err)
	return r
}

// settlement publishes the payment terms and trade terms a trading profile binds (Python
// FoundationTests.trading) and returns them as PTY carries them.
func (w *world) settlement(editor, reviewer context.Context) (payment, trade ptyapi.TermsRef) {
	w.t.Helper()
	win := newWindow()
	p := w.configure(editor, reviewer, polapi.KindPaymentTerms, "payment", 0, win.from, time.Time{}, polapi.ConfigData{PaymentTerms: &polapi.PaymentTerms{
		Name: "100 before shipment", Stages: []polapi.PaymentStage{{StageID: "all", Percent: "100", Trigger: polapi.TriggerBeforeShipment}}}}, true)
	t := w.configure(editor, reviewer, polapi.KindTradeTerms, "FCA", 0, win.from, time.Time{}, polapi.ConfigData{TradeTerms: &polapi.TradeTerms{
		Incoterm: "FCA", Edition: polapi.IncotermsEdition2020, NamedPlace: "TEST NAMED PLACE", ResponsibilityNote: "synthetic evidence only",
		Delivery: polapi.Delivery{Kind: polapi.DeliveryFixedDate, Date: "2027-01-31"}}}, true)
	return terms(p), terms(t)
}

func terms(r polapi.ConfigRef) ptyapi.TermsRef {
	return ptyapi.TermsRef{Kind: r.Kind, ConfigID: r.ConfigID, Revision: r.Revision}
}

func (w *world) saveProfile(ctx context.Context, partyID string, expected, partyRevision int64, payment, trade ptyapi.TermsRef,
	contacts ...ptyapi.PinnedContact) (ptyapi.Receipt, error) {
	return w.c.PTY.SaveTradingProfile(ctx, ptyapi.SaveTradingProfile{IdempotencyKey: w.key(), PartyID: partyID, ExpectedVersion: expected,
		PartyRevision: partyRevision, PaymentTerms: payment, TradeTerms: trade, Owner: ptyapi.OwnerRef{ActorID: "sales.owner", TeamID: "trade"},
		ContactRefs: contacts})
}

func (w *world) publishProfile(ctx context.Context, partyID string, expected, revision int64) (ptyapi.Receipt, error) {
	return w.c.PTY.PublishTradingProfile(ctx, ptyapi.PublishTradingProfile{IdempotencyKey: w.key(), PartyID: partyID, ExpectedVersion: expected,
		Revision: revision, EvidenceIDs: []string{"evidence"}, Reason: "核实合成交易档案"})
}

// AddressRole (PTY-AGG-Party, PTY-INV-001): addresses are part of the party's draft, a confirmation
// keeps them with its revision for good, and AddressRoleRef v1 resolves an address of a confirmed
// revision while that revision is the party's current one.
func TestAddressRolesAreKeptPerConfirmedRevision(t *testing.T) {
	w := newWorld(t)
	editor, _ := w.polActors()
	buyer := ptyapi.Address{AddressID: "hq", Role: ptyapi.AddressBuyer, Address: "合成买方地址 1"}
	billing := ptyapi.Address{AddressID: "ap", Role: ptyapi.AddressBilling, Address: "合成账单地址"}
	r := w.staffParty(editor, "customer", []string{ptyapi.RoleCustomer}, buyer, billing)
	if r.RowVersion != 2 || r.Status != ptyapi.StatusConfirmed {
		t.Fatalf("confirm %+v", r)
	}
	v, err := w.c.PTY.GetParty(editor, "customer")
	w.must(err)
	if v.Revision != 1 || len(v.Addresses) != 2 || v.Addresses[0] != billing || !slices.Equal(v.Revisions[0].Addresses, []ptyapi.Address{billing, buyer}) {
		t.Fatalf("party %+v", v)
	}
	ref1 := ptyapi.AddressRoleRef{PartyID: "customer", PartyRevision: 1, AddressID: "hq"}
	got, err := w.c.PTY.ResolveAddressRoles(editor, []ptyapi.AddressRoleRef{ref1, {PartyID: "customer", PartyRevision: 1, AddressID: "nowhere"}})
	w.must(err)
	if a := got[ref1]; len(got) != 1 || a.Role != ptyapi.AddressBuyer || a.Address != buyer.Address || a.Code != "" || a.Check("/args/address") != nil {
		t.Fatalf("resolved %+v", got)
	}

	// A confirmed party's addresses are revised, not edited.
	_, err = w.c.PTY.SaveParty(editor, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: "customer", ExpectedVersion: 2, Name: "customer",
		Roles: []string{ptyapi.RoleCustomer}, Addresses: []ptyapi.Address{buyer}})
	w.want(err, kernel.CodeInvalidState)
	err = w.exec("company.A", `INSERT INTO brant_pty.party_address VALUES (current_setting('app.tenant_id'), 'company.A', 'customer', 'x', 'payer', 'x')`)
	if err == nil || !strings.Contains(err.Error(), "only while the party is a draft") {
		t.Fatalf("an address must not be added to a confirmed party: %v", err)
	}
	if err = w.exec("company.A", `UPDATE brant_pty.party_revision_address SET address = 'edited'`); err == nil {
		t.Fatal("a confirmed revision's addresses must be immutable")
	}

	_, err = w.c.PTY.ReviseParty(editor, ptyapi.ReviseParty{IdempotencyKey: w.key(), PartyID: "customer", ExpectedVersion: 2, Reason: "地址变更"})
	w.must(err)
	got, err = w.c.PTY.ResolveAddressRoles(editor, []ptyapi.AddressRoleRef{ref1})
	w.must(err)
	if a := got[ref1]; a.Code != ptyapi.CodeReferenceStale || a.Stale != ptyapi.StalePartyUnavailable || code(a.Check("/args/address")) != ptyapi.CodeReferenceStale {
		t.Fatalf("a revised party's address is not current: %+v", a)
	}
	moved := ptyapi.Address{AddressID: "hq", Role: ptyapi.AddressBuyer, Address: "合成买方地址 2"}
	consignee := ptyapi.Address{AddressID: "dock", Role: ptyapi.AddressConsignee, Address: "合成收货地址"}
	_, err = w.c.PTY.SaveParty(editor, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: "customer", ExpectedVersion: 3, Name: "customer",
		Roles: []string{ptyapi.RoleCustomer}, Addresses: []ptyapi.Address{moved, consignee}})
	w.must(err)
	_, err = w.c.PTY.ConfirmParty(editor, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "customer", ExpectedVersion: 4, EvidenceIDs: []string{"evidence"}})
	w.must(err)

	ref2 := ptyapi.AddressRoleRef{PartyID: "customer", PartyRevision: 2, AddressID: "hq"}
	billing1 := ptyapi.AddressRoleRef{PartyID: "customer", PartyRevision: 1, AddressID: "ap"}
	billing2 := ptyapi.AddressRoleRef{PartyID: "customer", PartyRevision: 2, AddressID: "ap"}
	got, err = w.c.PTY.ResolveAddressRoles(editor, []ptyapi.AddressRoleRef{ref1, ref2, billing1, billing2})
	w.must(err)
	if a := got[ref1]; a.Address != buyer.Address || a.Stale != ptyapi.StalePartyRevised || a.CurrentRevision != 2 {
		t.Fatalf("revision 1 keeps its address and is stale: %+v", a)
	}
	if a := got[ref2]; a.Address != moved.Address || a.Code != "" {
		t.Fatalf("revision 2: %+v", a)
	}
	if _, ok := got[billing2]; ok || got[billing1].Role != ptyapi.AddressBilling || len(got) != 3 {
		t.Fatalf("the billing address left with revision 2: %+v", got)
	}
	v, err = w.c.PTY.GetParty(editor, "customer")
	w.must(err)
	if len(v.Revisions) != 2 || !slices.Equal(v.Revisions[0].Addresses, []ptyapi.Address{billing, buyer}) ||
		!slices.Equal(v.Revisions[1].Addresses, []ptyapi.Address{consignee, moved}) {
		t.Fatalf("revisions %+v", v.Revisions)
	}

	// A duplicate address ID is a known value used twice; a party of another company is absent.
	_, err = w.c.PTY.SaveParty(editor, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: "dup", Name: "dup", Roles: []string{ptyapi.RoleCustomer},
		Addresses: []ptyapi.Address{buyer, billing, {AddressID: "hq", Role: ptyapi.AddressPayer, Address: "x"}}})
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/addresses/2/address_id" {
		t.Fatalf("duplicate address: %v at %s", err, path(err))
	}
	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: actorRef("human", "editor"), Permissions: allPermissions, DataScope: "company"})
	got, err = w.c.PTY.ResolveAddressRoles(other, []ptyapi.AddressRoleRef{ref2})
	w.must(err)
	if len(got) != 0 {
		t.Fatalf("another company resolves %+v", got)
	}
}

// Python test_foundation.py test_supplier_contacts_addresses_and_external_sku_mapping, the party
// and address half: a supplier's seller address is confirmed with the party and its trading profile
// publishes the settlement terms. Contacts belong to customers in Go (contact.save_draft), so the
// supplier contact_profile of the Python test has no Go counterpart; the code_mapping half belongs
// to CAT.
func TestSupplierAddressesAndTradingProfile(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	factory := ptyapi.Address{AddressID: "factory", Role: ptyapi.AddressSeller, Address: "测试地址"}
	w.staffParty(editor, "supplier", []string{ptyapi.RoleSupplier}, factory)
	payment, trade := w.settlement(editor, reviewer)
	saved, err := w.saveProfile(editor, "supplier", 0, 1, payment, trade)
	w.must(err)
	_, err = w.publishProfile(reviewer, "supplier", saved.RowVersion, 1)
	w.must(err)
	v, err := w.c.PTY.GetTradingProfile(editor, "supplier")
	w.must(err)
	if r := v.Revisions[0]; v.ActiveRevision != 1 || r.PaymentTerms != payment || r.TradeTerms != trade || len(r.ContactRefs) != 0 {
		t.Fatalf("profile %+v", v)
	}
	p, err := w.c.PTY.GetParty(editor, "supplier")
	w.must(err)
	if !slices.Equal(p.Revisions[0].Addresses, []ptyapi.Address{factory}) {
		t.Fatalf("addresses %+v", p.Revisions[0].Addresses)
	}
}

func (w *world) verifyRole(ctx context.Context, partyID string, expected int64, roleID string, from, until time.Time, evidence ...string) (ptyapi.Receipt, error) {
	return w.c.PTY.VerifyServiceRole(ctx, ptyapi.VerifyServiceRole{IdempotencyKey: w.key(), PartyID: partyID, ExpectedVersion: expected,
		ServiceRoleID: roleID, Role: ptyapi.ServiceForwarder, ServiceCode: "sea.fcl", Origin: "CNSHA", Destination: "DEHAM",
		ValidFrom: kernel.FormatTimestamp(from), ValidUntil: kernel.FormatTimestamp(until), EvidenceIDs: evidence})
}

// ServicePartyRole (T02 spec 13.1): only a human verifies a supplier's service role, on evidence it
// may read; each verification is a new revision bound to the party's confirmed revision, and
// ServiceRoleRef v1 is current while it is the latest revision, the party is still confirmed in that
// revision and the validity contains the instant.
func TestServiceRoleVerifyIsHumanOnlyAndEvidenceReadable(t *testing.T) {
	w := newWorld(t)
	editor, _ := w.polActors()
	w.staffParty(editor, "forwarder", []string{ptyapi.RoleSupplier})
	w.staffParty(editor, "customer", []string{ptyapi.RoleCustomer})
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	until := from.Add(7 * 24 * time.Hour)

	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "agent"), Permissions: allPermissions, DataScope: "company"})
	_, err := w.verifyRole(ai, "forwarder", 2, "sea", from, until, "evidence")
	w.want(err, kernel.CodePermissionDenied)
	// A human who may verify but not read sources is refused before anything is read.
	limited := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "limited"),
		Permissions: []string{"service_role.verify"}, DataScope: "company"})
	_, err = w.verifyRole(limited, "forwarder", 2, "sea", from, until, "evidence")
	w.want(err, kernel.CodePermissionDenied)
	// A verifier of the party's team who sees the team's sources but not a source registered outside
	// the team.
	private := w.actor("private.owner", allPermissions...)
	w.source(private, "private", "合成服务方核实依据")
	teamVerifier := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "team.verifier"), Permissions: allPermissions,
		DataScope: "team", Team: "trade"})
	_, err = w.verifyRole(teamVerifier, "forwarder", 2, "sea", from, until, "evidence", "private")
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence_ids/1" {
		t.Fatalf("unreadable evidence: %v at %s", err, path(err))
	}
	_, err = w.verifyRole(editor, "customer", 2, "sea", from, until, "evidence")
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/id" {
		t.Fatalf("a customer has no service role: %v", err)
	}
	_, err = w.verifyRole(editor, "forwarder", 2, "sea", from.Add(-48*time.Hour), from.Add(-24*time.Hour), "evidence")
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/valid_until" {
		t.Fatalf("ended validity: %v", err)
	}
	_, err = w.verifyRole(editor, "forwarder", 1, "sea", from, until, "evidence")
	w.want(err, kernel.CodeVersionConflict)

	r, err := w.verifyRole(editor, "forwarder", 2, "sea", from, until, "evidence")
	w.must(err)
	if r.Status != ptyapi.StatusVerified || r.RowVersion != 3 || r.ObjectID != "forwarder" {
		t.Fatalf("verify %+v", r)
	}
	events := w.events("company.A", ptyapi.Source)
	e := events[len(events)-1]
	if e.Type != ptyapi.EventServiceRoleVerified || e.Payload["service_role_id"] != "sea" || e.Payload["service_role_revision"] != float64(1) ||
		e.Payload["revision"] != float64(1) || e.Payload["row_version"] != float64(3) {
		t.Fatalf("event %+v", e)
	}
	v, err := w.c.PTY.GetParty(editor, "forwarder")
	w.must(err)
	if s := v.ServiceRoles; v.Status != ptyapi.StatusConfirmed || len(s) != 1 || s[0].PartyRevision != 1 || s[0].Origin != "CNSHA" ||
		!s[0].ValidUntil.Equal(until) || s[0].VerifiedBy.ID != "editor" || !slices.Equal(s[0].EvidenceIDs, []string{"evidence"}) {
		t.Fatalf("party %+v", v)
	}
	sea1 := ptyapi.ServiceRoleRef{PartyID: "forwarder", ServiceRoleID: "sea", Revision: 1}
	now := time.Now()
	got, err := w.c.PTY.ResolveServiceRoles(editor, []ptyapi.ServiceRoleRef{sea1, {PartyID: "forwarder", ServiceRoleID: "air", Revision: 1}}, now)
	w.must(err)
	if s := got[sea1]; len(got) != 1 || s.Code != "" || s.Role != ptyapi.ServiceForwarder || s.ServiceCode != "sea.fcl" || s.Destination != "DEHAM" {
		t.Fatalf("resolved %+v", got)
	}
	got, err = w.c.PTY.ResolveServiceRoles(editor, []ptyapi.ServiceRoleRef{sea1}, until)
	w.must(err)
	if got[sea1].Stale != ptyapi.StaleOutsideValidity {
		t.Fatalf("after its validity: %+v", got[sea1])
	}

	// A new verification supersedes the revision; a party revision makes the role stale until it is
	// verified again against the new party revision.
	_, err = w.verifyRole(editor, "forwarder", 3, "sea", from, until.Add(24*time.Hour), "evidence")
	w.must(err)
	sea2 := ptyapi.ServiceRoleRef{PartyID: "forwarder", ServiceRoleID: "sea", Revision: 2}
	got, err = w.c.PTY.ResolveServiceRoles(editor, []ptyapi.ServiceRoleRef{sea1, sea2}, now)
	w.must(err)
	if got[sea1].Stale != ptyapi.StaleSuperseded || got[sea1].LatestRevision != 2 || got[sea2].Code != "" {
		t.Fatalf("superseded %+v", got)
	}
	_, err = w.c.PTY.ReviseParty(editor, ptyapi.ReviseParty{IdempotencyKey: w.key(), PartyID: "forwarder", ExpectedVersion: 4, Reason: "更名"})
	w.must(err)
	_, err = w.verifyRole(editor, "forwarder", 5, "sea", from, until, "evidence")
	w.want(err, kernel.CodeInvalidState)
	_, err = w.c.PTY.ConfirmParty(editor, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "forwarder", ExpectedVersion: 5, EvidenceIDs: []string{"evidence"}})
	w.must(err)
	got, err = w.c.PTY.ResolveServiceRoles(editor, []ptyapi.ServiceRoleRef{sea2}, now)
	w.must(err)
	if got[sea2].Stale != ptyapi.StalePartyChanged || code(got[sea2].Check("/args/service_role")) != ptyapi.CodeReferenceStale {
		t.Fatalf("party revised: %+v", got[sea2])
	}
	if err = w.exec("company.A", `UPDATE brant_pty.service_role SET destination = 'NLRTM'`); err == nil {
		t.Fatal("verified service roles must be immutable")
	}
}

// TradingProfile: the publisher is a human other than the revision's last editor (POL-INV-001 as
// precedent; Python foundation.publish INDEPENDENT_REVIEW_REQUIRED), and concurrent publications of
// one root version have one winner (root CAS).
func TestTradingProfilePublicationIsIndependentAndHasOneWinner(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	payment, trade := w.settlement(editor, reviewer)
	w.staffParty(editor, "customer", []string{ptyapi.RoleCustomer})
	w.contact(editor, "contact", "customer", true, "evidence")

	// An agent may prepare the profile; only a human other than its last editor publishes it.
	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "agent"), Permissions: allPermissions, DataScope: "company"})
	saved, err := w.saveProfile(ai, "customer", 0, 1, payment, trade, ptyapi.PinnedContact{ContactID: "contact", RowVersion: 2})
	w.must(err)
	if saved.Status != ptyapi.StatusWorking || saved.RowVersion != 1 || saved.ObjectID != "customer" {
		t.Fatalf("save %+v", saved)
	}
	saved, err = w.saveProfile(editor, "customer", 1, 1, payment, trade, ptyapi.PinnedContact{ContactID: "contact", RowVersion: 2})
	w.must(err)
	_, err = w.publishProfile(ai, "customer", 2, 1)
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.publishProfile(editor, "customer", 2, 1)
	if code(err) != ptyapi.CodeIndependentReviewRequired || path(err) != "/args/revision" {
		t.Fatalf("self publication: %v", err)
	}
	_, err = w.publishProfile(reviewer, "customer", 2, 2)
	w.want(err, kernel.CodeNotFound)

	second := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "reviewer.2"), Permissions: allPermissions, DataScope: "company"})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, ctx := range []context.Context{reviewer, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = w.c.PTY.PublishTradingProfile(ctx, ptyapi.PublishTradingProfile{IdempotencyKey: "publish." + string(rune('a'+i)),
				PartyID: "customer", ExpectedVersion: 2, Revision: 1, EvidenceIDs: []string{"evidence"}, Reason: "核实"})
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
	v, err := w.c.PTY.GetTradingProfile(editor, "customer")
	w.must(err)
	r := v.Revisions[0]
	if v.RootVersion != 3 || v.ActiveRevision != 1 || v.WorkingRevision != 0 || r.Status != ptyapi.StatusActive || r.SavedBy.ID != "editor" ||
		(r.PublishedBy.ID != "reviewer" && r.PublishedBy.ID != "reviewer.2") || r.Owner != (ptyapi.OwnerRef{ActorID: "sales.owner", TeamID: "trade"}) ||
		!slices.Equal(r.ContactRefs, []ptyapi.PinnedContact{{ContactID: "contact", RowVersion: 2}}) || len(r.ContentHash) != 64 {
		t.Fatalf("profile %+v", v)
	}
	events := w.events("company.A", ptyapi.Source)
	e := events[len(events)-1]
	if e.Type != ptyapi.EventTradingProfilePublished || e.Payload["revision"] != float64(1) || e.Payload["content_hash"] != r.ContentHash {
		t.Fatalf("event %+v", e)
	}

	// The next revision supersedes the first in one transaction; the first stays as published.
	_, err = w.saveProfile(editor, "customer", 3, 1, payment, trade)
	w.must(err)
	published, err := w.publishProfile(reviewer, "customer", 4, 2)
	w.must(err)
	if published.Status != ptyapi.StatusActive || published.RowVersion != 5 {
		t.Fatalf("publish %+v", published)
	}
	got, err := w.c.PTY.ResolveTradingProfiles(editor, []ptyapi.TradingProfileRef{{PartyID: "customer", Revision: 1}, {PartyID: "customer"}})
	w.must(err)
	first, active := got[ptyapi.TradingProfileRef{PartyID: "customer", Revision: 1}], got[ptyapi.TradingProfileRef{PartyID: "customer"}]
	if first.Status != ptyapi.StatusSuperseded || first.Stale != ptyapi.StaleSuperseded || first.ContentHash != r.ContentHash ||
		active.Ref.Revision != 2 || active.Code != "" || active.ActiveRevision != 2 || len(active.ContactRefs) != 0 {
		t.Fatalf("resolved %+v", got)
	}
	err = w.exec("company.A", `UPDATE brant_pty.trading_profile_revision SET owner_actor_id = 'edited' WHERE revision = 1`)
	if err == nil || !strings.Contains(err.Error(), "only a working revision is edited") {
		t.Fatalf("a published revision must not be edited: %v", err)
	}
	err = w.exec("company.A", `DELETE FROM brant_pty.trading_profile_contact`)
	if err == nil || !strings.Contains(err.Error(), "only while the revision is working") {
		t.Fatalf("a published revision's contacts must not change: %v", err)
	}
	err = w.exec("company.A", `UPDATE brant_pty.trading_profile_revision SET status = 'working' WHERE revision = 1`)
	if err == nil || !strings.Contains(err.Error(), "is not a revision transition") {
		t.Fatalf("a superseded revision never returns: %v", err)
	}
}

// Python test_foundation.py test_contact_version_and_snapshot_evidence_are_preserved, in Go terms:
// a published trading profile pins its contact's row version and keeps it; once the contact is
// revised and confirmed again, the profile is stale for new use (Python: a new snapshot of it is
// REFERENCE_INVALID; the error catalog V0.1 names a moved-on version REFERENCE_STALE), a new revision
// that pins the old version is refused, and one that pins the new version publishes.
func TestStaleContactReferenceIsRefused(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	payment, trade := w.settlement(editor, reviewer)
	w.staffParty(editor, "customer", []string{ptyapi.RoleCustomer})
	w.contact(editor, "contact", "customer", true, "evidence")
	pinned := ptyapi.PinnedContact{ContactID: "contact", RowVersion: 2}
	saved, err := w.saveProfile(editor, "customer", 0, 1, payment, trade, pinned)
	w.must(err)
	_, err = w.publishProfile(reviewer, "customer", saved.RowVersion, 1)
	w.must(err)
	ref := ptyapi.TradingProfileRef{PartyID: "customer", Revision: 1}
	got, err := w.c.PTY.ResolveTradingProfiles(editor, []ptyapi.TradingProfileRef{ref})
	w.must(err)
	if got[ref].Code != "" {
		t.Fatalf("published profile %+v", got[ref])
	}
	before, err := w.c.PTY.GetTradingProfile(editor, "customer")
	w.must(err)

	r, err := w.c.PTY.ReviseContact(editor, ptyapi.ReviseContact{IdempotencyKey: w.key(), ContactID: "contact", ExpectedVersion: 2, Reason: "changed contact"})
	w.must(err)
	if r.Status != ptyapi.StatusDraft || r.RowVersion != 3 {
		t.Fatalf("revise %+v", r)
	}
	_, err = w.c.PTY.SaveContact(editor, ptyapi.SaveContact{IdempotencyKey: w.key(), ContactID: "contact", ExpectedVersion: 3, Name: "new name", PartyID: "customer"})
	w.must(err)
	_, err = w.c.PTY.ConfirmContact(editor, ptyapi.ConfirmContact{IdempotencyKey: w.key(), ContactID: "contact", ExpectedVersion: 4, EvidenceIDs: []string{"evidence"}})
	w.must(err)

	got, err = w.c.PTY.ResolveTradingProfiles(editor, []ptyapi.TradingProfileRef{ref})
	w.must(err)
	if p := got[ref]; p.Code != ptyapi.CodeReferenceStale || p.Stale != ptyapi.StaleContactChanged || code(p.Check("/args/trading_profile")) != ptyapi.CodeReferenceStale ||
		!slices.Equal(p.ContactRefs, []ptyapi.PinnedContact{pinned}) {
		t.Fatalf("stale profile %+v", p)
	}
	after, err := w.c.PTY.GetTradingProfile(editor, "customer")
	w.must(err)
	if !slices.EqualFunc(before.Revisions, after.Revisions, func(a, b ptyapi.TradingProfileRevisionView) bool {
		return a.ContentHash == b.ContentHash && slices.Equal(a.ContactRefs, b.ContactRefs) && a.PublishedAt.Equal(b.PublishedAt)
	}) {
		t.Fatalf("the published revision changed: %+v / %+v", before, after)
	}

	_, err = w.saveProfile(editor, "customer", 2, 1, payment, trade, pinned)
	if code(err) != ptyapi.CodeReferenceStale || path(err) != "/args/contact_refs/0/row_version" {
		t.Fatalf("stale pin: %v at %s", err, path(err))
	}
	saved, err = w.saveProfile(editor, "customer", 2, 1, payment, trade, ptyapi.PinnedContact{ContactID: "contact", RowVersion: 5})
	w.must(err)
	// A working revision whose contact moves on before publication is not published.
	_, err = w.c.PTY.ReviseContact(editor, ptyapi.ReviseContact{IdempotencyKey: w.key(), ContactID: "contact", ExpectedVersion: 5, Reason: "again"})
	w.must(err)
	_, err = w.publishProfile(reviewer, "customer", saved.RowVersion, 2)
	if code(err) != ptyapi.CodeReferenceStale || path(err) != "/args/revision" {
		t.Fatalf("publication of a stale pin: %v at %s", err, path(err))
	}
	_, err = w.c.PTY.SaveContact(editor, ptyapi.SaveContact{IdempotencyKey: w.key(), ContactID: "contact", ExpectedVersion: 6, Name: "new name", PartyID: "customer"})
	w.must(err)
	_, err = w.c.PTY.ConfirmContact(editor, ptyapi.ConfirmContact{IdempotencyKey: w.key(), ContactID: "contact", ExpectedVersion: 7, EvidenceIDs: []string{"evidence"}})
	w.must(err)
	saved, err = w.saveProfile(editor, "customer", saved.RowVersion, 1, payment, trade, ptyapi.PinnedContact{ContactID: "contact", RowVersion: 8})
	w.must(err)
	_, err = w.publishProfile(reviewer, "customer", saved.RowVersion, 2)
	w.must(err)
	got, err = w.c.PTY.ResolveTradingProfiles(editor, []ptyapi.TradingProfileRef{ref, {PartyID: "customer"}})
	w.must(err)
	if got[ref].Stale != ptyapi.StaleSuperseded || got[ptyapi.TradingProfileRef{PartyID: "customer"}].Code != "" {
		t.Fatalf("after republication %+v", got)
	}
}

// The references of a trading profile are checked when it is saved and again when it is published:
// a confirmed party the actor sees in the revision named, contacts of that party, settlement terms
// POL has in force.
func TestTradingProfileReferencesAreChecked(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	payment, trade := w.settlement(editor, reviewer)
	w.staffParty(editor, "customer", []string{ptyapi.RoleCustomer})
	w.staffParty(editor, "other", []string{ptyapi.RoleCustomer})
	w.contact(editor, "elsewhere", "other", true, "evidence")
	_, err := w.c.PTY.SaveParty(editor, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: "draft", Name: "draft", Roles: []string{ptyapi.RoleCustomer}})
	w.must(err)

	for _, c := range []struct {
		name     string
		party    string
		expected int64
		revision int64
		payment  ptyapi.TermsRef
		contacts []ptyapi.PinnedContact
		code     string
		path     string
	}{
		{"unknown profile", "customer", 1, 1, payment, nil, kernel.CodeNotFound, "/args/id"},
		{"draft party", "draft", 0, 1, payment, nil, kernel.CodeReferenceInvalid, "/args/id"},
		{"missing party", "nobody", 0, 1, payment, nil, kernel.CodeReferenceInvalid, "/args/id"},
		{"party revision moved on", "customer", 0, 2, payment, nil, ptyapi.CodeReferenceStale, "/args/party_revision"},
		{"contact of another party", "customer", 0, 1, payment, []ptyapi.PinnedContact{{ContactID: "elsewhere", RowVersion: 2}}, kernel.CodeReferenceInvalid, "/args/contact_refs/0/id"},
		{"unknown terms", "customer", 0, 1, ptyapi.TermsRef{Kind: polapi.KindPaymentTerms, ConfigID: "payment", Revision: 9}, nil, kernel.CodeReferenceInvalid, "/args/payment_terms"},
		{"terms of the wrong kind", "customer", 0, 1, trade, nil, kernel.CodeSchemaInvalid, "/args/payment_terms/kind"},
	} {
		_, err := w.saveProfile(editor, c.party, c.expected, c.revision, c.payment, trade, c.contacts...)
		if code(err) != c.code || path(err) != c.path {
			t.Errorf("%s: %v at %s, want %s at %s", c.name, err, path(err), c.code, c.path)
		}
	}
	win := newWindow()
	draftTerms := w.configure(editor, reviewer, polapi.KindPaymentTerms, "unpublished", 0, win.from, time.Time{}, polapi.ConfigData{PaymentTerms: &polapi.PaymentTerms{
		Name: "30 days", Stages: []polapi.PaymentStage{{StageID: "all", Percent: "100", Trigger: polapi.TriggerAfterInvoice, Days: 30}}}}, false)
	_, err = w.saveProfile(editor, "customer", 0, 1, terms(draftTerms), trade)
	if code(err) != polapi.CodeConfigNotActive || path(err) != "/args/payment_terms" {
		t.Fatalf("unpublished terms: %v at %s", err, path(err))
	}

	saved, err := w.saveProfile(editor, "customer", 0, 1, payment, trade)
	w.must(err)
	// The party is revised and confirmed again before publication: the working revision binds the
	// old party revision and is not published; the published profile is stale too.
	_, err = w.c.PTY.ReviseParty(editor, ptyapi.ReviseParty{IdempotencyKey: w.key(), PartyID: "customer", ExpectedVersion: 2, Reason: "更名"})
	w.must(err)
	_, err = w.publishProfile(reviewer, "customer", saved.RowVersion, 1)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/revision" {
		t.Fatalf("publication while the party is a draft: %v at %s", err, path(err))
	}
	_, err = w.c.PTY.ConfirmParty(editor, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "customer", ExpectedVersion: 3, EvidenceIDs: []string{"evidence"}})
	w.must(err)
	_, err = w.publishProfile(reviewer, "customer", saved.RowVersion, 1)
	if code(err) != ptyapi.CodeReferenceStale || path(err) != "/args/revision" {
		t.Fatalf("publication of an old party revision: %v at %s", err, path(err))
	}
	saved, err = w.saveProfile(editor, "customer", saved.RowVersion, 2, payment, trade)
	w.must(err)
	key := w.key()
	publish := ptyapi.PublishTradingProfile{IdempotencyKey: key, PartyID: "customer", ExpectedVersion: saved.RowVersion, Revision: 1,
		EvidenceIDs: []string{"evidence"}, Reason: "核实"}
	first, err := w.c.PTY.PublishTradingProfile(reviewer, publish)
	w.must(err)
	replay, err := w.c.PTY.PublishTradingProfile(reviewer, publish)
	w.must(err)
	if !replay.Replayed || replay.ActionID != first.ActionID {
		t.Fatalf("replay %+v", replay)
	}

	_, err = w.c.PTY.DeactivateParty(editor, ptyapi.DeactivateParty{IdempotencyKey: w.key(), PartyID: "customer", ExpectedVersion: 4, Reason: "停止合作"})
	w.must(err)
	got, err := w.c.PTY.ResolveTradingProfiles(editor, []ptyapi.TradingProfileRef{{PartyID: "customer"}})
	w.must(err)
	if p := got[ptyapi.TradingProfileRef{PartyID: "customer"}]; p.Stale != ptyapi.StalePartyUnavailable || p.PartyRevision != 2 {
		t.Fatalf("deactivated party %+v", p)
	}

	// The profile belongs to company.A: company.B neither reads nor resolves it.
	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: actorRef("human", "editor"), Permissions: allPermissions, DataScope: "company"})
	_, err = w.c.PTY.GetTradingProfile(other, "customer")
	w.want(err, kernel.CodeNotFound)
	got, err = w.c.PTY.ResolveTradingProfiles(other, []ptyapi.TradingProfileRef{{PartyID: "customer", Revision: 1}})
	w.must(err)
	if len(got) != 0 {
		t.Fatalf("another company resolves %+v", got)
	}
}
