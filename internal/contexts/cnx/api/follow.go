package api

import (
	"context"
	"time"
)

// CNX follows order and regulation changes (S5 unit C4b-CNX, WBS S5-09b; event storming PL-14;
// tactical model WF-06 last step, SM-CNX-COMPLIANCE clear → reopen → pending, CNX-INV-001; S5
// decisions D-9, D-18; MD-47). Its Inbox consumer cnx.trade_compliance runs
// trade_compliance.reopen as system:inbox.cnx.trade_compliance:
//
//   - TRD's OrderAmended (R46) opens a pending check for the new order revision (order_amended),
//     once per order revision; the earlier revision's conclusion stays what it was and binds only
//     its own revision.
//   - POL's ConfigPublished of a screening_regulation revision (R50) reopens every clear
//     conclusion that follows an earlier revision of that regulation (regulation_revised).
//   - Reference data's CountryProgrammesImported reopens every clear conclusion whose evaluation
//     used another version of that list than the current one (programme_updated).
//
// The reads below show the clear conclusions a regulation revision or a programme list version
// carries, with their state now as TradeComplianceGuard v1 decides it, so the work is visible.
const (
	CommandChecksByRegulation = "trade_compliance.by_regulation"
	CommandChecksByList       = "trade_compliance.by_list"
)

// ClearCheck is a clear conclusion as the follow-up reads show it: the order revision it binds,
// its decision with the regulation revision it follows, the programme list versions its evaluation
// used, and, as of CheckedAt, its state and reason as TradeComplianceGuard v1 decides them (clear
// and cleared, expired, or pending and list_updated).
type ClearCheck struct {
	CheckID            string
	OrderID            string
	OrderRevision      int64
	OrderHash          string
	RowVersion         int64
	DecisionID         string
	RegulationID       string
	RegulationRevision int64
	Programmes         []ProgrammeView
	LicenseRequired    bool
	ValidUntil         time.Time
	State              string
	Reason             string
	CheckedAt          time.Time
}

// FollowQueries are the reads of the clear conclusions a regulation or a programme list carries
// (cnx.read), oldest check first, each the caller sees.
type FollowQueries interface {
	// ChecksByRegulation returns the clear conclusions that follow screening_regulation
	// regulationID, at revision only when revision is positive (trade_compliance.by_regulation).
	ChecksByRegulation(ctx context.Context, regulationID string, revision int64) ([]ClearCheck, error)
	// ChecksByList returns the clear conclusions whose evaluation used programme list listCode, at
	// version only when version is not empty (trade_compliance.by_list).
	ChecksByList(ctx context.Context, listCode, version string) ([]ClearCheck, error)
}
