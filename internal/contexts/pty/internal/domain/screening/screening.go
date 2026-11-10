// Package screening is the PartyScreening aggregate (PTY-AGG-PartyScreening, PTY-INV-002): the
// screening conclusions of one party, appended and never changed, and a root that points at the
// current conclusion and is compared and incremented by every command.
//
//	party_screening.record  human or AI: screen the party's confirmed revision against the current
//	                        versions of the lists the company's screening_regulation enables
//	party_screening.decide  human (compliance): a suspected match is a false positive, cleared with
//	                        a written reason and optionally put on the positive list, or a true
//	                        match, which stays blocked
//	party_screening.block   human (compliance): the party goes on the negative list for good
//
// A positive-list entry is bound to the party revision and to the content hash of the list entry;
// it stops applying when either changes. A true match is bound to the list entry: re-screening
// keeps the party blocked while the entry is listed. The negative list is never lifted. Whether a
// conclusion is still current (party revision, list versions, validity) is derived when it is read,
// by the guard function in the database; nothing rewrites a conclusion. A conclusion is a record of
// a screening, not a legal finding.
package screening

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Outcome is what a screening found: clear (nothing, or only hits the positive list covers),
// match (a hit) or pending (an enabled list has no imported version, so the screening is not
// complete).
type Outcome string

const (
	OutcomeClear   Outcome = "clear"
	OutcomeMatch   Outcome = "match"
	OutcomePending Outcome = "pending"
)

// Status is the state of the current conclusion: clear, match (a suspected hit awaits a decision),
// pending, or blocked (a true match).
type Status string

const (
	StatusClear   Status = "clear"
	StatusMatch   Status = "match"
	StatusPending Status = "pending"
	StatusBlocked Status = "blocked"
)

// Disposition is what a screening made of one hit.
type Disposition string

const (
	Suspected      Disposition = "suspected"       // needs a decision
	PositiveList   Disposition = "positive_list"   // a positive-list entry for this revision and entry content applies
	ConfirmedMatch Disposition = "confirmed_match" // decided a true match earlier
)

// Decision is a compliance decision on a suspected match.
type Decision string

const (
	FalsePositive Decision = "false_positive"
	TrueMatch     Decision = "true_match"
)

// Jurisdiction of a list a tenant enables only with its legal confirmation.
const JurisdictionForeign = "foreign"

// Codes of PTY's error catalog rows that screening rules decide; package api declares the same
// codes for callers.
const CodeReferenceStale = "REFERENCE_STALE"

func stale(path, reason string) error {
	return kernel.RejectAs(CodeReferenceStale, kernel.CategoryConflict, path, reason)
}

// Root is the aggregate root of one party's screening.
type Root struct {
	PartyID    string
	RowVersion int64  // 0: not stored yet
	Current    string // the current conclusion; empty before the first screening
	Status     Status // the current conclusion's status; empty before the first screening
	Negative   bool   // on the negative list, for good
}

