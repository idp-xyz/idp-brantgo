package tradecompliancecheck

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// DocumentContract names an evaluation's document, the exact bytes its facts hash covers.
const DocumentContract = "brant.trade-compliance-evaluation/0.1.0"

// Outcome is what an evaluation found, before any human conclusion.
type Outcome string

const (
	NoMatch    Outcome = "no_match"   // complete facts, no programme or control finding applies
	Match      Outcome = "match"      // at least one applying finding: compliance judges it
	Incomplete Outcome = "incomplete" // a fact is missing: the check cannot conclude clear
)

// Where a country comes from (CNX-INV-001, S5 D-8).
const (
	SourceConsignee = "consignee_address" // the destination: the country of the order's consignee address
	SourceEntered   = "entered"           // entered by the evaluator with evidence
	SourcePTY       = "pty"               // the country PTY records for the address
)

// Export-control classifications of a line (CAT DeclarationProfileRef v1, R39).
const (
	NotControlled = "not_controlled" // a verified profile classifies the goods as not controlled
	Controlled    = "controlled"     // a verified profile names a control list reference
	Unverified    = "unverified"     // the profile is not verified by an authorized professional
	Unknown       = "unknown"        // CAT has no declaration profile for the specification
)

// Measures a country programme imposes (reference data, S5 A3b), and the goods each covers as the
// check reads it: an embargo and sanctions any goods, an arms embargo and an export control only
// controlled goods — a programme of those is a finding that applies when a line is controlled or
// not classified.
const (
	MeasureEmbargo       = "embargo"
	MeasureArmsEmbargo   = "arms_embargo"
	MeasureSanctions     = "sanctions"
	MeasureExportControl = "export_control"
	GoodsAll             = "all"
	GoodsControlled      = "controlled"
)

// GoodsOf returns the goods measure covers.
func GoodsOf(measure string) string {
	if measure == MeasureArmsEmbargo || measure == MeasureExportControl {
		return GoodsControlled
	}
	return GoodsAll
}

// Kinds of finding and of gap.
const (
	FindingProgramme  = "programme_match" // a country examined is named by a programme
	FindingControlled = "controlled_goods"

	GapDestinationUnknown       = "destination_unknown"   // no consignee address, or its country is unknown
	GapDestinationAmbiguous     = "destination_ambiguous" // consignee addresses in different countries
	GapCountryUnknown           = "country_unknown"       // a buyer, payer or consignee address without a country
	GapClassificationUnknown    = "classification_unknown"
	GapClassificationUnverified = "classification_unverified"
	GapEndUseMissing            = "end_use_missing"
	GapProgrammeListMissing     = "programme_list_missing" // an enabled programme list the company never imported
	GapProgrammesMissing        = "programmes_missing"     // no programme list is enabled
)

// Address roles of the order's parties the check examines (PTY address roles): the consignee's
// country is the destination; the buyer's, payer's and consignee's countries must be known; other
// roles' countries are matched when PTY records them. RoleDestination names the destination among
// the roles a finding matched.
const (
	RoleDestination = "destination"
	RoleConsignee   = "consignee"
	RoleBuyer       = "buyer"
	RolePayer       = "payer"
)

func required(role string) bool {
	return role == RoleBuyer || role == RolePayer || role == RoleConsignee
}

// Evidence is an EVD source an entry relies on (SourceRef v1, R54).
type Evidence struct {
	SourceID      string `json:"source_id"`
	ContentSHA256 string `json:"content_sha256"`
}

// Address is one address of the order's parties as TRD binds it, with what PTY says of it: Known
// false when PTY does not resolve it, Country "" when PTY records no country (S5 D-8: an address
// confirmed before PTY recorded countries).
type Address struct {
	Role          string
	PartyID       string
	PartyRevision int64
	AddressID     string
	Known         bool
	Country       string
}

// Profile is a line's declaration profile as CAT states it (DeclarationProfileRef v1).
type Profile struct {
	ProfileID        string
	Revision         int64
	CodeTableVersion string
	Verified         bool
	CommodityCode    string
	ExportControl    string // NotControlled or Controlled
	ListReference    string
	ContentHash      string
}

