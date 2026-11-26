package listquery

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func TestWhereCarriesOnlyWhatApplies(t *testing.T) {
	var w Where
	w.Scope("p", kernel.Scope{Tenant: "t", Company: "c"})
	w.Sees(Sight{DataScope: "company", ActorID: "u"}, "p.owner", "p.team")
	w.After(Sight{}, "p.at", "p.id")
	if got := w.SQL(); got != "p.tenant_id = $1 AND p.company_id = $2" || len(w.Args) != 2 {
		t.Fatalf("%s %v", got, w.Args)
	}
}

func TestSeesFollowsTheDataScope(t *testing.T) {
	for _, c := range []struct {
		sight Sight
		want  string
		args  int
	}{
		{Sight{DataScope: "own", ActorID: "u", Team: "east"}, "o = $1", 1},
		{Sight{DataScope: "team", ActorID: "u", Team: "east"}, "(o = $1 OR t = $2)", 2},
		// A team scope without a team sees its own rows only.
		{Sight{DataScope: "team", ActorID: "u"}, "o = $1", 1},
		// An unknown scope is the narrowest.
		{Sight{DataScope: "", ActorID: "u"}, "o = $1", 1},
	} {
		var w Where
		w.Sees(c.sight, "o", "t")
		if w.SQL() != c.want || len(w.Args) != c.args {
			t.Errorf("%+v: %s %v", c.sight, w.SQL(), w.Args)
		}
	}
}

func TestAfterAndContains(t *testing.T) {
	var w Where
	at := time.Unix(10, 0)
	w.After(Sight{After: &kernel.ListCursor{At: at, ID: "x"}}, "at", "id")
	w.Contains(`50%_off\`, "name", "id")
	if got := w.SQL(); got != `(at, id) < ($1::timestamptz, $2) AND (name ILIKE $3 ESCAPE '\' OR id ILIKE $3 ESCAPE '\')` {
		t.Fatal(got)
	}
	if w.Args[2] != `%50\%\_off\\%` {
		t.Fatalf("pattern %q", w.Args[2])
	}
}
