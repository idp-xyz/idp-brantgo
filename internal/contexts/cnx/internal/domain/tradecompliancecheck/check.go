// Package tradecompliancecheck holds CNX's TradeComplianceCheck (tactical model
// CNX-AGG-TradeComplianceCheck, SM-CNX-COMPLIANCE, CNX-INV-001; WBS S5-13): the country embargo,
// sanctions programme and export-control check of one sales order revision. It is the order-level
// analogue of PTY's party screening (UL CNX-EmbargoCheck: "不能由主体名单筛查替代").
//
// One root per (order, order revision): a later order revision never inherits an earlier
// revision's conclusion (CNX-INV-001 "订单变更后旧结论不适用"). An evaluation records, for the
// revision at its order hash, the destination country, the countries of the order's parties, the
// export-control classification of every line, the end use and the regulation and country
// programme versions it used, and what it found: candidates only. The conclusion (clear or blocked)
// is a human compliance decision, append-only, bound to the evaluation it decided and to its
// regulation revision, with a validity; blocked never lapses and no business approval overrides
// it. The root's states are those of SM-CNX-COMPLIANCE: pending → clear | blocked (decide, human);
// clear → pending (reopen, system or compliance); blocked → pending (evaluate after facts change).
package tradecompliancecheck

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Status is a check's state (SM-CNX-COMPLIANCE).
type Status string

const (
	Pending Status = "pending"
	Clear   Status = "clear"
	Blocked Status = "blocked"
)

// Codes the domain decides with besides the kernel's, and their categories (error catalog V0.1):
// a stale evaluation or binding, and a line whose classification no verified declaration profile
// gives (CAT-INV-002, R39).
const (
	CodeReferenceStale               = "REFERENCE_STALE"
	CodeDeclarationProfileUnverified = "DECLARATION_PROFILE_UNVERIFIED"
	// A clear conclusion on an evaluation with any other gap (S5R registered it for CNX-INV-001).
	CodeEvaluationIncomplete = "TRADE_COMPLIANCE_EVALUATION_INCOMPLETE"
)

// Categories are the categories of the domain's own codes.
var Categories = map[string]kernel.Category{
	CodeReferenceStale:               kernel.CategoryConflict,
	CodeDeclarationProfileUnverified: kernel.CategoryInvalid,
	CodeEvaluationIncomplete:         kernel.CategoryInvalid,
}

func reject(code, path, reason string) *kernel.Rejection {
	return kernel.RejectAs(code, Categories[code], path, reason)
}

// Binding is an order revision as a check binds it: the order, its order revision and order hash
// (TRD SalesOrderRef v1, R46).
type Binding struct {
	OrderID       string `json:"order_id"`
	OrderRevision int64  `json:"order_revision"`
	OrderHash     string `json:"order_hash"`
}

// Check is the TradeComplianceCheck root. Evaluation and Decision are the current evaluation and
// decision ("" when there is none): a pending check has no current decision, a decided one has
// both. Previous is the check of an earlier revision of the order it was opened after ("" when
// none). The owner is who opened it, for the data scope.
type Check struct {
	ID         string
	Binding    Binding
	Status     Status
	RowVersion int64
	Evaluation string
	Decision   string
	Previous   string
	OwnerActor string
	OwnerTeam  string
	OpenedBy   kernel.ActorRef
	OpenEpoch  int64
	OpenedAt   time.Time
}

// Open returns a new pending check of binding at row version 1, opened by by (team, epoch) at at.
func Open(id string, b Binding, previous string, by kernel.ActorRef, team string, epoch int64, at time.Time) *Check {
	return &Check{ID: id, Binding: b, Status: Pending, RowVersion: 1, Previous: previous, OwnerActor: by.ID, OwnerTeam: team,
		OpenedBy: by, OpenEpoch: epoch, OpenedAt: kernel.Instant(at)}
}

// CheckVersion compares the caller's expected row version with c (nil: no check yet, version 0).
func CheckVersion(c *Check, expected int64) error {
	var have int64
	if c != nil {
		have = c.RowVersion
	}
	if have != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Evaluated makes evaluation id the check's current evaluation: a new check stays pending at
// version 1; a pending check is re-evaluated before its decision; a blocked one returns to pending
// after its facts changed (SM-CNX-COMPLIANCE blocked → evaluate → pending). A clear conclusion is
// not re-evaluated: it is reopened first (INVALID_STATE).
func (c *Check) Evaluated(id string, fresh bool) error {
	if fresh {
		c.Evaluation = id
		return nil
	}
	if c.Status == Clear {
		return kernel.Reject(kernel.CodeInvalidState, "/args/order_revision", "A clear conclusion is reopened before it is evaluated again")
	}
	c.Status, c.Evaluation, c.Decision = Pending, id, ""
	c.RowVersion++
	return nil
}

// Decided records decision d on the pending check.
func (c *Check) Decided(d *Decision) {
	c.Status, c.Decision = d.Conclusion, d.ID
	c.RowVersion++
}

// Reopened returns a clear check to pending without a current evaluation: the next conclusion
// needs a new evaluation (SM-CNX-COMPLIANCE clear → reopen → pending).
func (c *Check) Reopened() error {
	if c.Status != Clear {
		return kernel.Reject(kernel.CodeInvalidState, "/args/check_id", "Only a clear conclusion is reopened")
	}
	c.Status, c.Evaluation, c.Decision = Pending, "", ""
	c.RowVersion++
	return nil
}
