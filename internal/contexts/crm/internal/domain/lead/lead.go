// Package lead is the Lead aggregate. A lead is appended by lead.capture and never revised
// (S6-D04: no assignment, no public pool, no integration event).
package lead

// StatusCaptured is the only status.
const StatusCaptured = "captured"

// Lead is the aggregate root.
type Lead struct {
	ID            string
	SourceChannel string
	CompanyName   string
	ContactName   string
	Summary       string
	Status        string
	RowVersion    int64
	OwnerActor    string
	OwnerTeam     string
}

// Capture returns a new lead at row version 1.
func Capture(id, source, company, contact, summary, owner, team string) *Lead {
	return &Lead{ID: id, SourceChannel: source, CompanyName: company, ContactName: contact, Summary: summary,
		Status: StatusCaptured, RowVersion: 1, OwnerActor: owner, OwnerTeam: team}
}
