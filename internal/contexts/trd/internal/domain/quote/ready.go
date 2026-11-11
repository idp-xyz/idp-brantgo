package quote

import (
	"bytes"
	"encoding/json"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The judgments of TRD's read side over a formal revision (WBS S3-09; Python t03_storage
// quote_ready, current_reviewer, dispatch_preflight and dispatch_target; T03 A8, A14–A16, A34):
// whether the approval its commit evidence names still stands, whether its frozen bindings still
// hold on the current inputs, and its recipient as sealed. They decide nothing a command relies on:
// every command re-checks in its own transaction.

// Codes of the error catalog rows the read side decides (contexts TRD); package api declares the
// same with their categories (api/query_codes.go).
const (
	CodeCommitEvidenceMissing             = "COMMIT_EVIDENCE_MISSING"
	CodeApprovalMissing                   = "APPROVAL_MISSING"
	CodeRecipientSnapshotIntegrityFailure = "RECIPIENT_SNAPSHOT_INTEGRITY_FAILURE"
)

// ReadCategories maps the codes above to their error catalog category.
var ReadCategories = map[string]kernel.Category{
	CodeCommitEvidenceMissing:             kernel.CategoryInvalid,
	CodeApprovalMissing:                   kernel.CategoryInvalid,
	CodeRecipientSnapshotIntegrityFailure: kernel.CategoryInvalid,
}

func failRead(code, path, reason string) error {
	return kernel.RejectAs(code, ReadCategories[code], path, reason)
}

// Paths the read side's refusals point at: the formal revision's commit evidence and the frozen
// calculation's bindings in its commercial document.
const (
	PathCommit           = "/commit"
	PathCommitApproval   = "/commit/approval_id"
	PathCostScopeBinding = "/calculation/cost_scope_binding"
	PathSalesTermsBound  = "/calculation/sales_terms_binding"
)

// CommittedApproval returns the approval formal revision r's commit evidence names, while it is
// still in effect: COMMIT_EVIDENCE_MISSING for a revision never formalized, APPROVAL_MISSING when
// the root has no such approval, APPROVAL_REFERENCE_MISMATCH for an approval of another revision,
// hash or command, APPROVAL_REVOKED once it was revoked. Another approval of the root never stands
// in for it (Python test_t03_a34 test_another_approval_cannot_replace_formalization_approval).
func (q *Quote) CommittedApproval(r *Revision) (*Approval, error) {
	if r == nil || r.Formal == nil || r.Seal == nil {
		return nil, failRead(CodeCommitEvidenceMissing, PathCommit, "The revision has no commit evidence")
	}
	c := r.Formal.Commit
	a := q.Approval(c.ApprovalID)
	switch {
	case c.ApprovalID == "" || a == nil:
		return nil, failRead(CodeApprovalMissing, PathCommitApproval, "The approval the commit evidence names is missing")
	case a.Revision != r.Number || a.BusinessHash != r.Seal.BusinessHash || a.Target != c.Command:
		return nil, fail(CodeApprovalReferenceMismatch, PathCommitApproval, "The approval is not the one that formalized the revision")
	case a.Effectivity != Valid:
		return nil, fail(CodeApprovalRevoked, PathCommitApproval, "")
	}
	return a, nil
}

// ReadyApproval decides whether the approval that formalized revision r still stands at instant
// at, on its approvers' standings now (Python quote_ready and current_reviewer; T03 A34, A35): it
// is the committed one and in effect (CommittedApproval); its business hash and rule versions are
// the revision's and the current ones (APPROVAL_STALE); it has not lapsed (APPROVAL_EXPIRED, at <
// its expires_at, equality lapsed); and its reviewer and margin exception approver are still
// qualified (stands). It returns the approval when it was found, even when it no longer stands.
func (q *Quote) ReadyApproval(r *Revision, at time.Time, standings Standings) (*Approval, error) {
	a, err := q.CommittedApproval(r)
	if err != nil {
		if r != nil && r.Formal != nil {
			return q.Approval(r.Formal.Commit.ApprovalID), err
		}
		return nil, err
	}
	c, err := r.Calculation()
	if err != nil {
		return a, err
	}
	if c == nil || a.Rules != rulesOf(c) || a.Rules != current(c.CostScope.PricingPolicy) {
		return a, fail(CodeApprovalStale, PathCommitApproval, "The approval is of other content or rules")
	}
	if !kernel.Instant(at).Before(a.ExpiresAt) {
		return a, fail(CodeApprovalExpired, PathCommitApproval, "")
	}
	return a, q.stands(r, a, c, standings, PathCommitApproval)
}

// Bound reports the frozen calculation's bindings against the cost scope and sales terms calc binds
// on the current inputs (calc.BindScope, calc.BindTerms): REFERENCE_STALE at the binding that
// differs. The comparison is of the commercial document's form of both, which the business hash
// covers, so equal bindings are the ones the revision was approved and formalized on.
func (c *Calculation) Bound(scope calc.ScopeBinding, terms calc.TermsBinding) error {
	if c == nil {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/calculation", "The revision has no server calculation")
	}
	same := func(a, b any) bool {
		x, err := json.Marshal(a)
		if err != nil {
			return false
		}
		y, err := json.Marshal(b)
		return err == nil && bytes.Equal(x, y)
	}
	if !same(c.CostScope, scopeBinding(scope)) {
		return fail(CodeReferenceStale, PathCostScopeBinding, "The cost scope binds otherwise on the current inputs")
	}
	if !same(c.SalesTerms, termsBinding(terms)) {
		return fail(CodeReferenceStale, PathSalesTermsBound, "The sales terms bind otherwise on the current inputs")
	}
	return nil
}

// Recipient is who a formal revision is sent to as its sealed commercial document binds them
// (Python t03_dispatch_target_snapshot, T03 A16): the customer at its party revision, the contact
// at its row version, and the customer addresses with the role and text the freeze read. It is part
// of the commercial document, so it never changes and its integrity is the business hash's.
type Recipient struct {
	Customer  PartyRef           `json:"customer_ref"`
	Contact   ContactRef         `json:"contact_ref"`
	Addresses []RecipientAddress `json:"addresses"`
}

// RecipientAddress is one bound address of the recipient.
type RecipientAddress struct {
	Ref     AddressRef `json:"address_ref"`
	Role    string     `json:"role"`
	Address string     `json:"address"`
}

// RecipientOf returns the recipient sealed revision r binds and the SHA-256 of its canonical JSON,
// after checking the commercial document against its business hash
// (RECIPIENT_SNAPSHOT_INTEGRITY_FAILURE otherwise).
func RecipientOf(r *Revision) (Recipient, string, error) {
	if r == nil || r.Seal == nil {
		return Recipient{}, "", failRead(CodeRecipientSnapshotIntegrityFailure, "/", "The revision is not sealed")
	}
	c, err := ParseCommercial(r.Seal.Commercial, r.Seal.BusinessHash)
	if err != nil {
		return Recipient{}, "", failRead(CodeRecipientSnapshotIntegrityFailure, "/party_snapshots", "The sealed commercial document is not intact")
	}
	snap := c.PartySnapshots.Customer
	if snap.PartyID != c.Data.Customer.PartyID || snap.PartyRevision != c.Data.Customer.Revision || len(snap.Addresses) != len(c.Data.Addresses) {
		return Recipient{}, "", failRead(CodeRecipientSnapshotIntegrityFailure, "/party_snapshots/customer", "")
	}
	out := Recipient{Customer: c.Data.Customer, Contact: c.Data.Contact, Addresses: []RecipientAddress{}}
	for i, a := range c.Data.Addresses {
		s := snap.Addresses[i]
		if s.AddressID != a.AddressID {
			return Recipient{}, "", failRead(CodeRecipientSnapshotIntegrityFailure, "/party_snapshots/customer/addresses", "")
		}
		out.Addresses = append(out.Addresses, RecipientAddress{Ref: a, Role: s.Role, Address: s.Address})
	}
	tree, err := TreeOf(out)
	if err != nil {
		return Recipient{}, "", err
	}
	b, err := Canonical(tree)
	if err != nil {
		return Recipient{}, "", err
	}
	return out, Hash(b), nil
}
