//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The legal terms suite (WBS S3-17; the simulated G0 walkthrough's decision F-02, the condition of
// ADR-BR-013; SIM-02 step 3) on the TRD world: POL publishes legal terms like every configuration
// kind; the tenant's default — the CISG with PRC law, CIETAC arbitration in Shanghai — is the legal
// terms company.A's profile names (revision 2 of the profile); quotes print them with the
// quote-text/0.2.0 template quote.legal. "editor" drafts, freezes, activates and records the
// customer's replies, "reviewer" publishes POL revisions, approves and verifies.

var (
	cietac = polapi.DisputeResolution{Method: polapi.DisputeArbitration,
		Institution: "China International Economic and Trade Arbitration Commission (CIETAC)", Seat: "Shanghai", Language: "en"}
	hamburgArbitration = polapi.DisputeResolution{Method: polapi.DisputeArbitration,
		Institution: "Court of Arbitration of the Hamburg Chamber of Commerce", Seat: "Hamburg", Language: "de"}
)

func termsData(name, law, cisg, rule string, d polapi.DisputeResolution) polapi.ConfigData {
	return polapi.ConfigData{LegalTerms: &polapi.LegalTerms{Name: name, GoverningLaw: law, CISG: cisg, FormationRule: rule, DisputeResolution: d}}
}

type legalWorld struct {
	*trdWorld
	def, issuer, layout polapi.ConfigRef
}

// newLegalWorld is the TRD world with the tenant's default legal terms legal.default named by
// company.A's profile revision 2, and the quote-text/0.2.0 template quote.legal.
func newLegalWorld(t *testing.T) *legalWorld { return legalOn(newTrdWorld(t)) }

// legalOn adds the legal terms setup of newLegalWorld to the TRD world q.
func legalOn(q *trdWorld) *legalWorld {
	l := &legalWorld{trdWorld: q}
	ed, rv, win := q.author, q.reviewer, q.win
	l.def = q.configure(ed, rv, polapi.KindLegalTerms, "legal.default", 0, win.from, time.Time{},
		termsData("CISG with PRC law", polapi.GoverningLawCN, polapi.CISGApplies, polapi.FormationArrival, cietac), true)
	profile := companyData("USD")
	profile.CompanyProfile.DefaultLegalTerms = &l.def
	l.issuer = q.configure(ed, rv, polapi.KindCompanyProfile, "company.A", q.rootVersion(polapi.KindCompanyProfile, "company.A"), win.from,
		time.Time{}, profile, true)
	l.layout = q.configure(ed, rv, polapi.KindQuoteDocumentTemplate, "quote.legal", 0, win.from, time.Time{}, polapi.ConfigData{
		QuoteDocumentTemplate: &polapi.QuoteDocumentTemplate{Name: "formal with legal terms", Language: polapi.LanguageEn,
			RendererVersion: polapi.QuoteRendererVersion020, Title: "Quotation", Sections: polapi.QuoteSectionsFor(polapi.QuoteRendererVersion020),
			FooterNote: "合成测试，不构成要约"}}, true)
	return l
}

// rootVersion is the current root version of POL configuration kind:id.
func (q *trdWorld) rootVersion(kind, id string) int64 {
	q.t.Helper()
	v, err := q.c.POL.GetConfig(q.author, kind, id)
	q.must(err)
	return v.RootVersion
}

// draft is the fixture's draft issued by company.A's profile revision 2, printed by quote.legal.
func (l *legalWorld) draft() trdapi.QuoteData {
	d := l.data()
	d.IssuerRef, d.DocumentTemplateRef = trefp(l.issuer), trefp(l.layout)
	return d
}

// commercial is the sealed commercial document of revision rev of quote id.
func (l *legalWorld) commercial(id string, rev int64) map[string]any {
	l.t.Helper()
	for _, r := range l.getQuote(id).Revisions {
		if r.Revision == rev {
			var doc map[string]any
			l.must(json.Unmarshal([]byte(r.Commercial), &doc))
			return doc
		}
	}
	l.t.Fatalf("quote %s has no revision %d", id, rev)
	return nil
}

// manifested reports whether doc's dependency manifest pins ref with content hash.
func manifested(doc map[string]any, ref polapi.ConfigRef, hash string) bool {
	for _, d := range doc["dependency_manifest"].([]any) {
		e := d.(map[string]any)
		if e["kind"] == ref.Kind && e["id"] == ref.ConfigID && e["version"] == float64(ref.Revision) && e["content_hash"] == hash {
			return true
		}
	}
	return false
}

