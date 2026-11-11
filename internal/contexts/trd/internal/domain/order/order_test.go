package order

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/acceptance"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SC-07 and ES-08 again: the quote is formalized on 2026-10-21 and expires on 2026-10-28T08:00:00Z;
// the acceptance arrived on 2026-10-27T10:00:00Z and was verified on 2026-10-29. The order is
// prepared, reviewed and activated after the quote expired.
var (
	expires   = time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	arrived   = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	prepareOn = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	scope     = kernel.Scope{Tenant: "tenant.1", Company: "company.A"}
	clerk     = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk"}
	agent     = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}
	reviewer  = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "order.reviewer"}
	customer  = quote.PartyRef{PartyID: "customer.1", Revision: 2}
	contact   = quote.ContactRef{ContactID: "contact.1", RowVersion: 2}
	denied    = Risk{Path: "/data/cost_snapshot_ref", Code: "COST_SNAPSHOT_INELIGIBLE", Detail: "/sources/src.1"}
	retired   = Risk{Path: "/data/lines/0/tax_application_ref", Code: "CONFIG_NOT_ACTIVE"}
	qualified = func(string) (quote.Standing, error) {
		return quote.Standing{Human: true, Permissions: []string{PermissionDecide, PermissionRead}, SeesRoot: true}, nil
	}
)

// accepted is quote.1 with active formal revision 1, sealed with a commercial document of two lines
// and a server calculation (96000 + 1001 net, Python T03 A25's extended quote shape), and a valid
// acceptance of it that formed the contract.
func accepted(t *testing.T) (*quote.Quote, *acceptance.Acceptance) {
	t.Helper()
	ref := func(kind, id string) quote.Ref { return quote.Ref{Kind: kind, ID: id, Revision: 1} }
	c := quote.Commercial{QuoteID: "quote.1", BusinessRevision: 1, IssuedAt: "2026-10-21T08:00:00Z",
		Data: quote.Terms{Customer: customer, Contact: contact, Currency: ref("currency", "usd"), PaymentTerms: ref("payment_terms", "pay.1"),
			TradeTerms: ref("trade_terms", "fob"), ExpiresAt: kernel.FormatTimestamp(expires), Charges: []quote.Charge{{ChargeID: "freight.a25",
				ObligationKey: "freight.a25.1", Amount: "10.01"}},
			Lines: []quote.Line{{LineID: "L1", Quantity: "60", UnitPrice: "8"}, {LineID: "L2", Quantity: "60", UnitPrice: "8"}}},
		Calculation: &quote.Calculation{Lines: []quote.AmountLine{{ID: "L1", NetMinor: "48000", TaxMinor: "0", PayableMinor: "48000"},
			{ID: "L2", NetMinor: "48000", TaxMinor: "0", PayableMinor: "48000"}}, Charges: []quote.AmountLine{{ID: "freight.a25",
			NetMinor: "1001", TaxMinor: "0", PayableMinor: "1001"}}, NetMinor: "97001", TaxMinor: "0", PayableMinor: "97001"},
		NormalizationVersion: quote.NormalizationVersion}
	tree, err := quote.TreeOf(c)
	if err != nil {
		t.Fatal(err)
	}
	doc, hash, err := quote.Digest(tree)
	if err != nil {
		t.Fatal(err)
	}
	q := quote.New("quote.1", "clerk", "trade")
	q.Revisions = []*quote.Revision{{Number: 1, Status: quote.Active, Data: quote.Data{Customer: &customer, Contact: &contact},
		Seal:   &quote.Seal{Commercial: doc, BusinessHash: hash, ExpiresAt: expires},
		Formal: &quote.Formalization{Document: quote.Document{Number: "QT-2026-00001", ContentSHA256: strings.Repeat("d", 64)}}}}
	q.RootVersion = 5
	a := &acceptance.Acceptance{ID: "acceptance.1", ResponseID: "response.1", QuoteID: "quote.1", Revision: 1, BusinessHash: hash,
		Number: "QT-2026-00001", Customer: customer, Contact: contact, LineIDs: []string{"L1", "L2"}, ArrivalAt: arrived,
		ArrivalBasis: "channel_server", ExpiresAt: expires, Effectivity: acceptance.Valid, Version: 1}
	return q, a
}

