package profile

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	at       = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	editor   = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "editor"}
	agent    = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent"}
	reviewer = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"}
	payment  = TermsRef{Kind: KindPaymentTerms, ConfigID: "payment", Revision: 1}
	trade    = TermsRef{Kind: KindTradeTerms, ConfigID: "FCA", Revision: 1}
	owner    = Owner{ActorID: "sales.owner", TeamID: "trade"}
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}

func content(contacts ...ContactRef) Content { return NewContent(1, payment, trade, owner, contacts) }

// The order of the checks is the Python order: existence and visibility, root version.
func TestChecksRunInContractOrder(t *testing.T) {
	root := &Root{PartyID: "customer", Version: 2, LastRevision: 1}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"new at 0", CheckSave(nil, true, 0), "OK"},
		{"new at 1", CheckSave(nil, true, 1), "NOT_FOUND /args/id"},
		{"party not visible", CheckSave(root, false, 2), "NOT_FOUND /args/id"},
		{"stale", CheckSave(root, true, 1), "VERSION_CONFLICT /args/expected_version"},
		{"current", Check(root, true, 2), "OK"},
		{"missing", Check(nil, true, 1), "NOT_FOUND /args/id"},
	} {
		if got := code(c.err); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// SM-POL-REVISION as for configurations: one working revision is saved whole, a human other than
// its last editor publishes it, and the next publication supersedes it; revision numbers are never
// reused and the root version counts every command.
func TestRevisionsArePublishedIndependentlyAndSuperseded(t *testing.T) {
	r := New("customer")
	first, fresh := r.Save(content(), agent, 1, at)
	if !fresh || first.Number != 1 || r.Version != 1 || first.Status != Working {
		t.Fatalf("first save %+v %+v", r, first)
	}
	again, fresh := r.Save(content(ContactRef{ContactID: "c", RowVersion: 2}), editor, 1, at)
	if fresh || again != first || r.Version != 2 || again.SavedBy != editor || len(again.Content.Contacts) != 1 {
		t.Fatalf("second save %+v", again)
	}
	for _, c := range []struct {
		number int64
		by     kernel.ActorRef
		want   string
	}{
		{2, reviewer, "NOT_FOUND /args/revision"},
		{1, editor, "INDEPENDENT_REVIEW_REQUIRED /args/revision"},
		{1, reviewer, "OK"},
	} {
		_, err := r.Publishable(c.number, c.by)
		if got := code(err); got != c.want {
			t.Errorf("publish %d by %s: %s, want %s", c.number, c.by.ID, got, c.want)
		}
	}
	if superseded := r.Publish(reviewer, 3, []string{"evidence"}, at); superseded != nil || r.Active != first || r.Working != nil || r.Version != 3 {
		t.Fatalf("publish %+v", r)
	}
	if _, err := r.Publishable(1, reviewer); code(err) != "INVALID_STATE /args/revision" {
		t.Fatalf("published twice: %v", err)
	}
	second, fresh := r.Save(content(), editor, 3, at)
	if !fresh || second.Number != 2 || r.Active != first {
		t.Fatalf("a new working revision leaves the active one: %+v", r)
	}
	if _, err := r.Publishable(2, reviewer); err != nil {
		t.Fatal(err)
	}
	superseded := r.Publish(reviewer, 4, []string{"evidence"}, at.Add(time.Hour))
	if superseded != first || first.Status != Superseded || !first.EndedAt.Equal(at.Add(time.Hour)) || r.Active != second || r.Version != 5 {
		t.Fatalf("supersession %+v %+v", r, first)
	}
	if first.Publication.By != reviewer || first.Publication.EvidenceIDs[0] != "evidence" {
		t.Fatalf("the superseded revision keeps its publication: %+v", first.Publication)
	}
}

func TestContentHashIsCanonical(t *testing.T) {
	a := content(ContactRef{ContactID: "b", RowVersion: 2}, ContactRef{ContactID: "a", RowVersion: 5})
	b := Content{PartyRevision: 1, PaymentTerms: payment, TradeTerms: trade, Owner: owner,
		Contacts: []ContactRef{{ContactID: "a", RowVersion: 5}, {ContactID: "b", RowVersion: 2}}}
	if a.Hash() != b.Hash() || len(a.Hash()) != 64 {
		t.Fatalf("contact order changes the hash: %s %s", a.Hash(), b.Hash())
	}
	c := content(ContactRef{ContactID: "b", RowVersion: 3}, ContactRef{ContactID: "a", RowVersion: 5})
	if a.Hash() == c.Hash() || content().Hash() == NewContent(2, payment, trade, owner, nil).Hash() {
		t.Fatal("a row version or a party revision is part of the content")
	}
}

// Python party_profile validate(): the party must be a confirmed party the actor sees, at the bound
// revision; every contact must be a confirmed contact of the party at the bound row version. A
// version that moved on is REFERENCE_STALE (error catalog V0.1), everything else REFERENCE_INVALID.
func TestReferencesAreCheckedWithExactCodes(t *testing.T) {
	party := PartyFact{Visible: true, Confirmed: true, Revision: 1}
	contacts := map[string]ContactFact{
		"contact": {PartyID: "customer", Confirmed: true, RowVersion: 2},
		"draft":   {PartyID: "customer", Confirmed: false, RowVersion: 3},
		"other":   {PartyID: "other", Confirmed: true, RowVersion: 2},
	}
	pin := func(id string, v int64) ContactRef { return ContactRef{ContactID: id, RowVersion: v} }
	for _, c := range []struct {
		name    string
		content Content
		party   PartyFact
		want    string
	}{
		{"current", content(pin("contact", 2)), party, "OK"},
		{"party not visible", content(), PartyFact{Confirmed: true, Revision: 1}, "REFERENCE_INVALID /args/id"},
		{"party a draft", content(), PartyFact{Visible: true, Revision: 1}, "REFERENCE_INVALID /args/id"},
		{"party revised", content(), PartyFact{Visible: true, Confirmed: true, Revision: 2}, "REFERENCE_STALE /args/party_revision"},
		{"unknown contact", content(pin("contact", 2), pin("nobody", 1)), party, "REFERENCE_INVALID /args/contact_refs/1/id"},
		{"contact of another party", content(pin("other", 2)), party, "REFERENCE_INVALID /args/contact_refs/0/id"},
		{"contact moved on", content(pin("contact", 1)), party, "REFERENCE_STALE /args/contact_refs/0/row_version"},
		{"contact a draft", content(pin("draft", 3)), party, "REFERENCE_INVALID /args/contact_refs/0/id"},
	} {
		err := CheckReferences("customer", c.content, Facts{Party: c.party, Contacts: contacts})
		if got := code(err); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
		if rej, ok := kernel.AsRejection(err); ok && rej.Code == CodeReferenceStale && rej.Category != kernel.CategoryConflict {
			t.Errorf("%s: REFERENCE_STALE is a conflict", c.name)
		}
	}
	err := Repath(CheckReferences("customer", content(pin("contact", 1)), Facts{Party: party, Contacts: contacts}), "/args/revision")
	if code(err) != "REFERENCE_STALE /args/revision" {
		t.Fatalf("repath: %v", err)
	}
}

// Python test_foundation.py test_contact_version_and_snapshot_evidence_are_preserved, at the domain
// level: the published revision keeps the contact version it bound; once the contact is confirmed
// again at a later row version, the revision no longer serves new business, and neither does a
// superseded revision or one whose party moved on.
func TestPublishedRevisionKeepsItsContactVersion(t *testing.T) {
	r := New("customer")
	rev, _ := r.Save(content(ContactRef{ContactID: "contact", RowVersion: 2}), editor, 1, at)
	if _, err := r.Publishable(1, reviewer); err != nil {
		t.Fatal(err)
	}
	r.Publish(reviewer, 1, []string{"evidence"}, at)
	party := PartyFact{Visible: true, Confirmed: true, Revision: 1}
	confirmedAt := func(v int64) map[string]ContactFact {
		return map[string]ContactFact{"contact": {PartyID: "customer", Confirmed: true, RowVersion: v}}
	}
	for _, c := range []struct {
		name     string
		party    PartyFact
		contacts map[string]ContactFact
		want     Use
	}{
		{"as published", party, confirmedAt(2), Usable},
		{"contact revised and confirmed again", party, confirmedAt(5), UseContactChanged},
		{"contact revised into a draft", party, map[string]ContactFact{"contact": {PartyID: "customer", RowVersion: 3}}, UseContactChanged},
		{"party revised", PartyFact{Visible: true, Revision: 1}, confirmedAt(2), UsePartyUnavailable},
		{"party confirmed again", PartyFact{Visible: true, Confirmed: true, Revision: 2}, confirmedAt(2), UsePartyChanged},
	} {
		if got := Usability(rev, c.party, c.contacts); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if rev.Content.Contacts[0].RowVersion != 2 {
		t.Fatalf("the revision changed: %+v", rev.Content)
	}
	r.Save(content(ContactRef{ContactID: "contact", RowVersion: 5}), editor, 1, at)
	r.Publish(reviewer, 1, []string{"evidence"}, at)
	if got := Usability(rev, party, confirmedAt(2)); got != UseSuperseded {
		t.Fatalf("superseded revision: %q", got)
	}
}