func (l *legalWorld) contentHash(ref polapi.ConfigRef) string {
	l.t.Helper()
	v, err := l.c.POL.GetConfig(l.author, ref.Kind, ref.ConfigID)
	l.must(err)
	for _, r := range v.Revisions {
		if r.Revision == ref.Revision {
			return r.ContentHash
		}
	}
	l.t.Fatalf("no revision %+v", ref)
	return ""
}

// SIM-02 step 3 (PO 条款约定适用德国法与汉堡仲裁; WBS S3-17 acceptance: 合同成立规则按适用法律选择；
// 默认 CISG 与中国法；SIM-02 第 3 步可表达), with steps 1 and 2 around it. Quote quote.b2 to the German
// importer is frozen without legal terms of its own: it seals the tenant's default (the CISG with
// PRC law, source issuer_default), binds its revision in the manifest and prints it. The importer's
// PO states German law and Hamburg arbitration: recorded as a conditional reply, a counter-offer
// (different dispute settlement terms alter an offer materially, CISG Art. 19(3)), which is never
// verified into an acceptance. Legal publishes the terms; revision 2 names them, is frozen —
// sealing German law with the CISG and the arrival rule on it — approved and replaces revision 1,
// and its formal document (quote-text/0.2.0) prints them. The importer's acceptance of revision 2,
// arrived and verified, forms the contract under the arrival rule (CISG Art. 18(2), 23), which the
// acceptance, its event, its reads and its lookup record.
func TestSIM02GermanLawAndHamburgArbitrationFormTheContractByArrival(t *testing.T) {
	l := newLegalWorld(t)
	ed, rv := l.author, l.reviewer
	const id = "quote.b2"

	_, err := l.saveQuote(ed, id, 0, l.draft())
	l.must(err)
	f1, err := l.freezeQuote(ed, id, 1, 1)
	l.must(err)
	doc := l.commercial(id, 1)
	legal, _ := doc["legal_terms"].(map[string]any)
	if legal["source"] != "issuer_default" || legal["governing_law"] != "CN" || legal["cisg"] != "applies" ||
		legal["formation"].(map[string]any)["basis"] != trdapi.FormationBasisCISG ||
		doc["data"].(map[string]any)["legal_terms_ref"].(map[string]any)["id"] != "legal.default" ||
		!manifested(doc, l.def, l.contentHash(l.def)) {
		t.Fatalf("revision 1 seals the tenant's default: %v", legal)
	}
	if v := l.getQuote(id); v.Revisions[0].Data.LegalTermsRef != nil {
		t.Fatal("the draft keeps what it said: no legal terms of its own")
	}
	_, err = l.submit(ed, id, f1.RowVersion, 1, "review.1")
	l.must(err)
	d1, err := l.c.TRD.DecideReview(rv, l.approval(id, f1.RowVersion+1, "review.1", f1.BusinessHash, "approval.1"))
	l.must(err)
	a1, err := l.c.TRD.Activate(ed, l.activateCmd(id, d1.RowVersion, 1, "approval.1"))
	l.must(err)
	formal, err := l.c.TRD.GetFormalDocument(ed, id, 1)
	l.must(err)
	if !strings.Contains(string(formal.Content), "\n\nLegal terms: legal.default r1\nGoverning law: CN | CISG: applies\n"+
		"Contract formation: acceptance effective on arrival | CISG Art. 18(2), 23\n"+
		"Disputes: arbitration | China International Economic and Trade Arbitration Commission (CIETAC) | seat Shanghai | language en\n") ||
		formal.Formal.RendererVersion != polapi.QuoteRendererVersion020 {
		t.Fatalf("revision 1's formal document:\n%s", formal.Content)
	}

	// Steps 1 and 3: the importer's PO changes a colour and states German law and Hamburg arbitration.
	l.mail()
	po := l.reply("response.po", id, 1, a1.BusinessHash, trdapi.ClassificationConditional, time.Now(), trdapi.ArrivalChannelServer)
	po.CustomerPONumber = "B2-PO-2026-0042"
	po.Summary = "客户 PO：一个颜色改为藏青；PO 条款约定适用德国法与汉堡仲裁（争议解决条款不同，CISG 第 19(3) 条：实质变更），构成反要约"
	_, err = l.c.TRD.RecordResponse(ed, po)
	l.must(err)
	if _, err = l.c.TRD.VerifyResponse(rv, l.verification("response.po", "acceptance.po", 1, a1.BusinessHash)); code(err) !=
		trdapi.CodeAcceptanceNotUnconditional {
		t.Fatalf("a counter-offer verified: %s", code(err))
	}

	// Legal publishes the PO's terms; revision 2 names them and replaces revision 1 (step 2).
	hamburg := l.configure(ed, rv, polapi.KindLegalTerms, "legal.de-hamburg", 0, l.win.from, time.Time{},
		termsData("German law with the CISG, Hamburg arbitration", "DE", polapi.CISGApplies, polapi.FormationArrival, hamburgArbitration), true)
	r, err := l.c.TRD.Revise(ed, trdapi.Revise{IdempotencyKey: l.key(), QuoteID: id, ExpectedVersion: a1.RowVersion, BaseRevision: 1})
	l.must(err)
	d2 := l.draft()
	d2.LegalTermsRef = trefp(hamburg)
	saved, err := l.saveQuote(ed, id, r.RowVersion, d2)
	l.must(err)
	f2, err := l.freezeQuote(ed, id, saved.RowVersion, 2)
	l.must(err)
	doc = l.commercial(id, 2)
	legal = doc["legal_terms"].(map[string]any)
	dispute := legal["dispute_resolution"].(map[string]any)
	if legal["source"] != "quote" || legal["governing_law"] != "DE" || legal["cisg"] != "applies" ||
		legal["formation"].(map[string]any)["rule"] != trdapi.FormationArrival || legal["formation"].(map[string]any)["basis"] != trdapi.FormationBasisCISG ||
		dispute["seat"] != "Hamburg" || dispute["institution"] != hamburgArbitration.Institution || dispute["language"] != "de" ||
		!manifested(doc, hamburg, l.contentHash(hamburg)) || manifested(doc, l.def, l.contentHash(l.def)) || f2.BusinessHash == f1.BusinessHash {
		t.Fatalf("revision 2 seals German law and Hamburg arbitration: %v", legal)
	}
	_, err = l.c.TRD.SubmitReview(ed, trdapi.SubmitReview{IdempotencyKey: l.key(), QuoteID: id, ExpectedVersion: f2.RowVersion, BusinessRevision: 2,
		ReviewRequestID: "review.2", TargetCommand: trdapi.TargetReplace, ExpectedActiveRevision: 1})
	l.must(err)
	d, err := l.c.TRD.DecideReview(rv, l.approval(id, f2.RowVersion+1, "review.2", f2.BusinessHash, "approval.2"))
	l.must(err)
	replaced, err := l.c.TRD.Replace(ed, l.replaceCmd(id, d.RowVersion, 2, "approval.2"))
	l.must(err)
	if replaced.ReplacedRevision != 1 || !slices.Equal(l.statuses(id), []string{trdapi.StatusSuperseded, trdapi.StatusActive}) {
		t.Fatalf("replace %+v %v", replaced, l.statuses(id))
	}
	formal, err = l.c.TRD.GetFormalDocument(ed, id, 2)
	l.must(err)
	if !strings.HasSuffix(string(formal.Content), "\n\nLegal terms: legal.de-hamburg r1\nGoverning law: DE | CISG: applies\n"+
		"Contract formation: acceptance effective on arrival | CISG Art. 18(2), 23\n"+
		"Disputes: arbitration | Court of Arbitration of the Hamburg Chamber of Commerce | seat Hamburg | language de\n") {
		t.Fatalf("revision 2's formal document:\n%s", formal.Content)
	}
	if v := l.getQuote(id); v.Revisions[1].Data.LegalTermsRef == nil || *v.Revisions[1].Data.LegalTermsRef != tref(hamburg) {
		t.Fatalf("revision 2's draft %+v", v.Revisions[1].Data.LegalTermsRef)
	}

	// The importer accepts revision 2: the contract forms when the acceptance arrived.
	arrived := time.Now()
	_, err = l.c.TRD.RecordResponse(ed, l.reply("response.accept", id, 2, f2.BusinessHash, trdapi.ClassificationFullUnconditional, arrived,
		trdapi.ArrivalChannelServer))
	l.must(err)
	v, err := l.c.TRD.VerifyResponse(rv, l.verification("response.accept", "acceptance.b2", 2, f2.BusinessHash))
	l.must(err)
	if !v.Formed || !v.ArrivalAt.Equal(kernel.Instant(arrived)) || v.ArrivalBasis != trdapi.ArrivalChannelServer ||
		v.FormationRule != trdapi.FormationArrival || v.FormationBasis != trdapi.FormationBasisCISG {
		t.Fatalf("the acceptance %+v", v)
	}
	got, err := l.c.TRD.GetAcceptance(ed, "acceptance.b2")
	l.must(err)
	fact := l.fact("acceptance.b2")
	if got.FormationRule != trdapi.FormationArrival || got.FormationBasis != trdapi.FormationBasisCISG || !got.Formed ||
		fact.FormationBasis != trdapi.FormationBasisCISG || fact.Code != "" || !fact.Formed {
		t.Fatalf("reads %+v, lookup %+v", got, fact)
	}
	verified := l.eventsOf(trdapi.EventAcceptanceVerified)
	if len(verified) != 1 || verified[0].Payload["formation_rule"] != trdapi.FormationArrival ||
		verified[0].Payload["formation_basis"] != trdapi.FormationBasisCISG || verified[0].Payload["formed"] != true {
		t.Fatalf("event %+v", verified)
	}
	if g := l.acceptanceGuard(bindingOf(v)); !g.Usable() || !g.Formed {
		t.Fatalf("the guard %+v", g)
	}
}