func preparation(q *quote.Quote) Preparation {
	return Preparation{OrderID: "order.1", QuoteID: "quote.1", Revision: 1, BusinessHash: q.Revision(1).Seal.BusinessHash, AcceptanceID: "acceptance.1"}
}

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " " + rej.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

// prepared is the draft order of accepted's quote, prepared by an AI agent after the quote expired.
func prepared(t *testing.T) (*Order, *quote.Quote) {
	t.Helper()
	q, a := accepted(t)
	o, err := Prepare(preparation(q), q, a, scope, agent, 3, prepareOn)
	if err != nil {
		t.Fatal(err)
	}
	return o, q
}

// TRD-INV-010, 017 and Python T03 A25 (PYT-0203, 0204, 0207, 0221, 0227): the preparation, after the
// quote expired, converts the formed contract into a draft at row version 1 and takes the root's
// slot for it (the root version moves); the snapshot copies the sealed commercial document whole —
// nothing recalculated — with the acceptance it binds, and hashes to the order hash; a second
// preparation is CONVERSION_SLOT_CONFLICT. An unknown acceptance, another hash, an invalidated or a
// disputed acceptance and a held revision are refused.
func TestPrepareConvertsTheFormedContract(t *testing.T) {
	q, a := accepted(t)
	o, err := Prepare(preparation(q), q, a, scope, agent, 3, prepareOn)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != Draft || o.RowVersion != 1 || o.OrderRevision != 1 || !o.Fresh || o.Slot != quote.SlotFullV1 || o.Prepared.By != agent ||
		quote.Hash(o.Snapshot) != o.OrderHash || o.Source.AcceptanceID != "acceptance.1" || !o.Source.ArrivalAt.Equal(arrived) ||
		!o.Source.ExpiresAt.Equal(expires) || o.Source.Number != "QT-2026-00001" {
		t.Fatalf("prepared %+v", o)
	}
	if q.Slot == nil || q.Slot.OrderID != "order.1" || q.RootVersion != 6 || !q.Slot.Fresh {
		t.Fatalf("the slot %+v at %d", q.Slot, q.RootVersion)
	}
	s, err := o.Verify(scope, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if s.Commercial.Calculation.NetMinor != "97001" || s.Commercial.Calculation.Charges[0].NetMinor != "1001" || len(s.Lines()) != 2 ||
		s.Lines()[1].NetMinor != "48000" {
		t.Fatalf("the snapshot's commercial document %+v", s.Commercial.Calculation)
	}
	var body map[string]any
	if err := json.Unmarshal(o.Snapshot, &body); err != nil {
		t.Fatal(err)
	}
	terms := body["terms"].(map[string]any)
	if body["snapshot_contract"] != SnapshotContract || terms["issued_at"] != "2026-10-21T08:00:00Z" ||
		terms["expires_at"] != kernel.FormatTimestamp(expires) || body["acceptance"].(map[string]any)["arrival_at"] != kernel.FormatTimestamp(arrived) ||
		len(body["charges"].([]any)) != 1 {
		t.Fatalf("the snapshot %s", o.Snapshot)
	}
	if _, err := Prepare(preparation(q), q, a, scope, agent, 3, prepareOn); code(err) != "CONVERSION_SLOT_CONFLICT /args/quote_id" {
		t.Fatalf("a second preparation: %s", code(err))
	}

	for name, c := range map[string]struct {
		change func(*quote.Quote, *acceptance.Acceptance, *Preparation) *acceptance.Acceptance
		want   string
	}{
		"an unknown acceptance": {func(*quote.Quote, *acceptance.Acceptance, *Preparation) *acceptance.Acceptance { return nil },
			"ACCEPTANCE_SCOPE_MISMATCH /args/acceptance_id"},
		"another hash": {func(_ *quote.Quote, a *acceptance.Acceptance, p *Preparation) *acceptance.Acceptance {
			p.BusinessHash = strings.Repeat("0", 64)
			return a
		}, "BUSINESS_HASH_MISMATCH /args/business_revision"},
		"an invalidated acceptance": {func(_ *quote.Quote, a *acceptance.Acceptance, _ *Preparation) *acceptance.Acceptance {
			a.Effectivity, a.Version = acceptance.Invalidated, 2
			return a
		}, "ACCEPTANCE_NOT_VALID /args/acceptance_id"},
		"a disputed acceptance": {func(_ *quote.Quote, a *acceptance.Acceptance, _ *Preparation) *acceptance.Acceptance {
			a.Effectivity, a.Version = acceptance.Disputed, 2
			return a
		}, "ACCEPTANCE_NOT_VALID /args/acceptance_id"},
		"a held revision": {func(q *quote.Quote, a *acceptance.Acceptance, _ *Preparation) *acceptance.Acceptance {
			q.Holds = []*quote.Hold{{ID: "approval_revoked:approval.1", Revision: 1, Cause: quote.HoldApprovalRevoked, Code: quote.CodeApprovalRevoked}}
			return a
		}, "APPROVAL_REVOKED /args/business_revision"},
	} {
		q, a := accepted(t)
		p := preparation(q)
		a = c.change(q, a, &p)
		if _, err := Prepare(p, q, a, scope, agent, 3, prepareOn); code(err) != c.want || q.Slot != nil || q.RootVersion != 5 {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
}

// Python validate_order_snapshot (T03 A25, PYT-0211, 0212, 0224): an order whose snapshot does not
// hash to its order hash, or names another source than its columns, is REFERENCE_INVALID.
func TestVerifyDetectsATamperedOrder(t *testing.T) {
	for name, tamper := range map[string]func(*Order){
		"the snapshot's bytes": func(o *Order) { o.Snapshot = append(o.Snapshot[:len(o.Snapshot)-1], ' ', '}') },
		"a rehashed snapshot": func(o *Order) {
			o.Snapshot = []byte(strings.Replace(string(o.Snapshot), `"60"`, `"1"`, 1))
			o.OrderHash = quote.Hash(o.Snapshot)
		},
		"the acceptance column": func(o *Order) { o.Source.AcceptanceID = "other" },
		"the revision column":   func(o *Order) { o.Source.Revision = 2 },
		"the order ID":          func(o *Order) { o.ID = "order.other" },
	} {
		o, _ := prepared(t)
		tamper(o)
		if _, err := o.Verify(scope, "/args/order_id"); code(err) != "REFERENCE_INVALID /args/order_id" {
			t.Errorf("%s: %s", name, code(err))
		}
	}
}

// SM-TRD-ORDER-REVIEW and TRD-INV-013 (Python T04 A38–A41: PYT-0480, 0481, 0496, 0498, 0504, 0515,
// 0516): one open review per order; a taken review ID is ORDER_REVIEW_IMMUTABLE; the reviewer is
// neither the preparer nor the requester; the decision restates the order hash; an approval is valid
// after its decision (and not bounded by the quote's expiry, which passed); it acknowledges exactly
// the upstream risks found, one by one; a decided review never changes; only an approval is
// revoked, once; and nothing is reviewed once the order is not a draft.
func TestOrderReviewDecisions(t *testing.T) {
	o, _ := prepared(t)
	at := prepareOn.Add(time.Hour)
	r, err := o.RequestReview("review.1", clerk, 3, at)
	if err != nil || r.Status != ReviewOpen || o.RowVersion != 2 || r.OrderHash != o.OrderHash || !r.Fresh {
		t.Fatalf("requested %+v %v", r, err)
	}
	if _, err := o.RequestReview("review.2", clerk, 3, at); code(err) != "INVALID_STATE /args/review_id" {
		t.Fatalf("a second open review: %s", code(err))
	}
	if _, err := o.RequestReview("review.1", clerk, 3, at); code(err) != "ORDER_REVIEW_IMMUTABLE /args/review_id" {
		t.Fatalf("a taken review ID: %s", code(err))
	}
	approve := Decision{ReviewID: "review.1", OrderHash: o.OrderHash, Approve: true, ExpiresAt: at.Add(30 * 24 * time.Hour)}
	none := func() ([]Risk, error) { return nil, nil }
	found := func() ([]Risk, error) { return []Risk{denied, retired}, nil }
	for name, c := range map[string]struct {
		d     Decision
		by    kernel.ActorRef
		risks func() ([]Risk, error)
		want  string
	}{
		"by the preparer":  {approve, agent, none, "REVIEWER_NOT_INDEPENDENT /args/review_id"},
		"by the requester": {approve, clerk, none, "REVIEWER_NOT_INDEPENDENT /args/review_id"},
		"an unknown review": {Decision{ReviewID: "review.none", OrderHash: o.OrderHash, Approve: true, ExpiresAt: approve.ExpiresAt}, reviewer, none,
			"NOT_FOUND /args/review_id"},
		"another hash": {Decision{ReviewID: "review.1", OrderHash: strings.Repeat("0", 64), Approve: true, ExpiresAt: approve.ExpiresAt}, reviewer,
			none, "ORDER_REVIEW_MISMATCH /args/order_hash"},
		"valid until the decision": {Decision{ReviewID: "review.1", OrderHash: o.OrderHash, Approve: true, ExpiresAt: at}, reviewer, none,
			"ORDER_REVIEW_EXPIRY_INVALID /args/expires_at"},
		"a risk not acknowledged": {Decision{ReviewID: "review.1", OrderHash: o.OrderHash, Approve: true, ExpiresAt: approve.ExpiresAt,
			Acknowledged: []Risk{denied}}, reviewer, found, "ORDER_RISK_UNACKNOWLEDGED /args/risk_acknowledgements"},
		"a risk not found": {Decision{ReviewID: "review.1", OrderHash: o.OrderHash, Approve: true, ExpiresAt: approve.ExpiresAt,
			Acknowledged: []Risk{denied, retired, {Path: "/data/currency_ref", Code: "CONFIG_EXPIRED"}}}, reviewer, found,
			"REFERENCE_INVALID /args/risk_acknowledgements/2"},
		"a near miss": {Decision{ReviewID: "review.1", OrderHash: o.OrderHash, Approve: true, ExpiresAt: approve.ExpiresAt,
			Acknowledged: []Risk{{Path: denied.Path, Code: denied.Code, Detail: "/sources/src.2"}, retired}}, reviewer, found,
			"REFERENCE_INVALID /args/risk_acknowledgements/0"},
	} {
		if _, err := o.Decide(c.d, c.by, 4, c.risks, at); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
	if r.Status != ReviewOpen || o.RowVersion != 2 {
		t.Fatalf("refusals changed the review %+v", r)
	}
	ok := approve
	ok.Acknowledged = []Risk{retired, denied}
	if _, err := o.Decide(ok, reviewer, 4, found, at); err != nil || r.Status != ReviewApproved || o.RowVersion != 3 || len(r.Acknowledged) != 2 ||
		!r.ExpiresAt.Equal(approve.ExpiresAt) || r.Decided.By != reviewer {
		t.Fatalf("approved %+v %v", r, err)
	}
	if _, err := o.Decide(ok, reviewer, 4, found, at); code(err) != "ORDER_REVIEW_IMMUTABLE /args/review_id" {
		t.Fatalf("decided twice: %s", code(err))
	}
	revoke := quote.Act{By: reviewer, Epoch: 4, At: at, Reason: "客户条件变化"}
	if _, err := o.Revoke("review.1", revoke); err != nil || r.Status != ReviewRevoked || o.RowVersion != 4 {
		t.Fatalf("revoked %+v %v", r, err)
	}
	if _, err := o.Revoke("review.1", revoke); code(err) != "ORDER_REVIEW_IMMUTABLE /args/review_id" {
		t.Fatalf("revoked twice: %s", code(err))
	}
	if _, err := o.RequestReview("review.2", clerk, 3, at); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Revoke("review.2", revoke); code(err) != "ORDER_APPROVAL_REQUIRED /args/review_id" {
		t.Fatalf("revoking an open review: %s", code(err))
	}
	rejected, err := o.Decide(Decision{ReviewID: "review.2", OrderHash: o.OrderHash, Reason: "条款待定"}, reviewer, 4, nil, at)
	if err != nil || rejected.Status != ReviewRejected || rejected.Decided.Reason != "条款待定" {
		t.Fatalf("rejected %+v %v", rejected, err)
	}
	if _, err := o.Revoke("review.2", revoke); code(err) != "ORDER_APPROVAL_REQUIRED /args/review_id" {
		t.Fatalf("revoking a rejection: %s", code(err))
	}
	if _, err := o.Cancel(quote.Act{By: clerk, Epoch: 3, At: at, Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.RequestReview("review.3", clerk, 3, at); code(err) != "ORDER_NOT_DRAFT /args/order_id" {
		t.Fatalf("a review of a cancelled order: %s", code(err))
	}
}

// approvedOrder is prepared's order with review.1 requested by the clerk and approved by the
// reviewer at decided, valid until until, acknowledging acknowledged (found then).
func approvedOrder(t *testing.T, decided, until time.Time, acknowledged ...Risk) (*Order, *Review) {
	t.Helper()
	o, _ := prepared(t)
	if _, err := o.RequestReview("review.1", clerk, 3, decided); err != nil {
		t.Fatal(err)
	}
	r, err := o.Decide(Decision{ReviewID: "review.1", OrderHash: o.OrderHash, Approve: true, ExpiresAt: until, Acknowledged: acknowledged},
		reviewer, 4, func() ([]Risk, error) { return acknowledged, nil }, decided)
	if err != nil {
		t.Fatal(err)
	}
	return o, r
}

// TRD-INV-013, 014, BC-AT13 and Python T04 A39 (PYT-0487–0495, 0502): the order gate admits an
// approved, unrevoked review of the order's revision and hash by a reviewer still independent,
// human and qualified, before its expires_at (equality refused); activation needs every upstream
// risk found then acknowledged, after the quote expired too, and writes the commit evidence with the
// acceptance's arrival, the quote's expires_at as bound, the review and the risks.
func TestOrderGateAndActivation(t *testing.T) {
	decided := prepareOn.Add(time.Hour)
	until := decided.Add(48 * time.Hour)
	o, r := approvedOrder(t, decided, until, denied)
	activateOn := decided.Add(time.Hour)
	for name, c := range map[string]struct {
		change    func(*Order, *Review)
		standings quote.Standings
		at        time.Time
		want      string
	}{
		"an unknown review": {func(o *Order, r *Review) { r.ID = "review.other" }, qualified, activateOn, "NOT_FOUND /args/review_id"},
		"another order hash": {func(o *Order, r *Review) { r.OrderHash = strings.Repeat("0", 64) }, qualified, activateOn,
			"ORDER_REVIEW_STALE /args/review_id"},
		"an open review":    {func(o *Order, r *Review) { r.Status = ReviewOpen }, qualified, activateOn, "ORDER_APPROVAL_REQUIRED /args/review_id"},
		"a rejected review": {func(o *Order, r *Review) { r.Status = ReviewRejected }, qualified, activateOn, "ORDER_APPROVAL_REQUIRED /args/review_id"},
		"a revoked review":  {func(o *Order, r *Review) { r.Status = ReviewRevoked }, qualified, activateOn, "ORDER_APPROVAL_REVOKED /args/review_id"},
		"a reviewer who prepared the order (PYT-0495)": {func(o *Order, r *Review) { r.Decided.By = agent }, qualified, activateOn,
			"REVIEWER_NOT_INDEPENDENT /args/review_id"},
		"a reviewer without the permission (PYT-0492)": {func(*Order, *Review) {}, func(string) (quote.Standing, error) {
			return quote.Standing{Human: true, Permissions: []string{PermissionRead}, SeesRoot: true}, nil
		}, activateOn, "ORDER_APPROVAL_REQUIRED /args/review_id"},
		"a reviewer who no longer sees the quote (PYT-0493)": {func(*Order, *Review) {}, func(string) (quote.Standing, error) {
			return quote.Standing{Human: true, Permissions: []string{PermissionDecide, PermissionRead}}, nil
		}, activateOn, "ORDER_APPROVAL_REQUIRED /args/review_id"},
		"at the approval's expires_at (PYT-0491, 0502)": {func(*Order, *Review) {}, qualified, until, "ORDER_APPROVAL_EXPIRED /args/review_id"},
		"a cancelled order":                             {func(o *Order, _ *Review) { o.Status = Cancelled }, qualified, activateOn, "ORDER_NOT_DRAFT /args/order_id"},
	} {
		o, r := approvedOrder(t, decided, until, denied)
		c.change(o, r)
		if _, err := o.Admit("review.1", c.standings, c.at); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
	admitted, err := o.Admit("review.1", qualified, activateOn)
	if err != nil || admitted != r {
		t.Fatalf("admitted %+v %v", admitted, err)
	}
	a := Activation{Review: r, IdempotencyKey: "k.1", Risks: []Risk{denied, retired}, ApprovalID: "approval.1", CostSnapshotID: "snapshot.1",
		Upstream: []quote.Entry{{Name: "stl.credit_guard.v1", Value: "not_wired"}}, ControlEpoch: 0}
	if _, err := o.Activate(a, clerk, 3, activateOn); code(err) != "ORDER_RISK_UNACKNOWLEDGED /args/review_id" || o.Status != Draft || o.RowVersion != 3 {
		t.Fatalf("an unacknowledged risk: %s", code(err))
	}
	a.Risks = []Risk{denied}
	c, err := o.Activate(a, clerk, 3, activateOn)
	if err != nil || o.Status != Active || o.RowVersion != 4 || o.ActivatedReview != "review.1" || c.RowVersion != 4 || !c.Fresh ||
		o.ActivatedAt(4) != c {
		t.Fatalf("activated %+v %v", o, err)
	}
	entries := map[string]string{}
	for _, e := range c.Entries {
		entries[e.Name] = e.Value
	}
	for name, want := range map[string]string{"arrival_at": kernel.FormatTimestamp(arrived), "quote_expires_at": kernel.FormatTimestamp(expires),
		"contract_formed": "true", "review_id": "review.1", "reviewer": "human:order.reviewer", "risks_acknowledged": "1",
		"risks_found":         `[{"code":"COST_SNAPSHOT_INELIGIBLE","detail":"/sources/src.1","path":"/data/cost_snapshot_ref"}]`,
		"stl.credit_guard.v1": "not_wired", "formalization_approval_id": "approval.1", "row_version": "4", "committed_by": "human:clerk"} {
		if entries[name] != want {
			t.Errorf("entry %s = %q, want %q", name, entries[name], want)
		}
	}
	if _, err := o.Activate(a, clerk, 3, activateOn); code(err) != "ORDER_NOT_DRAFT /args/order_id" {
		t.Fatalf("activated twice: %s", code(err))
	}
	// Revoking the approval that activated the order: the order stays active.
	if _, err := o.Revoke("review.1", quote.Act{By: reviewer, Epoch: 4, At: activateOn, Reason: "r"}); err != nil || o.Status != Active ||
		o.RowVersion != 5 {
		t.Fatalf("revoked after activation %+v %v", o, err)
	}
}

// SM-TRD-ORDER draft → cancelled and draft → closed_unfulfilled: a closure at the next row
// version; neither applies to an order that is not a draft.
func TestDraftOrdersEnd(t *testing.T) {
	o, _ := prepared(t)
	act := quote.Act{By: clerk, Epoch: 3, At: prepareOn, Reason: "接受被作废", Evidence: []quote.Evidence{{SourceID: "mail.1", Locator: "x"}}}
	c, err := o.CloseUnfulfilled(act)
	if err != nil || o.Status != ClosedUnfulfilled || c.RowVersion != 2 || o.RowVersion != 2 || !c.Fresh || c.Closed.Reason != act.Reason {
		t.Fatalf("closed %+v %v", c, err)
	}
	if _, err := o.Cancel(act); code(err) != "ORDER_NOT_DRAFT /args/order_id" {
		t.Fatalf("cancelled after closing: %s", code(err))
	}
	if err := o.Check(1); code(err) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatalf("an earlier row version: %s", code(err))
	}
}
