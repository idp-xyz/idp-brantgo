package quote

import (
	"slices"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Legal terms and contract formation by governing law (WBS S3-17; the simulated G0 walkthrough's
// decision F-02, the condition ADR-BR-013 was accepted on; UL TRD-ContractFormation).
//
// A quote revision names POL legal terms (legal_terms_ref, kind legal_terms) or, when it names
// none, the freeze adopts the default its issuer's company profile names (CompanyProfile
// default_legal_terms; the design's default is the CISG with PRC law, configured by the tenant).
// The freeze binds the revision like every POL reference (dependency manifest, ConfigGuard v1 at
// the freeze and at every formalization) and seals the terms' content into the commercial document
// as legal_terms, with the contract-formation rule they imply (Formation). A revision that seals no
// legal terms (its draft names none and its issuer names no default) has no legal_terms member and
// no legal_terms_ref, so its document and business hash are what they were before legal terms
// existed; every revision frozen before them is such a revision.
//
// Formation: Brant implements one rule, arrival — an acceptance takes effect, and the contract is
// concluded, when it reaches the offeror — and it implements it where the law it cites fixes it:
// when the CISG applies (CISG Art. 18(2), 23) and under PRC law (Civil Code Art. 137, 483, 484). A
// revision without legal terms forms under the design's default, the CISG with PRC law: arrival
// too. Legal terms under another law without the CISG, or stating another rule, are refused at the
// freeze with DEPENDENCY_NOT_SUPPORTED (an undelivered dependency path, error catalog): never
// treated as arrival. The acceptance ledger judges every acceptance by the formation its revision
// seals (package acceptance).
//
// What downstream documents carry (order wave, export contract and PI): an order snapshot copies
// the source revision's legal_terms member and its legal_terms_ref with the manifest's content
// hash, and the acceptance's formation, from the sealed commercial document — never from POL's
// current revision, which may have changed since; an order amendment that changes the legal terms
// is a change of contract terms. An S/C or PI prints the same governing law, CISG choice, dispute
// resolution and language as the quote revision's formal document did.

// POL values of legal terms (POL api legal_terms).
const (
	CISGApplies        = "applies"
	CISGExcluded       = "excluded"
	GoverningLawCN     = "CN"
	DisputeArbitration = "arbitration"
	DisputeLitigation  = "litigation"

	// FormationArrival: an acceptance takes effect when it reaches the offeror; FormationDispatch:
	// when it is sent (POL's other rule, which Brant does not implement).
	FormationArrival  = "arrival"
	FormationDispatch = "dispatch"

	// Bases of a formation: the CISG, PRC law, or the design's default for a revision without legal
	// terms (the CISG with PRC law).
	FormationBasisCISG    = "cisg"
	FormationBasisPRC     = "prc_civil_code"
	FormationBasisDefault = "default"

	// Where sealed legal terms come from: the quote's own reference, or its issuer's default.
	LegalSourceQuote         = "quote"
	LegalSourceIssuerDefault = "issuer_default"
)

// QuoteRendererV020 is the renderer of quotes with legal terms: quote-text/0.1.0 and a legal_terms
// section (POL QuoteRendererVersion020).
const QuoteRendererV020 = "quote-text/0.2.0"

// Legal is what the freeze reads of a POL legal_terms revision.
type Legal struct {
	GoverningLaw      string
	CISG              string
	FormationRule     string
	DisputeResolution DisputeResolution
}

// DisputeResolution is arbitration (institution, seat, language) or litigation (court), as POL
// states it.
type DisputeResolution struct {
	Method      string `json:"method"`
	Institution string `json:"institution,omitempty"`
	Seat        string `json:"seat,omitempty"`
	Language    string `json:"language,omitempty"`
	Court       string `json:"court,omitempty"`
}

// Formation is the contract-formation rule an acceptance of a revision is judged by and the law it
// rests on.
type Formation struct {
	Rule  string `json:"rule"`
	Basis string `json:"basis"`
}

// DefaultFormation is the formation of a revision that seals no legal terms: arrival, under the
// design's default of the CISG with PRC law (ADR-BR-013 as accepted).
var DefaultFormation = Formation{Rule: FormationArrival, Basis: FormationBasisDefault}

// Check refuses a formation Brant does not implement (DEPENDENCY_NOT_SUPPORTED at path): only the
// arrival rule, on one of the three bases.
func (f Formation) Check(path string) error {
	if f.Rule != FormationArrival || !slices.Contains([]string{FormationBasisCISG, FormationBasisPRC, FormationBasisDefault}, f.Basis) {
		return fail(CodeDependencyNotSupported, path, "Brant forms contracts by the arrival of the acceptance under the CISG or PRC law only")
	}
	return nil
}

// LegalTerms are the legal terms a commercial document seals (its legal_terms member): where they
// come from, the governing law, whether the CISG applies, the dispute resolution, and the formation
// they imply. The revision and its content hash are in data.legal_terms_ref and the dependency
// manifest.
type LegalTerms struct {
	Source            string            `json:"source"`
	GoverningLaw      string            `json:"governing_law"`
	CISG              string            `json:"cisg"`
	DisputeResolution DisputeResolution `json:"dispute_resolution"`
	Formation         Formation         `json:"formation"`
}

// FormationUnder decides the formation of legal terms l (path: where the reference sits): arrival
// on the CISG when it applies, else on PRC law when that governs; DEPENDENCY_NOT_SUPPORTED for
// another rule or another law without the CISG, whose rule Brant does not implement.
func FormationUnder(l Legal, path string) (Formation, error) {
	if l.FormationRule != FormationArrival {
		return Formation{}, fail(CodeDependencyNotSupported, path,
			"The legal terms state formation by "+l.FormationRule+"; Brant forms contracts by the arrival of the acceptance only")
	}
	switch {
	case l.CISG == CISGApplies:
		return Formation{Rule: FormationArrival, Basis: FormationBasisCISG}, nil
	case l.GoverningLaw == GoverningLawCN:
		return Formation{Rule: FormationArrival, Basis: FormationBasisPRC}, nil
	}
	return Formation{}, fail(CodeDependencyNotSupported, path,
		"Brant implements contract formation under the CISG and under PRC law, not under "+l.GoverningLaw+" without the CISG")
}

// Formation returns the formation the commercial document seals: its legal terms', or
// DefaultFormation when it has none.
func (c Commercial) Formation() Formation {
	if c.LegalTerms == nil {
		return DefaultFormation
	}
	return c.LegalTerms.Formation
}

// Formation returns the formation of the sealed revision (BUSINESS_HASH_MISMATCH for a corrupt
// document).
func (s *Seal) Formation() (Formation, error) {
	c, err := ParseCommercial(s.Commercial, s.BusinessHash)
	if err != nil {
		return Formation{}, err
	}
	return c.Formation(), nil
}

// legalTerms decides the legal terms a freeze of complete draft d seals, after its references were
// found known and in effect: the draft's own, else its issuer's default (which must be known and in
// effect, refused at issuer_ref), else none. It returns the revision adopted (nil: none) and the
// content sealed, with the formation they imply (DEPENDENCY_NOT_SUPPORTED for a rule or law Brant
// does not implement), and pairs them with the template: quote-text/0.1.0 prints no legal terms
// and quote-text/0.2.0 prints them, so terms without a 0.2.0 template are DEPENDENCY_NOT_SUPPORTED
// and a 0.2.0 template without terms DRAFT_NOT_READY at legal_terms_ref.
func legalTerms(d Data, f Facts) (*Ref, *LegalTerms, error) {
	const p = "/args/data"
	ref, path, source := d.LegalTerms, p+"/legal_terms_ref", LegalSourceQuote
	if ref == nil {
		ref, path, source = f.Configs[*d.Issuer].DefaultLegalTerms, p+"/issuer_ref", LegalSourceIssuerDefault
	}
	renderer := f.Configs[*d.DocumentTemplate].Renderer
	if ref == nil {
		if renderer == QuoteRendererV020 {
			return nil, nil, kernel.Reject(kernel.CodeDraftNotReady, p+"/legal_terms_ref", "The template prints legal terms: name them, or the issuer's default")
		}
		return nil, nil, nil
	}
	c, ok := f.Configs[*ref]
	if !ok || c.Legal == nil {
		return nil, nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "The legal terms are unknown")
	}
	if err := c.Check.at(path); err != nil {
		return nil, nil, err
	}
	formation, err := FormationUnder(*c.Legal, path)
	if err != nil {
		return nil, nil, err
	}
	if renderer != QuoteRendererV020 {
		return nil, nil, fail(CodeDependencyNotSupported, p+"/document_template_ref",
			"The template's renderer prints no legal terms: use a quote-text/0.2.0 template")
	}
	r := *ref
	return &r, &LegalTerms{Source: source, GoverningLaw: c.Legal.GoverningLaw, CISG: c.Legal.CISG,
		DisputeResolution: c.Legal.DisputeResolution, Formation: formation}, nil
}
