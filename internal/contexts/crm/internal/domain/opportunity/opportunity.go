// Package opportunity is the Opportunity aggregate (SM-CRM-OPPORTUNITY, CRM-INV-001). FollowUp is
// a child entity, not a BusinessTask. Advancing a stage does not create a quote or an order.
package opportunity

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Stages of SM-CRM-OPPORTUNITY.
const (
	StageQualified   = "qualified"
	StageProposal    = "proposal"
	StageNegotiation = "negotiation"
	StageWon         = "won"
	StageLost        = "lost"
	StageAbandoned   = "abandoned"
)

// advance lists the stages a stage may move to. Follow-up is not a transition.
var advance = map[string][]string{
	StageQualified:   {StageProposal, StageAbandoned},
	StageProposal:    {StageNegotiation, StageLost, StageAbandoned},
	StageNegotiation: {StageWon, StageLost, StageAbandoned},
}

// FollowUp is one recorded follow-up. It does not change the stage.
type FollowUp struct {
	ID         string
	Summary    string
	NextStep   string
	RecordedAt time.Time
}

// Opportunity is the aggregate root.
type Opportunity struct {
	ID          string
	LeadID      string
	CustomerID  string
	Title       string
	Stage       string
	AmountMinor int64
	Currency    string
	RowVersion  int64
	OwnerActor  string
	OwnerTeam   string
	FollowUps   []FollowUp
}

// New returns an opportunity created at stage qualified, row version 1. currency empty means no
// expected amount. The minor units are stored as given; this package does not round.
func New(id, title, leadID, customerID, currency string, minor int64, owner, team string) *Opportunity {
	return &Opportunity{ID: id, LeadID: leadID, CustomerID: customerID, Title: title, Stage: StageQualified,
		AmountMinor: minor, Currency: currency, RowVersion: 1, OwnerActor: owner, OwnerTeam: team}
}

// Active reports whether the stage can be cited as a trade source.
func (o *Opportunity) Active() bool {
	switch o.Stage {
	case StageQualified, StageProposal, StageNegotiation, StageWon:
		return true
	}
	return false
}

// Advance moves the stage along SM-CRM-OPPORTUNITY and returns the stage it left. A stage that
// is not a registered successor is INVALID_STATE.
func (o *Opportunity) Advance(to string) (previous string, err error) {
	for _, next := range advance[o.Stage] {
		if next == to {
			previous = o.Stage
			o.Stage = to
			o.RowVersion++
			return previous, nil
		}
	}
	return "", kernel.Reject(kernel.CodeInvalidState, "/args/stage", "")
}

// Record appends a follow-up and bumps the row version. The stage does not change. A follow-up id
// already on this opportunity is BUSINESS_KEY_CONFLICT.
func (o *Opportunity) Record(f FollowUp) error {
	for _, have := range o.FollowUps {
		if have.ID == f.ID {
			return kernel.RejectAs("BUSINESS_KEY_CONFLICT", kernel.CategoryConflict, "/args/follow_up_id", "")
		}
	}
	o.FollowUps = append(o.FollowUps, f)
	o.RowVersion++
	return nil
}
