package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Party screening (PTY-AGG-PartyScreening, PTY-INV-002, WBS S1-09). A party is screened in its
// confirmed revision against the current versions of the screening lists its company's
// screening_regulation (POL, Context Map R60) enables; the lists are reference data
// (internal/reference). A conclusion records the regulation revision, the list versions, the hits,
// the screener and its validity; it is a record of a screening, not a legal finding.
const (
	CommandRecordScreening = "party_screening.record" // human or AI
	CommandDecideScreening = "party_screening.decide" // human only; compliance template
	CommandBlockParty      = "party_screening.block"  // human only; compliance template

	EventPartyScreeningRecorded = "idp-brant.pty.party_screening_recorded.v1"
	EventPartyScreeningDecided  = "idp-brant.pty.party_screening_decided.v1"
	EventPartyScreeningBlocked  = "idp-brant.pty.party_screening_blocked.v1"
)

// Outcomes of party_screening.record.
const (
	OutcomeClear   = "clear"   // no hit, or only hits a positive-list entry covers
	OutcomeMatch   = "match"   // a suspected or confirmed hit
	OutcomePending = "pending" // an enabled list has no imported version
)

// Screening statuses: whether a party may be relied on now. Only clear is clear.
const (
	ScreeningClear   = "clear"
	ScreeningPending = "pending"
	ScreeningMatch   = "match"
	ScreeningBlocked = "blocked"
	ScreeningExpired = "expired"
)

// Reasons for a screening status, in the order the guard decides them.
const (
	ReasonUnknownParty   = "unknown_party"   // pending: no such party in the scope
	ReasonNegativeList   = "negative_list"   // blocked: party_screening.block; never lifted
	ReasonNotScreened    = "not_screened"    // pending: no conclusion yet
	ReasonPartyChanged   = "party_changed"   // pending: the party is not confirmed in the revision screened
	ReasonExpired        = "expired"         // expired: the conclusion's validity has ended
	ReasonListUpdated    = "list_updated"    // pending: a list version used is no longer current, or a missing list was imported
	ReasonTrueMatch      = "true_match"      // blocked: a hit decided a true match
	ReasonSuspectedMatch = "suspected_match" // match: a hit awaits a compliance decision
	ReasonIncomplete     = "incomplete"      // pending: an enabled list had no imported version
	ReasonScreened       = "screened"        // clear
	// ReasonRegulationChanged is a reason of ScreeningsDue only: the regulation revision the
	// conclusion followed is no longer in force (POL is read before PTY's transaction; R60 carries
	// no guard lock, so the guard does not see it).
	ReasonRegulationChanged = "regulation_changed"
)

// Decisions of party_screening.decide.
const (
	DecisionFalsePositive = "false_positive"
	DecisionTrueMatch     = "true_match"
)

// Dispositions of a hit.
const (
	HitSuspected      = "suspected"
	HitPositiveList   = "positive_list"
	HitConfirmedMatch = "confirmed_match"
)

// Codes PTY declares, with the category the error catalog V0.1 gives them.
const (
	CodePartyScreeningNotClear = "PARTY_SCREENING_NOT_CLEAR" // 409
	CodeReferenceStale         = "REFERENCE_STALE"           // 409, refresh and resubmit
)

var codes = map[string]kernel.Category{
	CodePartyScreeningNotClear: kernel.CategoryConflict,
	CodeReferenceStale:         kernel.CategoryConflict,
}

