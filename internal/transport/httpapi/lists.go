package httpapi

import (
	"net/http"
	"reflect"
	"slices"
	"strconv"

	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The list reads (S7-06 read models): GET /api/brant/v1/<bc>/<objects>[?<filter>=][&after=][&limit=].
// Each returns a page of the objects the caller sees, most recently changed first
// (kernel.ListCursor), with "next": the after of the following page, absent on the last. The
// context validates the filter values; the transport refuses unknown and repeated parameters.

// listParams reads the query of a list read: the named text filters (absent ones are empty), after
// and limit.
func listParams(r *http.Request, filters ...string) (map[string]string, int, error) {
	q := r.URL.Query()
	for k, v := range q {
		if !slices.Contains(filters, k) && k != "after" && k != "limit" {
			return nil, 0, kernel.Reject(kernel.CodeSchemaInvalid, "/"+k, "unknown query parameter")
		}
		if len(v) != 1 || v[0] == "" {
			return nil, 0, kernel.Reject(kernel.CodeSchemaInvalid, "/"+k, "one non-empty value")
		}
	}
	limit := 0
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > kernel.MaxListPage {
			return nil, 0, kernel.Reject(kernel.CodeSchemaInvalid, "/limit", "1 to "+strconv.Itoa(kernel.MaxListPage))
		}
		limit = n
	}
	out := map[string]string{"after": q.Get("after")}
	for _, f := range filters {
		out[f] = q.Get(f)
	}
	return out, limit, nil
}

// listRoute describes a list read for the OpenAPI document: its text filters and the type of its
// page, whose tagged form is the 200 response.
type listRoute struct {
	filters []string
	page    reflect.Type
}

var listRoutes = map[string]listRoute{
	"/api/brant/v1/inq/rfqs": {[]string{"status", "customer_id"}, reflect.TypeFor[inqapi.RfqPage]()},
}

func (s *server) listRfqs(w http.ResponseWriter, r *http.Request) {
	p, limit, err := listParams(r, listRoutes["/api/brant/v1/inq/rfqs"].filters...)
	if err == nil {
		var page inqapi.RfqPage
		if page, err = s.cfg.INQ.ListRfqs(r.Context(), inqapi.RfqQuery{Status: p["status"], CustomerID: p["customer_id"], After: p["after"], Limit: limit}); err == nil {
			writeJSON(w, http.StatusOK, okView(page))
			return
		}
	}
	s.fail(w, r, err)
}
