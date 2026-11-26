// Package listquery builds the WHERE clause of a list read: the filters that are set, the rows
// the caller's authorization decision sees, and the page boundary. A filter that is not set is
// not in the statement, so the planner sees only the conditions that apply.
package listquery

import (
	"strconv"
	"strings"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Sight is the caller's authorization decision as a list query applies it: the rows of the scope
// that authorization.Decision.Sees accepts, as SQL. After is the last entry of the previous page,
// nil for the first; Limit is the most rows to return.
type Sight struct {
	DataScope, ActorID, Team string
	After                    *kernel.ListCursor
	Limit                    int
}

// Where collects clauses joined by AND and their numbered arguments.
type Where struct {
	clauses []string
	Args    []any
}

// Arg adds v to the arguments and returns its placeholder.
func (w *Where) Arg(v any) string {
	w.Args = append(w.Args, v)
	return "$" + strconv.Itoa(len(w.Args))
}

// Add adds a clause.
func (w *Where) Add(clause string) { w.clauses = append(w.clauses, clause) }

// SQL returns the clauses joined by AND.
func (w *Where) SQL() string { return strings.Join(w.clauses, " AND ") }

// Scope adds the tenant and company of scope on the columns of alias ("" for an unqualified table).
func (w *Where) Scope(alias string, scope kernel.Scope) {
	if alias != "" {
		alias += "."
	}
	w.Add(alias + "tenant_id = " + w.Arg(string(scope.Tenant)))
	w.Add(alias + "company_id = " + w.Arg(string(scope.Company)))
}

// Sees adds the caller's sight of rows owned by the actor in column ownerActor, whose team is in
// column ownerTeam: all of the company's, the caller's team's and own, or the caller's own.
func (w *Where) Sees(s Sight, ownerActor, ownerTeam string) {
	switch s.DataScope {
	case "company":
	case "team":
		if s.Team != "" {
			w.Add("(" + ownerActor + " = " + w.Arg(s.ActorID) + " OR " + ownerTeam + " = " + w.Arg(s.Team) + ")")
			return
		}
		fallthrough
	default:
		w.Add(ownerActor + " = " + w.Arg(s.ActorID))
	}
}

// After adds the page boundary on the columns at and id, both descending.
func (w *Where) After(s Sight, at, id string) {
	if s.After != nil {
		w.Add("(" + at + ", " + id + ") < (" + w.Arg(s.After.At) + "::timestamptz, " + w.Arg(s.After.ID) + ")")
	}
}

// Contains adds a case-insensitive search for text in any of columns. The text is matched
// literally: its %, _ and \ are escaped.
func (w *Where) Contains(text string, columns ...string) {
	pattern := w.Arg("%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text) + "%")
	parts := make([]string, len(columns))
	for i, c := range columns {
		parts[i] = c + " ILIKE " + pattern + ` ESCAPE '\'`
	}
	w.Add("(" + strings.Join(parts, " OR ") + ")")
}