// Reject returns a rejection with a code PTY declares, for downstream contexts that pass a PTY
// verdict on at a path of their own request. It panics on a code PTY does not declare.
func Reject(code, path string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("pty: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, "")
}

// Category returns the category of a code PTY declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// RecordScreening screens the party's confirmed revision against the lists the screening_regulation
// revision (RegulationID, RegulationRevision) enables, in their current versions, as of now; the
// revision must be in force. ExpectedVersion is the screening's row version, 0 for a party never
// screened or blocked.
type RecordScreening struct {
	IdempotencyKey     string
	PartyID            string
	ExpectedVersion    int64
	RegulationID       string
	RegulationRevision int64
}

// DecideScreening decides the party's current screening, a suspected match: a false positive is
// cleared with Reason and, with RegisterPositive, every suspected hit goes on the positive list for
// the party's revision and the entry's content; a true match stays blocked.
type DecideScreening struct {
	IdempotencyKey   string
	PartyID          string
	ExpectedVersion  int64
	ScreeningID      string
	Decision         string
	Reason           string
	RegisterPositive bool
}

// BlockParty puts the party on the negative list for good; no list update lifts it.
type BlockParty struct {
	IdempotencyKey  string
	PartyID         string
	ExpectedVersion int64
	Reason          string
}

// MaxRegulationIDLength is POL's longest config_id.
const MaxRegulationIDLength = 40

// Validate checks the closed contract.
func (c RecordScreening) Validate() error {
	if err := head(c.IdempotencyKey, c.PartyID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	switch {
	case len(c.RegulationID) > MaxRegulationIDLength || !kernel.ValidIdentifier(c.RegulationID):
		return schema("/args/regulation_id")
	case c.RegulationRevision < 1 || c.RegulationRevision > 2147483647:
		return schema("/args/regulation_revision")
	}
	return nil
}

func (c DecideScreening) Validate() error {
	if err := head(c.IdempotencyKey, c.PartyID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	switch {
	case !kernel.ValidIdentifier(c.ScreeningID):
		return schema("/args/screening_id")
	case c.Decision != DecisionFalsePositive && c.Decision != DecisionTrueMatch:
		return schema("/args/decision")
	case !Text(c.Reason, 1000):
		return schema("/args/reason")
	case c.RegisterPositive && c.Decision != DecisionFalsePositive:
		return schema("/args/register_positive")
	}
	return nil
}

func (c BlockParty) Validate() error {
	if err := head(c.IdempotencyKey, c.PartyID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	if !Text(c.Reason, 1000) {
		return schema("/args/reason")
	}
	return nil
}

// ScreeningCommands is PTY's command entry point for party screening. Receipts name the party
// (ObjectID), the screening's row version and its status after the command.
type ScreeningCommands interface {
	RecordScreening(ctx context.Context, cmd RecordScreening) (Receipt, error)
	DecideScreening(ctx context.Context, cmd DecideScreening) (Receipt, error)
	BlockParty(ctx context.Context, cmd BlockParty) (Receipt, error)
}

// ScreeningListUsed is a list version a screening used.
type ScreeningListUsed struct {
	ListCode     string
	Jurisdiction string
	Version      string
	ContentHash  string
}

// ScreeningHit is a list entry the party's name matched, and what the screening made of it.
type ScreeningHit struct {
	ListCode         string
	Version          string
	EntryID          string
	EntryContentHash string
	PrimaryName      string
	MatchedName      string
	NameKey          string
	Disposition      string // suspected, positive_list or confirmed_match
	PositiveID       string // the positive-list entry that covered it
}

// ScreeningDecisionView is the decision on a suspected match.
type ScreeningDecisionView struct {
	Decision       string
	Reason         string
	DecidedBy      kernel.ActorRef
	DecidedAt      time.Time
	AuthorityEpoch int64
}

// ScreeningResultView is one conclusion. Status is what the screening left; the decision, if any,
// says what compliance made of it.
type ScreeningResultView struct {
	ScreeningID        string
	PartyRevision      int64
	ScreenedName       string
	RegulationID       string
	RegulationRevision int64
	RegulationHash     string
	Outcome            string
	Status             string
	Lists              []ScreeningListUsed
	MissingLists       []string
	Hits               []ScreeningHit
	Normalization      string
	ScreenedBy         kernel.ActorRef
	ScreenedAt         time.Time
	ValidUntil         time.Time
	AuthorityEpoch     int64
	Decision           *ScreeningDecisionView
}

// PositiveEntryView is a positive-list entry. AppliesToRevision tells whether it is bound to the
// party's current revision; whether the entry's content is unchanged shows at the next screening.
type PositiveEntryView struct {
	PositiveID        string
	PartyRevision     int64
	ListCode          string
	EntryID           string
	EntryContentHash  string
	ScreeningID       string
	Reason            string
	RegisteredBy      kernel.ActorRef
	RegisteredAt      time.Time
	AppliesToRevision bool
}

// NegativeEntryView is the party's negative-list entry.
type NegativeEntryView struct {
	Reason         string
	BlockedBy      kernel.ActorRef
	BlockedAt      time.Time
	AuthorityEpoch int64
}

// ScreeningView is a party's screening as of a read: its status and reason now (as the guard
// decides them), the current conclusion, every conclusion oldest first, its positive-list entries
// and its negative-list entry.
type ScreeningView struct {
	PartyID            string
	RowVersion         int64 // 0: never screened or blocked
	Status             string
	Reason             string
	CurrentScreeningID string
	Screenings         []ScreeningResultView
	PositiveEntries    []PositiveEntryView
	Negative           *NegativeEntryView
}

// ScreeningDue is a confirmed party whose screening is due: never screened, screened in another
// revision, expired, screened against a list version that is no longer current, or under a
// regulation revision no longer in force. RowVersion is the ExpectedVersion of the next record.
type ScreeningDue struct {
	PartyID            string
	PartyRevision      int64
	RowVersion         int64
	Status             string
	Reason             string
	RegulationID       string // of the current conclusion; empty when never screened
	RegulationRevision int64
}

// ScreeningQueries is PTY's read side for party screening.
type ScreeningQueries interface {
	// GetScreening returns the party's screening, audited as party_screening.get (pty.read).
	GetScreening(ctx context.Context, partyID string) (ScreeningView, error)
	// ScreeningsDue lists the confirmed parties the caller sees whose screening is due at instant
	// at, ordered by party ID; audited as party_screening.due (pty.read). Re-screening after a list
	// import or a new party revision is derived here and in the guard, not written by the import.
	ScreeningsDue(ctx context.Context, at time.Time) ([]ScreeningDue, error)
}

// PartyScreeningGuard v1 (Context Map R44 PTY->FUL, R13 PTY->TRD; ADR-BR-004 option B). A
// downstream context calls it with its own executor inside its own transaction, after it locked its
// own aggregate root FOR UPDATE (ADR-BR-011):
//
//	brant_pty.party_screening_guard_v1(p_party_id text, p_at timestamptz)
//	    RETURNS TABLE (clear boolean, status text, reason text, screening_id text, party_revision bigint)
//
// It returns one row for the party in the scope of app.tenant_id and app.company_id as of p_at,
// the instant the downstream relies on the conclusion. It locks the party's screening root and the
// party FOR SHARE, and the current version of every list the conclusion used through
// reference data's guard, until the downstream transaction ends: a block, a new conclusion, a new
// party revision or a new list version either committed before the guard read it or waits for the
// downstream to commit. screening_id is empty and party_revision 0 when there is no conclusion. It
// is SECURITY DEFINER with a fixed search_path; brant_app may execute it.
const (
	ScreeningGuardFunction = "brant_pty.party_screening_guard_v1"
	ScreeningGuardSQL      = "SELECT clear, status, reason, screening_id, party_revision FROM brant_pty.party_screening_guard_v1($1, $2)"
)

// ScreeningGuardResult is the row PartyScreeningGuard v1 returns.
type ScreeningGuardResult struct {
	Clear         bool
	Status        string
	Reason        string
	ScreeningID   string
	PartyRevision int64
}

// Targets returns the scan destinations of ScreeningGuardSQL's columns, in order.
func (r *ScreeningGuardResult) Targets() []any {
	return []any{&r.Clear, &r.Status, &r.Reason, &r.ScreeningID, &r.PartyRevision}
}

// Check returns nil when the party is clear and PARTY_SCREENING_NOT_CLEAR at path otherwise.
func (r ScreeningGuardResult) Check(path string) error {
	if r.Clear {
		return nil
	}
	return kernel.RejectAs(CodePartyScreeningNotClear, kernel.CategoryConflict, path, "Party screening is "+r.Status+": "+r.Reason)
}