// WBS S3-17: a quote is frozen only under legal terms whose contract-formation rule Brant
// implements, printed by a template that prints them. POL publishes legal terms under any law (it
// records the tenant's terms) but refuses a rule the CISG or PRC law contradicts; the freeze refuses
// English law with the postal rule and German law without the CISG with DEPENDENCY_NOT_SUPPORTED at
// the reference — never treating them as arrival — as it refuses such terms as the issuer's default
// at issuer_ref; legal terms with a quote-text/0.1.0 template are DEPENDENCY_NOT_SUPPORTED at the
// template, a quote-text/0.2.0 template without any legal terms DRAFT_NOT_READY. Nothing is frozen.
func TestFreezeRefusesLegalTermsWhoseFormationBrantDoesNotImplement(t *testing.T) {
	l := newLegalWorld(t)
	ed, rv, win := l.author, l.reviewer, l.win
	_, err := l.saveConfig(ed, polapi.KindLegalTerms, "legal.cn-dispatch", 0, win.from, time.Time{},
		termsData("PRC law, dispatch", polapi.GoverningLawCN, polapi.CISGExcluded, polapi.FormationDispatch, cietac))
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/data/formation_rule" {
		t.Fatalf("PRC law forms by arrival: %s at %s", code(err), path(err))
	}
	england := l.configure(ed, rv, polapi.KindLegalTerms, "legal.england", 0, win.from, time.Time{},
		termsData("English law", "GB-ENG", polapi.CISGExcluded, polapi.FormationDispatch,
			polapi.DisputeResolution{Method: polapi.DisputeLitigation, Court: "High Court of Justice of England and Wales"}), true)
	germany := l.configure(ed, rv, polapi.KindLegalTerms, "legal.de-no-cisg", 0, win.from, time.Time{},
		termsData("German law without the CISG", "DE", polapi.CISGExcluded, polapi.FormationArrival, hamburgArbitration), true)
	for i, c := range []struct {
		name string
		edit func(d *trdapi.QuoteData)
		at   string
		want string
	}{
		{"English law and the postal rule", func(d *trdapi.QuoteData) { d.LegalTermsRef = trefp(england) }, "/args/data/legal_terms_ref",
			trdapi.CodeDependencyNotSupported},
		{"German law without the CISG", func(d *trdapi.QuoteData) { d.LegalTermsRef = trefp(germany) }, "/args/data/legal_terms_ref",
			trdapi.CodeDependencyNotSupported},
		{"the default printed by quote-text/0.1.0", func(d *trdapi.QuoteData) { d.DocumentTemplateRef = trefp(l.template) },
			"/args/data/document_template_ref", trdapi.CodeDependencyNotSupported},
	} {
		d := l.draft()
		c.edit(&d)
		l.refusedFreeze(c.name, "quote.refused."+string(rune('a'+i)), d, c.want, c.at)
	}
	// The issuer's default under English law; an issuer naming no default, with quote-text/0.2.0.
	reissue := func(def *polapi.ConfigRef) polapi.ConfigRef {
		profile := companyData("USD")
		profile.CompanyProfile.DefaultLegalTerms = def
		return l.configure(ed, rv, polapi.KindCompanyProfile, "company.A", l.rootVersion(polapi.KindCompanyProfile, "company.A"), win.from,
			time.Time{}, profile, true)
	}
	d := l.draft()
	d.IssuerRef = trefp(reissue(&england))
	l.refusedFreeze("an English default", "quote.refused.default", d, trdapi.CodeDependencyNotSupported, "/args/data/issuer_ref")
	d.IssuerRef = trefp(reissue(nil))
	l.refusedFreeze("quote-text/0.2.0 without legal terms", "quote.refused.none", d, kernel.CodeDraftNotReady, "/args/data/legal_terms_ref")
	if n := len(l.frozenEvents()); n != 0 {
		t.Fatalf("%d quotes frozen", n)
	}
}

