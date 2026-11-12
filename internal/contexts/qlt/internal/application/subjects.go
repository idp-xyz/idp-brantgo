package application

import (
	"context"
	"errors"

	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
)

// Subjects is QLT's port to PUR (Context Map R24 PUR→QLT, Customer/Supplier, "InspectionSubject v1",
// guardLock false; risk 供应商自报合格被当作放行). QLT asks it before its own transaction, in a
// transaction of PUR's: R24 is no guard edge, so nothing PUR holds is locked while QLT decides.
// What QLT binds of a subject (revision, hash, specification, unit, quantity, order line) is the
// subject as PUR said it was then.
//
// The planned implementation is PUR's InspectionSubject v1 lookup ResolveCommitmentLines (S5 unit
// A4); until it is wired, NotWired answers DEPENDENCY_NOT_SUPPORTED and tests implement the port.
// ResolveSubjects answers at most api.MaxLookup references; subjects PUR does not show the caller
// (another company, outside its data scope, no such line) are absent.
type Subjects interface {
	ResolveSubjects(ctx context.Context, refs []api.SubjectRef) (map[api.SubjectRef]Subject, error)
}

// Subject is what PUR says of a subject now: the line of a purchase commitment at the commitment's
// Revision and content Hash, its state (Status, as PUR names it) and Code — "" when the line serves
// inspection and release now, otherwise PUR's reason (such as a commitment not issued yet or
// cancelled) — the supplier, the sales order line it serves at the order revision and hash, the SKU
// specification revision, the POL unit revision and the line's Quantity (kernel.QuantityFormat,
// positive) in that unit.
type Subject struct {
	Ref           api.SubjectRef
	Revision      int64
	Hash          string
	Status        string
	Code          string
	SupplierID    string
	OrderID       string
	OrderRevision int64
	OrderHash     string
	OrderLineID   string
	SkuID         string
	SpecRevision  int64
	UnitID        string
	UnitRevision  int64
	Quantity      string
}

// ErrSubjectsNotWired is what a Subjects port answers when PUR's InspectionSubject v1 is not wired
// into the process; QLT refuses the command with DEPENDENCY_NOT_SUPPORTED.
var ErrSubjectsNotWired = errors.New("qlt: PUR's InspectionSubject v1 is not wired")

// NotWired is the Subjects port while PUR publishes no InspectionSubject v1 (S5 unit A4).
type NotWired struct{}

// ResolveSubjects answers ErrSubjectsNotWired.
func (NotWired) ResolveSubjects(context.Context, []api.SubjectRef) (map[api.SubjectRef]Subject, error) {
	return nil, ErrSubjectsNotWired
}