// Line is an order line with its specification and, when CAT has one, its profile.
type Line struct {
	LineID       string
	SkuID        string
	SpecRevision int64
	Profile      *Profile
}

// ListVersion is the version of a country programme list an evaluation used.
type ListVersion struct {
	ListCode     string
	Jurisdiction string
	Version      string
	ContentHash  string
}

// ProgrammeHit is a programme of a list version naming a country the evaluation asked about.
type ProgrammeHit struct {
	ListCode     string
	Jurisdiction string
	Version      string
	Country      string
	ProgrammeID  string
	Name         string
	Measure      string
	Scope        string
	ContentHash  string
}

// Regulation is the screening_regulation revision an evaluation follows (POL ConfigRef v1, R50):
// its content hash, how long a conclusion stays valid and the end of its own validity (zero:
// open-ended).
type Regulation struct {
	ID           string
	Revision     int64
	ContentHash  string
	RescreenDays int
	ValidUntil   time.Time
}

// Entered is a country the evaluator enters with evidence: the destination instead of the
// consignee address's country (Reason says why), or the country of an address PTY has none for.
type Entered struct {
	PartyID   string
	AddressID string
	Country   string
	Reason    string
	Evidence  []Evidence
}

// EndUse is the end use the evaluator states with evidence.
type EndUse struct {
	Statement string     `json:"statement"`
	Evidence  []Evidence `json:"evidence"`
}

// Input is everything one evaluation judges. Lists are the programme lists the evaluation is to
// use; Versions the version reference data holds of each (a list never imported is absent); Hits
// the programmes of those versions naming the countries asked about.
type Input struct {
	CheckID      string
	EvaluationID string
	Seq          int
	Binding      Binding
	Addresses    []Address
	Lines        []Line
	Destination  *Entered // nil: the consignee address's country
	Countries    []Entered
	EndUse       *EndUse
	Regulation   Regulation
	Lists        []string
	Versions     map[string]ListVersion
	Hits         []ProgrammeHit
	By           kernel.ActorRef
	Epoch        int64
	At           time.Time
}

// Document is an evaluation as stored: the exact JSON its FactsHash covers.
type Document struct {
	Contract       string         `json:"contract"`
	CheckID        string         `json:"check_id"`
	EvaluationID   string         `json:"evaluation_id"`
	Seq            int            `json:"seq"`
	Order          Binding        `json:"order"`
	Regulation     DocRegulation  `json:"regulation"`
	Destination    DocDestination `json:"destination"`
	Parties        []DocParty     `json:"parties"`
	Lines          []DocLine      `json:"lines"`
	EndUse         *EndUse        `json:"end_use"`
	Lists          []string       `json:"programme_lists"`
	Programmes     []DocProgramme `json:"programmes"`
	Findings       []Finding      `json:"findings"`
	Gaps           []Gap          `json:"gaps"`
	Outcome        Outcome        `json:"outcome"`
	EvaluatedBy    DocActor       `json:"evaluated_by"`
	AuthorityEpoch int64          `json:"authority_epoch"`
	EvaluatedAt    string         `json:"evaluated_at"`
	ValidUntil     string         `json:"valid_until"`
}

// DocRegulation is the regulation revision in the document; ValidUntil "" is open-ended.
type DocRegulation struct {
	ID           string `json:"id"`
	Revision     int64  `json:"revision"`
	ContentHash  string `json:"content_hash"`
	RescreenDays int    `json:"rescreen_interval_days"`
	ValidUntil   string `json:"valid_until"`
}

// DocDestination is the destination country and where it came from ("" when unknown).
type DocDestination struct {
	Country   string     `json:"country"`
	Source    string     `json:"source"`
	PartyID   string     `json:"party_id"`
	AddressID string     `json:"address_id"`
	Reason    string     `json:"reason"`
	Evidence  []Evidence `json:"evidence"`
}

// DocParty is one address of the order's parties with its country ("" when unknown).
type DocParty struct {
	Role          string     `json:"role"`
	PartyID       string     `json:"party_id"`
	PartyRevision int64      `json:"party_revision"`
	AddressID     string     `json:"address_id"`
	Country       string     `json:"country"`
	Source        string     `json:"source"`
	Evidence      []Evidence `json:"evidence"`
}