// refusedFreeze saves d as quote id and wants its freeze refused with code want at path at.
func (l *legalWorld) refusedFreeze(name, id string, d trdapi.QuoteData, want, at string) {
	l.t.Helper()
	_, err := l.saveQuote(l.author, id, 0, d)
	l.must(err)
	if _, err = l.freezeQuote(l.author, id, 1, 1); code(err) != want || path(err) != at {
		l.t.Fatalf("%s: %s at %s, want %s at %s", name, code(err), path(err), want, at)
	}
}

// ConfigGuard v1 covers legal terms like every POL revision a quote adopts (Context Map R15): while
// a freeze holds the guard on its legal terms, their retirement waits until the freeze commits;
// then a freeze naming them is refused CONFIG_NOT_ACTIVE at legal_terms_ref, and so is the
// activation of the revision frozen first, through the dependency manifest, with no number issued.
// In a transaction of TRD's own, the guard answers in force for the published revision and
// CONFIG_NOT_ACTIVE after the retirement, holding the revision row FOR SHARE.
func TestLegalTermsAreGuardedLikeEveryConfiguration(t *testing.T) {
	l := newLegalWorld(t)
	ed, rv := l.author, l.reviewer
	hamburg := l.configure(ed, rv, polapi.KindLegalTerms, "legal.de-hamburg", 0, l.win.from, time.Time{},
		termsData("German law with the CISG, Hamburg arbitration", "DE", polapi.CISGApplies, polapi.FormationArrival, hamburgArbitration), true)
	guard := func() polapi.ConfigGuardVerdict {
		var v polapi.ConfigGuardVerdict
		l.must(l.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
			var err error
			v, err = polapi.ScanConfigGuard(hamburg, exec.QueryRow(ctx, polapi.ConfigGuardSQL, polapi.ConfigGuardArgs(hamburg, time.Now())...).Scan)
			return err
		}))
		return v
	}
	if v := guard(); !v.InForce() || v.ContentHash != l.contentHash(hamburg) {
		t.Fatalf("the guard %+v", v)
	}
	d := l.draft()
	d.LegalTermsRef = trefp(hamburg)
	for _, id := range []string{"quote.1", "quote.2", "quote.3"} {
		_, err := l.saveQuote(ed, id, 0, d)
		l.must(err)
	}
	f, err := l.freezeQuote(ed, "quote.3", 1, 1)
	l.must(err)
	_, err = l.submit(ed, "quote.3", f.RowVersion, 1, "review.3")
	l.must(err)
	decided, err := l.c.TRD.DecideReview(rv, l.approval("quote.3", f.RowVersion+1, "review.3", f.BusinessHash, "approval.3"))
	l.must(err)

	down := newDownstream(l.world)
	cmd := trdapi.Freeze{IdempotencyKey: "freeze.held", QuoteID: "quote.1", ExpectedVersion: 1, WorkingRevision: 1}
	blocker, release := down.keyBlocker(t, "brant_trd", trdapi.CommandFreeze, cmd.IdempotencyKey)
	frozen := make(chan error, 1)
	go func() {
		_, err := l.c.TRD.Freeze(ed, cmd)
		frozen <- err
	}()
	if !awaitInHold(t, "the freeze holds its guards and waits at its action result", func() bool { return down.blocks(blocker) }, frozen) {
		release()
		return
	}
	freeze := blockedBy(t, blocker)[0]
	retired := make(chan timed, 1)
	retireKey := l.key()
	go func() {
		_, err := l.c.POL.Retire(rv, polapi.Retire{IdempotencyKey: retireKey, Kind: polapi.KindLegalTerms, ConfigID: "legal.de-hamburg",
			ExpectedVersion: 2, Reason: "法务撤回该条款"})
		retired <- timed{time.Now(), err}
	}()
	ok := awaitInHold(t, "the retirement waits on the freeze", func() bool { return len(blockedBy(t, freeze)) == 1 }, retired)
	released := time.Now()
	release()
	if !ok {
		return
	}
	if err = <-frozen; err != nil {
		t.Fatalf("the freeze holding the guard: %v", err)
	}
	if r := <-retired; r.err != nil || !r.at.After(released) {
		t.Fatalf("the retirement %v, after the freeze %v", r.err, r.at.After(released))
	}
	if _, err = l.freezeQuote(ed, "quote.2", 1, 1); code(err) != polapi.CodeConfigNotActive || path(err) != "/args/data/legal_terms_ref" {
		t.Fatalf("a freeze after the retirement: %s at %s", code(err), path(err))
	}
	_, _, numbers := l.formalRows()
	if _, err = l.c.TRD.Activate(ed, l.activateCmd("quote.3", decided.RowVersion, 1, "approval.3")); code(err) != polapi.CodeConfigNotActive ||
		!strings.HasSuffix(path(err), "/legal_terms_ref") {
		t.Fatalf("an activation after the retirement: %s at %s", code(err), path(err))
	}
	if _, _, after := l.formalRows(); after != numbers {
		t.Fatal("a refused activation issued a number")
	}
	if v := guard(); v.InForce() || v.Code != polapi.CodeConfigNotActive {
		t.Fatalf("the guard after the retirement %+v", v)
	}
}

