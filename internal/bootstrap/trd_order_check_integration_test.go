//go:build integration

package bootstrap_test

import (
	"testing"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TRD's checks of an AI agent's order commands before a person is asked (S4 G7b;
// trdapi.OrderRequestChecks): each judges the request as the command judges it on the stored records
// and writes nothing — no order, no review, no event — except its audit entry, needs the permission
// of the command it checks and no idempotency key, and answers the same code at the same path as the
// command would. What only the send's transaction can judge (the object control of TRD-INV-015, a
// slot taken after the check) is the command's.

// agree fails unless the check and the command both refused, with the same code at the same path.
func agree(t *testing.T, what string, check, command error) {
	t.Helper()
	if code(check) != code(command) || path(check) != path(command) {
		t.Errorf("%s: the check says %s at %q, the command %s at %q", what, code(check), path(check), code(command), path(command))
	}
	if code(check) == "OK" {
		t.Errorf("%s: both accepted, the case is no refusal", what)
	}
}

func TestOrderPrepareCheckJudgesAsTheCommandDoesWithoutWriting(t *testing.T) {
	q := newOrderWorld(t)
	notify := q.notifyParty()
	_, a1 := q.accepted("quote.1")
	_, a2 := q.accepted("quote.2")
	check := func(cmd trdapi.PrepareOrder) error {
		cmd.IdempotencyKey = ""
		return q.c.TRD.CheckPrepareOrder(q.author, cmd)
	}
	events := func() int { return len(q.events("company.A", trdapi.Source)) }
	before := events()

	// Refusals the check shares with the command, on the records as they are.
	otherRole := q.prepareCmd("order.1", a1)
	otherRole.NotifyPartyRef = &trdapi.AddressRoleRef{PartyID: notify.PartyID, PartyRevision: notify.PartyRevision, AddressID: "dock"}
	missingQuote := q.prepareCmd("order.1", a1)
	missingQuote.QuoteID = "quote.none"
	badHash := q.prepareCmd("order.1", a1)
	badHash.BusinessHash = "0000000000000000000000000000000000000000000000000000000000000000"
	noOrderID := q.prepareCmd("order.1", a1)
	noOrderID.OrderID = ""
	wrongAcceptance := q.prepareCmd("order.1", a1)
	wrongAcceptance.AcceptanceID = a2.ObjectID
	unknownAddress := q.prepareCmd("order.1", a1)
	unknownAddress.NotifyPartyRef = &trdapi.AddressRoleRef{PartyID: notify.PartyID, PartyRevision: notify.PartyRevision, AddressID: "nowhere"}
	for name, cmd := range map[string]trdapi.PrepareOrder{"a notify address of another role": otherRole, "an unknown quote": missingQuote,
		"another hash": badHash, "no order ID": noOrderID, "another quote's acceptance": wrongAcceptance, "an unknown notify address": unknownAddress} {
		got := check(cmd)
		if got == nil {
			t.Errorf("%s: the check accepted it", name)
			continue
		}
		cmd.IdempotencyKey = q.key()
		_, err := q.c.TRD.PrepareOrder(q.author, cmd)
		agree(t, name, got, err)
	}
	if got := check(otherRole); code(got) != kernel.CodeInvalidKnownValue || path(got) != "/args/notify_party_ref" {
		t.Errorf("the notify address of another role: %s at %q", code(got), path(got))
	}

	// What it accepts it writes nothing for; the command then does the same.
	good := q.prepareCmd("order.1", a1)
	good.NotifyPartyRef = &notify
	if err := check(good); err != nil {
		t.Fatalf("a valid request: %v", err)
	}
	if err := check(good); err != nil {
		t.Fatalf("a valid request again: %v", err)
	}
	_, err := q.c.TRD.GetOrder(q.author, "order.1")
	if code(err) != kernel.CodeNotFound {
		t.Fatalf("an order exists after the checks: %s", code(err))
	}
	if got := events(); got != before {
		t.Fatalf("events %d before the checks, %d after", before, got)
	}
	if _, err = q.c.TRD.PrepareOrder(q.author, good); err != nil {
		t.Fatalf("the command refused what the check accepted: %v", err)
	}

	// The records moved: the check sees them as the command does.
	slot := q.prepareCmd("order.2", a1)
	agree(t, "a taken conversion slot", check(slot), func() error { _, e := q.c.TRD.PrepareOrder(q.author, slot); return e }())
	if got := check(slot); code(got) != trdapi.CodeConversionSlotConflict {
		t.Errorf("a taken conversion slot: %s", code(got))
	}
	foreign := q.prepareCmd("order.1", a2)
	agree(t, "another quote's order ID", check(foreign), func() error { _, e := q.c.TRD.PrepareOrder(q.author, foreign); return e }())
	if got := check(foreign); code(got) != trdapi.CodeBusinessKeyConflict || path(got) != "/args/order_id" {
		t.Errorf("another quote's order ID: %s at %q", code(got), path(got))
	}

	// The permission of the command, and the audit of every check.
	nobody := q.provision(grantOf(q, "nobody.check", kernel.ActorHuman, "company"))
	if got := q.c.TRD.CheckPrepareOrder(nobody, good); code(got) != kernel.CodePermissionDenied {
		t.Errorf("a caller without order.prepare: %s", code(got))
	}
	entries, err := q.c.TRD.Audit(q.author)
	q.must(err)
	var read, rejected, denied int
	for _, e := range entries {
		switch {
		case e.Command != "order.prepare.check":
		case e.Outcome == "read":
			read++
		case e.Outcome == "rejected" && e.Code == kernel.CodePermissionDenied:
			denied++
		case e.Outcome == "rejected":
			rejected++
		}
	}
	if read != 2 || rejected < 6 || denied != 1 {
		t.Errorf("audited checks: %d read, %d rejected, %d denied", read, rejected, denied)
	}
}

func TestOrderReviewCheckJudgesAsTheCommandDoesWithoutWriting(t *testing.T) {
	q := newOrderWorld(t)
	_, order := q.prepared("quote.1")
	version := q.orderVersion(order.ObjectID)
	check := func(expected int64, review string) error {
		return q.c.TRD.CheckRequestOrderReview(q.author, trdapi.RequestOrderReview{OrderID: order.ObjectID, ExpectedVersion: expected, ReviewID: review})
	}
	run := func(expected int64, review string) error {
		_, err := q.c.TRD.RequestOrderReview(q.author, q.requestCmd(order.ObjectID, expected, review))
		return err
	}
	if got := q.c.TRD.CheckRequestOrderReview(q.author, trdapi.RequestOrderReview{OrderID: "order.none", ExpectedVersion: 1, ReviewID: "review.1"}); code(got) != kernel.CodeNotFound ||
		path(got) != "/args/order_id" {
		t.Errorf("an unknown order: %s at %q", code(got), path(got))
	}
	agree(t, "a stale version", check(version+2, "review.1"), run(version+2, "review.1"))
	if got := check(version+2, "review.1"); code(got) != kernel.CodeVersionConflict || path(got) != "/args/expected_version" {
		t.Errorf("a stale version: %s at %q", code(got), path(got))
	}
	agree(t, "no review ID", check(version, ""), run(version, ""))

	before := len(q.events("company.A", trdapi.Source))
	for range 2 {
		if err := check(version, "review.1"); err != nil {
			t.Fatalf("a valid request: %v", err)
		}
	}
	if q.orderVersion(order.ObjectID) != version || len(q.events("company.A", trdapi.Source)) != before {
		t.Fatal("the check wrote")
	}
	if err := run(version, "review.1"); err != nil {
		t.Fatalf("the command refused what the check accepted: %v", err)
	}
	// A review is open now: the check says what the command says.
	next := q.orderVersion(order.ObjectID)
	agree(t, "an open review", check(next, "review.2"), run(next, "review.2"))
	if got := check(next, "review.2"); code(got) == "OK" || code(got) == kernel.CodePermissionDenied {
		t.Errorf("an open review: %s", code(got))
	}
	nobody := q.provision(grantOf(q, "nobody.review", kernel.ActorHuman, "company"))
	if got := q.c.TRD.CheckRequestOrderReview(nobody, trdapi.RequestOrderReview{OrderID: order.ObjectID, ExpectedVersion: next, ReviewID: "review.3"}); code(got) != kernel.CodePermissionDenied {
		t.Errorf("a caller without order.review.request: %s", code(got))
	}
	entries, err := q.c.TRD.Audit(q.author)
	q.must(err)
	var read, rejected int
	for _, e := range entries {
		if e.Command == "order.review.request.check" {
			switch e.Outcome {
			case "read":
				read++
			case "rejected":
				rejected++
			}
		}
	}
	if read != 2 || rejected < 3 {
		t.Errorf("audited checks: %d read, %d rejected", read, rejected)
	}
}
