package tsk

import (
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/adapters/trd"
)

var _ api.VerificationQueries = (*Module)(nil)

// TRDGoals is what TSK reads in TRD to verify a goal: GoalReadiness v1 and the quote read that shows
// a bound root's object control (Context Map R18).
type TRDGoals = trd.Goals

// TRDReadiness is the port to TRD's GoalReadiness v1 over its api (WBS S4-01d): goal.verify asks it
// for the task's goal before TSK's transaction and records the verification.
func TRDReadiness(t TRDGoals) Readiness { return trd.Readiness{TRD: t} }
