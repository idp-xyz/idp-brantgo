package acceptance

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/response"
)

// sealedUnder is the fixture's active revision whose sealed document carries legal terms legal (a
// JSON object, "" for none), with its business hash.
func sealedUnder(legal string) (*quote.Quote, string) {
	doc := `{"business_revision":1,"quote_id":"quote.1"}`
	if legal != "" {
		doc = `{"business_revision":1,"legal_terms":` + legal + `,"quote_id":"quote.1"}`
	}
	q := activeQuote()
	s := q.Revisions[0].Seal
	s.Commercial, s.BusinessHash = []byte(doc), quote.Hash([]byte(doc))
	return q, s.BusinessHash
}

// verifiedUnder records a full acceptance of q's revision at hash that arrived at arrival and
// verifies it on day 8.
func verifiedUnder(t *testing.T, q *quote.Quote, hash, id string, arrival time.Time) (*Acceptance, error) {
	t.Helper()
	at := arrival.Add(time.Hour)
	r, _, err := response.Record(q, nil, response.Draft{ID: id, QuoteID: "quote.1", Revision: 1, BusinessHash: hash,
		Classification: response.FullUnconditional, Summary: "接受", Message: quote.Evidence{SourceID: "mail.1", Locator: id},
		Arrival: channel(arrival)}, readable, clerk, 1, at)
	if err != nil {
		t.Fatal(err)
	}
	v := Verification{AcceptanceID: "acceptance." + id, Revision: 1, BusinessHash: hash, Customer: customer, Contact: contact,
		LineIDs: []string{"L1", "L2"}}
	a, _, err := Verify(r, q, v, Known{}, readable, verifier, 1, verifiedOn)
	return a, err
}

// WBS S3-17 (G0 F-02: 合同成立规则按适用法律选择; the condition of ADR-BR-013; SIM-02 step 3): the
// acceptance ledger judges formation by the rule the revision's legal terms seal. Under German law
// with the CISG (basis cisg, CISG Art. 18(2), 23) and under PRC law without it (basis
// prc_civil_code, Civil Code Art. 137, 483, 484) an acceptance that arrived on day 6 forms the
// contract at its arrival although verified on day 8, after the quote expired, and records the
// formation it was judged by; one arriving at expires_at is late under both. A revision without
// legal terms forms by the design's default (the CISG with PRC law). A revision sealing a rule Brant
// does not implement is refused with DEPENDENCY_NOT_SUPPORTED, never judged as arrival.
func TestFormationFollowsTheRevisionsLegalTerms(t *testing.T) {
	hamburg := `{"cisg":"applies","dispute_resolution":{"institution":"Court of Arbitration of the Hamburg Chamber of Commerce",` +
		`"language":"en","method":"arbitration","seat":"Hamburg"},"formation":{"basis":"cisg","rule":"arrival"},"governing_law":"DE",` +
		`"source":"quote"}`
	prc := `{"cisg":"excluded","dispute_resolution":{"court":"Shanghai International Commercial Court","method":"litigation"},` +
		`"formation":{"basis":"prc_civil_code","rule":"arrival"},"governing_law":"CN","source":"issuer_default"}`
	for _, c := range []struct {
		name, legal string
		want        quote.Formation
	}{
		{"German law with the CISG", hamburg, quote.Formation{Rule: quote.FormationArrival, Basis: quote.FormationBasisCISG}},
		{"PRC law without the CISG", prc, quote.Formation{Rule: quote.FormationArrival, Basis: quote.FormationBasisPRC}},
		{"no legal terms", "", quote.DefaultFormation},
	} {
		q, hash := sealedUnder(c.legal)
		a, err := verifiedUnder(t, q, hash, "r1", arrived)
		if err != nil || a.Formation != c.want || !a.Formed() || !a.ArrivalAt.Equal(arrived) || a.Check("quote.1", 1, hash, "/x") != nil {
			t.Fatalf("%s: %+v %v", c.name, a, err)
		}
		q, hash = sealedUnder(c.legal)
		if _, err = verifiedUnder(t, q, hash, "r2", expires); code(err) != "ACCEPTANCE_LATE /args/response_id" {
			t.Fatalf("%s: an arrival at expires_at: %s", c.name, code(err))
		}
	}
	dispatch := `{"cisg":"excluded","dispute_resolution":{"court":"High Court of Justice","method":"litigation"},` +
		`"formation":{"basis":"common_law","rule":"dispatch"},"governing_law":"GB-ENG","source":"quote"}`
	q, hash := sealedUnder(dispatch)
	if _, err := verifiedUnder(t, q, hash, "r3", arrived); code(err) != "DEPENDENCY_NOT_SUPPORTED /args/business_revision" {
		t.Fatalf("a rule Brant does not implement: %s", code(err))
	}
	// A stored acceptance of another rule never stands for a formed contract.
	a := &Acceptance{QuoteID: "quote.1", Revision: 1, BusinessHash: hash, ArrivalAt: arrived, ExpiresAt: expires, Effectivity: Valid,
		Formation: quote.Formation{Rule: quote.FormationDispatch, Basis: "common_law"}}
	if a.Formed() || a.Code() != CodeAcceptanceLate {
		t.Fatalf("formed under dispatch: %v %s", a.Formed(), a.Code())
	}
}