// acceptedWith saves d as quote id, freezes it, has it approved for quote.activate and activates
// it; then the customer's full acceptance, arrived now, is recorded and verified as
// "acceptance.<id>".
func (l *legalWorld) acceptedWith(id string, d trdapi.QuoteData) trdapi.AcceptanceReceipt {
	l.t.Helper()
	_, err := l.saveQuote(l.author, id, 0, d)
	l.must(err)
	f, err := l.freezeQuote(l.author, id, 1, 1)
	l.must(err)
	_, err = l.submit(l.author, id, f.RowVersion, 1, "review."+id)
	l.must(err)
	decided, err := l.c.TRD.DecideReview(l.reviewer, l.approval(id, f.RowVersion+1, "review."+id, f.BusinessHash, "approval."+id))
	l.must(err)
	a, err := l.c.TRD.Activate(l.author, l.activateCmd(id, decided.RowVersion, 1, "approval."+id))
	l.must(err)
	_, err = l.c.TRD.RecordResponse(l.author, l.full("response."+id, id, 1, a.BusinessHash))
	l.must(err)
	v, err := l.c.TRD.VerifyResponse(l.reviewer, l.verification("response."+id, "acceptance."+id, 1, a.BusinessHash))
	l.must(err)
	return v
}

// sourceOf is the source quote snapshot order id's snapshot embeds.
func (l *legalWorld) sourceOf(id string) map[string]any {
	l.t.Helper()
	var snapshot map[string]any
	l.must(json.Unmarshal([]byte(l.getOrder(id).Snapshot), &snapshot))
	return snapshot["source_quote_snapshot"].(map[string]any)
}