// CheckVersion admits a command at expected, the root version the caller saw; 0 for a party
// never screened or blocked.
func CheckVersion(r *Root, expected int64) error {
	if (r == nil && expected != 0) || (r != nil && r.RowVersion != expected) {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Party is the party as screened: its confirmed revision and name.
type Party struct {
	ID        string
	Revision  int64
	Name      string
	Confirmed bool
}

// Regulation is the screening_regulation revision a screening follows.
type Regulation struct {
	ID           string
	Revision     int64
	Hash         string
	Lists        []EnabledList
	RescreenDays int
	ValidUntil   time.Time // zero: open-ended
}

// EnabledList is a list the regulation enables.
type EnabledList struct {
	ListCode     string
	Jurisdiction string
}

// ListVersion is the current imported version of an enabled list.
type ListVersion struct {
	ListCode     string
	Jurisdiction string
	Version      string
	ContentHash  string
}

// Hit is a list entry the party's name matched.
type Hit struct {
	ListCode    string
	Version     string
	EntryID     string
	EntryHash   string
	PrimaryName string
	MatchedName string
	NameKey     string
	Disposition Disposition
	PositiveID  string // the positive-list entry that covered it
}

// Positive is a positive-list entry: the party in revision PartyRevision is not the list entry
// EntryID whose content had EntryHash.
type Positive struct {
	ID            string
	PartyID       string
	PartyRevision int64
	ListCode      string
	EntryID       string
	EntryHash     string
	ScreeningID   string
	Reason        string
	By            kernel.ActorRef
	Epoch         int64
	At            time.Time
}

// Applies reports whether p covers hit h of the party in revision partyRevision: the revision and
// the entry's content are those p was registered for.
func (p Positive) Applies(partyRevision int64, h Hit) bool {
	return p.PartyRevision == partyRevision && p.ListCode == h.ListCode && p.EntryID == h.EntryID && p.EntryHash == h.EntryHash
}

// EntryKey names a list entry across versions.
type EntryKey struct{ ListCode, EntryID string }

// Result is one screening conclusion. Status is the status the screening left; a decision may
// change the root's status later, never the result.
type Result struct {
	ID            string
	PartyID       string
	PartyRevision int64
	Name          string
	Regulation    Regulation
	Outcome       Outcome
	Status        Status
	Lists         []ListVersion
	Missing       []string // enabled lists without an imported version
	Hits          []Hit
	Normalization string
	By            kernel.ActorRef
	Epoch         int64
	At            time.Time
	ValidUntil    time.Time
}

// Input is everything a screening judges.
type Input struct {
	ID            string
	Party         Party
	Regulation    Regulation
	Versions      map[string]ListVersion // current version per enabled list code; absent: none imported
	Hits          []Hit                  // the matches of the party's name in those versions
	Positives     []Positive             // the party's positive-list entries
	Confirmed     map[EntryKey]bool      // entries decided true matches of this party
	Normalization string
	By            kernel.ActorRef
	Epoch         int64
	At            time.Time
}

// Screen decides one screening (PTY-INV-002): only the enabled lists are used, each in its current
// version and with the jurisdiction the regulation enabled it under; a hit on an entry decided a
// true match keeps the party blocked; a hit a positive-list entry covers does not count; any other
// hit is a suspected match. The conclusion stays valid RescreenDays, and never beyond the
// regulation's own validity.
func Screen(in Input) (*Result, error) {
	r := &Result{ID: in.ID, PartyID: in.Party.ID, PartyRevision: in.Party.Revision, Name: in.Party.Name, Regulation: in.Regulation,
		Normalization: in.Normalization, By: in.By, Epoch: in.Epoch, At: in.At, Missing: []string{}}
	enabled := map[string]bool{}
	for _, l := range in.Regulation.Lists {
		enabled[l.ListCode] = true
		v, ok := in.Versions[l.ListCode]
		switch {
		case !ok:
			r.Missing = append(r.Missing, l.ListCode)
		case v.Jurisdiction != l.Jurisdiction:
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/regulation_id",
				"Imported list "+l.ListCode+" has another jurisdiction than the regulation enables")
		default:
			r.Lists = append(r.Lists, v)
		}
	}
	matched, suspected, confirmed := map[EntryKey]bool{}, false, false
	for _, h := range in.Hits {
		v, ok := in.Versions[h.ListCode]
		key := EntryKey{h.ListCode, h.EntryID}
		if !enabled[h.ListCode] || !ok || v.Version != h.Version || matched[key] {
			continue // only the enabled lists in their current versions; one hit per entry
		}
		matched[key] = true
		switch {
		case in.Confirmed[key]:
			h.Disposition, confirmed = ConfirmedMatch, true
		default:
			h.Disposition = Suspected
			for _, p := range in.Positives {
				if p.Applies(in.Party.Revision, h) {
					h.Disposition, h.PositiveID = PositiveList, p.ID
					break
				}
			}
			suspected = suspected || h.Disposition == Suspected
		}
		r.Hits = append(r.Hits, h)
	}
	switch {
	case confirmed || suspected:
		r.Outcome = OutcomeMatch
	case len(r.Missing) > 0:
		r.Outcome = OutcomePending
	default:
		r.Outcome = OutcomeClear
	}
	r.Status = Status(r.Outcome)
	if confirmed {
		r.Status = StatusBlocked
	}
	r.ValidUntil = kernel.Instant(in.At.Add(time.Duration(in.Regulation.RescreenDays) * 24 * time.Hour))
	if u := in.Regulation.ValidUntil; !u.IsZero() && u.Before(r.ValidUntil) {
		r.ValidUntil = u
	}
	return r, nil
}

// CheckScreenable admits screening a party: only a confirmed revision is screened.
func CheckScreenable(p *Party) error {
	if !p.Confirmed {
		return kernel.Reject(kernel.CodeInvalidState, "/args/id", "Only a confirmed party is screened")
	}
	return nil
}

// Record makes res the root's current conclusion.
func (r *Root) Record(res *Result) {
	r.Current, r.Status = res.ID, res.Status
	r.RowVersion++
}

// Decide decides the root's current conclusion res for the party as it is now. Only a suspected
// match awaiting a decision is decided, for the party revision it screened, and never for a party
// on the negative list. A false positive clears the conclusion (pending when an enabled list was
// missing) and, with register, puts every suspected hit on the positive list for this revision and
// entry content; a true match blocks it. It returns the new positive-list entries.
func (r *Root) Decide(res *Result, party *Party, screeningID string, d Decision, register bool, newID func() string,
	reason string, by kernel.ActorRef, epoch int64, at time.Time) ([]Positive, error) {
	switch {
	case r.Negative:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/id", "The party is on the negative list")
	case r.Current != screeningID:
		return nil, stale("/args/screening_id", "This is not the party's current screening")
	case r.Status != StatusMatch:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/screening_id", "The screening awaits no decision")
	case !party.Confirmed || party.Revision != res.PartyRevision:
		return nil, stale("/args/screening_id", "The party changed after this screening")
	}
	var positives []Positive
	switch d {
	case TrueMatch:
		r.Status = StatusBlocked
	case FalsePositive:
		r.Status = StatusClear
		if len(res.Missing) > 0 {
			r.Status = StatusPending
		}
		for _, h := range res.Hits {
			if register && h.Disposition == Suspected {
				positives = append(positives, Positive{ID: newID(), PartyID: res.PartyID, PartyRevision: res.PartyRevision,
					ListCode: h.ListCode, EntryID: h.EntryID, EntryHash: h.EntryHash, ScreeningID: res.ID, Reason: reason, By: by, Epoch: epoch, At: at})
			}
		}
	}
	r.RowVersion++
	return positives, nil
}

// Block puts the party on the negative list. It is never lifted.
func (r *Root) Block() error {
	if r.Negative {
		return kernel.Reject(kernel.CodeInvalidState, "/args/id", "The party is already on the negative list")
	}
	r.Negative = true
	r.RowVersion++
	return nil
}