// DocLine is one order line with its export-control classification.
type DocLine struct {
	LineID           string `json:"line_id"`
	SkuID            string `json:"sku_id"`
	SpecRevision     int64  `json:"spec_revision"`
	Classification   string `json:"classification"`
	ProfileID        string `json:"profile_id"`
	ProfileRevision  int64  `json:"profile_revision"`
	CodeTableVersion string `json:"code_table_version"`
	CommodityCode    string `json:"commodity_code"`
	ListReference    string `json:"list_reference"`
	ProfileHash      string `json:"profile_hash"`
}

// DocProgramme is a programme list version the evaluation used.
type DocProgramme struct {
	Code         string `json:"code"`
	Jurisdiction string `json:"jurisdiction"`
	Version      string `json:"version"`
	ContentHash  string `json:"content_hash"`
}

// DocActor is an actor in the document.
type DocActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Finding is a candidate for the compliance decision: a programme naming a country the evaluation
// examined (the destination or a party's), with the roles it matched, the goods its measure covers
// and whether it applies to these goods, or a line with controlled goods.
type Finding struct {
	Kind          string   `json:"kind"`
	ListCode      string   `json:"list_code,omitempty"`
	Jurisdiction  string   `json:"jurisdiction,omitempty"`
	Version       string   `json:"version,omitempty"`
	ProgrammeID   string   `json:"programme_id,omitempty"`
	ProgrammeName string   `json:"programme_name,omitempty"`
	ProgrammeHash string   `json:"programme_hash,omitempty"`
	Country       string   `json:"country,omitempty"`
	Measure       string   `json:"measure,omitempty"`
	Goods         string   `json:"goods,omitempty"`
	Scope         string   `json:"scope,omitempty"`
	Roles         []string `json:"roles,omitempty"`
	LineID        string   `json:"line_id,omitempty"`
	ListReference string   `json:"list_reference,omitempty"`
	Applies       bool     `json:"applies"`
}

// Gap is a missing fact: its kind and what it concerns (party/address, a line, a list code).
type Gap struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref,omitempty"`
}

// Evaluation is a stored evaluation: its document, its exact bytes and their SHA-256.
type Evaluation struct {
	Document
	Bytes     []byte
	FactsHash string
}

// validUntil is when a conclusion on the evaluation lapses: RescreenDays after the evaluation,
// and never after the regulation's own validity.
func (in Input) validUntil() time.Time {
	until := kernel.Instant(in.At).AddDate(0, 0, in.Regulation.RescreenDays)
	if v := in.Regulation.ValidUntil; !v.IsZero() && v.Before(until) {
		until = kernel.Instant(v)
	}
	return until
}

func addressRef(partyID, addressID string) string { return partyID + "/" + addressID }