// WBS S3-17 with S3-07: the legal terms reach the order. An order prepared from a revision with
// legal terms — its own (German law with the CISG, Hamburg arbitration) or its issuer's default (the
// CISG with PRC law) — embeds the sealed commercial document whole as source_quote_snapshot
// (brant.order-commercial-snapshot/0.1.0): legal_terms, data.legal_terms_ref and the manifest entry
// with its content hash; the acceptance's formation is arrival on the CISG. quote_ready and the
// order's check, review and activation re-check the legal terms through ConfigGuard v1 like every
// POL revision the manifest binds: once legal retires them, quote_ready answers CONFIG_NOT_ACTIVE at
// /data/legal_terms_ref (for the issuer's default too, where the document binds it), and the order
// carries that as a risk its review must acknowledge (ADR-BR-013: an upstream change does not undo
// a formed contract); acknowledged, the order activates with the legal terms it was prepared with.
func TestOrderCarriesTheLegalTermsAndRechecksThem(t *testing.T) {
	l := legalOn(newOrderWorld(t))
	hamburg := l.configure(l.author, l.reviewer, polapi.KindLegalTerms, "legal.de-hamburg", 0, l.win.from, time.Time{},
		termsData("German law with the CISG, Hamburg arbitration", "DE", polapi.CISGApplies, polapi.FormationArrival, hamburgArbitration), true)
	own := l.draft()
	own.LegalTermsRef = trefp(hamburg)
	a1 := l.acceptedWith("quote.1", own)
	a2 := l.acceptedWith("quote.2", l.draft())
	if a1.FormationBasis != trdapi.FormationBasisCISG || a2.FormationBasis != trdapi.FormationBasisCISG || !a1.Formed || !a2.Formed {
		t.Fatalf("formations %+v %+v", a1, a2)
	}
	o1, err := l.c.TRD.PrepareOrder(l.author, l.prepareCmd("order.1", a1))
	l.must(err)
	_, err = l.c.TRD.PrepareOrder(l.author, l.prepareCmd("order.2", a2))
	l.must(err)
	for _, c := range []struct {
		order, quote string
		terms        polapi.ConfigRef
		source       string
	}{{"order.1", "quote.1", hamburg, "quote"}, {"order.2", "quote.2", l.def, "issuer_default"}} {
		src, sealed := l.sourceOf(c.order), l.commercial(c.quote, 1)
		legal, _ := src["legal_terms"].(map[string]any)
		if legal == nil || !reflect.DeepEqual(legal, sealed["legal_terms"]) || legal["source"] != c.source ||
			src["data"].(map[string]any)["legal_terms_ref"].(map[string]any)["id"] != c.terms.ConfigID ||
			!manifested(src, c.terms, l.contentHash(c.terms)) {
			t.Fatalf("%s carries %v", c.order, legal)
		}
	}

	// Legal retires both terms after the contracts formed.
	for _, ref := range []polapi.ConfigRef{hamburg, l.def} {
		_, err = l.c.POL.Retire(l.reviewer, polapi.Retire{IdempotencyKey: l.key(), Kind: ref.Kind, ConfigID: ref.ConfigID,
			ExpectedVersion: l.rootVersion(ref.Kind, ref.ConfigID), Reason: "法务停用条款"})
		l.must(err)
	}
	lapsed := trdapi.Risk{Path: "/data/legal_terms_ref", Code: polapi.CodeConfigNotActive}
	for _, c := range []struct{ order, quote string }{{"order.1", "quote.1"}, {"order.2", "quote.2"}} {
		ready, err := l.c.TRD.QuoteReady(l.author, trdapi.ReadyQuery{QuoteID: c.quote})
		l.must(err)
		if ready.Ready || !slices.Contains(ready.Reasons, trdapi.Reason{Check: trdapi.CheckUpstream, Code: polapi.CodeConfigNotActive,
			Path: "/data/legal_terms_ref"}) {
			t.Fatalf("%s ready %+v", c.quote, ready.Reasons)
		}
		if risks := l.risks(c.order); len(risks) != 1 || risks[0] != lapsed {
			t.Fatalf("%s risks %+v", c.order, risks)
		}
	}
	r, err := l.c.TRD.RequestOrderReview(l.author, l.requestCmd(o1.ObjectID, l.orderVersion(o1.ObjectID), "review.order.1"))
	l.must(err)
	_, err = l.c.TRD.DecideOrderReview(l.reviewer, l.decideCmd(o1.ObjectID, r.RowVersion, "review.order.1", o1.OrderHash))
	if code(err) != trdapi.CodeOrderRiskUnacknowledged {
		t.Fatalf("an approval ignoring the lapsed legal terms: %s", code(err))
	}
	d, err := l.c.TRD.DecideOrderReview(l.reviewer, l.decideCmd(o1.ObjectID, r.RowVersion, "review.order.1", o1.OrderHash, ack(lapsed)))
	l.must(err)
	l.readyToActivate(o1.ObjectID)
	_, err = l.c.TRD.ActivateOrder(l.author, l.activateOrderCmd(o1.ObjectID, d.RowVersion, "review.order.1"))
	l.must(err)
	if v := l.getOrder(o1.ObjectID); v.Status != trdapi.OrderActive || v.OrderHash != o1.OrderHash ||
		l.sourceOf(o1.ObjectID)["legal_terms"].(map[string]any)["governing_law"] != "DE" {
		t.Fatalf("the activated order %+v", v)
	}
}