// Countries returns the countries an evaluation of in asks programmes about: every country PTY
// records for an order address, every entered one and the entered destination, sorted.
func Countries(addresses []Address, destination *Entered, entered []Entered) []string {
	var out []string
	for _, a := range addresses {
		if a.Country != "" {
			out = append(out, a.Country)
		}
	}
	for _, e := range entered {
		out = append(out, e.Country)
	}
	if destination != nil {
		out = append(out, destination.Country)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Evaluate judges in (CNX-INV-001): the destination is the consignee address's country unless the
// evaluator entered another with evidence (D-8); the buyer's, payer's and consignee's addresses
// need a country, from PTY or entered where PTY has none; every line needs a verified declaration
// profile; the end use is stated; every enabled programme list has a version; each programme of
// those versions naming the destination or a party's country is a finding. A country entered for an
// address that is not the order's is REFERENCE_INVALID, one entered where PTY records a country
// INVALID_KNOWN_VALUE.
func Evaluate(in Input) (*Evaluation, error) {
	doc := Document{Contract: DocumentContract, CheckID: in.CheckID, EvaluationID: in.EvaluationID, Seq: in.Seq, Order: in.Binding,
		Regulation: DocRegulation{ID: in.Regulation.ID, Revision: in.Regulation.Revision, ContentHash: in.Regulation.ContentHash,
			RescreenDays: in.Regulation.RescreenDays},
		Parties: []DocParty{}, Lines: []DocLine{}, Programmes: []DocProgramme{}, Findings: []Finding{}, Gaps: []Gap{},
		EvaluatedBy: DocActor{Kind: string(in.By.Kind), ID: in.By.ID}, AuthorityEpoch: in.Epoch, EvaluatedAt: kernel.FormatTimestamp(in.At),
		ValidUntil: kernel.FormatTimestamp(in.validUntil())}
	if !in.Regulation.ValidUntil.IsZero() {
		doc.Regulation.ValidUntil = kernel.FormatTimestamp(in.Regulation.ValidUntil)
	}
	known := map[string]Address{}
	for _, a := range in.Addresses {
		known[addressRef(a.PartyID, a.AddressID)] = a
	}
	entered := map[string]Entered{}
	for i, e := range in.Countries {
		path := "/args/address_countries/" + strconv.Itoa(i)
		a, ok := known[addressRef(e.PartyID, e.AddressID)]
		switch {
		case !ok:
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, path+"/address_id", "The order binds no such address")
		case a.Known && a.Country != "":
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, path+"/country", "PTY records the address's country")
		}
		entered[addressRef(e.PartyID, e.AddressID)] = e
	}

	// The parties' countries, in the order's address order.
	var gaps []Gap
	examined := map[string][]string{} // country → roles
	for _, a := range in.Addresses {
		p := DocParty{Role: a.Role, PartyID: a.PartyID, PartyRevision: a.PartyRevision, AddressID: a.AddressID, Evidence: []Evidence{}}
		ref := addressRef(a.PartyID, a.AddressID)
		switch e, ok := entered[ref]; {
		case a.Known && a.Country != "":
			p.Country, p.Source = a.Country, SourcePTY
		case ok:
			p.Country, p.Source, p.Evidence = e.Country, SourceEntered, sortedEvidence(e.Evidence)
		case required(a.Role):
			gaps = append(gaps, Gap{Kind: GapCountryUnknown, Ref: ref})
		}
		if p.Country != "" && !slices.Contains(examined[p.Country], a.Role) {
			examined[p.Country] = append(examined[p.Country], a.Role)
		}
		doc.Parties = append(doc.Parties, p)
	}

	// The destination: entered, or the consignee address's country.
	dest := &doc.Destination
	dest.Evidence = []Evidence{}
	if e := in.Destination; e != nil {
		dest.Country, dest.Source, dest.Reason, dest.Evidence = e.Country, SourceEntered, e.Reason, sortedEvidence(e.Evidence)
	} else {
		var consignees []DocParty
		for _, p := range doc.Parties {
			if p.Role == RoleConsignee {
				consignees = append(consignees, p)
			}
		}
		switch {
		case len(consignees) == 0 || consignees[0].Country == "":
			gaps = append([]Gap{{Kind: GapDestinationUnknown}}, gaps...)
		case slices.ContainsFunc(consignees, func(p DocParty) bool { return p.Country != consignees[0].Country }):
			gaps = append([]Gap{{Kind: GapDestinationAmbiguous}}, gaps...)
		default:
			c := consignees[0]
			dest.Country, dest.Source, dest.PartyID, dest.AddressID = c.Country, SourceConsignee, c.PartyID, c.AddressID
		}
	}
	if dest.Country != "" {
		examined[dest.Country] = append([]string{RoleDestination}, examined[dest.Country]...)
	}

	// The lines' classifications.
	controlled, unclassified := false, false
	for _, l := range in.Lines {
		dl := DocLine{LineID: l.LineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision, Classification: Unknown}
		if p := l.Profile; p != nil {
			dl.ProfileID, dl.ProfileRevision, dl.CodeTableVersion, dl.CommodityCode, dl.ProfileHash =
				p.ProfileID, p.Revision, p.CodeTableVersion, p.CommodityCode, p.ContentHash
			switch {
			case !p.Verified:
				dl.Classification = Unverified
			case p.ExportControl == Controlled:
				dl.Classification, dl.ListReference = Controlled, p.ListReference
			case p.ExportControl == NotControlled:
				dl.Classification = NotControlled
			}
		}
		switch dl.Classification {
		case Unknown:
			gaps, unclassified = append(gaps, Gap{Kind: GapClassificationUnknown, Ref: l.LineID}), true
		case Unverified:
			gaps, unclassified = append(gaps, Gap{Kind: GapClassificationUnverified, Ref: l.LineID}), true
		case Controlled:
			controlled = true
		}
		doc.Lines = append(doc.Lines, dl)
	}
	if in.EndUse == nil {
		gaps = append(gaps, Gap{Kind: GapEndUseMissing})
	} else {
		doc.EndUse = &EndUse{Statement: in.EndUse.Statement, Evidence: sortedEvidence(in.EndUse.Evidence)}
	}

	// The programme lists, in code order: a list never imported cannot be matched against, so the
	// check cannot conclude (a missing list is never a clear one).
	lists := slices.Clone(in.Lists)
	slices.Sort(lists)
	doc.Lists = slices.Compact(lists)
	if doc.Lists == nil {
		doc.Lists = []string{}
	}
	for _, code := range doc.Lists {
		v, ok := in.Versions[code]
		if !ok {
			gaps = append(gaps, Gap{Kind: GapProgrammeListMissing, Ref: code})
			continue
		}
		doc.Programmes = append(doc.Programmes, DocProgramme{Code: code, Jurisdiction: v.Jurisdiction, Version: v.Version, ContentHash: v.ContentHash})
	}
	if len(doc.Lists) == 0 {
		gaps = append(gaps, Gap{Kind: GapProgrammesMissing})
	}
	hits := slices.Clone(in.Hits)
	slices.SortFunc(hits, func(a, b ProgrammeHit) int {
		return strings.Compare(a.ListCode+"\x00"+a.ProgrammeID+"\x00"+a.Country, b.ListCode+"\x00"+b.ProgrammeID+"\x00"+b.Country)
	})
	for _, h := range hits {
		v, used := in.Versions[h.ListCode]
		roles, ok := examined[h.Country]
		if !used || v.Version != h.Version || !slices.Contains(doc.Lists, h.ListCode) || !ok {
			continue
		}
		goods := GoodsOf(h.Measure)
		doc.Findings = append(doc.Findings, Finding{Kind: FindingProgramme, ListCode: h.ListCode, Jurisdiction: h.Jurisdiction, Version: h.Version,
			ProgrammeID: h.ProgrammeID, ProgrammeName: h.Name, ProgrammeHash: h.ContentHash, Country: h.Country, Measure: h.Measure,
			Goods: goods, Scope: h.Scope, Roles: slices.Clone(roles), Applies: goods == GoodsAll || controlled || unclassified})
	}
	for _, l := range doc.Lines {
		if l.Classification == Controlled {
			doc.Findings = append(doc.Findings, Finding{Kind: FindingControlled, LineID: l.LineID, ListReference: l.ListReference, Applies: true})
		}
	}
	if gaps != nil {
		doc.Gaps = gaps
	}
	switch {
	case len(doc.Gaps) > 0:
		doc.Outcome = Incomplete
	case slices.ContainsFunc(doc.Findings, func(f Finding) bool { return f.Applies }):
		doc.Outcome = Match
	default:
		doc.Outcome = NoMatch
	}
	return Seal(doc)
}

func sortedEvidence(es []Evidence) []Evidence {
	out := slices.Clone(es)
	slices.SortFunc(out, func(a, b Evidence) int { return strings.Compare(a.SourceID, b.SourceID) })
	if out == nil {
		out = []Evidence{}
	}
	return out
}

// Seal encodes doc as its stored bytes and hash.
func Seal(doc Document) (*Evaluation, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return &Evaluation{Document: doc, Bytes: b, FactsHash: Hash(b)}, nil
}

// Hash is the lowercase hex SHA-256 of b.
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Parse reads a stored evaluation and checks its bytes against hash (REFERENCE_INVALID when the
// record is not intact).
func Parse(b []byte, hash string) (*Evaluation, error) {
	var doc Document
	if Hash(b) != hash || json.Unmarshal(b, &doc) != nil || doc.Contract != DocumentContract {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/evaluation", "The evaluation record is not intact")
	}
	return &Evaluation{Document: doc, Bytes: b, FactsHash: hash}, nil
}
